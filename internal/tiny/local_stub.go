//go:build !tinycgo

package tiny

import "context"

// unavailable is the shipped artifact's backend. xdev is one CGO-free static
// binary (<100 MB RSS, six-platform cross matrix), so no on-device runtime is
// linked: the stub reports that instead of pretending, and Select turns it
// into ErrNotBuilt. Nothing degrades to the API on the user's behalf.
//
// ponytail: no local inference at all in this build. Ceiling = tiny tasks need
// the network; upgrade path = the `tinycgo` artifact (option B in
// docs/decisions/local-tiny-models.md), which replaces this file's compiledIn.
type unavailable struct{}

func (unavailable) Name() string { return "none (CGO-free static build)" }

func (unavailable) Available() bool { return false }

func (unavailable) Complete(context.Context, string) (string, error) {
	return "", ErrNotBuilt
}

// compiledIn is the backend for the default build.
func compiledIn() LocalBackend { return unavailable{} }
