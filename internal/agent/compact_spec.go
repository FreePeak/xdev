package agent

import (
	"context"
	"slices"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
)

// Speculative compaction arming.
//
// omp arms a compaction BEFORE the threshold is due — inside a lead window
// around the threshold — so the summary is already there when the
// compaction is needed, and the armed result is consumed at the boundary
// where the compaction becomes DUE (omp
// packages/coding-agent/src/session/session-maintenance.ts:1154-1190, the
// arm; :1386-1408, the take; :1336-1385, the validity rules).
//
// xdev already has the background summarizer (compact_async.go); this file
// is the POLICY around it: when to arm, what the arm snapshots, and when an
// armed result may no longer be applied.
//
// The arm rides the existing compaction.async setting: with it off — the
// shipped default — nothing is ever armed and every compaction stays
// synchronous.
const (
	// leadWindowMin is the smallest lead window (omp's Fle = 8192, omp
	// packages/coding-agent/src/session/speculation-lead.ts:2).
	leadWindowMin int64 = 8192
	// leadWindowMax is the largest lead window (omp's cap = 32000, the
	// same file :3).
	leadWindowMax int64 = 32000
	// armedPendingTokens is the size of the messages that are queued but
	// not yet in the context — omp's pendingContextTokens (omp
	// packages/coding-agent/src/session/session-maintenance.ts:1509).
	// xdev's compaction boundary is pre-request (loop.go:737): everything
	// the next request will carry is already in the history, so nothing is
	// pending at the boundary.
	armedPendingTokens int64 = 0
)

// leadWindow is how far below the threshold an arm may start: omp's
// oF(n) = min(32000, max(8192, floor(n * 0.125))) — the same formula and
// the same constants (omp
// packages/coding-agent/src/session/speculation-lead.ts:5-7).
func leadWindow(threshold int64) int64 {
	return min(leadWindowMax, max(leadWindowMin, threshold*125/1000))
}

// armEligible reports whether this boundary may arm a speculative
// compaction: the context is below the threshold and within the lead
// window of it. It is omp's arm condition — the context is under the
// threshold and the gap to it is no wider than the lead window (omp
// packages/coding-agent/src/session/session-maintenance.ts:1170-1172).
func (a *Agent) armEligible(used int64) bool {
	threshold := a.compaction().threshold()
	if threshold <= 0 || used >= threshold {
		return false
	}
	return threshold-used <= leadWindow(threshold)
}

// maybeArm arms a speculative compaction when the context is inside the
// lead window below the threshold: the summary is produced in the
// background, so the boundary that crosses the threshold costs no provider
// round-trip.
//
// The arm is dropped and armed afresh when it went stale: the summarize
// failed, or the context grew past the kept window since the arm — the
// summary no longer describes the history it summarizes (omp's re-arm
// policy — omp
// packages/coding-agent/src/session/session-maintenance.ts:1175-1186).
//
// The arm's VALIDITY is not judged here: an arm that no longer applies is
// dropped at the boundary that would have applied it, and that boundary
// compacts synchronously (compact_spec.go's armedWindow).
func (a *Agent) maybeArm(ctx context.Context, history []ai.Message) {
	if !a.compaction().Async || !a.firstProductIsHandoff() {
		return
	}
	used := contextTokens(history)
	if job := a.compactAsync; job != nil {
		if job.pending == nil {
			// A summarize is in flight: one at a time.
			return
		}
		growth := max(int64(0), used-job.contextTokensAtStart)
		if job.pending.err == nil && growth <= max(a.compaction().keepRecent(), leadWindowMin) {
			return // the arm is still fresh: keep it
		}
		// Stale: drop it, and arm a fresh one below.
		a.forgetAsyncCompaction()
	}
	if !a.armEligible(used) {
		return
	}
	a.kickAsyncCompaction(ctx)
}

