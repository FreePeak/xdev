package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
)

// runMemoryCLI implements `xdev memory <sub>` (M15 #72: the mnemopi-CLI
// equivalent) over the local backend. Every subcommand is a thin wrapper:
// the files it reads and writes are exactly the ones the agent injects and
// the `learn` tool appends to, so the CLI can never fork the format.
func runMemoryCLI(args []string, settings *config.Settings) int {
	return memoryCmd(args, os.Stdin, os.Stdout, os.Stderr, settings)
}

const memoryUsage = `usage: xdev memory <subcommand>

  show [--injected]        MEMORY.md (--injected: the block the prompt sees)
  stats [--json]           on-disk sizes, lesson count, scratchpad
  lessons [--json] [--limit N]   parsed learned.md entries (oldest first)
  add [--context C] <text|->     append a lesson ("-" reads stdin)
  edit -                   replace MEMORY.md from stdin
  export [-o file]         JSON bundle to stdout or file
  import [--merge] <file|->      restore a bundle (--merge: append new lessons)
  scratchpad [--clear|<text|->]  bounded scratch file (16 KiB cap)
  clear --yes              delete MEMORY.md, learned.md and the scratchpad
`

func memoryCmd(args []string, in io.Reader, out, errOut io.Writer, settings *config.Settings) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	rest := args[1:]
	switch sub {
	case "", "help", "-h", "--help":
		fmt.Fprint(errOut, memoryUsage)
		if sub == "" {
			return 2
		}
		return 0
	}
	b, err := memoryCLIBackend(settings)
	if err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 2
	}
	switch sub {
	case "show":
		return memoryShow(b, rest, out, errOut)
	case "stats":
		return memoryStats(b, rest, settings, out, errOut)
	case "lessons":
		return memoryLessons(b, rest, out, errOut)
	case "add":
		return memoryAdd(b, rest, in, out, errOut)
	case "edit":
		return memoryEdit(b, rest, in, out, errOut)
	case "export":
		return memoryExport(b, rest, out, errOut)
	case "import":
		return memoryImport(b, rest, in, out, errOut)
	case "scratchpad":
		return memoryScratchpad(b, rest, in, out, errOut)
	case "clear":
		return memoryClear(b, rest, out, errOut)
	}
	fmt.Fprintf(errOut, "xdev memory: unknown subcommand %q\n\n%s", sub, memoryUsage)
	return 2
}

// memoryCLIBackend resolves the same directory buildMemory uses. When the
// backend is off in settings and nothing is stored yet there is nothing
// useful to show, so the command explains the switch instead of silently
// creating a store the agent would never read.
func memoryCLIBackend(settings *config.Settings) (*memory.Backend, error) {
	dir := filepath.Join(config.DataDir(), "memory")
	// This CLI reads and writes MEMORY.md/learned.md, which only the markdown
	// backend keeps. A store that lives elsewhere has no files to print, and
	// saying "memory is off" about it would be a lie: name its own surface.
	switch {
	case settings != nil && settings.Memory == "mnemopi":
		return nil, fmt.Errorf("the mnemopi backend keeps its facts in SQLite, not in %s — use /memory view|stats|queue|sync|enqueue in the TUI", dir)
	case settings != nil && settings.Memory == "hindsight":
		return nil, fmt.Errorf("the hindsight backend keeps its memories on the server — use /memory view|stats|diagnose|enqueue in the TUI")
	}
	on := settings != nil && settings.Memory == "local"
	if !on {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("memory is off — enable it with `xdev config set memory local` (or mnemopi|hindsight) (nothing stored at %s)", dir)
		}
	}
	return &memory.Backend{Dir: dir}, nil
}

func memoryShow(b *memory.Backend, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory show", flag.ContinueOnError)
	fs.SetOutput(errOut)
	injected := fs.Bool("injected", false, "print the block injected into the system prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *injected {
		s := b.Summary()
		if s == "" {
			fmt.Fprintln(out, "(no memories stored yet)")
			return 0
		}
		fmt.Fprintln(out, s)
		return 0
	}
	summary, _ := b.Paths()
	if summary == "" {
		fmt.Fprintln(errOut, "xdev memory: backend off")
		return 1
	}
	text, err := b.Read("memory://root/MEMORY.md")
	if err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintf(out, "(empty: %s)\n", summary)
		return 0
	}
	fmt.Fprint(out, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(out)
	}
	return 0
}

