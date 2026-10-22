package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
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
	mu   sync.Mutex
	next int
	jobs map[string]*hubJob
	// foreground is the same record shape for a child a tool call spawned
	// synchronously (TrackForeground): read-only, never a job, so the roster
	// and the job ops below cannot act on one.
	foreground map[string]*hubJob
	inbox      map[string][]InboxMsg // steering traffic per job id (Send audit log)
	procs      *ProcTable            // named long-running child processes (lazy)
	// notify fires when a job settles; see SetNotify.
	notify func()
	// settleListeners fire on the same edge with the job snapshot resolved;
	// see AddSettleListener.
	settleListeners []func(JobInfo, *SubagentResult)
	// agentChange fires when an agent's status or metrics change; see
	// SetAgentChange.
	agentChange func()
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
	// startedAt/endedAt bracket the child's run for the record's
	// duration; endedAt is zero while the agent is live. elapsed banks the
	// runs that already finished, so a revived agent's duration is the sum
	// of its runs. canceled marks a run the user (or the session's end) cut
	// short — the record reports it as aborted rather than as a plain
	// failure.
	startedAt time.Time
	endedAt   time.Time
	elapsed   time.Duration
	canceled  bool
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

// AgentStatus is an agent's liveness, in omp's vocabulary.
type AgentStatus string

const (
	// AgentRunning: the agent is live.
	AgentRunning AgentStatus = "running"
	// AgentIdle: the agent is settled and at rest; its Outcome says how it
	// ended.
	AgentIdle AgentStatus = "idle"
	// AgentParked: the agent is settled and parked, waiting for a revive.
	AgentParked AgentStatus = "parked"
	// AgentAborted: the agent was canceled before it could finish.
	AgentAborted AgentStatus = "aborted"
)

// AgentOutcome is how a settled agent ended ("" while it is still live).
type AgentOutcome string

const (
	// AgentOutcomeDone: the agent finished its work.
	AgentOutcomeDone AgentOutcome = "done"
	// AgentOutcomeFailed: the agent ended without a result.
	AgentOutcomeFailed AgentOutcome = "failed"
)

// AgentKind separates a background job from a foreground task child.
type AgentKind string

const (
	// AgentKindJob: a background job the hub spawned.
	AgentKindJob AgentKind = "job"
	// AgentKindChild: a foreground child a task call spawned.
	AgentKindChild AgentKind = "child"
)

// AgentMetrics is what one agent spent and did.
type AgentMetrics struct {
	Requests int
	Tools    int
	Tokens   int64
	Cost     float64 // USD
	Duration time.Duration
}

// AgentRecord is the hub's projection of one agent — a background job or a
// foreground task child — for a host that renders an agent list.
//
// It is a VIEW, not a second registry: AgentRecords projects the same jobs
// and foreground maps the roster reads, so a record can never disagree with
// the roster about who exists or what state they are in.
type AgentRecord struct {
	ID       string // "hub-1" / "fg-1"
	Name     string // the child's label
	ParentID string // the parent's record id when the agent is a child
	Model    string
	Level    string // the effort rung the child was spawned with (spec.Thinking.Level)
	Activity string
	Status   AgentStatus
	Outcome  AgentOutcome
	Kind     AgentKind
	Metrics  AgentMetrics
}

// AgentRecords projects every agent the hub knows — the background jobs
// and the foreground children a task call spawned — into one flat list of
// records, oldest first.
//
// It is a PROJECTION, not a second registry: the records are read out of
// the same maps Roster and Foreground read, so a record can never disagree
// with the roster about who exists or what state they are in.
//
// The status vocabulary is omp's: a live agent is running, a settled one is
// idle (its Outcome says how it ended), a canceled one is aborted, and a
// parked one is parked. xdev's own roster words — done and failed — are
// outcomes here, not statuses.
func (h *Hub) AgentRecords() []AgentRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	// A parent is recorded by LABEL (SubagentSpec.Parent), not by id, so
	// the label → id map is what turns that label into a parent id. The
	// first record wearing a label wins; a duplicate label is a name
	// collision, and the older agent is the likelier parent.
	byLabel := map[string]string{}
	for i := 1; i <= h.next; i++ {
		if j, ok := h.jobs[fmt.Sprintf("hub-%d", i)]; ok {
			if _, dup := byLabel[j.Label]; !dup && j.Label != "" {
				byLabel[j.Label] = j.ID
			}
		}
		if j, ok := h.foreground[fmt.Sprintf("fg-%d", i)]; ok {
			if _, dup := byLabel[j.Label]; !dup && j.Label != "" {
				byLabel[j.Label] = j.ID
			}
		}
	}
	out := make([]AgentRecord, 0, len(h.jobs)+len(h.foreground))
	for i := 1; i <= h.next; i++ {
		if j, ok := h.jobs[fmt.Sprintf("hub-%d", i)]; ok {
			out = append(out, h.recordFor(j, AgentKindJob, byLabel))
		}
		if j, ok := h.foreground[fmt.Sprintf("fg-%d", i)]; ok {
			out = append(out, h.recordFor(j, AgentKindChild, byLabel))
		}
	}
	return out
}

