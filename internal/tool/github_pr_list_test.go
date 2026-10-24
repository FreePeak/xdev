package tool

import (
	"strings"
	"testing"
)

// pr_list is the op that answers "list this repository's pull requests".
//
// Live 2026-10-22 (session 65ce60d7): the model asked for the open PRs of the
// repository it was sitting in. The tool had no op for that question, so it
// reached for search_prs — a GLOBAL search — and the answer came back with PRs
// from coder0211/dau-dau and opentracing-contrib/python-django. Scoping the
// search (#743) stops a search from wandering; pr_list removes the reason to
// wander at all.

func TestGithubPRListDefaultsToTheCheckout(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq("FreePeak/xdev\n",
		`[{"number":7,"title":"a pr","url":"https://github.com/FreePeak/xdev/pull/7","headRefName":"feat/x","isDraft":true}]`)
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"pr_list"}`)
	if res.IsError {
		t.Fatalf("pr_list errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (resolve the checkout, then list)", len(calls))
	}
	assertArgs(t, calls[1], "gh", stub.repo,
		"pr", "list", "--repo", "FreePeak/xdev", "--state", "open",
		"--json", ghPRListFields, "--limit", "30")
	for _, want := range []string{"#7", "a pr", "https://github.com/FreePeak/xdev/pull/7", "draft"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("pr_list output %q does not mention %q", res.Text, want)
		}
	}
}

// An explicit repo is the caller's decision: it is used as given, and no
// checkout resolution runs.
func TestGithubPRListHonoursAnExplicitRepoAndState(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(`[]`)
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"pr_list","repo":"other/repo","state":"merged","limit":5}`)
	if res.IsError {
		t.Fatalf("pr_list errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (an explicit repo is not re-resolved)", len(calls))
	}
	assertArgs(t, calls[0], "gh", stub.repo,
		"pr", "list", "--repo", "other/repo", "--state", "merged",
		"--json", ghPRListFields, "--limit", "5")
	if !strings.Contains(res.Text, "merged") {
		t.Fatalf("an empty listing must say which state was empty: %q", res.Text)
	}
}

// A state gh does not know is refused before a call is spent on it.
func TestGithubPRListRejectsAnUnknownState(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(`[]`)
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"pr_list","state":"soon"}`)
	if !res.IsError {
		t.Fatalf("an unknown state must fail, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "soon") {
		t.Fatalf("the error must name the state it refused: %q", res.Text)
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("a refused state still spent %d gh call(s): %v", len(calls), calls)
	}
}

// From a directory that is not a checkout and with no repo named, the listing
// cannot be scoped, and the tool says so instead of listing everything.
func TestGithubPRListWithoutACheckoutFails(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond("")
	stub.fail(1, "not a git repository")
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"pr_list"}`)
	if !res.IsError {
		t.Fatalf("an unscopable listing must fail, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "repo") {
		t.Fatalf("the error must name the repo it could not resolve: %q", res.Text)
	}
}
