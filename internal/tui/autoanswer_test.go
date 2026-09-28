package tui

import (
	"strings"
	"testing"
)

// /auto-answer is a settings flip with a body: the ask card's answer policy
// (ask.autoAnswer) belongs in config, but a human who is tired of answering
// their own questions should not have to leave the transcript to say so. The
// two rules worth pinning are the one that can answer a question without a
// human (the vocabulary) and the one that makes it take effect in THIS session
// (the seam, and the order the App applies it in).

func TestAutoAnswerCommand(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddSystemBlock("x")
	on := false
	var saved []bool
	app.SetAutoAnswerOps(&AutoAnswerOps{
		Current: func() bool { return on },
		Set:     func(v bool) error { on = v; saved = append(saved, v); return nil },
		Path:    "/tmp/config.yml",
	})

	// A bare call toggles, in both directions, and reports where it landed.
	if err := app.AutoAnswer(""); err != nil {
		t.Fatal(err)
	}
	if !on || len(saved) != 1 || !saved[0] {
		t.Fatalf("bare /auto-answer = %v saved %v", on, saved)
	}
	app.mu.Lock()
	last := app.blocks[len(app.blocks)-1].Text
	app.mu.Unlock()
	if !strings.Contains(last, "auto-answer yes") || !strings.Contains(last, "/tmp/config.yml") {
		t.Fatalf("confirmation = %q", last)
	}
	if err := app.AutoAnswer(""); err != nil {
		t.Fatal(err)
	}
	if on {
		t.Fatal("a second bare call must toggle back off")
	}

	// The words that were asked for, and the ones every other toggle takes.
	for _, arg := range []string{"yes", "on", "true"} {
		if err := app.AutoAnswer(arg); err != nil {
			t.Fatalf("%q: %v", arg, err)
		}
		if !on {
			t.Fatalf("%q did not turn it on", arg)
		}
	}
	for _, arg := range []string{"no", "off", "false"} {
		if err := app.AutoAnswer(arg); err != nil {
			t.Fatalf("%q: %v", arg, err)
		}
		if on {
			t.Fatalf("%q did not turn it off", arg)
		}
	}

	// A typo is a usage error, NOT a toggle: a misspelled word must never be
	// the thing that lets a card answer for its human.
	before := len(saved)
	for _, arg := range []string{"maybe", "yes no", "ye"} {
		if err := app.AutoAnswer(arg); err == nil {
			t.Fatalf("%q must be refused, not applied", arg)
		}
	}
	if len(saved) != before || on {
		t.Fatalf("a refused argument still wrote: on=%v saved=%v", on, saved)
	}
}

// The unwired seam degrades to a visible notice, like every other ops seam:
// a command that silently does nothing is worse than one that says it is not
// available.
func TestAutoAnswerUnwired(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddSystemBlock("x")
	if err := app.AutoAnswer(""); err == nil {
		t.Fatal("an unwired seam must be an error, not a silent success")
	}
}
