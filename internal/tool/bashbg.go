package tool

// Background bash jobs (#16, control surface #127): run_in_background=true
// starts a detached process group whose combined output streams to a temp file,
// and a foreground run that hits its timeout is handed to the same registry with
// an explicit notice instead of being killed.
//
// The registry is the single control plane for those jobs: it holds the process
// group id so a job can be stopped, and the stdin pipe so a REPL or a `y/N`
// prompt can be answered — both from a *later* turn, by job id, through the
// bash tool's `action` verbs. Before #127 the job was fire-and-forget: /tasks
// could list it, `read` on its output file could follow it, and nothing could
// talk to it or end it.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// bashJobsCap bounds the in-memory job registry. Eviction prefers finished
	// jobs (see add), so the entries that give up their slot first are the ones
	// whose process is already gone.
	bashJobsCap = 32
	// bashJobTailLines / bashJobTailBytes bound the per-job tail /tasks and
	// the logs verb read from the output file.
	bashJobTailLines = 3
	bashJobTailBytes = 4096
	// bashJobLogBytes / bashJobLogLines bound one logs response: enough to
	// read a server's startup or a test's summary without flooding context.
	bashJobLogBytes = 64 << 10
	bashJobLogLines = 400
)

// ErrUnknownJob is the seam error every job verb returns for an id the
// registry does not hold — either never issued, or evicted. Callers must
// surface it, never treat it as success: a stopped job the model believes is
// still running is a lie about side effects.
var ErrUnknownJob = errors.New("no such background job")

// BashJob is one backgrounded command.
type BashJob struct {
	ID      int64
	Command string
	Workdir string
	OutPath string
	Started time.Time

	mu       sync.Mutex
	done     bool
	exitCode int
	signaled bool
	pid      int
	stdin    io.WriteCloser // nil when the job has no writable input (see below)
	finished chan struct{}  // closed by finish, so Stop can wait for the reaper
	evicted  bool           // a running job whose handle was dropped at capacity
}

// finish records the reaped exit status.
func (j *BashJob) finish(exitCode int, signaled bool) {
	j.mu.Lock()
	if j.done {
		j.mu.Unlock()
		return
	}
	j.done, j.exitCode, j.signaled = true, exitCode, signaled
	if j.stdin != nil {
		_ = j.stdin.Close()
		j.stdin = nil
	}
	close(j.finished)
	j.mu.Unlock()
}

// Done reports whether the process has been reaped.
func (j *BashJob) Done() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

// ExitCode reports the exit code (or signal number) once the job finished.
func (j *BashJob) ExitCode() (int, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exitCode, j.done
}

// State renders "running", "exit N", or "signal N".
func (j *BashJob) State() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stateLocked()
}

func (j *BashJob) stateLocked() string {
	switch {
	case !j.done:
		return "running"
	case j.signaled:
		return fmt.Sprintf("signal %d", j.exitCode)
	default:
		return fmt.Sprintf("exit %d", j.exitCode)
	}
}

// PID is the child's process id, which is also the process-group id the job was
// started with (Setpgid), so it is what a stop signals.
func (j *BashJob) PID() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.pid
}

// CanWriteInput reports whether the job still has a stdin pipe. A job handed to
// the registry by a foreground timeout does not: its stdin was the pipeline's,
// and was closed at handoff.
func (j *BashJob) CanWriteInput() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stdin != nil && !j.done
}

// Tail returns up to n trailing output lines (n <= 0 → the last
// bashJobTailBytes worth). Best effort: an unreadable file yields "".
func (j *BashJob) Tail(n int) string {
	return j.tailBytes(n, bashJobTailBytes)
}

