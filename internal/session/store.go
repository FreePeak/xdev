package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// Store is one open session: an append-only entry tree with a mutable leaf
// pointer, persisted as omp-compatible JSONL (PRD §3.2).
//
// Durability model (deliberately pragmatic): appends are synchronous
// in-memory plus writer hand-off with no fsync (bufio.Writer + Flush per
// append); Options.StrictFsync upgrades to fsync-per-append. New sessions
// stay memory-only until EnsureOnDisk (or the first assistant message with
// auto-persist enabled) — no junk files for aborted starts. Any persistence
// error is latched and rethrown on every later Append/Close; never silent.
type Store struct {
	mu sync.Mutex

	id          string // session UUID (header "id")
	cwd         string
	title       string
	headerTS    time.Time
	file        string // empty until the session is on disk
	entries     []Entry
	byID        map[string]Entry
	children    map[string][]string
	leaf        string // "" while the session is empty
	headerSeen  bool   // session header line consumed during load
	manualTitle bool   // title came from a manual rename, not auto-generated
	subagent    bool   // child session: titleSource stamps "subagent", never user-resumable

	w           *bufio.Writer
	f           *os.File
	strictFsync bool
	latchErr    error // first persistence error, rethrown forever
	closed      bool

	autoPath string // when set, the first assistant message persists here
	autoOpts Options

	// Windowed materialization (M8): a long session's pre-boundary history
	// is already summarized into a compaction entry, so retaining its
	// entries costs memory the context will never read. During load the
	// store drops everything before the latest boundary once the boundary
	// is known. The FILE is untouched — reopening after a boundary-free
	// append still sees the full history — only the in-memory view is
	// bounded, which is the §IV.1 budget promise.
	windowCut   int  // index into entries of the oldest retained entry
	windowed    bool // a boundary has been applied
	entriesSeen int  // total entries parsed (diagnostics)
}

// WindowStats reports the load-windowing outcome (diagnostics + the M8
// audit harness).
func (s *Store) WindowStats() (retained, seen int, windowed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), s.entriesSeen, s.windowed
}

// Options tunes Store persistence behavior.
type Options struct {
	// StrictFsync fsyncs after every append (paranoid durability). Without
	// it, appends Flush the bufio writer only — omp's pragmatic model.
	StrictFsync bool
	// ParentSession stamps parentSession into the materialized header
	// (subagent children; empty for roots). Resume paths use it to keep
	// child sessions out of the user's --continue/--resume candidates.
	ParentSession string
}

// OpenMem creates a brand-new memory-only session. Nothing touches the file
// system until EnsureOnDisk or EnableAutoPersist fires.
func OpenMem(cwd, title string) *Store {
	return &Store{
		id:       newUUID(),
		cwd:      cwd,
		title:    title,
		byID:     map[string]Entry{},
		children: map[string][]string{},
	}
}

// Open loads an existing .jsonl session file: it skips the title slot,
// parses the session header, streams entries in file order, builds the
// id→entry map and children index, and replays the leaf pointer forward
// (each entry becomes the leaf; a "branch" marker re-points the leaf at its
// target).
//
// Unknown entry types become opaque UnknownEntry chain nodes so parent
// chains through foreign records (omp writes title_change /
// thinking_level_change / service_tier_change mid-chain) stay walkable;
// their lines are never rewritten or removed. Structurally broken lines
// fail the load loudly.
func Open(path string) (*Store, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	s := &Store{file: path, byID: map[string]Entry{}, children: map[string][]string{}}
	r := bufio.NewReaderSize(f, 64*1024)
	lineNo := 0
	for {
		line, rerr := r.ReadBytes('\n')
		if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
			lineNo++
			if err := s.loadLine(trimmed, lineNo); err != nil {
				return nil, fmt.Errorf("session: %s line %d: %w", path, lineNo, err)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("session: read %s: %w", path, rerr)
		}
	}
	return s, nil
}

