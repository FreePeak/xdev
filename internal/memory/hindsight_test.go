package memory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// fixtureRecord is one request the fake Hindsight server saw.
type fixtureRecord struct {
	method string
	path   string
	query  string
	body   map[string]any
	auth   string
}

// hindsightFixture is a fake Hindsight server: it records every request and
// answers the five endpoints the backend uses.
type hindsightFixture struct {
	mu       sync.Mutex
	requests []fixtureRecord
	// results is the recall answer.
	results []recallResult
	// reflectText is the reflect answer.
	reflectText string
	// status, when non-zero, is returned for every request (degraded mode).
	status int
}

func (f *hindsightFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, fixtureRecord{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: body, auth: r.Header.Get("Authorization")})
	status := f.status
	results, reflectText := f.results, f.reflectText
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"fixture degraded"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/health":
		_, _ = w.Write([]byte(`{"status":"ok","version":"0.4.0"}`))
	case strings.HasSuffix(r.URL.Path, "/stats"):
		_, _ = w.Write([]byte(`{"memories":7,"entities":3}`))
	case strings.HasSuffix(r.URL.Path, "/memories/recall"):
		_ = json.NewEncoder(w).Encode(recallResponse{Results: results})
	case strings.HasSuffix(r.URL.Path, "/reflect"):
		_ = json.NewEncoder(w).Encode(reflectResponse{Text: reflectText})
	case strings.HasSuffix(r.URL.Path, "/memories"):
		items, _ := body["items"].([]any)
		_ = json.NewEncoder(w).Encode(retainResponse{Success: true, BankID: "xdev", ItemsCount: len(items), Async: true, OperationIDs: []string{"op-1"}})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"no such route"}`))
	}
}

func (f *hindsightFixture) calls(suffix string) []fixtureRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fixtureRecord
	for _, r := range f.requests {
		if strings.HasSuffix(r.path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *hindsightFixture) all() []fixtureRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fixtureRecord(nil), f.requests...)
}

// repoRoot builds a directory that looks like a repository.
func repoRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// newFixtureBackend starts the fake server and returns a backend pointed at
// it, with a captured log and a controllable clock.
func newFixtureBackend(t *testing.T, cfg HindsightConfig, mutate func(*hindsightFixture)) (*Hindsight, *hindsightFixture, *[]string, *time.Time) {
	t.Helper()
	f := &hindsightFixture{reflectText: "the answer from memory", results: []recallResult{{ID: "m1", Text: "Alice prefers tabs", Type: "fact", Tags: []string{"project:repo"}, Scores: map[string]float64{"similarity": 0.9}}}}
	if mutate != nil {
		mutate(f)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	var logs []string
	var mu sync.Mutex
	clock := time.Unix(1_700_000_000, 0)
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	cfg.URL = srv.URL
	cfg.HTTPClient = srv.Client()
	cfg.Logf = logf
	cfg.Now = func() time.Time { return clock }
	if cfg.ProjectRoot == "" {
		cfg.ProjectRoot = repoRoot(t, "MyRepo")
	}
	return NewHindsight(cfg), f, &logs, &clock
}

func TestHindsightRecallRequestShape(t *testing.T) {
	h, f, _, _ := newFixtureBackend(t, HindsightConfig{}, nil)
	notes, err := h.Recall("  what does\nAlice prefer?  ")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(notes) != 1 || notes[0].Text != "Alice prefers tabs" || notes[0].ID != "m1" || notes[0].Score != 0.9 {
		t.Fatalf("parsed notes = %+v", notes)
	}
	calls := f.calls("/memories/recall")
	if len(calls) != 1 {
		t.Fatalf("recall requests = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if want := "/v1/default/banks/xdev/memories/recall"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if got.body["query"] != "what does Alice prefer?" {
		t.Errorf("query = %v, want the whitespace-collapsed query", got.body["query"])
	}
	if got.body["max_tokens"] != float64(DefaultHindsightRecallMaxTokens) {
		t.Errorf("max_tokens = %v", got.body["max_tokens"])
	}
	if got.body["budget"] != "mid" {
		t.Errorf("budget = %v, want mid", got.body["budget"])
	}
	if got.body["tags_match"] != "any" {
		t.Errorf("tags_match = %v, want any (project-tagged + untagged global)", got.body["tags_match"])
	}
	if tags, _ := got.body["tags"].([]any); len(tags) != 1 || tags[0] != "project:myrepo" {
		t.Errorf("tags = %v, want [project:myrepo]", got.body["tags"])
	}
}

// TestHindsightProjectSelectorRidesBankPathsOnly: the selector is LeanKG's
// multi-project routing key, so it belongs on the bank paths. /health is
// served by the server itself, outside any project — a selector there would
// 404 a healthy multi-project server. And an empty selector must produce the
// exact URLs a single-project server saw before this knob existed.
func TestHindsightProjectSelectorRidesBankPathsOnly(t *testing.T) {
	srv := func(t *testing.T, selector string) *hindsightFixture {
		t.Helper()
		h, f, _, _ := newFixtureBackend(t, HindsightConfig{ProjectSelector: selector}, nil)
		h.NoteUserTurn("what does Alice prefer?")
		_ = h.GuidanceBlock()
		if _, err := h.Enqueue(); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		_ = h.Stats()
		_,_ = h.Diagnose()
		return f
	}

	f := srv(t, "leankg")
	if f.calls("/health")[0].query != "" {
		t.Errorf("/health carried a selector: %q", f.calls("/health")[0].query)
	}
	for _, suffix := range []string{"/memories/recall", "/memories", "/stats"} {
		for _, got := range f.calls(suffix) {
			if got.query != "project=leankg" {
				t.Errorf("%s %s query = %q, want project=leankg", got.method, got.path, got.query)
			}
		}
	}

	// No selector: not even a bare "?" may appear.
	bare := srv(t, "")
	for _, got := range bare.all() {
		if got.query != "" {
			t.Errorf("%s %s grew a query with no selector configured: %q", got.method, got.path, got.query)
		}
	}
}

// TestHindsightLeanKGContractShape pins the exact wire the LeanKG pairing
// uses — bank omp, project-scoped tag, multi-project selector — so a change
// that silently retargets the bank or drops the selector fails here instead
// of looking like an empty memory on the live server.
func TestHindsightLeanKGContractShape(t *testing.T) {
	root := repoRoot(t, "xdev")
	h, f, _, _ := newFixtureBackend(t, HindsightConfig{
		BankID:          "omp",
		ProjectSelector: "/Users/me/work/freepeak/leankg",
		Scoping:         HindsightScopingTagged,
		ProjectRoot:     root,
	}, nil)
	_ = h.GuidanceBlock()

	got := f.calls("/memories/recall")[0]
	if want := "/v1/default/banks/omp/memories/recall"; got.path != want {
		t.Errorf("path = %q, want the configured bank %q", got.path, want)
	}
	if got.query != "project="+url.QueryEscape("/Users/me/work/freepeak/leankg") {
		t.Errorf("query = %q, want the encoded selector", got.query)
	}
	if tags, _ := got.body["tags"].([]any); len(tags) != 1 || tags[0] != "project:xdev" {
		t.Errorf("tags = %v, want [project:xdev]", got.body["tags"])
	}
}

// TestHindsightWarnsOnceWhenProjectScopeIsUnresolvable: outside a repository
// ProjectScope falls back to the directory itself, so the tag becomes a name
// nothing else on the server carries and recall returns nothing — measured
// against LeanKG, a tag no entry holds yields 0 results rather than a global
// answer. The emptiness must be said once, and said not at all once a
// selector names the project.
func TestHindsightWarnsOnceWhenProjectScopeIsUnresolvable(t *testing.T) {
	// A temp dir with no .git above it: ProjectScope falls back to the dir.
	h, _, logs, _ := newFixtureBackend(t, HindsightConfig{ProjectRoot: t.TempDir()}, nil)
	_ = h.AutoRecall()
	_ = h.AutoRecall()
	scopeWarns := 0
	for _, m := range *logs {
		if strings.Contains(m, "not inside a repository") {
			scopeWarns++
		}
	}
	if scopeWarns != 1 {
		t.Errorf("scope warnings = %d in %v, want exactly 1", scopeWarns, *logs)
	}

	// A named selector resolves the scope: nothing to warn about.
	named, _, quiet, _ := newFixtureBackend(t, HindsightConfig{ProjectRoot: t.TempDir(), ProjectSelector: "leankg"}, nil)
	_ = named.AutoRecall()
	for _, m := range *quiet {
		if strings.Contains(m, "not inside a repository") {
			t.Errorf("warned despite a selector: %v", *quiet)
		}
	}
}

func TestHindsightProjectScope(t *testing.T) {
	upper := repoRoot(t, "MyRepo")
	root, tag, id := ProjectScope(upper)
	if root != upper {
		t.Errorf("root = %q, want %q", root, upper)
	}
	if tag != "project:myrepo" {
		t.Errorf("tag = %q, want the lower-cased literal project:myrepo", tag)
	}
	if len(id) != 12 {
		t.Errorf("id = %q, want a 12-hex path hash", id)
	}
	if _, _, again := ProjectScope(upper); again != id {
		t.Errorf("project id is not stable: %q vs %q", id, again)
	}

	// A linked worktree resolves to the primary checkout root, so one
	// repository keeps one scope.
	main := repoRoot(t, "main")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(wt, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wtRoot, wtTag, wtID := ProjectScope(wt)
	if wtRoot != main || wtTag != "project:main" || wtID != ProjectScope2(main) {
		t.Errorf("worktree scope = (%q,%q,%q), want the primary checkout %q", wtRoot, wtTag, wtID, main)
	}

	// No repository above: the directory is its own scope.
	plain := filepath.Join(t.TempDir(), "Plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	pRoot, pTag, _ := ProjectScope(plain)
	if pRoot != plain || pTag != "project:plain" {
		t.Errorf("plain dir scope = (%q,%q)", pRoot, pTag)
	}
}

// ProjectScope2 is a readability helper for the id comparison above.
func ProjectScope2(root string) string { _, _, id := ProjectScope(root); return id }

func TestHindsightScopingModes(t *testing.T) {
	root := repoRoot(t, "Tagged")
	_, tag, id := ProjectScope(root)

	tagged, f, _, _ := newFixtureBackend(t, HindsightConfig{ProjectRoot: root}, nil)
	if bank, got := tagged.Scope(); bank != DefaultHindsightBank || got != tag {
		t.Fatalf("per-project-tagged scope = (%q,%q), want (%q,%q)", bank, got, DefaultHindsightBank, tag)
	}
	if err := tagged.Retain("remember this"); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	items := f.calls("/memories")[0].body["items"].([]any)
	item := items[0].(map[string]any)
	if tags, _ := item["tags"].([]any); len(tags) != 1 || tags[0] != tag {
		t.Errorf("retain tags = %v, want [%s]", item["tags"], tag)
	}
	if item["timestamp"] == "" || item["timestamp"] == nil {
		t.Errorf("retain item has no timestamp: %v", item)
	}

	perProject, f2, _, _ := newFixtureBackend(t, HindsightConfig{ProjectRoot: root, Scoping: "per-project"}, nil)
	if bank, _ := perProject.Scope(); bank != DefaultHindsightBank+"-"+id {
		t.Errorf("per-project bank = %q, want %q", bank, DefaultHindsightBank+"-"+id)
	}

	global, f3, _, _ := newFixtureBackend(t, HindsightConfig{ProjectRoot: root, Scoping: "global"}, nil)
	if bank, got := global.Scope(); bank != DefaultHindsightBank || got != "" {
		t.Errorf("global scope = (%q,%q), want the shared untagged bank", bank, got)
	}
	if err := global.Retain("global fact"); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	globalItem := f3.calls("/memories")[0].body["items"].([]any)[0].(map[string]any)
	if _, has := globalItem["tags"]; has {
		t.Errorf("global retain carries tags: %v", globalItem)
	}
	if _, err := global.Recall("anything"); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if _, err := perProject.Recall("scoped query"); err != nil {
		t.Fatalf("per-project Recall: %v", err)
	}
	scoped := f2.calls("/memories/recall")[0]
	if want := "/v1/default/banks/" + DefaultHindsightBank + "-" + id + "/memories/recall"; scoped.path != want {
		t.Errorf("per-project recall path = %q, want %q", scoped.path, want)
	}
}

func TestHindsightAutoRecallFirstTurnIsBoundedAndCached(t *testing.T) {
	huge := strings.Repeat("long memory text ", 2000)
	h, f, _, clock := newFixtureBackend(t, HindsightConfig{}, func(f *hindsightFixture) {
		f.results = []recallResult{{ID: "m1", Text: "Alice prefers tabs"}, {ID: "m2", Text: huge}}
	})

	block := h.GuidanceBlock()
	if !strings.Contains(block, "# Memory Guidance (Hindsight bank xdev, tag project:myrepo)") {
		t.Fatalf("guidance block missing the scope header:\n%s", block)
	}
	if !strings.Contains(block, "Alice prefers tabs") {
		t.Errorf("guidance block lost the recalled memory:\n%s", block)
	}
	if len(block) > h.injectionChars()+600 {
		t.Errorf("injected block = %d bytes, over the %d-char cap", len(block), h.injectionChars())
	}
	if !strings.Contains(block, "recall truncated") {
		t.Errorf("oversized recall was not clipped with a marker:\n%s", block)
	}
	if n := len(f.calls("/memories/recall")); n != 1 {
		t.Fatalf("recall requests = %d, want 1 (first turn only)", n)
	}

	// The prompt is rebuilt at every turn boundary: the block must stay
	// stable and cheap inside the TTL.
	if again := h.GuidanceBlock(); again != block {
		t.Errorf("guidance block changed within the TTL")
	}
	if n := len(f.calls("/memories/recall")); n != 1 {
		t.Fatalf("recall requests = %d, want the cached block reused", n)
	}
	*clock = clock.Add(DefaultHindsightRecallTTL + time.Second)
	_ = h.GuidanceBlock()
	if n := len(f.calls("/memories/recall")); n != 2 {
		t.Fatalf("recall requests = %d, want a refresh after the TTL", n)
	}

	// autoRecall off: no request at all.
	off, f2, _, _ := newFixtureBackend(t, HindsightConfig{AutoRecall: boolPtr(false)}, nil)
	if got := off.GuidanceBlock(); got != "" {
		t.Errorf("autoRecall off still injected %q", got)
	}
	if n := len(f2.all()); n != 0 {
		t.Errorf("autoRecall off issued %d request(s)", n)
	}
}

func TestHindsightAutoRetainCadence(t *testing.T) {
	h, f, _, _ := newFixtureBackend(t, HindsightConfig{}, nil)
	h.NoteUserTurn("first turn")
	h.NoteUserTurn("second turn")
	if n := h.QueueLen(); n != 0 {
		t.Fatalf("queue = %d after 2 turns, want nothing at the default cadence of %d", n, DefaultHindsightRetainEveryNTurns)
	}
	if err := h.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	h.NoteUserTurn("third turn")

	if n := h.QueueLen(); n != 1 {
		t.Fatalf("queue = %d after 3 turns, want 1", n)
	}
	if err := h.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	retains := f.calls("/memories")
	if len(retains) != 1 {
		t.Fatalf("retain requests = %d, want 1", len(retains))
	}
	item := retains[0].body["items"].([]any)[0].(map[string]any)
	content := item["content"].(string)
	for _, want := range []string{"first turn", "second turn", "third turn"} {
		if !strings.Contains(content, want) {
			t.Errorf("full-session retain lost %q:\n%s", want, content)
		}
	}
	if retains[0].body["async"] != true {
		t.Errorf("retain async = %v, want true", retains[0].body["async"])
	}
	if tags, _ := item["tags"].([]any); len(tags) != 1 || tags[0] != "project:myrepo" {
		t.Errorf("retain tags = %v", item["tags"])
	}
	if err := h.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if n := h.QueueLen(); n != 0 {
		t.Errorf("queue = %d after a successful flush", n)
	}

	// last-turn mode retains only the turn that tripped the cadence.
	last, f2, _, _ := newFixtureBackend(t, HindsightConfig{RetainMode: HindsightRetainLast, RetainEveryNTurns: 2}, nil)
	last.NoteUserTurn("alpha")
	last.NoteUserTurn("beta")
	if err := last.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	content2 := f2.calls("/memories")[0].body["items"].([]any)[0].(map[string]any)["content"].(string)
	if content2 != "beta" {
		t.Errorf("last-turn content = %q, want beta", content2)
	}

	// autoRetain off: the cadence never queues.
	manual, f3, _, _ := newFixtureBackend(t, HindsightConfig{AutoRetain: boolPtr(false)}, nil)
	for i := 0; i < 4; i++ {
		manual.NoteUserTurn(fmt.Sprintf("turn %d", i))
	}
	if n := manual.QueueLen(); n != 0 {
		t.Errorf("autoRetain off queued %d retains", n)
	}
	if n := len(f3.all()); n != 0 {
		t.Errorf("autoRetain off issued %d request(s)", n)
	}
}

func TestHindsightServerDownDegradesAndQueueIsBounded(t *testing.T) {
	h, f, logs, _ := newFixtureBackend(t, HindsightConfig{QueueLimit: 3}, func(f *hindsightFixture) {
		f.status = http.StatusServiceUnavailable
	})

	if got := h.GuidanceBlock(); got != "" {
		t.Errorf("degraded recall injected %q, want nothing", got)
	}
	if _, err := h.Recall("anything"); err == nil {
		t.Error("degraded Recall returned no error")
	}
	if _, err := h.Reflect("anything"); err == nil {
		t.Error("degraded Reflect returned no error")
	}

	for i := 0; i < 5; i++ {
		err := h.Retain(fmt.Sprintf("item %d", i))
		if err == nil {
			t.Fatalf("degraded Retain(%d) reported success", i)
		}
		if !strings.Contains(err.Error(), "queued locally") {
			t.Errorf("degraded retain error = %v, want the queued-locally note", err)
		}
	}
	if n := h.QueueLen(); n != 3 {
		t.Fatalf("queue = %d, want the bound of 3", n)
	}
	h.mu.Lock()
	oldest := h.queue[0].Content
	h.mu.Unlock()
	if oldest != "item 2" {
		t.Errorf("queue head = %q, want the oldest dropped (item 2)", oldest)
	}

	// One warning per outage, naming the URL, however many turns fail.
	if n := len(*logs); n != 1 {
		t.Fatalf("warnings = %d (%v), want exactly 1", n, *logs)
	}
	if !strings.Contains((*logs)[0], h.cfg.URL) {
		t.Errorf("warning does not name the URL: %q", (*logs)[0])
	}

	// A reachable server re-arms the warning, so a later outage is visible.
	f.mu.Lock()
	f.status = 0
	f.mu.Unlock()
	if err := h.Flush(); err != nil {
		t.Fatalf("Flush against the recovered server: %v", err)
	}
	if n := h.QueueLen(); n != 0 {
		t.Errorf("queue = %d after recovery", n)
	}
	f.mu.Lock()
	f.status = http.StatusServiceUnavailable
	f.mu.Unlock()
	if got := h.GuidanceBlock(); got != "" {
		t.Errorf("second outage injected %q", got)
	}
	if n := len(*logs); n != 2 {
		t.Errorf("warnings = %d, want the warning re-armed after a success", n)
	}

	// A transport failure (no listener at all) degrades the same way.
	dead := NewHindsight(HindsightConfig{URL: "http://127.0.0.1:1", ProjectRoot: repoRoot(t, "Dead"), Logf: func(string, ...any) {}})
	if got := dead.GuidanceBlock(); got != "" {
		t.Errorf("unreachable server injected %q", got)
	}
	if err := dead.Retain("queued while offline"); err == nil {
		t.Error("unreachable Retain reported success")
	}
	if dead.QueueLen() != 1 {
		t.Errorf("unreachable Retain queued %d items, want 1", dead.QueueLen())
	}
}

func TestHindsightRetainReflectShapesAndTools(t *testing.T) {
	h, f, _, _ := newFixtureBackend(t, HindsightConfig{}, nil)

	if err := h.Retain("the gateway key rotates"); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	retain := f.calls("/memories")[0]
	if retain.method != http.MethodPost {
		t.Errorf("retain method = %s", retain.method)
	}
	item := retain.body["items"].([]any)[0].(map[string]any)
	if item["content"] != "the gateway key rotates" || item["context"] != "session retain" {
		t.Errorf("retain item = %v", item)
	}

	text, err := h.Reflect("what rotates?")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "the answer from memory" {
		t.Errorf("reflect text = %q", text)
	}
	reflect := f.calls("/reflect")[0]
	if reflect.path != "/v1/default/banks/xdev/reflect" || reflect.body["query"] != "what rotates?" {
		t.Errorf("reflect request = %+v", reflect)
	}
	if reflect.body["max_tokens"] != float64(DefaultHindsightRecallMaxTokens) {
		t.Errorf("reflect max_tokens = %v", reflect.body["max_tokens"])
	}

	// The model-facing trio shares the backend.
	recallTool := &RecallTool{Backend: h}
	res, err := recallTool.Execute(nil, json.RawMessage(`{"query":"alice"}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, "Alice prefers tabs") {
		t.Fatalf("recall tool = %+v err=%v", res, err)
	}
	if res, _ := recallTool.Execute(nil, json.RawMessage(`{}`)); !res.IsError {
		t.Error("recall tool accepted an empty query")
	}
	retainTool := &RetainTool{Backend: h}
	if res, _ := retainTool.Execute(nil, json.RawMessage(`{"text":"tool retained fact"}`)); res.IsError {
		t.Fatalf("retain tool failed: %+v", res)
	}
	reflectTool := &ReflectTool{Backend: h}
	if res, _ := reflectTool.Execute(nil, json.RawMessage(`{"query":"why"}`)); res.IsError || res.Text != "the answer from memory" {
		t.Fatalf("reflect tool = %+v", res)
	}
	off := &RecallTool{}
	if res, _ := off.Execute(nil, json.RawMessage(`{"query":"x"}`)); !res.IsError {
		t.Error("recall tool without a backend reported success")
	}
}

