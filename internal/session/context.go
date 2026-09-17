package session

import (
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
	// TokensBefore approximates the consumed context as the sum of usage
	// totals across the path (MVP approximation).
	TokensBefore int64
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
//     (text/thinking kept); toolResult messages referencing unseen call ids
//     are dropped.
//   - branch_summary entries convert to a user message carrying their summary
//     text so the model knows what was abandoned.
//
// Pure function; no I/O.
func buildContext(entries []Entry, leafID string, sys SystemPrompt) (*ContextResult, error) {
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		env := e.Envelope()
		if env.ID != "" {
			byID[env.ID] = e
		}
	}

	// Walk leaf → root, cycle-bounded.
	var chain []Entry
	cur := byID[leafID]
	for steps := 0; cur != nil && steps <= len(entries); steps++ {
		chain = append(chain, cur)
		env := cur.Envelope()
		if env.ParentID == "" {
			break
		}
		next, ok := byID[env.ParentID]
		// Missing parent: the path ends here (root-side of a foreign chain
		// link, e.g. an entry skipped as unparseable). Not an error.
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
	// First pass: collect answered toolCall ids per assistant message.
	answered := map[string]bool{}
	for _, e := range chain {
		me, ok := e.(*MessageEntry)
		if !ok || me.Message.Role != ai.RoleToolResult {
			continue
		}
		if me.Message.ToolCallID != "" {
			answered[me.Message.ToolCallID] = true
		}
	}

	// Track toolCall ids actually present in prior assistant messages so
	// orphan toolResults can be dropped in one sweep (a toolResult must
	// follow the assistant message that issued the call).
	seenCalls := map[string]bool{}
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
				if hasToolCalls(m) {
					m.Content = neutralizeToolCalls(m, answered)
				}
				for _, b := range m.ToolCalls() {
					if answered[b.ID] {
						seenCalls[b.ID] = true
					}
				}
				out = append(out, m)
				entryIDs = append(entryIDs, e.Envelope().ID)
			case ai.RoleToolResult:
				if m.ToolCallID != "" && !seenCalls[m.ToolCallID] {
					continue // orphan result: no prior assistant issued the call
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
		TokensBefore: tokens,
	}, nil
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
// the path) from an assistant message, keeping text/thinking blocks. When
// every block was a dangling call, the message is dropped entirely.
func neutralizeToolCalls(m ai.Message, answered map[string]bool) []ai.Block {
	kept := make([]ai.Block, 0, len(m.Content))
	for _, b := range m.Content {
		if tc, ok := b.(ai.ToolCallBlock); ok && !answered[tc.ID] {
			continue // dangling call
		}
		kept = append(kept, b)
	}
	return kept
}
