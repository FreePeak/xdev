package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// The mermaid renderer's acceptance tests. Each one pins a promise the
// fallback paths make to the reader: a diagram either draws every message and
// edge it declared, or it is not a diagram at all (the code band comes back).

// renderMermaid draws a mermaid block at width w and returns the transcript
// text, plus whether the renderer claimed it.
func renderMermaid(t *testing.T, src string, w int) (string, bool) {
	t.Helper()
	app, _ := newTestApp(t, w, 24)
	app.mu.Lock()
	lines := app.mermaidLines(src, w)
	app.mu.Unlock()
	if lines == nil {
		return "", false
	}
	return joinedLines(lines), true
}

// The fixture is a call chain shaped like the ones agents write: an edge
// service, a tier behind it, a storage hop, a note pinned to one tier, and a
// reply that goes back the way it came. It is generic on purpose — a test
// fixture that named a real system would ship that system's internals to
// anyone who can read this repository, and the shape is all the renderer
// actually exercises.
const seqSrc = "sequenceDiagram\n" +
	"    participant C as web-client\n" +
	"    participant E as edge-router\n" +
	"    participant S as order-service\n" +
	"    participant D as order-db\n" +
	"    C->>E: POST /api/v1/orders (client.go:88)\n" +
	"    E->>S: POST /internal/orders (router.go:41)\n" +
	"    S->>D: FindByCustomerID (store.go:17)\n" +
	"    D-->>S: rows\n" +
	"    S-->>E: order ids\n" +
	"    E-->>C: 201 Created\n"

// Every participant and every message the source declares must be visible in
// the render: a diagram that silently lost a row would look identical to one
// that did not.
func TestMermaidSequenceDrawsEveryParticipantAndMessage(t *testing.T) {
	got, ok := renderMermaid(t, seqSrc, 110)
	if !ok {
		t.Fatal("a well-formed sequenceDiagram must render")
	}
	for _, want := range []string{
		"web-client", "edge-router", "order-service", "order-db",
		"POST /api/v1/orders (client.go:88)",
		"POST /internal/orders (router.go:41)",
		"FindByCustomerID (store.go:17)",
		"rows",
		"order ids",
		"201 Created",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render is missing %q:\n%s", want, got)
		}
	}
	// Four head boxes, so four top corners in the first row region.
	if n := strings.Count(got, "┌"); n != 4 {
		t.Errorf("want 4 participant head boxes, got %d:\n%s", n, got)
	}
	// Every line must fit the terminal: a diagram that wraps is a lie about
	// the width it was given.
	for _, row := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if wd := width(row); wd > 110 {
			t.Errorf("row is %d cells, want <= 110: %q", wd, row)
		}
	}
}

// A message is a claim about who called whom, so the head has to point the
// right way: right at the receiver for a call, left for a reply. The label
// sits on its own row below the wire, so the assertion is about the wire row
// each message drew — not about the row its text happens to land on.
func TestMermaidSequenceArrowDirection(t *testing.T) {
	got, ok := renderMermaid(t, "sequenceDiagram\n  A->>B: forward\n  B-->>A: back\n", 60)
	if !ok {
		t.Fatal("must render")
	}
	rows := strings.Split(strings.TrimRight(got, "\n"), "\n")
	fwdRow, backRow := -1, -1
	for i, r := range rows {
		if strings.Contains(r, "forward") {
			fwdRow = i - 1 // a label's row is directly under its wire
		}
		if strings.Contains(r, "back") {
			backRow = i - 1
		}
	}
	if fwdRow < 0 || !strings.Contains(rows[fwdRow], "▶") {
		t.Fatalf("the call has no right-pointing head:\n%s", got)
	}
	if backRow < 0 || !strings.Contains(rows[backRow], "◀") {
		t.Fatalf("the reply has no left-pointing head:\n%s", got)
	}
	// The reply's head sits at the sender's lifeline (the left end), the
	// call's at the receiver's (the right end): the direction is the claim.
	if strings.Index(rows[fwdRow], "▶") < strings.Index(rows[backRow], "◀") {
		t.Errorf("heads do not sit on opposite ends:\n%s", got)
	}
}

