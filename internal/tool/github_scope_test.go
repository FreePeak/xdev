package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A search that names no repository is a search of the CURRENT repository.
//
// Live 2026-10-22: "list the open PR urls" in a session whose checkout was
// xdev came back with PRs from a dozen other repositories. The tool's schema
// promised `repo` defaulted to the current checkout, but searchArgs only
// forwarded `--repo` when the caller passed one — so an unnamed search ran
// `gh search prs` with no scope at all, across everything the token can see.
//
// These tests pin the two halves of the fix: the inside search is SCOPED,
// and the outside one asks first.

func TestGithubSearchScopeClassification(t *testing.T) {
	cases := []struct {
		name    string
		op      string
		args    githubArgs
		current string
		want    ghScope
	}{
		{"no repo names the current one", "search_prs", githubArgs{}, "FreePeak/xdev", ghScopeInside},
		{"the current repo by name", "search_prs", githubArgs{Repo: "FreePeak/xdev"}, "FreePeak/xdev", ghScopeInside},
		{"the current repo by qualifier", "search_prs", githubArgs{Query: "repo:FreePeak/xdev"}, "FreePeak/xdev", ghScopeInside},
		{"another repository", "search_prs", githubArgs{Repo: "FreePeak/qa-agent"}, "FreePeak/xdev", ghScopeOutside},
		{"another repository by qualifier", "search_prs", githubArgs{Query: "repo:FreePeak/qa-agent"}, "FreePeak/xdev", ghScopeOutside},
		{"an author is a person, not a repository", "search_prs", githubArgs{Query: "author:linhdmn"}, "FreePeak/xdev", ghScopeOutside},
		{"an organization", "search_prs", githubArgs{Query: "org:FreePeak"}, "FreePeak/xdev", ghScopeOutside},
		{"a repository search is global by nature", "search_repos", githubArgs{Query: "cli"}, "FreePeak/xdev", ghScopeOutside},
		{"an unknown boundary is not a licence", "search_prs", githubArgs{}, "", ghScopeOutside},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ghSearchScope(tc.op, tc.args, tc.current)
			if got != tc.want {
				t.Fatalf("scope = %v, want %v", got, tc.want)
			}
		})
	}
}

// The inside case: the argv carries --repo, so the search cannot wander.
func TestGithubSearchPinsTheCurrentRepository(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq("FreePeak/xdev\n", "[]")
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"search_prs","query":"widget"}`)
	if res.IsError {
		t.Fatalf("search errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (repo view, then the search)", len(calls))
	}
	if !strings.Contains(strings.Join(calls[1].args, " "), "--repo FreePeak/xdev") {
		t.Fatalf("the search was not scoped to the checkout: %q", calls[1].args)
	}
}

// The outside case with nobody to ask: the search does not run at all.
func TestGithubSearchOutsideRefusesWithoutAnAnswer(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq("FreePeak/xdev\n", "[]")
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"search_prs","query":"author:linhdmn"}`)
	if !res.IsError {
		t.Fatalf("an outside search ran without approval: %q", res.Text)
	}
	if !strings.Contains(res.Text, "FreePeak/xdev") {
		t.Fatalf("the refusal does not name the boundary: %q", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (the repo view only — no search ran)", len(calls))
	}
}

// The outside case with a user who says yes: the search runs.
func TestGithubSearchOutsideRunsAfterTheUserSaysYes(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq("FreePeak/xdev\n", "[]")
	tool := NewGithubTool(stub.repo)
	tool.Ask = &AskTool{Sink: &scopeSink{answer: ghScopeWiden}}

	res := runGithub(t, tool, `{"op":"search_prs","query":"author:linhdmn"}`)
	if res.IsError {
		t.Fatalf("an approved search errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (repo view, then the approved search)", len(calls))
	}
	if strings.Contains(strings.Join(calls[1].args, " "), "--repo") {
		t.Fatalf("an approved outside search must stay unscoped: %q", calls[1].args)
	}
}

// The outside case with a user who says no: the search does not run.
func TestGithubSearchOutsideStopsWhenTheUserSaysNo(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq("FreePeak/xdev\n", "[]")
	tool := NewGithubTool(stub.repo)
	tool.Ask = &AskTool{Sink: &scopeSink{answer: ghScopeStay}}

	res := runGithub(t, tool, `{"op":"search_prs","query":"author:linhdmn"}`)
	if !res.IsError {
		t.Fatalf("a refused search ran: %q", res.Text)
	}
	if calls := stub.calls(); len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (the repo view only)", len(calls))
	}
}

// The question the user is asked says what would be reached.
func TestGithubScopeQuestionNamesTheTarget(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq("FreePeak/xdev\n", "[]")
	tool := NewGithubTool(stub.repo)
	sink := &scopeSink{}
	tool.Ask = &AskTool{Sink: sink}

	runGithub(t, tool, `{"op":"search_prs","query":"author:linhdmn"}`)
	if sink.req.Question == "" {
		t.Fatal("the scope question was never asked")
	}
	if !strings.Contains(sink.req.Question, "author:linhdmn") {
		t.Fatalf("the question does not name the target: %q", sink.req.Question)
	}
	if len(sink.req.Options) != 2 {
		t.Fatalf("options = %d, want 2 (widen or stay)", len(sink.req.Options))
	}
}

// scopeSink answers one question with a fixed label.
type scopeSink struct {
	answer string
	req    AskRequest
}

func (s *scopeSink) Ask(ctx context.Context, req AskRequest) (AskResponse, error) {
	s.req = req
	if s.answer == "" {
		return AskResponse{}, nil
	}
	return AskResponse{Labels: []string{s.answer}}, nil
}

// The scope rule is part of the tool's contract, so the schema says it.
func TestGithubSchemaStatesTheScopeDefault(t *testing.T) {
	params := NewGithubTool("/tmp").Parameters()
	var p struct {
		Properties struct {
			Repo struct {
				Description string `json:"description"`
			} `json:"repo"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatal(err)
	}
	d := strings.ToLower(p.Properties.Repo.Description)
	if !strings.Contains(d, "current checkout") {
		t.Fatalf("the repo argument does not state the default: %q", p.Properties.Repo.Description)
	}
	if !strings.Contains(d, "ask") {
		t.Fatalf("the repo argument does not say that leaving the checkout needs approval: %q", p.Properties.Repo.Description)
	}
}
