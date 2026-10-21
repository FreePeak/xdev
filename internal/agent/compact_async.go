package agent

import (
	"context"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
)

// Async compaction (M5 #24, compaction.async): instead of blocking a turn on
// the provider summarize, a due boundary starts that summarize in the
// background and a later boundary applies its result. It is bounded — one job
// at a time, output clamped by MaxSummaryTokens, input by the same transcript
// caps the synchronous path uses — and cancelable: the job derives from the
// ctx of the boundary that started it, so an aborted run cancels the provider
// call (CancelAsyncCompaction is the explicit spelling for wiring that
// cancels outside the ctx chain).
//
// The same job is the speculative arm: started BEFORE the threshold is due,
// inside the lead window (compact_spec.go), and applied at the boundary where
// the compaction becomes due — not at the next boundary regardless.
type asyncCompactState struct {
	cancel context.CancelFunc
	// result is buffered 1: the job sends once and never blocks, even if the
	// job is canceled and nobody is listening any more.
	result chan asyncCompactResult
	anchor string // the entry the summary anchors on (the span's cut)
	tokens int64  // the span's cost when the job started
	// snapshotLeaf is the leaf entry id when the job started: the arm's
	// snapshot of the session. A result whose snapshot left the active path
	// describes history the model can no longer see.
	snapshotLeaf string
	// contextTokensAtStart is the context size when the job started: the
	// arm's reference point for "how much has the context grown since".
	contextTokensAtStart int64
	// pending is the finished result, waiting for the boundary where the
	// compaction is due. nil while the summarize is still running.
	pending *asyncCompactResult
	// owed marks a DEFERRAL: a compaction was DUE and was handed to this
	// background job, so its result must land at the next boundary whether
	// or not the trigger that handed it over still fires. The idle trigger
	// is consumed by the attempt that takes it, so without the debt the
	// deferred compaction would never be applied — and the provider call
	// it spent would buy nothing.
	owed bool
}

// asyncCompactResult is one job's outcome.
type asyncCompactResult struct {
	summary ai.Message
	err     error
}

// kickAsyncCompaction starts a background summarize for this boundary and
// reports whether it took over. false means there is nothing to hand off (no
// droppable span); the caller then stays synchronous, which reports the same
// reason loudly instead of quietly skipping the compaction. A job already in
// flight is the maybeCompact guard's business, not this one.
//
// owed says what the job is: a DEFERRAL (a compaction was due and is now
// owed to this job — its result lands at the next boundary) or a speculative
// ARM (the compaction is not due yet; the result lands when it becomes due).
func (a *Agent) kickAsyncCompaction(ctx context.Context, owed bool) bool {
	if a.compactAsync != nil {
		return false
	}
	span, err := a.compactionSpan()
	if err != nil {
		return false
	}
	jobCtx, cancel := context.WithCancel(ctx)
	job := &asyncCompactState{
		cancel:               cancel,
		result:               make(chan asyncCompactResult, 1),
		anchor:               span.anchor(),
		tokens:               span.tokens,
		snapshotLeaf:         a.Store.LeafID(),
		contextTokensAtStart: span.tokens,
		owed:                 owed,
	}
	// Snapshot provider and model: a failover can move the live fields while
	// the job runs, and the goroutine must not read them.
	provider, model := a.Provider, a.Model
	msgs := span.msgs[:span.cp.historyEnd()]
	// Resolved here, not in the goroutine: the job must not read live agent
	// fields (same reason provider/model are copied above).
	memCtx := ""
	if a.MemoryContext != nil {
		memCtx = strings.TrimSpace(a.MemoryContext())
	}
	go func() {
		defer cancel()
		var res asyncCompactResult
		summary, usage, err := summarizeWith(jobCtx, provider, model, msgs, memCtx, span.split(), a.previousSummary(span))
		if err != nil {
			res.err = err
		} else {
			res.summary = textSummary(summary)
			// The background call was billed whether or not its result is
			// still applicable, so its usage rides the summary and is
			// banked if the entry is applied. A dropped result is charged
			// nowhere: it was not part of the conversation.
			res.summary.Usage = usage
			res.summary.Model = model
		}
		job.result <- res
	}()
	a.compactAsync = job
	logx.Infof("compaction: background summarize started (anchor %s, %d tokens before)", job.anchor, job.tokens)
	return true
}

// harvestAsyncResult takes a finished background summarize off the job's
// channel and keeps it pending: an armed result waits for the boundary
// where the compaction is DUE. It reports whether a result is pending now.
func (a *Agent) harvestAsyncResult() bool {
	job := a.compactAsync
	if job == nil || job.pending != nil {
		return false
	}
	select {
	case res := <-job.result:
		job.pending = &res
		return true
	default:
		return false // still running
	}
}

// applyAsyncCompaction applies the armed background summarize and reports
// whether it rebuilt the context. history comes back untouched while the
// job is still running, after it failed, or when its result is stale — the
// arm's rules are compact_spec.go's armedWindow.
//
// The caller decides WHEN the result is consumed: the boundary where the
// compaction is due.
func (a *Agent) applyAsyncCompaction(history []ai.Message) ([]ai.Message, bool) {
	job := a.compactAsync
	if job == nil || job.pending == nil {
		return history, false
	}
	res := job.pending
	kept, ok := a.armedWindow(job, res)
	if !ok {
		return history, false
	}
	entry := armedEntry(job, res, kept)
	if err := a.persistCompaction(entry, kept); err != nil {
		logx.Errorf("compaction: %v", err)
		return history, false
	}
	logx.Infof("compaction: armed summarize applied (%d tokens before)", job.tokens)
	if rebuilt, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{}); err == nil {
		return rebuilt.Messages, true
	}
	return history, true
}

// CancelAsyncCompaction aborts the in-flight background summarize, if any:
// nothing it produces is applied afterwards. Safe with no job in flight, so an
// abort path can call it unconditionally.
func (a *Agent) CancelAsyncCompaction() {
	a.forgetAsyncCompaction()
}

// forgetAsyncCompaction drops the background job: the summarize is canceled
// and the state cleared, so the next boundary may arm a fresh one. A job
// whose result was already consumed is simply forgotten.
func (a *Agent) forgetAsyncCompaction() {
	if job := a.compactAsync; job != nil {
		a.compactAsync = nil
		job.cancel()
	}
}
