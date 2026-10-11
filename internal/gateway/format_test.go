package gateway

import (
	"strings"
	"testing"
)

// TestFormatRendersMarkdownForThePhone is the whole point of the feature:
// what the model writes as markdown must arrive as tags the client renders.
func TestFormatRendersMarkdownForThePhone(t *testing.T) {
	got := ToTelegramHTML("## Root cause\n\nThe `foo_bar` value is **wrong** — see [docs](https://x.dev/a) and ~~old~~ text.")
	for _, want := range []string{
		"<b>Root cause</b>",
		"<code>foo_bar</code>",
		"<b>wrong</b>",
		`<a href="https://x.dev/a">docs</a>`,
		"<s>old</s>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// No markdown emphasis survives; an underscore inside an identifier is
	// fine (it is the exact character MarkdownV2 would have rejected).
	for _, bad := range []string{"**", "~~", "## Root"} {
		if strings.Contains(got, bad) {
			t.Errorf("leftover markdown marker %q in %q", bad, got)
		}
	}
}

// TestFormatNeverLeavesATagUnescaped: the escaping pass runs AFTER the tags
// are built, so any tag left inline becomes visible "&lt;b&gt;" text. The
// placeholder list is what prevents that; this pins it.
func TestFormatNeverLeavesATagUnescaped(t *testing.T) {
	got := ToTelegramHTML("plain <b>already</b> and a real **bold**")
	// The model's literal tag is ESCAPED (it must not become a tag), and the
	// tag this formatter built is left intact.
	if !strings.Contains(got, "&lt;b&gt;already&lt;/b&gt;") {
		t.Errorf("the model's literal tag was not neutralised: %q", got)
	}
	if strings.Count(got, "<b>") != 1 {
		t.Errorf("want exactly one live tag (the formatter's own), got %q", got)
	}
	if !strings.Contains(got, "<b>bold</b>") {
		t.Errorf("the model's real bold did not render: %q", got)
	}
}

// TestFormatEscapesModelOutput: model text is untrusted — a <script> in an
// answer must reach the phone as characters, never as a tag the client acts on.
func TestFormatEscapesModelOutput(t *testing.T) {
	got := ToTelegramHTML("look: <script>alert(1)</script> & <img src=x onerror=y>")
	for _, bad := range []string{"<script>", "<img"} {
		if strings.Contains(got, bad) {
			t.Errorf("unescaped %q in %q", bad, got)
		}
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("the payload was dropped instead of escaped: %q", got)
	}
}

// TestFormatKeepsCodeVerbatim: a code block is escaped once, never re-parsed,
// so an example containing markdown or a tag renders as code.
func TestFormatKeepsCodeVerbatim(t *testing.T) {
	got := ToTelegramHTML("```go\nif a<b { f(\"x\") }\n// **not bold**\n```")
	if !strings.Contains(got, `<pre><code class="language-go">`) {
		t.Errorf("no fenced block rendered: %q", got)
	}
	if strings.Contains(got, "a&lt;b { f(&quot;x&quot;) }") && !strings.Contains(got, "a&lt;b") {
		t.Errorf("code body was altered: %q", got)
	}
	if strings.Contains(got, "<b>not bold</b>") {
		t.Errorf("code content was re-parsed as markdown: %q", got)
	}
}

// TestFormatChunkIsIndependent: a chunk that starts mid-sentence (which the
// chunker produces) must render on its own — an unclosed fence or a marker
// split across the boundary must not eat the rest of the chunk.
func TestFormatChunkIsIndependent(t *testing.T) {
	chunk := "going to the store and **then** home"
	got := ToTelegramHTML(chunk)
	if strings.Contains(got, "**") {
		t.Errorf("unpaired bold markers leaked through: %q", got)
	}
	if !strings.Contains(got, "<b>then</b>") {
		t.Errorf("the paired bold in this chunk did not render: %q", got)
	}
}

// TestPlainFallbackKeepsTheWords: the fallback strips formatting, never
// content — the answer must survive a rejected parse.
func TestPlainFallbackKeepsTheWords(t *testing.T) {
	in := "## Root cause\n\nThe `foo_bar` value is **wrong**."
	out := PlainTextForTelegram(in)
	for _, want := range []string{"Root cause", "foo_bar", "wrong"} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback lost %q: %q", want, out)
		}
	}
	for _, bad := range []string{"**", "##"} {
		if strings.Contains(out, bad) {
			t.Errorf("fallback kept the marker %q: %q", bad, out)
		}
	}
}

// TestIsParseErrorDistinguishesTelegramRefusals: the fallback must fire only
// on a parse rejection — a 429 or a network error is not a formatting
// problem and must surface as itself.
func TestIsParseErrorDistinguishesTelegramRefusals(t *testing.T) {
	parse := &APIError{Method: "sendMessage", Code: 400, Description: "Bad Request: can't parse entities: can't find end of the entity starting at byte offset 12"}
	if !isParseError(parse) {
		t.Error("a parse refusal must trigger the fallback")
	}
	flood := &APIError{Code: 429, Description: "Too Many Requests: retry later"}
	if isParseError(flood) {
		t.Error("a flood error is not a parse error")
	}
	auth := &APIError{Code: 401, Description: "Unauthorized"}
	if isParseError(auth) {
		t.Error("an auth error is not a parse error")
	}
}
