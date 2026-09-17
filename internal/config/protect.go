package config

// Wiring the integrity floor (#192).
//
// `internal/tool` cannot import this package (the approval policy already runs
// the other direction), so the roots are pushed in here rather than pulled:
// this package is linked into every xdev entry point, which means the guard is
// installed before any tool can run and no call site — present or future — can
// forget to switch it on. The function is evaluated per check, so profile
// switching, XDG relocation and the test environment are all seen.

import (
	"github.com/FreePeak/xdev/internal/tool"
)

func init() {
	tool.SetProtectedRoots(func() []string {
		// DataDir holds settings, models.yml, credentials, sessions, blobs,
		// skills, memory, extensions and trust records; StateDir holds
		// breadcrumbs and stall dumps; CacheDir is regenerable but is still
		// written by the harness, not by the model.
		return []string{DataDir(), StateDir(), CacheDir()}
	})
}
