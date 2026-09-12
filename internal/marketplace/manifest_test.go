package marketplace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseManifestJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".xdev-plugin", "marketplace.json")
	writeFile(t, path, `{
	  "name": "shop",
	  "description": "internal plugins",
	  "plugins": [
	    {"name": "hello", "version": "1.2.0", "description": "greets", "source": "https://example.com/hello.git", "revision": "v1.2.0"},
	    {"name": "notes", "source": "./notes"}
	  ]
	}`)

	m, err := ParseManifest(path)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "shop" || len(m.Plugins) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	if p, ok := m.Find("hello", "1.2.0"); !ok || p.Revision != "v1.2.0" {
		t.Fatalf("Find(hello,1.2.0) = %+v, %v", p, ok)
	}
	if _, ok := m.Find("hello", "9.9.9"); ok {
		t.Fatal("version mismatch must not match")
	}
	if p, ok := m.Find("notes", ""); !ok || p.Source != "./notes" {
		t.Fatalf("Find(notes) = %+v, %v", p, ok)
	}
	if got := FindManifest(dir); got != path {
		t.Fatalf("FindManifest = %q, want %q", got, path)
	}
	// The Claude-compatible fallback is used when the xdev path is absent...
	claudeOnly := t.TempDir()
	claudePath := filepath.Join(claudeOnly, ".claude-plugin", "marketplace.json")
	writeFile(t, claudePath, `{"name":"c","plugins":[{"name":"p","source":"s"}]}`)
	if got := FindManifest(claudeOnly); got != claudePath {
		t.Fatalf("FindManifest fallback = %q, want %q", got, claudePath)
	}
	// ...and never wins when both are present.
	writeFile(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"),
		`{"name":"c","plugins":[{"name":"p","source":"s"}]}`)
	if got := FindManifest(dir); got != path {
		t.Fatalf("FindManifest precedence = %q, want %q", got, path)
	}
}

func TestParseManifestYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".xdev-plugin", "marketplace.yaml")
	writeFile(t, path, `name: shop
plugins:
  - name: hello
    version: "1.0.0"
    source: ./hello
    skills: [packs]
`)
	m, err := ParseManifest(path)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Plugins) != 1 || m.Plugins[0].Version != "1.0.0" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Plugins[0].Skills) != 1 || m.Plugins[0].Skills[0] != "packs" {
		t.Fatalf("skills = %v", m.Plugins[0].Skills)
	}
}

func TestParseManifestProblems(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing manifest name",
			body: `{"plugins":[{"name":"a","source":"s"}]}`,
			want: "name is required",
		},
		{
			name: "missing source",
			body: `{"name":"shop","plugins":[{"name":"a"}]}`,
			want: "source is required",
		},
		{
			name: "missing plugin name",
			body: `{"name":"shop","plugins":[{"source":"s"}]}`,
			want: "name is required",
		},
		{
			name: "no plugins",
			body: `{"name":"shop","plugins":[]}`,
			want: "at least one plugin is required",
		},
		{
			name: "duplicate names",
			body: `{"name":"shop","plugins":[{"name":"a","source":"s"},{"name":"a","source":"t"}]}`,
			want: `duplicate plugin name "a"`,
		},
		{
			name: "duplicate names differing in case",
			body: `{"name":"shop","plugins":[{"name":"a","source":"s"},{"name":"A","source":"t"}]}`,
			want: `duplicate plugin name "A"`,
		},
		{
			name: "path traversal in name",
			body: `{"name":"shop","plugins":[{"name":"../evil","source":"s"}]}`,
			want: "not a usable directory name",
		},
		{
			name: "separator in name",
			body: `{"name":"shop","plugins":[{"name":"a/b","source":"s"}]}`,
			want: "must not contain a path separator",
		},
		{
			name: "hidden name",
			body: `{"name":"shop","plugins":[{"name":".evil","source":"s"}]}`,
			want: "not a usable directory name",
		},
		{
			name: "absolute capability dir",
			body: `{"name":"shop","plugins":[{"name":"a","source":"s","commands":["/etc"]}]}`,
			want: "must be relative to the plugin root",
		},
		{
			name: "escaping capability dir",
			body: `{"name":"shop","plugins":[{"name":"a","source":"s","hooks":["../outside"]}]}`,
			want: "escapes the plugin root",
		},
		{
			name: "malformed json",
			body: `{"name":`,
			want: "unexpected end of JSON input",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "marketplace.json")
			writeFile(t, path, tc.body)
			_, err := ParseManifest(path)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParsePluginManifestNeedsNameOnly(t *testing.T) {
	// The inner manifest carries no source: it is the plugin's own identity.
	path := filepath.Join(t.TempDir(), "plugin.json")
	writeFile(t, path, `{"name":"hello","version":"1.0.0","commands":["cmds"]}`)
	p, err := ParsePluginManifest(path)
	if err != nil {
		t.Fatalf("ParsePluginManifest: %v", err)
	}
	if p.Name != "hello" || len(p.Commands) != 1 {
		t.Fatalf("plugin = %+v", p)
	}

	writeFile(t, path, `{"version":"1.0.0"}`)
	if _, err := ParsePluginManifest(path); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("err = %v, want a missing-name error", err)
	}
}

func TestSplitRef(t *testing.T) {
	for _, tc := range []struct{ ref, name, version string }{
		{"hello", "hello", ""},
		{"hello@1.2.3", "hello", "1.2.3"},
		{" hello@v2 ", "hello", "v2"},
		{"hello@", "hello", ""},
	} {
		name, version := SplitRef(tc.ref)
		if name != tc.name || version != tc.version {
			t.Fatalf("SplitRef(%q) = %q, %q; want %q, %q", tc.ref, name, version, tc.name, tc.version)
		}
	}
}

func TestResolveDirsConventions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "commands", "a.md"), "A\n")
	writeFile(t, filepath.Join(root, "hooks", "g.yml"), "event: tool_call\ncommand: ./g.sh\n")
	writeFile(t, filepath.Join(root, "packs", "s", "SKILL.md"), "---\ndescription: d\n---\nbody\n")

	d := ResolveDirs(Plugin{Name: "p"}, root)
	if len(d.Commands) != 1 || len(d.Hooks) != 1 {
		t.Fatalf("conventional dirs = %+v", d)
	}
	if len(d.Skills) != 0 || len(d.Agents) != 0 {
		t.Fatalf("absent conventional dirs must not be registered: %+v", d)
	}
	d = ResolveDirs(Plugin{Name: "p", Skills: []string{"packs"}}, root)
	if len(d.Skills) != 1 || !strings.HasSuffix(d.Skills[0], filepath.Join("packs")) {
		t.Fatalf("declared skills = %+v", d)
	}
}
