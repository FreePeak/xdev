package gateway

import (
	"fmt"
	"regexp"
	"strings"
)

// Telegram's HTML formatter.
//
// A chat message that arrives as `**bold**` and raw underlines reads as
// source, not prose. Hermes converts model markdown to Telegram's MarkdownV2
// (plugins/platforms/telegram/adapter.py format_message, :7600) so the phone
// renders headings, bold, code and links natively.
//
// This port uses Telegram's HTML dialect instead, because the failure modes
// are not symmetric: MarkdownV2 reserves ~20 characters that must ALL be
// escaped, and its grammar rejects an entire message for one unescaped
// character (a stray '_' inside an identifier, a '.' after a number), so a
// near-perfect conversion loses the whole answer. HTML reserves exactly
// three characters and they never collide with code, so a rendering is built
// from the text rather than by escaping around prose.
//
// The rule that carries over unchanged: ONE send attempt per message, and a
// plain-text fallback when the API rejects the parsed form (see
// Telegram.SendMessage).

var (
	reFencedBlock = regexp.MustCompile("(?s)```([^\n]*)\n(.*?)```")
	// Inline code matches the shortest span whose closing backtick does not
	// itself open another span, and never across a line: `a `b`` renders as
	// <code>a `b</code>, while `a\nb` is two paragraphs of prose.
	//
	// Both halves are load-bearing, and both were wrong before. The old
	// `([^`]+)` closed on the FIRST inner backtick, so a message with two code
	// spans on one line (`job `x`, … `y``) paired x's opener with y's opener
	// and rendered everything between them as code — the exact shape a model
	// produces when it quotes a test name and a flag in one sentence. Without
	// the newline ban, one unpaired backtick pairs with another paragraphs
	// later and turns the prose between them into a code block.
	reInlineCode = regexp.MustCompile("`((?:[^`\\n]|`[^`\\s])+?)`")
	reLinkTag    = regexp.MustCompile(`\[([^\]]+)\]\(([^()\s]+)\)`)
	reBoldPair   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalicPair = regexp.MustCompile(`\*([^*\n]+)\*`)
	reStrikePair = regexp.MustCompile(`~~([^~]+)~~`)
	reHeaderLine = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reTableSep   = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)+\|?\s*$`)
	reTableRow   = regexp.MustCompile(`^\s*\|.*\|\s*$`)
)

// ToTelegramHTML renders markdown as Telegram's HTML subset: <pre>, <code>,
// <b>, <i>, <s>, <a>, blockquote and paragraph breaks. Everything else is
// HTML-escaped, so model output can never inject a tag the phone would act on.
func ToTelegramHTML(text string) string {
	if text == "" {
		return text
	}
	// Fenced code first: its content is escaped verbatim and never re-parsed,
	// so a markdown example inside a code block stays a code block.
	var blocks []string
	text = reFencedBlock.ReplaceAllStringFunc(text, func(m string) string {
		g := reFencedBlock.FindStringSubmatch(m)
		lang := strings.ToLower(strings.TrimSpace(g[1]))
		cls := ""
		switch lang {
		case "go", "python", "py", "js", "ts", "tsx", "jsx", "rust", "java",
			"c", "cpp", "rb", "sh", "bash", "zsh", "json", "yaml", "yml",
			"sql", "html", "css", "toml", "diff":
			cls = ` class="language-` + lang + `"`
		}
		blocks = append(blocks, "<pre><code"+cls+">"+escapeHTML(strings.TrimRight(g[2], "\n"))+"</code></pre>")
		return blockPlaceholder(len(blocks) - 1)
	})
	// Tables next, before any inline span is claimed: a GFM pipe table is
	// prose to Telegram, which draws no grid — the pipes arrive as literal
	// characters. Each data row is rewritten into a bold heading plus
	// "• header: value" bullets, which is what Hermes does for the phone
	// (gateway/platforms/helpers.py convert_table_to_bullets). Running before
	// the bold pass is deliberate: the "**heading**" this emits is then
	// rendered by the ordinary bold rule, one wording for one construct.
	text = convertTablesToBullets(text)
	// Inline code: escaped verbatim.
	text = reInlineCode.ReplaceAllStringFunc(text, func(m string) string {
		return stashTag(&blocks, "<code>"+escapeHTML(reInlineCode.FindStringSubmatch(m)[1])+"</code>")
	})
	// Links before bold/italic, so a URL's * survives its own tag.
	text = reLinkTag.ReplaceAllStringFunc(text, func(m string) string {
		g := reLinkTag.FindStringSubmatch(m)
		return stashTag(&blocks, `<a href="`+escapeHTMLAttr(g[2])+`">`+escapeHTML(g[1])+"</a>")
	})
	text = reBoldPair.ReplaceAllStringFunc(text, func(m string) string {
		return stashTag(&blocks, "<b>"+escapeHTML(reBoldPair.FindStringSubmatch(m)[1])+"</b>")
	})
	text = reStrikePair.ReplaceAllStringFunc(text, func(m string) string {
		return stashTag(&blocks, "<s>"+escapeHTML(reStrikePair.FindStringSubmatch(m)[1])+"</s>")
	})
	text = reItalicPair.ReplaceAllStringFunc(text, func(m string) string {
		return stashTag(&blocks, "<i>"+escapeHTML(reItalicPair.FindStringSubmatch(m)[1])+"</i>")
	})
	// Headers render as bold: Telegram has no heading tag, and a "#" left in
	// the text reads as a bug to a model that just wrote a heading.
	text = reHeaderLine.ReplaceAllStringFunc(text, func(m string) string {
		return stashTag(&blocks, "<b>"+escapeHTML(strings.TrimSpace(reHeaderLine.FindStringSubmatch(m)[1]))+"</b>")
	})
	// Blockquotes: a leading > per line.
	text = reBlockquote.ReplaceAllStringFunc(text, func(m string) string {
		g := reBlockquote.FindStringSubmatch(m)
		if len(g) < 2 {
			return m
		}
		return stashTag(&blocks, "<blockquote>"+escapeHTML(g[1])+"</blockquote>")
	})
	// Every tag is already a placeholder, so the escape pass sees only prose.
	text = escapeHTML(text)
	// Paragraph breaks collapse to single newlines: the chat client already
	// draws the blank line, and Telegram keeps both only for <pre>.
	text = strings.ReplaceAll(text, "\n\n", "\n")
	// Tokens NEST: an inline span inside a bold run ("**evidence: `x`**") is
	// stashed as a bold tag whose body is the code token, and one ascending
	// pass would insert that body only after its own index was already
	// resolved — leaving the raw token in the message. Repeat until no token
	// is left, bounded by the block count (each pass resolves one level).
	for pass := 0; pass <= len(blocks) && strings.ContainsRune(text, '\x00'); pass++ {
		for i, b := range blocks {
			text = strings.ReplaceAll(text, blockPlaceholder(i), b)
		}
	}
	return text
}

// convertTablesToBullets rewrites GFM pipe tables into a bold heading per row
// plus "• header: value" bullets — the rendering Hermes sends to a phone
// (gateway/platforms/helpers.py convert_table_to_bullets). A table inside a
// fenced block is left alone: a code sample containing pipes is code.
//
// A table is recognised the way GFM defines one: a row containing a pipe whose
// NEXT line is the |---|---| delimiter, followed by further pipe rows. The
// delimiter is dropped — it carries no content.
func convertTablesToBullets(text string) string {
	if !strings.Contains(text, "|") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence || !strings.Contains(line, "|") || i+1 >= len(lines) || !reTableSep.MatchString(lines[i+1]) {
			out = append(out, line)
			continue
		}
		block := []string{line, lines[i+1]}
		j := i + 2
		for ; j < len(lines) && reTableRow.MatchString(lines[j]); j++ {
			block = append(block, lines[j])
		}
		out = append(out, renderTableBlock(block))
		i = j - 1
	}
	return strings.Join(out, "\n")
}

// renderTableBlock is the per-row rendering: the heading is the row's own
// label when the table carries a label column, else its first non-empty cell,
// and every other cell becomes a bullet naming its column.
func renderTableBlock(block []string) string {
	if len(block) < 3 {
		return strings.Join(block, "\n")
	}
	headers := splitTableRow(block[0])
	if len(headers) < 2 {
		return strings.Join(block, "\n")
	}
	// A label-column table has one more cell per row than it has headers; the
	// first cell is then the heading rather than a value.
	labelled := len(splitTableRow(block[2])) == len(headers)+1

	groups := make([]string, 0, len(block)-2)
	for n, row := range block[2:] {
		cells := splitTableRow(row)
		rawHeading, values := "", cells
		if labelled {
			if len(cells) > 0 {
				rawHeading = cells[0]
			}
			values = cells[1:]
		} else {
			for _, c := range cells {
				if c != "" {
					rawHeading = c
					break
				}
			}
		}
		if rawHeading == "" {
			rawHeading = fmt.Sprintf("Row %d", n+1)
		}
		// Pad/truncate to the header count so a ragged row cannot shift a
		// value under the wrong column name.
		for len(values) < len(headers) {
			values = append(values, "")
		}
		values = values[:len(headers)]

		// The heading is bolded ONCE. A row label that already carries
		// markdown bold ("**xdev**" in a | **Repo** | table) must be unwrapped
		// first: emitting "****xdev****" leaves a stray '*' next to the bold
		// pair, and that stray pairs with another one further down the message
		// — turning everything between them into one italic run.
		heading := reBoldPair.ReplaceAllString(rawHeading, "$1")
		group := []string{"**" + heading + "**"}
		for k, h := range headers {
			if !labelled && values[k] == rawHeading {
				continue // the heading already IS this cell
			}
			group = append(group, "• "+h+": "+values[k])
		}
		groups = append(groups, strings.Join(group, "\n"))
	}
	return strings.Join(groups, "\n\n")
}

// splitTableRow splits one pipe row into trimmed cells, dropping the optional
// outer pipes.
func splitTableRow(row string) []string {
	s := strings.TrimSpace(row)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// stashTag appends a built tag to the guarded list and returns its token.
func stashTag(blocks *[]string, tag string) string {
	*blocks = append(*blocks, tag)
	return blockPlaceholder(len(*blocks) - 1)
}
func protectTags(text string, blocks *[]string) string {
	for i := range *blocks {
		// Longer tags first so "<b>" does not match inside "<blockquote>".
		text = strings.ReplaceAll(text, (*blocks)[i], blockPlaceholder(i))
	}
	return text
}

// blockPlaceholder is a token that survives escaping and is replaced after.
func blockPlaceholder(i int) string { return "\u0000BLOCK" + itoa(i) + "\u0000" }

var reBlockquote = regexp.MustCompile(`(?m)^>\s?(.+)$`)

// escapeHTML escapes the three characters Telegram's HTML parser reserves.
// Apostrophes and quotes are left alone outside attributes, where they are
// legal and escaping them would render as visible entities.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

// escapeHTMLAttr escapes an attribute value (quotes matter there).
func escapeHTMLAttr(s string) string {
	s = escapeHTML(s)
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return strings.ReplaceAll(s, "'", "&#39;")
}

// PlainTextForTelegram is the fallback body when the API rejects the parsed
// form: the same answer, delivered as text rather than lost.
func PlainTextForTelegram(text string) string {
	if text == "" {
		return text
	}
	t := reBoldPair.ReplaceAllString(text, "$1")
	t = reStrikePair.ReplaceAllString(t, "$1")
	t = reItalicPair.ReplaceAllString(t, "$1")
	t = reHeaderLine.ReplaceAllStringFunc(t, func(m string) string {
		return strings.TrimSpace(reHeaderLine.FindStringSubmatch(m)[1])
	})
	return t
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
