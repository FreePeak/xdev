package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// SharpShooter is the friction-gated decision-file memory backend (M15 #73):
// instead of summarizing whole sessions, it watches the session stream for
// friction — the user restating a preference, or an instruction that has to
// follow a failed turn — and, once a session crosses the detector threshold,
// consolidates the raw quotes into ONE decision bullet through the smol-role
// seam and appends it to the matching decision file under memories/.
//
// It implements the same Store contract as the local Backend, so Memory
// Guidance injection, the memory:// read seam, the learn tool and /memory all
// work unchanged when memory.backend is sharpshooter.
type SharpShooter struct {
	// Dir is the memories directory holding the decision files. Empty = off.
	Dir string
	// Complete is the smol-role consolidation seam: prompt in, one JSON
	// decision out. nil writes the strongest friction quote verbatim instead
	// (a deliberate degradation: no model, no synthesis).
	Complete func(ctx context.Context, prompt string) (string, error)
	// Session stamps the frontmatter source-session. Empty = "unknown".
	Session string
	// Threshold is the friction score that proposes a decision (default
	// DefaultFrictionThreshold).
	Threshold int
	// MaxBullets caps the bullets kept per decision file (default
	// DefaultMaxBullets).
	MaxBullets int
	// MaxFileBytes caps one decision file (default DefaultDecisionMaxBytes).
	MaxFileBytes int
	// SummaryCapChars bounds the injected summary (default
	// DefaultSummaryCapChars, the same budget the local backend uses).
	SummaryCapChars int
	// LeaseTTL bounds the cross-process consolidation lease (default
	// DefaultLeaseTTL — the same lease algorithm the pipeline uses).
	LeaseTTL time.Duration
	// OnError receives background consolidation failures; nil is silent.
	OnError func(error)

	mu           sync.Mutex
	det          *FrictionDetector
	consolidated bool // at most one consolidation per session
	wg           sync.WaitGroup
}

// decisionScopes is the bounded set of decision files sharpshooter maintains,
// one per durable decision axis.
//
// ponytail: a fixed trio, not a discovered set — an invented scope name is a
// file nothing reads. The upgrade path is a settings-declared list plus a
// scope-per-file model pass.
var decisionScopes = []string{"architecture", "product", "style"}

const (
	// DefaultFrictionThreshold is the cumulative friction score that
	// proposes a decision.
	DefaultFrictionThreshold = 2
	// DefaultMaxBullets bounds the bullets kept per decision file.
	DefaultMaxBullets = 20
	// DefaultDecisionMaxBytes bounds one decision file (~8 KiB).
	DefaultDecisionMaxBytes = 8192
	// maxDecisionBulletChars bounds one consolidated decision bullet.
	maxDecisionBulletChars = 600
	// maxFrictionKeys / maxFrictionQuotes bound what one session's detector
	// retains: user turns are arbitrary text, and a long session must not grow
	// the process (the <100 MB RSS budget) with quotes nobody will read.
	//
	// ponytail: past the caps the newest unseen statements stop being tracked,
	// so a session that talks about >512 distinct things loses repeat
	// detection for the tail. The upgrade path is a rolling window over the
	// last N turns instead of a per-session key set.
	maxFrictionKeys   = 512
	maxFrictionQuotes = 12
	// sharpLeaseFile is this backend's lease path inside the memories dir.
	sharpLeaseFile = "sharpshooter.lease"
)

// Turn is one observed user turn of the session stream.
type Turn struct {
	// Text is what the user said.
	Text string
	// AfterFailure marks a turn that immediately followed a failed one (an
	// aborted run, a provider error, a turn that produced no reply).
	AfterFailure bool
}

// FrictionDetector scores correction signals across one session. Two
// patterns count:
//
//	repeat  (+2) the user states the same preference or rule again. The
//	             first statement did not stick; the restatement is friction.
//	failure (+1) an instruction has to follow a failed turn.
//
// The score is cumulative for the session and Observe reports the turn that
// crossed Threshold. Detection is deterministic and model-free on purpose: it
// only decides WHEN to consolidate, never WHAT the decision is.
type FrictionDetector struct {
	// Threshold is the score that triggers a proposal (default
	// DefaultFrictionThreshold).
	Threshold int

	counts map[string]int
	score  int
	quotes []string
}

