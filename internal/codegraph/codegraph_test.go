package codegraph

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// statusCalls counts identity probes across every server in the test binary,
// so TestVerifyIsCachedPerSession can assert on it without each fake owning
// its own counter.
var statusCalls atomic.Int64

// fakeServer is a LeanKG-shaped stub: /api/v1/status and /api/v1/query, with
// the project selector recorded so a test can prove it was sent.
type fakeServer struct {
	t          *testing.T
	projectDir string
	// failQuery makes the query endpoint unavailable (503).
	failQuery  bool
	sawProject string
	queries    []map[string]any
}

func newFake(t *testing.T) (*fakeServer, *httptest.Server) {
	t.Helper()
	f := &fakeServer{t: t, projectDir: "/repo"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sawProject = r.URL.Query().Get("project")
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status":
			statusCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"project_dir": f.projectDir, "elements": 42})
		case "/api/v1/query":
			if f.failQuery {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode query body: %v", err)
			}
			f.queries = append(f.queries, body)
			action, _ := body["action"].(string)
			if action == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"freshness": "fresh",
					"hits": []any{map[string]any{
						"name": "Target", "qualified_name": "pkg/target.go::Target",
						"element_type": "function", "file_path": "pkg/target.go", "line_start": 12,
					}},
					"retrieval": map[string]any{"rung": "L1"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"freshness": "fresh",
				"callers": []any{
					map[string]any{"qualified_name": "pkg/a.go::callsTarget"},
					map[string]any{"qualified_name": "pkg/b.go::alsoCalls"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// TestQuerySendsTheProjectSelector pins the rule the whole client rests on:
// without ?project= a LeanKG server answers for its own default store.
func TestQuerySendsTheProjectSelector(t *testing.T) {
	f, srv := newFake(t)
	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo"})
	if _, err := c.Query(context.Background(), Request{Query: "Target"}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if f.sawProject != "xdev" {
		t.Fatalf("project selector = %q, want xdev", f.sawProject)
	}
}

// TestQueryRefusesTheWrongRepository is the guard this package exists for.
// Measured 2026-10-02: a single-project server ignores ?project= and answers
// with another repository's graph — a 200 with confident, wrong elements.
// For a tool whose job is "what depends on this file" that is worse than a
// failure, so the client compares project_dir before trusting a result.
func TestQueryRefusesTheWrongRepository(t *testing.T) {
	f, srv := newFake(t)
	f.projectDir = "/some/other/repo"
	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo"})

	_, err := c.Query(context.Background(), Request{Query: "Target"})
	if err == nil {
		t.Fatal("a server answering for another repository must not be trusted")
	}
	var unavail *UnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("err = %v, want *UnavailableError", err)
	}
	if !strings.Contains(err.Error(), "grep") {
		t.Fatalf("the message must tell the model what to do instead, got %q", err)
	}
	if len(f.queries) != 0 {
		t.Fatalf("the query was sent despite the identity check (%d queries)", len(f.queries))
	}
}

func TestQuerySurfacesAnUnhealthyServer(t *testing.T) {
	f, srv := newFake(t)
	f.failQuery = true
	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo"})
	// status still answers, so the failure must surface from the query itself.
	c.verifyDone = true // skip the identity probe; we are testing the query path
	_, err := c.Query(context.Background(), Request{Query: "Target"})
	if err == nil {
		t.Fatal("a 503 from the graph must be an error, not an empty answer")
	}
}

func TestQueryUnconfiguredIsNotAnErrorString(t *testing.T) {
	c := New(Config{})
	if _, err := c.Query(context.Background(), Request{Query: "x"}); err != ErrUnconfigured {
		t.Fatalf("err = %v, want ErrUnconfigured", err)
	}
}

func TestQueryPassesCallerArgs(t *testing.T) {
	f, srv := newFake(t)
	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo"})
	_, err := c.Query(context.Background(), Request{
		Action: "impact", Query: "pkg/target.go::Target",
		Args: map[string]any{"depth": 3},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	last := f.queries[len(f.queries)-1]
	if last["action"] != "impact" {
		t.Errorf("action = %v", last["action"])
	}
	args, _ := last["args"].(map[string]any)
	if args["depth"] != float64(3) {
		t.Errorf("args.depth = %v, want 3 — the graph verb needs it", args["depth"])
	}
}

func TestVerifyIsCachedPerSession(t *testing.T) {
	_, srv := newFake(t)
	before := statusCalls.Load()
	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo"})
	for i := 0; i < 3; i++ {
		if _, err := c.Query(context.Background(), Request{Query: "Target"}); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	// One identity probe for the whole session: the status endpoint is not
	// asked once per query.
	if got := statusCalls.Load() - before; got > 1 {
		t.Fatalf("status probed %d times for 3 queries; it must be cached", got)
	}
}

func TestLocateReturnsQualifiedNames(t *testing.T) {
	_, srv := newFake(t)
	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo"})
	hits, err := c.Locate(context.Background(), "Target", 5)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if len(hits) != 1 || hits[0].QualifiedName != "pkg/target.go::Target" {
		t.Fatalf("hits = %+v, want the qualified name the graph verbs need", hits)
	}
}

func TestQueryDoesNotHangPastItsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{"project_dir": "/repo"})
			return
		}
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo", Timeout: 100 * time.Millisecond})
	start := time.Now()
	_, err := c.Query(context.Background(), Request{Query: "Target"})
	if err == nil {
		t.Fatal("a hanging graph must not hang the session")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the client waited %s past its 100ms budget", time.Since(start))
	}
}

// TestVerifyAcceptsTwoSpellingsOfOneDirectory covers the same hazard the lsp
// side hit in production: macOS answers /tmp/x for /private/tmp/x, and a
// symlinked working directory does the same in reverse. A server bound to the
// real directory must still be recognized when the caller expects the linked
// spelling — otherwise the guard refuses a healthy server and blames the
// user's configuration.
func TestVerifyAcceptsTwoSpellingsOfOneDirectory(t *testing.T) {
	f, srv := newFake(t)
	real := t.TempDir()
	f.projectDir = real

	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	c := New(Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: link})
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("a linked spelling of the bound directory must verify, got: %v", err)
	}
}
