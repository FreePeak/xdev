package tui

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The watchdog exists because a stuck UI loop is invisible after the fact:
// handleKey and draw run on the loop's goroutine (app.go's Run), so a
// non-returning command or frame kills keys, Ctrl+C and output together while
// the process stays alive — which is exactly how a user described it. Drive
// that shape and require a dump that names the blocker.
func TestStallWatchdogWritesDumpWhenLoopHangs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.beat()

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	// Tight thresholds so the stall is observed in a fraction of a second.
	go app.watchStall(100*time.Millisecond, 20*time.Millisecond, time.Now, stop)

	// The hang itself, and the hard case: the loop is stuck WHILE HOLDING
	// App.mu, which is the situation a mutex-based watchdog could never
	// report. Reading only the atomic beat is what keeps it alive.
	app.mu.Lock()
	time.Sleep(600 * time.Millisecond)

	entries, err := os.ReadDir(dir)
	if err != nil {
		app.mu.Unlock()
		t.Fatalf("dumps dir not created by the watchdog: %v", err)
	}
	if len(entries) != 1 {
		app.mu.Unlock()
		t.Fatalf("stall dumps = %d, want exactly 1 per episode (%v)", len(entries), entries)
	}
	name := entries[0].Name()
	data, err := os.ReadFile(filepath.Join(dir, name))
	info, statErr := os.Stat(filepath.Join(dir, name))
	app.mu.Unlock()

	if err != nil {
		t.Fatal(err)
	}
	if statErr == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("stall dump mode = %v, want 0600 (a stack dump carries paths and content the user did not choose to share)", info.Mode().Perm())
	}
	if !strings.HasPrefix(name, "tui-stall-") || !strings.HasSuffix(name, ".txt") {
		t.Fatalf("unexpected dump name %q", name)
	}
	txt := string(data)
	if !strings.Contains(txt, "xdev UI loop stall") {
		t.Fatalf("dump lacks its header:\n%.200s", txt)
	}
	// The point of the file: it must name the goroutine that hung.
	if !strings.Contains(txt, "TestStallWatchdogWritesDumpWhenLoopHangs") {
		t.Fatalf("dump does not contain the stalled test goroutine:\n%.400s", txt)
	}
	if !strings.Contains(txt, "goroutine") {
		t.Fatalf("dump has no stacks:\n%.200s", txt)
	}
}

// One episode, one file: a loop that never recovers must not fill the dumps
// dir before the user can read the first one.
func TestStallWatchdogDumpsOncePerEpisode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.beat()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(50*time.Millisecond, 20*time.Millisecond, time.Now, stop)

	app.mu.Lock()
	time.Sleep(900 * time.Millisecond) // ~40 overdue polls
	app.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("stall dumps = %d, want 1 for a single episode: %v", len(entries), names(entries))
	}
}

// A healthy loop must never produce a file, and the heartbeat must reset the
// episode so a later hang is still reported.
func TestStallWatchdogSilentWhenLoopHealthy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.beat()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(200*time.Millisecond, 20*time.Millisecond, time.Now, stop)

	for range 40 {
		app.beat()
		time.Sleep(10 * time.Millisecond)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) != 0 {
		t.Fatalf("healthy loop produced stall dumps: %v", names(entries))
	}

	// Recovery arms it again: stop beating and a file must appear.
	app.mu.Lock()
	time.Sleep(600 * time.Millisecond)
	entries, err := os.ReadDir(dir)
	app.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("post-recovery stall produced %d dumps, want 1", len(entries))
	}
}

// A machine that was asleep is not a loop that hung. The 2026-09-28 dumps made
// the point expensively: of 165 written that day, 16 were silence a `pmset -g
// log` sleep interval accounted for exactly, reported as stalls of 5s to
// 17m43s while goroutine 1 sat in the same idle select it always sits in. The
// watchdog can tell a sleep from a hang only by the clock: it polls once a
// second, and a sleep is one poll that is suddenly fifteen minutes wide.
func TestStallWatchdogIgnoresSilenceThatIsOnlyASleep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.beat()

	// A clock the test can suspend, exactly as the machine's is: the wall
	// reading jumps a long way while the beat stands still.
	clock := &jumpClock{now: time.Now()}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(50*time.Millisecond, 20*time.Millisecond, clock.Now, stop)

	// Poll normally: healthy, so nothing is written.
	time.Sleep(80 * time.Millisecond)
	// The lid closes. The next poll sees fifteen minutes of wall clock go by in
	// one step, and the loop beat is to the watchdog fifteen minutes old.
	clock.jump(15 * time.Minute)
	time.Sleep(200 * time.Millisecond)

	if entries, err := readDirNames(dir); err == nil && len(entries) != 0 {
		t.Fatalf("a suspended machine produced stall dumps: %v", entries)
	}

	// The loop beats again once awake. Nothing stale is left to condemn.
	app.beat()
	time.Sleep(200 * time.Millisecond)
	if entries, err := readDirNames(dir); err == nil && len(entries) != 0 {
		t.Fatalf("a loop that beat after waking produced stall dumps: %v", entries)
	}
}

// The grace is a pause, not a pardon: a loop still stuck once the allowance
// runs out is a real stall and must still be reported, asleep or not. Otherwise
// the fix is a way to lose the report this whole mechanism exists for.
func TestStallWatchdogStillReportsAStallAfterASleep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.beat()

	clock := &jumpClock{now: time.Now()}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(50*time.Millisecond, 20*time.Millisecond, clock.Now, stop)

	// The loop is stuck: no beat after this one, through the sleep and past it.
	clock.jump(15 * time.Minute)
	// The grace is two stall windows; let several of them elapse.
	for range 12 {
		clock.jump(50 * time.Millisecond)
		time.Sleep(25 * time.Millisecond)
	}

	entries, err := readDirNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a loop stuck across a sleep produced %d dumps, want 1: %v", len(entries), entries)
	}
}

// jumpClock is a wall clock the test can suspend, standing in for a machine
// that stopped: every jump is one "asleep, and now awake".
type jumpClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *jumpClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *jumpClock) jump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	return names(entries), nil
}

// No dump dir means no watchdog and no surprise files.
func TestStallWatchdogDisabledWithoutDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir("")
	app.beat()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(20*time.Millisecond, 10*time.Millisecond, time.Now, stop)
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("disabled watchdog created a dumps dir")
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
