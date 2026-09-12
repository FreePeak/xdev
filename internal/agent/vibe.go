package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// Vibe mode (M14 #58, omp vibe-mode.md): the top-level session becomes a
// DIRECTOR — it reads and steers, it never edits or executes. The active
// toolset shrinks to read + the parent-owned todo + five worker tools, and
// all real work happens in persistent worker subagents driven through the
// hub's existing subagent machinery (no second spawner lives here).
//
// A worker is a keep-alive subagent with its own child transcript: spawn
// starts its first turn as a background hub job, send steers the turn in
// flight or revives the worker for the next one, wait is the join point,
// kill retires it, status reports. Worker descriptors are persisted as
// session custom entries, so resuming a session rehydrates its workers
// instead of forgetting them.

// Worker-control tool names.
const (
	VibeSpawnToolName  = "vibe_spawn"
	VibeSendToolName   = "vibe_send"
	VibeWaitToolName   = "vibe_wait"
	VibeKillToolName   = "vibe_kill"
	VibeStatusToolName = "vibe_status"
)

// Session custom-entry types: worker descriptors and the mode flag.
const (
	VibeWorkerEntry = "vibe_worker"
	VibeModeEntry   = "vibe_mode"
)

// Worker lifecycle states. "idle" is the rehydrated state — the process
// that ran the worker is gone, so nothing is running and nothing resumes by
// itself. "killed" (an explicit kill or a mode exit) is terminal.
const (
	VibeRunning = "running"
	VibeIdle    = "idle"
	VibeDone    = "done"
	VibeFailed  = "failed"
	VibeKilled  = "killed"
)

// vibeOutputCap bounds the worker text a vibe tool hands back to the
// director: long output belongs in the child session, not in the director's
// context window.
const vibeOutputCap = 4000

// VibeDirectorPrompt is appended to the director's system prompt while the
// mode is active.
const VibeDirectorPrompt = `You are in VIBE MODE: you are the director, workers do the work.
- Your active tools are read, todo, and the vibe_* worker tools. You cannot edit, write, or run commands yourself — do not try.
- Split the request into independent workstreams and give each its own worker (vibe_spawn) with a self-contained brief: files, constraints, and observable acceptance criteria. Workers start blank and never see this conversation.
- Keep directing other workers while turns are in flight; call vibe_wait only when blocked. Route by difficulty: fast for mechanical execution and drafts, good for design, judgment, and review.
- A settled turn is not a correct result: read the touched files and inspect the full output before claiming anything is done. Reconcile verified work in todo.`

// VibeTier is one worker tier: the bundled agent it selects and the model
// role it resolves through.
type VibeTier struct {
	CLI   string // the vibe_spawn cli value
	Agent string // bundled agent definition
	Role  string // model role alias
}

// VibeTiers are the two tiers, fast first.
var VibeTiers = []VibeTier{
	{CLI: "fast", Agent: "sonic", Role: "@smol"},
	{CLI: "good", Agent: "task", Role: "@task"},
}

// VibeTierOf resolves a tier name; the bundled agent names work as aliases.
func VibeTierOf(name string) (VibeTier, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "fast", "sonic", "smol":
		return VibeTiers[0], true
	case "good", "task":
		return VibeTiers[1], true
	}
	return VibeTier{}, false
}

// tierOf maps a worker's tier back onto its definition (unknown → fast).
func tierOf(name string) VibeTier {
	if t, ok := VibeTierOf(name); ok {
		return t
	}
	return VibeTiers[0]
}

// vibeAgentBrief is the tier-specific guidance appended to the child's base
// system prompt.
var vibeAgentBrief = map[string]string{
	"fast": "You are a fast execution worker: mechanical, fully-specified work — batch edits, searches, running the build and the tests. Act instead of deliberating, and report exactly what you changed and the verification you ran.",
	"good": "You are a senior worker: design calls, ambiguity, and reviewing another worker's output. Read the code before judging it; report risks, the evidence, and the exact next step.",
}

