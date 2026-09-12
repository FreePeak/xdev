package eval

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// jobHistory bounds how many backgrounded cells are remembered.
const jobHistory = 32

// Job is a cell that outlived the tool call's background threshold. The cell
// keeps running on the kernel; the handle is reported to the model on the
// next eval call and rendered for /tasks.
type Job struct {
	ID      int64
	Code    string
	Started time.Time

	mu       sync.Mutex
	outcome  Outcome
	finished bool
	reported bool
}

// Finish records the job's outcome. Called exactly once by the tool.
func (j *Job) Finish(out Outcome) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.outcome = out
	j.finished = true
}

// Done reports whether the cell finished.
func (j *Job) Done() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished
}

// State is "running" while the cell executes, then "ok" | "error" | "exited".
func (j *Job) State() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.finished {
		return "running"
	}
	if j.outcome.TimedOut {
		return "timeout"
	}
	return j.outcome.Status
}

// Outcome returns the finished cell's outcome.
func (j *Job) Outcome() (Outcome, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.outcome, j.finished
}

// Text is the finished cell's output (bounded by the kernel's sinks).
func (j *Job) Text() string {
	out, ok := j.Outcome()
	if !ok {
		return ""
	}
	return out.Text
}

// Header is the one-line description of the job.
func (j *Job) Header() string {
	line := fmt.Sprintf("job #%d", j.ID)
	if !j.Done() {
		return line + " running " + time.Since(j.Started).Round(time.Second).String()
	}
	out, _ := j.Outcome()
	return fmt.Sprintf("%s %s %s", line, j.State(), out.Duration.Round(time.Millisecond))
}

// Jobs is the bounded table of backgrounded eval cells. The kernel runs one
// cell at a time, so at most one job is running.
type Jobs struct {
	mu    sync.Mutex
	jobs  map[int64]*Job
	order []int64
	next  int64
}

// NewJobs returns an empty job table.
func NewJobs() *Jobs {
	return &Jobs{jobs: make(map[int64]*Job)}
}

// Add registers a backgrounded cell.
func (js *Jobs) Add(code string) *Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.next++
	job := &Job{ID: js.next, Code: code, Started: time.Now()}
	js.jobs[job.ID] = job
	js.order = append(js.order, job.ID)
	// ponytail: linear scan over a 32-entry table on every Add; switch to a
	// ring buffer if the history ever grows. Running jobs are never evicted.
	for len(js.order) > jobHistory {
		idx := -1
		for i, id := range js.order {
			if old := js.jobs[id]; old != nil && old.Done() {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		delete(js.jobs, js.order[idx])
		js.order = append(js.order[:idx], js.order[idx+1:]...)
	}
	return job
}

// Get looks a job up by id.
func (js *Jobs) Get(id int64) (*Job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	job, ok := js.jobs[id]
	return job, ok
}

// Len is the number of remembered jobs.
func (js *Jobs) Len() int {
	js.mu.Lock()
	defer js.mu.Unlock()
	return len(js.order)
}

// List returns the remembered jobs, oldest first.
func (js *Jobs) List() []*Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := make([]*Job, 0, len(js.order))
	for _, id := range js.order {
		if job := js.jobs[id]; job != nil {
			out = append(out, job)
		}
	}
	return out
}

// Running returns the ids of jobs whose cell is still executing.
func (js *Jobs) Running() []int64 {
	var ids []int64
	for _, job := range js.List() {
		if !job.Done() {
			ids = append(ids, job.ID)
		}
	}
	return ids
}

// DrainFinished returns the jobs that finished since the last drain, each at
// most once.
func (js *Jobs) DrainFinished() []*Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	var out []*Job
	for _, id := range js.order {
		job := js.jobs[id]
		if job == nil || job.reported {
			continue
		}
		job.mu.Lock()
		finished := job.finished
		if finished {
			job.reported = true
		}
		job.mu.Unlock()
		if finished {
			out = append(out, job)
		}
	}
	return out
}

// Render lists the jobs for the /tasks view.
func (js *Jobs) Render() string {
	jobs := js.List()
	if len(jobs) == 0 {
		return "no eval jobs\n"
	}
	var b strings.Builder
	for _, job := range jobs {
		code := strings.SplitN(strings.TrimSpace(job.Code), "\n", 2)[0]
		if len(code) > 60 {
			code = code[:60] + "…"
		}
		fmt.Fprintf(&b, "%s  %s\n", job.Header(), code)
	}
	return b.String()
}
