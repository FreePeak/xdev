package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// Background sessions (issue #131, slice 1).
//
// A --bg run detaches a print-mode child so the work survives the terminal
// that launched it. The child is the same binary with XDEV_BG_ID set; its
// stdout/stderr land in <dataDir>/bg/<id>.log and a small JSON status file
// tracks pid / exit. There is no supervisor daemon: list/stop/logs read the
// status files and the host process table.
//
// Hang protection is the same ladder a foreground print run already has:
// stream first-progress + idle watchdogs (ai package), --max-turns (default
// 200), and --max-time. --bg defaults --max-time to 2h when the user did not
// set one, so a wedged provider cannot run forever unattended.
//
// ponytail: no daemon, no PTY, no attach TUI. Upgrade path is a long-lived
// supervisor that owns the children and an attach that replays the log +
// steers the live run (#131 residual).

const (
	bgDirName       = "bg"
	bgStatusName    = "status.json"
	bgLogName       = "log"
	bgDefaultMaxAge = 2 * time.Hour
	// bgEnvID is set on the child so it knows which status file to update.
	bgEnvID = "XDEV_BG_ID"
	// bgEnvMarker tags the child argv/environ so `xdev ps` / `xdev bg list`
	// can tell a bg worker from an ordinary print run.
	bgEnvMarker = "XDEV_BG=1"
)

