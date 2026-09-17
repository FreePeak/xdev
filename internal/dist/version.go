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
// filled missing components, ignored build metadata ("+meta"), a pre-release
// suffix that sorts below its own release, and a `git describe` suffix that
// says nothing about precedence at all (see parseSemver).
type semver struct {
	core []int
	pre  []string
}

// parseSemver parses "1.2.3", "v1.2", "1.2.3-rc.2", "0.1.0-dev".
//
// A `git describe` tail — "-2-gf419040", the commit count past the tag and
// its shortened SHA — is dropped rather than read as a pre-release. That
// suffix says a build is *ahead* of v0.4.0, but semver precedence ranks any
// hyphenated form *below* the plain tag — so both of this package's readers
// agree on the wrong answer: `xdev update --check` says "update available:
// v0.4.0-2-gf419040 → v0.4.0", and plain `xdev update` acts on it, replacing
// a newer binary with the release that predates it. The shape is matched
// strictly — digits, then "-g" plus seven or more hex characters — so a real
// pre-release like "0.1.0-dev" or "0.2.0-canary.1" keeps semver's rule.
//
// Order of trimming follows git's own layout,
// <tag>-<n>-g<sha>[-dirty][+meta], so each suffix has exactly one home:
// "-dirty" first (it is optional and sits after the SHA), then build
// metadata, then the describe tail, then the pre-release.
func parseSemver(v string) (semver, error) {
	s := strings.TrimSpace(v)
	if s != "" && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	// git's own order, so each suffix has exactly one home: build metadata
	// last of all, then the describe tail, then the real pre-release. The
	// tail is optional ("-dirty" from --dirty), so trim it before matching.
	s = strings.TrimSuffix(s, "-dirty")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := describeSuffix(s); i >= 0 {
		s = s[:i] // ahead of the tag it describes, not a pre-release of it
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

// describeSuffix returns the index of a `git describe` suffix ("-2-gf419040")
// in s, or -1 when s carries no such suffix. The shape is strict on purpose:
// digits, then "-g" plus at least seven hex characters. That leaves real
// pre-releases ("0.2.0-canary.1", "0.1.0-dev") to the semver rule.
func describeSuffix(s string) int {
	i := strings.LastIndexByte(s, '-')
	if i < 0 || !strings.HasPrefix(s[i+1:], "g") {
		return -1
	}
	rest := s[:i]
	j := strings.LastIndexByte(rest, '-')
	if j < 0 {
		return -1
	}
	num := rest[j+1:]
	if num == "" {
		return -1
	}
	for _, r := range num {
		if r < '0' || r > '9' {
			return -1
		}
	}
	const minHex = 7 // a shortened SHA-1 is 7+ chars; "g1" is not a describe tail
	hash := s[i+2:]
	if len(hash) < minHex {
		return -1
	}
	for _, r := range hash {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return -1
		}
	}
	return j
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
