package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/memlimit"
	"github.com/FreePeak/xdev/internal/session"
)

// Handoff is the *document* form of compaction (M5 #23, omp
// `compaction.methodOrder` member `handoff`). Unlike `summarize`, which
// renders a stripped transcript into one provider call, the handoff runs a
// side request that mirrors a live turn's transform — the live system
// prompt plus the live (redacted) history, with a trailing user message
// asking for the document and no tool surface (toolChoice none) — and
// commits the answer as an ordinary CompactionEntry. Every reader
// (context rebuild, /tree, /branch, resume) therefore keeps working
// without knowing what a handoff is.
//
// Cache alignment is the whole point of mirroring the live transform: the
// side request must re-send the *same* prefix a live turn would, so a
// provider with prompt caching reads a warm prefix instead of paying for
// the history twice (omp reuses sessionId/promptCacheKey for the same
// reason). That is why system and history go through liveRequest/liveSystem
// here rather than being re-assembled specially.

// HandoffAttribution tags the summary message of a handoff compaction. It is
// the machine-readable wrap that tells a handoff document apart from an
// automatic summary (both are CompactionEntry): the tag rides ai.Message's
// persisted attribution slot, so it survives a resume. (omp records the same
// fact as the entry's `method: "handoff"` field; when CompactionEntry grows
// that field, the handoff path should set it too — one line — and this tag
// stays the wire-compatible alias.)
const HandoffAttribution = "handoff"

// todoToolName is the todo tool's registry name (registered by cmd; the
// handoff clears its list through the tool's own surface).
const todoToolName = "todo"

// HandoffMaxAttempts is the side request's own retry budget (omp
// oneshot-retry: oneshots are side-effect-free, so they retry transient
// failures themselves — the turn ladder must not own them, since it may not
// replay unsafe output).
const HandoffMaxAttempts = 3

// handoffOpenTag/handoffCloseTag wrap the committed document text: a reader
// (the model on the next turn, a human in the transcript) sees a handoff
// block rather than an anonymous assistant message.
const (
	handoffOpenTag  = "<handoff>\n"
	handoffCloseTag = "\n</handoff>"
)

// HandoffSettings is the agent-side handoff configuration (M5 #23). The
// user-facing knobs are settings `handoff.saveToDisk` and the @smol role;
// cmd resolves them into these fields.
type HandoffSettings struct {
	// Target writes the document (the @smol role). Zero value → the
	// agent's active provider/model, so a session with no role wired still
	// hands off.
	Target FailoverTarget
	// SaveDir mirrors each document to <SaveDir>/<shortid>.md ("" = the
	// committed entry is the only copy).
	SaveDir string
	// Reset rewinds caller-owned per-branch state with the kept history
	// (the advisor feed cursor: Advisor.Reset). Plan mode and the todo list
	// are reachable from here and always reset.
	Reset func(kept []ai.Message)
}

// HandoffOrder resolves the compaction.methodOrder setting for
// CompactionConfig.Methods with the handoff method recognized.
//
// Validation belongs to ParseMethodOrder (the ladder vocabulary); this only
// covers the case where that vocabulary does not list the method yet. The
// name is stripped before parsing — the shipped parser would otherwise warn
// about an unknown method and fall back to the whole default order — and
// appended afterwards (its position carries no ordering semantics for the
// rung: it selects the boundary document). The shipped default order is never
// extended: an empty or all-invalid setting still means
// [threshold overflow promotion], so nothing hands off unless it is asked
// for.
func HandoffOrder(raw string) []string {
	methods := ParseMethodOrder(raw)
	if slices.Contains(methods, MethodHandoff) {
		// The ladder vocabulary already knows the method (compact.go's
		// CompactionMethodNames lists it): the shipped parse stands, so the
		// two parsers cannot drift.
		return methods
	}
	kept := make([]string, 0, len(methods)+1)
	selected := false
	for _, part := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(part), MethodHandoff) {
			selected = true
			continue
		}
		kept = append(kept, part)
	}
	if !selected {
		return methods // no handoff token: ParseMethodOrder owns the whole answer
	}
	rest := strings.Join(kept, ",")
	if strings.TrimSpace(rest) == "" {
		// The setting listed handoff alone: the ladder is exactly that
		// method, not the shipped default plus a rung.
		return []string{MethodHandoff}
	}
	return append(ParseMethodOrder(rest), MethodHandoff)
}

