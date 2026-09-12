// Package dist implements the distribution surface of issue #64: release
// channels (`xdev update`), onboarding (`xdev setup`), and provider
// latency/throughput measurement (`xdev bench`).
//
// Everything here is stdlib-only and bounded: the update path is the one
// place xdev writes outside its data dir, so it verifies a SHA-256 entry
// from the release manifest and refuses to install what it cannot verify.
package dist

import (
	"fmt"
	"strconv"
	"strings"
)

// semver is a tolerant semver-ish version: an optional "v" prefix, zero
// filled missing components, ignored build metadata ("+meta"), and a
// pre-release suffix that sorts below its own release.
type semver struct {
	core []int
	pre  []string
}

// parseSemver parses "1.2.3", "v1.2", "1.2.3-rc.2", "0.1.0-dev".
func parseSemver(v string) (semver, error) {
	s := strings.TrimSpace(v)
	if s != "" && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i] // build metadata does not participate in precedence
	}
	core, pre := s, ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core, pre = s[:i], s[i+1:]
	}
	if core == "" {
		return semver{}, fmt.Errorf("version %q: empty version", v)
	}
	var sv semver
	for _, part := range strings.Split(core, ".") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("version %q: %q is not a number", v, part)
		}
		sv.core = append(sv.core, n)
	}
	if pre != "" {
		sv.pre = strings.Split(pre, ".")
	}
	return sv, nil
}

// Valid reports whether v parses as a version (used to warn about a build
// whose version was never stamped).
func Valid(v string) bool {
	_, err := parseSemver(v)
	return err == nil
}

// Compare orders two version strings: -1 when a is older, 0 when equal,
// +1 when a is newer. An unparseable side falls back to a string compare so
// a hand-built binary ("dev") is never silently treated as ancient and
// overwritten by any release.
func Compare(a, b string) int {
	x, errA := parseSemver(a)
	y, errB := parseSemver(b)
	if errA != nil || errB != nil {
		return strings.Compare(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	for i := range max(len(x.core), len(y.core)) {
		if c := cmpInt(at(x.core, i), at(y.core, i)); c != 0 {
			return c
		}
	}
	switch {
	case len(x.pre) == 0 && len(y.pre) == 0:
		return 0
	case len(x.pre) == 0:
		return 1 // 1.0.0 > 1.0.0-rc.1
	case len(y.pre) == 0:
		return -1
	}
	for i := range max(len(x.pre), len(y.pre)) {
		switch {
		case i >= len(x.pre):
			return -1 // "rc.1" < "rc.1.2"
		case i >= len(y.pre):
			return 1
		}
		if c := cmpPre(x.pre[i], y.pre[i]); c != 0 {
			return c
		}
	}
	return 0
}

// cmpPre orders two pre-release identifiers: numeric identifiers compare
// numerically before alphanumeric ones (semver §11).
func cmpPre(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return cmpInt(na, nb)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func at(s []int, i int) int {
	if i >= len(s) {
		return 0
	}
	return s[i]
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
