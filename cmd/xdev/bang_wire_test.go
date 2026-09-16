package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tui"
	"github.com/gdamore/tcell/v2"
)

// Shell mode's wired executor (#163): the composer's "!cmd" path must run the
// command for real and land its output in the transcript as one finished tool
// box — no model call on the path. This is the end-to-end half of the seam;
// internal/tui pins the routing, this pins what the wired runner does with a
// command once the router hands it one.
func TestBangRunnerWritesTranscriptBox(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string // substring the result box must carry
	}{
		{name: "output", command: "echo bang-probe", want: "bang-probe"},
		// A non-zero exit is a normal result (the footer reports the code),
		// not a failed run: the command ran and said what it had to say.
		{name: "nonzero exit still reports output", command: "echo out; exit 3", want: "out"},
		{name: "unknown command reports the shell's complaint", command: "definitely-not-a-real-binary-xyz9", want: "command not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newBangTestApp(t)
			// The runner owns the goroutine; wait for the finished box rather
			// than sleeping a fixed time.
			if err := newBangRunner(app, t.TempDir(), context.Background())(tc.command); err != nil {
				t.Fatalf("runner rejected the command: %v", err)
			}
			done := waitForToolDone(t, app)

			if done.ToolName != bangToolName {
				t.Errorf("result box tool name = %q, want %q", done.ToolName, bangToolName)
			}
			// The command ran, so the box is not an error box whatever the
			// exit status was; the code rides the outcome footer instead.
			if done.Err {
				t.Errorf("Err = true, want a normal result (output %q)", done.Text)
			}
			if tc.want != "" && !strings.Contains(done.Text, tc.want) {
				t.Errorf("output = %q, want substring %q", done.Text, tc.want)
			}
			// The call row carries the raw JSON arguments, so the transcript
			// renders "!bash · <the command the user typed>".
			blocks := app.Blocks()
			if len(blocks) == 0 || blocks[0].Kind != tui.KindTool || !strings.Contains(blocks[0].Text, tc.command) {
				t.Errorf("call row = %+v, want the raw command arguments", blocks)
			}
		})
	}
}

// newBangTestApp builds the minimal App the runner writes into.
func newBangTestApp(t *testing.T) *tui.App {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scr.Fini)
	scr.SetSize(80, 24)
	return tui.New(scr, theme.Load("groknight"), "test/free", "sess")
}

// waitForToolDone blocks until the transcript's newest block is a finished
// tool result, which is what a bang run ends with.
func waitForToolDone(t *testing.T, app *tui.App) tui.Block {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if blocks := app.Blocks(); len(blocks) > 1 {
			if last := blocks[len(blocks)-1]; last.Kind == tui.KindToolDone {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the bang run never finished")
	return tui.Block{}
}
