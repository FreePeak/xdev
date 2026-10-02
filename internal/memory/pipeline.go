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

// Pipeline is the local backend's two-phase memory pipeline (M12 #13,
// research F1): phase 1 extracts candidate findings from the persisted
// sessions that changed since the last run, phase 2 consolidates them into
// MEMORY.md + learned.md through the same caps the prompt injection reads.
//
// Best-effort by construction: memory is heuristic context, so a failed
// extraction or consolidation must never break the session. Run reports
// errors; StartBackground (the session-end trigger) hands them to OnError.
//
// Concurrency is 1: within a process a mutex serializes runs, and across
// processes a lease file (refreshed by a heartbeat, stolen only when stale)
// keeps two xdev instances from consolidating at once.
type Pipeline struct {
	// Backend is the local store both phases write through. Required.
	Backend *Backend
	// DataDir is the agent data directory; sessions live under
	// DataDir/sessions. Required.
	DataDir string
	// Complete is the phase-1 (extraction) model seam: prompt in, text out.
	// The wiring point resolves the session model and streams one response;
	// nil (or an off backend) makes the whole pipeline a no-op.
	Complete func(ctx context.Context, prompt string) (string, error)
	// Consolidate is the phase-2 (consolidation) seam; nil falls back to
	// Complete so a single-model wiring needs one func.
	Consolidate func(ctx context.Context, prompt string) (string, error)

	// MaxStarts caps how many sessions one run extracts (default 8).
	MaxStarts int
	// ScanLimit caps how many listed sessions one run inspects (default 300).
	ScanLimit int
	// InputCap caps the transcript characters handed to one extraction call
	// (~4 chars/token; default 16000 ≈ 4000 tokens).
	InputCap int
	// LeaseTTL is how long a lease stays valid without a heartbeat
	// (default 180s).
	LeaseTTL time.Duration
	// MaxLessons caps the lessons one consolidation appends
	// (default DefaultLessonCap).
	MaxLessons int
	// OnError receives background failures; nil is silent.
	OnError func(error)

	mu sync.Mutex
}

// Pipeline defaults (research F1: bounded starts, scan limit, one lease).
const (
	DefaultMaxStarts     = 8
	DefaultScanLimit     = 300
	DefaultInputCapChars = 16000
	DefaultLeaseTTL      = 180 * time.Second
	leaseFile            = "pipeline.lease"
	watermarkFile        = "pipeline.watermark.json"
)

// maxLessonChars bounds one stored lesson (an unbounded model reply must not
// turn the lesson log into a prompt-size hazard).
const maxLessonChars = 600

// ErrLeaseHeld reports that another run (this process or another) holds the
// pipeline lease; the caller should drop the run, not retry.
var ErrLeaseHeld = errors.New("memory: pipeline run already in progress")

// Result reports what one run did (observability + tests).
type Result struct {
	Scanned      int  // listed sessions inspected
	Extracted    int  // sessions sent through phase 1
	Candidates   int  // findings phase 1 produced
	Lessons      int  // lessons appended to learned.md
	Consolidated bool // MEMORY.md rewritten
}

// Findings is one extraction response.
type Findings struct {
	Notes   []string `json:"notes"`
	Lessons []string `json:"lessons"`
}

func (p *Pipeline) off() bool {
	return p == nil || p.Backend.Off() || p.DataDir == "" || p.Complete == nil
}

func (p *Pipeline) consolidateFn() func(context.Context, string) (string, error) {
	if p.Consolidate != nil {
		return p.Consolidate
	}
	return p.Complete
}

func (p *Pipeline) maxStarts() int {
	if p.MaxStarts > 0 {
		return p.MaxStarts
	}
	return DefaultMaxStarts
}

func (p *Pipeline) scanLimit() int {
	if p.ScanLimit > 0 {
		return p.ScanLimit
	}
	return DefaultScanLimit
}

func (p *Pipeline) inputCap() int {
	if p.InputCap > 0 {
		return p.InputCap
	}
	return DefaultInputCapChars
}

