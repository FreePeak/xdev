package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // CGO-free SQLite driver (M12 #44)
)

// Mnemopi is the mnemopi memory backend (M12 #44): a local SQLite store of
// banks, facts (with tags and timestamps), and a fact-to-fact link graph,
// with polyphonic recall (keyword/FTS + tag + graph + recency + kind) merged
// by reciprocal rank fusion.
//
// It implements Store, so it is a drop-in replacement for the local markdown
// backend at the prompt/learn/memory:// seams; the extra surface (recall,
// retain, reflect, the retain queue) is reached through its tools.
//
// The zero value is off: Dir empty means every method is a no-op.
type Mnemopi struct {
	// Dir is the memory directory holding mnemopi.db. Empty = off.
	Dir string
	// Scope is the bank scoping rule: global | project | project-tagged.
	Scope string
	// Tag is the literal bank tag used by the project-tagged scope.
	Tag string
	// CWD anchors the project bank (the nearest repository root above it,
	// lowercased basename). Empty means the process working directory.
	CWD string
	// LLMMode is smol | remote | none. It selects which completion the
	// wiring hands to Complete (none disables reflect entirely).
	LLMMode string
	// RecallLimit bounds how many hits Recall returns (default 8, max 32).
	RecallLimit int
	// InjectionTokenLimit bounds the rendered recall/summary block
	// (~4 chars/token; default 1200).
	InjectionTokenLimit int
	// RetainEveryNTurns makes NoteTurn fire a consolidation every N turns
	// (default 3; 0 disables the turn trigger).
	RetainEveryNTurns int
	// Complete is the synthesis seam (the pipeline's smol completion):
	// prompt in, text out. Nil makes reflect a reported no-op, never a
	// failed session.
	Complete CompleteFunc
	// OnError receives best-effort background failures (queue drain).
	OnError func(error)

	mu    sync.Mutex
	db    *sql.DB
	turns int
}

// CompleteFunc is the model seam the reflection pass runs on (the same
// signature memory.Pipeline.Complete carries).
type CompleteFunc func(ctx context.Context, prompt string) (string, error)

// Bank scopes.
const (
	ScopeGlobal        = "global"
	ScopeProject       = "project"
	ScopeProjectTagged = "project-tagged"
)

// Fact kinds.
const (
	KindFact       = "fact"
	KindLesson     = "lesson"
	KindReflection = "reflection"
	KindSummary    = "summary"
)

// Queue item kinds.
const (
	QueueRetain      = "retain"
	QueueConsolidate = "consolidate"
)

// Mnemopi defaults (settings override each one).
const (
	DefaultScope               = ScopeProject
	DefaultLLMMode             = "smol"
	DefaultRetainEveryNTurns   = 3
	DefaultRecallLimit         = 8
	MaxRecallLimit             = 32
	DefaultInjectionTokenLimit = 1200
	DefaultQueueDrainMillis    = 1500
)

// Storage bounds. Every one of them exists because the input is model- or
// queue-supplied: an unbounded fact would silently blow the injection budget
// or the prompt-injection surface.
const (
	maxFactChars       = 4000
	maxReflectionChars = 1200
	maxTagsPerFact     = 8
	maxTagChars        = 32
	maxLinkNeighbors   = 8
	maxQueueApply      = 32
	// rrfK is the standard reciprocal-rank-fusion constant: 1/(k+rank)
	// flattens the head of every mode's list so one mode cannot dominate.
	rrfK          = 60
	modeRankLimit = 50
	// previewChars bounds one recall preview; the full row is always
	// readable through memory://<id> (read before memory_edit).
	previewChars  = 500
	charsPerToken = 4
	schemaVersion = 1
)

const mnemopiSchema = `
CREATE TABLE IF NOT EXISTS banks (
  id         INTEGER PRIMARY KEY,
  scope      TEXT NOT NULL,
  project    TEXT NOT NULL DEFAULT '',
  tag        TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  UNIQUE (scope, project, tag)
);
CREATE TABLE IF NOT EXISTS facts (
  id         INTEGER PRIMARY KEY,
  bank_id    INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  text       TEXT NOT NULL,
  source     TEXT NOT NULL DEFAULT '',
  session    TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  hits       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS facts_bank ON facts (bank_id, created_at DESC);
CREATE TABLE IF NOT EXISTS fact_tags (
  fact_id INTEGER NOT NULL,
  tag     TEXT NOT NULL,
  PRIMARY KEY (fact_id, tag)
);
CREATE INDEX IF NOT EXISTS fact_tags_tag ON fact_tags (tag);
CREATE TABLE IF NOT EXISTS edges (
  src        INTEGER NOT NULL,
  dst        INTEGER NOT NULL,
  rel        TEXT NOT NULL,
  weight     REAL NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (src, dst, rel)
);
CREATE INDEX IF NOT EXISTS edges_dst ON edges (dst);
CREATE TABLE IF NOT EXISTS queue (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL,
  text       TEXT NOT NULL DEFAULT '',
  source     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS facts_fts USING fts5(text, tokenize='porter unicode61');
`

// Bank is one memory bank row.
type Bank struct {
	ID      int64
	Scope   string
	Project string
	Tag     string
}

// Label renders the bank as scope[:project][/tag].
func (b Bank) Label() string {
	out := b.Scope
	if b.Project != "" {
		out += ":" + b.Project
	}
	if b.Tag != "" {
		out += "/" + b.Tag
	}
	return out
}

// Fact is one stored memory.
type Fact struct {
	ID        int64
	Kind      string
	Text      string
	Tags      []string
	Source    string
	Session   string
	CreatedAt time.Time
	UpdatedAt time.Time
	Hits      int
	Bank      string // bank label, filled by recall/listing
}

// Hit is one recall result: the fact, its fused score, and the recall modes
// that surfaced it (observability for the polyphonic merge).
type Hit struct {
	Fact  Fact
	Score float64
	Modes []string
}

// RetainResult reports what one Retain did.
type RetainResult struct {
	ID      int64
	Linked  int  // graph edges written to related facts
	Deduped bool // an identical fact already existed: it was touched, not duplicated
}

// SyncResult reports one bounded consolidation pass.
type SyncResult struct {
	Applied      int // queued retains written as facts
	Consolidated bool
	Reflections  int
	Remaining    int
	Elapsed      time.Duration
}

// FactEdit is a bounded edit of a stored fact (the memory_edit tool).
type FactEdit struct {
	ID     int64
	Text   *string
	Tags   *[]string
	Kind   *string
	Delete bool
}

// --- lifecycle ---

// Off reports whether the backend is disabled.
func (m *Mnemopi) Off() bool { return m == nil || m.Dir == "" }

