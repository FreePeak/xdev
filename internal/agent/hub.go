package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/session"
)

// Hub is the coordination surface over background subagents (M11, research
// §2). The task tool spawns children in-process synchronously; a hub lets
// the parent start children in the background and then steer (send), park
// on (wait), inspect (jobs), or abort (cancel) them while they run.
//
// Job IDs are process-local ("hub-1", "hub-2", ...). A settled job keeps
// its result readable until the hub is dropped, and can be parked: Send
// (or Revive) to a parked job re-spawns the child with its prior output
// injected as context (issue #37).
type Hub struct {
	mu    sync.Mutex
	next  int
	jobs  map[string]*hubJob
	inbox map[string][]InboxMsg // steering traffic per job id (Send audit log)
	procs *ProcTable            // named long-running child processes (lazy)
}

type hubJob struct {
	ID     string
	Label  string
	Agent  *Agent // live child agent; nil until the child's run started
	cancel context.CancelFunc
	done   chan struct{}
	Result *SubagentResult
	Err    error
	// Revival state (issue #37): spec + parent context allow a parked job
	// to re-spawn with its prior output injected as context; child stores
	// accumulate so transcripts survive across revives.
	spec   SubagentSpec
	parent context.Context
	parked bool
	stores []*session.Store
}

// JobInfo is the parent-visible snapshot of one job.
type JobInfo struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"` // running | yielded | completed | schema-mismatch | failed | canceled
	Parked bool   `json:"parked,omitempty"`
}

// InboxMsg is one steering message recorded by Send. xdev children consume
// steering at their next step boundary automatically, so the hub-side inbox
// is the traffic log an agent (or the TUI inspector) can review, not a poll
// queue.
type InboxMsg struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// TranscriptEntry is one rendered row of a job's child transcript.
type TranscriptEntry struct {
	Seq  int    `json:"seq"`
	Role string `json:"role"`
	Text string `json:"text"`
}

// RosterEntry is one row of the Agent Hub roster (TUI inspector + hub list
// op). Status is the normalized roster enum: running | idle | parked | done
// | failed. xdev children run to completion, so "idle" only appears for a
// queued job whose child has not started its run yet; "parked" is the
// user-settled state Send/Revive can bring back.
type RosterEntry struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Model    string  `json:"model"`
	Activity string  `json:"activity"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"` // USD; 0 when the provider reports none
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
	job := &hubJob{ID: id, Label: spec.Name, done: make(chan struct{}), spec: spec, parent: parent}
	h.jobs[id] = job
	h.launchLocked(job, spec.Prompt)
	h.mu.Unlock()
	return id, nil
}

// launchLocked spawns (or re-spawns) the child for an existing job. The
// caller holds h.mu; the run goroutine re-locks when it settles.
func (h *Hub) launchLocked(job *hubJob, prompt string) {
	spec := job.spec
	spec.Prompt = prompt
	cctx, cancel := context.WithCancel(job.parent)
	job.cancel = cancel
	userOnRun := spec.OnRun
	spec.OnRun = func(child *Agent) {
		h.mu.Lock()
		job.Agent = child
		if st := child.Store; st != nil {
			job.stores = append(job.stores, st)
		}
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
	info := JobInfo{ID: j.ID, Label: j.Label, Status: "running", Parked: j.parked}
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

// Roster snapshots every job for the Agent Hub inspector (issue #37),
// oldest first.
func (h *Hub) Roster() []RosterEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]RosterEntry, 0, len(h.jobs))
	for i := 1; i <= h.next; i++ {
		j, ok := h.jobs[fmt.Sprintf("hub-%d", i)]
		if !ok {
			continue
		}
		e := RosterEntry{ID: j.ID, Name: j.Label, Model: j.spec.Model}
		if j.parked {
			e.Status = "parked"
		} else {
			select {
			case <-j.done:
				if j.Err != nil || j.Result == nil || j.Result.Status == "failed" ||
					j.Result.Status == "schema-mismatch" || j.Result.Status == "canceled" {
					e.Status = "failed"
				} else {
					e.Status = "done"
				}
			default:
				if j.Agent == nil {
					e.Status = "idle"
				} else {
					e.Status = "running"
				}
			}
		}
		e.Tokens, e.Cost, e.Activity = rosterActivity(j)
		out = append(out, e)
	}
	return out
}

// rosterActivity summarizes a job's child transcripts: token/cost sums from
// the usage the provider reported, and the latest message snippet.
func rosterActivity(j *hubJob) (tokens int64, cost float64, activity string) {
	var count int
	var last string
	for _, st := range j.stores {
		for _, e := range st.Entries() {
			me, ok := e.(*session.MessageEntry)
			if !ok {
				continue
			}
			count++
			if me.Message.Usage != nil {
				tokens += me.Message.Usage.TotalTokens
				if me.Message.Usage.Cost != nil {
					cost += me.Message.Usage.Cost.Total
				}
			}
			if t := strings.TrimSpace(messageText(me.Message)); t != "" {
				last = t
			}
		}
	}
	if last != "" {
		r := []rune(last)
		if len(r) > 40 {
			r = append(r[:40], '…')
		}
		activity = string(r)
	} else {
		activity = fmt.Sprintf("%d msgs", count)
	}
	return tokens, cost, activity
}

