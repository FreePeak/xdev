package agent

import (
	"context"
	"slices"

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
type asyncCompactState struct {
	cancel context.CancelFunc
	// result is buffered 1: the job sends once and never blocks, even if the
	// job is canceled and nobody is listening any more.
	result chan asyncCompactResult
	anchor string // the entry the summary anchors on (the span's cut)
	tokens int64  // the span's cost when the job started
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
func (a *Agent) kickAsyncCompaction(ctx context.Context) bool {
	if a.compactAsync != nil {
		return false
	}
	span, err := a.compactionSpan()
	if err != nil {
		return false
	}
	jobCtx, cancel := context.WithCancel(ctx)
	job := &asyncCompactState{
		cancel: cancel,
		result: make(chan asyncCompactResult, 1),
		anchor: span.anchor(),
		tokens: span.tokens,
	}
	// Snapshot provider and model: a failover can move the live fields while
	// the job runs, and the goroutine must not read them.
	provider, model := a.Provider, a.Model
	msgs := span.msgs[:span.cut]
	go func() {
		defer cancel()
		var res asyncCompactResult
		if summary, err := summarizeWith(jobCtx, provider, model, msgs); err != nil {
			res.err = err
		} else {
			res.summary = textSummary(summary)
		}
		job.result <- res
	}()
	a.compactAsync = job
	logx.Infof("compaction: async summarize started in the background")
	return true
}

// applyAsyncCompaction applies a finished background summarize and reports
// whether it rebuilt the context. history comes back untouched while the job
// is still running, after it failed, or when its result is stale — the anchor
// entry left the active path (the session branched away), which makes the
// summary describe history the model can no longer see.
func (a *Agent) applyAsyncCompaction(history []ai.Message) ([]ai.Message, bool) {
	job := a.compactAsync
	if job == nil {
		return history, false
	}
	var res asyncCompactResult
	select {
	case res = <-job.result:
	default:
		return history, false // still running
	}
	a.compactAsync = nil
	job.cancel()
	if res.err != nil {
		logx.Errorf("compaction: async summarize: %v", res.err)
		return history, false
	}
	if res.summary.Role == "" {
		return history, false
	}
	fres, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		logx.Errorf("compaction: async: %v", err)
		return history, false
	}
	if !slices.Contains(fres.EntryIDs, job.anchor) {
		logx.Infof("compaction: async result dropped — its anchor left the active path")
		return history, false
	}
	anchor := job.anchor
	entry := &session.CompactionEntry{
		Summary:          res.summary,
		FirstKeptEntryID: &anchor,
		TokensBefore:     job.tokens,
		Method:           MethodHandoff,
	}
	if err := a.persistCompaction(entry); err != nil {
		logx.Errorf("compaction: %v", err)
		return history, false
	}
	logx.Infof("compaction: async summarize applied (%d tokens before)", job.tokens)
	if rebuilt, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{}); err == nil {
		return rebuilt.Messages, true
	}
	return history, true
}

// CancelAsyncCompaction aborts the in-flight background summarize, if any:
// nothing it produces is applied afterwards. Safe with no job in flight, so an
// abort path can call it unconditionally.
func (a *Agent) CancelAsyncCompaction() {
	if job := a.compactAsync; job != nil {
		a.compactAsync = nil
		job.cancel()
	}
}
