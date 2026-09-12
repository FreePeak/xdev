package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/serve"
)

// psHostProcs is the one host-dependent seam: it reads the host process table
// as `ps -axo pid=,ppid=,etime=,stat=,command=` would print it. Tests replace
// it so they never run the real `ps`; production points at psHostProcsReal.
var psHostProcs = psHostProcsReal

// psHostProcsReal probes the host. Discrete argv (no shell) keeps the
// invocation honest; the empty column headers (`pid=`) keep the output
// parseable without a header skip, and the trailing `command=` column absorbs
// the rest of the line so argument lists with spaces survive.
//
// ponytail: this is BSD `ps` syntax, so `xdev ps` is a POSIX-host command —
// on Windows the probe fails and the command exits 1 rather than pretending
// nothing is running. Upgrade path: a Windows process-enumeration path behind
// this same psHostProcs seam.
func psHostProcsReal() (string, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,etime=,stat=,command=").Output()
	if err != nil {
		return "", fmt.Errorf("ps -axo: %w", err)
	}
	return string(out), nil
}

// psRow is one xdev process, shared by the table and the --json view so the
// two can never disagree about what was parsed.
type psRow struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Elapsed string `json:"elapsed"`
	// Kind is the coarse role: daemon | interactive | wire | <subcommand> |
	// (bare) | (prompt).
	Kind    string `json:"kind"`
	Command string `json:"command"`

	// Daemon facts (serve rows only): the service's default listen address
	// and whether its per-install token file exists. Empty/false elsewhere,
	// which omits them from JSON instead of padding every row.
	Service     string `json:"service,omitempty"`
	Listen      string `json:"listen,omitempty"`
	TokenMinted bool   `json:"token_minted,omitempty"`
}

// psKind labels the roles worth naming; everything else is labelled with its
// literal subcommand so a new mode shows up without editing this file.
const (
	psKindDaemon      = "daemon"
	psKindInteractive = "interactive"
	psKindWire        = "wire"
	// psBareArg and psPrompt label the two invocations that carry no
	// subcommand: idle `xdev` (the TUI it opens) and `xdev "prompt"` /
	// piped stdin (a one-shot print run).
	psBareArg = "(bare)"
	psPrompt  = "(prompt)"
)

// psCommandWidth bounds the COMMAND cell. The full command line is still in
// --json, so the cap costs nothing an operator cannot recover.
const psCommandWidth = 72

// psSubcommands is the set of dispatching names xdev actually routes (main.go
// `subcommands` plus the run modes). It exists only to tell "this is a
// subcommand" from "this is a prompt": the first argument of a print run is
// arbitrary user text, so classifying it by name alone would label a session
// after a word from its prompt. A subcommand added to main.go but missing here
// degrades to (prompt) — wrong, but never a crash or a leak.
var psSubcommands = map[string]bool{
	"print": true, "tui": true, "rpc": true, "acp": true, "config": true,
	"lsp-config": true, "say": true, "plugin": true, "join": true,
	"login": true, "logout": true, "version": true, "serve": true,
	"stats": true, "memory": true, "share": true, "ps": true,
	"update": true, "setup": true, "bench": true,
}

// runPS implements `xdev ps`: which xdev processes are on this host, and an
// honest statement of what this process cannot see.
func runPS(args []string) int {
	return psCmd(args, os.Stdout, os.Stderr)
}

func psCmd(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "print the parsed rows as JSON (the informational note goes to stderr)")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev ps [--json]

  xdev ps           xdev processes on this host (table)
  xdev ps --json    the same rows as JSON

Rows come from ps -axo pid=,ppid=,etime=,stat=,command=, filtered to
commands whose first token is this xdev binary; the reading process itself
is skipped. Per-session state — background bash jobs and hub-supervised
child processes — is in-memory state inside the process that owns it and is
never persisted, so it is not visible from a separate invocation; the note
after the table says so instead of reporting an empty list.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintln(errOut, "xdev ps: unexpected argument", rest[0])
		return 2
	}

	raw, err := psHostProcs()
	if err != nil {
		fmt.Fprintln(errOut, "xdev ps:", err)
		return 1
	}
	dataDir := config.DataDir()
	rows := psParseTable(raw, os.Getpid(), dataDir)

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintln(errOut, "xdev ps:", err)
			return 1
		}
		fmt.Fprint(errOut, psInvisibleNote)
		return 0
	}

	fmt.Fprint(out, psRenderTable(rows, dataDir))
	if len(rows) == 0 {
		fmt.Fprint(out, "\nno xdev process on this host\n")
	} else {
		fmt.Fprintf(out, "\n%d xdev process(es)\n", len(rows))
	}
	// The note goes to stderr so --json stdout stays pipeable; in an
	// interactive terminal both land in the same view.
	fmt.Fprint(errOut, "\n"+psInvisibleNote)
	return 0
}

// psInvisibleNote states the real ceiling as a product fact. Without it a
// listing that shows no background jobs reads as "there are none" rather than
// "they are somewhere this process cannot look".
//
// ponytail: the ceiling is that process-local state is never persisted, so a
// separate CLI process cannot enumerate it. Upgrade path: have each owning
// session write <dataDir>/procs/<pid>.json on start and remove it on exit,
// then list that directory here and drop entries whose PID is no longer alive
// — that makes jobs and supervised children visible across processes while
// leaving the host probe above unchanged.
const psInvisibleNote = `not visible from here: background bash jobs and hub-supervised child
processes are in-memory state inside the xdev process that started them
(tool.SharedBashJobs, internal/agent ProcTable) and are never persisted, so a
standalone xdev ps lists processes, not their sessions. Use the owning
session's /jobs and /ps to see those.
`

