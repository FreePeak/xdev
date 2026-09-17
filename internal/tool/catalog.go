package tool

// Deferred tool catalog (M13 #54): progressive disclosure for the long tail.
// A catalogued tool stays registered and executable, but it leaves the eager
// tool schema (Registry.Defs) and the prompt recap — the model finds it with
// tool_search, reads its schema with tool_describe, and runs it through
// tool_call.
//
// The bridge never executes anything itself: it hands the named call back to
// the harness Runner, so a bridged call takes the identical path as a direct
// one (plan-mode gate, approval policy, interceptor chain, hooks) and the
// policy decides on the REAL tool name, never on "tool_call".

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Bridge tool names. The catalog refuses to hold them, so a bridged call can
// never re-enter the bridge.
const (
	ToolSearchName   = "tool_search"
	ToolDescribeName = "tool_describe"
	ToolCallName     = "tool_call"
)

// DefaultSearchLimit caps tool_search hits when the model passes no limit.
const DefaultSearchLimit = 10

// Runner executes one catalogued tool with the harness's full call path.
// agent.Agent.WireCatalog installs the agent loop's path; a Catalog with no
// Runner refuses to run anything, because a bridge that executed tools on its
// own would be a policy bypass.
type Runner func(ctx context.Context, name string, args json.RawMessage) (Result, error)

// Entry is one deferred tool as the catalog knows it.
type Entry struct {
	Name  string
	Index string   // one-line summary: prompt index, search hits
	Tags  []string // extra search terms ("search", "plan", ...)
}

// Catalog is one registry's deferred-tool view.
type Catalog struct {
	reg     *Registry
	mu      sync.RWMutex
	entries map[string]Entry
	runner  Runner
}

// NewCatalog returns an empty catalog over reg. Defer requires the tool to be
// registered in reg first; Registry.Defer is the usual entry point.
func NewCatalog(reg *Registry) *Catalog {
	return &Catalog{reg: reg, entries: map[string]Entry{}}
}

// SetRunner installs the harness call path for tool_call (nil = refuse).
func (c *Catalog) SetRunner(r Runner) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runner = r
}

// Defer moves an already-registered tool behind the catalog: it leaves the
// eager tool schema and the prompt recap, and becomes reachable through
// tool_search / tool_describe / tool_call. index is the one-line summary the
// prompt index and the search hits show, and it is required the same way a
// name is. Deferring an unregistered or bridge tool is a programming error.
//
// A deferred tool called directly still runs (the registry still holds it):
// the catalog is discovery, not a boundary — the approval policy is.
func (c *Catalog) Defer(name, index string, tags ...string) {
	if c == nil {
		return
	}
	if name == "" || index == "" {
		panic("tool: Defer needs a tool name and a one-line index")
	}
	if isBridgeTool(name) {
		panic(fmt.Sprintf("tool: Defer(%q): the catalog bridge cannot itself be deferred", name))
	}
	if _, ok := c.reg.Get(name); !ok {
		panic(fmt.Sprintf("tool: Defer(%q): register the tool first", name))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[name] = Entry{Name: name, Index: index, Tags: append([]string(nil), tags...)}
}

// drop forgets a deferred tool whose registration went away
// (Registry.Remove). Callers hold the registry lock: registry → catalog is
// the documented lock order.
func (c *Catalog) drop(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, name)
}

// Entries returns every deferred tool, ordered by name for deterministic
// prompts and search output.
func (c *Catalog) Entries() []Entry {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns one deferred tool.
func (c *Catalog) Lookup(name string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[name]
	return e, ok
}

// Search ranks the deferred tools against a free-text query: exact name >
// name prefix > name substring > tag > index prose, summed across query
// tokens so a tool matching more of the query ranks higher. An empty query
// lists the catalog; limit <= 0 uses DefaultSearchLimit.
func (c *Catalog) Search(query string, limit int) []Entry {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return capEntries(c.Entries(), limit)
	}
	type hit struct {
		e     Entry
		score int
	}
	var hits []hit
	for _, e := range c.Entries() {
		if s := rankEntry(e, tokens); s > 0 {
			hits = append(hits, hit{e, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].e.Name < hits[j].e.Name
	})
	out := make([]Entry, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.e)
	}
	return capEntries(out, limit)
}

