package tool

import (
	"encoding/json"
	"testing"
)

func permBashArgs(t *testing.T, cmd string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMatchPermissionSpec pins the shared matcher a hook `if` prefilter
// rides (issue #38): Bash(git *) filters bash commands with the same
// compound-aware globbing the approval policy uses, Write(path) filters a
// tool's subject argument, and nothing else matches.
func TestMatchPermissionSpec(t *testing.T) {
	tests := []struct {
		name string
		spec string
		tool string
		args json.RawMessage
		want bool
	}{
		{"bare bash", "Bash", "bash", permBashArgs(t, "ls"), true},
		{"git pattern hits", "Bash(git *)", "bash", permBashArgs(t, "git push origin main"), true},
		{"compound smuggling", "Bash(git *)", "bash", permBashArgs(t, "echo x; git push"), true},
		{"pattern miss", "Bash(git *)", "bash", permBashArgs(t, "ls -la"), false},
		{"other tool never matches a Bash spec", "Bash(git *)", "write", json.RawMessage(`{"path":"x"}`), false},
		{"tool name must match", "Write(src/**)", "read", json.RawMessage(`{"path":"src/a.go"}`), false},
		{"subject arg matches", "Write(src/**)", "write", json.RawMessage(`{"path":"src/a.go"}`), true},
		{"subject arg miss", "Write(src/**)", "write", json.RawMessage(`{"path":"doc/a.md"}`), false},
	}
	for _, tc := range tests {
		if got := MatchPermissionSpec(tc.spec, tc.tool, tc.args); got != tc.want {
			t.Errorf("%s: MatchPermissionSpec(%q, %s) = %v, want %v", tc.name, tc.spec, tc.tool, got, tc.want)
		}
	}
}
