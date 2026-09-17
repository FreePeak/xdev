package dist

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},      // v-prefix is not part of precedence
		{"V1.2", "1.2.0", 0},        // missing components are zero
		{"1.2.1", "1.3", -1},        // minor beats patch
		{"2.0.0", "1.99.99", 1},     // major wins outright
		{"1.0.0", "1.0.1", -1},      // patch matters
		{"v0.2.0", "v0.1.9", 1},     // release tags as published
		{"1.0.0-rc.1", "1.0.0", -1}, // a pre-release sorts below its release
		{"1.0.0", "1.0.0-rc.2", 1},  // ... and above it the other way round
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1}, // numeric identifiers compare numerically
		{"1.0.0-rc.1.2", "1.0.0-rc.1", 1}, // more identifiers wins a shared prefix
		{"0.1.0-dev", "0.1.0", -1},        // a local dev build is behind its release
		{"0.1.0-dev", "0.2.0", -1},
		// A `git describe` stamp means "commits ahead of this tag", not a
		// pre-release of it — semver's own rule reads the hyphenated form as
		// older, which offered a published release as an "update" to a
		// binary already built past it.
		{"v0.4.0-2-gf419040", "v0.4.0", 0},
		{"v0.4.0-2-gf419040", "v0.3.1", 1},
		{"v0.3.1-5-gf71e12c", "v0.4.0", -1},
		{"v0.2.0-canary.1-3-gabcdef1", "v0.2.0-canary.1", 0}, // pre + describe
		{"v0.2.0-canary.1-3-gabcdef1", "v0.1.9", 1},
		// describe's optional pieces, in git's own order: <tag>-<n>-g<sha>
		// [-dirty][+meta]. Each has one home, so none leaks into the core.
		{"v0.4.0-2-gf419040-dirty", "v0.4.0", 0},
		{"v0.4.0-2-gf419040+meta", "v0.4.0", 0},
		{"v0.4.0+meta-2-gf419040", "v0.4.0", 0},
		{"v0.4.0-dirty", "v0.4.0", 0},
		{"1.2.3+build.9", "1.2.3", 0}, // build metadata is ignored
		{"v0.2.0-canary.1", "v0.2.0", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(c.b, c.a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d (inverse)", c.b, c.a, got, -c.want)
		}
	}
}

// An unparseable version must not be silently ordered below every release:
// the updater falls back to a string compare instead of overwriting a
// hand-built binary.
func TestCompareUnparseableFallsBackToStrings(t *testing.T) {
	if got := Compare("dev", "dev"); got != 0 {
		t.Errorf("Compare(dev, dev) = %d, want 0", got)
	}
	if Valid("dev") {
		t.Error("Valid(dev) = true, want false")
	}
	if !Valid("v1.2.3-rc.1") {
		t.Error("Valid(v1.2.3-rc.1) = false, want true")
	}
}