// NewFrictionDetector builds a detector; a non-positive threshold means the
// default.
func NewFrictionDetector(threshold int) *FrictionDetector {
	if threshold <= 0 {
		threshold = DefaultFrictionThreshold
	}
	return &FrictionDetector{Threshold: threshold, counts: map[string]int{}}
}

// Observe records one turn; it reports that this turn made the session cross
// the threshold.
func (d *FrictionDetector) Observe(t Turn) bool {
	if d == nil {
		return false
	}
	if d.counts == nil {
		d.counts = map[string]int{}
	}
	key := normalizeTurn(t.Text)
	if key != "" {
		if _, known := d.counts[key]; known || len(d.counts) < maxFrictionKeys {
			d.counts[key]++
		}
		if d.counts[key] > 1 {
			d.score += 2
			d.quotes = appendQuote(d.quotes, t.Text)
		}
	}
	if t.AfterFailure {
		d.score++
		d.quotes = appendQuote(d.quotes, t.Text)
	}
	return d.score >= threshold(d.Threshold)
}

// Score is the friction accumulated so far (observability + tests).
func (d *FrictionDetector) Score() int {
	if d == nil {
		return 0
	}
	return d.score
}

// Quotes is the friction evidence, in observation order, deduplicated.
func (d *FrictionDetector) Quotes() []string {
	if d == nil {
		return nil
	}
	return append([]string(nil), d.quotes...)
}

func threshold(v int) int {
	if v <= 0 {
		return DefaultFrictionThreshold
	}
	return v
}

// normalizeTurn folds a turn into the repeat key: case, whitespace and
// trailing sentence punctuation are not part of a restated rule.
//
// ponytail: verbatim-after-normalization matching only, so a reworded
// restatement escapes the detector. The upgrade path is token-overlap
// similarity (or handing candidate pairs to the smol seam) once a reworded
// repeat is observed in practice.
func normalizeTurn(s string) string {
	s = capTextHint(collapseWS(strings.ToLower(s)), maxDecisionBulletChars, "")
	return strings.TrimRight(s, " .!?,;:")
}

func appendQuote(quotes []string, text string) []string {
	text = capTextHint(collapseWS(text), maxDecisionBulletChars, "")
	if text == "" {
		return quotes
	}
	for _, q := range quotes {
		if q == text {
			return quotes
		}
	}
	if len(quotes) >= maxFrictionQuotes {
		return quotes // the oldest evidence is enough for one decision
	}
	return append(quotes, text)
}

// ErrNoFriction reports that a consolidation was asked for with nothing to
// consolidate (the friction gate has not opened).
var ErrNoFriction = errors.New("memory: sharpshooter has no friction to consolidate")

// Observe feeds one user turn into the friction detector. When the session
// crosses the threshold it starts at most ONE background consolidation for
// that session (the gate is session-scoped, not per-turn) and reports true.
// Print mode must call Wait before exiting; an interactive session can let the
// pass finish on its own.
func (s *SharpShooter) Observe(t Turn) bool {
	if s.Off() {
		return false
	}
	s.mu.Lock()
	if s.det == nil {
		s.det = NewFrictionDetector(s.Threshold)
	}
	triggered := s.det.Observe(t)
	start := triggered && !s.consolidated
	if start {
		s.consolidated = true
	}
	s.mu.Unlock()
	if !start {
		return triggered
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.Consolidate(context.Background()); err != nil {
			s.report(err)
		}
	}()
	return true
}

// Wait blocks until the in-flight background consolidation finished.
func (s *SharpShooter) Wait() {
	if s == nil {
		return
	}
	s.wg.Wait()
}

