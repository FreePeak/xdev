package tui

// Syntax highlighting for fenced code blocks (#501).
//
// The 9 `syntax_*` theme tokens shipped with the theme contract and had no
// consumer: a fenced block painted as ONE run in the body ink, so a comment,
// a string and a keyword were the same colour. This file is the consumer.
//
// The hard constraint is the one the whole repo is built on: no CGO, no new
// dependency. Embedding tree-sitter means CGO plus a grammar per language
// (PRD §3.6 — the same reasoning that keeps `ast_grep` shelling out to the
// `ast-grep` binary), so this is a hand-written scanner instead. It is a
// heuristic, and it is honest about being one:
//
//   - It lexes ONE LINE at a time. A block comment opened on line 1 and
//     closed on line 4 is painted in four pieces, three of them plain, because
//     the renderer hands out one line at a time and the state that would fix
//     it does not exist yet. A mis-token paints the wrong COLOUR; it never
//     drops, reorders or rewrites a byte — runsString(runs) is byte-identical
//     to the source line, which is the property the selection/copy path and
//     the transcript depend on.
//   - It colours comments, strings, numbers, keywords, built-in types,
//     variables, calls and punctuation. It does not parse; a false negative
//     is a plain run, which is what the block looked like anyway.
//
// The fallback is the part that matters most. A block whose language is
// unknown, untagged, or a dialect this scanner does not know paints exactly
// as it did before this file existed — one run, body ink, md_code_bg band. So
// does NO_COLOR, and so does a theme that leaves the syntax_* slots to the
// terminal default. Highlighting that cannot be right is not worth the risk of
// being wrong.

import (
	"os"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// tokKind is the semantic class a lexed run maps onto. Each kind names one
// theme slot; a kind the theme did not colour falls back to the body ink.
type tokKind uint8

const (
	tokPlain tokKind = iota
	tokComment
	tokString
	tokNumber
	tokKeyword
	tokType
	tokVariable
	tokFunc
	tokOperator
	tokPunct
)

// codeStyle is the resolved syntax palette: one colour per token kind, with
// present recording which kinds the theme actually pinned.
type codeStyle struct {
	plain   tcell.Style
	kinds   [tokPunct + 1]tcell.Color
	present [tokPunct + 1]bool
	// enabled is false when the theme pins no syntax colour at all — the
	// NO_COLOR / all-defaults / unknown-theme case, where the whole pass is
	// skipped and every block paints flat.
	enabled bool
}

func (cs codeStyle) styleFor(k tokKind) tcell.Style {
	if k == tokPlain || !cs.enabled || !cs.present[k] {
		return cs.plain
	}
	return tcell.StyleDefault.Foreground(cs.kinds[k])
}

// syntaxSlots maps every token kind to the theme slot that colours it. This
// is the whole of the 9-token contract: the reason they were required.
var syntaxSlots = [tokPunct + 1]string{
	tokComment:  theme.SyntaxComment,
	tokString:   theme.SyntaxString,
	tokNumber:   theme.SyntaxNumber,
	tokKeyword:  theme.SyntaxKeyword,
	tokType:     theme.SyntaxType,
	tokVariable: theme.SyntaxVariable,
	tokFunc:     theme.SyntaxFunction,
	tokOperator: theme.SyntaxOperator,
	tokPunct:    theme.SyntaxPunctuation,
}

// codeStyleFor resolves the syntax palette from the theme. A kind counts as
// present only when the theme pins an explicit colour (Slot, not Get): a slot
// left "" means "the terminal's own ink", and a token coloured from a palette
// value the theme declined to paint is the red-on-red failure the diff
// renderer already refused. A theme may fill as many of the 9 as it likes; the
// rest fall back per kind.
func (a *App) codeStyleFor() codeStyle {
	cs := codeStyle{plain: tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary)))}
	// NO_COLOR is the one global off switch, and it is read HERE rather than
	// left to tcell. tcell drops colour at emission, so a run that picked an
	// ink would still LOOK highlighted in a cell dump and in any tool that
	// reads the grid, while the terminal shows one ink. NO_COLOR means "no
	// colour output at all", so the renderer must not choose inks it will be
	// forbidden to emit.
	if os.Getenv("NO_COLOR") != "" {
		return cs
	}
	for kind, name := range syntaxSlots {
		if name == "" {
			continue
		}
		c, ok := a.th.Slot(name)
		if !ok {
			continue
		}
		cs.kinds[kind] = a.cellColor(c)
		cs.present[kind] = true
		cs.enabled = true
	}
	return cs
}

