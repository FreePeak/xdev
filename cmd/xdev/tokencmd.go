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
	"github.com/FreePeak/xdev/internal/serve"
)

// runToken implements `xdev token`: inspect and rotate the per-install service
// tokens the loopback daemons (<dataDir>/serve/<service>.token, 0600, minted
// on first use) authenticate with. It reads and writes exactly the files the
// daemons read, so the CLI can never disagree with what a service loads.
func runToken(args []string) int { return tokenCmd(args, config.DataDir(), os.Stdout, os.Stderr) }

// tokenListJSON is the `--json` row shape. The token field is deliberately the
// masked form: list output is the thing people paste into a chat or an issue
// tracker, so the secret never leaves the machine through it (only `show` and
// `rotate` print a full value).
type tokenListJSON struct {
	Service string `json:"service"`
	Listen  string `json:"listen"`
	Path    string `json:"path"`
	Minted  bool   `json:"minted"`
	Token   string `json:"token"`
}

// tokenUsageText builds the help block. dataDir is threaded in because the
// token location is the one thing an operator needs from the help text.
func tokenUsageText(dataDir string) string {
	return "usage: xdev token [verb] [service] [flags]\n\n" +
		"  xdev token                    list every service with listener and token state\n" +
		"  xdev token list               same, spelled out\n" +
		"  xdev token show [service]     print a token in full (every service when omitted)\n" +
		"  xdev token rotate [service]   mint a fresh token (every service when omitted)\n\n" +
		"Services: " + strings.Join(serve.Services(), ", ") + "\n" +
		"Token files: " + tokenDir(dataDir) + "\n\n" +
		"Flags:\n"
}

// tokenDir is the directory holding the per-service token files. It is derived
// from serve.TokenPath rather than re-joined here, so a layout change inside
// internal/serve cannot leave this command pointing at a stale path.
func tokenDir(dataDir string) string {
	services := serve.Services()
	if len(services) == 0 {
		return dataDir
	}
	return filepath.Dir(serve.TokenPath(dataDir, services[0]))
}

// tokenMask renders a secret for listing: enough of the prefix and suffix to
// tell two tokens apart in a table, never enough to use one.
func tokenMask(tok string) string {
	if tok == "" {
		return "-"
	}
	if len(tok) <= 8 {
		return "…"
	}
	return tok[:4] + "…" + tok[len(tok)-4:]
}

// tokenServices is the set a verb acts on: one named service, or all of them
// when the operator named none (or forced --all).
func tokenServices(service string, all bool) []string {
	if all || service == "" {
		return serve.Services()
	}
	return []string{service}
}

// tokenUnknownService reports a bad service name. The known names are part of
// the message because the token table is small and fixed — listing it turns a
// typo into a one-line fix instead of a trip to the docs.
func tokenUnknownService(dataDir, service string, errOut io.Writer) int {
	fmt.Fprintf(errOut, "xdev token: unknown service %q (have: %s)\n\n",
		service, strings.Join(serve.Services(), ", "))
	tokenWriteUsage(dataDir, errOut)
	return 2
}

// Flag help lives in constants because two renderers print it: the FlagSet's
// own PrintDefaults when a parse fails, and tokenWriteUsage for the paths that
// reject an argument before a flag is ever parsed. Drifting copies of the same
// sentence would be the only way for the two help texts to disagree.
const (
	tokenJSONFlagUsage = "print the list as JSON (list only)"
	tokenAllFlagUsage  = "rotate every service (rotate only)"
)

// tokenWriteUsage prints the usage block without a *flag.FlagSet, for the
// paths that reject an argument before a verb ever runs.
func tokenWriteUsage(dataDir string, errOut io.Writer) {
	fmt.Fprint(errOut, tokenUsageText(dataDir))
	fmt.Fprintf(errOut, "  -json\n    \t%s\n", tokenJSONFlagUsage)
	fmt.Fprintf(errOut, "  -all\n    \t%s\n", tokenAllFlagUsage)
}

