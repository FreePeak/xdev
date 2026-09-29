package tui

// The mid-turn submit queue (#157), end to end from the TUI's side: Enter
// while a turn runs becomes a visible pending row, the row retires when the
// agent's steering drain proves the message reached the conversation, the row
// can be taken back into the composer, and the [send now] button arms the
// host's immediate-delivery path without running it under a.mu.
//
// The agent-side half (the loop drains steering at a step boundary and
// announces it) is covered in internal/agent; what these tests pin down is the
// contract between the two, which is where a queue silently loses messages.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// typeText feeds a string through the composer, one rune at a time, the way
// handleKey sees a real keystroke.
func typeText(app *App, s string) {
	for _, r := range s {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
}

// enter presses Enter.
func enter(app *App) {
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
}

// queueApp returns an app with the queue wired and a turn marked running, the
// state a mid-turn submit happens in.
func queueApp(t *testing.T) (*App, *[]string, *[]string) {
	t.Helper()
	app, _ := newTestApp(t, 100, 24)
	var sent, queued []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})
	app.SetQueueHandlers(func(text string) bool {
		queued = append(queued, text)
		return true
	}, func(text string) { queued = append(queued, "now:"+text) })
	app.SetRunning(true)
	return app, &sent, &queued
}

// queueWired marks a turn running and wires an accepting host: a mid-turn
// submit goes to the steer seam, and a send-now goes to the same seam, which
// is enough for a paint/geometry test that never inspects the delivery.
func queueWired(app *App) {
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.SetQueueHandlers(func(string) bool { return true }, func(string) {})
	app.SetRunning(true)
}

// TestEnterWhileWorkingQueuesInsteadOfRefusing: the whole point of #157. A
// prompt typed during a turn used to be rejected with "a turn is already
// running — Esc cancels it", which left the composer cleared and the words
// gone. It must reach the host's steer seam, in order, and never touch the
// ordinary send path.
func TestEnterWhileWorkingQueuesInsteadOfRefusing(t *testing.T) {
	app, sent, queued := queueApp(t)

	typeText(app, "use the other package")
	enter(app)

	if len(*sent) != 0 {
		t.Fatalf("a mid-turn prompt started its own turn: sent = %v", *sent)
	}
	if len(*queued) != 1 || (*queued)[0] != "use the other package" {
		t.Fatalf("queued = %v, want the prompt through the steer seam", *queued)
	}
	if got := app.PendingTexts(); len(got) != 1 || got[0] != "use the other package" {
		t.Fatalf("pending rows = %v, want the prompt listed once", got)
	}
}

// TestQueueOrderIsAppendOnly: three prompts typed during a turn keep their
// order, and the painted list reads oldest-first — the order they will be
// delivered in. A queue that re-sorts is a queue nobody trusts.
func TestQueueOrderIsAppendOnly(t *testing.T) {
	app, _, queued := queueApp(t)

	for _, s := range []string{"first", "second", "third"} {
		typeText(app, s)
		enter(app)
	}
	want := []string{"first", "second", "third"}
	if got := app.PendingTexts(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("pending = %v, want %v", got, want)
	}
	if got := strings.Join(*queued, "|"); got != "first|second|third" {
		t.Fatalf("steer seam saw %q, want first|second|third", got)
	}
}

// TestIdleSubmitIsUnchanged: the queue is a mid-turn affordance. With no turn
// running, Enter is still an ordinary send — a regression here would make every
// prompt in an idle session "pending" forever.
func TestIdleSubmitIsUnchanged(t *testing.T) {
	app, sent, queued := queueApp(t)
	app.SetRunning(false)

	typeText(app, "hello")
	enter(app)

	if len(*sent) != 1 || (*sent)[0] != "hello" {
		t.Fatalf("sent = %v, want the idle submit delivered as a turn", *sent)
	}
	if len(*queued) != 0 {
		t.Fatalf("an idle submit was queued: %v", *queued)
	}
	if app.QueuedCount() != 0 {
		t.Fatalf("an idle submit left a pending row: %v", app.PendingTexts())
	}
}

// TestDeclinedQueueWithdrawsTheRow: a host that cannot deliver (a guest room
// with no local run) returns false. The row must go away rather than sit there
// promising a delivery that will never come, and the submit falls through to
// the ordinary path so the host prints its own refusal.
func TestDeclinedQueueWithdrawsTheRow(t *testing.T) {
	app, sent, _ := newTestAppWithDeclinedQueue(t)

	typeText(app, "queue me")
	enter(app)

	if app.QueuedCount() != 0 {
		t.Fatalf("a declined prompt left a pending row: %v", app.PendingTexts())
	}
	if len(*sent) != 1 || (*sent)[0] != "queue me" {
		t.Fatalf("sent = %v, want the declined submit to fall through to a turn", *sent)
	}
}