// loadLine ingests one raw JSONL line during Open. Lines 1 and 2 are the
// fixed-width title slot and the session header (tolerated if absent).
func (s *Store) loadLine(line []byte, no int) error {
	if no == 1 {
		if title, ok := ParseTitleSlot(line); ok {
			s.title = title
			return nil
		}
		// No slot: fall through and try the header on this line.
		if h, ok := ParseHeader(line); ok {
			s.adoptHeader(h)
			s.headerSeen = true
			return nil
		}
	} else if no == 2 || !s.headerSeen {
		if h, ok := ParseHeader(line); ok {
			s.adoptHeader(h)
			s.headerSeen = true
			return nil
		}
	}

	e, err := ParseEntry(line)
	if errors.Is(err, ErrUnknownEntryType) {
		// Keep the line as an opaque chain node (it stays on disk
		// untouched). An unparseable envelope cannot anchor anything, so
		// such a line is skipped entirely.
		env, envErr := ParseEnvelope(line)
		if envErr != nil || env.ID == "" {
			return nil
		}
		var t struct {
			Type string `json:"type"`
		}
		json.Unmarshal(line, &t) // best effort; type name only decorates
		e = &UnknownEntry{Env: env, EntryType: t.Type, Raw: json.RawMessage(bytes.Clone(line))}
	} else if err != nil {
		return err
	}

	env := e.Envelope()
	s.entries = append(s.entries, e)
	s.entriesSeen++
	s.byID[env.ID] = e
	if env.ParentID != "" {
		s.children[env.ParentID] = append(s.children[env.ParentID], env.ID)
	}
	// Forward leaf replay: a branch marker re-points the leaf at its target;
	// every other entry becomes the leaf itself.
	if c, ok := e.(*CustomEntry); ok && c.CustomType == TypeBranch {
		if to, _ := c.Data["to"].(string); to != "" && s.byID[to] != nil {
			s.leaf = to
			return nil
		}
	}
	s.leaf = env.ID
	s.maybeWindow()
	return nil
}

// maybeWindow drops entries the next context build can never read: those
// strictly before the latest reset boundary / compaction window. It runs
// after every append but only rebuilds indexes when it actually cuts, so
// the amortized cost is one rebuild per boundary.
func (s *Store) maybeWindow() {
	cut := -1
	for i := len(s.entries) - 1; i >= 0; i-- {
		switch s.entries[i].(type) {
		case *ResetBoundaryEntry:
			cut = i + 1
		case *CompactionEntry:
			// Keep the compaction entry itself: its summary is the first
			// thing a rebuilt context emits. Everything before it is dead.
			cut = i
		}
		if cut >= 0 {
			break
		}
	}
	// Prune only in batches: rebuilding the indexes per entry would make
	// loading quadratic.
	if cut <= 0 || cut-s.windowCut < loadWindowBatch {
		return
	}
	s.entries = append([]Entry(nil), s.entries[cut:]...)
	s.windowCut = 0
	s.windowed = true
	s.reindex()
	// A cut can orphan the leaf if the leaf lived before the boundary;
	// the last entry is the chronological tail, so it stays the leaf.
	if len(s.entries) > 0 {
		s.leaf = s.entries[len(s.entries)-1].Envelope().ID
	}
}

// loadWindowBatch is how many droppable entries accumulate before the
// index rebuild pays for itself.
const loadWindowBatch = 512

// reindex rebuilds byID/children from the retained entries.
func (s *Store) reindex() {
	s.byID = make(map[string]Entry, len(s.entries))
	s.children = make(map[string][]string, len(s.entries))
	for _, e := range s.entries {
		env := e.Envelope()
		s.byID[env.ID] = e
		if env.ParentID != "" {
			s.children[env.ParentID] = append(s.children[env.ParentID], env.ID)
		}
	}
}

func (s *Store) adoptHeader(h SessionHeader) {
	s.id = h.ID
	s.cwd = h.CWD
	s.headerTS = h.Timestamp
	if s.title == "" {
		s.title = h.Title
	}
}

// EnableAutoPersist records where the session file should materialize: the
// first assistant message appended afterwards triggers EnsureOnDisk (omp's
// "no junk files for aborted starts" rule).
func (s *Store) EnableAutoPersist(path string, opts Options) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoPath = path
	s.autoOpts = opts
}