// memoryStatsJSON is the --json shape (the text path stays Backend.Stats).
type memoryStatsJSON struct {
	Dir        string          `json:"dir"`
	Info       memory.Info     `json:"info"`
	Pipeline   bool            `json:"pipelineOn"`
	SummaryLen int             `json:"summaryChars"`
	Lessons    []memory.Lesson `json:"lessons,omitempty"`
}

func memoryStats(b *memory.Backend, args []string, settings *config.Settings, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory stats", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "machine-readable stats")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	info := b.Info()
	if *asJSON {
		payload := memoryStatsJSON{
			Dir:      info.Dir,
			Info:     info,
			Pipeline: settings != nil && settings.MemoryPipelineOn(),
		}
		if summary, _ := b.Paths(); summary != "" {
			if text, err := b.Read("memory://root/MEMORY.md"); err == nil {
				payload.SummaryLen = len(strings.TrimSpace(text))
			}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintln(errOut, "xdev memory:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(out, b.Stats())
	fmt.Fprintf(out, "  scratchpad %d B (cap %d B)\n", info.Scratchpad.Bytes, info.ScratchpadCap)
	if settings != nil && settings.MemoryPipelineOn() {
		fmt.Fprintln(out, "  pipeline: on (two-phase consolidation)")
	} else {
		fmt.Fprintln(out, "  pipeline: off")
	}
	return 0
}

func memoryLessons(b *memory.Backend, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory lessons", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "machine-readable lessons")
	limit := fs.Int("limit", 0, "show only the newest N lessons")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	lessons := b.Lessons()
	if *limit > 0 && len(lessons) > *limit {
		lessons = lessons[len(lessons)-*limit:]
	}
	if *asJSON {
		if lessons == nil {
			lessons = []memory.Lesson{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(lessons); err != nil {
			fmt.Fprintln(errOut, "xdev memory:", err)
			return 1
		}
		return 0
	}
	if len(lessons) == 0 {
		fmt.Fprintln(out, "(no lessons stored yet)")
		return 0
	}
	for i, l := range lessons {
		fmt.Fprintf(out, "%4d. %s %s", i+1, l.Date, l.Text)
		if l.Context != "" {
			fmt.Fprintf(out, " (context: %s)", l.Context)
		}
		fmt.Fprintln(out)
	}
	return 0
}

func memoryAdd(b *memory.Backend, args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory add", flag.ContinueOnError)
	fs.SetOutput(errOut)
	contextFlag := fs.String("context", "", "free-form context stored with the lesson")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Go's flag package stops at the first positional, so `add "text"
	// --context x` would swallow the flag into the text. Lift it back out.
	operands, tailContext := liftValueFlag(fs.Args(), "context")
	ctx := *contextFlag
	if ctx == "" {
		ctx = tailContext
	}
	text, code := textArg(operands, in, errOut, "add")
	if code != 0 {
		return code
	}
	before := len(b.Lessons())
	if err := b.SaveLesson(text, ctx); err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	fmt.Fprintf(out, "added lesson %d of %d\n", before+1, len(b.Lessons()))
	return 0
}

// liftValueFlag removes a "--name value" / "--name=value" pair from a token
// list and returns the value (last one wins).
func liftValueFlag(args []string, name string) ([]string, string) {
	var rest []string
	var value string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--"+name || a == "-"+name:
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--"+name+"="):
			value = strings.TrimPrefix(a, "--"+name+"=")
		case strings.HasPrefix(a, "-"+name+"="):
			value = strings.TrimPrefix(a, "-"+name+"=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, value
}

func memoryEdit(b *memory.Backend, args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory edit", flag.ContinueOnError)
	fs.SetOutput(errOut)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	body, err := io.ReadAll(in)
	if err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		fmt.Fprintln(errOut, "xdev memory: refusing to replace MEMORY.md with empty input (use `clear --yes` to delete it)")
		return 1
	}
	if err := b.WriteSummary(string(body)); err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	summary, _ := b.Paths()
	fmt.Fprintf(out, "MEMORY.md updated (%s, %d bytes)\n", summary, len(strings.TrimSpace(string(body))))
	return 0
}

func memoryExport(b *memory.Backend, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory export", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dest := fs.String("o", "", "write the bundle to this file (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bundle, err := b.Export()
	if err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	blob, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	blob = append(blob, '\n')
	if *dest == "" {
		out.Write(blob)
		return 0
	}
	if err := os.WriteFile(*dest, blob, 0o600); err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	fmt.Fprintf(out, "exported %d lesson(s) to %s\n", len(b.Lessons()), *dest)
	return 0
}

func memoryImport(b *memory.Backend, args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory import", flag.ContinueOnError)
	fs.SetOutput(errOut)
	merge := fs.Bool("merge", false, "keep the existing summary and append only new lessons")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(errOut, "usage: xdev memory import [--merge] <file|->")
		return 2
	}
	var blob []byte
	var err error
	if rest[0] == "-" {
		blob, err = io.ReadAll(in)
	} else {
		blob, err = os.ReadFile(rest[0])
	}
	if err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	var bundle memory.Bundle
	if err := json.Unmarshal(blob, &bundle); err != nil {
		fmt.Fprintln(errOut, "xdev memory: not a memory bundle:", err)
		return 1
	}
	if err := b.Import(bundle, *merge); err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	info := b.Info()
	mode := "replaced"
	if *merge {
		mode = "merged"
	}
	fmt.Fprintf(out, "%s: summary %d B, %d lesson(s), scratchpad %d B\n",
		mode, info.Summary.Bytes, info.LessonCount, info.Scratchpad.Bytes)
	return 0
}

func memoryScratchpad(b *memory.Backend, args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory scratchpad", flag.ContinueOnError)
	fs.SetOutput(errOut)
	clear := fs.Bool("clear", false, "delete the scratch file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *clear {
		if err := b.ClearScratchpad(); err != nil {
			fmt.Fprintln(errOut, "xdev memory:", err)
			return 1
		}
		fmt.Fprintln(out, "scratchpad cleared")
		return 0
	}
	if rest := fs.Args(); len(rest) == 0 {
		text := b.Scratchpad()
		if strings.TrimSpace(text) == "" {
			fmt.Fprintf(out, "(scratchpad is empty: %s)\n", b.ScratchpadPath())
			return 0
		}
		fmt.Fprint(out, text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(out)
		}
		return 0
	}
	text, code := textArg(fs.Args(), in, errOut, "scratchpad")
	if code != 0 {
		return code
	}
	if err := b.SetScratchpad(text); err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	fmt.Fprintf(out, "scratchpad set (%d bytes of %d)\n", len(text), memory.DefaultScratchpadCap)
	return 0
}

func memoryClear(b *memory.Backend, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("memory clear", flag.ContinueOnError)
	fs.SetOutput(errOut)
	yes := fs.Bool("yes", false, "confirm deletion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	summary, lessons := b.Paths()
	if !*yes {
		fmt.Fprintf(errOut, "xdev memory clear: this deletes %s, %s and the scratchpad — re-run with --yes\n", summary, lessons)
		return 2
	}
	if err := b.Clear(); err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	if err := b.ClearScratchpad(); err != nil {
		fmt.Fprintln(errOut, "xdev memory:", err)
		return 1
	}
	fmt.Fprintf(out, "cleared %s\n", b.Dir)
	return 0
}

// textArg resolves the text operand of add/scratchpad: an explicit argument
// list, or "-" meaning stdin.
func textArg(args []string, in io.Reader, errOut io.Writer, sub string) (string, int) {
	if len(args) == 0 {
		fmt.Fprintf(errOut, "usage: xdev memory %s <text|->\n", sub)
		return "", 2
	}
	if len(args) == 1 && args[0] == "-" {
		blob, err := io.ReadAll(in)
		if err != nil {
			fmt.Fprintln(errOut, "xdev memory:", err)
			return "", 1
		}
		text := strings.TrimSpace(string(blob))
		if text == "" {
			fmt.Fprintln(errOut, "xdev memory: no input on stdin")
			return "", 1
		}
		return text, 0
	}
	return strings.Join(args, " "), 0
}
