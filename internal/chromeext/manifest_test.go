package chromeext

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// extensionDir is the shipped extension folder, relative to this package.
const extensionDir = "../../extensions/chrome"

type chromeManifest struct {
	ManifestVersion int `json:"manifest_version"`
	Background      struct {
		ServiceWorker string `json:"service_worker"`
		Type          string `json:"type"`
	} `json:"background"`
}

// staticImport matches a static import statement at the start of a line.
var staticImport = regexp.MustCompile(`(?m)^\s*import\s+(?:[^\n]*?\bfrom\s+)?['"]`)

// staticExport matches an export statement at the start of a line.
var staticExport = regexp.MustCompile(`(?m)^\s*export\s`)

// usesESMSyntax reports whether a script uses static import or export
// statements — the things that only parse in a module scope.
func usesESMSyntax(src string) bool {
	stripped := stripJSComments(src)
	return staticImport.MatchString(stripped) || staticExport.MatchString(stripped)
}

// stripJSComments removes line and block comments so a word like "import" in
// prose does not look like a statement.
func stripJSComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	cleaned := out.String()
	for {
		i := strings.Index(cleaned, "/*")
		if i < 0 {
			return cleaned
		}
		j := strings.Index(cleaned[i:], "*/")
		if j < 0 {
			return cleaned[:i]
		}
		cleaned = cleaned[:i] + cleaned[i+j+2:]
	}
}

// TestManifestServiceWorkerTypeMatchesTheScript pins the manifest to the
// script it loads. Chrome loads the background service worker as a classic
// worker unless the manifest says "type": "module", and a module worker
// cannot use importScripts() — so a script with no module syntax must not be
// declared a module, and a script that gains module syntax must be.
func TestManifestServiceWorkerTypeMatchesTheScript(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(extensionDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var man chromeManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	if man.ManifestVersion != 3 {
		t.Fatalf("manifest_version = %d, want 3", man.ManifestVersion)
	}
	if man.Background.ServiceWorker == "" {
		t.Fatal("background.service_worker is empty")
	}
	src, err := os.ReadFile(filepath.Join(extensionDir, man.Background.ServiceWorker))
	if err != nil {
		t.Fatal(err)
	}
	module := usesESMSyntax(string(src))
	switch {
	case module && man.Background.Type != "module":
		t.Errorf("%s uses module syntax but background.type = %q; Chrome would load it as a classic worker and fail to parse",
			man.Background.ServiceWorker, man.Background.Type)
	case !module && man.Background.Type == "module":
		t.Errorf("%s has no module syntax but background.type = %q; a module worker cannot use importScripts() and gains nothing",
			man.Background.ServiceWorker, man.Background.Type)
	}
}

// TestManifestKeyIsTheShippedExtensionKey guards the id: the manifest key is
// what produces the stable extension id the native host allowlist names.
func TestManifestKeyIsTheShippedExtensionKey(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(extensionDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var man struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(man.Key) == "" {
		t.Fatal("manifest key is empty; the extension id would not be stable")
	}
}