// langSpec is one language's lexing rules. All of it is per-line and
// stateless, so a spec is a few sets and some delimiters.
type langSpec struct {
	keywords map[string]bool
	// types are names that read as types without being keywords (`string` in
	// Go, `int` in Python). They ride the tokType slot.
	types map[string]bool
	// lineComments are the prefixes that open a to-end-of-line comment.
	lineComments []string
	// blockPairs are the open/close delimiters of a block comment, each pair
	// written as one string (`/* */`). Block comments are NOT carried across
	// lines; see the file comment.
	blockPairs [][2]string
	// strings are the quote characters; raw is the quote that opens a raw
	// string (Go backticks, JS template literals), if the language has one;
	// escape is the escape character, 0 when the language has none.
	strings string
	raw     string
	escape  byte
	// numbers gates digit-run colouring, calls colours identifier-followed-
	// by-paren, and variables colours a bare identifier.
	numbers   bool
	calls     bool
	variables bool
}

func wordSet(words string) map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// blockSpecs parses `"/* */"` into open/close pairs.
func blockSpecs(spec string) [][2]string {
	var out [][2]string
	for _, p := range strings.Fields(spec) {
		if len(p) >= 4 {
			out = append(out, [2]string{p[:2], p[2:]})
		}
	}
	return out
}

// cLike is the C-family shape: // and /* */, " and ' strings, backslash
// escapes, digit runs, identifier-then-paren as a call, bare identifier as a
// variable.
func cLike(kw, ty, line, block string) langSpec {
	return langSpec{
		keywords:     wordSet(kw),
		types:        wordSet(ty),
		lineComments: strings.Fields(line),
		blockPairs:   blockSpecs(block),
		strings:      "\"'",
		escape:       '\\',
		numbers:      true,
		calls:        true,
		variables:    true,
	}
}

// cLikeRaw adds a backtick raw string, the way Go and the JS family have one.
func cLikeRaw(sp langSpec) langSpec {
	sp.raw = "`"
	sp.strings = "\"'"
	return sp
}