// vibeAgent is the tier's BUNDLED agent definition. It is built here rather
// than discovered so a project agent that happens to be named "sonic" or
// "task" can never hijack a tier. omp's bundled defs also pin a tool list;
// the worker's tool surface comes from the host's task tool (which carries
// the parent's approval posture), so a tier overrides prompt and model only.
func vibeAgent(t VibeTier) AgentDefinition {
	return AgentDefinition{
		Name:         t.Agent,
		Description:  "vibe worker tier " + t.CLI,
		SystemPrompt: SubagentSystemPromptBase + "\n\n" + vibeAgentBrief[t.CLI],
	}
}

// VibeModel is the resolved provider/model for one tier. The host resolves
// it (role alias → settings override → parent model fallback), so the agent
// package stays free of config layering.
type VibeModel struct {
	Provider ai.Provider
	Model    string
	Thinking *ai.ThinkingBudget
}

// label is the human-facing name of the resolved model.
func (m *VibeModel) label() string {
	if m == nil {
		return ""
	}
	if m.Provider == nil {
		return m.Model
	}
	return m.Provider.Name() + "/" + m.Model
}

// VibeWorker is one persistent worker: a keep-alive subagent with its own
// child transcript, driven by the director through vibe_send.
type VibeWorker struct {
	ID      string    `json:"id"`
	Tier    string    `json:"tier"`
	Status  string    `json:"status"`
	Model   string    `json:"model,omitempty"`
	Job     string    `json:"job,omitempty"` // hub job id ("" when rehydrated)
	Session string    `json:"session,omitempty"`
	Brief   string    `json:"brief,omitempty"`
	Output  string    `json:"output,omitempty"` // last settled turn's text
	Turn    int       `json:"turn"`
	Created time.Time `json:"created"`
}

// data is the persisted descriptor shape (session custom-entry payload).
func (w VibeWorker) data() map[string]any {
	b, err := json.Marshal(w)
	if err != nil {
		return map[string]any{"id": w.ID, "tier": w.Tier, "status": w.Status}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{"id": w.ID, "tier": w.Tier, "status": w.Status}
	}
	return out
}

// workerOf decodes one persisted descriptor.
func workerOf(data map[string]any) (VibeWorker, bool) {
	b, err := json.Marshal(data)
	if err != nil {
		return VibeWorker{}, false
	}
	var w VibeWorker
	if err := json.Unmarshal(b, &w); err != nil || w.ID == "" {
		return VibeWorker{}, false
	}
	return w, true
}

// LoadVibe replays a session's vibe custom entries: the last mode flag wins
// and worker descriptors are upserted by id (every lifecycle transition
// persists the whole descriptor). A worker interrupted by a process exit
// comes back idle — nothing is running and nothing resumes by itself;
// killed workers stay terminal.
func LoadVibe(entries []session.Entry) (workers []VibeWorker, active bool) {
	idx := map[string]int{}
	for _, e := range entries {
		ce, ok := e.(*session.CustomEntry)
		if !ok {
			continue
		}
		switch ce.CustomType {
		case VibeModeEntry:
			on, _ := ce.Data["on"].(bool)
			active = on
		case VibeWorkerEntry:
			w, ok := workerOf(ce.Data)
			if !ok {
				continue
			}
			if w.Status == VibeRunning {
				w.Status = VibeIdle
			}
			if i, seen := idx[w.ID]; seen {
				workers[i] = w
				continue
			}
			idx[w.ID] = len(workers)
			workers = append(workers, w)
		}
	}
	return workers, active
}

// VibeConfig wires a scope to the host.
type VibeConfig struct {
	// Hub runs worker turns (the session's hub registry).
	Hub *Hub
	// Task is the subagent template workers inherit: tool surface, working
	// directory, persistence dir, and approval posture.
	Task *TaskTool
	// Tools is the parent registry: the source of the director's read and
	// todo tools, and the toolset restored on exit.
	Tools *tool.Registry
	// Resolve maps a tier's model role onto a provider/model.
	Resolve func(role string) (*VibeModel, error)
	// Persist records a lifecycle event as a session custom entry.
	Persist func(customType string, data map[string]any)
	// ParentID stamps worker child sessions with the owning session id.
	ParentID func() string
	// Conflicts names the incompatible modes that are active right now
	// (plan, goal): entering vibe while non-empty is refused.
	Conflicts func() []string
	// OnSettle is called from the worker goroutine when a turn settles, so
	// the host can surface the result in the transcript.
	OnSettle func(w VibeWorker)
}