// HandoffDoc generates the handoff document for the current session context,
// commits it as a CompactionEntry at the cut point, and rewinds the
// per-branch state the dropped history carried. Returns the document text.
//
// system is the caller's base system prompt (the same string it passes to
// Run); the plan-mode and goal reminders a live turn appends are applied
// here, so the side request shares the live prefix. instruction is the
// user's /handoff argument ("" = the generic document).
func (a *Agent) HandoffDoc(ctx context.Context, system, instruction string) (string, error) {
	return a.handoff(ctx, system, instruction, true)
}

// handoff is the shared implementation. planReset is false for the automatic
// rung: a run in flight keeps its plan-mode sub-state (flipping it off
// mid-run would silently lift the read-only policy while the run's system
// prompt still promises it).
func (a *Agent) handoff(ctx context.Context, system, instruction string, planReset bool) (string, error) {
	if a.Store == nil {
		return "", fmt.Errorf("handoff: no session store")
	}
	if a.Provider == nil && a.Handoff.Target.Provider == nil {
		return "", fmt.Errorf("handoff: no provider")
	}
	res, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return "", fmt.Errorf("handoff: build context: %w", err)
	}
	if len(res.Messages) == 0 {
		return "", fmt.Errorf("handoff: nothing to hand off")
	}

	doc, err := a.handoffDoc(ctx, system, res.Messages, instruction)
	if err != nil {
		return "", fmt.Errorf("handoff: %w", err)
	}

	firstKept, kept := a.handoffCut(res)
	tokensBefore := contextTokens(res.Messages)
	entry := &session.CompactionEntry{
		// The document is a briefing the fresh context continues from, so it
		// is emitted as a USER message. Two reasons: that is omp's shape
		// (compaction summaries are a non-assistant role on the wire), and a
		// leading assistant turn trips thinking-mode upstreams — an
		// openai-completions adapter drops thinking blocks by design, so an
		// assistant summary with no reasoning echo is rejected outright
		// ("the reasoning_content in the thinking mode must be passed back
		// to the API", onegw Console Go, reproduced live).
		Summary: ai.Message{
			Role:        ai.RoleUser,
			Attribution: HandoffAttribution,
			Content:     []ai.Block{ai.TextBlock{Text: handoffOpenTag + doc + handoffCloseTag}},
		},
		FirstKeptEntryID: firstKept,
		TokensBefore:     tokensBefore,
	}
	if err := a.Store.Append(entry); err != nil {
		return "", fmt.Errorf("handoff: persist: %w", err)
	}
	// Same emission point as compact(): a handoff IS a compaction, so the
	// bus sees session_compact and the UI can explain the cache miss.
	if a.Hooks != nil {
		a.Hooks.OnCompaction(tokensBefore)
	}
	a.resetHandoffState(ctx, kept, planReset)
	if a.Handoff.SaveDir != "" {
		if path, werr := writeHandoffDoc(a.Handoff.SaveDir, a.Store.ID(), doc); werr != nil {
			// The committed entry is the durable copy; the mirror is an
			// artifact. Report, never fail the handoff over it.
			logx.Errorf("handoff: save artifact %s: %v", path, werr)
		} else {
			logx.Debugf("handoff: saved %s", path)
		}
	}
	return doc, nil
}

// HandoffDue reports whether this step boundary should commit a handoff
// document instead of a summary: the method order names `handoff` and the
// token budget is due. Live memory pressure forces it (the same hard
// backstop maybeCompact applies), and the context window guard matches
// maybeCompact — ContextWindow 0 disables context maintenance entirely.
func (a *Agent) HandoffDue(history []ai.Message) bool {
	if a.Store == nil || a.Compaction.ContextWindow <= 0 {
		return false
	}
	if !slices.Contains(a.Compaction.methods(), MethodHandoff) {
		return false
	}
	if p := memPressure(); p >= memlimit.HighPressure {
		return true
	}
	if a.Compaction.threshold() <= 0 {
		return false
	}
	return contextTokens(history) > a.Compaction.threshold()
}