// codeLangs maps a normalized fence info string to its lexer. The spellings
// here are what markdown authors actually write; an absent key is the flat
// fallback, not an error.
var codeLangs = map[string]langSpec{
	"go": cLikeRaw(cLike(
		"break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var",
		"bool byte complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr any comparable",
		"//", "/* */")),
	"rust": cLike(
		"as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while",
		"bool char f32 f64 i8 i16 i32 i64 i128 isize str u8 u16 u32 u64 u128 usize String Vec Option Result Box",
		"//", "/* */"),
	"c": cLike(
		"auto break case const continue default do else enum extern for goto if inline register restrict return sizeof static struct switch typedef union volatile while",
		"bool char double float int long short signed size_t unsigned void FILE va_list va_start va_end",
		"//", "/* */"),
	"cpp": cLike(
		"alignas alignof auto bool break case catch class const constexpr continue default delete do double dynamic_cast else enum explicit extern false float for friend goto if inline int long mutable namespace new noexcept nullptr operator private protected public register return short signed sizeof static struct switch template this throw true try typedef typename union unsigned using virtual void volatile while",
		"size_t string wchar_t uint8_t uint16_t uint32_t uint64_t int8_t int16_t int32_t int64_t",
		"//", "/* */"),
	"java": cLike(
		"abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static strictfp super switch synchronized this throw throws transient try void volatile while true false null",
		"byte char double float int long short void String Integer Long Boolean Double List Map Set",
		"//", "/* */"),
	"javascript": cLikeRaw(cLike(
		"async await break case catch class const continue debugger default delete do else export extends false finally for function get if import in instanceof let new null of return set static super switch this throw true try typeof undefined var void while with yield",
		"Array Boolean Date Error Function JSON Map Math Number Object Promise RegExp Set String Symbol WeakMap WeakSet",
		"//", "/* */")),
	"typescript": cLikeRaw(cLike(
		"abstract any as async await boolean break case catch class const constructor continue declare debugger default delete do else enum export extends false finally for from function get if implements import in infer instanceof interface is keyof let module namespace never new null number object of override private protected public readonly return satisfies set static string super switch symbol this throw true try type typeof undefined unique unknown var void while yield",
		"any bigint boolean never number object string symbol unknown void Array Boolean Date Error Function JSON Map Number Object Promise Record RegExp Set String",
		"//", "/* */")),
	"c#": cLike(
		"abstract as base break case catch checked class const continue default delegate do else enum event explicit extern false finally fixed for foreach goto if implicit in interface internal is lock namespace new null operator out override params private protected public readonly ref return sealed sizeof stackalloc static struct switch this throw true try typeof unchecked unsafe using var virtual void volatile while",
		"bool byte char decimal double float int long object sbyte short string uint ulong ushort",
		"//", "/* */"),
	"swift": cLike(
		"associatedtype class deinit enum extension fileprivate func import init inout internal let open operator private protocol public rethrows static struct subscript typealias var break case continue default defer do else fallthrough for guard if in repeat return switch where while as catch is nil super self Self throw throws true false try",
		"Any Bool Character Double Float Int String UInt Void Array Dictionary Optional Set",
		"//", "/* */"),
	"kotlin": cLike(
		"as break by catch class companion const constructor continue crossinline do dynamic else enum external false finally for fun get if import in infix init inline inner interface internal is lateinit noinline object open operator out override package private protected public reified return sealed set super suspend this throw true try typealias val var vararg when where while",
		"Any Boolean Byte Char Double Float Int Long Nothing Short String Unit Array List Map Set",
		"//", "/* */"),
	"php": cLike(
		"abstract and array as break callable case catch class clone const continue declare default do echo else elseif empty enddeclare endfor endforeach endif endswitch endwhile enum extends final finally fn for foreach function global goto if implements include include_once instanceof insteadof interface isset list match namespace new or print private protected public readonly require require_once return static switch throw trait try unset use var while xor yield true false null",
		"bool float int iterable mixed never object string void array",
		"//", "/* */"),
	"dart": cLike(
		"abstract as assert async await break case catch class const continue covariant default deferred do dynamic else enum export extends extension external factory false final finally for get hide if implements import in interface is late library mixin new null on operator part required rethrow return set show static super switch sync this throw true try typedef var void while with yield",
		"bool double int num String List Map Set Future Stream",
		"//", "/* */"),
	"zig": cLike(
		"addrspace align allowzero and anyframe anytype asm async await break callconv catch comptime const continue defer else enum errdefer error export extern fn for if inline linksection noalias nosuspend opaque or orelse packed pub resume return struct suspend switch test threadlocal try union unreachable usingnamespace var volatile while",
		"anyerror bool f16 f32 f64 i8 i16 i32 i64 i128 isize noreturn type usize void u8 u16 u32 u64 u128",
		"//", "/* */"),
	"nim": cLike(
		"addr and as asm bind block break case cast concept const continue converter defer discard distinct div do elif else end enum except export finally for from func generic if import in include interface is isnot iterator let macro method mixin mod nil not notin object of or out proc ptr raise ref return shl shr static template try tuple type using var when while xor yield",
		"int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 float float32 float64 bool char string seq array set table",
		"#", "#[ ]#"),
	"haskell": langSpec{
		keywords: wordSet("case class data default deriving do else foreign if import in infix infixl infixr instance let module newtype of then type where"),
		types:    wordSet("Bool Char Double Either Float Int Integer Maybe Ordering Rational String"),
		// A double dash is only a comment marker at a token boundary in
		// Haskell; the scanner enforces that, so `f--1` stays arithmetic.
		lineComments: []string{"--"},
		blockPairs:   [][2]string{{"{-", "-}"}},
		strings:      "\"'",
		numbers:      true,
		variables:    true,
	},
	"lua": langSpec{
		keywords:     wordSet("and break do else elseif end false for function goto if in local nil not or repeat return then true until while"),
		types:        wordSet("string number table boolean thread userdata io math os coroutine"),
		lineComments: []string{"--"},
		blockPairs:   [][2]string{{"--[[", "]]"}},
		strings:      "\"'",
		numbers:      true,
		variables:    true,
	},
	"sql": cLike(
		"ADD ALL ALTER AND AS ASC BEGIN BETWEEN BY CASE CREATE DELETE DESC DISTINCT DROP ELSE END EXISTS FROM FULL GROUP HAVING IN INDEX INNER INSERT INTO IS JOIN LEFT LIKE LIMIT NOT NULL ON OR ORDER OUTER PRIMARY REFERENCES RIGHT SELECT SET TABLE THEN UNION UPDATE VALUES VIEW WHEN WHERE",
		"int integer bigint smallint decimal numeric float real double char varchar text date datetime timestamp boolean blob",
		"--", "/* */"),
	"python": langSpec{
		keywords:     wordSet("and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match case"),
		types:        wordSet("bool bytes complex dict float frozenset int list object set str tuple type"),
		lineComments: []string{"#"},
		strings:      "\"'",
		numbers:      true,
		calls:        true,
		variables:    true,
	},
	"ruby": langSpec{
		keywords: wordSet("alias and begin break case class def defined do else elsif end ensure false for if in module next nil not or redo rescue retry return self super then true undef unless until when while yield require require_relative include extend lambda proc attr_accessor attr_reader attr_writer"),
		types:    wordSet("Array Hash Integer Float String Symbol Range Struct"),
		// Ruby comments are '#', but a '#' inside a word is a method or a
		// label; atWordStart is what keeps `h#fetch` from going grey.
		lineComments: []string{"#"},
		strings:      "\"'",
		numbers:      true,
		calls:        true,
		variables:    true,
	},
	"shell": langSpec{
		keywords:     wordSet("if then else elif fi case esac for while until do done in function select time return exit local export unset break continue shift source alias declare readonly typeset trap eval exec set"),
		lineComments: []string{"#"},
		strings:      "\"'`",
		escape:       '\\',
		numbers:      true,
		variables:    true,
	},
	"makefile": langSpec{
		keywords:     wordSet("ifeq ifneq ifdef ifndef else endif include define endef export unexport override vpath .PHONY"),
		lineComments: []string{"#"},
		strings:      "\"'",
		escape:       '\\',
		variables:    true,
	},
	"dockerfile": langSpec{
		keywords:     wordSet("FROM RUN CMD LABEL MAINTAINER EXPOSE ENV ADD COPY ENTRYPOINT VOLUME USER WORKDIR ARG ONBUILD STOPSIGNAL HEALTHCHECK SHELL AS"),
		lineComments: []string{"#"},
		strings:      "\"'`",
		escape:       '\\',
		variables:    true,
	},
	"yaml": langSpec{
		keywords:     wordSet("true false null yes no on off"),
		lineComments: []string{"#"},
		strings:      "\"'",
	},
	"toml": langSpec{
		keywords:     wordSet("true false"),
		lineComments: []string{"#"},
		strings:      "\"'",
	},
	"ini": langSpec{
		keywords:     wordSet("true false"),
		lineComments: []string{"#", ";"},
		strings:      "\"'",
	},
	"graphql": cLike(
		"query mutation subscription fragment on type interface union enum input schema scalar directive extend implements",
		"Int Float String Boolean ID", "#", ""),
	"proto": cLike(
		"syntax package import option message enum service rpc returns oneof repeated optional required stream",
		"bool bytes double float int32 int64 sint32 sint64 string uint32 uint64", "//", "/* */"),
	"vue": cLike(
		"import export from as const let var function return if else for while do try catch finally throw new class extends this props data computed watch methods setup reactive ref",
		"string number boolean object array null undefined", "//", "/* */"),
	"svelte": cLike(
		"import export from as const let var function return if else for while do try catch finally throw new class extends this props data reactive onMount onDestroy",
		"string number boolean object array null undefined", "//", "/* */"),
}

