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
	app.diffOv = &diffOverlay{path: "internal/tui/dock.go", diff: "+added", width: app.width}
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