func (p *Pipeline) leaseTTL() time.Duration {
	if p.LeaseTTL > 0 {
		return p.LeaseTTL
	}
	return DefaultLeaseTTL
}

func (p *Pipeline) maxLessons() int {
	if p.MaxLessons > 0 {
		return p.MaxLessons
	}
	return DefaultLessonCap
}

func (p *Pipeline) report(err error) {
	if err != nil && p.OnError != nil {
		p.OnError(err)
	}
}

// Run executes one bounded pipeline pass: phase 1 over at most MaxStarts
// changed sessions out of ScanLimit inspected, then phase 2 over everything
// phase 1 produced. A concurrent run (in-process or leased by another) is
// rejected with ErrLeaseHeld. Per-session failures are reported through
// OnError and skipped — the watermark stays put so the next run retries.
func (p *Pipeline) Run(ctx context.Context) (Result, error) {
	if p.off() {
		return Result{}, nil
	}
	if !p.mu.TryLock() {
		return Result{}, ErrLeaseHeld
	}
	defer p.mu.Unlock()

	if err := p.Backend.Ensure(); err != nil {
		return Result{}, err
	}
	release, runID, err := p.acquireLease()
	if err != nil {
		return Result{}, err
	}
	defer release()

	wm := p.loadWatermark()
	res, all, seen, err := p.extract(ctx, wm, runID)
	if serr := p.saveWatermark(wm, seen); serr != nil && err == nil {
		err = serr
	}
	if err != nil {
		return res, err
	}
	res.Candidates = len(all.Notes) + len(all.Lessons)
	if res.Candidates == 0 {
		return res, nil
	}
	if err := p.heartbeat(runID); err != nil {
		return res, err
	}
	return res, p.consolidate(ctx, all, &res)
}

// StartBackground runs Run in a goroutine — the optional session-end trigger.
// It returns immediately and never fails the caller: a lost race is expected
// (another process may be consolidating), so only real errors reach OnError.
func (p *Pipeline) StartBackground(ctx context.Context) {
	if p.off() {
		return
	}
	go func() {
		_, err := p.Run(ctx)
		if err == nil || errors.Is(err, ErrLeaseHeld) || errors.Is(err, context.Canceled) {
			return
		}
		p.report(err)
	}()
}

// extract is phase 1. It returns the merged findings and the set of session
// paths the listing still knows about (the watermark prunes the rest). Only
// infrastructure failures (listing, a lost lease, cancellation) are errors;
// one bad session is reported and skipped.
func (p *Pipeline) extract(ctx context.Context, wm *watermark, runID string) (Result, Findings, map[string]bool, error) {
	var res Result
	var all Findings
	seen := map[string]bool{}

	metas, err := session.List(p.DataDir)
	if err != nil {
		return res, all, seen, err
	}
	for _, m := range metas {
		if res.Scanned >= p.scanLimit() || res.Extracted >= p.maxStarts() {
			break
		}
		seen[m.Path] = true
		if m.TitleSource == "subagent" {
			continue // a child session is never the user's own work
		}
		res.Scanned++
		mark, known := wm.Sessions[m.Path]
		if known && !m.ModTime.After(time.Unix(0, mark.ModTime)) {
			continue // unchanged since the last run
		}
		if err := ctx.Err(); err != nil {
			return res, all, seen, err
		}
		f, extracted, err := p.extractSession(ctx, m.Path, mark, known)
		if err != nil {
			// The watermark stays where it was: retry next run.
			p.report(fmt.Errorf("memory: %s: %w", filepath.Base(m.Path), err))
			continue
		}
		if !extracted {
			wm.Sessions[m.Path] = sessionMark{ModTime: m.ModTime.UnixNano(), UserTurns: f.turns}
			continue // nothing new the last run had not seen
		}
		res.Extracted++
		wm.Sessions[m.Path] = sessionMark{ModTime: m.ModTime.UnixNano(), UserTurns: f.turns}
		all.Notes = append(all.Notes, f.findings.Notes...)
		all.Lessons = append(all.Lessons, f.findings.Lessons...)
		if err := p.heartbeat(runID); err != nil {
			return res, all, seen, err
		}
	}
	return res, all, seen, nil
}

