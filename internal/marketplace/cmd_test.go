package marketplace

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunListInstallInfoRemove(t *testing.T) {
	testDataDir(t)
	repo, v1, _ := newGitFixture(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{
		Name: "hello", Version: "1.0.0", Description: "greets", Source: repo, Revision: "v1.0.0",
	})

	var out, errBuf bytes.Buffer
	run := func(args ...string) (int, string) {
		out.Reset()
		errBuf.Reset()
		code := Run(args, &out, &errBuf, nil)
		return code, out.String()
	}

	code, stdout := run("list", "-marketplace", cat)
	if code != 0 || !strings.Contains(stdout, "installed (0)") || !strings.Contains(stdout, "available (1)") {
		t.Fatalf("list = %d, %q", code, stdout)
	}
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("list output missing the catalog entry:\n%s", stdout)
	}

	code, stdout = run("install", "-marketplace", cat, "hello@1.0.0")
	if code != 0 || !strings.Contains(stdout, "installed hello 1.0.0") || !strings.Contains(stdout, v1) {
		t.Fatalf("install = %d, %q", code, stdout)
	}

	code, stdout = run("list", "-marketplace", cat)
	if code != 0 || !strings.Contains(stdout, "installed (1)") {
		t.Fatalf("list after install = %d, %q", code, stdout)
	}

	// The settings-sourced marketplace list works without any flag.
	out.Reset()
	if code := Run([]string{"search", "hello"}, &out, &errBuf, []string{cat}); code != 0 ||
		!strings.Contains(out.String(), "hello") {
		t.Fatalf("search = %d, %q", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"search", "zzz"}, &out, &errBuf, []string{cat}); code != 0 ||
		!strings.Contains(out.String(), "no plugin matches") {
		t.Fatalf("empty search = %d, %q", code, out.String())
	}

	code, stdout = run("info", "hello")
	if code != 0 || !strings.Contains(stdout, "revision:    "+v1) || !strings.Contains(stdout, "commands") {
		t.Fatalf("info = %d, %q", code, stdout)
	}

	code, stdout = run("remove", "hello")
	if code != 0 || !strings.Contains(stdout, "removed hello") {
		t.Fatalf("remove = %d, %q", code, stdout)
	}
	if _, stdout = run("info", "-marketplace", cat, "hello"); !strings.Contains(stdout, "status:      not installed") {
		t.Fatalf("info after remove = %q", stdout)
	}
}

func TestRunUsageErrors(t *testing.T) {
	testDataDir(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Source: t.TempDir()})
	var out, errBuf bytes.Buffer

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"unknown subcommand", []string{"frobnicate"}, 2},
		{"install without name", []string{"install"}, 2},
		{"remove without name", []string{"remove"}, 2},
		{"search without query", []string{"search"}, 2},
		{"unknown flag", []string{"list", "-nope"}, 2},
		{"info unknown plugin", []string{"info", "ghost", "-marketplace", cat}, 1},
		{"install unknown plugin", []string{"install", "ghost", "-marketplace", cat}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			errBuf.Reset()
			if code := Run(tc.args, &out, &errBuf, nil); code != tc.want {
				t.Fatalf("Run(%v) = %d, want %d\nstdout: %s\nstderr: %s",
					tc.args, code, tc.want, out.String(), errBuf.String())
			}
		})
	}
}

func TestRunForceFlagAfterSubcommand(t *testing.T) {
	testDataDir(t)
	repo, _, _ := newGitFixture(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Source: repo})
	var out, errBuf bytes.Buffer

	if code := Run([]string{"install", "-marketplace", cat, "hello"}, &out, &errBuf, nil); code != 0 {
		t.Fatalf("first install = %d, %s", code, errBuf.String())
	}
	// Flags may follow the subcommand (the reason Run splits them itself).
	out.Reset()
	if code := Run([]string{"install", "-force", "-marketplace", cat, "hello"}, &out, &errBuf, nil); code != 0 {
		t.Fatalf("forced install = %d, %s", code, errBuf.String())
	}
}

// TestRunFlagAfterPositional pins the interleaved parse: a flag written after
// an argument must still be honored, not silently leave zero catalogs.
func TestRunFlagAfterPositional(t *testing.T) {
	testDataDir(t)
	repo, _, _ := newGitFixture(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Description: "greets", Source: repo})
	var out, errBuf bytes.Buffer

	if code := Run([]string{"search", "hello", "-marketplace", cat}, &out, &errBuf, nil); code != 0 ||
		!strings.Contains(out.String(), "hello") || strings.Contains(out.String(), "no plugin matches") {
		t.Fatalf("search with a trailing flag = %d, %q", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"install", "hello", "-marketplace", cat}, &out, &errBuf, nil); code != 0 {
		t.Fatalf("install with a trailing flag = %d, %s", code, errBuf.String())
	}
}