// Transcript returns a job's child-session messages with sequence >= fromSeq
// (incremental fetch; entries accumulate across revives). total is the full
// message count; ok is false for unknown jobs.
func (h *Hub) Transcript(id string, fromSeq int) ([]TranscriptEntry, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		return nil, 0, false
	}
	var out []TranscriptEntry
	total := 0
	for _, st := range j.stores {
		for _, e := range st.Entries() {
			me, isMsg := e.(*session.MessageEntry)
			if !isMsg {
				continue
			}
			if total >= fromSeq {
				out = append(out, TranscriptEntry{
					Seq:  total,
					Role: string(me.Message.Role),
					Text: messageText(me.Message),
				})
			}
			total++
		}
	}
	return out, total, true
}

// Inbox returns the steering traffic recorded for one job ("" = all jobs).
func (h *Hub) Inbox(id string) map[string][]InboxMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]InboxMsg{}
	for jobID, msgs := range h.inbox {
		if id != "" && jobID != id {
			continue
		}
		out[jobID] = append([]InboxMsg(nil), msgs...)
	}
	return out
}

func (h *Hub) recordInboxLocked(id, text string) {
	if h.inbox == nil {
		h.inbox = map[string][]InboxMsg{}
	}
	h.inbox[id] = append(h.inbox[id], InboxMsg{At: time.Now().UTC(), Text: text})
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
// child's next step boundary. Sending to a parked job revives it: the child
// re-spawns with the prior final output injected as context and text as the
// new instruction (issue #37).
func (h *Hub) Send(id, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		return fmt.Errorf("unknown job %q", id)
	}
	select {
	case <-j.done:
		if !j.parked {
			return fmt.Errorf("job %q already finished (park it to revive)", id)
		}
		prior := ""
		if j.Result != nil {
			prior = j.Result.Text
		}
		j.parked = false
		j.Agent, j.Result, j.Err = nil, nil, nil
		j.done = make(chan struct{})
		h.recordInboxLocked(id, text)
		h.launchLocked(j, revivePrompt(prior, text))
		return nil
	default:
	}
	if j.Agent == nil {
		return fmt.Errorf("job %q has not started yet", id)
	}
	h.recordInboxLocked(id, text)
	j.Agent.Steer(text)
	return nil
}

// Park moves a settled job into the parked state: it stays on the roster
// and Send/Revive can bring it back. Parking a running (or unknown) job is
// refused.
func (h *Hub) Park(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		return false
	}
	select {
	case <-j.done:
		j.parked = true
		return true
	default:
		return false
	}
}

// Revive re-spawns a parked job under the same id with its prior final
// output injected as context and text ("", a generic continuation) as the
// new instruction.
func (h *Hub) Revive(id, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		return fmt.Errorf("unknown job %q", id)
	}
	select {
	case <-j.done:
	default:
		return fmt.Errorf("job %q is still running", id)
	}
	if !j.parked {
		return fmt.Errorf("job %q is not parked", id)
	}
	prior := ""
	if j.Result != nil {
		prior = j.Result.Text
	}
	j.parked = false
	j.Agent, j.Result, j.Err = nil, nil, nil
	j.done = make(chan struct{})
	h.launchLocked(j, revivePrompt(prior, text))
	return nil
}

func revivePrompt(prior, text string) string {
	var b strings.Builder
	b.WriteString("Your previous run finished with this result:\n\n")
	if strings.TrimSpace(prior) != "" {
		b.WriteString(prior)
	} else {
		b.WriteString("(no output was returned)")
	}
	b.WriteString("\n\nThe session was revived to continue the work.")
	if strings.TrimSpace(text) != "" {
		b.WriteString("\n\nNew instruction:\n\n")
		b.WriteString(text)
	}
	return b.String()
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

// StopProcesses terminates every supervised child process (hub start). The
// run modes defer it on exit: xdev has no persist/detach concept, so a
// process the model started is session-scoped by definition and must not
// outlive the session as an orphan (parity finding T3 #8).
func (h *Hub) StopProcesses() {
	if h == nil {
		return
	}
	h.mu.Lock()
	t := h.procs
	h.mu.Unlock()
	t.StopAll()
}

// Procs returns the hub's process table (named long-running child
// processes, M11 research §2), created on first use.
func (h *Hub) Procs() *ProcTable {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.procs == nil {
		h.procs = NewProcTable()
	}
	return h.procs
}