// sessionState is one phase-1 outcome.
type sessionState struct {
	findings Findings
	turns    int
}

// extractSession runs phase 1 over one session file. extracted is false when
// the session holds no user turns the last run had not already seen.
func (p *Pipeline) extractSession(ctx context.Context, path string, prev sessionMark, known bool) (sessionState, bool, error) {
	st, err := session.Open(path)
	if err != nil {
		return sessionState{}, false, err
	}
	defer st.Close()
	built, err := session.BuildContext(st.Entries(), st.LeafID(), session.SystemPrompt{})
	if err != nil {
		return sessionState{}, false, err
	}
	turns := userTurns(built.Messages)
	if turns == 0 || (known && turns <= prev.UserTurns) {
		return sessionState{turns: turns}, false, nil
	}
	out, err := p.Complete(ctx, extractionPrompt(transcript(built.Messages, p.inputCap())))
	if err != nil {
		return sessionState{}, false, err
	}
	f, err := parseFindings(out)
	if err != nil {
		return sessionState{}, false, err
	}
	return sessionState{findings: f, turns: turns}, true, nil
}

// consolidate is phase 2: one model pass merging the extracted candidates
// into MEMORY.md and appending the new lessons to learned.md, both through
// the caps the injected summary already enforces.
func (p *Pipeline) consolidate(ctx context.Context, all Findings, res *Result) error {
	summaryPath, lessonsPath := p.Backend.paths()
	cur := capText(readFileOrEmpty(summaryPath), p.Backend.summaryCap())
	lessons := tailLines(readFileOrEmpty(lessonsPath), p.Backend.lessonCap())

	out, err := p.consolidateFn()(ctx, consolidationPrompt(cur, lessons, all, p.Backend.summaryCap()))
	if err != nil {
		return err
	}
	c, err := parseConsolidation(out)
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.Summary) != "" {
		if err := p.Backend.WriteSummary(capText(c.Summary, p.Backend.summaryCap())); err != nil {
			return err
		}
		res.Consolidated = true
	}
	for _, l := range c.Lessons {
		if res.Lessons >= p.maxLessons() {
			break
		}
		l = strings.TrimSpace(capText(l, maxLessonChars))
		if l == "" {
			continue
		}
		if err := p.Backend.SaveLesson(l, "memory pipeline"); err != nil {
			return err
		}
		res.Lessons++
	}
	return nil
}

// --- lease ---

// lease is the pipeline lease file's payload.
type lease struct {
	PID int    `json:"pid"`
	Run string `json:"run"`
	At  string `json:"at"` // RFC3339Nano UTC of the last heartbeat
}

func (p *Pipeline) leasePath() string { return filepath.Join(p.Backend.Dir, leaseFile) }

func (p *Pipeline) watermarkPath() string { return filepath.Join(p.Backend.Dir, watermarkFile) }

// acquireFileLease takes a cross-process lease file and returns its release
// func plus this run's id. A lease older than ttl (a crashed run) is stolen.
// Two xdev processes never write memory state concurrently: the pipeline
// takes this lease, so a second process backs off instead of racing it.
//
// ponytail: two lock stealers can both win the remove-then-create race, so a
// crash exactly at the stale boundary can double-run once. Phase writes are
// replace/append of heuristic context, so the damage is a duplicated pass,
// not corruption — the upgrade path is a per-run temp file plus rename, or
// flock under a build tag (same ceiling as internal/agent's mailbox lock).
func acquireFileLease(path string, ttl time.Duration) (func(), string, error) {
	runID := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", err
	}
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			if err := writeLeaseFile(path, runID); err != nil {
				os.Remove(path)
				return nil, "", err
			}
			return func() { releaseFileLease(path, runID) }, runID, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
		if leaseFreshAt(path, ttl) {
			return nil, "", ErrLeaseHeld
		}
		_ = os.Remove(path) // stale: a crashed or stuck run; retry the take
	}
	return nil, "", ErrLeaseHeld // lost the steal race to another process
}