// TestHindsightEndSession: the session boundary queues the turns that never
// tripped the cadence as one final retain (a one-shot run must not be lost)
// and drains the queue.
func TestHindsightEndSession(t *testing.T) {
	h, f, _, _ := newFixtureBackend(t, HindsightConfig{}, nil)
	h.NoteUserTurn("the only user turn of a one-shot run")
	if err := h.EndSession(); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	retains := f.calls("/memories")
	if len(retains) != 1 {
		t.Fatalf("retain requests = %d, want the pending turn retained once", len(retains))
	}
	content := retains[0].body["items"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "one-shot run") {
		t.Errorf("final retain content = %q", content)
	}
	if h.QueueLen() != 0 {
		t.Errorf("queue = %d after the session-end drain", h.QueueLen())
	}
	// Nothing pending, nothing sent.
	if err := h.EndSession(); err != nil {
		t.Fatalf("second EndSession: %v", err)
	}
	if n := len(f.calls("/memories")); n != 1 {
		t.Errorf("second EndSession sent %d extra retain(s)", n-1)
	}
	// autoRetain off: the boundary only drains what is already queued.
	manual, f2, _, _ := newFixtureBackend(t, HindsightConfig{AutoRetain: boolPtr(false)}, nil)
	manual.NoteUserTurn("not retained")
	if err := manual.EndSession(); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if n := len(f2.all()); n != 0 {
		t.Errorf("autoRetain off sent %d request(s) at the session boundary", n)
	}
	// A dead server at the boundary returns the error, never a panic.
	dead := NewHindsight(HindsightConfig{URL: "http://127.0.0.1:1", ProjectRoot: repoRoot(t, "Dead3"), Logf: func(string, ...any) {}})
	dead.NoteUserTurn("offline turn")
	if err := dead.EndSession(); err == nil {
		t.Error("EndSession against a dead server reported success")
	}
	if dead.QueueLen() != 1 {
		t.Errorf("queue = %d, want the retain kept for the next boundary", dead.QueueLen())
	}
}