// ID returns the session UUID.
func (s *Store) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// CWD returns the session's working directory.
func (s *Store) CWD() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

// Title returns the current session title.
func (s *Store) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// SetTitleSourceSubagent marks this store as a subagent child. The marker
// reaches disk with the next materialized title slot/header write.
func (s *Store) SetTitleSourceSubagent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subagent = true
}

// SetTitle updates the in-memory title. The on-disk title slot is only
// written when a new file is materialized; existing slots are rewritten in
// place by a later rename pass, never by Append.
func (s *Store) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title = title
}

// Path returns the session file path, empty until the session is on disk.
func (s *Store) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file
}

// LeafID returns the current leaf entry id ("" for an empty session).
func (s *Store) LeafID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaf
}

// Entries returns all entries in file order (a copy; the store's slice is
// never handed out).
func (s *Store) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Children returns the ids of entries whose ParentID is id, in file order.
func (s *Store) Children(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.children[id]))
	copy(out, s.children[id])
	return out
}

// Entry returns the entry with the given id, or nil.
func (s *Store) Entry(id string) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// Append adds e to the tree: ParentID is set to the current leaf, a missing
// ID/timestamp is assigned, the entry is appended in memory and becomes the
// leaf — and, once the session is on disk, one JSON line is written and
// flushed. Persistence errors latch.
func (s *Store) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(e)
}

func (s *Store) appendLocked(e Entry) error {
	if s.closed {
		return errors.New("session: append on closed store")
	}
	if s.latchErr != nil {
		return s.latchErr
	}
	// Memory-only sessions materialize on the first assistant message.
	if s.w == nil && s.autoPath != "" {
		if me, ok := e.(*MessageEntry); ok && me.Message.Role == ai.RoleAssistant {
			if _, err := s.ensureOnDiskLocked(s.autoPath, s.autoOpts); err != nil {
				return err
			}
		}
	}

	env := e.Envelope()
	if env.ID == "" {
		env.ID = NewID()
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}
	env.Type = entryTypeOf(e)
	env.ParentID = s.leaf
	setEnvelope(e, env)

	s.entries = append(s.entries, e)
	s.byID[env.ID] = e
	if env.ParentID != "" {
		s.children[env.ParentID] = append(s.children[env.ParentID], env.ID)
	}
	s.leaf = env.ID

	if s.w == nil {
		if err := s.openWriterLocked(); err != nil {
			return err
		}
	}
	if s.w == nil {
		return nil // still memory-only
	}
	return s.appendLineLocked(e, env.ID)
}

// entryTypeOf reports the canonical wire "type" for a concrete entry.
func entryTypeOf(e Entry) string {
	switch t := e.(type) {
	case *MessageEntry:
		return TypeMessage
	case *ModelChangeEntry:
		return TypeModelChange
	case *CompactionEntry:
		return TypeCompaction
	case *BranchSummaryEntry:
		return TypeBranchSummary
	case *ResetBoundaryEntry:
		return TypeResetBoundary
	case *CustomEntry:
		return TypeCustom
	case *UnknownEntry:
		return t.EntryType
	case *GoalUpdatedEntry:
		return TypeGoalUpdated
	default:
		return ""
	}
}

// setEnvelope stamps e with env; every concrete entry type carries Env.
func setEnvelope(e Entry, env Envelope) {
	switch t := e.(type) {
	case *MessageEntry:
		t.Env = env
	case *ModelChangeEntry:
		t.Env = env
	case *CompactionEntry:
		t.Env = env
	case *BranchSummaryEntry:
		t.Env = env
	case *ResetBoundaryEntry:
		t.Env = env
	case *CustomEntry:
		t.Env = env
	case *UnknownEntry:
		t.Env = env
	case *GoalUpdatedEntry:
		t.Env = env
	}
}