// --- the lexer ---

// highlightCode splits one line of code into styled runs. It returns nil when
// nothing can be coloured — unknown language, no palette, or a line that lexes
// to a single plain run — and nil is the caller's signal to paint the flat
// body-ink row, which is byte-for-byte today's behaviour.
func (a *App) highlightCode(raw, info string, cs codeStyle) []cell {
	if !cs.enabled {
		return nil
	}
	spec, ok := codeLangs[normalizeLang(info)]
	if !ok {
		return nil
	}
	toks := lexLine(raw, spec)
	if len(toks) == 0 {
		return nil
	}
	coloured := 0
	for _, t := range toks {
		if t.kind != tokPlain {
			coloured++
		}
	}
	if coloured == 0 {
		return nil
	}
	// Coalesce adjacent same-kind tokens, drop empty runs, and never split
	// the line: the concatenation is the source line byte for byte.
	runs := make([]cell, 0, len(toks))
	for _, t := range toks {
		if t.text == "" {
			continue
		}
		st := cs.styleFor(t.kind)
		if n := len(runs) - 1; n >= 0 && runs[n].style == st {
			runs[n].text += t.text
			continue
		}
		runs = append(runs, cell{text: t.text, style: st})
	}
	return runs
}

// normalizeLang folds a fence info string to a lookup key: the first word
// (```go, ```go linenums), lowercased, with the common aliases collapsed. A
// fence with no tag returns "" — the flat fallback. `text`, `txt`, `output`
// and `log` are explicit non-highlighted tags: an author who wrote one means
// "this is prose, do not colour it".
func normalizeLang(info string) string {
	lang := strings.ToLower(strings.TrimSpace(info))
	if i := strings.IndexAny(lang, " \t{,"); i >= 0 {
		lang = lang[:i]
	}
	switch lang {
	case "", "text", "txt", "plain", "plaintext", "output", "log", "none":
		return ""
	case "sh", "shell", "bash", "zsh", "fish", "console":
		return "shell"
	case "py", "python", "python3":
		return "python"
	case "js", "javascript", "node":
		return "javascript"
	case "ts", "typescript":
		return "typescript"
	case "golang":
		return "go"
	case "rs":
		return "rust"
	case "yml":
		return "yaml"
	case "make", "mk":
		return "makefile"
	case "docker":
		return "dockerfile"
	case "kt":
		return "kotlin"
	case "rb":
		return "ruby"
	case "hs":
		return "haskell"
	case "cs", "csharp":
		return "c#"
	}
	return lang
}