// tailBytes is Tail with the read window widened, which is what the logs verb
// uses to return a real chunk of output rather than three lines.
func (j *BashJob) tailBytes(n int, window int64) string {
	f, err := os.Open(j.OutPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	size := st.Size()
	read := window
	if size < read {
		read = size
	}
	buf := make([]byte, read)
	if read > 0 {
		if _, err := f.ReadAt(buf, size-read); err != nil && err != io.EOF {
			return ""
		}
	}
	s := strings.TrimRight(string(buf), "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// BashJobs is a bounded registry of background jobs.
type BashJobs struct {
	mu    sync.Mutex
	jobs  map[int64]*BashJob
	order []int64
	seq   int64
}

// NewBashJobs returns an empty registry (tests construct their own so jobs
// never bleed across cases).
func NewBashJobs() *BashJobs { return &BashJobs{jobs: map[int64]*BashJob{}} }

var (
	sharedBashJobsOnce sync.Once
	sharedBashJobs     *BashJobs
)

// SharedBashJobs returns the process-wide job registry: the bash tool
// records into it and the TUI's /tasks lists it (one truth).
func SharedBashJobs() *BashJobs {
	sharedBashJobsOnce.Do(func() { sharedBashJobs = NewBashJobs() })
	return sharedBashJobs
}

// add registers a job at the cap by dropping the oldest one whose process is
// already gone, so finished jobs stay addressable as results while a running
// job keeps the handle that can stop it. Only when every tracked job is still
// running does the oldest give up its slot, and the returned evicted job says so
// (the caller reports it instead of losing a live handle in silence).
func (b *BashJobs) add(command, workdir, outPath string, pid int, stdin io.WriteCloser) (job, evicted *BashJob) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	job = &BashJob{
		ID: b.seq, Command: command, Workdir: workdir, OutPath: outPath,
		Started: time.Now(), pid: pid, stdin: stdin, finished: make(chan struct{}),
	}
	if len(b.order) >= bashJobsCap {
		evicted = b.evictLocked()
	}
	b.jobs[job.ID] = job
	b.order = append(b.order, job.ID)
	return job, evicted
}

// evictLocked drops one entry, preferring a finished job; returns what it
// dropped (a running job is marked evicted so the caller can say the handle was
// lost while the process keeps running).
func (b *BashJobs) evictLocked() *BashJob {
	drop := -1
	for i, id := range b.order {
		if j, ok := b.jobs[id]; ok && j.Done() {
			drop = i
			break
		}
	}
	if drop < 0 {
		drop = 0 // every job is running: the oldest handle goes, as before
	}
	if drop >= len(b.order) {
		return nil
	}
	id := b.order[drop]
	b.order = append(b.order[:drop], b.order[drop+1:]...)
	j, ok := b.jobs[id]
	if !ok {
		return nil
	}
	delete(b.jobs, id)
	if !j.Done() {
		j.mu.Lock()
		j.evicted = true
		j.mu.Unlock()
	}
	return j
}

// Get returns the job with id.
func (b *BashJobs) Get(id int64) (*BashJob, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	return j, ok
}

// job resolves an id or reports ErrUnknownJob wrapped with it, so every verb
// fails the same way at the seam.
func (b *BashJobs) job(id int64) (*BashJob, error) {
	j, ok := b.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %d (the registry holds %d job(s); ids are not reused)", ErrUnknownJob, id, b.Len())
	}
	return j, nil
}

// Stop terminates a job's process group the way a cancelled foreground run is
// terminated: SIGTERM to the group, then SIGKILL if it is still there after
// KillGrace. It waits for the reaper so the state it reports is final — a job
// reported as stopped while still running would be the same lie as a cancelled
// tool that kept executing (#126).
func (b *BashJobs) Stop(id int64) (*BashJob, error) {
	j, err := b.job(id)
	if err != nil {
		return nil, err
	}
	if j.Done() {
		return j, nil // already reaped: stopping it is not an error
	}
	pgid := j.PID()
	killProcessGroup(pgid, signalTerm)
	select {
	case <-j.finished:
	case <-time.After(KillGrace):
		killProcessGroup(pgid, signalKill)
		<-j.finished
	}
	return j, nil
}

// WriteInput sends exact bytes to a job's stdin: enough for a REPL, a build
// watching for a keypress, or the `y/N` prompt a background script stops at.
// It never interprets them — the caller spells `\n` — because a job waiting on
// a TTY-style input is exactly where silently rewriting bytes loses.
func (b *BashJobs) WriteInput(id int64, s string) (*BashJob, error) {
	j, err := b.job(id)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	w := j.stdin
	done := j.done
	j.mu.Unlock()
	if done {
		return j, fmt.Errorf("background job %d already finished (%s); its input is closed", id, j.stateFromLock())
	}
	if w == nil {
		return j, fmt.Errorf("background job %d has no writable input: it was handed to the registry by a foreground timeout, whose stdin was the pipeline's", id)
	}
	if _, err := io.WriteString(w, s); err != nil {
		return j, fmt.Errorf("background job %d: write: %w", id, err)
	}
	return j, nil
}

// stateFromLock renders State without taking j.mu again (the caller holds it).
func (j *BashJob) stateFromLock() string { return j.stateLocked() }

// Logs returns the job's output tail within the logs verb's bounds.
func (b *BashJobs) Logs(id int64, lines int) (string, *BashJob, error) {
	j, err := b.job(id)
	if err != nil {
		return "", nil, err
	}
	if lines <= 0 || lines > bashJobLogLines {
		lines = bashJobLogLines
	}
	return j.tailBytes(lines, bashJobLogBytes), j, nil
}

// List returns every tracked job, oldest first — finished ones included, since
// a result the model can no longer address is a lost side effect.
func (b *BashJobs) List() []*BashJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*BashJob, 0, len(b.order))
	for _, id := range b.order {
		if j, ok := b.jobs[id]; ok {
			out = append(out, j)
		}
	}
	return out
}

// Len reports how many jobs the registry tracks.
func (b *BashJobs) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.order)
}

// Render is the /tasks body: one block per job with state, command, output
// path, and the newest output lines.
func (b *BashJobs) Render() string {
	jobs := b.List()
	if len(jobs) == 0 {
		return "No background bash jobs."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Background bash jobs (%d):\n", len(jobs))
	for _, j := range jobs {
		fmt.Fprintf(&sb, "#%d %s pid %d (%s) — %s\n", j.ID, j.State(), j.PID(), time.Since(j.Started).Round(time.Second), j.Command)
		fmt.Fprintf(&sb, "    output: %s\n", j.OutPath)
		for _, line := range strings.Split(j.Tail(bashJobTailLines), "\n") {
			if line != "" {
				sb.WriteString("    | " + line + "\n")
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