// recordFor projects one job (background or foreground) into a record. The
// caller holds h.mu.
func (h *Hub) recordFor(j *hubJob, kind AgentKind, byLabel map[string]string) AgentRecord {
	s := childStats(j)
	rec := AgentRecord{
		ID:       j.ID,
		Name:     j.Label,
		Model:    j.spec.Model,
		Kind:     kind,
		Activity: s.activity,
		Metrics: AgentMetrics{
			Requests: s.requests,
			Tools:    s.tools,
			Tokens:   s.tokens,
			Cost:     s.cost,
			Duration: j.duration(),
		},
	}
	if j.spec.Thinking != nil {
		rec.Level = j.spec.Thinking.Level
	}
	if p := j.spec.Parent; p != "" {
		rec.ParentID = byLabel[p]
	}
	rec.Status, rec.Outcome = j.statusOutcome()
	return rec
}

// statusOutcome maps a job's live state onto omp's status vocabulary. The
// caller holds h.mu.
func (j *hubJob) statusOutcome() (AgentStatus, AgentOutcome) {
	settled := false
	select {
	case <-j.done:
		settled = true
	default:
	}
	switch {
	case j.parked:
		return AgentParked, j.settledOutcome()
	case j.canceled || j.wasCanceled():
		if !settled {
			// Canceled but not yet wound down: the agent is aborted, and
			// its outcome is still unknown.
			return AgentAborted, ""
		}
		return AgentAborted, AgentOutcomeFailed
	case settled:
		return AgentIdle, j.settledOutcome()
	}
	return AgentRunning, ""
}

// settledOutcome is how a finished agent ended. The caller holds h.mu.
func (j *hubJob) settledOutcome() AgentOutcome {
	if j.Err != nil || j.Result == nil {
		return AgentOutcomeFailed
	}
	switch j.Result.Status {
	case "failed", "schema-mismatch":
		return AgentOutcomeFailed
	}
	return AgentOutcomeDone
}

// wasCanceled reports whether a settled agent was cut short rather than
// failing on its own. The caller holds h.mu.
func (j *hubJob) wasCanceled() bool {
	if j.canceled {
		return true
	}
	if j.Err != nil && errors.Is(j.Err, context.Canceled) {
		return true
	}
	return j.Result != nil && j.Result.Err != "" &&
		strings.Contains(j.Result.Err, context.Canceled.Error())
}

