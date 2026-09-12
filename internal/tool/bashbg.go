package tool

// Background bash jobs (issue #16): run_in_background=true starts a detached
// process group whose combined output streams to a temp file, and a
// foreground run that hits its timeout is handed to the same registry with an
// explicit notice instead of being killed.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// bashJobsCap bounds the in-memory job registry; the oldest job is
	// evicted first (an evicted running job keeps running, just unlisted).
	bashJobsCap = 32
	// bashJobTailLines / bashJobTailBytes bound the per-job tail /tasks and
	// accessors read from the output file.
	bashJobTailLines = 3
	bashJobTailBytes = 4096
)

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
}

// finish records the reaped exit status.
func (j *BashJob) finish(exitCode int, signaled bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done, j.exitCode, j.signaled = true, exitCode, signaled
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
	switch {
	case !j.done:
		return "running"
	case j.signaled:
		return fmt.Sprintf("signal %d", j.exitCode)
	default:
		return fmt.Sprintf("exit %d", j.exitCode)
	}
}

// Tail returns up to n trailing output lines (n <= 0 → the last
// bashJobTailBytes worth). Best effort: an unreadable file yields "".
func (j *BashJob) Tail(n int) string {
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
	read := int64(bashJobTailBytes)
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

// BashJobs is a bounded, FIFO-evicting registry of background jobs.
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

// add registers a job, evicting the oldest entry once the cap is reached.
func (b *BashJobs) add(command, workdir, outPath string) *BashJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	job := &BashJob{ID: b.seq, Command: command, Workdir: workdir, OutPath: outPath, Started: time.Now()}
	if len(b.order) >= bashJobsCap {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.jobs, oldest)
	}
	b.jobs[job.ID] = job
	b.order = append(b.order, job.ID)
	return job
}

// Get returns the job with id.
func (b *BashJobs) Get(id int64) (*BashJob, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	return j, ok
}

// List returns the live jobs, oldest first.
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
		fmt.Fprintf(&sb, "#%d %s (%s) — %s\n", j.ID, j.State(), time.Since(j.Started).Round(time.Second), j.Command)
		fmt.Fprintf(&sb, "    output: %s\n", j.OutPath)
		for _, line := range strings.Split(j.Tail(bashJobTailLines), "\n") {
			if line != "" {
				sb.WriteString("    | " + line + "\n")
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
