// Tests for `xdev connect`: the listing reports state, and connecting one row
// writes a loadable models.yml without ever putting a key in it.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// connectSandbox redirects every xdev root (models.yml, credentials.json, .env)
// into a temp directory for one test.
func connectSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	t.Setenv("DEEPSEEK_API_KEY", "") // the ready-row check needs a clean slate
	return dir
}

func TestConnectCmdAppliesAndReports(t *testing.T) {
	dir := connectSandbox(t)
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	var out, errOut bytes.Buffer
	if code := connectCmd([]string{"deepseek"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "connected deepseek") {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(got, "DEEPSEEK_API_KEY from the environment") {
		t.Fatalf("output does not name the credential in use: %q", got)
	}
	if !strings.Contains(got, "xdev print -model deepseek/") {
		t.Fatalf("output gives no runnable model ref: %q", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "models.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "env-key") {
		t.Errorf("the environment's key was copied into models.yml:\n%s", raw)
	}
	if _, err := config.LoadModels(filepath.Join(dir, "models.yml")); err != nil {
		t.Errorf("written models.yml does not load: %v", err)
	}
}

// --key stores the credential rather than writing it where a config file lives.
func TestConnectCmdKeyFlagStoresCredential(t *testing.T) {
	dir := connectSandbox(t)
	var out, errOut bytes.Buffer
	args := []string{"zai", "--key", "sk-pasted-key"}
	if code := connectCmd(args, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "models.yml"))
	if strings.Contains(string(raw), "sk-pasted-key") {
		t.Errorf("--key leaked into models.yml:\n%s", raw)
	}
	store, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if store["zai"].APIKey != "sk-pasted-key" {
		t.Errorf("stored credential = %+v", store["zai"])
	}
}

// A typo is the likeliest error at this prompt, so it names the near misses
// instead of only failing.
func TestConnectCmdUnknownNamesAlternatives(t *testing.T) {
	connectSandbox(t)
	var out, errOut bytes.Buffer
	if code := connectCmd([]string{"deep"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2 (%s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "deepseek") {
		t.Errorf("no suggestion for the typo:\n%s", errOut.String())
	}
}

// The default listing is the rows a user can connect right now; --list is the
// whole catalog. Both must be non-empty, and --json must parse as the shape
// scripts read.
func TestConnectCmdListings(t *testing.T) {
	connectSandbox(t)
	var out, errOut bytes.Buffer
	if code := connectCmd([]string{"--list"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "openrouter") || !strings.Contains(out.String(), "anthropic") {
		t.Errorf("--list is missing known hosts:\n%s", firstLines(out.String(), 8))
	}

	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	out.Reset()
	if code := connectCmd(nil, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "deepseek") {
		t.Errorf("the ready listing omits a provider whose key is set:\n%s", out.String())
	}
	if strings.Contains(out.String(), "openrouter") {
		t.Errorf("the ready listing includes a provider with no credential:\n%s", out.String())
	}

	out.Reset()
	if code := connectCmd([]string{"--list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "[") || !strings.Contains(out.String(), `"baseUrl"`) {
		t.Errorf("--json output is not the row shape:\n%s", firstLines(out.String(), 6))
	}
}

// --set-default writes the default model, and leaves a default the user already
// chose alone... except when asked: the flag is the decision.
func TestConnectCmdSetDefault(t *testing.T) {
	dir := connectSandbox(t)
	var out, errOut bytes.Buffer
	args := []string{"moonshotai", "--set-default"}
	if code := connectCmd(args, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	path := filepath.Join(dir, "models.yml")
	cfg, err := config.LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel == "" || !strings.HasPrefix(cfg.DefaultModel, "moonshotai/") {
		t.Errorf("defaultModel = %q", cfg.DefaultModel)
	}
	// A second connect with the flag moves it, and says so.
	out.Reset()
	args = []string{"deepseek", "--set-default"}
	if code := connectCmd(args, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "was moonshotai/") {
		t.Errorf("replacing a default went unmentioned:\n%s", out.String())
	}
}

func TestConnectCmdInfo(t *testing.T) {
	connectSandbox(t)
	var out, errOut bytes.Buffer
	if code := connectCmd([]string{"volcengine", "--info"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	body := out.String()
	if !strings.Contains(body, "volcengine/") || !strings.Contains(body, "pinned models") {
		t.Errorf("info output = %q", firstLines(body, 6))
	}
	// The first row is the newest model, which is also what --set-default
	// picks: the two listings must agree or the hint and the write diverge.
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 {
		t.Fatalf("info rows = %q", body)
	}
	if got, want := strings.TrimSpace(lines[1]), config.ConnectDefaultRef("volcengine"); got != want {
		t.Errorf("first info row = %q, want the default ref %q", got, want)
	}
}

func firstLines(s string, n int) string {
	all := strings.Split(s, "\n")
	if len(all) > n {
		all = all[:n]
	}
	return strings.Join(all, "\n")
}

// --key - is the private route: the credential arrives on a pipe, so it never
// enters the argument vector (visible in ps) or the shell history.
func TestConnectCmdKeyFromStdin(t *testing.T) {
	dir := connectSandbox(t)
	var out, errOut bytes.Buffer
	// connectCmd reads os.Stdin for "-", so this test drives the real binary.
	bin := filepath.Join(t.TempDir(), "xdev")
	if err := exec.Command("go", "build", "-o", bin, ".").Run(); err != nil {
		t.Fatalf("build xdev: %v", err)
	}
	cmd := exec.Command(bin, "connect", "deepseek", "--key", "-")
	cmd.Env = append(os.Environ(), "XDEV_AGENT_DIR="+dir)
	cmd.Stdin = strings.NewReader("piped-secret\n")
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("exit: %v (%s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "credentials.json") {
		t.Errorf("output = %q", out.String())
	}
	store, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if store["deepseek"].APIKey != "piped-secret" {
		t.Errorf("stored = %+v, want the piped credential", store["deepseek"])
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "models.yml"))
	if strings.Contains(string(raw), "piped-secret") {
		t.Errorf("the piped key reached models.yml:\n%s", raw)
	}
}

// The picker rows are cmd-side glue, and their ordering is the feature: the
// groups must be contiguous (a section header paints above the first row of its
// group) and ordered by what the user can act on.
func TestConnectPickerItems(t *testing.T) {
	connectSandbox(t)
	cfg, err := config2Load()
	if err != nil {
		t.Fatal(err)
	}
	items := connectPickerItems(cfg)()
	if len(items) != len(config.ConnectNames()) {
		t.Fatalf("rows = %d, want the whole catalog (%d)", len(items), len(config.ConnectNames()))
	}
	seen := map[string]int{}
	last := -1
	for i, it := range items {
		if _, dup := seen[it.Value]; dup {
			t.Fatalf("provider %s appears twice in the picker", it.Value)
		}
		seen[it.Value] = i
		rank := map[string]int{"credential in hand": 0, "already in models.yml": 1, "subscriptions": 2, "catalog": 3}[it.Section]
		if rank < last {
			t.Fatalf("row %d (%s) is section %q after a group that sorts later", i, it.Value, it.Section)
		}
		last = rank
	}
	// With a credential exported, that host leads the list — the row a user
	// wants first, and the reason the picker needs no keys.
	t.Setenv("DEEPSEEK_API_KEY", "from-env")
	reloaded, err := config2Load()
	if err != nil {
		t.Fatal(err)
	}
	items = connectPickerItems(reloaded)()
	if items[0].Value != "deepseek" || items[0].Section != "credential in hand" {
		t.Fatalf("first row = %+v, want deepseek in the ready group", items[0])
	}
	if !strings.Contains(items[0].Detail, "env DEEPSEEK_API_KEY") {
		t.Errorf("row detail hides the credential source: %q", items[0].Detail)
	}
}
