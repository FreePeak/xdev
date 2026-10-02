package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The UI loop is single-goroutine by design: Run calls handleKey and draw
// inline (app.go). That is what makes the TUI cheap to reason about, and it is
// also why any operation that fails to return there is indistinguishable from
// a dead terminal — keys stop, Ctrl+C stops, output stops, but the process is
// alive. A report of "the whole TUI froze" cannot be diagnosed from the
// outside after the fact, so the app diagnoses itself.
//
// The watchdog runs on its own goroutine and never takes App.mu: if it did, a
// loop stuck while holding that mutex would block the very thing meant to
// record it. It only reads an atomic heartbeat.
const (
	// stallAfter is how long one loop iteration may take before it is
	// considered hung. Normal draws are sub-millisecond; a turn's work runs
	// on the agent goroutine, so a slow iteration here is a real stall, not
	// just a busy frame. Generous on purpose: a false alarm in a frozen-UI
	// file is worse than a five-second delay in writing the true one.
	stallAfter = 5 * time.Second
	// stallCheck is the watchdog's poll interval.
	stallCheck = time.Second
	// stallSlowLog is when a loop that is still beating names its slowest
	// iteration. Five seconds is the stall threshold; ten is the floor for
	// /export, /dump and a transcript snapshot, and a synchronous terminal
	// flush under a paste the user still holds (45s and 60s measured, #20)
	// is exactly the case the stall dump could never record because the loop
	// always recovered.
	stallSlowLog = 10 * time.Second
	// stallStackCap bounds the dump. Every goroutine's stack can exceed a
	// megabyte on a busy session; the head is where the stuck loop is, and a
	// truncated tail is still worth more than no file.
	stallStackCap = 1 << 20
	// pollSkip is how far the wall clock may run between two watchdog polls
	// before the gap is read as the machine having slept rather than the loop
	// being stuck. A suspended Mac advances the wall clock but not the UI loop,
	// and the loop cannot be blamed for time it never had: of the 165 dumps
	// xdev wrote on 2026-09-28, 16 were exactly this — every one of them inside
	// a `pmset -g log` sleep interval, the day's false alarms running 5s to
	// 17m43s. A poll merely late by seconds is scheduling, not sleep, so the
	// bound is generous.
	pollSkip = 30 * time.Second
)

// SetStallDumpDir enables the UI-loop stall dump. dir is where
// tui-stall-<timestamp>.txt files are written (the same <dataDir>/dumps tree
// `xdev dump` uses, so `xdev gc` already knows to collect them). Empty dir
// disables the watchdog. Call before Run.
func (a *App) SetStallDumpDir(dir string) {
	a.stallDir = dir
}

// SetStallExitAfter arms the watchdog's last resort. A loop stuck this long in
// one episode is not coming back — nothing can reach it, not even the quit
// chord, because handleKey runs on the loop — so the session ends: the
// terminal is put back first (scr.Fini via the app's restore hook) and the
// process exits non-zero. Call before Run.
func (a *App) SetStallExitAfter(d time.Duration) { a.stallExitAfter = d }

// exitWedgedLoop ends a session whose UI loop is not coming back. The restore
// hook is the same one the signal and panic guards use (tui.go:
// terminalRestore), so the tty leaves raw mode and the alt screen however this
// ends. It does not go through Quit: closing quitCh ASKS a loop that is
// already wedged to return, which is the thing that does not work. The process
// exits because nothing else can — the turn goroutine, the MCP servers and the
// tool workers are all still inside this pid.
func (a *App) exitWedgedLoop(stuck time.Duration) {
	fmt.Fprintf(os.Stderr, "\nxdev: UI loop wedged for %s and is not recovering — restoring the terminal and exiting.\n", stuck.Truncate(time.Second))
	// The restore is BOUNDED, and that is the whole point of this change.
	// The loop is frequently wedged inside the terminal write itself (17
	// frozen writes of up to 17m are recorded under dumps/), and scr.Fini
	// writes to that same fd — so an unbounded restore blocks on exactly the
	// thing it is rescuing the terminal from, the give-up never reaches
	// exitProcess, and the only way out is a kill from outside. Waiting is a
	// courtesy to the common case (a wedged loop, a tty that still accepts
	// bytes); passing the deadline is what makes the escape hatch an escape
	// hatch.
	if a.restoreTty != nil {
		restored := make(chan struct{})
		go func() {
			defer close(restored)
			a.restoreTty()
		}()
		select {
		case <-restored:
		case <-time.After(restoreGrace):
			fmt.Fprintf(os.Stderr, "xdev: terminal restore did not return in %s; exiting anyway\n", restoreGrace)
		}
	}
	exitProcess(1)
}

// restoreGrace bounds the pre-exit terminal restore: long enough for the
// usual case, short enough that a wedged tty cannot keep the process alive.
const restoreGrace = 2 * time.Second

// exitProcess is os.Exit behind one seam, so a test can observe the decision
// to end a wedged session without ending the test binary with it.
var exitProcess = os.Exit

// SetStallRestore wires the terminal restore exitWedgedLoop must run before
// ending the process. cmd passes scr.Fini, the same value terminalRestore
// holds. Call before Run.
func (a *App) SetStallRestore(fini func()) { a.restoreTty = fini }