// A diagram that cannot fit the terminal must not silently drop a message: it
// redraws as a call log, where every message is still its own block of rows.
// At this width a label wraps, so the assertion is on the words, not on the
// exact line they were cut at.
func TestMermaidSequenceNarrowCollapsesToLogNotSource(t *testing.T) {
	got, ok := renderMermaid(t, seqSrc, 24)
	if !ok {
		t.Fatal("a narrow terminal must still render the diagram, not the source")
	}
	for _, want := range []string{"api/v1/orders", "internal/orders", "FindByCustomerID", "Created"} {
		if !strings.Contains(got, want) {
			t.Errorf("collapsed log lost %q:\n%s", want, got)
		}
	}
	// Six messages, six arrows: nothing was dropped to make it fit.
	if n := strings.Count(got, "▶"); n != 6 {
		t.Errorf("want 6 message arrows, got %d:\n%s", n, got)
	}
	for _, row := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if wd := width(row); wd > 24 {
			t.Errorf("row is %d cells, want <= 24: %q", wd, row)
		}
	}
}

// A self-message is a call back into the same participant; it must draw as a
// hook on that lifeline rather than as a zero-length arrow.
func TestMermaidSelfMessageDrawsAHook(t *testing.T) {
	got, ok := renderMermaid(t, "sequenceDiagram\n  A->>A: retry\n", 60)
	if !ok {
		t.Fatal("must render")
	}
	if !strings.Contains(got, "retry") {
		t.Errorf("self message label missing:\n%s", got)
	}
	if !strings.Contains(got, "▶") {
		t.Errorf("self message has no arrowhead:\n%s", got)
	}
}

// A flowchart's promise is its edges. Every declared edge must reach a head
// on the target box: the node count is the box count, and the arrow count is
// the edge count.
func TestMermaidFlowchartDrawsEveryEdge(t *testing.T) {
	src := "flowchart TD\n  A[read config] --> B[parse yaml]\n  B --> C[build request]\n  C -->|send| D[store]\n"
	got, ok := renderMermaid(t, src, 100)
	if !ok {
		t.Fatal("a well-formed flowchart must render")
	}
	for _, want := range []string{"read config", "parse yaml", "build request", "store", "send"} {
		if !strings.Contains(got, want) {
			t.Errorf("render is missing %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "▼"); n != 3 {
		t.Errorf("want one downward head per edge (3), got %d:\n%s", n, got)
	}
	if n := strings.Count(got, "┌"); n != 4 {
		t.Errorf("want 4 node boxes, got %d:\n%s", n, got)
	}
}

// Fan-out is the case a naive layout draws on top of itself: the three
// branches must occupy three different columns, side by side under their
// common parent, and each must have its own head.
func TestMermaidFlowchartFanOutSpreadsBranches(t *testing.T) {
	src := "flowchart TD\n  A[start] --> B[read]\n  A --> C[write]\n  A --> D[delete]\n"
	got, ok := renderMermaid(t, src, 100)
	if !ok {
		t.Fatal("must render")
	}
	cols := map[string]int{}
	for _, r := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		for _, name := range []string{"read", "write", "delete"} {
			if i := strings.Index(r, "│"+name+"│"); i >= 0 {
				cols[name] = i
			}
		}
	}
	if len(cols) != 3 {
		t.Fatalf("want 3 branch boxes, got %d:\n%s", len(cols), got)
	}
	if cols["read"] == cols["write"] || cols["write"] == cols["delete"] {
		t.Errorf("branches landed in the same column: %v\n%s", cols, got)
	}
	if n := strings.Count(got, "▼"); n != 3 {
		t.Errorf("want 3 branch heads, got %d:\n%s", n, got)
	}
}

// A cycle must terminate the ranking and still draw: the source is a graph
// the model wrote, and a hang or a dropped node would both be failures.
func TestMermaidFlowchartCycleTerminates(t *testing.T) {
	got, ok := renderMermaid(t, "flowchart TD\n  A --> B\n  B --> C\n  C --> A\n", 100)
	if !ok {
		t.Fatal("a cycle must still render")
	}
	for _, want := range []string{"A", "B", "C"} {
		if !strings.Contains(got, want) {
			t.Errorf("cycle lost node %q:\n%s", want, got)
		}
	}
}

// The fallback is the safety property, and it is per block: a type this file
// does not draw, and a source whose lines the parser cannot read, both return
// nil so the caller keeps the code band.
func TestMermaidUnsupportedTypesAndBadSourceFallBack(t *testing.T) {
	for name, src := range map[string]string{
		"class diagram": "classDiagram\n  Animal <|-- Duck\n",
		"state diagram": "stateDiagram-v2\n  [*] --> pending\n  pending --> done\n",
		"empty graph":   "flowchart TD\n",
		"subgraph":      "flowchart TD\n  subgraph one\n  A --> B\n  end\n",
		"bare arrow":    "sequenceDiagram\n  A - B: no arrow head at all\n",
		"bare word":     "hello\n",
	} {
		if _, ok := renderMermaid(t, src, 100); ok {
			t.Errorf("%s must fall back to the code band, not render", name)
		}
	}
}

// A diagram's rows must fit the width it was given, at every width, including
// the degenerate ones. This is the property the transcript's row index
// depends on: a row wider than the frame pushes the timestamps out.
func TestMermaidEveryRenderedRowFitsTheWidth(t *testing.T) {
	sources := map[string]string{
		"sequence": seqSrc,
		"notes":    "sequenceDiagram\n  Note over A,B: a note that runs on for a while and keeps going past the edge\n  A->>B: go\n",
		"flow":     "flowchart TD\n  A[one] --> B[two]\n  B --> C[three]\n",
		"fanout":   "flowchart TD\n  A --> B[b]\n  A --> C[c]\n  A --> D[d]\n  A --> E[e]\n",
	}
	for name, src := range sources {
		for _, w := range []int{10, 20, 40, 60, 80, 100, 120, 200} {
			got, ok := renderMermaid(t, src, w)
			if !ok {
				continue
			}
			for _, row := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
				if wd := width(row); wd > w {
					t.Errorf("%s at w=%d: row is %d cells: %q", name, w, wd, row)
				}
			}
		}
	}
}