// TestRetireDeliveredRemovesTheRowItNames: the agent's steering drain is the
// delivery point, so the row must retire there and only there. A row that
// outlives its delivery is a ghost the user can click on a message that is
// already in the transcript.
func TestRetireDeliveredRemovesTheRowItNames(t *testing.T) {
	app, _, _ := queueApp(t)
	for _, s := range []string{"one", "two", "three"} {
		typeText(app, s)
		enter(app)
	}

	app.RetireDelivered([]string{"one"})

	got := app.PendingTexts()
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("pending after one delivery = %v, want [two three]", got)
	}
	// A message that never had a row (an extension steer, a mailbox push) is a
	// no-op, not a corruption.
	app.RetireDelivered([]string{"from an extension"})
	if n := app.QueuedCount(); n != 2 {
		t.Fatalf("a foreign delivery changed the queue: %d rows left", n)
	}
	// Delivering the same text twice retires two rows: the person typed it
	// twice, so there are two entries and two deliveries.
	typeText(app, "two")
	enter(app)
	app.RetireDelivered([]string{"two"})
	app.RetireDelivered([]string{"two"})
	if n := app.QueuedCount(); n != 1 || app.PendingTexts()[0] != "three" {
		t.Fatalf("duplicate delivery retirement = %v, want [three]", app.PendingTexts())
	}
}

// TestQueuePaintsAboveTheComposer: the list is chrome, and it has to be
// visible — a pending prompt nobody can see is a prompt that might as well not
// have been typed. The rows sit between the transcript and the composer's top
// border, and they carry the [send now] button.
func TestQueuePaintsAboveTheComposer(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.SetQueueHandlers(func(string) bool { return true }, func(string) {})
	app.SetRunning(true)
	app.AddUserBlock("a prompt that is on screen")
	typeText(app, "queued message")
	enter(app)
	app.draw()

	screen := screenText(scr)
	if !strings.Contains(screen, "queued message") {
		t.Fatalf("the queued row is not on screen:\n%s", screen)
	}
	if !strings.Contains(screen, sendNowLabel) {
		t.Fatalf("the [send now] button is not on screen:\n%s", screen)
	}
	// The row is ABOVE the composer: the composer's border is the box's top
	// edge and the queue must not paint over it.
	app.mu.Lock()
	ct := app.height - 1 - app.composerRows()
	hits := append([]queueHit(nil), app.queueHits...)
	app.mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("published hit rows = %d, want 1", len(hits))
	}
	if hits[0].row.y >= ct {
		t.Fatalf("queued row painted at y=%d, above the composer top %d", hits[0].row.y, ct)
	}
	if !hits[0].btn.contains(hits[0].btn.x, hits[0].btn.y) {
		t.Fatal("the send-now button published no hit rect")
	}
}

// TestQueueRowClickTakesItBack: a click on the row body (not the button) puts
// the text back in the composer so a mistyped prompt can be fixed. The entry
// leaves the queue at the same moment — a row and a composer copy of the same
// message is one message in two places.
func TestQueueRowClickTakesItBack(t *testing.T) {
	app, _, _ := queueApp(t)
	typeText(app, "half-typed thought")
	enter(app)
	app.draw()

	app.mu.Lock()
	row := app.queueHits[0].row
	app.mu.Unlock()
	// A click on the row body, clear of the button. Routed through handleKey
	// because that is where the real event lands — and where the press edge is
	// computed, which the queue answers on.
	x, y := row.x+row.w/3, row.y
	app.handleKey(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone))

	if app.QueuedCount() != 0 {
		t.Fatalf("take-back left the row queued: %v", app.PendingTexts())
	}
	if got := app.ed.Text(); got != "half-typed thought" {
		t.Fatalf("composer after take-back = %q, want the entry back", got)
	}
}

// TestSendNowButtonArmsTheHostPath: the button's pixels are the button's
// action. The click ARMS the host's immediate-delivery path under the lock and
// the loop RUNS it unlocked — interrupting a turn and starting another while
// a.mu is held would deadlock against the transcript painter.
func TestSendNowButtonArmsTheHostPath(t *testing.T) {
	app, _, queued := queueApp(t)
	typeText(app, "answer this first")
	enter(app)
	app.draw()

	app.mu.Lock()
	btn := app.queueHits[0].btn
	app.mu.Unlock()
	press := tcell.NewEventMouse(btn.x+1, btn.y, tcell.Button1, tcell.ModNone)
	app.handleKey(press) // the press arms the action
	app.runPendingQueueAction()

	if len(*queued) != 2 || (*queued)[1] != "now:answer this first" {
		t.Fatalf("send-now delivered %v, want the now: path", *queued)
	}
	// The entry is still pending until it is actually delivered: send-now
	// hands the text to the host, and the host's run persists it.
	if got := app.PendingTexts(); len(got) != 1 || got[0] != "answer this first" {
		t.Fatalf("pending after send-now = %v, want the entry still tracked", got)
	}
}

