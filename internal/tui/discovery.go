package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/marketplace"
)

// MarkdownCommand is one slash command discovered from a markdown file.
type MarkdownCommand struct {
	Name        string // command name: frontmatter "name" when set, else the filename without .md; lowercased
	Description string
	Template    string // file body with frontmatter stripped
	Path        string // absolute source path (diagnostics only)
}

// userCommandsDir returns the user-level command root (config.DataDir). It
// is a var, not a plain call, so tests can swap it for a temp dir and
// discovery never reads the developer's real home directory.
var userCommandsDir = config.DataDir

// DiscoverCommands finds markdown slash commands for cwd. Project root
// <cwd>/.xdev/commands/*.md wins over user root ~/.xdev/agent/commands/*.md
// on a name collision; results are sorted by Name. Non-recursive, *.md only,
// hidden files skipped, missing roots skipped, unreadable/invalid files
// skipped (never fatal).
func DiscoverCommands(cwd string) []MarkdownCommand {
	byName := make(map[string]MarkdownCommand)
	roots := []string{filepath.Join(cwd, ".xdev", "commands")}
	if dir := userCommandsDir(); dir != "" {
		roots = append(roots, filepath.Join(dir, "commands"))
	}
	// Installed plugins last: they never shadow authored content, so a name
	// collision resolves to the project/user/managed root and the plugin only
	// supplies names nobody else claims (#85).
	roots = append(roots, marketplace.CommandDirs()...)
	// os.ReadDir sorts entries lexically, so the first file claiming a name
	// within a root is the lexical winner; earlier roots shadow later ones.
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // missing or unreadable root: skip, never fatal
		}
		for _, e := range entries {
			file := e.Name()
			if e.IsDir() || strings.HasPrefix(file, ".") || !strings.HasSuffix(file, ".md") {
				continue
			}
			cmd, ok := parseMarkdownCommand(filepath.Join(root, file))
			if !ok {
				continue
			}
			if _, seen := byName[cmd.Name]; seen {
				continue
			}
			byName[cmd.Name] = cmd
		}
	}
	out := make([]MarkdownCommand, 0, len(byName))
	for _, cmd := range byName {
		out = append(out, cmd)
	}
	slices.SortFunc(out, func(a, b MarkdownCommand) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// parseMarkdownCommand reads one markdown command file. It fails only for
// unreadable files, an unterminated frontmatter block, or a whitespace-only
// body — those commands would be empty anyway.
func parseMarkdownCommand(path string) (MarkdownCommand, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MarkdownCommand{}, false
	}
	lines := strings.Split(string(data), "\n")
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(path), ".md"))
	description := ""
	body := lines
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		closed := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				closed = i
				break
			}
		}
		if closed < 0 {
			return MarkdownCommand{}, false // frontmatter never closes
		}
		for _, line := range lines[1:closed] {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(key) {
			case "name":
				name = strings.ToLower(strings.TrimSpace(value))
			case "description":
				description = strings.TrimSpace(value)
			}
		}
		body = lines[closed+1:]
	}
	template := strings.Join(body, "\n")
	if strings.TrimSpace(template) == "" {
		return MarkdownCommand{}, false // body-less or whitespace-only file
	}
	// A name the parser can never accept (underscores, uppercase, a colon)
	// or one a built-in already owns is not a command: listing it would put
	// a row in the dropdown that submits literal text to the model. Built-ins
	// reserve their names (omp parity), so the shadowed file is skipped.
	if !isPlainCommandName(name) || isReservedCommandName(name) {
		return MarkdownCommand{}, false
	}
	if description == "" {
		description = firstLine(template, 60)
	}
	return MarkdownCommand{Name: name, Description: description, Template: template, Path: path}, true
}

// isPlainCommandName reports whether name is dispatchable as a bare slash
// command: the [a-z][a-z0-9-]* shape with no colon (a colon means an
// extension-qualified "/server:cmd", which markdown files may not claim).
func isPlainCommandName(name string) bool {
	return !strings.Contains(name, ":") && isCommandName(name)
}

// isReservedCommandName reports whether a built-in owns the name.
func isReservedCommandName(name string) bool {
	for _, c := range builtinCommands() {
		if c.Name == name || slices.Contains(c.Aliases, name) {
			return true
		}
	}
	return false
}

// firstLine returns the first non-empty line of s, trimmed, truncated to max
// runes with "…" appended when it runs longer.
func firstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) <= max {
			return line
		}
		return string(runes[:max]) + "…"
	}
	return ""
}

