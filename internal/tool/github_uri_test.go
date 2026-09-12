package tool

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseGithubURI(t *testing.T) {
	cases := []struct {
		uri     string
		scheme  string
		repo    string
		num     int
		wantErr bool
	}{
		{uri: "pr://49", scheme: "pr", num: 49},
		{uri: "pr://049", scheme: "pr", num: 49},
		{uri: "issue://FreePeak/xdev/7", scheme: "issue", repo: "FreePeak/xdev", num: 7},
		{uri: "pr://github.com/FreePeak/xdev/7", scheme: "pr", repo: "github.com/FreePeak/xdev", num: 7},
		{uri: "issue://49/", scheme: "issue", num: 49},
		{uri: "pr://abc", wantErr: true},
		{uri: "pr://", wantErr: true},
		{uri: "pr:/49", wantErr: true},
		{uri: "svn://49", wantErr: true},
		{uri: "pr://0", wantErr: true},
		{uri: "pr://-3", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseGithubURI(tc.uri)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: expected an error, got %+v", tc.uri, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.uri, err)
		}
		if got.Scheme != tc.scheme || got.Number != tc.num || got.Repo != tc.repo {
			t.Fatalf("%s = %+v, want {%s %d %s}", tc.uri, got, tc.scheme, tc.num, tc.repo)
		}
	}
	if got := (githubURI{Scheme: "pr", Number: 49}).cacheKey(); got != "pr://49" {
		t.Fatalf("cacheKey = %q", got)
	}
	if got := (githubURI{Scheme: "pr", Repo: "FreePeak/xdev", Number: 49}).cacheKey(); got != "pr://FreePeak/xdev/49" {
		t.Fatalf("cacheKey = %q", got)
	}
}

const prReaderJSON = `{"number":49,"title":"github tool via gh","state":"OPEN","isDraft":false,"author":{"login":"ann"},"url":"https://github.com/FreePeak/xdev/pull/49","createdAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-02T00:00:00Z","body":"implements the github tool","baseRefName":"main","headRefName":"feat/github","headRefOid":"abc1234567890def","labels":[{"name":"M13"}],"comments":[{"author":{"login":"bob"},"body":"looks good","createdAt":"2026-09-02T00:00:00Z"}]}`

func TestGithubURIReadAndCache(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(prReaderJSON)
	tool := NewGithubTool(stub.repo)

	first, err := tool.readGithubURI("pr://49")
	if err != nil {
		t.Fatalf("pr://49: %v", err)
	}
	for _, want := range []string{
		"# github tool via gh",
		"pr #49 · open · opened by ann",
		"branches: main <- feat/github",
		"head: abc12345",
		"labels: M13",
		"implements the github tool",
		"## comments (1)",
		"looks good",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("reader text missing %q:\n%s", want, first)
		}
	}
	if len(stub.calls()) != 1 {
		t.Fatalf("calls = %v", stub.calls())
	}
	assertArgs(t, stub.calls()[0], "gh", stub.repo, "pr", "view", "49", "--json", ghPRReaderFields)

	second, err := tool.readGithubURI("pr://49")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if second != first {
		t.Fatalf("cached text differs:\n%s\n---\n%s", first, second)
	}
	// An equivalent spelling (zero-padded) shares the same cache row.
	if _, err := tool.readGithubURI("pr://049"); err != nil {
		t.Fatalf("pr://049: %v", err)
	}
	if len(stub.calls()) != 1 {
		t.Fatalf("cache miss: %d gh calls for one URL read", len(stub.calls()))
	}
}

