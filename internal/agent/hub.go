package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Hub is the coordination surface over background subagents (M11, research
// §2). The task tool spawns children in-process synchronously; a hub lets
// the parent start children in the background and then steer (send), park
// on (wait), inspect (jobs), or abort (cancel) them while they run.
//
// Job IDs are process-local ("hub-1", "hub-2", ...). A settled job keeps
// its result readable until the hub is dropped.
type Hub struct {
	mu   sync.Mutex
	next int
	jobs map[string]*hubJob
}

type hubJob struct {
	ID     string
	Label  string
	Agent  *Agent // live child agent; nil until the child's run started
	cancel context.CancelFunc
	done   chan struct{}
	Result *SubagentResult
	Err    error
}

// JobInfo is the parent-visible snapshot of one job.
type JobInfo struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"` // running | yielded | completed | schema-mismatch | failed | canceled
}

func NewHub() *Hub {
	return &Hub{jobs: map[string]*hubJob{}}
}

// Start launches a subagent in the background and returns its job id.
// The child inherits everything the synchronous path would (approval
// posture, persistence, yield contract); the only difference is that the
// caller does not block on it.
func (h *Hub) Start(parent context.Context, spec SubagentSpec) (string, error) {
	if parent == nil {
		parent = context.Background()
	}
	h.mu.Lock()
	if h.jobs == nil {
		h.jobs = map[string]*hubJob{}
	}
	h.next++
	id := fmt.Sprintf("hub-%d", h.next)
	job := &hubJob{ID: id, Label: spec.Name, done: make(chan struct{})}
	h.jobs[id] = job
	h.mu.Unlock()

	cctx, cancel := context.WithCancel(parent)
	job.cancel = cancel
	userOnRun := spec.OnRun
	spec.OnRun = func(child *Agent) {
		h.mu.Lock()
		job.Agent = child
		h.mu.Unlock()
		if userOnRun != nil {
			userOnRun(child)
		}
	}
	go func() {
		defer close(job.done)
		res, err := SpawnChild(cctx, spec)
		h.mu.Lock()
		defer h.mu.Unlock()
		if err != nil {
			job.Err = err
			if res == nil {
				res = &SubagentResult{Status: "failed", Err: err.Error()}
			}
		}
		if res != nil {
			job.Result = res
		}
	}()
	return id, nil
}

// Status reports one job's state without blocking.
func (h *Hub) Status(id string) (JobInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusLocked(id)
}

func (h *Hub) statusLocked(id string) (JobInfo, bool) {
	j, ok := h.jobs[id]
	if !ok {
		return JobInfo{}, false
	}
	info := JobInfo{ID: j.ID, Label: j.Label, Status: "running"}
	select {
	case <-j.done:
		if j.Err != nil {
			info.Status = "failed"
		} else if j.Result != nil {
			info.Status = j.Result.Status
		} else {
			info.Status = "completed"
		}
	default:
	}
	return info, true
}

// Jobs lists every job, oldest first.
func (h *Hub) Jobs() []JobInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]JobInfo, 0, len(h.jobs))
	for i := 1; i <= h.next; i++ {
		if info, ok := h.statusLocked(fmt.Sprintf("hub-%d", i)); ok {
			out = append(out, info)
		}
	}
	return out
}

// Wait blocks until one of the named jobs settles (or any job when ids is
// empty), the timeout elapses, or the context is canceled. Returns the
// ids that are settled when it returns.
func (h *Hub) Wait(ctx context.Context, ids []string, timeout time.Duration) []string {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	// ponytail: a 50ms poll is plain and correct for hub-scale job counts;
	// a condition-variable design buys latency xdev does not need yet.
	for {
		if settled := h.Settled(ids); len(settled) > 0 {
			return settled
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Settled returns the ids (subset of ids, or all jobs when empty) that
// have finished.
func (h *Hub) Settled(ids []string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	consider := func(id string) {
		if info, ok := h.statusLocked(id); ok && info.Status != "running" {
			out = append(out, id)
		}
	}
	if len(ids) == 0 {
		for i := 1; i <= h.next; i++ {
			consider(fmt.Sprintf("hub-%d", i))
		}
		return out
	}
	for _, id := range ids {
		if _, ok := h.jobs[id]; ok {
			consider(id)
		}
	}
	return out
}

// Send steers a running job: the text is queued as a user message at the
// child's next step boundary.
func (h *Hub) Send(id, text string) error {
	h.mu.Lock()
	j, ok := h.jobs[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("hub: unknown job %q", id)
	}
	ag := j.Agent
	h.mu.Unlock()
	select {
	case <-j.done:
		return fmt.Errorf("hub: job %q already finished", id)
	default:
	}
	if ag == nil {
		return fmt.Errorf("hub: job %q has not started yet", id)
	}
	ag.Steer(text)
	return nil
}

// Cancel aborts a running job (its child run fails with a canceled
// status); canceling a settled job is a no-op returning false.
func (h *Hub) Cancel(id string) bool {
	h.mu.Lock()
	j, ok := h.jobs[id]
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-j.done:
		return false
	default:
	}
	if j.cancel != nil {
		j.cancel()
	}
	return true
}

// Result returns a settled job's handoff (ok=false while still running or
// for unknown ids).
func (h *Hub) Result(id string) (*SubagentResult, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		return nil, false
	}
	select {
	case <-j.done:
		return j.Result, j.Result != nil
	default:
		return nil, false
	}
}