// Describe renders one deferred tool's schema and docs; an unknown name
// carries the tool_search hint instead of a dead end.
func (c *Catalog) Describe(name string) string {
	e, ok := c.Lookup(name)
	if !ok {
		return c.missText(ToolDescribeName, name)
	}
	t, _ := c.reg.Get(name)
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", e.Name)
	fmt.Fprintf(&b, "index: %s\n", e.Index)
	if len(e.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(e.Tags, ", "))
	}
	fmt.Fprintf(&b, "status: deferred (absent from the tool list until called)\n")
	fmt.Fprintf(&b, "description: %s\n", t.Description())
	fmt.Fprintf(&b, "schema:\n%s\n", indentJSON(t.Parameters()))
	fmt.Fprintf(&b, "invoke: %s {\"name\": %q, \"args\": <schema args>}", ToolCallName, e.Name)
	return b.String()
}

// Call runs one deferred tool through the harness Runner. Every failure is a
// model-visible Result (never a Go error): an unknown name, or a catalog with
// no runner installed, refuses instead of executing.
func (c *Catalog) Call(ctx context.Context, name string, args json.RawMessage) Result {
	if _, ok := c.Lookup(name); !ok {
		return Result{Text: c.missText(ToolCallName, name), IsError: true}
	}
	c.mu.RLock()
	run := c.runner
	c.mu.RUnlock()
	if run == nil {
		return Result{Text: ToolCallName + ": no runner installed — the harness must wire the catalog before a deferred tool can run", IsError: true}
	}
	res, err := run(ctx, name, nestedArgs(args))
	if err != nil {
		return Result{Text: fmt.Sprintf("%s: %s failed: %v", ToolCallName, name, err), IsError: true}
	}
	return res
}

// rankEntry scores one entry against the lowercased query tokens.
func rankEntry(e Entry, tokens []string) int {
	name := strings.ToLower(e.Name)
	text := strings.ToLower(e.Index)
	score := 0
	for _, tok := range tokens {
		switch {
		case name == tok:
			score += 100
		case strings.HasPrefix(name, tok):
			score += 60
		case strings.Contains(name, tok):
			score += 40
		case tagMatches(e.Tags, tok):
			score += 25
		case strings.Contains(text, tok):
			score += 10
		}
	}
	return score
}

// tagMatches reports whether any tag contains tok.
func tagMatches(tags []string, tok string) bool {
	for _, tg := range tags {
		if strings.Contains(strings.ToLower(tg), tok) {
			return true
		}
	}
	return false
}

// capEntries truncates a result list to limit.
func capEntries(es []Entry, limit int) []Entry {
	if len(es) > limit {
		return es[:limit]
	}
	return es
}

// missText is the shared name-miss message. It distinguishes three kinds of
// miss, because the wrong one sends the model hunting for a capability it
// already has: live, `tool_describe {"name":"web_search"}` answered
// `unknown tool "web_search"`, the model concluded the tool did not exist, and
// it shelled out to curl instead. A registered-but-not-deferred name IS
// callable directly; only a name the registry has never heard of is unknown.
// An empty name is not a miss at all: it is a malformed bridge call (a
// provider streaming a nameless tool call — seen live as `unknown tool ""`),
// so it answers with the shape to resend instead of a catalog to search.
func (c *Catalog) missText(op, name string) string {
	if strings.TrimSpace(name) == "" {
		return fmt.Sprintf("%s: name is required — resend as %s {\"name\":<tool name>,\"args\":{...}} (%s lists the names)",
			op, ToolCallName, ToolSearchName)
	}
	entries := c.Entries()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	list := "(empty)"
	if len(names) > 0 {
		list = strings.Join(names, ", ")
	}
	if _, eager := c.reg.Get(name); eager {
		return fmt.Sprintf("%s: %q is not a deferred tool — it is advertised in your tool list, so call it directly (the deferred catalog holds: %s)", op, name, list)
	}
	return fmt.Sprintf("%s: unknown tool %q (not in the deferred catalog: %s); use %s to look for it",
		op, name, list, ToolSearchName)
}

// isBridgeTool reports whether name is one of the catalog's own tools.
func isBridgeTool(name string) bool {
	switch name {
	case ToolSearchName, ToolDescribeName, ToolCallName:
		return true
	}
	return false
}

// nestedArgs normalizes the nested call payload: models send either a JSON
// object or a JSON-encoded string of one, and omit it entirely when the tool
// takes no arguments.
func nestedArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return json.RawMessage(s)
		}
	}
	return raw
}

// indentJSON pretty-prints a tool schema for tool_describe; unparseable or
// empty input is passed through unchanged ("" becomes "{}").
func indentJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// --- Registry: the deferred view ---

