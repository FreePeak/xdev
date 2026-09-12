// This file adds the `xdev say` subcommand: the CLI front door to the same
// backend and chunker the tts tool uses.
package tts

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Say runs `xdev say [flags] <text...>`, joining the positional args as the
// text to speak. It writes progress/plan output to stdout, errors to stderr,
// and returns the process exit code (0 ok, 1 failure, 2 usage).
func Say(args []string, stdout, stderr io.Writer, s Settings) int {
	fs := flag.NewFlagSet("say", flag.ContinueOnError)
	fs.SetOutput(stderr)
	voice := fs.String("voice", "", "voice name (overrides tts.voice)")
	rate := fs.Int("rate", 0, "speaking rate in words per minute, 80–600 (overrides tts.rate)")
	dryRun := fs.Bool("dry-run", false, "print the chunk plan instead of speaking")
	usage := fs.Usage
	if err := fs.Parse(args); err != nil {
		return 2
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		usage()
		return 2
	}
	if !RateOK(*rate) {
		fmt.Fprintf(stderr, "say: rate %d out of range (80–600 words/min, or omit for the default)\n", *rate)
		return 2
	}
	sp, err := New()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	req := merge(s, text, *voice, *rate, *dryRun)
	if req.DryRun {
		for _, line := range sp.Plan(req) {
			fmt.Fprintln(stdout, line)
		}
		return 0
	}
	rep, err := sp.Speak(context.Background(), req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, rep.Summary())
	return 0
}
