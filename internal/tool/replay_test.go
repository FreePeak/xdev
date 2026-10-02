package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// replayStub is a registry tool that declares its own replay safety, so one
// fixture covers both a tool that answers and one that does not.
type replayStub struct {
	name string
	safe bool
}

func (s *replayStub) Name() string        { return s.name }
func (s *replayStub) Description() string { return "stub" }

func (s *replayStub) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

func (s *replayStub) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{Text: "ok"}, nil
}

func (s *replayStub) ReplaySafe() bool { return s.safe }

// TestRegistryReplaySafeDefaultsUnsafe: a tool that does not implement
// Replayer is unsafe. The failure that guards is a duplicated side effect,
// not a redundant read.
func TestRegistryReplaySafeDefaultsUnsafe(t *testing.T) {
	r := NewRegistry()
	r.Register(NewBashTool(t.TempDir()))
	r.Register(NewWriteTool())
	if r.ReplaySafe("bash") {
		t.Fatal("bash reported replay-safe; it is the one tool that must not be")
	}
	if r.ReplaySafe("write") {
		t.Fatal("write reported replay-safe")
	}
}

// TestRegistryReplaySafeReads covers the bundled inspectors.
func TestRegistryReplaySafeReads(t *testing.T) {
	r := NewRegistry()
	r.Register(NewReadTool())
	r.Register(&GrepTool{CWD: t.TempDir()})
	r.Register(&GlobTool{CWD: t.TempDir()})
	for _, n := range []string{"read", "grep", "glob"} {
		if !r.ReplaySafe(n) {
			t.Errorf("%s reported unsafe: it only reads state", n)
		}
	}
}

// TestRegistryReplaySafeUnknownName: a session can name a tool this process
// does not have — a --tools filter, a remote transcript, a renamed tool. An
// uninspectable tool must never read as harmless.
func TestRegistryReplaySafeUnknownName(t *testing.T) {
	r := NewRegistry()
	if r.ReplaySafe("nope") {
		t.Fatal("an unregistered tool reported replay-safe")
	}
	if r.ReplaySafe("") {
		t.Fatal("an empty tool name reported replay-safe")
	}
	var nilReg *Registry
	if nilReg.ReplaySafe("read") {
		t.Fatal("a nil registry reported replay-safe")
	}
}

// TestRegistryReplaySafeFollowsDeclaration pins that the answer comes from the
// tool and not from a name list: the same name flips both ways.
func TestRegistryReplaySafeFollowsDeclaration(t *testing.T) {
	quiet := NewRegistry()
	quiet.Register(&replayStub{name: "thing"})
	if quiet.ReplaySafe("thing") {
		t.Fatal("a tool with no Replayer reported safe")
	}

	loud := NewRegistry()
	loud.Register(&replayStub{name: "thing", safe: true})
	if !loud.ReplaySafe("thing") {
		t.Fatal("a tool declaring itself safe was reported unsafe")
	}
}