// The ascii symbol preset must not emit a glyph the terminal cannot draw: a
// terminal that cannot do box-drawing would render a tofu box per character.
func TestMermaidASCIIPresetDrawsNoUnicode(t *testing.T) {
	ascii := theme.Load("groknight")
	ascii.Symbols.Preset = "ascii"
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scr.Fini() })
	app := New(scr, ascii, "test/free", "sess1234")
	app.mu.Lock()
	lines := app.mermaidLines(seqSrc, 100)
	app.mu.Unlock()
	if lines == nil {
		t.Fatal("must render under the ascii preset")
	}
	for _, ln := range lines {
		for _, r := range runsString(ln.runs) {
			if r > 127 {
				t.Fatalf("ascii preset emitted %q in:\n%s", r, joinedLines(lines))
			}
		}
	}
}

// A role canvas is only worth having if the roles reach the paint: the head,
// the wire and the node text must be three different inks. The slots differ by
// colour and by the bold bit (a node label is bold, a wire is not), so the
// test reads both — a diagram painted entirely in one ink would still be
// legible and still be wrong.
func TestMermaidRolesStyleTheRow(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.mu.Lock()
	lines := app.mermaidLines("sequenceDiagram\n  A->>B: go\n", 60)
	app.mu.Unlock()
	if lines == nil {
		t.Fatal("must render")
	}
	ink := map[string]int{}
	for _, ln := range lines {
		for _, r := range ln.runs {
			fg, _, attrs := r.style.Decompose()
			ink[fmt.Sprintf("%d/%d", fg, attrs)]++
		}
	}
	// The head box, the wire and the label are three inks; a diagram painted
	// entirely in one ink would still be legible and still be wrong.
	if len(ink) < 3 {
		t.Errorf("want >= 3 inks (box, head, label), got %d: %v", len(ink), ink)
	}
}

// The fence is the only thing that decides whether a diagram renders, so the
// tag — not the source — is the seam. A ```go fence with mermaid-looking text
// stays code; a ```mermaid fence with a type this file cannot draw comes back
// as the code band it was.
func TestMermaidFenceTagDecidesInMarkdown(t *testing.T) {
	drawable := "```mermaid\n" + seqSrc + "```"
	untagged := "```\n" + seqSrc + "```"
	other := "```go\n" + seqSrc + "```"
	undrawable := "```mermaid\nclassDiagram\n  Animal <|-- Duck\n```"
	for _, tc := range []struct {
		name     string
		src      string
		diagrams bool
	}{
		{"a mermaid fence draws", drawable, true},
		{"an untagged fence is code", untagged, false},
		{"a go fence is code", other, false},
		{"an unsupported type falls back", undrawable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := newTestApp(t, 110, 24)
			app.mu.Lock()
			got := joinedLines(app.renderMarkdown(tc.src, 110))
			app.mu.Unlock()
			hasBox := strings.Contains(got, "┌")
			hasSource := strings.Contains(got, "Diagram")
			if hasBox != tc.diagrams {
				t.Errorf("diagram=%v, want %v:\n%s", hasBox, tc.diagrams, got)
			}
			if hasSource == tc.diagrams {
				t.Errorf("the source is %spainted, want the opposite:\n%s",
					map[bool]string{true: "", false: "still "}[tc.diagrams], got)
			}
		})
	}
}