// writeLeaseFile re-stamps a lease atomically (a live run is never mistaken
// for stale, and a reader never sees a torn payload).
func writeLeaseFile(path, runID string) error {
	raw, err := json.Marshal(lease{PID: os.Getpid(), Run: runID, At: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// leaseFreshAt reports whether the lease on disk is still inside its TTL.
func leaseFreshAt(path string, ttl time.Duration) bool {
	l, ok := readLease(path)
	if !ok {
		return false // unreadable/torn: treat as stale and reclaim
	}
	at, err := time.Parse(time.RFC3339Nano, l.At)
	if err != nil {
		return false
	}
	return time.Since(at) < ttl
}

// releaseFileLease drops the lease only when it is still ours: a stolen
// lease belongs to the run that took it.
func releaseFileLease(path, runID string) {
	if l, ok := readLease(path); ok && l.Run == runID {
		_ = os.Remove(path)
	}
}

func (p *Pipeline) acquireLease() (func(), string, error) {
	return acquireFileLease(p.leasePath(), p.leaseTTL())
}

// heartbeat re-stamps the lease so a live run is never mistaken for stale.
func (p *Pipeline) heartbeat(runID string) error {
	if l, ok := readLease(p.leasePath()); !ok || l.Run != runID {
		return ErrLeaseHeld
	}
	return p.writeLease(runID)
}

func (p *Pipeline) writeLease(runID string) error { return writeLeaseFile(p.leasePath(), runID) }

func readLease(path string) (lease, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return lease{}, false
	}
	var l lease
	if err := json.Unmarshal(raw, &l); err != nil || l.Run == "" {
		return lease{}, false
	}
	return l, true
}

// --- watermark ---

// sessionMark is what the last run already saw in one session file.
type sessionMark struct {
	ModTime   int64 `json:"modTime"` // unix nanoseconds
	UserTurns int   `json:"userTurns"`
}

// watermark is the per-session progress record. A session is extracted only
// when its file changed AND it gained user turns, so a session resumed for
// tool work alone costs nothing.
type watermark struct {
	Sessions map[string]sessionMark `json:"sessions"`
	Updated  string                 `json:"updated,omitempty"`
}

func (p *Pipeline) loadWatermark() *watermark {
	wm := &watermark{Sessions: map[string]sessionMark{}}
	raw, err := os.ReadFile(p.watermarkPath())
	if err != nil {
		return wm
	}
	if err := json.Unmarshal(raw, wm); err != nil {
		return &watermark{Sessions: map[string]sessionMark{}} // corrupt: restart the record
	}
	if wm.Sessions == nil {
		wm.Sessions = map[string]sessionMark{}
	}
	return wm
}

// saveWatermark persists the record atomically, dropping entries for session
// files the listing no longer knows about.
func (p *Pipeline) saveWatermark(wm *watermark, seen map[string]bool) error {
	for path := range wm.Sessions {
		if !seen[path] {
			delete(wm.Sessions, path)
		}
	}
	wm.Updated = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.Marshal(wm)
	if err != nil {
		return err
	}
	path := p.watermarkPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- prompts ---

const extractionInstructions = `You maintain the long-term memory of a coding agent. Read one session transcript and extract only what stays true after the session ends.

Return ONLY a JSON object, no prose:
{"notes": ["..."], "lessons": ["..."]}

notes   - durable facts about the project or the user: conventions, architecture, commands, environment quirks, preferences. One short sentence each.
lessons - non-obvious fixes and gotchas worth recalling later (what, when, why), plus explicit user instructions that should outlive the session.
Omit anything transient (the current task, open files, tool output), anything the repository already states, and every secret or credential.
Return {"notes":[],"lessons":[]} when the session taught nothing durable.`

const consolidationInstructions = `You consolidate the long-term memory of a coding agent.

Return ONLY a JSON object, no prose:
{"summary": "<markdown>", "lessons": ["..."]}

summary - the complete replacement for MEMORY.md: the existing knowledge with the new findings merged in, deduplicated, with stale or contradictory entries dropped, as short markdown bullets under short headings. Declarative facts, never instructions. Keep it under %d characters; it is injected into every future system prompt.
lessons - the new lessons worth appending to the lesson log: self-contained one-liners, not already present in the existing lessons.
Never write secrets or credentials.`

func extractionPrompt(transcript string) string {
	return extractionInstructions + "\n\nSession transcript:\n\n" + transcript
}

func consolidationPrompt(cur, lessons string, all Findings, maxSummary int) string {
	var b strings.Builder
	fmt.Fprintf(&b, consolidationInstructions, maxSummary)
	b.WriteString("\n\nCurrent MEMORY.md:\n\n")
	if cur == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(cur + "\n")
	}
	b.WriteString("\nRecent lessons:\n\n")
	if lessons == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(lessons + "\n")
	}
	b.WriteString("\nNew findings:\n\nnotes:\n")
	writeBullets(&b, all.Notes)
	b.WriteString("\nlessons:\n")
	writeBullets(&b, all.Lessons)
	return b.String()
}

func writeBullets(b *strings.Builder, items []string) {
	if len(items) == 0 {
		b.WriteString("- (none)\n")
		return
	}
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", collapseWS(it))
	}
}

