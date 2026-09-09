package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

var ts0 = time.Date(2026, 9, 7, 3, 20, 45, 220e6, time.UTC) // .220 ms precision

func env(t, id, parent string, ts time.Time) Envelope {
	return Envelope{Type: t, ID: id, ParentID: parent, Timestamp: ts}
}

var slotTS = time.Date(2026, 9, 7, 3, 20, 57, 319e6, time.UTC) // .319Z like omp's updatedAt

func TestNewIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id := NewID()

		if len(id) != 8 {
			t.Fatalf("NewID length = %d, want 8", len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("NewID %q not hex", id)
			}
		}
		if seen[id] {
			t.Fatalf("NewID collision: %s", id)
		}
		seen[id] = true
	}
}

func TestMarshalEntryMessageWireShape(t *testing.T) {
	e := &MessageEntry{
		Env: env(TypeMessage, "abcd1234", "", ts0),
		Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.Block{ai.TextBlock{Text: "hi"}},
		},
	}
	got, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"message","id":"abcd1234","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z",` +
		`"message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
	if string(got) != want {
		t.Fatalf("wire mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestMarshalEntryRootParentNull(t *testing.T) {
	e := &ModelChangeEntry{Env: env(TypeModelChange, "11111111", "", ts0), Model: "router/free"}
	got, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"model_change","id":"11111111","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z","model":"router/free","resolvedModelIsFallback":false}`
	if string(got) != want {
		t.Fatalf("wire mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestEntryRoundTripAllTypes(t *testing.T) {
	kept := "kept1"
	entries := []Entry{
		&MessageEntry{
			Env: env(TypeMessage, "aaaaaaaa", "bbbbbbbb", ts0),
			Message: ai.Message{
				Role:    ai.RoleAssistant,
				Content: []ai.Block{ai.ThinkingBlock{Thinking: "hmm"}, ai.TextBlock{Text: "hi"}},
				Model:   "router/free", Usage: &ai.Usage{Input: 10, Output: 5, TotalTokens: 15},
			},
		},
		&MessageEntry{
			Env: env(TypeMessage, "cccccccc", "aaaaaaaa", ts0),
			Message: ai.Message{
				Role:       ai.RoleToolResult,
				ToolCallID: "call_1", ToolName: "bash", IsError: true,
				Content: []ai.Block{ai.TextBlock{Text: "boom"}},
			},
		},
		&ModelChangeEntry{Env: env(TypeModelChange, "dddddddd", "cccccccc", ts0), Model: "onegw/gpt", ResolvedModelIsFallback: true},
		&CompactionEntry{
			Env: env(TypeCompaction, "eeeeeeee", "dddddddd", ts0),
			Summary: ai.Message{
				Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "so far"}},
			},
			FirstKeptEntryID: &kept, TokensBefore: 4321,
		},
		&CompactionEntry{
			Env: env(TypeCompaction, "ffffffff", "eeeeeeee", ts0),
			Summary: ai.Message{
				Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "all"}},
			},
			TokensBefore: 1,
		},
		&BranchSummaryEntry{
			Env: env(TypeBranchSummary, "12345678", "ffffffff", ts0),
			Summary: ai.Message{
				Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "abandoned"}},
			},
		},
		&ResetBoundaryEntry{Env: env(TypeResetBoundary, "87654321", "12345678", ts0)},
		&CustomEntry{
			Env:        env(TypeCustom, "10101010", "87654321", ts0),
			CustomType: "tool_execution_start",
			Data:       map[string]any{"toolCallId": "call_9", "toolName": "read"},
		},
		&CustomEntry{Env: env(TypeCustom, "20101010", "10101010", ts0), CustomType: "session_exit"},
	}
	for _, e := range entries {
		line, err := MarshalEntry(e)
		if err != nil {
			t.Fatalf("%T: %v", e, err)
		}
		parsed, err := ParseEntry(line)
		if err != nil {
			t.Fatalf("%T: %v", e, err)
		}
		again, err := MarshalEntry(parsed)
		if err != nil {
			t.Fatalf("%T: re-marshal: %v", e, err)
		}
		if string(line) != string(again) {
			t.Fatalf("%T: round-trip mismatch:\n%s\n%s", e, line, again)
		}
	}
}

func TestParseEntrySemanticFields(t *testing.T) {
	t.Run("compaction", func(t *testing.T) {
		e, err := ParseEntry([]byte(`{"type":"compaction","id":"a1b2c3d4","parentId":"00000000","timestamp":"2026-09-07T03:20:45.220Z","summary":{"role":"assistant","content":[]},"firstKeptEntryId":"deadbeef","tokensBefore":77}`))
		if err != nil {
			t.Fatal(err)
		}
		c := e.(*CompactionEntry)
		if c.FirstKeptEntryID == nil || *c.FirstKeptEntryID != "deadbeef" || c.TokensBefore != 77 {
			t.Fatalf("got %+v", c)
		}
		if c.Envelope().ParentID != "00000000" {
			t.Fatalf("parent = %q", c.Envelope().ParentID)
		}
	})
	t.Run("compaction null firstKept", func(t *testing.T) {
		e, err := ParseEntry([]byte(`{"type":"compaction","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z","summary":{"role":"assistant","content":[]},"firstKeptEntryId":null,"tokensBefore":5}`))
		if err != nil {
			t.Fatal(err)
		}
		if c := e.(*CompactionEntry); c.FirstKeptEntryID != nil {
			t.Fatalf("firstKept = %v", *c.FirstKeptEntryID)
		}
	})
	t.Run("message with toolCall blocks", func(t *testing.T) {
		e, err := ParseEntry([]byte(`{"type":"message","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"plan","thinkingSignature":"reasoning_content"},{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"/x"},"partialArgs":"{\"path\":\"/x\"}","streamIndex":0,"intent":"read x"}]}}`))
		if err != nil {
			t.Fatal(err)
		}
		m := e.(*MessageEntry).Message
		if len(m.Content) != 2 {
			t.Fatalf("blocks = %d", len(m.Content))
		}
		tc, ok := m.Content[1].(ai.ToolCallBlock)
		if !ok || tc.ID != "call_1" || tc.Name != "read" || tc.Intent != "read x" {
			t.Fatalf("toolCall = %+v", tc)
		}
	})
}

func TestParseEntryUnknownType(t *testing.T) {
	_, err := ParseEntry([]byte(`{"type":"from_the_future","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z"}`))
	if !errors.Is(err, ErrUnknownEntryType) {
		t.Fatalf("err = %v, want ErrUnknownEntryType", err)
	}
}

func TestMarshalTitleSlotExactWidth(t *testing.T) {
	line := MarshalTitleSlot("Configure header to hide sidebar", TitleSourceAuto, slotTS)
	if len(line) != TitleSlotWidth {
		t.Fatalf("title slot = %d bytes, want %d", len(line), TitleSlotWidth)
	}
	if line[TitleSlotWidth-1] != '\n' {
		t.Fatalf("slot must end with newline")
	}
	title, ok := ParseTitleSlot(line[:TitleSlotWidth-1])
	if !ok || title != "Configure header to hide sidebar" {
		t.Fatalf("parse back = %q %v", title, ok)
	}
}

func TestMarshalTitleSlotLongTitleTruncated(t *testing.T) {
	long := strings.Repeat("абвгдеж", 500) // multibyte, long
	line := MarshalTitleSlot(long, TitleSourceAuto, slotTS)
	if len(line) != TitleSlotWidth {
		t.Fatalf("long-title slot = %d bytes, want %d", len(line), TitleSlotWidth)
	}
	if title, ok := ParseTitleSlot(line[:TitleSlotWidth-1]); !ok || title == "" {
		t.Fatalf("parse back failed: %q %v", title, ok)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := SessionHeader{
		Version: 3, ID: "01a079e1-efa4-70f4-936b-a39a83bd5771",
		Timestamp: ts0, CWD: "/Users/x/proj", Title: "t", TitleSource: TitleSourceAuto,
	}
	line := MarshalHeader(h)
	if !strings.HasPrefix(string(line), `{"type":"session","version":3,"id":"01a079e1-efa4-70f4-936b-a39a83bd5771"`) {
		t.Fatalf("header wire shape: %s", line)
	}
	if line[len(line)-1] != '\n' {
		t.Fatal("header must end with newline")
	}
	got, ok := ParseHeader(line[:len(line)-1])
	if !ok || got.ID != h.ID || got.CWD != h.CWD || got.Title != h.Title || got.TitleSource != h.TitleSource {
		t.Fatalf("parse back = %+v %v", got, ok)
	}
}

func TestParseHeaderRejectsNonHeader(t *testing.T) {
	if _, ok := ParseHeader([]byte(`{"type":"message","id":"x"}`)); ok {
		t.Fatal("non-header accepted")
	}
	if _, ok := ParseHeader([]byte(`{oops`)); ok {
		t.Fatal("garbage accepted")
	}
}

func TestEntryEnvelopeAccurateAfterParse(t *testing.T) {
	e, err := ParseEntry([]byte(`{"type":"message","id":"a1b2c3d4","parentId":"deadbeef","timestamp":"2026-09-07T03:20:45.220Z","message":{"role":"user","content":[{"type":"text","text":"x"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := e.Envelope()
	want := env(TypeMessage, "a1b2c3d4", "deadbeef", ts0)
	if got != want {
		t.Fatalf("envelope = %+v want %+v", got, want)
	}
	if !json.Valid(MarshalMust(t, e)) {
		t.Fatal("re-marshal invalid")
	}
}

// MarshalMust is a test helper.
func MarshalMust(t *testing.T, e Entry) []byte {
	t.Helper()
	b, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