// token is one lexed span. text is the slice itself rather than an offset, so
// the runs concatenate back to the input exactly.
type token struct {
	kind tokKind
	text string
}

// lexLine tokenizes one line. It is a scanner, not a parser: comments,
// strings, numbers, then identifiers/keywords/operators. The order IS the
// contract — a '#' inside a string is part of the string because the string is
// scanned first, and prose inside a comment stays a comment.
func lexLine(s string, sp langSpec) []token {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		// line comment (a marker mid-identifier is not a comment: `a#b` in
		// Ruby, `x--1` in Haskell)
		case hasPrefixAt(s, i, sp.lineComments) && atWordStart(s, i):
			out = append(out, token{kind: tokComment, text: s[i:]})
			return out
		// block comment, same line only
		case sp.opensBlock(s, i):
			open, close := sp.blockAt(s, i)
			end := i + len(open)
			if j := strings.Index(s[end:], close); j >= 0 {
				end += j + len(close)
			} else {
				end = len(s) // unterminated: to EOL, never past it
			}
			out = append(out, token{kind: tokComment, text: s[i:end]})
			i = end
			continue
		// raw string: no escapes, runs to EOL
		case sp.raw != "" && c == sp.raw[0]:
			out = append(out, token{kind: tokString, text: s[i:]})
			return out
		// quoted string
		case strings.IndexByte(sp.strings, c) >= 0:
			end := scanString(s, i, c, sp.escape)
			out = append(out, token{kind: tokString, text: s[i:end]})
			i = end
			continue
		// number (not the tail of an identifier, so `x2` and `b0` stay one)
		case sp.numbers && isDigit(c) && !isIdentByte(byteAt(s, i-1)):
			end := scanNumber(s, i)
			out = append(out, token{kind: tokNumber, text: s[i:end]})
			i = end
			continue
		// identifier / keyword / type / call / variable
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			word := s[i:j]
			kind := tokVariable
			switch {
			case sp.keywords[word]:
				kind = tokKeyword
			case sp.types[word]:
				kind = tokType
			case sp.calls && nextNonSpace(s, j) == '(':
				kind = tokFunc
			}
			if kind == tokVariable && !sp.variables {
				kind = tokPlain
			}
			out = append(out, token{kind: kind, text: word})
			i = j
			continue
		// operator / punctuation / anything else: one byte at minimum, so
		// the scanner always advances
		default:
			j := i + 1
			for j < len(s) && isOpByte(s[j]) {
				j++
			}
			kind := tokPunct
			if isAlphaByte(s[i]) {
				kind = tokOperator
			}
			out = append(out, token{kind: kind, text: s[i:j]})
			i = j
			continue
		}
	}
	return out
}