// TestHindsightWarnSink: print mode leaves the process logger off, so the
// one-time unreachable warning must be routable somewhere visible.
func TestHindsightWarnSink(t *testing.T) {
	var logged []string
	h := NewHindsight(HindsightConfig{URL: "http://127.0.0.1:1", ProjectRoot: repoRoot(t, "Sink"),
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})
	var sunk []string
	h.SetWarnSink(func(msg string) { sunk = append(sunk, msg) })
	_ = h.GuidanceBlock()
	if len(sunk) != 1 || !strings.Contains(sunk[0], "http://127.0.0.1:1") {
		t.Fatalf("sink messages = %v", sunk)
	}
	if len(logged) != 0 {
		t.Errorf("the logger was used despite a sink: %v", logged)
	}
	h.SetWarnSink(nil)
	if err := h.Flush(); err != nil && !strings.Contains(err.Error(), "hindsight") {
		t.Errorf("Flush error = %v", err)
	}
}

func TestHindsightClearDiagnoseStatsAndRead(t *testing.T) {
	h, f, _, _ := newFixtureBackend(t, HindsightConfig{}, nil)
	h.NoteUserTurn("a")
	h.NoteUserTurn("b")
	h.NoteUserTurn("c")
	if h.QueueLen() != 1 {
		t.Fatalf("precondition: queue = %d", h.QueueLen())
	}
	if err := h.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if h.QueueLen() != 0 {
		t.Errorf("Clear left %d queued retain(s)", h.QueueLen())
	}
	if n := len(f.calls("/memories")); n != 1 {
		t.Errorf("Clear did not drain the pending retain (requests = %d)", n)
	}
	for _, r := range f.all() {
		if r.method == http.MethodDelete {
			t.Errorf("Clear deleted server-side state: %s %s", r.method, r.path)
		}
	}

	diag := collapseWS(func() string { s, _ := h.Diagnose(); return s }())
	for _, want := range []string{"hindsight backend", "bank xdev", "tag project:myrepo", "auth none", "health ok"} {
		if !strings.Contains(diag, want) {
			t.Errorf("diagnose missing %q:\n%s", want, diag)
		}
	}
	if stats := h.Stats(); !strings.Contains(stats, "memories") || !strings.Contains(stats, "xdev") {
		t.Errorf("stats = %q", stats)
	}
	root, err := h.Read("memory://root")
	if err != nil || !strings.Contains(root, "Alice prefers tabs") {
		t.Errorf("memory://root = (%q, %v)", root, err)
	}
	if _, err := h.Read("memory://root/learned.md"); err == nil {
		t.Error("hindsight Read accepted a file path that does not exist")
	}

	// enqueue folds the pending turns into one retain and flushes it.
	h.NoteUserTurn("d")
	h.NoteUserTurn("e")
	h.NoteUserTurn("f")
	out, err := h.Enqueue()
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !strings.Contains(out, "queued") || !strings.Contains(out, "flushed") {
		t.Errorf("enqueue output = %q", out)
	}
	if h.QueueLen() != 0 {
		t.Errorf("enqueue left %d queued", h.QueueLen())
	}
	again, err := h.Enqueue()
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if !strings.Contains(again, "0 retain(s) queued") {
		t.Errorf("second enqueue output = %q", again)
	}

	// A dead server: diagnose and stats say so instead of failing.
	dead := NewHindsight(HindsightConfig{URL: "http://127.0.0.1:1", ProjectRoot: repoRoot(t, "Dead2"), Logf: func(string, ...any) {}})
	if got, _ := dead.Diagnose(); !strings.Contains(got, "unreachable") {
		t.Errorf("dead diagnose = %q", got)
	}
	if got := dead.Stats(); !strings.Contains(got, "server unreachable") {
		t.Errorf("dead stats = %q", got)
	}
	summary, lessons := dead.Paths()
	if summary == "" || !strings.Contains(lessons, "project:dead2") {
		t.Errorf("dead paths = (%q,%q)", summary, lessons)
	}
}