// SplitArgs splits a raw argument string quote-aware: single and double
// quotes group words (quote characters removed), no backslash escapes, an
// unmatched quote is not an error (the rest of the string is one argument),
// fields split on ASCII space/tab.
func SplitArgs(raw string) []string {
	var args []string
	var token strings.Builder
	inToken := false // tracks empty quoted args like ""
	var quote byte   // 0 = unquoted, else the active quote character
	flush := func() {
		args = append(args, token.String())
		token.Reset()
		inToken = false
	}
	for i := range len(raw) {
		c := raw[i]
		switch {
		case quote == 0 && (c == ' ' || c == '\t'):
			if inToken {
				flush()
			}
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
			inToken = true
		case quote != 0 && c == quote:
			quote = 0
		default:
			token.WriteByte(c)
			inToken = true
		}
	}
	if inToken || quote != 0 {
		flush() // unmatched quote: the rest of the string is one argument
	}
	return args
}

// ExpandArgs substitutes placeholders in a command template using the raw
// argument text: $1..$n (1-based, empty when absent), $@ (all arguments),
// $@[start] and $@[start:length] (1-based, clamped, out-of-range → empty),
// and $ARGUMENTS (the raw argument text verbatim). When the template contains
// no placeholder at all, the raw args are appended as a fallback separated by
// a blank line, and an empty raw argument string appends nothing.
//
// The scan is a single left-to-right pass over the template: substituted
// argument text lands in the output and is never rescanned, so a literal $1
// or $ARGUMENTS inside the raw args stays literal. Templates are not
// shell-expanded — unknown $ sequences pass through untouched.
func ExpandArgs(template, raw string) string {
	if !strings.Contains(template, "$") {
		return appendArgsFallback(template, raw)
	}
	args := SplitArgs(raw)
	var b strings.Builder
	sawPlaceholder := false
	for i := 0; i < len(template); {
		if template[i] != '$' {
			b.WriteByte(template[i])
			i++
			continue
		}
		rest := template[i+1:]
		switch {
		case strings.HasPrefix(rest, "ARGUMENTS") && !wordFollows(rest, "ARGUMENTS"):
			sawPlaceholder = true
			b.WriteString(raw)
			i += len("$ARGUMENTS")
		case strings.HasPrefix(rest, "@["):
			if end := strings.IndexByte(rest, ']'); end >= 0 {
				if out, ok := expandBracket(args, rest[2:end]); ok {
					sawPlaceholder = true
					b.WriteString(out)
					i += end + 2 // '$' + content through ']'
					continue
				}
			}
			b.WriteString("$@") // malformed bracket: literal $@, body flows as text
			i += 2
		case strings.HasPrefix(rest, "@"):
			sawPlaceholder = true
			b.WriteString(strings.Join(args, " "))
			i += 2
		case len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9':
			// Whole digit run is the index: $10 is argument 10, never $1 + "0".
			j := 0
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			sawPlaceholder = true
			if n, err := strconv.Atoi(rest[:j]); err == nil && n >= 1 && n <= len(args) {
				b.WriteString(args[n-1])
			}
			i += 1 + j
		default:
			b.WriteByte('$') // lone $ or unknown $name: literal
			i++
		}
	}
	if !sawPlaceholder {
		return appendArgsFallback(template, raw)
	}
	return b.String()
}

// wordFollows reports whether the text right after the placeholder word is
// an identifier character — i.e. the match is a LONGER word ($ARGUMENTS2),
// not the placeholder itself.
func wordFollows(rest, word string) bool {
	if len(rest) <= len(word) {
		return false
	}
	c := rest[len(word)]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// expandBracket expands $@[start] / $@[start:length] content. Both bounds are
// 1-based and clamped: start past the end yields empty, a length running past
// the end stops at the last argument. It reports ok=false for malformed
// content (non-numeric bounds) so the caller keeps the text literal.
func expandBracket(args []string, content string) (string, bool) {
	startStr, lenStr := content, ""
	hasLen := false
	if idx := strings.IndexByte(content, ':'); idx >= 0 {
		startStr, lenStr, hasLen = content[:idx], content[idx+1:], true
	}
	start, err := strconv.Atoi(startStr)
	if err != nil || start < 1 {
		return "", false
	}
	first := start - 1
	if first >= len(args) {
		return "", true // out of range: empty, still a real placeholder
	}
	if !hasLen {
		return strings.Join(args[first:], " "), true
	}
	n, err := strconv.Atoi(lenStr)
	if err != nil {
		return "", false
	}
	last := first + n
	if last > len(args) {
		last = len(args)
	}
	return strings.Join(args[first:last], " "), true
}

// appendArgsFallback is the no-placeholder rule: bare templates still receive
// the raw args, after a blank line; empty (or whitespace-only) args append
// nothing.
func appendArgsFallback(template, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return template
	}
	return template + "\n\n" + raw
}