// VibeScope is the session-scoped director state: the mode flag and the
// worker registry. Safe for concurrent use — worker tools run on the
// director's turn while settle watchers run on their own goroutines.
type VibeScope struct {
	cfg  VibeConfig
	view *tool.Registry

	mu      sync.Mutex
	active  bool
	workers []*VibeWorker
	seq     int
}

// NewVibeScope builds the scope and its restricted view: a fresh read tool
// (the parent registry keeps its own freshness records), the parent's todo
// tool (shared — the list the director edits is the session's list), and
// the five worker tools.
func NewVibeScope(cfg VibeConfig) *VibeScope {
	v := &VibeScope{cfg: cfg, view: tool.NewRegistry()}
	if cfg.Tools != nil {
		if t, ok := cfg.Tools.Get("todo"); ok {
			v.view.Register(t)
		}
	}
	v.view.Register(tool.NewReadTool())
	for _, t := range v.tools() {
		v.view.Register(t)
	}
	return v
}

// Registry is the director's restricted toolset. It is populated once at
// construction; only an active mode routes the agent to it, so entering and
// leaving costs no registry surgery and the exit path restores the parent
// registry by construction.
func (v *VibeScope) Registry() *tool.Registry { return v.view }

// Active reports whether the director mode is on.
func (v *VibeScope) Active() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.active
}

// Enter turns the mode on. An active plan or goal mode refuses the entry:
// the director's reduced toolset and a read-only/budgeted mode contradict
// each other, and silently dropping one would lose the user's state.
func (v *VibeScope) Enter() error {
	if v.cfg.Hub == nil || v.cfg.Task == nil {
		return fmt.Errorf("vibe: no subagent hub wired")
	}
	if v.cfg.Conflicts != nil {
		if c := v.cfg.Conflicts(); len(c) > 0 {
			return fmt.Errorf("vibe: exit %s mode first — vibe is mutually exclusive with plan and goal modes", strings.Join(c, " and "))
		}
	}
	v.mu.Lock()
	v.active = true
	v.mu.Unlock()
	v.persist(VibeModeEntry, map[string]any{"on": true, "ts": time.Now().UTC()})
	return nil
}

// Exit turns the mode off: every in-flight worker turn is canceled and every
// worker becomes terminal — a worker never outlives an intentional mode
// exit. The prior toolset is restored by construction: the mode only ever
// selected the restricted view, it never mutated the parent registry.
func (v *VibeScope) Exit() {
	v.mu.Lock()
	v.active = false
	ids := make([]string, 0, len(v.workers))
	for _, w := range v.workers {
		ids = append(ids, w.ID)
	}
	v.mu.Unlock()

	for _, id := range ids {
		if w, ok := v.snapshot(id); ok && w.Job != "" {
			v.cfg.Hub.Cancel(w.Job) // no-op for a settled job
		}
		if w, ok := v.setState(id, func(x *VibeWorker) { x.Status = VibeKilled }); ok {
			v.persist(VibeWorkerEntry, w.data())
		}
	}
	v.persist(VibeModeEntry, map[string]any{"on": false, "ts": time.Now().UTC()})
}

// Restore adopts a resumed session's persisted state: the worker
// descriptors land in the registry and the mode flag follows the session
// (nothing is running in a fresh process, so this only re-registers).
func (v *VibeScope) Restore(workers []VibeWorker, active bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.workers = nil
	for _, w := range workers {
		cp := w
		// Hub job ids are process-local: a resumed worker has no job yet,
		// so vibe_send starts its next turn in this process (the persisted
		// id stays history, never a handle into a foreign hub).
		cp.Job = ""
		v.workers = append(v.workers, &cp)
		if n := idSeq(cp.ID); n > v.seq {
			v.seq = n
		}
	}
	v.active = active && v.cfg.Hub != nil && v.cfg.Task != nil
}