// HandoffRung is the ladder's handoff method at a step boundary: when due,
// the document is committed and the rebuilt history returned (ok = true); a
// failed handoff returns ok = false so the caller's summary rung still runs
// — a handoff must never cost the boundary its compaction.
func (a *Agent) HandoffRung(ctx context.Context, system string, history []ai.Message) ([]ai.Message, bool) {
	if !a.HandoffDue(history) {
		return history, false
	}
	if _, err := a.handoff(ctx, system, "", false); err != nil {
		logx.Errorf("handoff rung: %v — falling back to the summary method", err)
		return history, false
	}
	res, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return history, false
	}
	return res.Messages, true
}

// handoffCut resolves the cut point: the kept window is the same keepRecent
// tail an automatic compaction would keep and findCutPoint never splits a
// tool turn. A session with nothing droppable hands the whole history to the
// document (nil firstKeptEntryId = only the document survives), because an
// explicit /handoff must never fail for being short.
func (a *Agent) handoffCut(res *session.ContextResult) (*string, []ai.Message) {
	cut := findCutPoint(res.Messages, a.Compaction.keepRecent())
	if cut < 1 || cut >= len(res.EntryIDs) || res.EntryIDs[cut] == "" {
		return nil, nil
	}
	id := res.EntryIDs[cut]
	return &id, res.Messages[cut:]
}

// handoffDoc runs the side request: the live transform over the full history
// plus the trailing instruction, tools omitted (toolChoice none), on the
// handoff target, with its own transient-error retry.
func (a *Agent) handoffDoc(ctx context.Context, system string, msgs []ai.Message, instruction string) (string, error) {
	prov, model := a.Provider, a.Model
	if a.Handoff.Target.Provider != nil {
		prov, model = a.Handoff.Target.Provider, a.Handoff.Target.Model
	}
	trailing := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: handoffPrompt(instruction)}}}
	// Copy: the live history slice must not gain a trailing message that a
	// caller might later persist.
	withTrailing := make([]ai.Message, 0, len(msgs)+1)
	withTrailing = append(withTrailing, msgs...)
	withTrailing = append(withTrailing, trailing)

	req := a.liveRequest(a.liveSystem(system), withTrailing, nil)
	req.Model = model
	req.MaxTokens = MaxSummaryTokens
	// A summarize call is served and dropped: its markers stop at the last
	// completed tool round so the prefix it writes is one the main chain reads.
	req.Cache.SideRequest = true

	policy := a.Retry
	if policy.MaxRetries == 0 && policy.BaseDelay == 0 {
		policy = DefaultRetryPolicy()
	}
	var lastErr error
	for attempt := range HandoffMaxAttempts {
		if attempt > 0 {
			if err := sleepBackoff(ctx, policy.delay(attempt)); err != nil {
				return "", err
			}
		}
		doc, err := streamOneshot(ctx, prov, req)
		if err == nil && strings.TrimSpace(doc) != "" {
			return strings.TrimSpace(doc), nil
		}
		if err == nil {
			err = fmt.Errorf("provider returned an empty document")
		}
		lastErr = err
		// Only transient failures are worth a retry: retrying auth or a
		// malformed request cannot succeed (omp oneshot-retry).
		if ai.Classify(err) != ai.ClassTransient {
			break
		}
	}
	return "", lastErr
}

// streamOneshot runs one completion and returns its text.
func streamOneshot(ctx context.Context, prov ai.Provider, req ai.StreamRequest) (string, error) {
	ch, err := prov.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case ai.EventTextDelta:
			text.WriteString(ev.Delta)
		case ai.EventDone:
			if ev.Message != nil && ev.Message.Text() != "" {
				return ev.Message.Text(), nil
			}
			return text.String(), nil
		case ai.EventError:
			return "", ev.Err
		}
	}
	return "", fmt.Errorf("handoff: stream ended without done")
}

