package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fake records every invocation and answers from a route table keyed by the
// joined args. The recording is the point: several tests below assert on the
// exact argv, because the argv IS the contract with opencode (R2 — we drive
// their CLI, we never touch their files).
type fake struct {
	calls  [][]string
	routes map[string]reply
}

type reply struct {
	stdout string
	err    error
}

func (f *fake) exec(ctx context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	// Longest-prefix match, so a route can name just the operation
	// ("api session.list") while the client appends --param flags after it.
	var best string
	for k := range f.routes {
		if strings.HasPrefix(joined, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return nil, errors.New("fake: no route for " + joined)
	}
	r := f.routes[best]
	return []byte(r.stdout), r.err
}

func (f *fake) argv(i int) string {
	if i >= len(f.calls) {
		return "<no call>"
	}
	return strings.Join(f.calls[i], " ")
}

func testClient(routes map[string]reply) (*Client, *fake) {
	f := &fake{routes: routes}
	return &Client{Exec: f.exec}, f
}

const upURL = "http://127.0.0.1:49374"

// --- F8: discovery degrades to unknown, never fabricates -------------------

func TestDiscoverUsesOnlySanctionedCommands(t *testing.T) {
	c, f := testClient(map[string]reply{
		"service status":       {stdout: upURL + "\n"},
		"service get password": {stdout: "s3cr3t\n"},
	})
	ep, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ep.URL != upURL || ep.Password != "s3cr3t" {
		t.Fatalf("got %+v", ep)
	}
	if len(f.calls) != 2 {
		t.Fatalf("want exactly 2 calls, got %d: %v", len(f.calls), f.calls)
	}
	if got := f.argv(0); got != "service status" {
		t.Errorf("call 0 = %q, want %q", got, "service status")
	}
	if got := f.argv(1); got != "service get password" {
		t.Errorf("call 1 = %q, want %q", got, "service get password")
	}
}

func TestDiscoverUnknownWhenStatusFails(t *testing.T) {
	c, _ := testClient(map[string]reply{
		"service status":       {err: errors.New("not running")},
		"service get password": {stdout: "s3cr3t\n"},
	})
	ep, err := c.Discover(context.Background())
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, want ErrNoServer", err)
	}
	if ep.URL != "" || ep.Password != "" {
		t.Fatalf("want a zero Endpoint, got %+v", ep)
	}
}

func TestDiscoverUnknownWhenPasswordFails(t *testing.T) {
	c, _ := testClient(map[string]reply{
		"service status":       {stdout: upURL + "\n"},
		"service get password": {stdout: ""},
	})
	ep, err := c.Discover(context.Background())
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, want ErrNoServer", err)
	}
	if ep.URL != "" {
		t.Fatalf("partial URL leaked into the Endpoint: %+v", ep)
	}
}

// The whole point of going through `service get password` is that xdev never
// opens opencode's config directory. A non-loopback URL is refused outright:
// xdev attaches to a local server, never to something on the network.
func TestDiscoverRejectsNonLoopbackURL(t *testing.T) {
	for _, u := range []string{
		"http://10.0.0.5:4096", "http://example.com:4096", "https://0.0.0.0:4096",
	} {
		c, _ := testClient(map[string]reply{
			"service status":       {stdout: u + "\n"},
			"service get password": {stdout: "pw\n"},
		})
		if _, err := c.Discover(context.Background()); !errors.Is(err, ErrNoServer) {
			t.Errorf("url %q: err = %v, want ErrNoServer", u, err)
		}
	}
}

// --- F1: a 200 is not proof ------------------------------------------------

// opencode's web-UI handler serves index.html for every path that is not
// /api, /auth/ or /openapi.json. A mistyped path therefore returns 200 with
// text/html. Reading that as "no sessions" is the silent bug this guards.
func TestListSessionsRejectsHTMLShell(t *testing.T) {
	c, _ := testClient(map[string]reply{
		"api session.list": {stdout: "<!doctype html><html lang=\"en\"></html>"},
	})
	_, err := c.ListSessions(context.Background(), "/tmp")
	if !errors.Is(err, ErrNotJSON) {
		t.Fatalf("err = %v, want ErrNotJSON", err)
	}
}

func TestListSessionsRejectsUnauthorizedTag(t *testing.T) {
	c, _ := testClient(map[string]reply{
		"api session.list": {stdout: `{"_tag":"UnauthorizedError","message":"Authentication required"}`},
	})
	_, err := c.ListSessions(context.Background(), "/tmp")
	if !errors.Is(err, ErrAPIError) {
		t.Fatalf("err = %v, want ErrAPIError", err)
	}
}

