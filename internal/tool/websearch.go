package tool

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/FreePeak/xdev/internal/websearch"
)

// web_search (M13 #48): the agent-facing wrapper over internal/websearch,
// which owns the provider chain, query parsing, relaxation, and result
// sanitization. The split exists because internal/config imports
// internal/tool (through internal/agent), so the settings type the registry
// passes in cannot live in either one — see the websearch package doc.

// WebSearchTool searches the web through a fallback chain of providers.
type WebSearchTool struct {
	Searcher *websearch.Searcher
}

// NewWebSearchTool builds the tool from the layered webSearch settings block.
func NewWebSearchTool(cfg websearch.Settings) *WebSearchTool {
	return &WebSearchTool{Searcher: websearch.New(cfg)}
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	// One terse line on purpose: the system prompt only carries a budgeted
	// tool recap (PRD §1 Goal 4) and degrades to name-only listing once that
	// budget is spent, so extra prose there buys nothing. The operator syntax
	// below ships in the tool schema instead, which the provider receives
	// uncapped.
	return "search the web through fallback providers; returns title/url/snippet rows"
}

func (t *WebSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "search query; Google-style operators: site:, -site:, \"exact phrase\", -term, after:/before: date, filetype:"},
    "max_results": {"type": "integer", "description": "cap on returned results (default 10, max 20)"}
  },
  "required": ["query"]
}`)
}

type webSearchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

func (t *WebSearchTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a webSearchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: "web_search: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return Result{Text: "web_search: query is required", IsError: true}, nil
	}
	if t.Searcher == nil {
		return Result{Text: "web_search: no searcher configured", IsError: true}, nil
	}
	ans := t.Searcher.Search(ctx, a.Query, a.MaxResults)
	return Result{Text: ans.Text, Details: ans.Details, IsError: ans.Failed}, nil
}
