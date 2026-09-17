package dist

import (
	"fmt"
	"io"
)

// Run dispatches the distribution subcommands (issue #64) and returns the
// process exit code:
//
//	xdev update [--channel stable|canary] [--check] [--timeout D]
//	xdev update job <install|remove|status>   (the twice-daily release check)
//	xdev setup
//	xdev bench  [--turns N] [--model ref]
//
// `job` is a verb of update rather than a top-level subcommand — it exists only
// to schedule a check, so updateMain routes it before flag parsing.
//
// open builds the provider bench measures (nil when the caller cannot).
func Run(sub string, args []string, version string, open OpenProvider, out, errw io.Writer) int {
	switch sub {
	case "update":
		return updateMain(args, version, out, errw)
	case "setup":
		return setupMain(args, version, out, errw)
	case "bench":
		return benchMain(args, version, open, out, errw)
	default:
		fmt.Fprintf(errw, "xdev: unknown distribution subcommand %q (want update|setup|bench)\n", sub)
		return 2
	}
}