func TestHindsightConfigFromSettingsAndEnv(t *testing.T) {
	t.Setenv("HINDSIGHT_URL", "http://example.test:9000/")
	t.Setenv("HINDSIGHT_TOKEN", "tok-123")
	t.Setenv("HINDSIGHT_TIMEOUT", "45s")
	t.Setenv("HINDSIGHT_AUTO_RECALL", "0")
	t.Setenv("HINDSIGHT_RETAIN_EVERY_N_TURNS", "5")
	t.Setenv("HINDSIGHT_RECALL_BUDGET", "nonsense")
	t.Setenv("HINDSIGHT_SCOPING", "global")
	t.Setenv("HINDSIGHT_PROJECT", "leankg")

	settings := &config.Settings{Memory: "hindsight", Hindsight: config.HindsightSettings{
		APIURL:              "http://ignored-by-env:1",
		APIToken:            "settings-token",
		ProjectSelector:     "from-settings",
		BankID:              "team",
		RecallMaxTokens:     2048,
		RetainEveryNTurns:   9,
		InjectionTokenLimit: 512,
		RequestTimeoutMS:    1500,
	}}
	h := NewHindsight(HindsightConfigFromSettings(settings, repoRoot(t, "MyRepo")))

	if h.cfg.ProjectSelector != "leankg" {
		t.Errorf("HINDSIGHT_PROJECT = %q, want it to beat the settings value", h.cfg.ProjectSelector)
	}
	if h.cfg.URL != "http://example.test:9000" {
		t.Errorf("env URL did not win: %q", h.cfg.URL)
	}
	if h.cfg.Token != "tok-123" {
		t.Errorf("env token did not win: %q", h.cfg.Token)
	}
	if h.cfg.RequestTimeout != 45*time.Second {
		t.Errorf("HINDSIGHT_TIMEOUT = %s, want 45s", h.cfg.RequestTimeout)
	}
	if h.autoRecall() {
		t.Error("HINDSIGHT_AUTO_RECALL=0 did not disable the first-turn recall")
	}
	if h.cfg.RetainEveryNTurns != 5 {
		t.Errorf("env cadence = %d, want 5", h.cfg.RetainEveryNTurns)
	}
	if h.cfg.RecallBudget != "mid" {
		t.Errorf("invalid env enum was accepted: %q", h.cfg.RecallBudget)
	}
	if h.cfg.Scoping != "global" || h.tag != "" {
		t.Errorf("env scoping = %q (tag %q)", h.cfg.Scoping, h.tag)
	}
	if h.cfg.RecallMaxTokens != 2048 || h.cfg.InjectionTokenLimit != 512 {
		t.Errorf("settings knobs lost: %+v", h.cfg)
	}
	if h.cfg.RequestTimeout != 45*time.Second {
		t.Errorf("HINDSIGHT_TIMEOUT should override requestTimeoutMs, got %s", h.cfg.RequestTimeout)
	}
	if bank, _ := h.Scope(); bank != "team" {
		t.Errorf("bank = %q, want the settings bank base", bank)
	}
	if !h.autoRetain() {
		t.Error("autoRetain should default on")
	}

	// Unreachable-URL warning names the configured URL, and the backend is
	// never "off" once hindsight is selected.
	var warned []string
	bare := NewHindsight(HindsightConfig{URL: "http://127.0.0.1:1", Logf: func(f string, a ...any) {
		warned = append(warned, fmt.Sprintf(f, a...))
	}})
	if bare.Off() {
		t.Error("a selected hindsight backend must not be off")
	}
	_ = bare.GuidanceBlock()
	if len(warned) != 1 || !strings.Contains(warned[0], "http://127.0.0.1:1") {
		t.Errorf("warnings = %v", warned)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestHindsightRecallQueryUsesRecentTurns: recallContextTurns decides how
// many recent user turns seed the recall query (the knob must not be inert).
func TestHindsightRecallQueryUsesRecentTurns(t *testing.T) {
	h, f, _, _ := newFixtureBackend(t, HindsightConfig{RecallContextTurns: 2}, nil)
	h.NoteUserTurn("first request")
	h.NoteUserTurn("second request")
	if got := h.GuidanceBlock(); got == "" {
		t.Fatal("no guidance block")
	}
	query, _ := f.calls("/memories/recall")[0].body["query"].(string)
	if !strings.Contains(query, "first request") || !strings.Contains(query, "second request") {
		t.Errorf("recall query = %q, want the two most recent turns", query)
	}
}
