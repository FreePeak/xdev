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
// log` sleep interval accounted for exactly, reported as stalls of 5s to 17m43s
// while goroutine 1 sat in the same idle select it always sits in. The old
// detector looked for a WIDE POLL — a wall-clock gap over pollSkip — which only
// catches the long sleeps and never the short one that dominates the record.
//
// The verdict is now reached on the awake clock, so the test suspends exactly
// the clock the machine suspends.
func TestStallWatchdogIgnoresSilenceThatIsOnlyASleep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	clock := newSleepClock(t)
	app.beat()

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(50*time.Millisecond, 20*time.Millisecond, clock.Now, stop)

	// Poll normally: healthy, so nothing is written. The loop keeps beating,
	// as a live one does every 25ms.
	clock.advance(80 * time.Millisecond)
	app.beat()
	time.Sleep(80 * time.Millisecond)
	// The lid closes. The wall clock jumps fifteen minutes; the awake clock,
	// which is the one the loop is judged on, does not move at all.
	clock.suspend(15 * time.Minute)
	time.Sleep(200 * time.Millisecond)

	if entries, err := readDirNames(dir); err == nil && len(entries) != 0 {
		t.Fatalf("a suspended machine produced stall dumps: %v", entries)
	}

	// The loop beats again once awake. Nothing stale is left to condemn.
	clock.advance(time.Millisecond)
	app.beat()
	time.Sleep(200 * time.Millisecond)
	if entries, err := readDirNames(dir); err == nil && len(entries) != 0 {
		t.Fatalf("a loop that beat after waking produced stall dumps: %v", entries)
	}
}

// The 25-second case, which is the one that actually filled the dumps dir: 112
// of 525 files have their last beat exactly 25s old, because that is how long
// the machine had been suspended when the wake landed. It is below pollSkip, so
// the wide-poll grace never armed and every one of them was written.
func TestStallWatchdogIgnoresAShortSleepBelowThePollSkipBound(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	clock := newSleepClock(t)
	app.beat()

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(5*time.Second, 20*time.Millisecond, clock.Now, stop)

	// A loop that is running beats on its 25ms tick; model that, or the test
	// is a two-second stall rather than a suspension.
	clock.advance(100 * time.Millisecond)
	app.beat()
	time.Sleep(100 * time.Millisecond)
	// One 25s suspension: the wall clock moves, the loop did not run for any
	// of it. On the old code this is a 25s stall and gets a file.
	clock.suspend(25 * time.Second)
	time.Sleep(200 * time.Millisecond)

	if entries, err := readDirNames(dir); err == nil && len(entries) != 0 {
		t.Fatalf("a 25s suspension produced stall dumps: %v", entries)
	}
}

// The other half, and the reason the grace is not a pardon: a loop that is
// still stuck once the machine is awake again is a real stall and is reported.
// A suspension withholds only the time it took.
func TestStallWatchdogStillReportsAStallAfterASleep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	clock := newSleepClock(t)
	app.beat()

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(50*time.Millisecond, 20*time.Millisecond, clock.Now, stop)

	// The loop is stuck: no beat after this one, through the sleep and past it.
	clock.suspend(15 * time.Minute)
	for range 12 {
		clock.advance(50 * time.Millisecond)
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

// sleepClock is the pair of clocks a suspended Mac really has: a wall clock
// that jumps across the sleep, and an awake clock that does not. advanced
// moves both; suspend moves only the wall one. Install it with newSleepClock,
// which swaps awakeNowFn for the duration of the test.
type sleepClock struct {
	mu    sync.Mutex
	wall  time.Time
	awake time.Duration
}

func newSleepClock(t *testing.T) *sleepClock {
	t.Helper()
	c := &sleepClock{wall: time.Now()}
	prev := awakeNowFn
	awakeNowFn = c.Awake
	t.Cleanup(func() { awakeNowFn = prev })
	return c
}

func (c *sleepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *sleepClock) Awake() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.awake
}

// advance is time passing with the machine awake: both clocks move.
func (c *sleepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.awake += d
}

// suspend is the lid closing: the wall clock moves, the awake clock does not.
func (c *sleepClock) suspend(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
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

// The dump's own header is what a reader has instead of the machine, and its
// staleness number must be the AWAKE silence. On the old header a 25s
// suspension and a 25s hang were the same line, and the files are read days
// later — pmset keeps 24h, so by then nothing can tell them apart.
func TestStallDumpNamesAwakeSilenceNotWallSilence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)

	path, err := app.dumpStall(time.Now(), 3*time.Minute, 7*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := string(data)
	if !strings.Contains(head, "7s of awake time ago") {
		t.Fatalf("the header does not say how long the loop was really silent:\n%.400s", head)
	}
	if !strings.Contains(head, "process awake for: 3m0s") {
		t.Fatalf("the header does not say how long this process has been running:\n%.400s", head)
	}
}