// appendLineLocked writes one marshaled entry and flushes (fsync optional).
func (s *Store) appendLineLocked(e Entry, id string) error {
	line, err := MarshalEntry(e)
	if err != nil {
		s.latchErr = fmt.Errorf("session: marshal entry %s: %w", id, err)
		return s.latchErr
	}
	if _, err := s.w.Write(line); err != nil {
		s.latchErr = fmt.Errorf("session: write entry %s: %w", id, err)
		return s.latchErr
	}
	if err := s.w.WriteByte('\n'); err != nil {
		s.latchErr = fmt.Errorf("session: write entry %s: %w", id, err)
		return s.latchErr
	}
	if err := s.w.Flush(); err != nil {
		s.latchErr = fmt.Errorf("session: flush entry %s: %w", id, err)
		return s.latchErr
	}
	if s.strictFsync {
		if err := s.f.Sync(); err != nil {
			s.latchErr = fmt.Errorf("session: fsync entry %s: %w", id, err)
			return s.latchErr
		}
	}
	return nil
}

// openWriterLocked lazily attaches an append-mode writer to an existing
// file (Open keeps the handle closed so read-only listings never need write
// permission).
func (s *Store) openWriterLocked() error {
	if s.file == "" {
		return nil
	}
	f, err := os.OpenFile(s.file, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.latchErr = fmt.Errorf("session: open for append %s: %w", s.file, err)
		return s.latchErr
	}
	s.f = f
	s.w = bufio.NewWriter(f)
	return nil
}

// Branch moves the leaf pointer to entryID and persists a "branch" custom
// marker (customType "branch", data {"to":entryID}) so Open reconstructs the
// same leaf after a restart. Nothing is mutated or deleted.
// Tree renders the entry graph as an indented text tree (M10 #11: /tree).
// Each line is "<shortID> <type> <summary>" indented by depth; the leaf
// pointer is marked with "→ ".
func (s *Store) Tree() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build adjacency: id → children.
	children := map[string][]string{}
	var roots []string
	for _, e := range s.entries {
		env := e.Envelope()
		if env.ParentID == "" {
			roots = append(roots, env.ID)
		} else {
			children[env.ParentID] = append(children[env.ParentID], env.ID)
		}
	}

	var b strings.Builder
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		e := s.byID[id]
		if e == nil {
			return
		}
		env := e.Envelope()
		prefix := strings.Repeat("  ", depth)
		marker := "  "
		if id == s.leaf {
			marker = "→ "
		}
		fmt.Fprintf(&b, "%s%s%s %s %s\n", prefix, marker, shortID(id), env.Type, summarize(e))
		for _, c := range children[id] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return b.String()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// summarize returns a one-line preview of an entry for the tree view.
func summarize(e Entry) string {
	env := e.Envelope()
	switch t := e.(type) {
	case *MessageEntry:
		txt := t.Message.Text()
		if len(txt) > 60 {
			txt = txt[:60] + "…"
		}
		return txt
	case *CompactionEntry:
		return "(compaction)"
	case *ResetBoundaryEntry:
		return "(reset boundary)"
	case *BranchSummaryEntry:
		return "(branch summary)"
	case *ModelChangeEntry:
		return t.Model
	case *CustomEntry:
		return "(" + t.CustomType + ")"
	case *GoalUpdatedEntry:
		return "goal " + t.Goal.Status
	default:
		return "(" + env.Type + ")"
	}
}

// Branches returns the branch points in the tree (entry IDs that have more
// than one child), newest first.
func (s *Store) Branches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	children := map[string]int{}
	var roots []string
	for _, e := range s.entries {
		env := e.Envelope()
		if env.ParentID == "" {
			roots = append(roots, env.ID)
		} else {
			children[env.ParentID]++
		}
	}
	seen := map[string]bool{}
	var branches []string
	for id, n := range children {
		if n > 1 && !seen[id] {
			seen[id] = true
			branches = append(branches, id)
		}
	}
	for _, r := range roots {
		if children[r] > 1 && !seen[r] {
			seen[r] = true
			branches = append(branches, r)
		}
	}
	return branches
}

func (s *Store) Branch(entryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("session: branch on closed store")
	}
	if s.latchErr != nil {
		return s.latchErr
	}
	if s.byID[entryID] == nil {
		return fmt.Errorf("session: branch: unknown entry %q", entryID)
	}
	marker := &CustomEntry{CustomType: TypeBranch, Data: map[string]any{"to": entryID}}
	if err := s.appendLocked(marker); err != nil {
		return err
	}
	// The marker itself is not the leaf; the branch target is.
	s.leaf = entryID
	return nil
}