// Workers snapshots the registry (spawn order), refreshing live job state
// first so no caller sees a stale status.
func (v *VibeScope) Workers() []VibeWorker {
	v.refresh()
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]VibeWorker, 0, len(v.workers))
	for _, w := range v.workers {
		out = append(out, *w)
	}
	return out
}

// Status renders the worker registry for /vibe status.
func (v *VibeScope) Status() string {
	rows := v.Workers()
	if len(rows) == 0 {
		return "vibe: no workers — vibe_spawn one (fast|good)"
	}
	running := 0
	for _, w := range rows {
		if w.Status == VibeRunning {
			running++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "vibe: %d worker(s)", len(rows))
	if running > 0 {
		fmt.Fprintf(&b, ", %d running", running)
	}
	b.WriteString("\n")
	for _, w := range rows {
		fmt.Fprintf(&b, "%-12s %-5s %-8s turn %d  %-20s %s\n",
			w.ID, w.Tier, w.Status, w.Turn, w.Model, VibePreview(w.Output))
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- worker registry internals ---

// findLocked resolves a selector (exact id, else a unique prefix) under mu.
func (v *VibeScope) findLocked(sel string) *VibeWorker {
	if sel == "" {
		return nil
	}
	var hit *VibeWorker
	matches := 0
	for _, w := range v.workers {
		if w.ID == sel {
			return w
		}
		if strings.HasPrefix(w.ID, sel) {
			hit, matches = w, matches+1
		}
	}
	if matches == 1 {
		return hit
	}
	return nil
}

// snapshot returns a copy of one worker (the returned value is not shared).
func (v *VibeScope) snapshot(sel string) (VibeWorker, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if w := v.findLocked(strings.TrimSpace(sel)); w != nil {
		return *w, true
	}
	return VibeWorker{}, false
}

// setState mutates one worker under mu and returns the updated copy.
func (v *VibeScope) setState(id string, fn func(*VibeWorker)) (VibeWorker, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	w := v.findLocked(id)
	if w == nil {
		return VibeWorker{}, false
	}
	fn(w)
	return *w, true
}

// add installs a new worker record.
func (v *VibeScope) add(w *VibeWorker) {
	v.mu.Lock()
	v.workers = append(v.workers, w)
	v.mu.Unlock()
}

// persist records one lifecycle entry (nil Persist = memory-only).
func (v *VibeScope) persist(customType string, data map[string]any) {
	if v.cfg.Persist != nil {
		v.cfg.Persist(customType, data)
	}
}

// refresh pulls settled job outcomes into the descriptors.
func (v *VibeScope) refresh() {
	if v.cfg.Hub == nil {
		return
	}
	v.mu.Lock()
	type pending struct {
		id, job string
		turn    int
	}
	var todo []pending
	for _, w := range v.workers {
		if w.Job == "" || w.Status != VibeRunning {
			continue
		}
		// Only a settled job has an outcome to record; a turn still in
		// flight keeps its running state.
		if info, ok := v.cfg.Hub.Status(w.Job); ok && info.Status != "running" {
			todo = append(todo, pending{w.ID, w.Job, w.Turn})
		}
	}
	v.mu.Unlock()
	for _, p := range todo {
		v.settleJob(p.id, p.job, p.turn)
	}
}

// watch blocks a goroutine on one worker turn and records its settlement.
// Wait with a zero timeout returns as soon as the job settles (or the hub
// is dropped), so the goroutine is bounded by the turn itself.
func (v *VibeScope) watch(id, job string, turn int) {
	if v.cfg.Hub == nil {
		return
	}
	v.cfg.Hub.Wait(context.Background(), []string{job}, 0)
	v.settleJob(id, job, turn)
}

// settleJob moves a settled turn's outcome into the descriptor. turn guards
// against a late watcher settling a newer turn of the same job (revives
// reuse the job id).
func (v *VibeScope) settleJob(id, job string, turn int) {
	res, ok := v.cfg.Hub.Result(job)
	if !ok || res == nil {
		return // not settled yet
	}
	status, text, sess := VibeDone, res.Text, res.SessionID
	if res.Status == "failed" || res.Status == "schema-mismatch" {
		status = VibeFailed
	}
	v.mu.Lock()
	w := v.findLocked(id)
	if w == nil || w.Job != job || w.Turn != turn || w.Status != VibeRunning {
		v.mu.Unlock()
		return
	}
	w.Status, w.Output = status, text
	if sess != "" {
		w.Session = sess
	}
	cp := *w
	v.mu.Unlock()
	v.persist(VibeWorkerEntry, cp.data())
	if v.cfg.OnSettle != nil {
		v.cfg.OnSettle(cp)
	}
}

// uniqueID derives a worker id from the requested name (sanitized and
// capped, per omp's 48-character name cap) or the next counter; the result
// never collides with a live or rehydrated worker.
func (v *VibeScope) uniqueID(name string) string {
	base := sanitizeName(name)
	v.mu.Lock()
	defer v.mu.Unlock()
	if base == "" {
		for {
			v.seq++
			if id := fmt.Sprintf("vibe-%d", v.seq); v.findLocked(id) == nil {
				return id
			}
		}
	}
	if v.findLocked(base) == nil {
		return base
	}
	for n := 2; ; n++ {
		if id := fmt.Sprintf("%s-%d", base, n); v.findLocked(id) == nil {
			return id
		}
	}
}

const vibeNameCap = 48

// sanitizeName narrows a requested worker name to the id shape the tools
// accept ([A-Za-z0-9_-], capped) so a stray symbol cannot make an id
// unaddressable.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if r := []rune(out); len(r) > vibeNameCap {
		out = string(r[:vibeNameCap])
	}
	return strings.Trim(out, "-")
}

// idSeq extracts the counter from a "vibe-N" id (0 when it has none), so a
// restored registry never re-mints an id it already holds.
func idSeq(id string) int {
	rest, ok := strings.CutPrefix(id, "vibe-")
	if !ok {
		return 0
	}
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// --- worker tools ---

// vibeTool adapts one worker-control op to the tool interface: five names,
// one plumbing shape.
type vibeTool struct {
	scope  *VibeScope
	name   string
	desc   string
	params json.RawMessage
	exec   func(ctx context.Context, v *VibeScope, args json.RawMessage) (tool.Result, error)
}

func (t *vibeTool) Name() string                { return t.name }
func (t *vibeTool) Description() string         { return t.desc }
func (t *vibeTool) Parameters() json.RawMessage { return t.params }

func (t *vibeTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	return t.exec(ctx, t.scope, args)
}

// tools builds the five worker-control tools for this scope.
func (v *VibeScope) tools() []tool.Tool {
	return []tool.Tool{
		&vibeTool{scope: v, name: VibeSpawnToolName, desc: "start a persistent worker subagent on one workstream (cli: fast | good)", params: vibeSpawnParams, exec: vibeSpawnExec},
		&vibeTool{scope: v, name: VibeSendToolName, desc: "message a worker: steer its running turn, or start its next one", params: vibeSendParams, exec: vibeSendExec},
		&vibeTool{scope: v, name: VibeWaitToolName, desc: "block until a worker turn settles and return its final text", params: vibeWaitParams, exec: vibeWaitExec},
		&vibeTool{scope: v, name: VibeKillToolName, desc: "cancel a worker's turn and retire it", params: vibeKillParams, exec: vibeKillExec},
		&vibeTool{scope: v, name: VibeStatusToolName, desc: "list the workers: tier, state, turn, model, latest output", params: vibeStatusParams, exec: vibeStatusExec},
	}
}

var (
	vibeSpawnParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "cli": {"type": "string", "description": "worker tier: fast (sonic/@smol) or good (task/@task)"},
    "prompt": {"type": "string", "description": "self-contained brief: goal, files, constraints, expected result"},
    "name": {"type": "string", "description": "short worker name (optional; an id is generated when omitted)"}
  },
  "required": ["cli", "prompt"]
}`)
	vibeSendParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "session": {"type": "string", "description": "worker id (or unique id prefix)"},
    "message": {"type": "string", "description": "steering message or the next instruction"}
  },
  "required": ["session", "message"]
}`)
	vibeWaitParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "sessions": {"type": "array", "items": {"type": "string"}, "description": "worker ids to watch (default: every running worker)"},
    "timeout": {"type": "number", "description": "seconds to wait (default 30, max 600)"}
  }
}`)
	vibeKillParams = json.RawMessage(`{
  "type": "object",
  "properties": {"session": {"type": "string", "description": "worker id (or unique id prefix)"}},
  "required": ["session"]
}`)
	vibeStatusParams = json.RawMessage(`{"type": "object", "properties": {}}`)
)

func vibeErr(text string) (tool.Result, error) {
	return tool.Result{Text: text, IsError: true}, nil
}

func vibeSpawnExec(_ context.Context, v *VibeScope, args json.RawMessage) (tool.Result, error) {
	var a struct {
		CLI    string `json:"cli"`
		Prompt string `json:"prompt"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return vibeErr("vibe_spawn: malformed arguments: " + err.Error())
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return vibeErr("vibe_spawn: prompt is required")
	}
	tier, ok := VibeTierOf(a.CLI)
	if !ok {
		return vibeErr(`vibe_spawn: cli must be "fast" or "good"`)
	}
	if v.cfg.Hub == nil || v.cfg.Task == nil {
		return vibeErr("vibe_spawn: no subagent hub wired")
	}
	model, err := v.model(tier)
	if err != nil {
		return vibeErr("vibe_spawn: " + err.Error())
	}
	w := VibeWorker{
		ID:      v.uniqueID(a.Name),
		Tier:    tier.CLI,
		Status:  VibeRunning,
		Model:   model.label(),
		Brief:   a.Prompt,
		Turn:    1,
		Created: time.Now().UTC(),
	}
	// A worker outlives the director turn that spawned it, so it owns a
	// background context; vibe_kill and a mode exit are the stop paths.
	job, err := v.cfg.Hub.Start(context.Background(), v.specFor(tier, w, model, a.Prompt))
	if err != nil {
		return vibeErr("vibe_spawn: " + err.Error())
	}
	w.Job = job
	v.add(&w)
	v.persist(VibeWorkerEntry, w.data())
	go v.watch(w.ID, w.Job, w.Turn)
	return tool.Result{Text: fmt.Sprintf("vibe: worker %s (%s, %s) started as job %s — vibe_wait to block on it, vibe_send to steer it", w.ID, w.Tier, w.Model, job)}, nil
}

