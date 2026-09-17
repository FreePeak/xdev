// Package tiny is the seam for on-device "tiny model" work: the session-title
// and memory-pipeline tasks that omp serves from local ONNX/MLX models
// (omp://local-models.md). xdev ships one CGO-free static binary, so no local
// runtime is linked today — this package is the *decision* made executable:
//
//   - Requested reports whether the environment asked for on-device inference
//     (XDEV_TINY_LOCAL=on). Unset/"off" — the default — keeps every tiny task
//     on the configured API completer, which is the only path that exists in the
//     shipped artifact.
//   - Backend is the local backend compiled into this binary. In the default
//     build it is a stub that reports unavailability; a `-tags tinycgo` build
//     (a separate, non-static artifact — option B in
//     docs/decisions/local-tiny-models.md) replaces it.
//   - Select is the single consultation point: it returns the API completer
//     unless local inference was explicitly requested, and fails with
//     ErrNotBuilt when it was but this binary has no runtime. It never
//     substitutes the API completer for a requested local one: the user asked
//     for work to stay on the machine, and quietly moving it to the network
//     would be a privacy bug, not a graceful degradation.
//
// Adding option B means implementing LocalBackend in a build-tagged file and
// replacing compiledIn — the seam and Select's contract do not change.
package tiny

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvLocal is the opt-in switch for on-device tiny inference. It is read from
// the process environment after the dotenv chain loads (config.LoadEnv), so it
// can live in a shell export or a project/agent .env file.
const EnvLocal = "XDEV_TINY_LOCAL"

// ErrNotBuilt reports that a local tiny backend was requested but this binary
// has none linked. Callers must surface it, never work around it.
var ErrNotBuilt = errors.New("no local tiny backend built into this binary")

// Task names one tiny-model job, so refusal and error messages say which work
// was affected.
type Task string

const (
	// TaskTitle is session-title generation. Not implemented in xdev yet: new
	// sessions get mechanical titles (cmd/xdev/print.go openSession); the
	// generator, when it exists, reads its prompt override from
	// agent.SystemPromptOverrides.TitleSystemPrompt.
	TaskTitle Task = "session-title"
	// TaskMemoryExtract is memory-pipeline phase 1 (findings extraction).
	TaskMemoryExtract Task = "memory-extract"
	// TaskMemoryConsolidate is memory-pipeline phase 2 (MEMORY.md rewrite).
	TaskMemoryConsolidate Task = "memory-consolidate"
)

// LocalBackend is the on-device implementation seam. A backend is consulted
// only when the environment opted in (Select), and it must answer Available
// honestly: a backend that cannot serve a request reports false and lets
// Select refuse, rather than degrading to the API behind the caller's back.
type LocalBackend interface {
	// Name identifies the backend + model for messages (never empty).
	Name() string
	// Available reports whether this backend can serve Complete right now.
	Available() bool
	// Complete runs one prompt to completion on-device. It returns a plain
	// ErrNotBuilt-wrapping error when no runtime is linked or the worker
	// cannot start, and a request error otherwise.
	Complete(ctx context.Context, prompt string) (string, error)
}

// Completer is the model seam the memory pipeline (and a future title
// generator) consumes: prompt in, text out, one streamed response.
type Completer func(ctx context.Context, prompt string) (string, error)

// Backend returns the local backend compiled into this binary. It is never
// nil; the default (CGO-free) artifact returns the stub whose Available is
// false and whose Complete fails with ErrNotBuilt.
func Backend() LocalBackend { return compiledIn() }

// Requested reports whether the environment opted into on-device inference.
// A value outside the on/off vocabulary is an error rather than a silent
// "off", so a typo cannot quietly keep tiny work on the API.
func Requested() (bool, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvLocal)))
	switch v {
	case "", "off", "0", "false", "no":
		return false, nil
	case "on", "1", "true", "yes":
		return true, nil
	default:
		return false, fmt.Errorf("tiny: %s=%q is not on/off", EnvLocal, v)
	}
}

// Select resolves the completer for one tiny task: api by default, the local
// backend when XDEV_TINY_LOCAL=on, and ErrNotBuilt when that opt-in cannot be
// honored. api is returned unchanged (same func) unless local was requested,
// so the default path is byte-for-byte the path xdev runs today.
func Select(task Task, api Completer) (Completer, error) {
	local, err := Requested()
	if err != nil {
		return nil, err
	}
	if !local {
		if api == nil {
			return nil, fmt.Errorf("tiny: %s: no API completer configured", task)
		}
		return api, nil
	}
	b := Backend()
	if b != nil && b.Available() {
		return b.Complete, nil
	}
	name := "none"
	if b != nil {
		name = b.Name()
	}
	return nil, fmt.Errorf("tiny: %s: %w (backend %q; see docs/decisions/local-tiny-models.md)", task, ErrNotBuilt, name)
}
