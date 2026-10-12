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

// TestFormatTwoCodeSpansOnOneLine is the bug that made a whole answer read as
// code: with `([^`]+)` the second span's OPENER was taken as the first span's
// closer, so everything between them was escaped as code and the rest of the
// line was left unparsed. Measured on a live turn (2026-10-12): a line naming
// a bg job and a flag arrived as one giant <code> run.
func TestFormatTwoCodeSpansOnOneLine(t *testing.T) {
	got := ToTelegramHTML("job `alpha`, 2h cap, **exit 1, `--max-time`**) — pid\n")
	if n := strings.Count(got, "<code>"); n != 2 {
		t.Fatalf("want 2 code spans, got %d: %q", n, got)
	}
	for _, want := range []string{"<code>alpha</code>", "<code>--max-time</code>", "<b>exit 1, "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "**") {
		t.Errorf("bold markers survived: %q", got)
	}
}

// TestFormatInlineCodeNeverCrossesALine: an UNPAIRED backtick must not pair
// with one paragraphs later — that turned the prose between them into one
// code block (the same live turn).
func TestFormatInlineCodeNeverCrossesALine(t *testing.T) {
	got := ToTelegramHTML("a stray ` here\n\nand ordinary prose\n\nand a closing ` there\n")
	if strings.Contains(got, "<code>") {
		t.Errorf("an unpaired backtick swallowed the prose: %q", got)
	}
	if !strings.Contains(got, "ordinary prose") {
		t.Errorf("the prose was lost: %q", got)
	}
}

// TestFormatNestedTokensAreResolved: a code span inside a bold run is stashed
// as a tag whose body is another token. One ascending substitution pass left
// the inner token in the message as literal "\u0000BLOCK…\u0000" text.
func TestFormatNestedTokensAreResolved(t *testing.T) {
	got := ToTelegramHTML("**evidence: `xdev stats` commits**\n")
	if strings.ContainsRune(got, '\x00') || strings.Contains(got, "BLOCK") {
		t.Fatalf("a placeholder leaked: %q", got)
	}
	if !strings.Contains(got, "<b>") || !strings.Contains(got, "<code>xdev stats</code>") {
		t.Errorf("the nesting did not render: %q", got)
	}
}

// TestFormatTableBecomesBullets: Telegram draws no grid, so a GFM pipe table
// reaches the phone as literal pipes. Each row becomes a bold heading plus
// "• column: value" bullets — the shape Hermes sends
// (gateway/platforms/helpers.py convert_table_to_bullets).
func TestFormatTableBecomesBullets(t *testing.T) {
	got := ToTelegramHTML("| Repo | Result |\n|---|---|\n| **xdev** | 7 commits |\n| **leankg** | 37 merged |\n")
	if strings.Contains(got, "|") {
		t.Errorf("the table reached the phone as pipes: %q", got)
	}
	if strings.Contains(got, "---") {
		t.Errorf("the delimiter row survived: %q", got)
	}
	for _, want := range []string{"<b>xdev</b>", "• Result: 7 commits", "<b>leankg</b>", "• Result: 37 merged"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// A row label that already carried markdown bold must not become
	// "****xdev****": the stray '*' pairs with another one further down the
	// message and turns everything between them into one italic run.
	if strings.Contains(got, "**") {
		t.Errorf("the row heading left a stray marker: %q", got)
	}
}

// TestFormatLeavesTablesInsideCodeAlone: a fenced sample that contains pipes
// is code, not a table.
func TestFormatLeavesTablesInsideCodeAlone(t *testing.T) {
	got := ToTelegramHTML("```\n| a | b |\n|---|---|\n| 1 | 2 |\n```\n")
	if !strings.Contains(got, "<pre>") {
		t.Fatalf("the fence was lost: %q", got)
	}
	if !strings.Contains(got, "| a | b |") {
		t.Errorf("the code sample was rewritten: %q", got)
	}
}

// TestFormatTableWithoutALabelColumn: a headerless table's first cell becomes
// the heading and is not repeated as a bullet.
func TestFormatTableWithoutALabelColumn(t *testing.T) {
	got := ToTelegramHTML("| Name | Count |\n|---|---|\n| alpha | 3 |\n")
	if strings.Contains(got, "|") {
		t.Errorf("pipes survived: %q", got)
	}
	if !strings.Contains(got, "<b>alpha</b>") {
		t.Errorf("the row heading is not the first cell: %q", got)
	}
	if strings.Contains(got, "• Name: alpha") {
		t.Errorf("the heading was repeated as a bullet: %q", got)
	}
	if !strings.Contains(got, "• Count: 3") {
		t.Errorf("the value bullet is missing: %q", got)
	}
}