// scanString returns the end offset of a quoted string opening at i. An
// unterminated string runs to end of line — the same honest degradation as an
// unterminated comment, and the reason a broken string never eats the rest of
// the transcript.
func scanString(s string, i int, quote, escape byte) int {
	for j := i + 1; j < len(s); j++ {
		if escape != 0 && s[j] == escape {
			j++
			continue
		}
		if s[j] == quote {
			return j + 1
		}
	}
	return len(s)
}

// scanNumber runs the digit/letter soup: 42, 3.14, 1e-9, 0x1f, 0b1010, 1_000.
func scanNumber(s string, i int) int {
	j := i
	for j < len(s) {
		c := s[j]
		switch {
		case isDigit(c), c == '_', c == '.':
			j++
		case isAlphaByte(c) || c == 'x' || c == 'X' || c == 'b' || c == 'B' ||
			c == 'o' || c == 'O' || c == 'e' || c == 'E' || c == 'd' || c == 'D' ||
			c == 'f' || c == 'F' || c == 'u' || c == 'U' || c == 'L':
			// An exponent sign belongs to the number; a plus after a letter
			// does not (`0x1f+2` is a number then an operator).
			j++
		case (c == '+' || c == '-') && j > i && (s[j-1] == 'e' || s[j-1] == 'E'):
			j++
		default:
			return j
		}
	}
	return j
}

func (sp langSpec) opensBlock(s string, i int) bool {
	open, _ := sp.blockAt(s, i)
	return open != ""
}

func (sp langSpec) blockAt(s string, i int) (open, close string) {
	for _, p := range sp.blockPairs {
		if strings.HasPrefix(s[i:], p[0]) {
			return p[0], p[1]
		}
	}
	return "", ""
}

func hasPrefixAt(s string, i int, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(s[i:], p) {
			return true
		}
	}
	return false
}

// atWordStart reports whether i begins a word, so a comment marker only opens
// a comment at a token boundary. Offset 0 counts, so a shebang is a comment.
func atWordStart(s string, i int) bool {
	return i == 0 || !isIdentByte(s[i-1])
}

// byteAt is s[i] for i in range, else 0 — the "previous byte" that stops a
// number from swallowing the tail of an identifier (x2, b0, foo42).
func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlphaByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentStart(c byte) bool { return isAlphaByte(c) || c == '_' || c == '$' }

func isIdentByte(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isOpByte(c byte) bool {
	return strings.IndexByte("+-*/%<>=!&|^~?:.,;@#\\", c) >= 0
}

func nextNonSpace(s string, i int) byte {
	for ; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return s[i]
		}
	}
	return 0
}