// --- parsing ---

// parseFindings decodes one phase-1 reply, tolerating a fenced or
// prose-wrapped JSON object.
func parseFindings(s string) (Findings, error) {
	var f Findings
	body, ok := jsonObject(s)
	if !ok {
		return f, errors.New("extraction returned no JSON object")
	}
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		return f, fmt.Errorf("extraction JSON: %w", err)
	}
	f.Notes = cleanList(f.Notes)
	f.Lessons = cleanList(f.Lessons)
	return f, nil
}

// consolidation is one phase-2 reply.
type consolidation struct {
	Summary string   `json:"summary"`
	Lessons []string `json:"lessons"`
}

func parseConsolidation(s string) (consolidation, error) {
	var c consolidation
	body, ok := jsonObject(s)
	if !ok {
		return c, errors.New("consolidation returned no JSON object")
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return c, fmt.Errorf("consolidation JSON: %w", err)
	}
	c.Summary = strings.TrimSpace(c.Summary)
	c.Lessons = cleanList(c.Lessons)
	return c, nil
}

// jsonObject extracts the first {...} object from a reply, dropping a
// markdown code fence when the model wrapped its answer in one.
func jsonObject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "json"))
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return "", false
	}
	return s[start : end+1], true
}

// cleanList trims, drops empties, and deduplicates a model-supplied list.
func cleanList(items []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, it := range items {
		it = strings.TrimSpace(capText(it, maxLessonChars))
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	return out
}

// --- transcript ---

// userTurns counts the user messages the model actually saw.
func userTurns(msgs []ai.Message) int {
	n := 0
	for i := range msgs {
		if msgs[i].Role == ai.RoleUser {
			n++
		}
	}
	return n
}

// transcript renders the model-visible messages for extraction, keeping the
// tail (max chars): what changed since the last run is at the end. Tool
// results are dropped — they are noise for a durable-fact extraction.
func transcript(msgs []ai.Message, max int) string {
	var b strings.Builder
	for i := range msgs {
		var label string
		switch msgs[i].Role {
		case ai.RoleUser:
			label = "User"
		case ai.RoleAssistant:
			label = "Assistant"
		default:
			continue
		}
		txt := strings.TrimSpace(msgs[i].Text())
		if txt == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n\n", label, txt)
	}
	out := strings.TrimSpace(b.String())
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
		for len(out) > 0 && out[0]&0xC0 == 0x80 { // do not start mid-rune
			out = out[1:]
		}
		out = "…[earlier turns omitted]\n\n" + strings.TrimSpace(out)
	}
	return out
}
