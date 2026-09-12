//go:build tinycgo

package tiny

import "context"

// Option B placeholder (docs/decisions/local-tiny-models.md). `-tags tinycgo`
// opts a build into a local runtime, but this checkout links none, so the
// backend still reports unavailability and Select still refuses instead of
// falling back to the API. Implementing option B means replacing this file
// with one that links the runtime (onnxruntime / llama.cpp) and returns a
// real LocalBackend; the seam, the env switch, and Select's contract stay put.
//
// ponytail: the tag alone buys nothing but a compiling seam — deliberate, so
// the opt-in artifact is never a silently-empty stub. Ceiling = no inference
// on `-tags tinycgo` either; upgrade = a real local backend in this file.
type cgoPlaceholder struct{}

func (cgoPlaceholder) Name() string { return "tinycgo (no runtime linked)" }

func (cgoPlaceholder) Available() bool { return false }

func (cgoPlaceholder) Complete(context.Context, string) (string, error) {
	return "", ErrNotBuilt
}

// compiledIn is the backend for an opted-in (non-default) build.
func compiledIn() LocalBackend { return cgoPlaceholder{} }
