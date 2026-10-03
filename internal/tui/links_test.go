package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestClickVisibleLinkOpensTarget(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.BeginAssistant()
	app.AppendAssistant("See [PR #24775](https://github.com/pulumi/pull/24775) for details.")
	app.EndAssistant()
	app.draw()

	var opened []string
	app.linkOpen = func(raw string) error { opened = append(opened, raw); return nil }
	y, x := rowWith(t, scr, "PR #24775")
	click(app, x, y)
	if len(opened) != 1 || opened[0] != "https://github.com/pulumi/pull/24775" {
		t.Fatalf("opened = %q", opened)
	}
}

// TestUserRowAndLinkStayIndependent pins that the two click affordances sharing
// a primary-button press do not interfere. A user prompt opens its menu; a
// rendered link opens its target. They look like one gesture, so each half
// asserts that its own surface opens and the other's does not.
//
// They cannot in fact collide: user blocks are painted from `blockLines` as a
// banded "❯ " prefix plus wrapped plain text and never go through the Markdown
// renderer, so a user row never carries a link hit. A URL typed into a prompt is
// therefore text, and clicking it opens the menu. The release path keeps a
// link-over-menu precedence for the day that changes, and this test is what
// would notice.
//
// Each half gets its own App: handleClick treats a second press within
// clickWordTol of the first as a double-click and consumes it for word
// selection, so driving both gestures at one transcript would measure the
// click counter rather than the two affordances.
func TestUserRowAndLinkStayIndependent(t *testing.T) {
	t.Run("link opens the target and no menu", func(t *testing.T) {
		app, scr := newTestApp(t, 100, 30)
		defer scr.Fini()
		app.AddUserBlock("first prompt")
		app.AddAssistantBlock("see https://example.com/issue for details")
		app.draw()

		var opened []string
		app.linkOpen = func(raw string) error { opened = append(opened, raw); return nil }

		ly, lx := rowWith(t, scr, "https://example.com/issue")
		app.mu.Lock()
		press(app, lx, ly)
		release(app, lx, ly)
		menuOpen := app.msgm != nil
		app.mu.Unlock()
		if len(opened) != 1 || opened[0] != "https://example.com/issue" {
			t.Fatalf("clicking the link: opened = %q, want the target", opened)
		}
		if menuOpen {
			t.Fatal("a link click opened the message menu")
		}
	})

	t.Run("user row opens the menu and no link", func(t *testing.T) {
		app, scr := newTestApp(t, 100, 30)
		defer scr.Fini()
		app.AddUserBlock("first prompt")
		app.draw()

		var opened []string
		app.linkOpen = func(raw string) error { opened = append(opened, raw); return nil }

		y := userRow(t, app, 0)
		app.mu.Lock()
		press(app, 6, y)
		release(app, 6, y)
		menuOpen := app.msgm != nil
		app.mu.Unlock()
		if !menuOpen {
			t.Fatal("clicking a user row did not open the message menu")
		}
		if len(opened) != 0 {
			t.Fatalf("a user-row click opened a link: %q", opened)
		}
	})
}

func TestDragFromLinkSelectsText(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.BeginAssistant()
	app.AppendAssistant("See https://example.com/path for details.")
	app.EndAssistant()
	app.draw()

	var opened []string
	app.linkOpen = func(raw string) error { opened = append(opened, raw); return nil }
	y, x := rowWith(t, scr, "https://example.com/path")
	app.mu.Lock()
	drag(app, x, y, x+2, y)
	app.mu.Unlock()
	if len(opened) != 0 {
		t.Fatalf("drag opened link: %q", opened)
	}
	if got := string(scr.GetClipboardData()); got == "" {
		t.Fatal("drag from link did not select text")
	}
}

func TestClickAfterRepaintDoesNotOpenStaleLink(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.BeginAssistant()
	app.AppendAssistant("See [PR #24775](https://github.com/pulumi/pull/24775) for details.")
	app.EndAssistant()
	app.draw()

	var opened []string
	app.linkOpen = func(raw string) error { opened = append(opened, raw); return nil }
	y, x := rowWith(t, scr, "PR #24775")
	app.mu.Lock()
	press(app, x, y)
	app.linkHits = nil // a resize or scroll rebuilt the painted frame
	release(app, x, y)
	app.mu.Unlock()
	if len(opened) != 0 {
		t.Fatalf("stale link opened: %q", opened)
	}
}

func TestOverlaySuppressesTranscriptLinkHits(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.BeginAssistant()
	app.AppendAssistant("See https://example.com/path")
	app.EndAssistant()
	app.draw()
	hit := app.linkHits[0]
	app.diffOv = &diffOverlay{path: "internal/tui/dock.go", diff: "+added"}
	if got := app.linkAt(hit.x0, hit.y); got != "" {
		t.Fatalf("overlay click opened hidden transcript link %q", got)
	}
}