// Consolidate runs one consolidation pass over the friction already observed:
// the smol seam (or the verbatim fallback) turns the quotes into one decision
// bullet appended to the matching decision file. Concurrent passes — this
// process or another — are rejected with ErrLeaseHeld: the same lease-file
// algorithm the memory pipeline uses, on this backend's own lease path.
func (s *SharpShooter) Consolidate(ctx context.Context) error {
	if s.Off() {
		return nil
	}
	quotes := s.friction()
	if len(quotes) == 0 {
		return ErrNoFriction
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	release, _, err := acquireFileLease(filepath.Join(s.Dir, sharpLeaseFile), s.leaseTTL())
	if err != nil {
		return err
	}
	defer release()
	dec, err := s.decide(ctx, quotes)
	if err != nil {
		return err
	}
	return s.appendBullet(dec.Scope, dec.Bullet)
}

func (s *SharpShooter) friction() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.det.Quotes()
}

// Replay seeds the detector from the persisted session's user turns, so a
// session resumed in a new process (--continue, or a restarted TUI) keeps its
// friction history instead of restarting the count at zero. Turns that already
// crossed the threshold leave it open: the next observed turn proposes the
// decision, quoting the whole session.
//
// The transcript records no failure marks, so replayed turns score as plain
// statements (a repeat); the live Observe call is what carries AfterFailure.
func (s *SharpShooter) Replay(st *session.Store) error {
	if s.Off() || st == nil {
		return nil
	}
	built, err := session.BuildContext(st.Entries(), st.LeafID(), session.SystemPrompt{})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.det == nil {
		s.det = NewFrictionDetector(s.Threshold)
	}
	for _, m := range built.Messages {
		if m.Role != ai.RoleUser {
			continue
		}
		s.det.Observe(Turn{Text: m.Text()})
	}
	return nil
}

// decision is one consolidation reply.
type decision struct {
	Scope  string `json:"scope"`
	Bullet string `json:"bullet"`
}

// decide turns raw friction quotes into one scope + bullet. Without a model
// seam the strongest quote is filed as-is (still a decision worth keeping — it
// is the user's own words — just unsynthesized).
func (s *SharpShooter) decide(ctx context.Context, quotes []string) (decision, error) {
	if s.Complete == nil {
		return decision{Scope: scopeFor(quotes[0]), Bullet: quotes[0]}, nil
	}
	out, err := s.Complete(ctx, frictionPrompt(quotes))
	if err != nil {
		return decision{}, err
	}
	d, err := parseDecision(out)
	if err != nil {
		return decision{}, err
	}
	if d.Bullet == "" {
		return decision{}, errors.New("memory: sharpshooter consolidation returned no bullet")
	}
	d.Scope = scopeFor(d.Scope + " " + d.Bullet)
	return d, nil
}

const frictionInstructions = `You maintain the project decision files of a coding agent. In this session the user had to correct themselves or repeat themselves — the quotes below are that friction.

Return ONLY a JSON object, no prose:
{"scope": "architecture|product|style", "bullet": "..."}

scope  - architecture: how the system is built and why. product: what the project does and for whom. style: how work is done here (conventions, workflow, tooling).
bullet - ONE durable decision as a single imperative sentence, general enough to guide future sessions: state the rule the user was trying to establish, not the incident that produced it.
Never include secrets, credentials, file paths from this session, or session-specific detail. Return {"scope":"","bullet":""} when the friction taught nothing durable.`

func frictionPrompt(quotes []string) string {
	var b strings.Builder
	b.WriteString(frictionInstructions)
	b.WriteString("\n\nFriction quotes (chronological):\n")
	for _, q := range quotes {
		fmt.Fprintf(&b, "- %s\n", capTextHint(q, maxDecisionBulletChars, ""))
	}
	return b.String()
}

func parseDecision(s string) (decision, error) {
	var d decision
	body, ok := jsonObject(s)
	if !ok {
		return d, errors.New("memory: sharpshooter consolidation returned no JSON object")
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return d, fmt.Errorf("memory: sharpshooter consolidation JSON: %w", err)
	}
	d.Scope = strings.ToLower(strings.TrimSpace(d.Scope))
	d.Bullet = strings.TrimSpace(collapseWS(d.Bullet))
	return d, nil
}

