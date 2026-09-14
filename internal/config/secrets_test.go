package config

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// warnSink collects the warning lines a loader or redactor reports.
func warnSink() (func(string), *[]string) {
	var lines []string
	return func(s string) { lines = append(lines, s) }, &lines
}

// TestLoadSecretsRefusesProjectShadowing (#114): a repository's secrets.yml
// may add entries — that is the point of a per-project file — but it may not
// replace a profile entry by name. Redaction matches on the value, so a clone
// that shadows `GITHUB_TOKEN` with a dummy stops the user's real token from
// being masked, and it starts flowing to the provider in the clear.
func TestLoadSecretsRefusesProjectShadowing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	writeFile(t, GlobalSecretsPath(), `
secrets:
  - name: global-token
    value: ghp-global-value-0001
  - name: shared
    value: global-shared-value-01
  - name: shared
    value: duplicate-value-dropped
`)
	writeFile(t, projectSecretsPath(cwd), `
- name: shared
  value: project-shared-value-1
- name: aws
  pattern: "AKIA[0-9A-Z]{16}"
`)

	warn, lines := warnSink()
	got := LoadSecrets(cwd, warn)
	byName := map[string]SecretEntry{}
	for _, e := range got {
		byName[e.Name] = e
	}
	if len(got) != 3 {
		t.Fatalf("entries = %+v, want 3", got)
	}
	if e := byName["shared"]; e.Value != "global-shared-value-01" {
		t.Errorf("shared = %q, want the profile value to survive a project shadow", e.Value)
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "shared") ||
		!strings.Contains(strings.Join(*lines, "\n"), "ignored") {
		t.Errorf("the refusal must be reported, got %v", *lines)
	}
	if e := byName["global-token"]; e.Value != "ghp-global-value-0001" {
		t.Errorf("global-only entry lost: %+v", byName)
	}
	if e := byName["aws"]; e.Pattern != "AKIA[0-9A-Z]{16}" {
		t.Errorf("bare-list row not decoded: %+v", e)
	}
	if !strings.Contains(strings.Join(*lines, "\n"), `duplicate entry "shared"`) {
		t.Errorf("duplicate entry inside one file should warn, got %v", *lines)
	}
}

func TestLoadSecretsSkipsBadFilesWithWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"malformed", "secrets: [oops\n", "skipped"},
		{"empty", "", "empty file"},
		{"scalar document", "just a string\n", "expected a list"},
		{"secrets not a list", "secrets: nope\n", "not a list"},
		{"no secrets key", "other: []\n", "no `secrets:` list"},
		{"empty list", "secrets: []\n", "no usable entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			cwd := t.TempDir()
			writeFile(t, GlobalSecretsPath(), tc.body)

			warn, lines := warnSink()
			if got := LoadSecrets(cwd, warn); len(got) != 0 {
				t.Fatalf("entries = %+v, want none", got)
			}
			if len(*lines) == 0 || !strings.Contains((*lines)[0], tc.want) {
				t.Fatalf("warnings = %v, want one containing %q", *lines, tc.want)
			}
		})
	}
}

func TestRedactorApplyLiteralAndPattern(t *testing.T) {
	const literal = "sk-live-9f3a2b1c"
	const token = "AKIA1234567890ABCDEF"
	r := NewRedactor([]SecretEntry{
		{Name: "api", Value: literal},
		{Name: "aws", Pattern: `AKIA[0-9A-Z]{16}`},
	}, nil)

	in := "token " + literal + " and key " + token + " end"
	out := r.Apply(in)
	if strings.Contains(out, literal) || strings.Contains(out, token) {
		t.Fatalf("raw value left in %q", out)
	}
	if n := len(placeholderRE.FindAllString(out, -1)); n != 2 {
		t.Fatalf("placeholders in %q = %d, want 2", out, n)
	}
	if back := r.Expand(out); back != in {
		t.Fatalf("Expand(Apply(x)) = %q, want %q", back, in)
	}
	// A placeholder this session never minted is left verbatim: expansion
	// never invents a value.
	if got := r.Expand("$$DEADBEEF$$"); got != "$$DEADBEEF$$" {
		t.Fatalf("foreign placeholder = %q, want it untouched", got)
	}
}

