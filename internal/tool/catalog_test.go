package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// catalogStub is a registry tool that records every execution.
type catalogStub struct {
	name, desc string
	params     string
	ran        *[]string
}

func (s *catalogStub) Name() string        { return s.name }
func (s *catalogStub) Description() string { return s.desc }

func (s *catalogStub) Parameters() json.RawMessage {
	if s.params == "" {
		return json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	}
	return json.RawMessage(s.params)
}

func (s *catalogStub) Execute(_ context.Context, args json.RawMessage) (Result, error) {
	*s.ran = append(*s.ran, s.name+" "+string(args))
	return Result{Text: s.name + " ran"}, nil
}

// catalogFixture registers four tools and defers two of them.
func catalogFixture(t *testing.T) (*Registry, *Catalog, *[]string) {
	t.Helper()
	var ran []string
	reg := NewRegistry()
	reg.Register(&catalogStub{name: "read", desc: "read a file", ran: &ran})
	reg.Register(&catalogStub{name: "github", desc: "GitHub operations: PRs, issues, files", ran: &ran})
	reg.Register(&catalogStub{name: "ast_grep", desc: "structural code search with ast-grep patterns", ran: &ran})
	reg.Register(&catalogStub{name: "send_message", desc: "send a message to another session", ran: &ran})
	reg.Defer("github", "GitHub operations: PRs, issues, files", "git", "pr")
	reg.Defer("ast_grep", "structural code search with ast-grep patterns", "search", "code")
	return reg, reg.Catalog(), &ran
}

// TestDeferredToolsLeaveEagerDefsButStayRunnable pins the split the prompt
// depends on: a catalogued tool is gone from Defs, still in Get, and listed in
// the one-line index the prompt shows instead.
func TestDeferredToolsLeaveEagerDefsButStayRunnable(t *testing.T) {
	reg, _, _ := catalogFixture(t)
	names := map[string]bool{}
	for _, d := range reg.Defs() {
		names[d.Name] = true
	}
	if names["ast_grep"] || names["github"] {
		t.Fatalf("deferred tool still eager in Defs: %v", names)
	}
	if !names["read"] || !names["send_message"] {
		t.Fatalf("eager tool vanished from Defs: %v", names)
	}
	if _, ok := reg.Get("ast_grep"); !ok {
		t.Fatal("deferred tool left the registry: a direct call must still run")
	}
	idx := reg.Deferred()
	if len(idx) != 2 || idx[0].Name != "ast_grep" || idx[1].Name != "github" {
		t.Fatalf("index = %+v, want ast_grep then github", idx)
	}
	if idx[0].Index == "" || len(idx[0].Tags) == 0 {
		t.Fatalf("index entry lost its summary or tags: %+v", idx[0])
	}
	if NewRegistry().Deferred() != nil {
		t.Fatal("a catalog-free registry must have an empty index")
	}
}

// TestCatalogSearchRanks: name beats tag beats index prose, and a query that
// matches nothing returns nothing.
func TestCatalogSearchRanks(t *testing.T) {
	_, cat, _ := catalogFixture(t)
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"ast_grep", "ast_grep"}, // exact name
		{"ast", "ast_grep"},      // name prefix
		{"git", "github"},        // name substring / tag
		{"code", "ast_grep"},     // tag
		{"files", "github"},      // index prose
	} {
		hits := cat.Search(tc.query, 0)
		if len(hits) == 0 || hits[0].Name != tc.want {
			t.Fatalf("Search(%q) = %+v, want %s first", tc.query, hits, tc.want)
		}
	}
	if hits := cat.Search("nothing-matches-this", 0); len(hits) != 0 {
		t.Fatalf("Search hit on an unmatched query: %+v", hits)
	}
	if hits := cat.Search("", 0); len(hits) != 2 {
		t.Fatalf("empty query must list the catalog, got %+v", hits)
	}
	if hits := cat.Search("", 1); len(hits) != 1 {
		t.Fatalf("limit ignored: %+v", hits)
	}
}

// TestCatalogDescribeReturnsSchemaAndHint: describe serves the tool's real
// schema, and an unknown name points back at tool_search.
func TestCatalogDescribeReturnsSchemaAndHint(t *testing.T) {
	_, cat, _ := catalogFixture(t)
	got := cat.Describe("ast_grep")
	for _, want := range []string{"ast_grep", "structural code search", "deferred", `"q"`, ToolCallName} {
		if !strings.Contains(got, want) {
			t.Fatalf("describe output missing %q:\n%s", want, got)
		}
	}
	miss := cat.Describe("nope")
	if !strings.Contains(miss, "unknown tool") || !strings.Contains(miss, ToolSearchName) {
		t.Fatalf("unknown describe must carry the search hint: %s", miss)
	}
}