// scopeFor classifies text into one of the bounded decision scopes, defaulting
// to style.md (the widest axis: how work is done here).
//
// ponytail: keyword matching, not a model call — a misclassified decision
// still lands in a readable decision file, and the model's own scope answer is
// what usually decides. The upgrade path is a dedicated classification pass.
func scopeFor(text string) string {
	low := strings.ToLower(text)
	best, bestHits := decisionScopes[len(decisionScopes)-1], 0
	for _, scope := range decisionScopes {
		hits := 0
		for _, kw := range decisionKeywords[scope] {
			if strings.Contains(low, kw) {
				hits++
			}
		}
		if hits > bestHits {
			best, bestHits = scope, hits
		}
	}
	return best
}

var decisionKeywords = map[string][]string{
	"architecture": {"architect", "design", "package", "module", "interface", "dependenc", "structure", "layer", "abstraction", "schema", "storage", "backend"},
	"product":      {"product", "user", "feature", "behavio", "ux", "scope", "requirement", "roadmap", "release"},
	"style":        {"style", "convention", "format", "naming", "commit", "gofmt", "lint", "test", "workflow", "process", "prefer", "always", "never"},
}

// --- Store: the memory seam ---

// Off reports whether the backend is disabled.
func (s *SharpShooter) Off() bool { return s == nil || s.Dir == "" }

// Ensure creates the memories directory (first use).
func (s *SharpShooter) Ensure() error {
	if s.Off() {
		return nil
	}
	return os.MkdirAll(s.Dir, 0o755)
}

// Summary returns the decision-file bodies (frontmatter dropped) as the
// injected guidance text, capped to the shared summary budget.
func (s *SharpShooter) Summary() string {
	if s.Off() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	for _, scope := range decisionScopes {
		if body := decisionBody(readFileOrEmpty(s.path(scope))); body != "" {
			parts = append(parts, body)
		}
	}
	return capTextHint(strings.TrimSpace(strings.Join(parts, "\n\n")), s.summaryCap(),
		"read the decision files under memory://root for the rest")
}

// GuidanceBlock wraps the summary in the Memory Guidance shape, or "" when
// nothing has been decided yet.
func (s *SharpShooter) GuidanceBlock() string { return guidanceBlock(s.Summary()) }

// Read resolves a memory:// URL to text:
//
//	memory://root                    the injected summary (what the prompt saw)
//	memory://root/architecture.md    one raw decision file
//	memory://root/product.md         "
//	memory://root/style.md           "
func (s *SharpShooter) Read(uri string) (string, error) {
	if s.Off() {
		return "", errors.New("memory: backend off (set memory: sharpshooter in settings)")
	}
	rest := strings.TrimPrefix(uri, "memory://")
	if rest == uri {
		return "", fmt.Errorf("memory: not a memory URL: %q", uri)
	}
	rest = strings.Trim(rest, "/")
	if rest == "" || rest == "root" {
		if sum := s.Summary(); sum != "" {
			return sum, nil
		}
		return "(no decision files yet)", nil
	}
	rest = strings.Trim(strings.TrimPrefix(rest, "root/"), "/")
	for _, scope := range decisionScopes {
		if rest == scope+".md" {
			return readFileOrEmpty(s.path(scope)), nil
		}
	}
	return "", fmt.Errorf("memory: unknown path %q (root, root/architecture.md, root/product.md, root/style.md)", rest)
}

// Stats reports the decision files for /memory stats.
func (s *SharpShooter) Stats() string {
	if s.Off() {
		return "memory: off"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "memory: sharpshooter\n  dir: %s", s.Dir)
	for _, scope := range decisionScopes {
		path := s.path(scope)
		fmt.Fprintf(&b, "\n  %s.md: %d bullets, %d bytes", scope, len(readBullets(readFileOrEmpty(path))), fileSize(path))
	}
	return b.String()
}