// bgStatus is the on-disk record for one background job.
type bgStatus struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	CWD       string    `json:"cwd"`
	Prompt    string    `json:"prompt,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Started   time.Time `json:"started"`
	// Finished is zero while the job is live.
	Finished time.Time `json:"finished,omitempty"`
	// ExitCode is nil while running; 0 on success, non-zero on failure.
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	LogPath  string `json:"log_path"`
	Command  string `json:"command,omitempty"`
}

func bgRoot() string {
	return filepath.Join(config.DataDir(), bgDirName)
}

func bgJobDir(id string) string {
	return filepath.Join(bgRoot(), id)
}

func bgStatusPath(id string) string {
	return filepath.Join(bgJobDir(id), bgStatusName)
}

func bgLogPath(id string) string {
	return filepath.Join(bgJobDir(id), bgLogName)
}

// mintBgID is a short, filesystem-safe id. Full UUIDs are overkill for a
// directory the user types into `xdev bg logs <id>`.
func mintBgID() string {
	id := session.NewSessionID()
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func writeBgStatus(st bgStatus) error {
	dir := bgJobDir(st.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st.LogPath = bgLogPath(st.ID)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := bgStatusPath(st.ID) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, bgStatusPath(st.ID))
}

func readBgStatus(id string) (bgStatus, error) {
	raw, err := os.ReadFile(bgStatusPath(id))
	if err != nil {
		return bgStatus{}, err
	}
	var st bgStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return bgStatus{}, err
	}
	return st, nil
}

// bgAlive reports whether the recorded PID is still a live process. A
// reaped PID is treated as dead even if the status file still says running.
func bgAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0: existence check, no delivery. On Windows FindProcess always
	// succeeds for any pid, so we still best-effort Signal.
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) {
		return false
	}
	// ESRCH / "process already finished" → dead. EPERM → alive but not ours.
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.ESRCH {
		return false
	}
	// Unknown error: prefer "alive" so stop still has a target rather than
	// silently GC'ing a job we could not probe.
	if runtime.GOOS == "windows" {
		return true
	}
	return !errors.Is(err, syscall.EINVAL)
}

// reconcileBgStatus marks a job finished when its PID is gone but the file
// still claims running. Exit code unknown (-1) — the child writes the real
// code on its own way out; this is only the orphan path.
func reconcileBgStatus(st *bgStatus) {
	if st.ExitCode != nil {
		return
	}
	if bgAlive(st.PID) {
		return
	}
	code := -1
	st.ExitCode = &code
	if st.Finished.IsZero() {
		st.Finished = time.Now().UTC()
	}
	if st.Error == "" {
		st.Error = "process exited without writing a final status"
	}
	_ = writeBgStatus(*st)
}

func listBgStatuses() ([]bgStatus, error) {
	root := bgRoot()
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]bgStatus, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		st, err := readBgStatus(e.Name())
		if err != nil {
			continue
		}
		reconcileBgStatus(&st)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Started.After(out[j].Started)
	})
	return out, nil
}

// resolveBgID accepts a full id or a unique prefix (same shape as --resume).
func resolveBgID(query string) (string, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return "", fmt.Errorf("background job id required")
	}
	all, err := listBgStatuses()
	if err != nil {
		return "", err
	}
	var hits []string
	for _, st := range all {
		id := strings.ToLower(st.ID)
		if id == q || strings.HasPrefix(id, q) {
			hits = append(hits, st.ID)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no background job matching %q", query)
	case 1:
		return hits[0], nil
	default:
		return "", fmt.Errorf("ambiguous id %q matches %s", query, strings.Join(hits, ", "))
	}
}

// spawnBg launches this binary as a detached print-mode worker and returns
// the short job id. The parent returns as soon as the child is started; the
// child owns the run and updates status.json on exit.
//
// argv is the flag set the parent would have used for a foreground print run,
// minus --bg itself. prompt is the user text (may be empty for --continue /
// --resume-only shapes).
func spawnBg(prompt string, argv []string) (string, error) {
	if runtime.GOOS == "windows" {
		// SysProcAttr.Setsid is a Unix path. Windows needs CREATE_NEW_PROCESS_GROUP
		// + a hidden window; leave that for a follow-up rather than ship a half path.
		return "", fmt.Errorf("--bg is not supported on windows yet")
	}
	id := mintBgID()
	if err := os.MkdirAll(bgJobDir(id), 0o700); err != nil {
		return "", err
	}
	logFile, err := os.OpenFile(bgLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	// The child inherits the log fd; the parent only needed it open long
	// enough to pass ExtraFiles-less Stdout/Stderr.
	defer logFile.Close()

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve xdev binary: %w", err)
	}
	// Rebuild argv: strip --bg/-bg, force print mode, pass the prompt.
	childArgs := stripBgFlag(argv)
	// os.Args already carries the prompt as a trailing positional; drop a
	// trailing copy so we do not pass it twice when we append below.
	if prompt != "" && len(childArgs) > 0 && childArgs[len(childArgs)-1] == prompt {
		childArgs = childArgs[:len(childArgs)-1]
	}
	// Drop a leading "print" subcommand — we force --print ourselves.
	if len(childArgs) > 0 && childArgs[0] == "print" {
		childArgs = childArgs[1:]
	}
	childArgs = append([]string{"--print"}, childArgs...)
	if prompt != "" {
		childArgs = append(childArgs, prompt)
	}

	cmd := exec.Command(self, childArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Dir, _ = os.Getwd()
	cmd.Env = append(os.Environ(),
		bgEnvMarker,
		bgEnvID+"="+id,
		// A detached worker must not inherit a TTY sense of "interactive".
		"TERM=dumb",
	)
	cmd.SysProcAttr = bgSysProcAttr()

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start background worker: %w", err)
	}
	st := bgStatus{
		ID:      id,
		PID:     cmd.Process.Pid,
		CWD:     cmd.Dir,
		Prompt:  trimPrompt(prompt),
		Started: time.Now().UTC(),
		LogPath: bgLogPath(id),
		Command: strings.Join(append([]string{filepath.Base(self)}, childArgs...), " "),
	}
	if err := writeBgStatus(st); err != nil {
		// Best-effort kill: we could not record the job, so leave nothing orphaned.
		_ = cmd.Process.Kill()
		return "", err
	}
	// Reap in the background so the child does not become a zombie if the
	// parent outlives it. The child's own defer writes the final status.
	go func() { _, _ = cmd.Process.Wait() }()
	return id, nil
}

func trimPrompt(p string) string {
	p = strings.TrimSpace(p)
	if len(p) > 200 {
		return p[:200] + "…"
	}
	return p
}

// stripBgFlag drops --bg / -bg and their optional =value form from argv so
// the child does not recurse.
func stripBgFlag(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--bg" || a == "-bg":
			continue
		case strings.HasPrefix(a, "--bg=") || strings.HasPrefix(a, "-bg="):
			continue
		default:
			out = append(out, a)
		}
	}
	return out
}

// bgSysProcAttr puts the child in a new session so closing the parent's
// terminal (SIGHUP to the foreground process group) does not kill it.
func bgSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// finalizeBgStatus is called by the child on the way out.
func finalizeBgStatus(code int, runErr error) {
	id := os.Getenv(bgEnvID)
	if id == "" {
		return
	}
	st, err := readBgStatus(id)
	if err != nil {
		st = bgStatus{ID: id, PID: os.Getpid(), Started: time.Now().UTC()}
	}
	st.ExitCode = &code
	st.Finished = time.Now().UTC()
	if runErr != nil {
		st.Error = runErr.Error()
	}
	// Prefer the live session id when the run created one.
	if st.SessionID == "" {
		if p := latestSessionHint(); p != "" {
			st.SessionID = p
		}
	}
	_ = writeBgStatus(st)
}

// latestSessionHint is a best-effort breadcrumb: the child may have written
// a session file; surface its id in the status so `xdev bg list` can point
// the user at `xdev --resume`. Empty when nothing landed.
func latestSessionHint() string {
	// The breadcrumb is pane-keyed and this child has no TTY pane; skip.
	return ""
}

// isBgWorker is true inside a --bg child process.
func isBgWorker() bool {
	return os.Getenv(bgEnvID) != "" || os.Getenv("XDEV_BG") == "1"
}

// stopBg sends SIGTERM, then SIGKILL after a grace period.
func stopBg(id string) error {
	st, err := readBgStatus(id)
	if err != nil {
		return err
	}
	reconcileBgStatus(&st)
	if st.ExitCode != nil {
		return fmt.Errorf("job %s already finished", id)
	}
	p, err := os.FindProcess(st.PID)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal %d: %w", st.PID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !bgAlive(st.PID) {
			code := -15
			st.ExitCode = &code
			st.Finished = time.Now().UTC()
			st.Error = "stopped"
			return writeBgStatus(st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = p.Kill()
	code := -9
	st.ExitCode = &code
	st.Finished = time.Now().UTC()
	st.Error = "killed"
	return writeBgStatus(st)
}

func rmBg(id string) error {
	st, err := readBgStatus(id)
	if err != nil {
		return err
	}
	reconcileBgStatus(&st)
	if st.ExitCode == nil && bgAlive(st.PID) {
		return fmt.Errorf("job %s is still running — stop it first", id)
	}
	return os.RemoveAll(bgJobDir(id))
}

func bgStateLabel(st bgStatus) string {
	if st.ExitCode == nil {
		if bgAlive(st.PID) {
			return "running"
		}
		return "lost"
	}
	if *st.ExitCode == 0 {
		return "done"
	}
	if st.Error == "stopped" || *st.ExitCode == -15 {
		return "stopped"
	}
	if st.Error == "killed" || *st.ExitCode == -9 {
		return "killed"
	}
	return "failed"
}

func renderBgList(out io.Writer, rows []bgStatus) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "no background jobs")
		return
	}
	fmt.Fprintf(out, "%-10s %-8s %7s  %-19s  %s\n", "ID", "STATE", "PID", "STARTED", "PROMPT")
	for _, st := range rows {
		prompt := st.Prompt
		if prompt == "" {
			prompt = "—"
		}
		if len(prompt) > 48 {
			prompt = prompt[:45] + "…"
		}
		fmt.Fprintf(out, "%-10s %-8s %7d  %-19s  %s\n",
			st.ID, bgStateLabel(st), st.PID,
			st.Started.Local().Format("2006-01-02 15:04:05"),
			prompt)
	}
}

func tailFile(path string, n int) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if n <= 0 {
		return string(raw), nil
	}
	lines := strings.Split(string(raw), "\n")
	// Drop trailing empty from final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// runBg is the `xdev bg` subcommand: list | logs | stop | rm.
func runBg(args []string) int {
	out, errOut := os.Stdout, os.Stderr
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		rows, err := listBgStatuses()
		if err != nil {
			fmt.Fprintln(errOut, "xdev bg:", err)
			return 1
		}
		asJSON := false
		for _, a := range args[1:] {
			if a == "--json" {
				asJSON = true
			}
		}
		if asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rows)
			return 0
		}
		renderBgList(out, rows)
		return 0
	case "logs", "log":
		if len(args) < 2 {
			fmt.Fprintln(errOut, "usage: xdev bg logs <id> [--tail N]")
			return 2
		}
		id, err := resolveBgID(args[1])
		if err != nil {
			fmt.Fprintln(errOut, "xdev bg:", err)
			return 1
		}
		tailN := 80
		for i := 2; i < len(args); i++ {
			if args[i] == "--tail" && i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n >= 0 {
					tailN = n
				}
				i++
			} else if args[i] == "--all" {
				tailN = 0
			}
		}
		text, err := tailFile(bgLogPath(id), tailN)
		if err != nil {
			fmt.Fprintln(errOut, "xdev bg logs:", err)
			return 1
		}
		if text != "" {
			fmt.Fprintln(out, text)
		}
		return 0
	case "stop":
		if len(args) < 2 {
			fmt.Fprintln(errOut, "usage: xdev bg stop <id>")
			return 2
		}
		id, err := resolveBgID(args[1])
		if err != nil {
			fmt.Fprintln(errOut, "xdev bg:", err)
			return 1
		}
		if err := stopBg(id); err != nil {
			fmt.Fprintln(errOut, "xdev bg stop:", err)
			return 1
		}
		fmt.Fprintf(out, "stopped %s\n", id)
		return 0
	case "rm", "remove":
		if len(args) < 2 {
			fmt.Fprintln(errOut, "usage: xdev bg rm <id>")
			return 2
		}
		id, err := resolveBgID(args[1])
		if err != nil {
			fmt.Fprintln(errOut, "xdev bg:", err)
			return 1
		}
		if err := rmBg(id); err != nil {
			fmt.Fprintln(errOut, "xdev bg rm:", err)
			return 1
		}
		fmt.Fprintf(out, "removed %s\n", id)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(out, `usage: xdev bg <list|logs|stop|rm> [id]

  xdev --bg "prompt"     start a detached print run; prints the job id
  xdev bg list           background jobs (status under ~/.xdev/agent/bg/)
  xdev bg logs <id>      tail the job's log (--tail N, --all)
  xdev bg stop <id>      SIGTERM, then SIGKILL after 5s
  xdev bg rm <id>        delete a finished job's status + log

A --bg run defaults --max-time to 2h when unset, so a wedged provider cannot
run forever. Stream watchdogs and --max-turns still apply.
`)
		return 0
	default:
		fmt.Fprintf(errOut, "xdev bg: unknown subcommand %q (list|logs|stop|rm)\n", args[0])
		return 2
	}
}

// lastDetachID holds the bg job id created by a detach-on-quit, so the exit
// banner can name it. Process-local: one TUI process, one possible detach.
var (
	lastDetachMu sync.Mutex
	lastDetach   string
)

func setLastDetachID(id string) {
	lastDetachMu.Lock()
	lastDetach = id
	lastDetachMu.Unlock()
}

func lastDetachID() string {
	lastDetachMu.Lock()
	defer lastDetachMu.Unlock()
	return lastDetach
}
