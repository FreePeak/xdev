package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The working-state block exists because the facts a resumed session needs are
// not narrative. These tests pin that it records what the harness actually
// did, costs nothing when there is nothing, and never damages a rung's own
// output.

func toolCallMsg(name, args string) ai.Message {
	return ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
		ai.ToolCallBlock{ID: "c" + name, Name: name, Arguments: json.RawMessage(args)},
	}}
}

func toolResult(tool string, isErr bool, body string) ai.Message {
	return ai.Message{
		Role: ai.RoleToolResult, ToolName: tool, IsError: isErr,
		Content: []ai.Block{ai.TextBlock{Text: body}},
	}
}

func TestWorkingStateRecordsWhatHappened(t *testing.T) {
	msgs := []ai.Message{
		toolCallMsg("read", `{"path":"internal/a.go"}`),
		toolCallMsg("bash", `{"command":"go test ./internal/...  2>&1 | tail -5"}`),
		toolCallMsg("write", `{"path":"internal/b.go","content":"package b"}`),
		toolCallMsg("ask", `{"question":"rename b.go too?"}`),
		toolResult("bash", true, "FAIL github.com/FreePeak/xdev/internal/a"),
	}
	block := extractWorkingState(msgs).render()

	for _, want := range []string{
		"internal/b.go",            // edited
		"internal/a.go",            // read
		"go test ./internal/...",   // command, whitespace collapsed
		"rename b.go too?",         // question
		"FAIL github.com/FreePeak", // failure
	} {
		if !strings.Contains(block, want) {
			t.Errorf("working state missing %q:\n%s", want, block)
		}
	}
}

// TestWorkingStateReadsTheEditPatchHeader pins the one parsing rule in the
// extractor: the edit tool takes a hashline patch whose target lives in the
// first line, not in a JSON field.
func TestWorkingStateReadsTheEditPatchHeader(t *testing.T) {
	msgs := []ai.Message{toolCallMsg("edit", `{"input":"[src/pkg/t.go#1a2b]\nPUT 3:=4:\n+\tx := 1"}`)}
	block := extractWorkingState(msgs).render()
	if !strings.Contains(block, "src/pkg/t.go") {
		t.Fatalf("the patch header path was not recorded:\n%s", block)
	}
	// The freshness tag must not leak into the recorded path.
	if strings.Contains(block, "1a2b") {
		t.Fatalf("the freshness tag leaked into the path:\n%s", block)
	}
}

func TestWorkingStateIsFreeWhenNothingHappened(t *testing.T) {
	// A span of pure conversation records nothing and therefore costs zero
	// bytes: the "free tier" must not add noise where it has no facts.
	msgs := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	}
	if block := extractWorkingState(msgs).render(); block != "" {
		t.Fatalf("a span with no tool activity must render nothing, got:\n%s", block)
	}
}

func TestWorkingStateDedupesAndBounds(t *testing.T) {
	var msgs []ai.Message
	for i := 0; i < 50; i++ {
		msgs = append(msgs, toolCallMsg("read", `{"path":"same.go"}`))
		msgs = append(msgs, toolCallMsg("bash", `{"command":"make test"}`))
	}
	block := extractWorkingState(msgs).render()
	if n := strings.Count(block, "same.go"); n != 1 {
		t.Errorf("the same read appears %d times; the extractor must dedupe", n)
	}
	if n := strings.Count(block, "make test"); n != 1 {
		t.Errorf("the same command appears %d times", n)
	}
}

// TestWithWorkingStateLeavesImageSummariesAlone protects snapcompact: its
// summary is a rendered bitmap, and there is nowhere to put prose beside an
// image. Returning it untouched is better than mangling it.
func TestWithWorkingStateLeavesImageSummariesAlone(t *testing.T) {
	img := ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
		ai.TextBlock{Text: "caption"},
		ai.ImageBlock{Source: ai.ImageSource{Type: "base64", MediaType: "image/png", Data: "..."}},
	}}
	ws := extractWorkingState([]ai.Message{toolCallMsg("read", `{"path":"a.go"}`)})
	got := withWorkingState(img, ws)
	if len(got.Content) != 2 {
		t.Fatalf("an image summary was rewritten: %d blocks", len(got.Content))
	}
	if _, ok := got.Content[0].(ai.TextBlock); !ok {
		t.Fatal("the original first block must survive")
	}
}

func TestWithWorkingStatePrependsBeforeTheRungsOwnOutput(t *testing.T) {
	// Order matters: the facts come first so a resumed session reads them
	// before (or instead of) a rung's prose summary.
	summary := textSummary("shake: elided 40 messages")
	ws := extractWorkingState([]ai.Message{toolCallMsg("edit", `{"path":"internal/x.go"}`)})
	got := withWorkingState(summary, ws)

	first, ok := got.Content[0].(ai.TextBlock)
	if !ok || !strings.Contains(first.Text, "internal/x.go") {
		t.Fatalf("working state must lead, got %+v", got.Content)
	}
	if len(got.Content) != 2 {
		t.Fatalf("the rung's own summary must survive, got %d blocks", len(got.Content))
	}
	if second, ok := got.Content[1].(ai.TextBlock); !ok || !strings.Contains(second.Text, "shake") {
		t.Fatalf("the rung's output was lost: %+v", got.Content[1])
	}
}

// TestWorkingStateBlockIsBounded guards the claim that this tier is cheap: a
// pathological span must not become the largest thing in the retained context.
// The paths are unique per call, because the dedupe under test above would
// otherwise collapse this to two lines and never reach the cap at all.
func TestWorkingStateBlockIsBounded(t *testing.T) {
	var msgs []ai.Message
	for i := 0; i < 400; i++ {
		msgs = append(msgs,
			toolCallMsg("read", `{"path":"some/deep/path/file`+strconv.Itoa(i)+`.go"}`),
			toolCallMsg("bash", `{"command":"a long command that repeats `+strconv.Itoa(i)+`"}`))
	}
	block := extractWorkingState(msgs).render()
	if len(block) > workingStateMaxChars+64 {
		t.Fatalf("block is %d bytes, past the %d cap", len(block), workingStateMaxChars)
	}
	// The per-list cap is the real bound; the true totals must still be
	// stated, because "25 rows" must never read as "that is all of them".
	if !strings.Contains(block, "files read (400)") {
		t.Fatalf("the header must state the true total:\n%s", block)
	}
	if !strings.Contains(block, "and 375 more") {
		t.Fatalf("the overflow must be stated:\n%s", block)
	}
}

// TestWorkingStateIsNotInjectedIntoShake is the regression guard for a real
// mistake: the extractor originally prepended to every rung, and shake's own
// tests caught it, because a file list re-introduces exactly the argument
// values that shake exists to delete.
func TestWorkingStateIsNotInjectedIntoShake(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("shake"))
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 || entries[0].Method != methodShake {
		t.Fatalf("entries = %d, method = %q", len(entries), entries[0].Method)
	}
	text := entries[0].Summary.Text()
	if strings.Contains(text, "Working state") {
		t.Fatalf("shake must not carry the working-state block:\n%s", text)
	}
	if strings.Contains(text, "/tmp/cfg.go") {
		t.Fatalf("shake kept an argument value:\n%s", text)
	}
}