func TestListSessionsRejectsJSONThatIsNotAnObject(t *testing.T) {
	c, _ := testClient(map[string]reply{
		"api session.list": {stdout: `["not","an","object"]`},
	})
	_, err := c.ListSessions(context.Background(), "/tmp")
	if !errors.Is(err, ErrNotJSON) {
		t.Fatalf("err = %v, want ErrNotJSON", err)
	}
}

// A well-formed error envelope from the CLI's own stderr path still has to
// surface as an error rather than as an empty session list.
func TestListSessionsRejectsNonZeroExit(t *testing.T) {
	c, _ := testClient(map[string]reply{
		"api session.list": {stdout: "", err: errors.New("HTTP 404 Not Found")},
	})
	_, err := c.ListSessions(context.Background(), "/tmp")
	if err == nil || errors.Is(err, ErrNotJSON) {
		t.Fatalf("err = %v, want a non-nil CLI error", err)
	}
}

func TestListSessionsUnwrapsData(t *testing.T) {
	c, f := testClient(map[string]reply{
		"api session.list": {stdout: `{"data":[
			{"id":"ses_a","title":"one","agent":"build","cost":0.5,
			 "tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},
			 "time":{"created":1},"location":{"directory":"/tmp"}},
			{"id":"ses_b","title":"two","agent":"plan","cost":0,
			 "tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},
			 "time":{"created":2},"location":{"directory":"/tmp"}}]}`},
	})
	got, err := c.ListSessions(context.Background(), "/tmp")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "ses_a" || got[0].Title != "one" || got[0].Agent != "build" {
		t.Errorf("row 0 = %+v", got[0])
	}
	if got[0].Cost != 0.5 || got[0].Tokens.Input != 10 || got[0].Tokens.Output != 2 {
		t.Errorf("row 0 accounting = %+v", got[0])
	}
	if got[0].Location.Directory != "/tmp" {
		t.Errorf("directory = %q", got[0].Location.Directory)
	}
	// The directory is a filter argument, and it must be passed as an OpenAPI
	// parameter rather than guessed.
	if got := f.argv(0); !strings.Contains(got, "session.list") {
		t.Errorf("argv = %q", got)
	}
}

// --- create: the directory must be in the body ------------------------------

// opencode's session.create has NO query parameters. An earlier probe passed
// `--param directory=...`, which was silently ignored and created the session
// against the server's own cwd. The directory has to travel in the body.
func TestCreateSessionSendsLocationInBody(t *testing.T) {
	c, f := testClient(map[string]reply{
		"api session.create": {stdout: `{"data":{"id":"ses_new","agent":"lean",
			"cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},
			"time":{"created":1},"location":{"directory":"/w"}}}`},
	})
	got, err := c.CreateSession(context.Background(), CreateRequest{
		Agent: "lean", Directory: "/w", Title: "t",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got.ID != "ses_new" {
		t.Fatalf("id = %q", got.ID)
	}
	call := f.calls[0]
	joined := strings.Join(call, " ")
	if strings.Contains(joined, "--param") || strings.Contains(joined, "directory=/w") {
		t.Errorf("directory must not travel as a CLI param: %v", call)
	}
	var body CreateRequest
	if i := indexOf(call, "-d"); i >= 0 {
		if err := json.Unmarshal([]byte(call[i+1]), &body); err != nil {
			t.Fatalf("body is not JSON: %v (%q)", err, call[i+1])
		}
	} else {
		t.Fatalf("no -d in %v", call)
	}
	if body.Agent != "lean" || body.Location.Directory != "/w" {
		t.Errorf("body = %+v", body)
	}
}

func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

func TestCreateSessionRejectsEmptyAgent(t *testing.T) {
	c, f := testClient(map[string]reply{})
	_, err := c.CreateSession(context.Background(), CreateRequest{Directory: "/w"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("must not shell out on an invalid request: %v", f.calls)
	}
}

// --- prompt ----------------------------------------------------------------

func TestPromptPostsTextToOperationID(t *testing.T) {
	c, f := testClient(map[string]reply{"api session.prompt": {stdout: "{}"}})
	if err := c.Prompt(context.Background(), "ses_a", "do the thing"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	joined := strings.Join(f.calls[0], " ")
	if !strings.Contains(joined, "session.prompt") {
		t.Errorf("argv = %q, want the operation id", joined)
	}
	if !strings.Contains(joined, "do the thing") {
		t.Errorf("argv = %q, want the text in the body", joined)
	}
}

func TestPromptRejectsEmptyText(t *testing.T) {
	c, f := testClient(map[string]reply{})
	if err := c.Prompt(context.Background(), "ses_a", "   "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("must not shell out: %v", f.calls)
	}
}

func TestPromptRejectsEmptySessionID(t *testing.T) {
	c, _ := testClient(map[string]reply{})
	if err := c.Prompt(context.Background(), "", "hi"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}
