package tui

import "testing"

// The chrome names the model, it does not route to it: the divider, the status
// row's segment and the sidebar footer read "Deepseek V4.1 Flash", and only
// /model (and the switch toast) keep the id that the wire needs.
func TestModelDisplay(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"onegw/opencode/deepseek-v4.1-flash", "Deepseek V4.1 Flash"},
		{"onegw/opencode/space-bunny-free", "Space Bunny Free"},
		{"opencode-free/big-pickle", "Big Pickle"},
		// No prefix, nothing to strip.
		{"grok-4.6", "Grok 4.6"},
		// An id that is already a name keeps its capitals: only the first
		// letter of a word is raised, nothing is lowered.
		{"GPT-5", "GPT 5"},
		// Underscores are a word break too, and a single word still reads as
		// a name.
		{"qwen_max", "Qwen Max"},
		{"dev", "Dev"},
		// Degenerate inputs must not panic or invent a name.
		{"", ""},
		{"onegw/", "Onegw/"},
	} {
		if got := modelDisplay(tc.id); got != tc.want {
			t.Errorf("modelDisplay(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