func TestGithubURIIssueQualifiedRepo(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(`{"number":7,"title":"broken build","state":"CLOSED","author":{"login":"ann"},"url":"https://github.com/FreePeak/xdev/issues/7","body":"boom","labels":[{"name":"bug"}]}`)
	tool := NewGithubTool(stub.repo)

	text, err := tool.readGithubURI("issue://FreePeak/xdev/7")
	if err != nil {
		t.Fatalf("issue://FreePeak/xdev/7: %v", err)
	}
	for _, want := range []string{"# broken build", "issue #7 · closed · opened by ann", "labels: bug", "boom"} {
		if !strings.Contains(text, want) {
			t.Fatalf("reader text missing %q:\n%s", want, text)
		}
	}
	assertArgs(t, stub.calls()[0], "gh", stub.repo, "issue", "view", "7", "--repo", "FreePeak/xdev", "--json", ghIssueReaderFields)
}

func TestGithubURIReadThroughReadTool(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(prReaderJSON)
	tool := NewGithubTool(stub.repo)
	tool.RegisterURISchemes()
	rt := NewReadTool()

	res, err := rt.Execute(context.Background(), json.RawMessage(`{"path":"pr://49"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("read tool errored: %s", res.Text)
	}
	if !strings.Contains(res.Text, "1:# github tool via gh") {
		t.Fatalf("read tool text = %q", res.Text)
	}
	res2, err := rt.Execute(context.Background(), json.RawMessage(`{"path":"pr://49"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res2.IsError || res2.Text != res.Text {
		t.Fatalf("second read = %+v", res2)
	}
	if len(stub.calls()) != 1 {
		t.Fatalf("the read seam must share the tool's cache: %v", stub.calls())
	}
}

func TestGithubURIErrorSurfaces(t *testing.T) {
	stub := newGithubStub(t)
	stub.fail(1, "gh: Bad credentials (HTTP 401)\n")
	tool := NewGithubTool(stub.repo)
	tool.RegisterURISchemes()

	res, err := NewReadTool().Execute(context.Background(), json.RawMessage(`{"path":"pr://49"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "GITHUB_TOKEN") {
		t.Fatalf("401 through the read seam = %+v", res)
	}
}

func TestGithubCacheBoundedTTLAndInvalidation(t *testing.T) {
	c := newGithubCache(time.Minute, 3)
	for i := 0; i < 5; i++ {
		c.put("pr://"+strconv.Itoa(i), "text")
	}
	if len(c.entries) != 3 {
		t.Fatalf("cache holds %d rows, want the 3-row cap", len(c.entries))
	}
	if _, ok := c.get("pr://4"); !ok {
		t.Fatalf("newest entry evicted: %v", c.entries)
	}

	short := newGithubCache(10*time.Millisecond, 4)
	short.put("pr://1", "text")
	time.Sleep(25 * time.Millisecond)
	if _, ok := short.get("pr://1"); ok {
		t.Fatal("expired entry served")
	}

	inv := newGithubCache(time.Minute, 8)
	inv.put("pr://7", "a")
	inv.put("pr://FreePeak/xdev/7", "b")
	inv.put("pr://17", "c")
	inv.invalidateNumber(7)
	if _, ok := inv.get("pr://7"); ok {
		t.Fatal("pr://7 must be invalidated")
	}
	if _, ok := inv.get("pr://FreePeak/xdev/7"); ok {
		t.Fatal("repo-qualified row must be invalidated")
	}
	if _, ok := inv.get("pr://17"); !ok {
		t.Fatal("pr://17 must survive invalidating #7")
	}
}

func TestGithubReaderCapsComments(t *testing.T) {
	v := ghPRView{Number: 1, Title: "big thread", State: "OPEN"}
	for i := 0; i < 30; i++ {
		v.Comments = append(v.Comments, ghComment{Author: ghAuthor{Login: "u"}, Body: "c"})
	}
	text := renderGithubReader("pr", v)
	if !strings.Contains(text, "## comments (30)") || !strings.Contains(text, "… 10 more comments") {
		t.Fatalf("comment summary missing:\n%s", text)
	}
	if got := strings.Count(text, "\n### "); got != githubReaderCommentCap {
		t.Fatalf("rendered %d comments, want the %d-comment cap", got, githubReaderCommentCap)
	}
}