// psParseTable turns `ps` output into the xdev rows worth showing, preserving
// host order (ps emits ascending PID, i.e. oldest first, which makes a
// long-running daemon easy to spot).
func psParseTable(raw string, selfPID int, dataDir string) []psRow {
	out := []psRow{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		// pid, ppid, etime, stat, then the command — anything shorter has no
		// command at all and cannot be an xdev process we can classify.
		if len(fields) < 5 {
			continue
		}
		pid, err := psAtoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := psAtoi(fields[1])
		if err != nil {
			continue
		}
		argv := fields[4:]
		// Match by basename so a PATH-resolved `xdev`, an absolute path and a
		// relative ./xdev classify the same way. Skipping self keeps the
		// listing about xdev sessions rather than about the `xdev ps` that is
		// printing it.
		if pid == selfPID || !psIsXdevArgv(argv) {
			continue
		}
		kind, service, listen, hasToken := psClassify(argv, dataDir)
		// strings.Fields already trimmed and normalized the whitespace of the
		// command, so joining it back is the trimmed COMMAND the table wants.
		out = append(out, psRow{
			PID:         pid,
			PPID:        ppid,
			Elapsed:     fields[2],
			Kind:        kind,
			Command:     strings.Join(argv, " "),
			Service:     service,
			Listen:      listen,
			TokenMinted: hasToken,
		})
	}
	return out
}

// psClassify maps a command line to the role it plays and, for `serve`, the
// two facts `xdev ps` can state without asking the daemon: the address the
// service binds by default and whether its per-install token file has been
// minted.
func psClassify(argv []string, dataDir string) (kind, service, listen string, hasToken bool) {
	arg := func(i int) string {
		if i < len(argv) {
			return argv[i]
		}
		return ""
	}
	sub := arg(1)
	switch sub {
	case "serve":
		svc := arg(2)
		if svc == "" {
			// `xdev serve` with no service prints usage and exits; if one is
			// somehow still alive it is a daemon with nothing to report.
			return psKindDaemon, "", "", false
		}
		if !serve.KnownService(svc) {
			// An unknown service name is a usage error, not a daemon; label
			// it rather than inventing a listen address for it.
			return "serve:" + svc, "", "", false
		}
		// One token read answers both facts: a value proves the file exists.
		return psKindDaemon, svc, serve.DefaultListen(svc),
			serve.ReadToken(dataDir, svc) != ""
	case "tui":
		return psKindInteractive, "", "", false
	case "rpc", "acp":
		return psKindWire, "", "", false
	case "":
		// Bare `xdev` opens the TUI; any argument is a print run's prompt.
		if len(argv) == 1 {
			return psBareArg, "", "", false
		}
		return psPrompt, "", "", false
	default:
		if !psSubcommands[sub] {
			// `xdev "fix the tests"`: the first positional is the prompt, not
			// a mode — it is classified (prompt), not labelled by its words.
			return psPrompt, "", "", false
		}
		return sub, "", "", false
	}
}

// psIsXdevArgv reports whether the command's first token names the xdev
// binary. Basename handles `xdev`, `/opt/xdev` and `./xdev`; the explicit
// suffix check keeps a path ending in /xdev working even when the binary was
// invoked through a differently named symlink.
func psIsXdevArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	return psBase(argv[0]) == "xdev" || strings.HasSuffix(argv[0], "/xdev")
}

// psBase is path.Base without importing path for a single call; it never
// touches the filesystem (the process may already be gone).
func psBase(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// psRenderTable renders the fixed-width table plus one detail block per
// daemon. Detail lines exist because a daemon's listen address and token state
// are the two facts an operator actually came for, and folding them into the
// COMMAND cell would push the command out of view.
func psRenderTable(rows []psRow, dataDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-7s %-7s %-10s %-12s %s\n", "PID", "PPID", "ELAPSED", "KIND", "COMMAND")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-7d %-7d %-10s %-12s %s\n",
			r.PID, r.PPID, psTruncate(r.Elapsed, 10), psTruncate(r.Kind, 12),
			psTruncate(r.Command, psCommandWidth))
	}
	// Daemon details after the table keep the row grid readable.
	for _, r := range rows {
		if r.Kind != psKindDaemon || r.Service == "" {
			continue
		}
		fmt.Fprintf(&b, "\n  %s  pid %d\n", r.Service, r.PID)
		fmt.Fprintf(&b, "    listen   %s (default; --listen overrides)\n", r.Listen)
		fmt.Fprintf(&b, "    token    %s\n", psTokenStateLine(dataDir, r))
	}
	return b.String()
}

// psTokenStateLine words the token fact for the detail block. A daemon started
// with --token or XDEV_SERVE_TOKEN holds a value this process cannot observe,
// so an absent file is a statement about the store, not about the daemon.
func psTokenStateLine(dataDir string, r psRow) string {
	if !r.TokenMinted {
		return "token file absent (not minted in this data dir)"
	}
	return "token file present: " + serve.TokenPath(dataDir, r.Service)
}

// psTruncate caps a table cell without splitting a rune. It deliberately
// duplicates stats' truncate: four lines, and sharing it would couple `ps` to
// a command whose notion of width may diverge.
func psTruncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// psAtoi parses a `ps` numeric column. Both pid columns are unsigned, so a
// strict digit loop keeps a garbled line from silently becoming PID 0.
func psAtoi(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a pid: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