// Clear removes the decision files and this backend's lease. Missing files are
// not an error.
func (s *SharpShooter) Clear() error {
	if s.Off() {
		return errors.New("memory: backend off")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, scope := range decisionScopes {
		if err := os.Remove(s.path(scope)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = os.Remove(filepath.Join(s.Dir, sharpLeaseFile))
	return nil
}

// Paths exposes the memory directory and the decision files (diagnostics).
func (s *SharpShooter) Paths() (summary, lessons string) {
	if s.Off() {
		return "", ""
	}
	paths := make([]string, 0, len(decisionScopes))
	for _, scope := range decisionScopes {
		paths = append(paths, filepath.Join(s.Dir, scope+".md"))
	}
	return s.Dir, strings.Join(paths, ", ")
}

// SaveLesson files an explicit lesson as a decision bullet in the
// best-matching scope — the learn tool's landing zone, the parity counterpart
// of the local backend's learned.md.
func (s *SharpShooter) SaveLesson(text, context string) error {
	if s.Off() {
		return errors.New("memory: backend off")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("memory: lesson text is required")
	}
	if context != "" {
		text += " _(context: " + collapseWS(context) + ")_"
	}
	return s.appendBullet(scopeFor(text), text)
}

// --- decision files ---

func (s *SharpShooter) path(scope string) string { return filepath.Join(s.Dir, scope+".md") }

// appendBullet adds one bullet to a decision file, deduplicated, refreshing
// the frontmatter. The caps are ceilings on what the writer keeps: over them
// the OLDEST bullets are dropped, newest wins.
//
// ponytail: a file at its cap loses history instead of consolidating it, and a
// single bullet is always kept whole (bounded by maxDecisionBulletChars). The
// upgrade path is a merge pass over the file through the smol seam before the
// drop.
func (s *SharpShooter) appendBullet(scope, bullet string) error {
	if s.Off() {
		return errors.New("memory: backend off")
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	bullet = strings.TrimSpace(capTextHint(collapseWS(bullet), maxDecisionBulletChars, ""))
	if bullet == "" {
		return errors.New("memory: sharpshooter: empty decision bullet")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bullets := readBullets(readFileOrEmpty(s.path(scope)))
	for _, b := range bullets {
		if strings.EqualFold(b, bullet) {
			return nil // already decided; repeated friction adds nothing
		}
	}
	bullets = append(bullets, bullet)
	rendered := renderDecision(scope, bullets, s.Session, time.Now())
	for len(bullets) > 1 && (len(bullets) > s.maxBullets() || len(rendered) > s.maxFileBytes()) {
		bullets = bullets[1:]
		rendered = renderDecision(scope, bullets, s.Session, time.Now())
	}
	return writeFileAtomic(s.path(scope), rendered)
}

// renderDecision is the decision-file shape: YAML frontmatter naming the axis,
// the update time and the session that produced it, then the bullets.
func renderDecision(scope string, bullets []string, session string, at time.Time) string {
	if strings.TrimSpace(session) == "" {
		session = "unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nscope: %s\nupdated: %s\nsource-session: %s\n---\n\n",
		scope, at.UTC().Format(time.RFC3339), collapseWS(session))
	for _, it := range bullets {
		fmt.Fprintf(&b, "- %s\n", it)
	}
	return b.String()
}

// decisionBody strips the YAML frontmatter, leaving the bullet list.
func decisionBody(raw string) string {
	if !strings.HasPrefix(raw, "---") {
		return raw
	}
	end := strings.Index(raw[3:], "\n---")
	if end < 0 {
		return raw
	}
	return strings.TrimSpace(raw[3+end+4:])
}

// readBullets returns the "- " lines of a decision file body.
func readBullets(raw string) []string {
	var out []string
	for _, line := range strings.Split(decisionBody(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		if b := strings.TrimSpace(line[2:]); b != "" {
			out = append(out, b)
		}
	}
	return out
}

func writeFileAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- bounds ---

func (s *SharpShooter) leaseTTL() time.Duration {
	if s.LeaseTTL > 0 {
		return s.LeaseTTL
	}
	return DefaultLeaseTTL
}

func (s *SharpShooter) maxBullets() int {
	if s.MaxBullets > 0 {
		return s.MaxBullets
	}
	return DefaultMaxBullets
}

func (s *SharpShooter) maxFileBytes() int {
	if s.MaxFileBytes > 0 {
		return s.MaxFileBytes
	}
	return DefaultDecisionMaxBytes
}

func (s *SharpShooter) summaryCap() int {
	if s.SummaryCapChars > 0 {
		return s.SummaryCapChars
	}
	return DefaultSummaryCapChars
}

func (s *SharpShooter) report(err error) {
	if err != nil && s.OnError != nil {
		s.OnError(err)
	}
}
