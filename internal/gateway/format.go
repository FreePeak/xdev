package gateway

import (
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
	reInlineCode  = regexp.MustCompile("`([^`]+)`")
	reLinkTag     = regexp.MustCompile(`\[([^\]]+)\]\(([^()\s]+)\)`)
	reBoldPair    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalicPair  = regexp.MustCompile(`\*([^*\n]+)\*`)
	reStrikePair  = regexp.MustCompile(`~~([^~]+)~~`)
	reHeaderLine  = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
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
	for i, b := range blocks {
		text = strings.ReplaceAll(text, blockPlaceholder(i), b)
	}
	return text
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