// Catalog returns the registry's deferred-tool catalog, creating it on first
// use (a nil registry has none).
func (r *Registry) Catalog() *Catalog {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.catalog == nil {
		r.catalog = NewCatalog(r)
	}
	return r.catalog
}

// Defer moves an already-registered tool behind the catalog; see
// Catalog.Defer. It panics on an unknown name, like Register does on a
// duplicate — a typo'd deferral must not silently leave a tool eager.
func (r *Registry) Defer(name, index string, tags ...string) {
	r.Catalog().Defer(name, index, tags...)
}

// Deferred returns the catalogued tools, ordered by name: exactly the set the
// prompt index lists and Defs omits. Empty when no catalog is configured, so
// a catalog-free prompt is unchanged.
func (r *Registry) Deferred() []Entry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	c := r.catalog
	r.mu.RUnlock()
	return c.Entries()
}

// deferred reports whether name is behind the catalog. Callers hold r.mu (the
// lock order is registry then catalog, never the reverse).
func (r *Registry) deferred(name string) bool {
	if r == nil || r.catalog == nil {
		return false
	}
	return r.catalog.has(name)
}

// has reports whether name is catalogued.
func (c *Catalog) has(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.entries[name]
	return ok
}

// --- The three bridge tools ---

var (
	_ Tool = (*ToolSearchTool)(nil)
	_ Tool = (*ToolDescribeTool)(nil)
	_ Tool = (*ToolCallTool)(nil)
)

// ToolSearchTool searches the deferred catalog.
type ToolSearchTool struct{ Catalog *Catalog }

// NewToolSearchTool returns the tool_search bridge tool.
func NewToolSearchTool(c *Catalog) *ToolSearchTool { return &ToolSearchTool{Catalog: c} }

func (t *ToolSearchTool) Name() string { return ToolSearchName }

func (t *ToolSearchTool) Description() string {
	return "search tools that are not in the tool list: names and one-line summaries; tool_describe shows a schema, tool_call runs it"
}

func (t *ToolSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"what the tool does, its name, or a tag"},"limit":{"type":"integer","description":"max hits (default 10)"}},"required":["query"]}`)
}

func (t *ToolSearchTool) Execute(_ context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: ToolSearchName + ": invalid arguments: " + err.Error(), IsError: true}, nil
	}
	hits := t.Catalog.Search(a.Query, a.Limit)
	if len(hits) == 0 {
		return Result{Text: fmt.Sprintf("no deferred tool matches %q; try another word, or call a listed tool directly", a.Query)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es):\n", len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s: %s", h.Name, h.Index)
		if len(h.Tags) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(h.Tags, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s <name> for the schema; %s {\"name\":<name>,\"args\":{...}} to run it.", ToolDescribeName, ToolCallName)
	return Result{Text: b.String()}, nil
}

// ToolDescribeTool returns one deferred tool's full schema and docs.
type ToolDescribeTool struct{ Catalog *Catalog }

// NewToolDescribeTool returns the tool_describe bridge tool.
func NewToolDescribeTool(c *Catalog) *ToolDescribeTool { return &ToolDescribeTool{Catalog: c} }

func (t *ToolDescribeTool) Name() string { return ToolDescribeName }

func (t *ToolDescribeTool) Description() string {
	return "show a deferred tool's full schema and description by name"
}

func (t *ToolDescribeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"tool name from tool_search"}},"required":["name"]}`)
}

func (t *ToolDescribeTool) Execute(_ context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: ToolDescribeName + ": invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if _, ok := t.Catalog.Lookup(a.Name); !ok {
		return Result{Text: t.Catalog.missText(ToolDescribeName, a.Name), IsError: true}, nil
	}
	return Result{Text: t.Catalog.Describe(a.Name)}, nil
}

// ToolCallTool runs a deferred tool by name through the harness runner.
type ToolCallTool struct{ Catalog *Catalog }

// NewToolCallTool returns the tool_call bridge tool.
func NewToolCallTool(c *Catalog) *ToolCallTool { return &ToolCallTool{Catalog: c} }

func (t *ToolCallTool) Name() string { return ToolCallName }

func (t *ToolCallTool) Description() string {
	return "run a tool found with tool_search (tools already in the tool list are called directly)"
}

func (t *ToolCallTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"tool name from tool_search"},"args":{"type":"object","description":"the tool's arguments (see tool_describe)"}},"required":["name"]}`)
}

func (t *ToolCallTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: ToolCallName + ": invalid arguments: " + err.Error(), IsError: true}, nil
	}
	return t.Catalog.Call(ctx, a.Name, a.Args), nil
}
