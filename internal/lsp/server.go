package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ServerSpec describes one language server command. The lsp-side twin of
// config.LSPServer, converted by ConfigFromSettings.
type ServerSpec struct {
	Command     string          `json:"command"`
	Args        []string        `json:"args"`
	FileTypes   []string        `json:"fileTypes"`
	RootMarkers []string        `json:"rootMarkers"`
	InitOptions json.RawMessage `json:"initOptions"`
	Disabled    bool            `json:"disabled"`
}

// DefaultServers are the built-in servers, all opt-in-lazy: a server starts
// only when the lsp tool first touches a matching file and only if its
// binary is on PATH. Users override any field per language via lsp.servers.
func DefaultServers() map[string]ServerSpec {
	return map[string]ServerSpec{
		"go": {
			Command:     "gopls",
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod", "go.work", ".git"},
		},
		"rust": {
			Command:     "rust-analyzer",
			FileTypes:   []string{"rs"},
			RootMarkers: []string{"Cargo.toml", ".git"},
		},
		"typescript": {
			Command:     "typescript-language-server",
			Args:        []string{"--stdio"},
			FileTypes:   []string{"ts", "tsx", "js", "jsx", "mjs", "cjs"},
			RootMarkers: []string{"package.json", "tsconfig.json", "jsconfig.json", ".git"},
		},
		"python": {
			Command:     "pyright-langserver",
			Args:        []string{"--stdio"},
			FileTypes:   []string{"py", "pyi"},
			RootMarkers: []string{"pyproject.toml", "setup.py", "setup.cfg", "pyrightconfig.json", ".git"},
		},
	}
}

// builtinLanguage maps file extensions to an LSP language id, used when no
// server claims the extension and for didOpen's languageId.
var builtinLanguage = map[string]string{
	"go": "go", "rs": "rust",
	"ts": "typescript", "tsx": "typescriptreact", "mts": "typescript", "cts": "typescript",
	"js": "javascript", "jsx": "javascriptreact", "mjs": "javascript", "cjs": "javascript",
	"py": "python", "pyi": "python",
}

func langIDFor(ext string) string {
	if id, ok := builtinLanguage[ext]; ok {
		return id
	}
	return ext
}

// uriFromPath / uriToPath convert between file paths and file:// URIs.
func uriFromPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

// uriToPath is the inverse; a URI of another scheme (a virtual document, say)
// is returned unchanged so the caller can report it verbatim.
func uriToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	return filepath.FromSlash(u.Path)
}

// startServer launches one server subprocess and performs the initialize
// handshake. A missing binary is an actionable error, never a crash.
func startServer(ctx context.Context, name string, spec ServerSpec, root string) (*Client, error) {
	bin, err := exec.LookPath(spec.Command)
	if err != nil {
		return nil, fmt.Errorf(
			"lsp/%s: server %q not found on PATH — install it, or point lsp.servers.%s.command at another binary",
			name, spec.Command, name)
	}
	cmd := exec.Command(bin, spec.Args...)
	cmd.Dir = root
	prepareProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp/%s: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp/%s: %w", name, err)
	}
	tail := newTailWriter(4 << 10)
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp/%s: start %s: %w", name, bin, err)
	}
	c := newClient(stdin, stdout, name)
	c.proc = cmd.Process
	c.exited = make(chan struct{})
	c.stderr = tail
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	c.start()

	params := map[string]any{
		"processId":    os.Getpid(),
		"rootUri":      uriFromPath(root),
		"capabilities": map[string]any{},
	}
	if len(spec.InitOptions) > 0 {
		params["initializationOptions"] = spec.InitOptions
	}
	if _, err := c.Call(ctx, "initialize", params); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// detectRoot walks up from dir to stop (the session cwd) looking for a root
// marker; the file's own directory is the fallback. Markers are file or
// directory names (go.mod, .git).
func detectRoot(dir, stop string, markers []string) string {
	d := dir
	for {
		for _, m := range markers {
			if _, err := os.Stat(filepath.Join(d, m)); err == nil {
				return d
			}
		}
		if d == stop || d == filepath.Dir(d) {
			break
		}
		d = filepath.Dir(d)
	}
	return dir
}