// The fallback is the old code band, so a diagram the renderer cannot draw
// is what a message written before the feature existed looks like — which is
// the whole promise of turning it off.
func TestMermaidFallsBackToTheCodeBand(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	src := "before\n\n```mermaid\nstateDiagram-v2\n  [*] --> pending\n```\n\nafter"
	app.mu.Lock()
	lines := app.renderMarkdown(src, 100)
	got := joinedLines(lines)
	app.mu.Unlock()
	for _, want := range []string{"before", "after", "```mermaid", "stateDiagram-v2"} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback lost %q:\n%s", want, got)
		}
	}
	// The banded rows carry the code background; a drawn diagram never does.
	banded := 0
	for _, ln := range lines {
		if ln.bg != 0 {
			banded++
		}
	}
	if banded < 3 {
		t.Errorf("the fallback must band its rows, %d did", banded)
	}
}

// The setting is display-only and live: flipping it redraws what is already
// on screen, in both directions, without touching the block's text.
func TestMermaidToggleRedrawsLive(t *testing.T) {
	app, _ := newTestApp(t, 110, 24)
	// Every helper here takes a.mu itself (AddAssistantBlock and
	// SetRenderMermaid both lock), so the test must not hold it across them:
	// a nested Lock is an instant self-deadlock, not a slow test.
	app.AddAssistantBlock("```mermaid\n" + seqSrc + "```")
	blocks := app.Blocks()
	app.mu.Lock()
	drawn := joinedLines(app.blockLines(0, &blocks[0], 100))
	app.mu.Unlock()
	if !strings.Contains(drawn, "┌") {
		t.Fatalf("a mermaid fence must draw by default:\n%s", drawn)
	}
	app.SetRenderMermaid(false)
	app.mu.Lock()
	source := joinedLines(app.blockLines(0, &blocks[0], 100))
	app.mu.Unlock()
	if strings.Contains(source, "┌") {
		t.Errorf("with the setting off the fence is source again:\n%s", source)
	}
	if !strings.Contains(source, "sequenceDiagram") {
		t.Errorf("the source text must survive the toggle:\n%s", source)
	}
	// And back on, so the stamp is a two-way switch rather than a one-shot.
	app.SetRenderMermaid(true)
	app.mu.Lock()
	again := joinedLines(app.blockLines(0, &blocks[0], 100))
	app.mu.Unlock()
	if !strings.Contains(again, "┌") {
		t.Errorf("turning it back on must redraw:\n%s", again)
	}
}

// /settings renderMermaid is the slash path to the same key, and it persists
// through the wired seam rather than only flipping local state.
func TestMermaidSlashSettingPersistsAndFlips(t *testing.T) {
	app, _ := newTestApp(t, 110, 24)
	var saved []bool
	app.SetSettingsOps(&SettingsOps{Path: "/tmp/xdev/config.yml", SetMermaid: func(on bool) error {
		saved = append(saved, on)
		return nil
	}})
	if err := app.SettingsView("renderMermaid off"); err != nil {
		t.Fatalf("/settings renderMermaid off: %v", err)
	}
	if app.Mermaid() {
		t.Error("the setting did not turn rendering off")
	}
	if len(saved) != 1 || saved[0] {
		t.Errorf("the persisted value is %v, want [false]", saved)
	}
	if err := app.SettingsView("renderMermaid on"); err != nil {
		t.Fatalf("/settings renderMermaid on: %v", err)
	}
	if !app.Mermaid() {
		t.Error("the setting did not turn rendering back on")
	}
	if err := app.SettingsView("renderMermaid maybe"); err == nil {
		t.Error("a value outside on|off must be refused, not guessed at")
	}
}
