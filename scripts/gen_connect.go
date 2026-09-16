//go:build ignore

// Command gen_connect regenerates internal/config/connect_catalog.go from a
// models.dev snapshot — the same catalog opencode ships its provider list
// with (~215 hosts, each with per-model context/output/reasoning/vision).
//
//	go run scripts/gen_connect.go /tmp/models.json
//
// (fetch the snapshot with `curl -sL https://models.dev/api.json`).
//
// //go:build ignore keeps it out of `go build ./...`: it is a maintenance tool
// with a local file argument, not part of the binary.
//
// The snapshot is filtered to what xdev can actually call: an entry needs an
// npm package whose wire protocol xdev speaks (npmToAPI) plus a base URL —
// models.dev omits `api` for the hosts whose SDK hardcodes it, so
// nativeSDK supplies those — and it contributes only models that can drive a
// coding agent: tool calling, a published context window, text out.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// npmToAPI maps a models.dev SDK package onto xdev's wire-adapter names
// (internal/ai/provider.go). In practice there are two dialects: the
// OpenAI-compatible chat/completions route most hosts publish, and Anthropic
// Messages.
var npmToAPI = map[string]string{
	"@ai-sdk/openai-compatible":              "openai-completions",
	"@ai-sdk/openai":                         "openai-completions",
	"@ai-sdk/groq":                           "openai-completions",
	"@ai-sdk/cerebras":                       "openai-completions",
	"@ai-sdk/togetherai":                     "openai-completions",
	"@ai-sdk/xai":                            "openai-completions",
	"@ai-sdk/mistral":                        "openai-completions",
	"@ai-sdk/cohere":                         "openai-completions",
	"@ai-sdk/deepinfra":                      "openai-completions",
	"@ai-sdk/perplexity":                     "openai-completions",
	"@openrouter/ai-sdk-provider":            "openai-completions",
	"@aihubmix/ai-sdk-provider":              "openai-completions",
	"merge-gateway-ai-sdk-provider":          "openai-completions",
	"venice-ai-sdk-provider":                 "openai-completions",
	"ai-gateway-provider":                    "openai-completions",
	"gitlab-ai-provider":                     "openai-completions",
	"watsonx-ai-provider":                    "openai-completions",
	"@jerome-benoit/sap-ai-provider-v2":      "openai-completions",
	"@saladtechnologies-oss/ai-sdk-provider": "openai-completions",
	"@qvac/ai-sdk-provider":                  "openai-completions",
	"@ai-sdk/anthropic":                      "anthropic-messages",
	"@ai-sdk/google-vertex/anthropic":        "anthropic-messages",
}

// nativeSDK is the endpoint + protocol for the hosts models.dev identifies
// only by SDK package: their adapter hardcodes the URL, so the snapshot
// carries none. Each URL answers GET <url>/models (401 unauthenticated,
// verified live), which is what makes discovery work for them too.
type native struct{ BaseURL, API string }

var nativeSDK = map[string]native{
	"@ai-sdk/anthropic":  {"https://api.anthropic.com", "anthropic-messages"},
	"@ai-sdk/openai":     {"https://api.openai.com/v1", "openai-completions"},
	"@ai-sdk/google":     {"https://generativelanguage.googleapis.com/v1beta", "google-generative-ai"},
	"@ai-sdk/groq":       {"https://api.groq.com/openai/v1", "openai-completions"},
	"@ai-sdk/cerebras":   {"https://api.cerebras.ai/v1", "openai-completions"},
	"@ai-sdk/xai":        {"https://api.x.ai/v1", "openai-completions"},
	"@ai-sdk/mistral":    {"https://api.mistral.ai/v1", "openai-completions"},
	"@ai-sdk/togetherai": {"https://api.together.xyz/v1", "openai-completions"},

	// Below here the same rule: models.dev names the SDK but publishes no URL.
	// Each of these answered GET <url>/models live (200 or 401, never 404), so
	// the pinned rows and discovery both work.
	"@ai-sdk/deepinfra":         {"https://api.deepinfra.com/v1/openai", "openai-completions"},
	"@ai-sdk/cohere":            {"https://api.cohere.com/compatibility/openai/v1", "openai-completions"},
	"@aihubmix/ai-sdk-provider": {"https://api.aihubmix.com/v1", "openai-completions"},
	"venice-ai-sdk-provider":    {"https://api.venice.ai/api/v1", "openai-completions"},
}

type providerDoc struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	API    string              `json:"api"`
	Npm    string              `json:"npm"`
	Env    []string            `json:"env"`
	Doc    string              `json:"doc"`
	Models map[string]modelDoc `json:"models"`
}