// TestCatalogCallRunsThroughRunner: the bridge executes the named tool through
// the installed Runner — never on its own — and normalizes the nested args.
func TestCatalogCallRunsThroughRunner(t *testing.T) {
	reg, cat, ran := catalogFixture(t)
	cat.SetRunner(func(_ context.Context, name string, args json.RawMessage) (Result, error) {
		inner, ok := reg.Get(name)
		if !ok {
			t.Fatalf("runner got an unregistered name %q", name)
		}
		return inner.Execute(context.Background(), args)
	})
	if res := cat.Call(context.Background(), "ast_grep", json.RawMessage(`{"q":"x"}`)); res.IsError {
		t.Fatalf("bridged call failed: %s", res.Text)
	}
	// A model that stringifies the nested args object must still work.
	if res := cat.Call(context.Background(), "ast_grep", json.RawMessage(`"{\"q\":\"y\"}"`)); res.IsError {
		t.Fatalf("stringified args rejected: %s", res.Text)
	}
	// A tool that is not deferred is not bridgeable: tool_call is not a
	// second door around the eager list.
	res := cat.Call(context.Background(), "read", nil)
	if !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("non-deferred name must refuse: %+v", res)
	}
	want := []string{`ast_grep {"q":"x"}`, `ast_grep {"q":"y"}`}
	if len(*ran) != len(want) || (*ran)[0] != want[0] || (*ran)[1] != want[1] {
		t.Fatalf("ran = %v, want %v", *ran, want)
	}
}

// TestCatalogCallRefusesWithoutRunnerAndRecursion: no harness path means no
// execution, and the bridge cannot be re-entered.
func TestCatalogCallRefusesWithoutRunnerAndRecursion(t *testing.T) {
	_, cat, ran := catalogFixture(t)
	if res := cat.Call(context.Background(), "ast_grep", nil); !res.IsError || len(*ran) != 0 {
		t.Fatalf("unwired catalog executed a tool: %+v (ran=%v)", res, *ran)
	}
	// Defer rejects the bridge tools, so tool_call can never resolve to
	// itself through the catalog.
	miss := cat.Call(context.Background(), ToolCallName, nil)
	if !miss.IsError || !strings.Contains(miss.Text, "unknown tool") {
		t.Fatalf("bridge name must not resolve: %+v", miss)
	}
}

// TestDeferRejectsBadNames: a typo in the deferral list is a programming
// error, exactly like a duplicate Register.
func TestDeferRejectsBadNames(t *testing.T) {
	reg, _, _ := catalogFixture(t)
	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"unregistered tool", func() { reg.Defer("nope", "index") }},
		{"empty index", func() { reg.Defer("read", "") }},
		{"bridge tool", func() { reg.Defer(ToolCallName, "index") }},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("Defer with %s did not panic", tc.name)
				}
			}()
			tc.fn()
		}()
	}
}

// TestBridgeToolsExposeTheCatalog drives the three tools the model actually
// calls, including the miss paths.
func TestBridgeToolsExposeTheCatalog(t *testing.T) {
	reg, cat, ran := catalogFixture(t)
	cat.SetRunner(func(_ context.Context, name string, args json.RawMessage) (Result, error) {
		inner, _ := reg.Get(name)
		return inner.Execute(context.Background(), args)
	})
	ctx := context.Background()

	search := NewToolSearchTool(cat)
	if search.Name() != ToolSearchName || len(search.Parameters()) == 0 {
		t.Fatalf("tool_search registration: %s %s", search.Name(), search.Parameters())
	}
	res, err := search.Execute(ctx, json.RawMessage(`{"query":"code"}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, "ast_grep") || !strings.Contains(res.Text, ToolCallName) {
		t.Fatalf("search hit: %+v err=%v", res, err)
	}
	res, err = search.Execute(ctx, json.RawMessage(`{"query":"zzz"}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, "no deferred tool matches") {
		t.Fatalf("search miss: %+v err=%v", res, err)
	}
	if res, err = search.Execute(ctx, json.RawMessage(`{`)); err != nil || !res.IsError {
		t.Fatalf("malformed search args must fail: %+v err=%v", res, err)
	}

	describe := NewToolDescribeTool(cat)
	if res, err = describe.Execute(ctx, json.RawMessage(`{"name":"github"}`)); err != nil || res.IsError || !strings.Contains(res.Text, "GitHub operations") {
		t.Fatalf("describe hit: %+v err=%v", res, err)
	}
	if res, err = describe.Execute(ctx, json.RawMessage(`{"name":"zzz"}`)); err != nil || !res.IsError || !strings.Contains(res.Text, ToolSearchName) {
		t.Fatalf("describe miss must hint at the search tool: %+v err=%v", res, err)
	}

	call := NewToolCallTool(cat)
	if res, err = call.Execute(ctx, json.RawMessage(`{"name":"github","args":{"q":"1"}}`)); err != nil || res.IsError {
		t.Fatalf("bridge call: %+v err=%v", res, err)
	}
	if res, err = call.Execute(ctx, json.RawMessage(`{"name":"zzz"}`)); err != nil || !res.IsError || !strings.Contains(res.Text, ToolSearchName) {
		t.Fatalf("bridge miss must hint at the search tool: %+v err=%v", res, err)
	}
	if len(*ran) != 1 || (*ran)[0] != `github {"q":"1"}` {
		t.Fatalf("ran = %v", *ran)
	}
}