func TestRedactorPrefersLongestLiteral(t *testing.T) {
	r := NewRedactor([]SecretEntry{
		{Name: "short", Value: "sk-abc"},
		{Name: "long", Value: "sk-abc123456"},
	}, nil)

	out := r.Apply("x sk-abc123456 y")
	if strings.Contains(out, "sk-abc") {
		t.Fatalf("shorter literal matched inside the longer one: %q", out)
	}
	if len(placeholderRE.FindAllString(out, -1)) != 1 {
		t.Fatalf("placeholders in %q = %d, want 1", out, 1)
	}
	if back := r.Expand(out); back != "x sk-abc123456 y" {
		t.Fatalf("round trip = %q", back)
	}
}

func TestRedactorHashStableWithinSession(t *testing.T) {
	entries := []SecretEntry{{Name: "t", Value: "ghp-abcdef123456"}}
	r1 := NewRedactor(entries, nil)
	a, b := r1.Apply("a ghp-abcdef123456"), r1.Apply("b ghp-abcdef123456")
	pa, pb := placeholderRE.FindString(a), placeholderRE.FindString(b)
	if pa == "" || pa != pb {
		t.Fatalf("placeholders %q / %q differ for one value", pa, pb)
	}
	// A second redactor (a resumed session, same per-install key) agrees.
	r2 := NewRedactor(entries, nil)
	if got := placeholderRE.FindString(r2.Apply("ghp-abcdef123456")); got != pa {
		t.Fatalf("placeholder across redactors = %q, want %q", got, pa)
	}
}

func TestRedactorMultiLineValue(t *testing.T) {
	pem := "-----BEGIN KEY-----\nQUJDREVGRw==\n-----END KEY-----"
	r := NewRedactor([]SecretEntry{{Name: "pem", Value: pem}}, nil)

	in := "before\n" + pem + "\nafter"
	out := r.Apply(in)
	if strings.Contains(out, "QUJDREVGRw==") {
		t.Fatalf("multi-line value left in %q", out)
	}
	if back := r.Expand(out); back != in {
		t.Fatalf("round trip = %q, want %q", back, in)
	}
}

func TestRedactorValueRoundTrip(t *testing.T) {
	const secret = "sk-deep-000011112222"
	r := NewRedactor([]SecretEntry{{Name: "s", Value: secret}}, nil)

	args := map[string]any{
		"cmd":  "echo " + secret,
		"raw":  json.RawMessage(`{"k":"` + secret + `"}`),
		"list": []any{secret, 7},
	}
	red, ok := r.ApplyValue(args).(map[string]any)
	if !ok {
		t.Fatal("ApplyValue changed the payload shape")
	}
	if strings.Contains(red["cmd"].(string), secret) || strings.Contains(string(red["raw"].(json.RawMessage)), secret) {
		t.Fatalf("raw value survived ApplyValue: %+v", red)
	}
	back, ok := r.ExpandValue(red).(map[string]any)
	if !ok {
		t.Fatal("ExpandValue changed the payload shape")
	}
	if back["cmd"] != args["cmd"] || string(back["raw"].(json.RawMessage)) != string(args["raw"].(json.RawMessage)) {
		t.Fatalf("round trip = %+v", back)
	}
	if got := back["list"].([]any)[0]; got != secret {
		t.Fatalf("list element = %v, want the secret back", got)
	}
}

func TestRedactorInertWhenNilOrEmpty(t *testing.T) {
	const text = "sk-live-9f3a2b1c"
	var nilR *Redactor
	if got := nilR.Apply(text); got != text {
		t.Fatalf("nil Apply = %q", got)
	}
	if got := nilR.Expand("$$ABCD1234$$"); got != "$$ABCD1234$$" {
		t.Fatalf("nil Expand = %q", got)
	}
	if got, ok := nilR.ApplyValue(text).(string); !ok || got != text {
		t.Fatalf("nil ApplyValue = %v", got)
	}

	empty := NewRedactor(nil, nil)
	if got := empty.Apply(text); got != text {
		t.Fatalf("empty Apply = %q", got)
	}
	if got := empty.Expand(text); got != text {
		t.Fatalf("empty Expand = %q", got)
	}
}