// ResetLeaf appends a payload-free reset_boundary entry and makes it the
// leaf, starting a new root segment (the /clear marker).
func (s *Store) ResetLeaf() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("session: reset on closed store")
	}
	if s.latchErr != nil {
		return s.latchErr
	}
	return s.appendLocked(&ResetBoundaryEntry{})
}

// EnsureOnDisk materializes the session file: fixed-width title slot,
// session header, then every in-memory entry — all at once, so a brand-new
// session that never persists leaves no trace. Afterwards appends stream.
// Idempotent: once on disk, returns the existing path. Errors latch.
func (s *Store) EnsureOnDisk(path string, opts Options) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureOnDiskLocked(path, opts)
}

func (s *Store) ensureOnDiskLocked(path string, opts Options) (string, error) {
	if s.closed {
		return "", errors.New("session: ensure on closed store")
	}
	if s.latchErr != nil {
		return "", s.latchErr
	}
	if s.file != "" {
		return s.file, nil // already materialized
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.latchErr = fmt.Errorf("session: mkdir: %w", err)
		return "", s.latchErr
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		s.latchErr = fmt.Errorf("session: create %s: %w", path, err)
		return "", s.latchErr
	}
	s.file = path
	s.f = f
	s.strictFsync = opts.StrictFsync

	w := bufio.NewWriter(f)
	write := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			s.latchErr = fmt.Errorf("session: write session file: %w", err)
			return s.latchErr
		}
		return nil
	}
	updated := time.Now().UTC()
	if s.headerTS.IsZero() {
		s.headerTS = updated
	}
	if err := write(MarshalTitleSlot(s.title, s.titleSourceFor(), updated)); err != nil {
		return "", err
	}
	if err := write(MarshalHeader(SessionHeader{
		Version: 3, ID: s.id, Timestamp: s.headerTS, ParentSession: opts.ParentSession,
		CWD: s.cwd, Title: s.title, TitleSource: s.titleSourceFor(),
	})); err != nil {
		return "", err
	}
	for _, e := range s.entries {
		line, err := MarshalEntry(e)
		if err != nil {
			s.latchErr = fmt.Errorf("session: marshal entry: %w", err)
			return "", s.latchErr
		}
		if err := write(line); err != nil {
			return "", err
		}
		if err := write(nl); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		s.latchErr = fmt.Errorf("session: flush session file: %w", err)
		return "", s.latchErr
	}
	if s.strictFsync {
		if err := f.Sync(); err != nil {
			s.latchErr = fmt.Errorf("session: fsync session file: %w", err)
			return "", s.latchErr
		}
	}
	s.w = w
	return s.file, nil
}

var nl = []byte{'\n'}

func titleSource(manual bool) string {
	if manual {
		return TitleSourceManual
	}
	return TitleSourceAuto
}

// titleSourceFor resolves the slot/header source for this store's flags.
func (s *Store) titleSourceFor() string {
	if s.subagent {
		return TitleSourceSubagent
	}
	return titleSource(s.manualTitle)
}

// Close flushes and releases the underlying file. A latched error rethrows.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return s.latchErr
	}
	s.closed = true
	if s.f == nil {
		return s.latchErr
	}
	if s.w != nil {
		if err := s.w.Flush(); err != nil && s.latchErr == nil {
			s.latchErr = fmt.Errorf("session: final flush: %w", err)
		}
	}
	if err := s.f.Close(); err != nil && s.latchErr == nil {
		s.latchErr = fmt.Errorf("session: close file: %w", err)
	}
	s.w = nil
	s.f = nil
	return s.latchErr
}

// newUUID returns a random RFC 4122 version-4 UUID string (crypto/rand; no
// external dependency).
// NewSessionID mints a fresh session uuid (exported for fork callers).
func NewSessionID() string { return newUUID() }

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Unrecoverable in practice; deterministic fallback keeps the
		// header shape valid.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// stringsSplitLines splits s on '\n', drops a trailing empty piece, and
// trims '\r' (session files may be CRLF on some filesystems).
func stringsSplitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}