type modelDoc struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Reasoning   bool   `json:"reasoning"`
	ReleaseDate string `json:"release_date"`
	ToolCall    *bool  `json:"tool_call"`
	Attachment  *bool  `json:"attachment"`
	Modalities  *struct {
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

// entry and model mirror the shapes emitted into internal/config; the program
// compiles alone, so it declares its own copy.
type entry struct {
	Title, BaseURL, API, Doc string
	Env                      []string
	Models                   []model
}

type model struct {
	ID, Name                 string
	ContextWindow, MaxTokens int
	Reasoning, Vision        bool
	Release                  string
}

// Pinning limits: enough rows that /model offers the current generation of a
// host, few enough that models.yml stays readable.
const (
	pinMaxModels  = 12
	pinMinContext = 128000
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen_connect <models.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	fatal(err)
	var doc map[string]providerDoc
	fatal(json.Unmarshal(raw, &doc))

	out := map[string]entry{}
	for key, p := range doc {
		api, ok := npmToAPI[p.Npm]
		if !ok {
			continue // an SDK xdev has no adapter for
		}
		base := p.API
		if n, isNative := nativeSDK[p.Npm]; isNative {
			api, base = n.API, n.BaseURL
		}
		if base == "" {
			continue // no published endpoint: nothing to connect to
		}
		var rows []model
		for id, m := range p.Models {
			if m.ToolCall != nil && !*m.ToolCall {
				continue // cannot call tools: cannot drive the agent
			}
			if m.Limit.Context <= 0 || m.Limit.Output <= 0 {
				continue // no window published: compaction would be guessing
			}
			if m.Limit.Context < pinMinContext {
				continue // below a coding agent's working set
			}
			if !textOutput(m) {
				continue
			}
			name := m.Name
			if name == "" || name == id {
				name = ""
			}
			rows = append(rows, model{ID: id, Name: name, ContextWindow: m.Limit.Context,
				MaxTokens: m.Limit.Output, Reasoning: m.Reasoning, Release: m.ReleaseDate,
				Vision: m.Attachment != nil && *m.Attachment})
		}
		if len(rows) == 0 {
			continue
		}
		// Newest first, ties by id: the order the catalog keeps, so the first
		// pinned row is the host's current model — what a default should be.
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Release != rows[j].Release {
				return rows[i].Release > rows[j].Release
			}
			return rows[i].ID < rows[j].ID
		})
		// Pin the newest few, not the whole list: the snapshot carries 5.7k
		// models and models.yml is a file people read. The rest stay reachable
		// through discovery, which every connected provider turns on.
		if len(rows) > pinMaxModels {
			rows = rows[:pinMaxModels]
		}
		title := p.Name
		if title == "" {
			title = key
		}
		out[key] = entry{Title: title, BaseURL: base, API: api, Env: usableEnv(p.Env), Doc: p.Doc, Models: rows}
	}

	var buf strings.Builder
	buf.WriteString(header)
	names := make([]string, 0, len(out))
	for k := range out {
		names = append(names, k)
	}
	sort.Strings(names)
	buf.WriteString("var connectCatalog = map[string]connectEntry{\n")
	for _, k := range names {
		e := out[k]
		fmt.Fprintf(&buf, "\t%s: {\n", quote(k))
		fmt.Fprintf(&buf, "\t\tTitle: %s, BaseURL: %s, API: %s,\n", quote(e.Title), quote(e.BaseURL), quote(e.API))
		if len(e.Env) > 0 {
			fmt.Fprintf(&buf, "\t\tEnv: []string{%s},\n", joinQuoted(e.Env))
		}
		if e.Doc != "" {
			fmt.Fprintf(&buf, "\t\tDoc: %s,\n", quote(e.Doc))
		}
		buf.WriteString("\t\tModels: []connectModel{\n")
		for _, m := range e.Models {
			fmt.Fprintf(&buf, "\t\t\t{ID: %s", quote(m.ID))
			if m.Name != "" {
				fmt.Fprintf(&buf, ", Name: %s", quote(m.Name))
			}
			fmt.Fprintf(&buf, ", ContextWindow: %d, MaxTokens: %d", m.ContextWindow, m.MaxTokens)
			if m.Reasoning {
				buf.WriteString(", Reasoning: true")
			}
			if m.Vision {
				buf.WriteString(", Vision: true")
			}
			buf.WriteString("},\n")
		}
		buf.WriteString("\t\t},\n\t},\n")
	}
	buf.WriteString("}\n")
	fatal(os.WriteFile("internal/config/connect_catalog.go", []byte(buf.String()), 0o644))
	count := 0
	for _, e := range out {
		count += len(e.Models)
	}
	fmt.Printf("wrote internal/config/connect_catalog.go: %d providers, %d models\n", len(names), count)
}

// textOutput reports whether a model emits text: image/audio-only entries
// (tts, image generation) sit in the same table and would be dead rows.
func textOutput(m modelDoc) bool {
	if m.Modalities == nil {
		return true // undocumented: leave it to the user's judgment
	}
	for _, o := range m.Modalities.Output {
		if o == "text" {
			return true
		}
	}
	return false
}

// usableEnv keeps credential names the config loader can expand: models.yml
// ${VAR} substitution matches [A-Za-z_][A-Za-z0-9_]*, so a path-shaped
// reference (${AWS_PROFILE[0]}) would be written verbatim and never resolve.
func usableEnv(env []string) []string {
	var out []string
	for _, e := range env {
		if isIdent(e) {
			out = append(out, e)
		}
	}
	return out
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func quote(s string) string { return fmt.Sprintf("%q", s) }

func joinQuoted(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = quote(s)
	}
	return strings.Join(out, ", ")
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen_connect:", err)
		os.Exit(1)
	}
}

const header = `// Code generated by scripts/gen_connect.go from a models.dev snapshot. DO NOT EDIT.
//
// Regenerate with:
//
//	curl -sL https://models.dev/api.json -o /tmp/models.json
//	go run scripts/gen_connect.go /tmp/models.json
//
// A snapshot rather than a live fetch: models.dev has no stable hosted
// endpoint (it redirects to a CDN), and "xdev connect" must answer without the
// network. Each row is an endpoint, a wire protocol, the credential variable
// the host reads, and the models worth pinning — never a key.

package config

`