// handoffPrompt is the trailing user instruction: the document contract plus
// the user's optional /handoff argument.
func handoffPrompt(instruction string) string {
	var b strings.Builder
	b.WriteString("Write the handoff document for this session. It REPLACES the conversation above in a fresh context, so it must stand alone.\n\n")
	b.WriteString("Use these sections:\n")
	b.WriteString("## Goal — what the user is trying to achieve, constraints included\n")
	b.WriteString("## State — what is done, in flight, and broken right now\n")
	b.WriteString("## Decisions — choices made and why (so they are not re-litigated)\n")
	b.WriteString("## Files — every path touched, with its current state and the change in it\n")
	b.WriteString("## Commands — commands run and their outcomes\n")
	b.WriteString("## Next steps — the exact next actions, in order\n")
	b.WriteString("## Gotchas — errors hit, fixes applied, and traps to avoid\n\n")
	b.WriteString("Be dense and factual. Name paths, commands, and identifiers exactly. Do not include tool calls or ask questions — the document is the entire output.\n")
	if s := strings.TrimSpace(instruction); s != "" {
		b.WriteString("\nThe user asks the handoff to focus on: " + s + "\n")
	}
	return b.String()
}

// resetHandoffState rewinds the per-branch state the handed-off history
// carried. The todo list is cleared through the tool's own surface, so the
// sink persists the cleared snapshot and a resume replays an empty list
// rather than the handed-off tasks.
func (a *Agent) resetHandoffState(ctx context.Context, kept []ai.Message, planReset bool) {
	if planReset {
		a.PlanMode.Reset()
	}
	if a.Tools != nil {
		if t, ok := a.Tools.Get(todoToolName); ok {
			if _, err := t.Execute(ctx, json.RawMessage(`{"op":"rm"}`)); err != nil {
				logx.Errorf("handoff: reset todo list: %v", err)
			}
		}
	}
	if a.Handoff.Reset != nil {
		a.Handoff.Reset(kept)
	}
}

// writeHandoffDoc mirrors the document to <dir>/<shortid>.md (0600, like the
// session files: a handoff carries the same secrets the transcript does).
func writeHandoffDoc(dir, sessionID, doc string) (string, error) {
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" {
		short = "session"
	}
	path := filepath.Join(dir, short+".md")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, err
	}
	return path, os.WriteFile(path, []byte(doc+"\n"), 0o600)
}

// liveSystem resolves the system prompt a live turn streams: the plan-mode
// reminder (while the sub-state is live) plus the active goal reminder. Run
// applies the same two steps to its caller's prompt, so a side request built
// through liveRequest shares the live prefix byte-for-byte.
func (a *Agent) liveSystem(system string) string {
	if a.PlanMode != nil && a.PlanMode.Active {
		system += "\n\n" + planModeSystemReminder(a.PlanMode.Note)
	}
	return a.goalSystem(system)
}

// liveRequest builds the provider request for system + history exactly the
// way a live turn does (same fields, same redaction), minus any tool surface
// when tools is nil. oneTurn is the other caller: if the two ever diverge,
// every cache-aligned side request silently stops sharing the warm prefix.
func (a *Agent) liveRequest(system string, history []ai.Message, tools []ai.ToolDef) ai.StreamRequest {
	req := ai.StreamRequest{
		System:    system,
		Messages:  history,
		Tools:     tools,
		MaxTokens: a.MaxTokens,
		Model:     a.Model,
		Thinking:  a.Thinking,
		Cache:     a.cacheOpts(),
	}
	if a.Redactor != nil {
		// Redact a copy: the store keeps the raw values, only the
		// provider request carries placeholders (M13 #55).
		req.System = a.Redactor.Apply(req.System)
		req.Messages = redactMessages(history, a.Redactor)
	}
	return req
}

// Reset leaves plan mode. Declared here (not planmode.go) because the handoff
// is its consumer: a handed-off context must not keep a pending proposal the
// document already captured — the host re-enters with /plan.
func (pm *PlanMode) Reset() {
	if pm == nil {
		return
	}
	pm.Active = false
	pm.Pending = ""
}

// cacheOpts is the prompt-cache identity of this session's requests: the stable
// session id, which is what both cache vocabularies key on — Anthropic's
// markers ride the request body, OpenAI's prompt_cache_key routes affinity — and
// is shared by the main chain and its side requests on purpose so a side request
// reads the prefix the main turn wrote (omp forwards promptCacheKey ??
// sessionId). Empty when no store is bound: a provider then sends exactly the
// request shape it sent before caching existed.
func (a *Agent) cacheOpts() ai.CacheOpts {
	if a.Store == nil {
		return ai.CacheOpts{}
	}
	return ai.CacheOpts{Key: a.Store.ID()}
}