// childStats is one walk over a job's child transcripts: the token and cost
// sums the provider reported, the request and tool counts the child's own
// messages carry, and the latest message snippet.
//
// The walk is the roster's own (rosterActivity reads the same stores); it
// is factored out here so the record projection and the roster cannot
// disagree about what a child spent.
//
// Requests counts the child's provider turns (each assistant message is one
// turn) and Tools counts the tool calls those turns made. Both come from
// the same store the hub already keeps, so neither costs a new dependency
// or a new scan.
func childStats(j *hubJob) (s jobStats) {
	var count int
	var last string
	for _, st := range j.stores {
		for _, e := range st.Entries() {
			me, ok := e.(*session.MessageEntry)
			if !ok {
				continue
			}
			count++
			if me.Message.Role == ai.RoleAssistant {
				s.requests++
				s.tools += len(me.Message.ToolCalls())
			}
			if me.Message.Usage != nil {
				s.tokens += me.Message.Usage.TotalTokens
				if me.Message.Usage.Cost != nil {
					s.cost += me.Message.Usage.Cost.Total
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
		s.activity = string(r)
	} else {
		s.activity = fmt.Sprintf("%d msgs", count)
	}
	return s
}

// jobStats is what one child's transcript adds up to.
type jobStats struct {
	tokens   int64
	cost     float64
	requests int
	tools    int
	activity string
}

// duration is the agent's elapsed time: the run's own length once it has
// settled, the time since it started while it is still live, plus whatever
// earlier runs banked (a revived agent's duration is its whole life). The
// caller holds h.mu.
func (j *hubJob) duration() time.Duration {
	d := j.elapsed
	if !j.startedAt.IsZero() {
		end := j.endedAt
		if end.IsZero() {
			end = time.Now()
		}
		d += end.Sub(j.startedAt)
	}
	return d
}

// SetAgentChange registers a callback that fires when an agent's status or
// metrics change: a child starts, settles, is parked, is revived, or is
// canceled. It is the seam an Agent Hub panel repaints on, so the panel
// never has to poll the hub at frame rate.
//
// Contract, like SetNotify's: the callback fires from the hub's own
// goroutine with no lock held, so it MAY read the hub (AgentRecords) but
// must not block and must not mutate the hub. A single callback, set once
// at startup.
func (h *Hub) SetAgentChange(fn func()) {
	h.mu.Lock()
	h.agentChange = fn
	h.mu.Unlock()
}

// fireChange calls an agent-change callback captured under the hub's lock.
// The caller must hold no lock: the callback is what a panel calls to
// re-read the hub, so it must never run with the hub's mutex held.
func fireChange(fn func()) {
	if fn != nil {
		fn()
	}
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

// TrackForeground registers a child that is NOT a background job — one a
// `task` call spawned synchronously — and returns the id its transcript is
// readable under plus the OnRun hook that captures the child's store.
//
// It exists for the same reason Start does, minus the lifetime: a foreground
// child blocks the turn that spawned it, so its transcript is the one thing a
// user cannot otherwise reach (the roster only lists jobs). The record is
// read-only — no cancel, no park, no revive — because the tool call already
// owns the child's context and those actions would have nothing honest to do
// to it.
//
// The id is unique per spawn ("fg-N") so a child's transcript cannot collide
// with a job's, and Transcript serves both from one store walk.
func (h *Hub) TrackForeground(spec SubagentSpec) (string, func(*Agent)) {
	job := &hubJob{Label: spec.Name, spec: spec, done: make(chan struct{}), startedAt: time.Now()}
	h.mu.Lock()
	if h.foreground == nil {
		h.foreground = map[string]*hubJob{}
	}
	h.next++
	id := fmt.Sprintf("fg-%d", h.next)
	job.ID = id
	h.foreground[id] = job
	change := h.agentChange
	h.mu.Unlock()
	fireChange(change)
	return id, func(ag *Agent) {
		if ag == nil || ag.Store == nil {
			return
		}
		h.mu.Lock()
		job.Agent = ag
		job.stores = append(job.stores, ag.Store)
		h.mu.Unlock()
	}
}

// SettleForeground marks a tracked foreground child terminal, so its roster
// row stops saying "running" once its call has returned. A status the child
// never reached (a killed process, a panic in the tool) is reported as
// failed rather than left running forever.
func (h *Hub) SettleForeground(id string, res *SubagentResult, err error) {
	h.mu.Lock()
	j, ok := h.foreground[id]
	if !ok {
		h.mu.Unlock()
		return
	}
	j.Result, j.Err = res, err
	j.endedAt = time.Now()
	close(j.done)
	notify := h.notify
	change := h.agentChange
	h.mu.Unlock()
	if notify != nil {
		notify()
	}
	// The child's status and metrics just changed.
	fireChange(change)
}

// Foreground reports one roster row per tracked foreground child, so a panel
// can name the child whose transcript it is about to show. Jobs are excluded:
// Roster already lists those, and one list mixing the two would offer controls
// (kill/park/revive) that only work on half its rows.
func (h *Hub) Foreground() []RosterEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []RosterEntry
	for i := 1; i <= h.next; i++ {
		j, ok := h.foreground[fmt.Sprintf("fg-%d", i)]
		if !ok {
			continue
		}
		e := RosterEntry{ID: j.ID, Name: j.Label, Model: j.spec.Model}
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
		e.Tokens, e.Cost, e.Activity = rosterActivity(j)
		out = append(out, e)
	}
	return out
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
	// A background job's lifetime is the SESSION, not the turn that
	// dispatched it. `parent` here is the tool-call context, which is canceled
	// the moment the turn ends — inheriting it killed every "background"
	// subagent as soon as the parent stopped waiting (reproduced: a child
	// running `sleep 25` reported `[killed: signal]` while the TUI was still
	// open). Values survive WithoutCancel; only the cancel/deadline edge is
	// dropped, and Close (session exit) or the roster's kill still end it.
	job := &hubJob{ID: id, Label: spec.Name, done: make(chan struct{}), spec: spec, parent: context.WithoutCancel(parent)}
	h.jobs[id] = job
	h.launchLocked(job, spec.Prompt)
	change := h.agentChange
	h.mu.Unlock()
	// A new agent exists: a panel showing the roster must learn about it
	// now, not on the next settle.
	fireChange(change)
	return id, nil
}

// launchLocked spawns (or re-spawns) the child for an existing job. The
// caller holds h.mu; the run goroutine re-locks when it settles.
func (h *Hub) launchLocked(job *hubJob, prompt string) {
	spec := job.spec
	spec.Prompt = prompt
	cctx, cancel := context.WithCancel(job.parent)
	job.cancel = cancel
	// A revived job's previous run is banked, so the record's duration is
	// the agent's whole life rather than its last leg.
	if !job.startedAt.IsZero() && !job.endedAt.IsZero() {
		job.elapsed += job.endedAt.Sub(job.startedAt)
	}
	job.startedAt = time.Now()
	job.endedAt = time.Time{}
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
		res, err := SpawnChild(cctx, spec)
		h.mu.Lock()
		if err != nil {
			job.Err = err
			if res == nil {
				res = &SubagentResult{Status: "failed", Err: err.Error()}
			}
		}
		if res != nil {
			job.Result = res
		}
		job.endedAt = time.Now()
		notify := h.notify
		change := h.agentChange
		listeners := append([]func(JobInfo, *SubagentResult){}, h.settleListeners...)
		h.mu.Unlock()
		// Close the settle edge before anyone is told about it: Roster() reads
		// `done` to decide whether a row still runs, so a bump that fired first
		// would repaint the pre-settle state and the dock would show a finished
		// child as running. The deferred close is gone on purpose — an unrecovered
		// panic here takes the process down, so there is no waiter left to hang.
		close(job.done)
		// A job may settle long after the turn that started it, and nothing else
		// repaints a roster nobody is looking at.
		if notify != nil {
			notify()
		}
		// The agent's status and metrics just changed: a panel showing the
		// roster has to learn about it now.
		fireChange(change)
		// Settle listeners get the snapshot rather than being told to go and
		// fetch it: SetNotify's contract forbids re-entering the Hub from the
		// callback, and building the notice needs the terminal status, which
		// statusLocked only reports once `done` is closed. One extra
		// uncontended lock acquisition on a path that just ran a whole
		// subagent is not worth a second contract carve-out.
		if len(listeners) > 0 {
			h.mu.Lock()
			info, _ := h.statusLocked(job.ID)
			result := job.Result
			h.mu.Unlock()
			for _, fn := range listeners {
				fn(info, result)
			}
		}
	}()
}

// SetNotify registers a callback that fires when a job settles — including a
// canceled or failed one, since those settle too. It exists for #291's context
// dock, which must show a live roster without being polled at frame rate.
//
// Starting, parking and reviving are deliberately not covered: a job starts
// inside a tool call, and the host bumps on the tool's end; park and revive are
// only reachable from the /hub overlay, which repaints itself.
//
// Contract, because the hub's lock is the transcript's rival: the callback fires
// from the job's own goroutine with no lock held, must not call back into the
// Hub, and must be cheap and non-blocking. A single callback, set once at
// startup, like the other host seams on this type.
func (h *Hub) SetNotify(fn func()) {
	h.mu.Lock()
	h.notify = fn
	h.mu.Unlock()
}

// AddSettleListener registers a callback that fires when a job settles, with
// that job's parent-visible snapshot and handoff already resolved. It is
// additive where SetNotify is a slot, so a host can have both a repaint (the
// dock) and a delivery (the model notice) without one replacing the other —
// the same additive shape launchLocked already uses for spec.OnRun.
//
// Listeners fire in registration order from the settling job's goroutine with
// no lock held, and inherit SetNotify's contract: do not call back into the
// Hub, and keep the callback cheap and non-blocking. A listener that blocks
// delays only its own job's goroutine, but a panic in one takes the process
// down, so keep the body to bookkeeping plus a hand-off.
//
// Because Revive and Send-to-parked re-enter launchLocked, a listener will see
// the same job id more than once; dedupe on JobInfo.ID.
func (h *Hub) AddSettleListener(fn func(JobInfo, *SubagentResult)) {
	if fn == nil {
		return
	}
	h.mu.Lock()
	h.settleListeners = append(h.settleListeners, fn)
	h.mu.Unlock()
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
// the usage the provider reported, and the latest message snippet. It is
// the same walk the agent record uses (childStats), so a roster row and a
// record can never disagree about what a child spent.
func rosterActivity(j *hubJob) (tokens int64, cost float64, activity string) {
	s := childStats(j)
	return s.tokens, s.cost, s.activity
}

// Transcript returns a child-session's messages with sequence >= fromSeq
// (incremental fetch; entries accumulate across revives). total is the full
// message count; ok is false for an unknown id. A foreground child's id
// (TrackForeground) resolves here too — the same walk, the same entries — so
// whatever reads a transcript needs no second code path for a child a tool
// call spawned.
func (h *Hub) Transcript(id string, fromSeq int) ([]TranscriptEntry, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok {
		j, ok = h.foreground[id]
	}
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
	j, ok := h.jobs[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("unknown job %q", id)
	}
	select {
	case <-j.done:
		if !j.parked {
			h.mu.Unlock()
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
		change := h.agentChange
		h.mu.Unlock()
		// Sending to a parked job revives it: the agent is running again.
		fireChange(change)
		return nil
	default:
	}
	if j.Agent == nil {
		h.mu.Unlock()
		return fmt.Errorf("job %q has not started yet", id)
	}
	h.recordInboxLocked(id, text)
	j.Agent.Steer(text)
	h.mu.Unlock()
	return nil
}

// Park moves a settled job into the parked state: it stays on the roster
// and Send/Revive can bring it back. Parking a running (or unknown) job is
// refused.
func (h *Hub) Park(id string) bool {
	h.mu.Lock()
	j, ok := h.jobs[id]
	if !ok {
		h.mu.Unlock()
		return false
	}
	select {
	case <-j.done:
		j.parked = true
		change := h.agentChange
		h.mu.Unlock()
		// The agent's status just changed (parked).
		fireChange(change)
		return true
	default:
		h.mu.Unlock()
		return false
	}
}

// Revive re-spawns a parked job under the same id with its prior final
// output injected as context and text ("", a generic continuation) as the
// new instruction.
func (h *Hub) Revive(id, text string) error {
	h.mu.Lock()
	j, ok := h.jobs[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("unknown job %q", id)
	}
	select {
	case <-j.done:
	default:
		h.mu.Unlock()
		return fmt.Errorf("job %q is still running", id)
	}
	if !j.parked {
		h.mu.Unlock()
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
	change := h.agentChange
	h.mu.Unlock()
	// The agent is running again: its status and metrics changed.
	fireChange(change)
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
// Close cancels every still-running job. Jobs are detached from the turn
// that spawned them (see Start), so this is the session-lifetime boundary:
// each run mode calls it on exit, exactly like stopping the hub's supervised
// processes. Waiting is deliberately bounded — a child that ignores its
// context must not hang the exit.
func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	var pending []chan struct{}
	for _, j := range h.jobs {
		select {
		case <-j.done:
		default:
			if j.cancel != nil {
				j.cancel()
			}
			pending = append(pending, j.done)
		}
	}
	h.mu.Unlock()
	deadline := time.After(2 * time.Second)
	for _, d := range pending {
		select {
		case <-d:
		case <-deadline:
			return // a stubborn child is reaped by process exit anyway
		}
	}
}

func (h *Hub) Cancel(id string) bool {
	h.mu.Lock()
	j, ok := h.jobs[id]
	if !ok {
		h.mu.Unlock()
		return false
	}
	select {
	case <-j.done:
		h.mu.Unlock()
		return false
	default:
	}
	// The child is cut short: the record reports it as aborted, not as a
	// plain failure, and the panel must learn about it now.
	j.canceled = true
	change := h.agentChange
	cancel := j.cancel
	h.mu.Unlock()
	fireChange(change)
	if cancel != nil {
		cancel()
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