// TestSendNowChordTakesTheOldest: F6 is the keyboard path to the same thing,
// and it takes the OLDEST pending message — the one that has been waiting
// longest goes first and the rest keep their place.
func TestSendNowChordTakesTheOldest(t *testing.T) {
	app, _, queued := queueApp(t)
	for _, s := range []string{"older", "newer"} {
		typeText(app, s)
		enter(app)
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyF6, 0, tcell.ModNone))

	if len(*queued) != 3 || (*queued)[2] != "now:older" {
		t.Fatalf("F6 delivered %v, want the oldest entry through now:", *queued)
	}
}

// TestSendNowWithNothingQueuedSaysSo: a chord that silently does nothing is a
// bug report waiting to happen. The notice is the contract.
func TestSendNowWithNothingQueuedSaysSo(t *testing.T) {
	app, _, queued := queueApp(t)

	app.handleKey(tcell.NewEventKey(tcell.KeyF6, 0, tcell.ModNone))

	if len(*queued) != 0 {
		t.Fatalf("F6 delivered %v with an empty queue", *queued)
	}
	if !strings.Contains(app.noticeText(), "nothing queued") {
		t.Fatalf("no notice for an empty send-now:\n%s", app.noticeText())
	}
}

// TestUnwiredSendNowIsANoticeNotASilentNoOp: a headless host wires no
// immediate-delivery path. The button must say so rather than pretend.
func TestUnwiredSendNowIsANoticeNotASilentNoOp(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.SetRunning(true)
	typeText(app, "waiting")
	enter(app) // no onQueue either: the pre-queue behavior, refused
	app.SetQueueHandlers(nil, nil)

	app.handleKey(tcell.NewEventKey(tcell.KeyF6, 0, tcell.ModNone))

	if !strings.Contains(app.noticeText(), "not wired") {
		t.Fatalf("unwired send-now did not report itself:\n%s", app.noticeText())
	}
}

// TestQueueOverflowIsNamedNotSilent: past the row cap the tail is counted, not
// dropped. A queue whose overflow is invisible looks like a queue that lost
// messages.
func TestQueueOverflowIsNamedNotSilent(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	queueWired(app)
	for i := 0; i < queueRowMax+3; i++ {
		typeText(app, "message")
		enter(app)
	}
	app.draw()

	if n := app.QueuedCount(); n != queueRowMax+3 {
		t.Fatalf("queue holds %d entries, want %d — painting must not drop any", n, queueRowMax+3)
	}
	if !strings.Contains(screenText(scr), "more queued") {
		t.Fatalf("the overflow is not named on screen:\n%s", screenText(scr))
	}
}

// TestMultilineQueueRowStaysOneLine: a queued prompt may contain a hard
// newline. A row that painted one would tear the box in half, so the row text
// is flattened — and the TAKE-BACK still returns the original, newlines and
// all, because that is the text the person typed.
func TestMultilineQueueRowStaysOneLine(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	queueWired(app)
	typeText(app, "first line")
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlJ, 0, tcell.ModNone))
	typeText(app, "second line")
	enter(app)
	app.draw()

	if !strings.Contains(screenText(scr), "first line second line") {
		t.Fatalf("the row did not flatten its newline:\n%s", screenText(scr))
	}
	if got := strings.Join(app.PendingTexts(), "|"); got != "first line\nsecond line" {
		t.Fatalf("the queued entry lost its newline: %q", got)
	}
}

// TestShellAndCommandEntriesAreMarked: "!<cmd>" and "/cmd" are not prompts.
// A row that showed them as ordinary text would promise a delivery the queue
// cannot make, so they wear their own mark.
func TestShellAndCommandEntriesAreMarked(t *testing.T) {
	if got := queueLabel("!ls -la"); got != queueKindShell {
		t.Fatalf("bang entry kind = %q", got)
	}
	if got := queueLabel("/model"); got != queueKindCommand {
		t.Fatalf("slash entry kind = %q", got)
	}
	if got := queueLabel("an ordinary question"); got != queueKindText {
		t.Fatalf("plain entry kind = %q", got)
	}
}

// newTestAppWithDeclinedQueue is a mid-turn app whose host REFUSES a queue.
func newTestAppWithDeclinedQueue(t *testing.T) (*App, *[]string, *[]string) {
	t.Helper()
	app, _ := newTestApp(t, 100, 24)
	var sent, queued []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})
	app.SetQueueHandlers(func(text string) bool {
		queued = append(queued, text)
		return false
	}, func(text string) {})
	app.SetRunning(true)
	return app, &sent, &queued
}

// noticeText is the transcript's system text joined, so a test can assert a
// notice was actually printed rather than trusting that a call reported.
func (a *App) noticeText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	for _, blk := range a.blocks {
		if blk.Kind == KindSystem {
			b.WriteString(blk.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