func vibeSendExec(_ context.Context, v *VibeScope, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Session string `json:"session"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return vibeErr("vibe_send: malformed arguments: " + err.Error())
	}
	msg := strings.TrimSpace(a.Message)
	if strings.TrimSpace(a.Session) == "" || msg == "" {
		return vibeErr("vibe_send: session and message are required")
	}
	if v.cfg.Hub == nil || v.cfg.Task == nil {
		return vibeErr("vibe_send: no subagent hub wired")
	}
	w, ok := v.snapshot(a.Session)
	if !ok {
		return vibeErr("vibe_send: unknown worker " + quoteSel(a.Session) + " (known: " + v.knownIDs() + ")")
	}
	if w.Status == VibeKilled {
		return vibeErr("vibe_send: worker " + w.ID + " was killed — vibe_spawn a new one")
	}

	turn := w.Turn + 1
	if w.Job != "" {
		if err := v.deliver(w.Job, msg); err != nil {
			return vibeErr("vibe_send: " + err.Error())
		}
	} else {
		// Rehydrated worker (or a fresh process): its transcript is gone,
		// so the next turn starts from the last result plus this message —
		// the same continuity the hub's revival path gives a parked job.
		tier := tierOf(w.Tier)
		model, err := v.model(tier)
		if err != nil {
			return vibeErr("vibe_send: " + err.Error())
		}
		prompt := msg
		if strings.TrimSpace(w.Output) != "" {
			prompt = "Previous result from this workstream:\n" + w.Output + "\n\nNext instruction: " + msg
		}
		job, err := v.cfg.Hub.Start(context.Background(), v.specFor(tier, w, model, prompt))
		if err != nil {
			return vibeErr("vibe_send: " + err.Error())
		}
		w.Job = job
	}
	upd, ok := v.setState(w.ID, func(x *VibeWorker) {
		x.Status, x.Turn, x.Job = VibeRunning, turn, w.Job
	})
	if !ok {
		return vibeErr("vibe_send: worker " + w.ID + " vanished")
	}
	v.persist(VibeWorkerEntry, upd.data())
	go v.watch(upd.ID, upd.Job, upd.Turn)
	return tool.Result{Text: fmt.Sprintf("vibe: sent to %s (turn %d)", upd.ID, upd.Turn)}, nil
}

func vibeWaitExec(ctx context.Context, v *VibeScope, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Sessions []string `json:"sessions"`
		Timeout  float64  `json:"timeout"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return vibeErr("vibe_wait: malformed arguments: " + err.Error())
	}
	if v.cfg.Hub == nil {
		return vibeErr("vibe_wait: no subagent hub wired")
	}
	rows := v.Workers() // refreshes first: a settled worker is not watched

	var watched []VibeWorker
	if len(a.Sessions) > 0 {
		for _, sel := range a.Sessions {
			w, ok := v.snapshot(sel)
			if !ok {
				return vibeErr("vibe_wait: unknown worker " + quoteSel(sel) + " (known: " + v.knownIDs() + ")")
			}
			watched = append(watched, w)
		}
	} else {
		watched = rows
	}

	// Nothing to block on: hand back what the watched workers already
	// produced, so a reissued wait (or a wait on a worker that finished
	// while the director was busy) is never empty-handed.
	ids := make([]string, 0, len(watched))
	watchedIDs := make([]string, 0, len(watched))
	for _, w := range watched {
		watchedIDs = append(watchedIDs, w.ID)
		if w.Job != "" && w.Status == VibeRunning {
			ids = append(ids, w.Job)
		}
	}
	if len(ids) == 0 {
		if len(rows) == 0 {
			return vibeErr("vibe_wait: no workers — vibe_spawn one first")
		}
		if out := vibeOutcomes(v.waitRows(watchedIDs)); out != "" {
			return tool.Result{Text: out}, nil
		}
		return tool.Result{Text: "vibe: nothing running yet (vibe_status for the registry)"}, nil
	}

	timeout := time.Duration(a.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > vibeWaitMax {
		timeout = vibeWaitMax
	}
	if settled := v.cfg.Hub.Wait(ctx, ids, timeout); len(settled) == 0 {
		return tool.Result{Text: fmt.Sprintf("vibe: no worker settled within %s — reissue the wait or check vibe_status", timeout)}, nil
	}
	return tool.Result{Text: vibeOutcomes(v.waitRows(watchedIDs))}, nil
}

