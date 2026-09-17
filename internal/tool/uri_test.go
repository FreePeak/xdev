package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadToolResolvesRegisteredURI(t *testing.T) {
	RegisterURIScheme("testscheme", func(uri string) (string, error) {
		return "line one\nline two\nline three", nil
	})
	rt := NewReadTool()
	res, err := rt.Execute(context.Background(), json.RawMessage(`{"path":"testscheme://thing","offset":2,"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("URI read errored: %q", res.Text)
	}
	if !strings.Contains(res.Text, "2:line two") {
		t.Fatalf("windowed URI text = %q", res.Text)
	}
	if strings.Contains(res.Text, "1:line one") {
		t.Fatalf("offset not honoured: %q", res.Text)
	}
}

func TestReadToolURIErrorSurfaces(t *testing.T) {
	RegisterURIScheme("badscheme", func(uri string) (string, error) {
		return "", os.ErrNotExist
	})
	rt := NewReadTool()
	res, _ := rt.Execute(context.Background(), json.RawMessage(`{"path":"badscheme://missing"}`))
	if !res.IsError {
		t.Fatalf("expected an error result, got %q", res.Text)
	}
}

func TestReadToolUnregisteredSchemeFallsThroughToFile(t *testing.T) {
	// http:// is not registered: the read tool must treat it as a path
	// (and fail as a missing file), not silently resolve it.
	rt := NewReadTool()
	res, _ := rt.Execute(context.Background(), json.RawMessage(`{"path":"noscheme://x"}`))
	if !res.IsError {
		t.Fatalf("unregistered scheme must fall through: %q", res.Text)
	}
}

func TestReadURITextTruncationNote(t *testing.T) {
	RegisterURIScheme("manyscheme", func(uri string) (string, error) {
		var b strings.Builder
		for i := 0; i < 10; i++ {
			b.WriteString("x\n")
		}
		return b.String(), nil
	})
	rt := NewReadTool()
	res, _ := rt.Execute(context.Background(), json.RawMessage(`{"path":"manyscheme://a","limit":3}`))
	if !strings.Contains(res.Text, "more lines") {
		t.Fatalf("truncation note missing: %q", res.Text)
	}
}

var _ = filepath.Join
