package tui

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/theme"
)

// The divider must name the leg that ACTUALLY served once the wire reported a
// switch: a status row still claiming the pinned model is the lie the badge
// exists to prevent. Painted through the real draw so the assertion is about
// the row the user reads, not about dividerModel in isolation.
func TestDividerShowsTheServingLeg(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock("ready")
	app.draw()
	if div := dividerRow(t, scr); !strings.Contains(div, "test/free") {
		t.Fatalf("divider %q must carry the pinned model before any switch", div)
	}

	app.SetServedBy("opencode-free/big-pickle")
	app.draw()
	div := dividerRow(t, scr)
	if !strings.Contains(div, modelSwitchBadge) {
		t.Fatalf("divider %q must badge a switched turn", div)
	}
	if !strings.Contains(div, "big-pickle") {
		t.Fatalf("divider %q must name the serving leg", div)
	}
	if strings.Contains(div, "test/free") {
		t.Fatalf("divider %q still claims the pinned model answered", div)
	}

	// Clearing it (a deliberate /model switch does exactly this) restores the
	// pinned reading: a divergence that no longer exists must not be painted.
	app.SetServedBy("")
	app.draw()
	if div := dividerRow(t, scr); strings.Contains(div, modelSwitchBadge) || !strings.Contains(div, "test/free") {
		t.Fatalf("divider %q must go back to the pinned model", div)
	}
}

// The status-row segment and the composer divider are two readings of one
// fact; they must never disagree about which model answered.
func TestStatusSegmentAndDividerAgree(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.SetServedBy("p2/m2")
	text, token := app.hudSegment("model")
	if text != app.dividerParts()[0].text {
		t.Fatalf("status segment %q and divider %q disagree", text, app.dividerParts()[0].text)
	}
	if token != theme.AccentError {
		t.Fatalf("a switched model must wear the warning ink, got %q", token)
	}
}

// A deliberate model switch clears the previous answer's serving leg: leaving
// it up would paint a divergence that the new choice just removed.
func TestSetStatusModelClearsServedBy(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.SetServedBy("p2/m2")
	app.SetStatusModel("onegw/dev")
	app.mu.Lock()
	model, served := app.st.Model, app.st.ServedBy
	app.mu.Unlock()
	if model != "onegw/dev" || served != "" {
		t.Fatalf("after /model: model=%q served=%q, want the switch cleared", model, served)
	}
}