// waitRows snapshots the watched workers, refreshing job state first.
func (v *VibeScope) waitRows(ids []string) []VibeWorker {
	v.refresh()
	out := make([]VibeWorker, 0, len(ids))
	for _, id := range ids {
		if w, ok := v.snapshot(id); ok {
			out = append(out, w)
		}
	}
	return out
}

// vibeOutcomes renders the settled workers of a set with their bounded
// final text ("" when none of them settled).
func vibeOutcomes(rows []VibeWorker) string {
	var b strings.Builder
	for _, w := range rows {
		if w.Status == VibeRunning {
			continue
		}
		fmt.Fprintf(&b, "worker %s (%s) %s — turn %d\n", w.ID, w.Tier, w.Status, w.Turn)
		if w.Output != "" {
			b.WriteString(VibePreview(w.Output) + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func vibeKillExec(_ context.Context, v *VibeScope, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return vibeErr("vibe_kill: malformed arguments: " + err.Error())
	}
	w, ok := v.snapshot(a.Session)
	if !ok {
		return vibeErr("vibe_kill: unknown worker " + quoteSel(a.Session) + " (known: " + v.knownIDs() + ")")
	}
	if w.Job != "" && v.cfg.Hub != nil {
		v.cfg.Hub.Cancel(w.Job) // no-op once the turn settled
	}
	upd, _ := v.setState(w.ID, func(x *VibeWorker) { x.Status = VibeKilled })
	v.persist(VibeWorkerEntry, upd.data())
	if w.Status == VibeKilled {
		return tool.Result{Text: "vibe: worker " + w.ID + " was already killed"}, nil
	}
	return tool.Result{Text: "vibe: killed worker " + w.ID + " (its child transcript stays inspectable)"}, nil
}

func vibeStatusExec(_ context.Context, v *VibeScope, _ json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: v.Status()}, nil
}

// knownIDs lists the registry for an unknown-worker error.
func (v *VibeScope) knownIDs() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.workers) == 0 {
		return "none"
	}
	out := make([]string, 0, len(v.workers))
	for _, w := range v.workers {
		out = append(out, w.ID)
	}
	return strings.Join(out, ", ")
}

