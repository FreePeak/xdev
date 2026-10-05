package ai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A model whose tool-call stream is spliced with NUL bytes (measured 2026-10-05
// on onegw/opencode-space-bunny-free: 51 of 83,060 stored calls carried a
// U+0000, three of them inside the tool NAME, which then resolved to
// `unknown tool "\x00bash"`) must never hand a NUL to a tool. NUL is valid
// inside a JSON string, so nothing on the decode path rejects it — and
// execve(2) answers any argv string containing one with EINVAL, which is what
// surfaced as `bash: start: fork/exec /bin/bash: invalid argument` and sent the
// model hunting a broken shell.
//
// Two properties matter and they pull in opposite directions:
//
//   - every other control character is legitimate payload. \t \n \r are what a
//     multi-line command, a `write` of a source file, or an `edit` patch is
//     made of, so the scrub must not touch them.
//   - the model's own bytes survive in PartialArgs for diagnostics, and the
//     in-band dialects re-serialize from the parsed arguments, so only the
//     parsed form is scrubbed.
func TestToolCallArgsAndNameDropNUL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string // the streamed argument blob
		wantCmd string // the command after the scrub
		want    string // the whole scrubbed object, when the case pins one
	}{
		{
			name:    "nul between path segments reads as the missing separator",
			raw:     `{"command":"cd ~ && ls -1 Library/Accounts \u0000 2>/dev/null"}`,
			wantCmd: "cd ~ && ls -1 Library/Accounts  2>/dev/null",
		},
		{
			name:    "nul inside a redirect survives as a drop, not an edit",
			raw:     `{"command":"plutil -p Library/Preferences\u0000/com.apple.commerce.plist"}`,
			wantCmd: "plutil -p Library/Preferences/com.apple.commerce.plist",
		},
		{
			// Tabs and newlines are the payload of a multi-line command, a
			// written file and an edit patch. The guard must not touch them.
			name:    "tabs and newlines are payload",
			raw:     `{"command":"for i in 1 2; do\techo \"$i\"\ndone"}`,
			wantCmd: "for i in 1 2; do\techo \"$i\"\ndone",
		},
		{
			// An argument that was ONLY the splice keeps its key and arrives
			// empty, so bash's own "command is required" error is what the
			// model reads — not a guess, and not a dropped key quietly
			// satisfying `required`.
			name:    "an all-nul argument arrives empty, not deleted",
			raw:     `{"command":"\u0000"}`,
			wantCmd: "",
		},
		{
			// A nested string is scrubbed too, and a non-string keeps its JSON
			// type through the re-encode rather than round-tripping a float.
			name:    "nested strings are scrubbed and numbers keep their type",
			raw:     `{"command":"ls","timeout":60,"env":{"tags":["a\u0000b"],"n":7}}`,
			wantCmd: "ls",
			want:    `{"command":"ls","env":{"n":7,"tags":["ab"]},"timeout":60}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseToolArgs(APIOpenAICompletions, "call_1", tc.raw)
			var a struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(got, &a); err != nil {
				t.Fatalf("scrubbed args are not decodable: %v (%s)", err, got)
			}
			if strings.ContainsRune(a.Command, 0) {
				t.Fatalf("a NUL survived into the argument: %q", a.Command)
			}
			if a.Command != tc.wantCmd {
				t.Fatalf("command = %q, want %q", a.Command, tc.wantCmd)
			}
			if tc.want == "" {
				return
			}
			// Re-encoding is map order, so compare decoded JSON, not bytes.
			var gotv, wantv any
			if err := json.Unmarshal(got, &gotv); err != nil {
				t.Fatalf("scrubbed args do not decode: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantv); err != nil {
				t.Fatalf("case want does not decode: %v", err)
			}
			if !reflect.DeepEqual(gotv, wantv) {
				t.Fatalf("args = %s, want %s", got, tc.want)
			}
		})
	}
}

// The name is the other half: `\x00bash` reached the registry lookup as an
// unknown tool, which reads to the model as "this session has no bash tool"
// rather than "your own output was spliced". Scrubbing it back to `bash` turns
// a dead end into a call that runs.
func TestToolNameNULIsScrubbedNotLookedUp(t *testing.T) {
	if got := scrubToolName("\x00bash"); got != "bash" {
		t.Fatalf("scrubToolName(%q) = %q, want bash", "\x00bash", got)
	}
	if got := scrubToolName("bash"); got != "bash" {
		t.Fatalf("a clean name was rewritten: %q", got)
	}
	// A name that was ONLY the splice has nothing to salvage: leaving it empty
	// routes to the existing nameless-call error rather than inventing a tool.
	if got := scrubToolName("\x00"); got != "" {
		t.Fatalf("scrubToolName(%q) = %q, want empty", "\x00", got)
	}
}

// CleanUTF8 is the emit-side filter every text and thinking delta passes
// through, and it deliberately does NOT drop NUL (NUL is valid UTF-8 and the
// session JSONL keeps it). The tool-argument scrub is the separate, narrower
// guard this fix adds — this test pins that the two do not drift into each
// other.
func TestCleanUTF8StillKeepsNULForTranscripts(t *testing.T) {
	if got := CleanUTF8("a\x00b"); got != "a\x00b" {
		t.Fatalf("CleanUTF8 dropped a NUL a transcript may legitimately carry: %q", got)
	}
}