// tokenTable renders rows with two-space gutters, sized to the widest cell per
// column. ponytail: width is counted in bytes, which is correct for the
// ASCII-only columns here (names, listeners, hex, paths); if a non-ASCII cell
// is ever added, switch to utf8.RuneCountInString for the width probe.
func tokenTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	for _, row := range rows {
		for i, cell := range row {
			b.WriteString(cell)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// tokenList is the default verb: one row per service, secrets masked.
// It only ever reads — listing must not mint a token as a side effect.
func tokenList(dataDir string, asJSON bool, out, errOut io.Writer) int {
	services := serve.Services()
	jsonRows := make([]tokenListJSON, 0, len(services))
	rows := make([][]string, 0, len(services)+1)
	rows = append(rows, []string{"SERVICE", "LISTEN", "STATE", "TOKEN", "PATH"})
	for _, s := range services {
		row := tokenListJSON{
			Service: s,
			Listen:  serve.DefaultListen(s),
			Path:    serve.TokenPath(dataDir, s),
		}
		state, display := "not minted", "-"
		if tok := serve.ReadToken(dataDir, s); tok != "" {
			row.Minted = true
			row.Token = tokenMask(tok)
			state, display = "minted", row.Token
		}
		jsonRows = append(jsonRows, row)
		rows = append(rows, []string{s, row.Listen, state, display, row.Path})
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(jsonRows); err != nil {
			fmt.Fprintln(errOut, "xdev token:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(out, "%s\n\n", tokenDir(dataDir))
	fmt.Fprint(out, tokenTable(rows))
	return 0
}

// tokenShow prints full token values for one service or all of them. It never
// mints: an operator asking to *see* what is configured must not be handed a
// value that only exists because they looked.
func tokenShow(dataDir, service string, out, errOut io.Writer) int {
	services := tokenServices(service, false)
	minted := 0
	for _, s := range services {
		tok := serve.ReadToken(dataDir, s)
		if tok == "" {
			fmt.Fprintf(out, "%s: not minted — run `xdev serve %s` or `xdev token rotate %s`\n", s, s, s)
			continue
		}
		minted++
		if service != "" {
			// A named service is the script-facing path: the bare value on
			// one line, so `xdev token show auth-gateway` can be captured.
			fmt.Fprintln(out, tok)
			continue
		}
		fmt.Fprintf(out, "%s  %s\n", s, tok)
	}
	if minted == 0 && service == "" {
		all := serve.Services()
		fmt.Fprintf(out, "\nNo local gateway token is configured yet: no token file exists under %s.\n"+
			"Mint one now, or let the service mint it on first start:\n  xdev token rotate %s\n  xdev serve %s\n",
			tokenDir(dataDir), all[0], all[0])
	}
	return 0
}

// tokenRotate mints fresh values with serve.RotateToken — the same writer the
// daemons use, so a rotated file is byte-identical to what they would have
// produced. A running daemon keeps the token it read at startup, hence the
// restart notice: printing the new value without it invites an operator to
// believe a rotation is live when the old token still authenticates.
func tokenRotate(dataDir, service string, all bool, out, errOut io.Writer) int {
	services := tokenServices(service, all)
	for _, s := range services {
		tok, err := serve.RotateToken(dataDir, s)
		if err != nil {
			fmt.Fprintln(errOut, "xdev token:", err)
			return 1
		}
		fmt.Fprintf(out, "%s  %s\n", s, tok)
	}
	fmt.Fprintln(out, "\nwarning: a running daemon keeps the token it loaded at startup, so a rotation only takes effect when that service restarts:")
	for _, s := range services {
		fmt.Fprintf(out, "  xdev serve %s\n", s)
	}
	return 0
}

// tokenCmd is the testable core of `xdev token`: an explicit data directory
// and writers, so the command never has to touch the operator's real install.
func tokenCmd(args []string, dataDir string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, tokenJSONFlagUsage)
	all := fs.Bool("all", false, tokenAllFlagUsage)
	fs.Usage = func() { fmt.Fprint(errOut, tokenUsageText(dataDir)); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// flag.Parse stops at the first positional, so a verb may be followed by
	// flags (`xdev token rotate --all`); re-parse once the verb is peeled off.
	rest := fs.Args()
	verb := "list"
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		verb = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
		rest = fs.Args()
	}

	switch verb {
	case "help", "-h", "--help":
		tokenWriteUsage(dataDir, errOut)
		return 0
	case "list", "show", "rotate":
	default:
		fmt.Fprintf(errOut, "xdev token: unknown verb %q\n\n", verb)
		tokenWriteUsage(dataDir, errOut)
		return 2
	}

	service := ""
	if len(rest) > 0 {
		service = rest[0]
	}
	if len(rest) > 1 {
		fmt.Fprintln(errOut, "xdev token: unexpected argument", rest[1])
		return 2
	}
	// list covers the whole fixed table; a service name here is a confusion
	// (probably a bare `token <service>`) and must not silently list everything
	// while the operator believes they asked about one service.
	if service != "" && verb == "list" {
		fmt.Fprintf(errOut, "xdev token: list takes no service (use show or rotate %s)\n", service)
		return 2
	}
	if service != "" && !serve.KnownService(service) {
		return tokenUnknownService(dataDir, service, errOut)
	}
	if *asJSON && verb != "list" {
		fmt.Fprintf(errOut, "xdev token: --json only applies to list, not %s\n", verb)
		return 2
	}
	if *all && verb != "rotate" {
		fmt.Fprintf(errOut, "xdev token: --all only applies to rotate, not %s\n", verb)
		return 2
	}

	switch verb {
	case "show":
		return tokenShow(dataDir, service, out, errOut)
	case "rotate":
		return tokenRotate(dataDir, service, *all, out, errOut)
	default:
		return tokenList(dataDir, *asJSON, out, errOut)
	}
}
