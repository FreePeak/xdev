package dist

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
)

// setupStarterConfig is the starter ~/.xdev/agent/config.yml `xdev setup`
// writes when none exists: the shipped defaults, each with the values it
// accepts. Every key here is a real Settings field — the loader rejects
// unknown keys, so a typo in this template would make xdev refuse to start.
const setupStarterConfig = `# xdev configuration — written by "xdev setup".
#
# Layering (later wins): schema defaults <- this file <- <project>/.xdev/config.yml
# <- -config <file> overlays. Unknown keys are rejected, so a typo is reported
# rather than silently dropped. "xdev config list" prints the resolved values
# and the layer each came from; "xdev config set <key> <value>" edits this file.

theme: auto             # groknight | grokday | auto
approvalMode: yolo      # always-ask | write | yolo
maxTurns: 200           # agent turns per run before the graceful wrap-up
memoryLimit: 104857600  # bytes; hard RSS backstop (default 100 MB)
showThinking: true      # render reasoning output
personality: default    # default | friendly | pragmatic | none
memory: local           # local (MEMORY.md + learned.md under the data dir) | off | mnemopi | hindsight

# The reviewer used by the advisor and by task.agentAdvisor: "on".
# advisorModel: xdev-server/deepseek-v4.1-flash
`

// setupModelsTemplate is the models.yml shape printed when no provider file
// exists yet. It is printed rather than written: it references a credential
// and an endpoint only the user knows.
const setupModelsTemplate = `# <data-dir>/models.yml — providers and models. ${VAR} is expanded from the
# environment (and from <data-dir>/.env), so no key has to live in this file.
providers:
  xdev-server:
    baseUrl: ${XDEV_SERVER_URL}/v1
    apiKey: ${XDEV_SERVER_KEY}
    api: openai-completions          # also: openai-responses | anthropic-messages | google-generative-ai
    discovery:
      type: openai-models-list
    models:
      - { id: free, name: Free, reasoning: true, contextWindow: 200000 }
      - { id: xdev, name: xdev, reasoning: true, contextWindow: 200000 }
defaultModel: xdev-server/free
`

// setupDirs are the data-dir subdirectories users or the product drop files
// into; setup creates them so the first session does not race a read.
var setupDirs = []struct{ name, purpose string }{
	{"sessions", "session transcripts (append-only JSONL)"},
	{"agents", "user task agents (markdown + frontmatter)"},
	{"commands", "user slash commands (markdown)"},
	{"themes", "custom themes (JSON)"},
}

// setupOptionalTools names the binaries xdev shells out to when present. All
// of them are optional — xdev degrades to a pure-Go path or reports the tool
// unavailable — so setup only reports, it never installs.
var setupOptionalTools = []struct{ bin, why, hint string }{
	{"git", "worktrees, diffs, the commit workflow", "https://git-scm.com/downloads"},
	{"rg", "faster grep (pure-Go fallback)", "brew install ripgrep | apt install ripgrep"},
	{"fd", "faster glob (pure-Go fallback)", "brew install fd | apt install fd-find"},
	{"ast-grep", "ast_grep / ast_edit structural search", "brew install ast-grep | npm i -g @ast-grep/cli"},
	{"gh", "the github tool (PRs, issues, Actions)", "brew install gh | apt install gh"},
	{"python3", "the eval tool's persistent cells", "https://www.python.org/downloads/"},
}