// armedWindow resolves an armed result against the current context: the
// window the compaction keeps, and whether the arm may still be applied at
// all.
//
// The rules are omp's validity rules for an armed compaction (omp
// packages/coding-agent/src/session/session-maintenance.ts:1336-1385):
//
//  1. the snapshot must still be on the active path — a session that
//     branched away leaves the summary describing history the model can no
//     longer see;
//  2. no compaction or reset_boundary may have been written after the
//     snapshot — the context was rebuilt and the summary describes the old
//     one;
//  3. the anchor must still resolve — the kept window starts there;
//  4. the compacted context must fit the head-room the threshold leaves:
//     post + pending > floor(threshold * 0.8) drops the arm;
//  5. the compaction must shrink the context: post + pending >= stored +
//     pending drops it.
//
// A dropped arm costs latency, never correctness: the boundary falls back
// to the synchronous compaction.
func (a *Agent) armedWindow(job *asyncCompactState, res *asyncCompactResult) ([]ai.Message, bool) {
	if res == nil || res.err != nil || res.summary.Role == "" {
		return nil, false
	}
	entries := a.Store.Entries()
	path := activePath(entries, a.Store.LeafID())
	idx := -1
	for i, e := range path {
		if e.Envelope().ID == job.snapshotLeaf {
			idx = i
			break
		}
	}
	if idx < 0 {
		logx.Infof("compaction: armed result dropped — the session branched away from the snapshot")
		return nil, false
	}
	for _, e := range path[idx+1:] {
		switch e.(type) {
		case *session.CompactionEntry, *session.ResetBoundaryEntry:
			logx.Infof("compaction: armed result dropped — the context was rebuilt after the arm")
			return nil, false
		}
	}
	fres, err := session.BuildContext(entries, a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		logx.Errorf("compaction: armed: %v", err)
		return nil, false
	}
	i := slices.Index(fres.EntryIDs, job.anchor)
	if i < 0 {
		logx.Infof("compaction: armed result dropped — its anchor left the active path")
		return nil, false
	}
	kept := fres.Messages[i:]
	after := armedEntry(job, res, kept).TokensAfter
	// The compacted context (plus whatever is pending) must fit the
	// head-room the threshold leaves — omp's 0.8 rule, strictly greater.
	if after+armedPendingTokens > a.compaction().threshold()*80/100 {
		logx.Infof("compaction: armed result dropped — the compacted context would not fit")
		return nil, false
	}
	// A compaction that does not shrink the context is pointless — omp's
	// no-shrink rule, inclusive.
	before := contextTokens(fres.Messages)
	if before > 0 && after+armedPendingTokens >= before+armedPendingTokens {
		logx.Infof("compaction: armed result dropped — it would not shrink the context")
		return nil, false
	}
	return kept, true
}

// armedEntry is the compaction entry an armed result persists: the summary
// replaces the discarded prefix, the kept window survives verbatim. The
// record is filled by FillCompactionRecord — the one helper every
// compaction path uses — so the armed path says the same thing about
// itself as the ladder's.
func armedEntry(job *asyncCompactState, res *asyncCompactResult, kept []ai.Message) *session.CompactionEntry {
	anchor := job.anchor
	entry := &session.CompactionEntry{
		Summary:          res.summary,
		FirstKeptEntryID: &anchor,
		TokensBefore:     job.tokens,
		Method:           MethodHandoff,
	}
	FillCompactionRecord(entry, kept)
	return entry
}

// activePath is the entry path the session's context is built from: the
// leaf's ancestors, oldest first. It is the same walk session.BuildContext
// makes (internal/session/context.go), spelled out here because the arm's
// rules ask about the path's ENTRIES (a compaction or a reset_boundary on
// it), not about the messages the rebuild emits.
func activePath(entries []session.Entry, leafID string) []session.Entry {
	byID := make(map[string]session.Entry, len(entries))
	for _, e := range entries {
		if id := e.Envelope().ID; id != "" {
			byID[id] = e
		}
	}
	var rev []session.Entry
	cur := byID[leafID]
	for steps := 0; cur != nil && steps <= len(entries); steps++ {
		rev = append(rev, cur)
		env := cur.Envelope()
		if env.ParentID == "" {
			break
		}
		next, ok := byID[env.ParentID]
		if !ok {
			break
		}
		cur = next
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}