// deliver hands one message to a worker's hub job: a running turn is
// steered at its next step boundary; a settled job is parked and revived,
// which re-runs the worker with its prior output injected as context.
func (v *VibeScope) deliver(job, msg string) error {
	if info, ok := v.cfg.Hub.Status(job); ok && info.Status == "running" {
		if err := v.cfg.Hub.Send(job, msg); err == nil {
			return nil
		}
		// The turn settled between the status read and the send; fall
		// through to the revive path.
	}
	if !v.cfg.Hub.Park(job) {
		return fmt.Errorf("worker job %s cannot take a message (not settled)", job)
	}
	return v.cfg.Hub.Send(job, msg)
}

// model resolves a tier's role through the host resolver.
func (v *VibeScope) model(t VibeTier) (*VibeModel, error) {
	if v.cfg.Resolve == nil {
		return nil, fmt.Errorf("no model resolver wired")
	}
	m, err := v.cfg.Resolve(t.Role)
	if err != nil {
		return nil, err
	}
	if m == nil || m.Provider == nil {
		return nil, fmt.Errorf("tier %s (%s): no provider resolved", t.CLI, t.Role)
	}
	return m, nil
}

// specFor builds a worker's subagent spec from the host's task tool: the
// child inherits the task tool's tool surface, working directory, and
// approval posture (delegation must not sideload a side channel), and gains
// the tier's bundled agent prompt and resolved model.
func (v *VibeScope) specFor(tier VibeTier, w VibeWorker, m *VibeModel, prompt string) SubagentSpec {
	t := v.cfg.Task
	parent := ""
	if v.cfg.ParentID != nil {
		parent = v.cfg.ParentID()
	}
	return SubagentSpec{
		Name:            w.ID,
		Prompt:          prompt,
		System:          vibeAgent(tier).SystemPrompt,
		Provider:        m.Provider,
		Model:           m.Model,
		Tools:           t.ChildTools,
		CWD:             t.CWD,
		DataDir:         t.DataDir,
		MaxTurns:        t.MaxTurns,
		MaxTokens:       t.MaxTokens,
		Output:          &SubagentOutput{},
		ParentSessionID: parent,
		Policy:          t.Policy,
		Approve:         t.Approve,
		Thinking:        m.Thinking,
	}
}

// VibePreview bounds one worker text block and keeps it on the director's
// side of the context budget.
func VibePreview(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output yet)"
	}
	r := []rune(s)
	if len(r) <= vibeOutputCap {
		return s
	}
	return string(r[:vibeOutputCap]) + "\n… (truncated)"
}

// vibeWaitMax caps a single vibe_wait (the director reissues as needed).
const vibeWaitMax = 10 * time.Minute

// quoteSel quotes a selector in an error message.
func quoteSel(s string) string { return fmt.Sprintf("%q", s) }