func TestRedactorWarnsAboutBadEntries(t *testing.T) {
	warn, lines := warnSink()
	r := NewRedactor([]SecretEntry{
		{Name: "no-matcher"},
		{Name: "bad-regex", Pattern: "([unclosed"},
		{Value: "sk-unnamed-123456"},
		{Name: "both", Value: "sk-both-123456789", Pattern: "AKIA.*"},
	}, warn)

	out := r.Apply("sk-unnamed-123456 sk-both-123456789")
	if strings.Contains(out, "sk-unnamed") || strings.Contains(out, "sk-both") {
		t.Fatalf("a kept entry was not redacted: %q", out)
	}
	joined := strings.Join(*lines, "\n")
	for _, want := range []string{"no value or pattern", "error parsing regexp", "unnamed entry", "using value"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings = %v, want one containing %q", *lines, want)
		}
	}
}

func TestRedactorPlaceholderShape(t *testing.T) {
	r := NewRedactor([]SecretEntry{{Name: "k", Value: "sk-shape-12345678"}}, nil)
	out := r.Apply("sk-shape-12345678")
	if !regexp.MustCompile(`^\$\$[0-9A-F]{8}\$\$$`).MatchString(out) {
		t.Fatalf("placeholder = %q, want $$HASH8$$", out)
	}
}

func TestEnvEntries(t *testing.T) {
	got := EnvEntries([]string{
		"GH_TOKEN=ghp_value_12345678",
		"PATH=/usr/bin:/bin",
		"SHORT_KEY=abc",
		"ONEGW_KEY=sk-3657312345678901",
		"DUP_TOKEN=ghp_value_12345678",
		"MALFORMED",
	})
	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "GH_TOKEN,ONEGW_KEY" {
		t.Fatalf("entries = %v, want only the long secret-shaped names", names)
	}
}

func TestPlaceholderKeyFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	warn, lines := warnSink()

	first := loadPlaceholderKey(warn)
	if len(first) < 16 {
		t.Fatalf("key = %q, want real key material", first)
	}
	info, err := os.Stat(PlaceholderKeyPath())
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key mode = %v, want 0600", perm)
	}
	if second := loadPlaceholderKey(warn); string(second) != string(first) {
		t.Fatal("key is not stable across calls")
	}

	// A truncated key file is regenerated, with a warning, rather than trusted.
	writeFile(t, PlaceholderKeyPath(), "short\n")
	third := loadPlaceholderKey(warn)
	if string(third) == "short" || len(third) < 16 {
		t.Fatalf("key after a bad file = %q, want a fresh key", third)
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "unusable key") {
		t.Fatalf("warnings = %v, want an unusable-key warning", *lines)
	}
	raw, err := os.ReadFile(PlaceholderKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if persisted, derr := hex.DecodeString(strings.TrimSpace(string(raw))); derr != nil || string(persisted) != string(third) {
		t.Fatal("the regenerated key was not persisted")
	}
}

func TestRedactorConcurrentUse(t *testing.T) {
	// Tool calls run on a bounded pool, so Apply/Expand are hit from several
	// goroutines at once — including the lazy placeholder a pattern match
	// mints on first sight.
	r := NewRedactor([]SecretEntry{
		{Name: "lit", Value: "sk-live-9f3a2b1c"},
		{Name: "pat", Pattern: `AKIA[0-9A-Z]{16}`},
	}, nil)
	const in = "a sk-live-9f3a2b1c b AKIA1234567890ABCDEF"

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if back := r.Expand(r.Apply(in)); back != in {
					t.Errorf("round trip = %q", back)
					return
				}
			}
		}()
	}
	wg.Wait()
}
