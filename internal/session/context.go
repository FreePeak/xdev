package session

import (
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// SystemPrompt is the resolved system prompt text for one turn.
type SystemPrompt struct {
	Text string
}

// ContextResult is the provider request skeleton that buildContext derives
// from the entry path (PRD §3.2 context reconstruction).
type ContextResult struct {
	System   string
	Messages []ai.Message
	// EntryIDs is parallel to Messages: the store entry id each message
	// came from, or "" for synthesized messages (compaction summaries).
	// Compaction anchors firstKeptEntryId through it.
	EntryIDs []string
	Model    string
	// Thinking is the reasoning state the session's own transcript records
	// at the leaf of the path: the level, the level the user pinned, and
	// the effort rung whose tool surface the following turns ran on (omp
	// folds the same record back when it rebuilds the context —
	// packages__coding-agent__src__session__session-context.ts:137-140, and
	// hands it back as "thinkingLevel" at :412).
	Thinking ThinkingState
	// TokensBefore approximates the consumed context as the sum of usage
	// totals across the path (MVP approximation).
	TokensBefore int64
}

// ThinkingState is what a session's transcript records about the reasoning
// posture of the turns that follow a point in it.
//
// Level and Rung are two different facts: /thinking pins a LEVEL on the rung
// the session already had, and only the rung says which tools the model was
// offered. A resumed session needs both, and the record that carries them is
// the same one omp folds back.
type ThinkingState struct {
	// Level is the request-side level in force ("" when the session pinned
	// reasoning off).
	Level string
	// Configured is the level the user pinned, or ConfiguredAuto when the
	// level was inherited from the model role's ":effort" default ("" when
	// the record says nothing about it).
	Configured string
	// Rung is the session effort rung in force: the rung whose tool surface
	// the following turns ran on. Empty when the record predates the field.
	Rung string
	// Found reports whether the transcript records a thinking level at all.
	// "No record" and "a record that says off" are different answers, and a
	// caller must not confuse them: a session that pinned reasoning off
	// resumes off.
	Found bool
}

// buildContext reconstructs the model-visible conversation for the path from
// the given leaf to the root (parent chain), reversing to file order.
//
// Semantics (PRD §3.2):
//   - The walk is cycle-bounded: it stops after len(entries) steps.
//   - The LATEST reset_boundary in the path drops everything before it.
//   - If a compaction entry is in the path, its summary message is emitted
//     followed only by entries AFTER its firstKeptEntryId (nil → summary only).
//   - The latest model_change in the path wins for Model.
//   - Dangling tool calls are neutralized: assistant toolCall blocks whose
//     results are not in the path are dropped from the assistant message
//     (text/thinking kept); toolResult messages that answer no live call —
//     a stale id from an earlier turn, a duplicated answer — are dropped. A
//     call and its result are paired per turn: the result answers the
//     assistant turn it follows. An assistant turn left with no content at
//     all carries UnansweredToolCallNotice instead of an empty shell.
//   - branch_summary entries convert to a user message carrying their summary
//     text so the model knows what was abandoned.
//
// Pure function; no I/O.

// UnansweredToolCallNotice is the rebuild-time stand-in for a tool call that
// never came back (crash, cancel, or a stream that died mid-decode). It says
// so in the model's own terms, because the alternative — replaying the
// neutralized assistant shell — reaches the provider as an empty content list
// or, on openai-completions, `"content": ""`, which upstreams reject or
// silently drop. It is deliberately retry-guiding: the model is told to re-run

// ReplaySafety answers whether re-running an interrupted call to name is
// harmless. cmd/xdev wires it to the live tool registry (tool.Replayer); the
// nil default leaves every call unsafe, so a build that never wires it behaves
// exactly as it does today.
//
// The session package cannot import the tool package (tool imports session,
// for checkpoint/todo/imagegen), so this is the seam — the same
// overridable-func shape internal/tool/ast.go uses for ast-grep.
var ReplaySafety func(name string) bool

// replaySafe reports whether name is known to be free to repeat.
func replaySafe(name string) bool {
	return ReplaySafety != nil && ReplaySafety(name)
}

// unansweredToolCallText is the notice for one turn's dropped calls, naming
// the ones that are safe to repeat. A turn whose calls are all reads gets
// told plainly; a turn holding a single unsafe call keeps the plain notice,
// because telling the model which of them "may be re-run" is worse than
// leaving it to judge the whole set.
func unansweredToolCallText(names []string) string {
	safe := make([]string, 0, len(names))
	for _, n := range names {
		if replaySafe(n) {
			safe = append(safe, n)
		}
	}
	if len(safe) == 0 || len(safe) != len(names) {
		return UnansweredToolCallNotice
	}
	return UnansweredToolCallNotice + " (" + strings.Join(safe, ", ") +
		" only reads state, so re-running it is free)"
}

// the call rather than assume it succeeded.
const UnansweredToolCallNotice = "the previous turn ended before this tool call returned; its result is unknown, so run it again if you still need it"

func buildContext(entries []Entry, leafID string, sys SystemPrompt) (*ContextResult, error) {
	chain := entryChain(entries, leafID)

	// The reasoning state in force at the leaf: the LAST
	// thinking_level_change on the path (omp folds the same record back when
	// it rebuilds the context — session-context.ts:137-140). It is read
	// before the boundaries below cut the chain: the level is a state
	// record, not a message, and the session's own record stands whatever
	// the messages say.
	thinking := foldThinkingLevel(chain)

	// Emission boundary: drop everything before the LATEST reset_boundary.
	// (reset_boundary itself is not a message.)
	{
		cut := -1
		for i, e := range chain {
			if _, ok := e.(*ResetBoundaryEntry); ok {
				cut = i
			}
		}
		if cut >= 0 {
			chain = chain[cut+1:]
		}
	}

	// Compaction: the LAST compaction in the surviving chain governs.
	// Its summary is emitted first, then the kept window: entries from
	// firstKeptEntryId onward (firstKeptEntryId names the first entry that
	// survives). nil → only the summary. firstKeptEntryId pointing outside
	// the path (branched away from) → everything after the compaction entry.
	var summaries []ai.Message
	if idx := lastIndexOfType[*CompactionEntry](chain); idx >= 0 {
		c := chain[idx].(*CompactionEntry)
		summaries = append(summaries, c.Summary)
		switch {
		case c.FirstKeptEntryID == nil:
			chain = nil
		case indexOfID(chain, *c.FirstKeptEntryID) >= 0:
			chain = chain[indexOfID(chain, *c.FirstKeptEntryID):]
		default:
			chain = chain[idx+1:]
		}
	}

	// Derive model (latest model_change wins) and token approximation.
	var model string
	var tokens int64
	for _, e := range chain {
		if mc, ok := e.(*ModelChangeEntry); ok {
			model = mc.Model
		}
	}

	// Convert the chain to messages with tool-call neutralization.
	//
	// Pairing is PER TURN: a tool result answers the calls of the
	// assistant turn it follows, and each call is answered at most once.
	// Both sides of a pair have to agree, so the pairing is computed
	// before emission: a result that answers no live call is dropped, and
	// a call with no result in its own turn is dropped with it.
	//
	// A result whose id names a call of an EARLIER turn belongs to no
	// turn. That is a stale id — the agent's per-run dedup cache hands a
	// repeated call the result of the earlier, identical call, so the id
	// is the earlier call's — or a duplicated answer. Either way the
	// upstream pairs a result with its call by id, and a history carrying
	// a result that answers no live call is rejected outright
	// ("invalid params, tool call id is invalid", MiniMax 2013) — and
	// because the offending turn is already in the session, every retry
	// replays the same rejected request.
	answered := map[string]bool{}   // call id -> a result in its own turn answers it
	keepResult := map[string]bool{} // result entry id -> the result answers its turn
	{
		var open []string // call ids asked by the assistant turn in progress
		for _, e := range chain {
			t, ok := e.(*MessageEntry)
			if !ok {
				continue
			}
			switch t.Message.Role {
			case ai.RoleAssistant:
				open = nil
				for _, b := range t.Message.Content {
					if tc, ok := b.(ai.ToolCallBlock); ok {
						open = append(open, tc.ID)
					}
				}
			case ai.RoleToolResult:
				id := t.Message.ToolCallID
				idx := -1
				for i, o := range open {
					if o == id {
						idx = i
						break
					}
				}
				if idx < 0 {
					continue // answers no call of the turn it follows
				}
				open = append(open[:idx], open[idx+1:]...)
				answered[id] = true
				keepResult[t.Envelope().ID] = true
			}
		}
	}

	var out []ai.Message
	// entryIDs is parallel to out: source entry per message ("" for
	// synthesized messages such as compaction summaries).
	var entryIDs []string
	out = append(out, summaries...)
	for range summaries {
		entryIDs = append(entryIDs, "")
	}
	for _, e := range chain {
		switch t := e.(type) {
		case *MessageEntry:
			m := t.Message
			switch m.Role {
			case ai.RoleAssistant:
				// A turn whose only content was tool calls, none of which ever
				// produced a result, neutralizes to an assistant message with
				// zero blocks. Replaying that shell is what an interrupted session
				// used to hand the provider: openai-completions encoded it as
				// `{"role":"assistant","content":""}` (a request many upstreams
				// reject) and every other wire dropped it, so the model saw the
				// turn silently disappear. The call still happened and its result
				// is unknowable, so the honest record is the crash-repair text —
				// the same shape ai.EnsureToolOutput already guarantees for a
				// silent tool (#386 lineage), applied here at rebuild time
				// instead of once per run. dropped names the calls when the
				// turn had any, so the notice can say which are free to repeat.
				var dropped []string
				if hasToolCalls(m) {
					m.Content, dropped = neutralizeToolCalls(m, answered)
				}
				// Neutralizing a turn whose every block was a dangling call
				// leaves it with nothing, which is the case the notice covers.
				if len(dropped) > 0 && len(m.Content) == 0 {
					m.Content = []ai.Block{ai.TextBlock{Text: unansweredToolCallText(dropped)}}
				}
				out = append(out, m)
				entryIDs = append(entryIDs, e.Envelope().ID)
			case ai.RoleToolResult:
				if !keepResult[e.Envelope().ID] {
					continue // answers no live call: no assistant turn asked for it
				}
				// Heal on rebuild: a session stored while the Responses encoder
				// omitted an empty `output` would otherwise re-send the same
				// rejected request on every resume (ai.Message.EnsureToolOutput).
				out = append(out, m.EnsureToolOutput())
				entryIDs = append(entryIDs, e.Envelope().ID)
			default:
				out = append(out, m)
				entryIDs = append(entryIDs, e.Envelope().ID)
			}
			if u := m.Usage; u != nil {
				tokens += u.TotalTokens
			}
		case *BranchSummaryEntry:
			// Abandoned-branch context reaches the model as a user message.
			if txt := t.Summary.Text(); txt != "" {
				out = append(out, ai.Message{
					Role:    ai.RoleUser,
					Content: []ai.Block{ai.TextBlock{Text: txt}},
				})
				entryIDs = append(entryIDs, e.Envelope().ID)
			}
		}
	}

	return &ContextResult{
		System:       sys.Text,
		Messages:     out,
		EntryIDs:     entryIDs,
		Model:        model,
		Thinking:     thinking,
		TokensBefore: tokens,
	}, nil
}

// entryChain walks the leaf→root parent chain of entries and returns it in
// file order.
//
// The walk is cycle-bounded: it stops after len(entries) steps, and a
// missing parent ends the path (the root-side of a foreign chain link, e.g.
// an entry skipped as unparseable) rather than failing it.
func entryChain(entries []Entry, leafID string) []Entry {
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		env := e.Envelope()
		if env.ID != "" {
			byID[env.ID] = e
		}
	}
	var chain []Entry
	cur := byID[leafID]
	for steps := 0; cur != nil && steps <= len(entries); steps++ {
		chain = append(chain, cur)
		env := cur.Envelope()
		if env.ParentID == "" {
			break
		}
		next, ok := byID[env.ParentID]
		if !ok {
			break
		}
		if _, seen := seenID(chain, next.Envelope().ID); seen {
			break // cycle guard
		}
		cur = next
	}
	// Reverse to chronological order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// foldThinkingLevel returns the reasoning state the path records: the LAST
// thinking_level_change on it, with the level it names, the level the user
// pinned (ConfiguredAuto when the level was inherited from the model role's
// ":effort" default), and the effort rung whose tool surface the following
// turns ran on.
//
// Found is false when the path records none: a session that never changed
// its level says nothing, and the caller falls back to the machine-wide
// setting. A record that pins reasoning OFF is a record, not an absence —
// its level is "" and its answer is "off", not "whatever the setting says".
func foldThinkingLevel(chain []Entry) ThinkingState {
	var st ThinkingState
	for _, e := range chain {
		if t, ok := e.(*ThinkingLevelChangeEntry); ok {
			st = ThinkingState{
				Level:      t.ThinkingLevel,
				Configured: t.Configured,
				Rung:       t.Effort,
				Found:      true,
			}
		}
	}
	return st
}

// PersistedThinkingState returns the reasoning state the session's own
// transcript records at its leaf: the last thinking_level_change on the path
// to the leaf.
//
// It is the accessor next to BuildContext for the callers that need the
// state without rebuilding the whole conversation — a resume needs the level
// AND the rung, and it needs to tell "no record" from "a record that says
// off".
func PersistedThinkingState(entries []Entry, leafID string) ThinkingState {
	return foldThinkingLevel(entryChain(entries, leafID))
}

// BuildContext is the exported entry point for context reconstruction
// (PRD §3.2): walk leaf→root, apply boundaries, neutralize dangling tool
// calls, and derive the provider request skeleton.
func BuildContext(entries []Entry, leafID string, sys SystemPrompt) (*ContextResult, error) {
	return buildContext(entries, leafID, sys)
}

func seenID(chain []Entry, id string) (int, bool) {
	for i, e := range chain {
		if e.Envelope().ID == id {
			return i, true
		}
	}
	return -1, false
}

func indexOfID(chain []Entry, id string) int {
	i, ok := seenID(chain, id)
	if !ok {
		return -1
	}
	return i
}

// lastIndexOfType returns the index of the last entry of concrete type T in
// the chain, or -1.
func lastIndexOfType[T Entry](chain []Entry) int {
	for i := len(chain) - 1; i >= 0; i-- {
		if _, ok := chain[i].(T); ok {
			return i
		}
	}
	return -1
}

func hasToolCalls(m ai.Message) bool {
	for _, b := range m.Content {
		if _, ok := b.(ai.ToolCallBlock); ok {
			return true
		}
	}
	return false
}

// neutralizeToolCalls drops dangling toolCall blocks (no matching result in
// the path) from an assistant message, keeping text/thinking blocks, and
// returns the names of the calls it dropped so the caller can say which of
// them are free to repeat. When every block was a dangling call, the message
// is left with no blocks and the caller fills it with the notice.
func neutralizeToolCalls(m ai.Message, answered map[string]bool) (blocks []ai.Block, dropped []string) {
	kept := make([]ai.Block, 0, len(m.Content))
	for _, b := range m.Content {
		if tc, ok := b.(ai.ToolCallBlock); ok && !answered[tc.ID] {
			dropped = append(dropped, tc.Name)
			continue // dangling call
		}
		kept = append(kept, b)
	}
	return kept, dropped
}