func TestTimestampDoesNotCoverLinkHit(t *testing.T) {
	app, scr := newTestApp(t, 32, 24)
	app.BeginAssistant()
	app.AppendAssistant("See https://example.com/very/long/path")
	app.EndAssistant()
	app.draw()

	app.mu.Lock()
	ts := app.blocks[0].Ts.Format("3:04 PM")
	app.mu.Unlock()
	y, tsX := rowWith(t, scr, ts)
	if got := app.linkAt(tsX, y); got != "" {
		t.Fatalf("timestamp cell opens %q", got)
	}
	for _, hit := range app.linkHits {
		if hit.y == y && hit.target == "https://example.com/very/long/path" && hit.x1 >= tsX {
			t.Fatalf("hit extends under timestamp: %+v at timestamp x=%d", hit, tsX)
		}
	}
}

func TestWrappedBareURLUsesPaintedHitRegion(t *testing.T) {
	app, _ := newTestApp(t, 32, 24)
	app.BeginAssistant()
	app.AppendAssistant("Read https://example.com/very/long/path")
	app.EndAssistant()
	app.draw()

	var hit linkHit
	for _, h := range app.linkHits {
		if h.target == "https://example.com/very/long/path" {
			hit = h
			break
		}
	}
	if hit.target == "" {
		t.Fatalf("wrapped URL has no painted hit region: %+v", app.linkHits)
	}
	var opened []string
	app.linkOpen = func(raw string) error { opened = append(opened, raw); return nil }
	click(app, hit.x0, hit.y)
	if len(opened) != 1 || opened[0] != hit.target {
		t.Fatalf("opened = %q, want %q", opened, hit.target)
	}
}

func TestLinkHitMatchesPaintedWidth(t *testing.T) {
	app, scr := newTestApp(t, 40, 24)
	app.BeginAssistant()
	app.AppendAssistant("See [👩‍💻 code](https://example.com/path)")
	app.EndAssistant()
	app.draw()

	y, x := rowWith(t, scr, "code")
	if got := app.linkAt(x, y); got != "https://example.com/path" {
		t.Fatalf("last visible cell is not clickable: %q", got)
	}
}

func TestWebURLDetection(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"https://example.com/path.", "https://example.com/path"},
		{"https://example.com/a?x=1&y=2,", "https://example.com/a?x=1&y=2"},
		{"(https://example.com/a)", "https://example.com/a"},
		{"HTTPS://example.com/path", "HTTPS://example.com/path"},
		{"abchttps://example.com", ""},
		{"•https://example.com/path", "https://example.com/path"},
		{"https://en.wikipedia.org/wiki/Foo_(bar)", "https://en.wikipedia.org/wiki/Foo_(bar)"},
		{"https:///missing-host", ""},
		{"https://example.com。", "https://example.com"},
		{"https://example.com/path）", "https://example.com/path"},
		{"https://example.com。下文", "https://example.com"},
		{"https://example.com/路径(章节)", "https://example.com/路径(章节)"},
		{"https://example.com/{章节}", "https://example.com/{章节}"},
		{"https://example.com/〈章节〉", "https://example.com/〈章节〉"},
		{"éhttps://example.com", ""},
		{"file:///tmp/x", ""},
		{"https://example.com/path**", "https://example.com/path"},
		{"https://example.com/path__", "https://example.com/path"},
		{"https://example.com/path``", "https://example.com/path"},
		{"plain text", ""},
		{"https://example.com/path*", "https://example.com/path"},
		{"https://example.com/path_", "https://example.com/path"},
		{"https://example.com/path`", "https://example.com/path"},
	} {
		var got []string
		for _, run := range appendWebURLRuns(nil, tc.text, tcell.StyleDefault, tcell.StyleDefault) {
			if run.link != "" {
				got = append(got, run.link)
			}
		}
		if strings.Join(got, "\n") != tc.want {
			t.Errorf("URLs in %q = %q, want %q", tc.text, got, tc.want)
		}
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://example.com", true},
		{"http://example.com", true},
		{"file:///tmp/x", false},
		{"https:///missing-host", false},
		{"", false},
	} {
		if got := isWebURL(tc.raw); got != tc.want {
			t.Errorf("isWebURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestOpenLinkRejectsNonWebTarget(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var opened []string
	app.linkOpen = func(raw string) error { opened = append(opened, raw); return nil }
	if err := app.openLink("file:///tmp/x"); err == nil {
		t.Fatal("openLink accepted a non-web target")
	}
	if len(opened) != 0 {
		t.Fatalf("non-web target reached opener: %q", opened)
	}
}

func TestStartAndReapOpener(t *testing.T) {
	done, err := startAndReap(exec.Command(os.Args[0], "-test.run=^TestLinkOpenerHelperProcess$"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("opener process: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opener process was not reaped")
	}
}

func TestLinkOpenerHelperProcess(t *testing.T) {}

func click(app *App, x, y int) {
	app.mu.Lock()
	defer app.mu.Unlock()
	press(app, x, y)
	release(app, x, y)
}
