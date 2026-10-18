package gateway

import (
	"strings"
	"testing"
)

// TestWorkerArgs pins the argv of one chat turn. Every clause here is a
// behaviour the operator sees on the phone, so a change to one should have to
// argue with this test first.
func TestWorkerArgs(t *testing.T) {
	const text = "why is /tmp full"
	const model = "anthropic/claude-sonnet-4"

	args := workerArgs("sess-1", model, text)

	// The prompt is the positional argument, so the flag parser stops there:
	// anything after it is read as part of the prompt instead of parsed.
	if last := args[len(args)-1]; last != text {
		t.Fatalf("prompt must be the last arg, got %q (args: %v)", last, args)
	}

	// Thinking and the auto-title stay off the wire: stdout is forwarded to the
	// chat verbatim, so either would land as the answer. --allow-home keeps the
	// temp-dir switch from relocating a turn whose workspace IS $HOME.
	for _, flag := range []string{"--no-title", "--hide-thinking", "--allow-home"} {
		if !hasFlag(args, flag) {
			t.Errorf("missing %s, got %v", flag, args)
		}
	}

	// A turn must not narrate.
	if !hasFlagValue(args, "--append-system-prompt", chatStyle) {
		t.Errorf("missing --append-system-prompt with chatStyle, got %v", args)
	}
	// The style has to name the two shapes ToTelegramHTML can render, or the
	// renderer has nothing to do with a table or a command.
	for _, want := range []string{"table", "code block"} {
		if !strings.Contains(chatStyle, want) {
			t.Errorf("chatStyle does not mention %q: %q", want, chatStyle)
		}
	}

	if !hasFlagValue(args, "--resume", "sess-1") {
		t.Errorf("missing --resume sess-1, got %v", args)
	}
	if !hasFlagValue(args, "--model", model) {
		t.Errorf("missing --model %s, got %v", model, args)
	}

	// A fresh chat has neither, and neither flag may appear with an empty value
	// (the worker would parse "" as the model or session).
	fresh := workerArgs("", "", text)
	for _, flag := range []string{"--resume", "--model"} {
		if hasFlag(fresh, flag) {
			t.Errorf("empty value still passed %s: %v", flag, fresh)
		}
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}
