package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderCard(t *testing.T) {
	spec := RenderSpec{Kind: "card", Spec: json.RawMessage(`{"title":"name","fields":["name","size"]}`)}
	got, ok := renderToolOutput(spec, "ext_x_get", `{"name":"report.pdf","size":12,"extra":true}`)
	if !ok {
		t.Fatal("card did not render")
	}
	if got != "report.pdf\n  name: report.pdf\n  size: 12" {
		t.Fatalf("card = %q", got)
	}
}

func TestRenderTableAlignsColumns(t *testing.T) {
	spec := RenderSpec{Kind: "table", Spec: json.RawMessage(`{"columns":["file","hits"],"items":"rows"}`)}
	got, ok := renderToolOutput(spec, "t", `{"rows":[{"file":"a.go","hits":3},{"file":"bb.go","hits":10}]}`)
	if !ok {
		t.Fatal("table did not render")
	}
	lines := strings.Split(got, "\n")
	if lines[0] != "file   hits" { // last column unpadded
		t.Fatalf("header = %q", lines[0])
	}
	if len(lines) != 3 || !strings.Contains(lines[2], "bb.go") {
		t.Fatalf("rows = %q", got)
	}
}

func TestRenderTreeOverArray(t *testing.T) {
	spec := RenderSpec{Kind: "tree"}
	got, ok := renderToolOutput(spec, "t", `[{"path":"cmd"},{"path":"internal"}]`)
	if !ok || !strings.Contains(got, "cmd") || !strings.Contains(got, "  internal") && !strings.Contains(got, "internal") {
		t.Fatalf("tree = %q ok=%v", got, ok)
	}
}

// TestRenderersDegradeGracefully is the PRD promise: unknown kinds,
// non-JSON payloads, and missing fields all fall back to plain text
// instead of hiding the tool result.
func TestRenderersDegradeGracefully(t *testing.T) {
	cases := []struct {
		name string
		spec RenderSpec
		raw  string
	}{
		{"unknown kind", RenderSpec{Kind: "hologram"}, `{"a":1}`},
		{"plain text payload", RenderSpec{Kind: "card"}, "just words"},
		{"array for card", RenderSpec{Kind: "card"}, `[1,2]`},
		{"bad spec", RenderSpec{Kind: "card", Spec: json.RawMessage(`{oops`)}, `{"a":1}`},
		{"empty table", RenderSpec{Kind: "table", Spec: json.RawMessage(`{"items":"rows"}`)}, `{"rows":[]}`},
	}
	for _, tc := range cases {
		if got, ok := renderToolOutput(tc.spec, "t", tc.raw); ok {
			t.Errorf("%s: rendered %q instead of degrading", tc.name, got)
		}
	}
}

func TestAppFinishToolUsesRenderer(t *testing.T) {
	a := &App{}
	a.SetRenderers(map[string]RenderSpec{
		"ext_x_info": {Kind: "card", Spec: json.RawMessage(`{"title":"status"}`)},
	})
	a.AddToolBlock("ext_x_info", "{}")
	a.FinishTool("ext_x_info", false, `{"status":"all green"}`, ToolOutcome{Dur: "1ms"})
	b := a.blocks[len(a.blocks)-1]
	if b.Kind != KindToolDone || b.Text != "all green" {
		t.Fatalf("result block = %+v", b)
	}
	// Errors keep the raw text: a failing tool is not being decorated.
	a.FinishTool("ext_x_info", true, `{"status":"broken"}`, ToolOutcome{Dur: "1ms"})
	if got := a.blocks[len(a.blocks)-1].Text; got != `{"status":"broken"}` {
		t.Fatalf("error result was rewritten: %q", got)
	}
}