// setupMain implements `xdev setup`: a non-interactive onboarding pass that
// is safe to re-run — every step reports what it created and what it left
// alone, and nothing is overwritten.
func setupMain(args []string, _ string, out, errw io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(errw, "usage: xdev setup    (no arguments)")
		return 2
	}
	dataDir := config.DataDir()
	fmt.Fprintf(out, "xdev setup — data dir %s\n\n", dataDir)
	if os.Getenv("XDEV_AGENT_DIR") != "" {
		fmt.Fprintf(out, "  (XDEV_AGENT_DIR is set — this profile's data lives here)\n")
	}

	// 1. Data dir + the subdirectories the product reads and writes.
	created, existing := 0, 0
	for _, dir := range append([]struct{ name, purpose string }{{"", "data directory"}}, setupDirs...) {
		path := dataDir
		label := dataDir
		if dir.name != "" {
			path = filepath.Join(dataDir, dir.name)
			label = dir.name
		}
		_, statErr := os.Stat(path)
		if statErr == nil {
			existing++
			fmt.Fprintf(out, "  ok       %-9s %s\n", label, orDefault(dir.purpose, "data directory"))
			continue
		}
		// 0700: credentials.json and secrets.yml live under here.
		if err := os.MkdirAll(path, 0o700); err != nil {
			fmt.Fprintln(errw, "xdev:", err)
			return 1
		}
		created++
		fmt.Fprintf(out, "  created  %-9s %s\n", label, orDefault(dir.purpose, "data directory"))
	}
	fmt.Fprintf(out, "  %d created, %d already present\n\n", created, existing)

	// 2. Starter config.yml, written only when absent.
	cfgPath := config.GlobalSettingsPath()
	if _, err := os.Stat(cfgPath); err == nil {
		fmt.Fprintf(out, "config.yml   kept existing %s\n", cfgPath)
	} else {
		if err := writeStarterConfig(cfgPath); err != nil {
			fmt.Fprintln(errw, "xdev:", err)
			return 1
		}
		fmt.Fprintf(out, "config.yml   wrote starter %s\n", cfgPath)
	}

	// 3. Providers: report what is configured, or print the template.
	modelsPath := filepath.Join(dataDir, "models.yml")
	if providers := configuredProviders(modelsPath); len(providers) > 0 {
		fmt.Fprintf(out, "models.yml   found %s (providers: %s)\n", modelsPath, strings.Join(providers, ", "))
	} else {
		fmt.Fprintf(out, "models.yml   missing — save this as %s:\n\n%s\n", modelsPath, indent(setupModelsTemplate, "  "))
	}

	// 4. Optional external tools (report only).
	fmt.Fprint(out, "\noptional tools\n")
	missing := 0
	for _, t := range setupOptionalTools {
		if _, err := exec.LookPath(t.bin); err == nil {
			fmt.Fprintf(out, "  ok       %-9s %s\n", t.bin, t.why)
			continue
		}
		missing++
		fmt.Fprintf(out, "  missing  %-9s %s  (%s)\n", t.bin, t.why, t.hint)
	}
	if missing == 0 {
		fmt.Fprint(out, "  all present\n")
	}

	// 5. Next steps.
	fmt.Fprintf(out, `
Next steps
  1. export XDEV_SERVER_URL=http://<gateway-host>:8080   the gateway you run
     export XDEV_SERVER_KEY=...                          and its key
     (or put both in %s)
  2. xdev "explain this repo"           one-shot run
     xdev tui                           interactive session
  3. xdev config list                   resolved settings and their layer
     xdev update --check                is a newer release available?
`, filepath.Join(dataDir, ".env"))
	return 0
}

// writeStarterConfig writes the starter file and refuses to leave an
// unparseable one behind: the loader rejects unknown keys, and a config.yml
// that fails to load would make every later run fail.
func writeStarterConfig(path string) error {
	if err := validateStarterConfig([]byte(setupStarterConfig)); err != nil {
		return fmt.Errorf("starter config is invalid (this is a bug): %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(setupStarterConfig), 0o644)
}

// validateStarterConfig decodes the template with the loader's exact policy
// (strict keys, typed Settings) before it is written.
func validateStarterConfig(raw []byte) error {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var s config.Settings
	return dec.Decode(&s)
}

// configuredProviders lists the provider names in models.yml (empty when the
// file is absent or unparseable — setup reports, it does not police).
func configuredProviders(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Providers map[string]any `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Providers))
	for name := range doc.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