// beatDone closes one loop iteration and names it when it took absurdly long.
// A stall dump only fires for a loop that never comes back; the slow-but-alive
// iteration is the one the user actually reports ("it froze while I was
// pasting"), so it leaves a line naming the phase instead of no trace at all.
// The phase is which part of the loop ran, so waiting on a key ("event") is
// told apart from the background repaint tick.
func (a *App) beatDone(phase string, start time.Time) {
	a.beat()
	if d := time.Since(start); d >= slowIteration {
		fmt.Fprintf(os.Stderr, "xdev: UI loop iteration took %s (%s)\n", d.Truncate(time.Millisecond), phase)
	}
}

// slowIteration is when beatDone names an iteration; it is a variable rather
// than the shipped constant only so a test can drive it without sleeping ten
// seconds. Production never writes it.
var slowIteration = stallSlowLog

// beat records that the UI loop is making progress. Called from Run once per
// iteration, so a hung handleKey/draw simply stops updating it.
func (a *App) beat() { a.loopBeat.Store(time.Now().UnixNano()) }

// watchStall is the watchdog: it watches for the loop to stop beating and
// writes the goroutine stacks that explain why. The thresholds and the stop
// channel are arguments rather than package state, so the goroutine reads
// nothing mutable — a test (and a future caller) can run one at a different
// cadence without racing another. It exits when stop closes. now is time.Now in
// production and a jumpable clock in a test: the one silence that is not a
// stall can only be told from a stall by how the clock itself behaved across
// the gap.
func (a *App) watchStall(after, check time.Duration, now func() time.Time, stop <-chan struct{}) {
	if a.stallDir == "" {
		return
	}
	tick := time.NewTicker(check)
	defer tick.Stop()
	// dumpedForBeat identifies the stall episode: it holds the beat timestamp
	// the current dump was written for, so one episode yields one file (the
	// loop may never recover, and a file per second would bury the useful
	// first one). A NEW episode is a beat that has advanced past it, i.e. the
	// loop did recover and hung again.
	dumpedForBeat := int64(-1)
	// Grace after a wake, so the loop gets to draw its first frame back before
	// a beat that predates the sleep is condemned a second time. DarkWake brings
	// the machine back without the user, and the beat it wakes into is stale by
	// construction. Two stall windows is the whole allowance: past that the
	// beat is a real stall, sleep or not, and it is reported.
	graceUntil := time.Time{}
	// condemnedAt is when this episode was first reported. A loop that never
	// comes back must not keep the process alive forever: after stallExitAfter
	// the watchdog restores the terminal and ends the session, because the
	// user reported the dead end as "I cannot exit, Ctrl+C and Ctrl+D do
	// nothing" (session 00b1c5a0, 2026-09-30) and no key can reach a loop
	// that is wedged. A recovered loop clears it, so the next hang is judged
	// on its own clock.
	condemnedAt := time.Time{}
	// prev is the previous poll's wall time; a skip shows up as one gap far
	// wider than the poll interval.
	prev := now()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			at := now()
			// A skip arms the grace; it does not disarm the watchdog. A loop
			// still stuck after a wake must still be reported.
			if at.Sub(prev) > pollSkip {
				graceUntil = at.Add(2 * after)
			}
			prev = at
			if at.Before(graceUntil) {
				continue
			}
			beat := a.loopBeat.Load()
			if at.Sub(time.Unix(0, beat)) < after {
				// The loop came back: a new episode starts from scratch, and
				// the chord belongs to the loop again.
				condemnedAt = time.Time{}
				a.wedged.Store(false)
				continue
			}
			a.wedged.Store(true)
			if condemnedAt.IsZero() {
				condemnedAt = at
			}
			if beat == dumpedForBeat {
				// Already reported for this beat, but the exit clock keeps
				// running: a loop that never recovers must not hold the
				// process forever.
				if a.stallExitAfter > 0 && at.Sub(condemnedAt) >= a.stallExitAfter {
					a.exitWedgedLoop(at.Sub(time.Unix(0, beat)))
					return
				}
				continue
			}
			dumpedForBeat = beat
			// stderr, never the screen: the loop is stuck so nothing can be
			// drawn, and raw-mode output still lands where it can be read.
			fmt.Fprintf(os.Stderr, "\nxdev: UI loop stalled for %s (pid %d)\n",
				at.Sub(time.Unix(0, beat)).Truncate(time.Second), os.Getpid())
			if path, err := a.dumpStall(at, beat); err != nil {
				fmt.Fprintf(os.Stderr, "xdev: stall dump failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "xdev: stall dump written to %s\n", path)
			}
		}
	}
}

// dumpStall captures every goroutine and writes it under the dumps dir.
func (a *App) dumpStall(at time.Time, beat int64) (string, error) {
	buf := make([]byte, stallStackCap)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	if err := os.MkdirAll(a.stallDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(a.stallDir, fmt.Sprintf("tui-stall-%s.txt", at.Format("20060102-150405")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// The header deliberately avoids App state: reading model/session needs
	// the mutex the stuck loop may be holding.
	head := fmt.Sprintf("xdev UI loop stall\n"+
		"written: %s\npid: %d\nlast loop beat: %s (%s ago)\n"+
		"--- goroutine stacks ---\n",
		at.Format(time.RFC3339), os.Getpid(),
		time.Unix(0, beat).Format(time.RFC3339Nano),
		at.Sub(time.Unix(0, beat)).Truncate(time.Millisecond))
	if _, err := f.WriteString(head); err != nil {
		return "", err
	}
	if _, err := f.Write(buf); err != nil {
		return "", err
	}
	return path, nil
}

// startStallWatchdog arms the watchdog with the shipped thresholds.
func (a *App) startStallWatchdog() { go a.watchStall(stallAfter, stallCheck, time.Now, a.quitCh) }