// dbPath is the SQLite file.
func (m *Mnemopi) dbPath() string { return filepath.Join(m.Dir, "mnemopi.db") }

// Ensure creates the directory and the schema (idempotent).
func (m *Mnemopi) Ensure() error {
	if m.Off() {
		return nil
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	_, err := m.handle()
	return err
}

// handle opens (once) and returns the connection.
//
// ponytail: one connection per process (SetMaxOpenConns(1)) — SQLite writes
// serialize anyway, and this removes SQLITE_BUSY from the picture entirely.
// Upgrade path: a small pool plus BEGIN IMMEDIATE retries if a future
// read-heavy workload ever cares.
func (m *Mnemopi) handle() (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handleLocked()
}

func (m *Mnemopi) handleLocked() (*sql.DB, error) {
	if m.db != nil {
		return m.db, nil
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + m.dbPath() + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("memory: open %s: %w", m.dbPath(), err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(mnemopiSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory: schema: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory: schema version: %w", err)
	}
	m.db = db
	return db, nil
}

// Close releases the connection (tests, drain-on-exit).
func (m *Mnemopi) Close() error {
	if m.Off() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db == nil {
		return nil
	}
	err := m.db.Close()
	m.db = nil
	return err
}

// Paths exposes the two backing locations. Both point at the one SQLite
// file: mnemopi keeps facts, lessons and the link graph in a single store.
func (m *Mnemopi) Paths() (summary, lessons string) {
	if m.Off() {
		return "", ""
	}
	return m.dbPath(), m.dbPath() + " (banks, facts, links and the retain queue)"
}

// bankName lowercases a directory name into a bank key.
func bankName(dir string) string {
	base := strings.ToLower(filepath.Base(dir))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "root"
	}
	return base
}

// projectName resolves the project bank key: the nearest repository root
// above the working directory, lowercased. A cwd that is not inside a
// repository falls back to its own basename, so a scratch directory still
// gets its own bank instead of landing in the global one.
func projectName(cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	start := filepath.Clean(cwd)
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return bankName(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return bankName(start)
}

// scopeOrDefault reports the effective scope.
func (m *Mnemopi) scopeOrDefault() string {
	switch m.Scope {
	case ScopeGlobal, ScopeProject, ScopeProjectTagged:
		return m.Scope
	default:
		return DefaultScope
	}
}

// bankKey is the (scope, project, tag) triple the active bank is keyed by.
func (m *Mnemopi) bankKey() (scope, project, tag string) {
	scope = m.scopeOrDefault()
	switch scope {
	case ScopeGlobal:
		return scope, "", ""
	case ScopeProject:
		return scope, projectName(m.CWD), ""
	default:
		return scope, projectName(m.CWD), normalizeTag(m.Tag)
	}
}

// resolveBank returns the active bank id, creating the row when missing.
func resolveBank(db *sql.DB, scope, project, tag string, create bool) (int64, error) {
	row := db.QueryRow(`SELECT id FROM banks WHERE scope = ? AND project = ? AND tag = ?`, scope, project, tag)
	var id int64
	err := row.Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if !create {
		return 0, sql.ErrNoRows
	}
	res, err := db.Exec(`INSERT INTO banks (scope, project, tag, created_at) VALUES (?, ?, ?, ?)`,
		scope, project, tag, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// activeBank resolves (creating) the active bank.
func (m *Mnemopi) activeBank(db *sql.DB) (int64, Bank, error) {
	scope, project, tag := m.bankKey()
	id, err := resolveBank(db, scope, project, tag, true)
	if err != nil {
		return 0, Bank{}, err
	}
	return id, Bank{ID: id, Scope: scope, Project: project, Tag: tag}, nil
}

// readableBanks is the recall/manage scope: the active bank plus the global
// bank, which by definition applies to every project.
func (m *Mnemopi) readableBanks(db *sql.DB) ([]int64, error) {
	scope, project, tag := m.bankKey()
	var ids []int64
	rows, err := db.Query(`SELECT id FROM banks WHERE (scope = ? AND project = ? AND tag = ?) OR scope = ?`,
		scope, project, tag, ScopeGlobal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- write path ---

// Retain stores one fact (deduplicating identical text within the bank) and
// proactively links it to the related facts already in that bank.
func (m *Mnemopi) Retain(ctx context.Context, f Fact) (RetainResult, error) {
	if m.Off() {
		return RetainResult{}, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	text := collapseWS(f.Text)
	if text == "" {
		return RetainResult{}, fmt.Errorf("memory: fact text is required")
	}
	text = capChars(text, maxFactChars)
	kind := f.Kind
	switch kind {
	case KindFact, KindLesson, KindReflection, KindSummary:
	case "":
		kind = KindFact
	default:
		return RetainResult{}, fmt.Errorf("memory: unknown fact kind %q (fact|lesson|reflection|summary)", kind)
	}
	tags := normalizeTags(f.Tags)
	now := time.Now().Unix()

	db, err := m.handle()
	if err != nil {
		return RetainResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	bankID, _, err := m.activeBank(db)
	if err != nil {
		return RetainResult{}, err
	}
	// Identical text in the same bank is the same memory: touch it instead
	// of growing the store with paraphrases of itself.
	var existing int64
	err = db.QueryRow(`SELECT id FROM facts WHERE bank_id = ? AND text = ? LIMIT 1`, bankID, text).Scan(&existing)
	if err == nil {
		if _, err := db.Exec(`UPDATE facts SET updated_at = ?, kind = ? WHERE id = ?`, now, kind, existing); err != nil {
			return RetainResult{}, err
		}
		for _, tg := range tags {
			if _, err := db.Exec(`INSERT OR IGNORE INTO fact_tags (fact_id, tag) VALUES (?, ?)`, existing, tg); err != nil {
				return RetainResult{}, err
			}
		}
		return RetainResult{ID: existing, Deduped: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RetainResult{}, err
	}

	res, err := db.Exec(`INSERT INTO facts (bank_id, kind, text, source, session, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, bankID, kind, text, capChars(collapseWS(f.Source), 200), f.Session, now, now)
	if err != nil {
		return RetainResult{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return RetainResult{}, err
	}
	if _, err := db.Exec(`INSERT INTO facts_fts (rowid, text) VALUES (?, ?)`, id, text); err != nil {
		return RetainResult{}, err
	}
	for _, tg := range tags {
		if _, err := db.Exec(`INSERT OR IGNORE INTO fact_tags (fact_id, tag) VALUES (?, ?)`, id, tg); err != nil {
			return RetainResult{}, err
		}
	}
	linked, err := m.link(db, bankID, id, text, tags, now)
	if err != nil {
		return RetainResult{}, err
	}
	return RetainResult{ID: id, Linked: linked}, nil
}

// link writes the proactive graph edges for a freshly inserted fact: the
// facts sharing a tag with it, plus the facts sharing its keywords, ranked
// by overlap and capped at maxLinkNeighbors.
func (m *Mnemopi) link(db *sql.DB, bankID, id int64, text string, tags []string, now int64) (int, error) {
	weights := map[int64]float64{}
	if len(tags) > 0 {
		args := []any{bankID, id}
		for _, tg := range tags {
			args = append(args, tg)
		}
		rows, err := db.Query(`SELECT ft.fact_id, count(*) FROM fact_tags ft JOIN facts f ON f.id = ft.fact_id
			WHERE f.bank_id = ? AND ft.fact_id <> ? AND ft.tag IN (`+placeholders(len(tags))+`)
			GROUP BY ft.fact_id`, args...)
		if err != nil {
			return 0, err
		}
		if err := weightsFromRows(rows, weights, 1.0); err != nil {
			return 0, err
		}
	}
	// Keyword overlap: reuses the FTS index over the last 200 facts.
	if toks := tokens(text); len(toks) > 0 {
		query := ftsQuery(toks)
		rows, err := db.Query(`SELECT f.id FROM facts_fts x JOIN facts f ON f.id = x.rowid
			WHERE x.facts_fts MATCH ? AND f.bank_id = ? AND f.id <> ? ORDER BY bm25(x) LIMIT 40`,
			query, bankID, id)
		if err == nil {
			defer rows.Close()
			rank := 0.0
			for rows.Next() {
				var other int64
				if err := rows.Scan(&other); err != nil {
					rows.Close()
					return 0, err
				}
				rank++
				if _, ok := weights[other]; !ok {
					weights[other] = 0.5 // keyword-only link: weaker than a shared tag
				}
			}
			if err := rows.Err(); err != nil {
				return 0, err
			}
		}
	}
	if len(weights) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(weights))
	for other := range weights {
		ids = append(ids, other)
	}
	sort.Slice(ids, func(i, j int) bool {
		if weights[ids[i]] != weights[ids[j]] {
			return weights[ids[i]] > weights[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > maxLinkNeighbors {
		ids = ids[:maxLinkNeighbors]
	}
	for _, other := range ids {
		if _, err := db.Exec(`INSERT OR REPLACE INTO edges (src, dst, rel, weight, created_at) VALUES (?, ?, 'related', ?, ?)`,
			id, other, weights[other], now); err != nil {
			return 0, err
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO edges (src, dst, rel, weight, created_at) VALUES (?, ?, 'related', ?, ?)`,
			other, id, weights[other], now); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func weightsFromRows(rows *sql.Rows, into map[int64]float64, scale float64) error {
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n float64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		into[id] += n * scale
	}
	return rows.Err()
}

// --- recall ---

// Recall runs the polyphonic merge: keyword/FTS, tag, graph, recency and
// kind-ranked candidate lists, fused with reciprocal rank fusion.
//
// A fact surfaced by only one mode still appears — that is the point of the
// merge (a tag-only or graph-only hit is exactly what keyword search misses).
func (m *Mnemopi) Recall(ctx context.Context, query string, limit int) ([]Hit, error) {
	if m.Off() {
		return nil, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	if limit <= 0 {
		limit = m.recallLimit()
	}
	if limit > MaxRecallLimit {
		limit = MaxRecallLimit
	}
	db, err := m.handle()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	banks, err := m.readableBanks(db)
	if err != nil {
		return nil, err
	}
	if len(banks) == 0 {
		return nil, nil // nothing retained yet: not an error
	}
	toks := tokens(query)

	var lists []ranked
	if kw := m.modeKeyword(db, banks, toks, query); len(kw) > 0 {
		lists = append(lists, ranked{mode: "keyword", ids: kw})
	}
	seeds := topIDs(lists, 5)
	if tg := m.modeTag(db, banks, toks); len(tg) > 0 {
		lists = append(lists, ranked{mode: "tag", ids: tg})
	}
	if gr := m.modeGraph(db, banks, seeds); len(gr) > 0 {
		lists = append(lists, ranked{mode: "graph", ids: gr})
	}
	if rc := m.modeRecency(db, banks); len(rc) > 0 {
		lists = append(lists, ranked{mode: "recency", ids: rc})
	}
	if kd := m.modeKind(db, banks, toks); len(kd) > 0 {
		lists = append(lists, ranked{mode: "kind", ids: kd})
	}
	if len(lists) == 0 {
		return nil, nil
	}

	score, modes := fuse(lists)
	facts, err := m.fetchFacts(db, keysOf(score))
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(facts))
	for _, f := range facts {
		hits = append(hits, Hit{Fact: f, Score: score[f.ID], Modes: modes[f.ID]})
	}
	sortHits(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	// Usage feedback: a recalled fact is a used fact (/memory stats shows it).
	for _, h := range hits {
		if _, err := db.Exec(`UPDATE facts SET hits = hits + 1 WHERE id = ?`, h.Fact.ID); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

// ranked is one mode's ordered candidate list.
type ranked struct {
	mode string
	ids  []int64
}

func topIDs(lists []ranked, n int) []int64 {
	var out []int64
	for _, l := range lists {
		for _, id := range l.ids {
			out = append(out, id)
			if len(out) >= n {
				return out
			}
		}
	}
	return out
}

// fuse merges the mode candidate lists with reciprocal rank fusion:
// score(d) = Σ over modes 1/(rrfK + rank(d)). Every candidate of every mode
// is in the result, so a hit only one mode found still surfaces — it just
// ranks below a hit several modes agree on.
func fuse(lists []ranked) (map[int64]float64, map[int64][]string) {
	score := map[int64]float64{}
	modes := map[int64][]string{}
	for _, l := range lists {
		for i, id := range l.ids {
			score[id] += 1.0 / float64(rrfK+i+1)
			modes[id] = append(modes[id], l.mode)
		}
	}
	return score, modes
}

// sortHits orders the fused result: score desc, then most recently updated,
// then id — fully deterministic, so identical stores answer identically.
func sortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if !hits[i].Fact.UpdatedAt.Equal(hits[j].Fact.UpdatedAt) {
			return hits[i].Fact.UpdatedAt.After(hits[j].Fact.UpdatedAt)
		}
		return hits[i].Fact.ID < hits[j].Fact.ID
	})
}

// modeKeyword is the FTS5 path; a query the tokenizer cannot express falls
// back to a LIKE scan rather than returning nothing.
func (m *Mnemopi) modeKeyword(db *sql.DB, banks []int64, toks []string, query string) []int64 {
	if len(toks) == 0 {
		return nil
	}
	args := []any{ftsQuery(toks)}
	args = append(args, intsToAny(banks)...)
	args = append(args, modeRankLimit)
	rows, err := db.Query(`SELECT f.id FROM facts_fts x JOIN facts f ON f.id = x.rowid
		WHERE x.facts_fts MATCH ? AND f.bank_id IN (`+placeholders(len(banks))+`)
		ORDER BY bm25(x) LIMIT ?`, args...)
	if ids, err := scanIDs(rows, err); err == nil {
		return ids
	}
	// LIKE fallback (FTS tokenization and Go tokenization can disagree, and
	// an unindexable query is still better answered than dropped): same bank
	// scope, newest first.
	like := append(intsToAny(banks), "%"+strings.ToLower(collapseWS(query))+"%", modeRankLimit)
	rows, err = db.Query(`SELECT id FROM facts WHERE bank_id IN (`+placeholders(len(banks))+`)
		AND lower(text) LIKE ? ORDER BY created_at DESC LIMIT ?`, like...)
	if ids, err := scanIDs(rows, err); err == nil {
		return ids
	}
	return nil
}

// modeTag ranks facts carrying any of the query's tokens as tags.
func (m *Mnemopi) modeTag(db *sql.DB, banks []int64, toks []string) []int64 {
	if len(toks) == 0 {
		return nil
	}
	args := []any{}
	for _, t := range toks {
		args = append(args, t)
	}
	args = append(args, intsToAny(banks)...)
	args = append(args, modeRankLimit)
	rows, err := db.Query(`SELECT ft.fact_id FROM fact_tags ft JOIN facts f ON f.id = ft.fact_id
		WHERE ft.tag IN (`+placeholders(len(toks))+`) AND f.bank_id IN (`+placeholders(len(banks))+`)
		GROUP BY ft.fact_id ORDER BY count(*) DESC, max(f.created_at) DESC LIMIT ?`, args...)
	return mustIDs(rows, err)
}

// modeGraph expands the strongest keyword seeds one hop through the link
// graph: the proactive links Retain writes.
func (m *Mnemopi) modeGraph(db *sql.DB, banks []int64, seeds []int64) []int64 {
	if len(seeds) == 0 {
		return nil
	}
	args := intsToAny(seeds)
	args = append(args, intsToAny(seeds)...)
	args = append(args, intsToAny(banks)...)
	args = append(args, modeRankLimit)
	rows, err := db.Query(`SELECT e.dst FROM edges e JOIN facts f ON f.id = e.dst
		WHERE e.src IN (`+placeholders(len(seeds))+`) AND e.dst NOT IN (`+placeholders(len(seeds))+`)
		AND f.bank_id IN (`+placeholders(len(banks))+`)
		GROUP BY e.dst ORDER BY max(e.weight) DESC LIMIT ?`, args...)
	return mustIDs(rows, err)
}

// modeRecency is the newest-first list: temporal proximity is a recall mode
// of its own (what was just learned beats what is merely similar).
func (m *Mnemopi) modeRecency(db *sql.DB, banks []int64) []int64 {
	args := intsToAny(banks)
	args = append(args, modeRankLimit)
	rows, err := db.Query(`SELECT id FROM facts WHERE bank_id IN (`+placeholders(len(banks))+`)
		ORDER BY updated_at DESC, id DESC LIMIT ?`, args...)
	return mustIDs(rows, err)
}

// modeKind is fact-type matching: a query that asks for a lesson (or a
// reflection) ranks that kind first. No intent word means the mode stays
// silent instead of padding the merge with noise.
func (m *Mnemopi) modeKind(db *sql.DB, banks []int64, toks []string) []int64 {
	kind := intentKind(toks)
	if kind == "" {
		return nil
	}
	args := []any{kind}
	args = append(args, intsToAny(banks)...)
	args = append(args, modeRankLimit)
	rows, err := db.Query(`SELECT id FROM facts WHERE kind = ? AND bank_id IN (`+placeholders(len(banks))+`)
		ORDER BY updated_at DESC, id DESC LIMIT ?`, args...)
	return mustIDs(rows, err)
}

func scanIDs(rows *sql.Rows, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// mustIDs is scanIDs for the modes whose failure means "no candidates".
func mustIDs(rows *sql.Rows, err error) []int64 {
	ids, err := scanIDs(rows, err)
	if err != nil {
		return nil
	}
	return ids
}

// fetchFacts loads the candidate facts (with tags and bank labels).
func (m *Mnemopi) fetchFacts(db *sql.DB, ids []int64) ([]Fact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := db.Query(`SELECT f.id, f.kind, f.text, f.source, f.session, f.created_at, f.updated_at, f.hits,
		b.scope, b.project, b.tag
		FROM facts f JOIN banks b ON b.id = f.bank_id WHERE f.id IN (`+placeholders(len(ids))+`)`, intsToAny(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := []Fact{}
	byID := map[int64]int{}
	for rows.Next() {
		var f Fact
		var scope, project, tag string
		var created, updated int64
		if err := rows.Scan(&f.ID, &f.Kind, &f.Text, &f.Source, &f.Session, &created, &updated, &f.Hits,
			&scope, &project, &tag); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(created, 0).UTC()
		f.UpdatedAt = time.Unix(updated, 0).UTC()
		f.Bank = Bank{Scope: scope, Project: project, Tag: tag}.Label()
		facts = append(facts, f)
		byID[f.ID] = len(facts) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tags, err := m.tagsFor(db, ids)
	if err != nil {
		return nil, err
	}
	for id, list := range tags {
		if i, ok := byID[id]; ok {
			facts[i].Tags = list
		}
	}
	return facts, nil
}

func (m *Mnemopi) tagsFor(db *sql.DB, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	rows, err := db.Query(`SELECT fact_id, tag FROM fact_tags WHERE fact_id IN (`+placeholders(len(ids))+`) ORDER BY tag`, intsToAny(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// --- rendering / injection ---

func (m *Mnemopi) recallLimit() int {
	if m.RecallLimit > 0 {
		return m.RecallLimit
	}
	return DefaultRecallLimit
}

func (m *Mnemopi) injectionChars() int {
	n := m.InjectionTokenLimit
	if n <= 0 {
		n = DefaultInjectionTokenLimit
	}
	chars := n * charsPerToken
	if cap := DefaultSummaryCapChars; chars > cap {
		chars = cap
	}
	return chars
}

// RecallText is the injected/tool-visible form of a recall: bounded by
// recallLimit hits and by the injection token cap, with the truncation
// stated instead of silently dropping facts.
func (m *Mnemopi) RecallText(ctx context.Context, query string) (string, error) {
	hits, err := m.Recall(ctx, query, m.recallLimit())
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no matching memories", nil
	}
	return renderHits(hits, m.injectionChars()), nil
}

// renderHits renders the recall bullet list. Each line carries the memory id
// (memory_edit needs it), the kind, the tags, the date, the modes that found
// it and the fused score; the preview is clipped at previewChars with an
// explicit marker, exactly like the reference backend — read memory://<id>
// for the full row before editing it.
func renderHits(hits []Hit, charBudget int) string {
	var b strings.Builder
	used := 0
	shown := 0
	for _, h := range hits {
		line := "- " + clipPreview(h.Fact.Text) + fmt.Sprintf(" (id: %d) [%s]", h.Fact.ID, h.Fact.Kind)
		if len(h.Fact.Tags) > 0 {
			line += " (tags: " + strings.Join(h.Fact.Tags, ", ") + ")"
		}
		line += fmt.Sprintf(" (%s · via %s · c:%.3f)", h.Fact.UpdatedAt.Format("2006-01-02"),
			strings.Join(h.Modes, "+"), h.Score)
		if used+len(line)+1 > charBudget {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
		used += len(line) + 1
		shown++
	}
	if shown < len(hits) {
		fmt.Fprintf(&b, "(%d more matching memories not shown; raise memory.mnemopi.injectionTokenLimit or read memory://<id>)\n", len(hits)-shown)
	}
	return strings.TrimRight(b.String(), "\n")
}

// clipPreview bounds one recall preview (the reference clips at 500 chars).
func clipPreview(s string) string {
	if len(s) <= previewChars {
		return s
	}
	return s[:previewChars] + "…"
}

// Summary is the injected guidance text: the consolidated summary fact, the
// newest facts, and the lesson tail — all inside the injection token cap.
func (m *Mnemopi) Summary() string {
	if m.Off() {
		return ""
	}
	db, err := m.handle()
	if err != nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	banks, err := m.readableBanks(db)
	if err != nil || len(banks) == 0 {
		return ""
	}
	summary, err := m.latestSummary(db, banks)
	if err != nil {
		return ""
	}
	facts, err := m.recent(db, banks, []string{KindFact, KindReflection, KindSummary}, m.lessonCap())
	if err != nil {
		return ""
	}
	lessons, err := m.recent(db, banks, []string{KindLesson}, m.lessonCap())
	if err != nil {
		return ""
	}
	var parts []string
	if summary != "" {
		parts = append(parts, summary)
	}
	if len(facts) > 0 {
		parts = append(parts, "Memories:\n"+renderFactLines(facts))
	}
	if len(lessons) > 0 {
		parts = append(parts, "Lessons:\n"+renderFactLines(lessons))
	}
	return capText(strings.TrimSpace(strings.Join(parts, "\n\n")), m.injectionChars())
}

func (m *Mnemopi) lessonCap() int {
	return DefaultLessonCap
}

func renderFactLines(facts []Fact) string {
	var b strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&b, "- %s — %s", f.UpdatedAt.Format("2006-01-02"), f.Text)
		if len(f.Tags) > 0 {
			fmt.Fprintf(&b, " (tags: %s)", strings.Join(f.Tags, ", "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// latestSummary returns the consolidated summary fact text for the banks.
func (m *Mnemopi) latestSummary(db *sql.DB, banks []int64) (string, error) {
	args := append([]any{KindSummary}, intsToAny(banks)...)
	var text string
	err := db.QueryRow(`SELECT text FROM facts WHERE kind = ? AND bank_id IN (`+placeholders(len(banks))+`)
		ORDER BY updated_at DESC LIMIT 1`, args...).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return text, nil
}

// recent lists the newest facts of the given kinds.
func (m *Mnemopi) recent(db *sql.DB, banks []int64, kinds []string, limit int) ([]Fact, error) {
	args := []any{}
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, intsToAny(banks)...)
	args = append(args, limit)
	rows, err := db.Query(`SELECT id FROM facts WHERE kind IN (`+placeholders(len(kinds))+`)
		AND bank_id IN (`+placeholders(len(banks))+`) ORDER BY updated_at DESC, id DESC LIMIT ?`, args...)
	ids, err := scanIDs(rows, err)
	if err != nil {
		return nil, err
	}
	return m.fetchFacts(db, ids)
}

// GuidanceBlock wraps Summary in the Memory Guidance shape omp injects.
func (m *Mnemopi) GuidanceBlock() string {
	s := m.Summary()
	if s == "" {
		return ""
	}
	return "# Memory Guidance\n\nHeuristic context from earlier sessions — not authoritative. " +
		"When it changes your plan, read the source (memory://root) and cite it.\n\n" + s
}

// Read resolves a memory:// URL. The paths mirror the local backend, so the
// two backends answer the same seam.
//
//	memory://root              the injected summary (what the prompt saw)
//	memory://root/MEMORY.md    the consolidated facts (summary + newest)
//	memory://root/learned.md   the lesson log
//	memory://<id>              one full memory row (recall previews are
//	                           clipped; read the row before memory_edit)
func (m *Mnemopi) Read(uri string) (string, error) {
	if m.Off() {
		return "", fmt.Errorf("memory: backend off (set memory: mnemopi in settings)")
	}
	rest := strings.TrimPrefix(uri, "memory://")
	if rest == uri {
		return "", fmt.Errorf("memory: not a memory URL: %q", uri)
	}
	rest = strings.Trim(rest, "/")
	switch rest {
	case "", "root":
		if s := m.Summary(); s != "" {
			return s, nil
		}
		return "(no memories stored yet)", nil
	case "root/MEMORY.md", "MEMORY.md":
		return m.renderBank(false), nil
	case "root/learned.md", "learned.md":
		return m.renderBank(true), nil
	}
	if id, err := strconv.ParseInt(strings.TrimPrefix(rest, "root/"), 10, 64); err == nil {
		return m.renderFact(id)
	}
	return "", fmt.Errorf("memory: unknown path %q (root, root/MEMORY.md, root/learned.md, <id>)", rest)
}

// renderFact renders one memory row in full: the header a model needs to
// decide what to edit, then the untruncated text.
func (m *Mnemopi) renderFact(id int64) (string, error) {
	f, err := m.FactByID(context.Background(), id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "memory %d\nkind: %s\nbank: %s\n", f.ID, f.Kind, f.Bank)
	if len(f.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(f.Tags, ", "))
	}
	if f.Source != "" {
		fmt.Fprintf(&b, "source: %s\n", f.Source)
	}
	fmt.Fprintf(&b, "created: %s\nupdated: %s\nhits: %d\n\n%s\n",
		f.CreatedAt.Format(time.RFC3339), f.UpdatedAt.Format(time.RFC3339), f.Hits, f.Text)
	return b.String(), nil
}

// renderBank renders the stored facts (lessonsOnly = the learned.md view).
func (m *Mnemopi) renderBank(lessonsOnly bool) string {
	if m.Off() {
		return ""
	}
	db, err := m.handle()
	if err != nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	banks, err := m.readableBanks(db)
	if err != nil || len(banks) == 0 {
		return ""
	}
	kinds := []string{KindFact, KindReflection, KindSummary}
	title := "# Memories"
	if lessonsOnly {
		kinds = []string{KindLesson}
		title = "# Lessons"
	}
	facts, err := m.recent(db, banks, kinds, modeRankLimit)
	if err != nil || len(facts) == 0 {
		return title + "\n\n(nothing stored yet)"
	}
	return title + "\n\n" + renderFactLines(facts) + "\n"
}

// WriteSummary replaces the consolidated summary (manual sync, or the
// two-phase pipeline once it writes through Store).
func (m *Mnemopi) WriteSummary(text string) error {
	if m.Off() {
		return fmt.Errorf("memory: backend off")
	}
	text = collapseWS(text)
	if text == "" {
		return fmt.Errorf("memory: summary text is required")
	}
	db, err := m.handle()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bankID, _, err := m.activeBank(db)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	ids, err := scanIDs(db.Query(`SELECT id FROM facts WHERE bank_id = ? AND kind = ?`, bankID, KindSummary))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := m.deleteFact(db, id); err != nil {
			return err
		}
	}
	res, err := db.Exec(`INSERT INTO facts (bank_id, kind, text, source, created_at, updated_at)
		VALUES (?, ?, ?, 'summary', ?, ?)`, bankID, KindSummary, capChars(text, maxFactChars), now, now)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO facts_fts (rowid, text) VALUES (?, ?)`, id, text)
	return err
}

// SaveLesson stores one lesson (the learn tool / pipeline seam).
func (m *Mnemopi) SaveLesson(text, origin string) error {
	_, err := m.Retain(context.Background(), Fact{Kind: KindLesson, Text: text, Source: origin})
	return err
}

// Clear drops the active bank's memories. The global bank and other
// projects' banks are deliberately untouched: /memory clear in one project
// must not be a data-loss switch for every other project in the store.
func (m *Mnemopi) Clear() error {
	if m.Off() {
		return fmt.Errorf("memory: backend off")
	}
	db, err := m.handle()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	scope, project, tag := m.bankKey()
	bankID, err := resolveBank(db, scope, project, tag, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ids, err := scanIDs(db.Query(`SELECT id FROM facts WHERE bank_id = ?`, bankID))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := m.deleteFact(db, id); err != nil {
			return err
		}
	}
	_, err = db.Exec(`DELETE FROM queue WHERE source = ?`, Bank{Scope: scope, Project: project, Tag: tag}.Label())
	return err
}

// deleteFact removes a fact, its FTS row, its tags and its graph edges.
func (m *Mnemopi) deleteFact(db *sql.DB, id int64) error {
	for _, stmt := range []string{
		`DELETE FROM facts_fts WHERE rowid = ?`,
		`DELETE FROM fact_tags WHERE fact_id = ?`,
		`DELETE FROM edges WHERE src = ? OR dst = ?`,
		`DELETE FROM facts WHERE id = ?`,
	} {
		args := []any{id}
		if strings.Count(stmt, "?") == 2 {
			args = []any{id, id}
		}
		if _, err := db.Exec(stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

// Stats reports the store for /memory stats.
func (m *Mnemopi) Stats() string {
	if m.Off() {
		return "memory: off"
	}
	db, err := m.handle()
	if err != nil {
		return "memory: " + err.Error()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	scope, project, tag := m.bankKey()
	active := Bank{Scope: scope, Project: project, Tag: tag}
	bankID, err := resolveBank(db, scope, project, tag, false)
	var banks, facts, edges, queued, bytes int64
	_ = db.QueryRow(`SELECT count(*) FROM banks`).Scan(&banks)
	if err == nil {
		_ = db.QueryRow(`SELECT count(*) FROM facts WHERE bank_id = ?`, bankID).Scan(&facts)
		_ = db.QueryRow(`SELECT count(*) FROM edges e JOIN facts f ON f.id = e.src WHERE f.bank_id = ?`, bankID).Scan(&edges)
		_ = db.QueryRow(`SELECT count(*) FROM queue`).Scan(&queued)
	}
	if fi, err := os.Stat(m.dbPath()); err == nil {
		bytes = fi.Size()
	}
	llm := m.LLMMode
	if llm == "" {
		llm = DefaultLLMMode
	}
	return fmt.Sprintf("memory: mnemopi (banks %d, this bank %s: %d facts, %d links, %d queued, %s on disk)\n"+
		"  recallLimit %d · injectionTokenLimit %d · retainEveryNTurns %d · llmMode %s\n  %s",
		banks, active.Label(), facts, edges, queued, humanBytes(bytes),
		m.recallLimit(), m.InjectionTokenLimit, m.RetainEveryNTurns, llm, m.dbPath())
}

// --- reflect + queue ---

// Reflect is the synthesis pass: the recent facts go to the smol-role
// completion seam, and the (bounded) reply is stored as a reflection fact so
// it is recallable afterwards.
func (m *Mnemopi) Reflect(ctx context.Context, topic string) (Fact, error) {
	if m.Off() {
		return Fact{}, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	if m.Complete == nil {
		return Fact{}, fmt.Errorf("memory: reflect needs a synthesis model (memory.mnemopi.llmMode: none disables it)")
	}
	db, err := m.handle()
	if err != nil {
		return Fact{}, err
	}
	m.mu.Lock()
	banks, err := m.readableBanks(db)
	if err != nil {
		m.mu.Unlock()
		return Fact{}, err
	}
	facts, err := m.recent(db, banks, []string{KindFact, KindLesson, KindReflection}, 24)
	m.mu.Unlock()
	if err != nil {
		return Fact{}, err
	}
	if len(facts) == 0 {
		return Fact{}, fmt.Errorf("memory: nothing to reflect on yet")
	}
	out, err := m.Complete(ctx, reflectPrompt(topic, facts))
	if err != nil {
		return Fact{}, err
	}
	text := collapseWS(out)
	if text == "" {
		return Fact{}, fmt.Errorf("memory: the synthesis pass returned nothing")
	}
	text = capChars(text, maxReflectionChars)
	tags := tokens(topic)
	tags = append(tags, "reflection")
	res, err := m.Retain(ctx, Fact{Kind: KindReflection, Text: text, Tags: tags, Source: "reflect"})
	if err != nil {
		return Fact{}, err
	}
	stored, err := m.FactByID(ctx, res.ID)
	return stored, err
}

func reflectPrompt(topic string, facts []Fact) string {
	var b strings.Builder
	b.WriteString("You are the memory consolidation pass for a coding agent. Below are the facts and lessons it retained recently.\n")
	b.WriteString("Write ONE terse paragraph (max 600 characters) a future session must know: durable conclusions, " +
		"contradictions resolved, and anything the retained facts only imply. No preamble, no bullet points, no markdown headings.\n\n")
	if strings.TrimSpace(topic) != "" {
		b.WriteString("Topic: " + collapseWS(topic) + "\n\n")
	}
	b.WriteString("Retained memories:\n")
	for _, f := range facts {
		fmt.Fprintf(&b, "- [%s] %s\n", f.Kind, capChars(f.Text, 400))
	}
	return b.String()
}

// Enqueue appends a pending retain (the auto-retain path: text captured now,
// written later, bounded).
func (m *Mnemopi) Enqueue(text, source string) (int64, error) {
	if m.Off() {
		return 0, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	text = collapseWS(text)
	if text == "" {
		return 0, fmt.Errorf("memory: queued fact text is required")
	}
	return m.enqueue(QueueRetain, capChars(text, maxFactChars), source)
}

// EnqueueSync appends a consolidation request (force a reflect pass).
func (m *Mnemopi) EnqueueSync() (int64, error) {
	if m.Off() {
		return 0, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	return m.enqueue(QueueConsolidate, "", "")
}

func (m *Mnemopi) enqueue(kind, text, source string) (int64, error) {
	db, err := m.handle()
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	scope, project, tag := m.bankKey()
	label := Bank{Scope: scope, Project: project, Tag: tag}.Label()
	own := source
	if own == "" {
		own = label
	}
	res, err := db.Exec(`INSERT INTO queue (kind, text, source, created_at) VALUES (?, ?, ?, ?)`,
		kind, text, own, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// NoteTurn advances the per-session turn counter; when retainEveryNTurns is
// reached it enqueues a consolidation and reports true (the caller is free
// to ignore the bool — the work is already queued). A negative setting
// disables the trigger: 0 means "unset" (use the default), so an overlay
// could not otherwise switch it off.
func (m *Mnemopi) NoteTurn() (bool, error) {
	if m.Off() {
		return false, nil
	}
	n := m.RetainEveryNTurns
	if n < 0 {
		return false, nil
	}
	if n == 0 {
		n = DefaultRetainEveryNTurns
	}
	m.mu.Lock()
	m.turns++
	due := m.turns%n == 0
	m.mu.Unlock()
	if !due {
		return false, nil
	}
	if _, err := m.EnqueueSync(); err != nil {
		return false, err
	}
	return true, nil
}

// queueItem is one pending queue row.
type queueItem struct {
	id     int64
	kind   string
	text   string
	source string
}

// Sync drains the queue against a deadline: queued retains become facts, and
// any consolidation marker (or at least one applied retain, when a synthesis
// seam is wired) triggers one reflect pass. Bounded by maxQueueApply items
// and by budget; leftover items stay queued and are reported, never dropped.
func (m *Mnemopi) Sync(ctx context.Context, budget time.Duration) (SyncResult, error) {
	started := time.Now()
	if m.Off() {
		return SyncResult{}, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	if budget <= 0 {
		budget = time.Duration(DefaultQueueDrainMillis) * time.Millisecond
	}
	deadline := started.Add(budget)
	db, err := m.handle()
	if err != nil {
		return SyncResult{}, err
	}
	var res SyncResult
	rows, err := db.Query(`SELECT id, kind, text, source FROM queue ORDER BY id LIMIT ?`, maxQueueApply)
	if err != nil {
		return res, err
	}
	var items []queueItem
	for rows.Next() {
		var it queueItem
		if err := rows.Scan(&it.id, &it.kind, &it.text, &it.source); err != nil {
			rows.Close()
			return res, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	consolidate := false
	for _, it := range items {
		if time.Now().After(deadline) {
			break
		}
		switch it.kind {
		case QueueRetain:
			if _, err := m.Retain(ctx, Fact{Kind: KindFact, Text: it.text, Source: it.source}); err != nil {
				continue // leave the item queued: a failed retain is retried, not lost
			}
			res.Applied++
			consolidate = true
		case QueueConsolidate:
			consolidate = true
		}
		if _, err := db.Exec(`DELETE FROM queue WHERE id = ?`, it.id); err != nil {
			return res, err
		}
	}
	if consolidate && m.Complete != nil && time.Now().Before(deadline) {
		if _, err := m.Reflect(ctx, "consolidate recent memories"); err == nil {
			res.Reflections = 1
		}
		res.Consolidated = true
	}
	_ = db.QueryRow(`SELECT count(*) FROM queue`).Scan(&res.Remaining)
	res.Elapsed = time.Since(started)
	return res, nil
}

// Drain is the session-exit drain: a bounded, best-effort Sync inside the
// 1.5s budget. Errors are swallowed by design — an exit path must never
// delay or fail a session; the queue survives for the next run.
func (m *Mnemopi) Drain(ctx context.Context) SyncResult {
	if m.Off() {
		return SyncResult{}
	}
	res, err := m.Sync(ctx, time.Duration(DefaultQueueDrainMillis)*time.Millisecond)
	if err != nil && m.OnError != nil {
		m.OnError(err)
	}
	return res
}

// QueueStats reports queue depth for /memory queue.
func (m *Mnemopi) QueueStats() string {
	if m.Off() {
		return "memory: off"
	}
	db, err := m.handle()
	if err != nil {
		return "memory: " + err.Error()
	}
	var depth, retains, consolidations int
	var oldest sql.NullInt64
	_ = db.QueryRow(`SELECT count(*) FROM queue`).Scan(&depth)
	_ = db.QueryRow(`SELECT count(*) FROM queue WHERE kind = ?`, QueueRetain).Scan(&retains)
	_ = db.QueryRow(`SELECT count(*) FROM queue WHERE kind = ?`, QueueConsolidate).Scan(&consolidations)
	_ = db.QueryRow(`SELECT min(created_at) FROM queue`).Scan(&oldest)
	out := fmt.Sprintf("retain queue: %d pending (%d retains, %d consolidation requests)", depth, retains, consolidations)
	if oldest.Valid {
		out += fmt.Sprintf("; oldest %s ago", time.Since(time.Unix(oldest.Int64, 0)).Round(time.Second))
	}
	if m.Complete == nil {
		out += "\nsynthesis: off (no model wired; retains still drain, reflections do not)"
	}
	return out
}

// --- edit ---

// FactByID returns one fact (empty ID → error).
func (m *Mnemopi) FactByID(ctx context.Context, id int64) (Fact, error) {
	if m.Off() {
		return Fact{}, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	db, err := m.handle()
	if err != nil {
		return Fact{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	banks, err := m.readableBanks(db)
	if err != nil {
		return Fact{}, err
	}
	if len(banks) == 0 {
		return Fact{}, fmt.Errorf("memory: no fact #%d in %s", id, m.bankLabelScope())
	}
	rows, err := db.Query(`SELECT id FROM facts WHERE id = ? AND bank_id IN (`+placeholders(len(banks))+`)`,
		append([]any{id}, intsToAny(banks)...)...)
	ids, err := scanIDs(rows, err)
	if err != nil {
		return Fact{}, err
	}
	if len(ids) == 0 {
		return Fact{}, fmt.Errorf("memory: no fact #%d in %s", id, m.bankLabelScope())
	}
	facts, err := m.fetchFacts(db, ids)
	if err != nil {
		return Fact{}, err
	}
	return facts[0], nil
}

func (m *Mnemopi) bankLabelScope() string {
	scope, project, tag := m.bankKey()
	return Bank{Scope: scope, Project: project, Tag: tag}.Label() + " or the global bank"
}

// MemoryEdit applies a bounded edit to a stored fact. Bounds are enforced
// here, not only in the tool schema: the store is the trust boundary.
// Facts outside the readable banks are refused (a project cannot edit
// another project's memory).
func (m *Mnemopi) MemoryEdit(ctx context.Context, e FactEdit) (Fact, error) {
	if m.Off() {
		return Fact{}, fmt.Errorf("memory: mnemopi backend off (set memory: mnemopi in settings)")
	}
	if e.ID <= 0 {
		return Fact{}, fmt.Errorf("memory_edit: id is required")
	}
	if e.Text == nil && e.Tags == nil && e.Kind == nil && !e.Delete {
		return Fact{}, fmt.Errorf("memory_edit: nothing to change (give text, tags, kind, or delete: true)")
	}
	db, err := m.handle()
	if err != nil {
		return Fact{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	banks, err := m.readableBanks(db)
	if err != nil {
		return Fact{}, err
	}
	var bankID int64
	if len(banks) > 0 {
		args := append([]any{e.ID}, intsToAny(banks)...)
		if err := db.QueryRow(`SELECT bank_id FROM facts WHERE id = ? AND bank_id IN (`+placeholders(len(banks))+`)`, args...).Scan(&bankID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Fact{}, fmt.Errorf("memory_edit: fact #%d is not in %s", e.ID, m.bankLabelScope())
			}
			return Fact{}, err
		}
	} else {
		return Fact{}, fmt.Errorf("memory_edit: fact #%d is not in %s", e.ID, m.bankLabelScope())
	}
	if e.Delete {
		if err := m.deleteFact(db, e.ID); err != nil {
			return Fact{}, err
		}
		return Fact{ID: e.ID, Kind: "deleted"}, nil
	}
	if e.Text != nil {
		text := collapseWS(*e.Text)
		if text == "" {
			return Fact{}, fmt.Errorf("memory_edit: text must not be empty (use delete: true)")
		}
		text = capChars(text, maxFactChars)
		if _, err := db.Exec(`UPDATE facts SET text = ?, updated_at = ? WHERE id = ?`, text, time.Now().Unix(), e.ID); err != nil {
			return Fact{}, err
		}
		if _, err := db.Exec(`DELETE FROM facts_fts WHERE rowid = ?`, e.ID); err != nil {
			return Fact{}, err
		}
		if _, err := db.Exec(`INSERT INTO facts_fts (rowid, text) VALUES (?, ?)`, e.ID, text); err != nil {
			return Fact{}, err
		}
	}
	if e.Kind != nil {
		switch *e.Kind {
		case KindFact, KindLesson, KindReflection, KindSummary:
			if _, err := db.Exec(`UPDATE facts SET kind = ?, updated_at = ? WHERE id = ?`, *e.Kind, time.Now().Unix(), e.ID); err != nil {
				return Fact{}, err
			}
		default:
			return Fact{}, fmt.Errorf("memory_edit: unknown kind %q (fact|lesson|reflection|summary)", *e.Kind)
		}
	}
	if e.Tags != nil {
		tags := normalizeTags(*e.Tags)
		if _, err := db.Exec(`DELETE FROM fact_tags WHERE fact_id = ?`, e.ID); err != nil {
			return Fact{}, err
		}
		for _, tg := range tags {
			if _, err := db.Exec(`INSERT OR IGNORE INTO fact_tags (fact_id, tag) VALUES (?, ?)`, e.ID, tg); err != nil {
				return Fact{}, err
			}
		}
	}
	facts, err := m.fetchFacts(db, []int64{e.ID})
	if err != nil {
		return Fact{}, err
	}
	if len(facts) == 0 {
		return Fact{}, fmt.Errorf("memory_edit: fact #%d vanished", e.ID)
	}
	return facts[0], nil
}

// --- helpers ---

// placeholders builds "?, ?, ?" for an IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func intsToAny(ids []int64) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

func keysOf(m map[int64]float64) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// tokens splits text into lowercase word tokens (letters, digits, _).
func tokens(s string) []string {
	var out []string
	seen := map[string]bool{}
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		t := cur.String()
		cur.Reset()
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// ftsQuery builds an FTS5 MATCH expression from tokens: every token is
// quoted, so user text (quotes, dashes, NEAR, parens) can never turn into
// FTS syntax and error out the query.
func ftsQuery(toks []string) string {
	quoted := make([]string, 0, len(toks))
	for _, t := range toks {
		quoted = append(quoted, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// intentKind maps the query's intent words to the kind they ask for.
func intentKind(toks []string) string {
	lessonIntent := map[string]bool{
		"lesson": true, "lessons": true, "learned": true, "learn": true, "remember": true,
		"convention": true, "conventions": true, "preference": true, "preferences": true,
		"pitfall": true, "pitfalls": true, "gotcha": true, "mistake": true, "mistakes": true,
	}
	reflectionIntent := map[string]bool{"reflection": true, "reflections": true, "summary": true, "summaries": true}
	lesson, reflection := false, false
	for _, t := range toks {
		if lessonIntent[t] {
			lesson = true
		}
		if reflectionIntent[t] {
			reflection = true
		}
	}
	switch {
	case lesson:
		return KindLesson
	case reflection:
		return KindReflection
	default:
		return ""
	}
}

// normalizeTag lowercases and bounds one tag.
func normalizeTag(t string) string {
	t = strings.ToLower(collapseWS(t))
	t = strings.ReplaceAll(t, " ", "-")
	return capChars(t, maxTagChars)
}

// normalizeTags normalizes, dedupes and bounds a tag list.
func normalizeTags(tags []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tags {
		t = normalizeTag(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == maxTagsPerFact {
			break
		}
	}
	return out
}

// capChars truncates to max characters with an explicit marker, so the model
// can tell the text was clipped.
func capChars(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max < 32 {
		return s[:max]
	}
	return s[:max-16] + " […truncated]"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
