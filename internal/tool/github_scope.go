package tool

import (
	"context"
	"fmt"
	"strings"
)

// The scope of one search relative to the session's checkout.
//
// The github tool's schema always promised that `repo` defaulted to "the
// current checkout", but only file_read honoured it: searchArgs forwarded
// `--repo` only when the caller passed one, so an unscoped `gh search prs`
// ran across every repository the token can see. The live case (2026-10-22)
// was "list the open PR urls" in a session whose checkout was xdev: the
// answer came back with PRs from a dozen other repositories, because
// nothing in the tool and nothing in the prompt said that leaving the
// checkout was a decision somebody had to make.
//
// So the scope is explicit here: a search is inside the current repository
// unless it names another repository, an owner, or the account at large —
// and leaving it asks the user first. The asking is the point: the model
// that wanted the wider answer is the one that must not get it silently.

// ghScope is a search's reach relative to the current repository.
type ghScope int

const (
	// ghScopeInside is a search confined to the current repository.
	ghScopeInside ghScope = iota
	// ghScopeOutside reaches another repository, an owner, or the account.
	ghScopeOutside
)

// ghOutside names what a search would reach beyond the current repository.
// It is the subject of the confirmation question, so it says it in words a
// human can answer yes or no to.
type ghOutside struct {
	// Desc is the target, e.g. "repository FreePeak/qa-agent" or
	// "organization FreePeak".
	Desc string
}

// ghOwnerQualifiers are the search qualifiers that widen a query beyond
// the repository the session is in. A person qualifier counts: `author:`
// names a human, and a human has pull requests in many repositories.
//
// `repo:` is deliberately NOT here: it names a repository, so it is
// compared against the current one instead (a query scoped to the current
// repository is inside it).
var ghOwnerQualifiers = []string{
	"org:", "user:", "owner:", "author:", "author-name:", "assignee:",
	"mentions:", "team:", "reviewed-by:", "commenter:", "involves:",
}

// ghSearchScope classifies one search call against the current repository.
func ghSearchScope(op string, a githubArgs, current string) (ghScope, ghOutside) {
	if current == "" {
		// The boundary is unknown. That is not a licence to search
		// everything: an unknown boundary is an outside search.
		return ghScopeOutside, ghOutside{Desc: "the current repository could not be resolved, so the search would be unscoped"}
	}
	if op == "search_repos" {
		// A repository search is global by nature: `gh search repos` has no
		// repository scope to narrow it, so it is always an outside search.
		return ghScopeOutside, ghOutside{Desc: "every repository you can see"}
	}
	explicit := normalizeRepo(a.Repo)
	if explicit != "" {
		if strings.EqualFold(explicit, current) {
			return ghScopeInside, ghOutside{}
		}
		return ghScopeOutside, ghOutside{Desc: "repository " + explicit}
	}
	q := strings.TrimSpace(a.Query)
	if repo, ok := ghQualifierValue(q, "repo:"); ok {
		if strings.EqualFold(repo, current) {
			return ghScopeInside, ghOutside{}
		}
		return ghScopeOutside, ghOutside{Desc: "repository " + repo}
	}
	for _, qual := range ghOwnerQualifiers {
		if v, ok := ghQualifierValue(q, qual); ok {
			return ghScopeOutside, ghOutside{Desc: qual + v}
		}
	}
	return ghScopeInside, ghOutside{}
}

// ghQualifierValue reads one qualifier's value out of a query. A
// qualifier-less term is not a qualifier, and an empty value is no value.
func ghQualifierValue(query, qual string) (string, bool) {
	for _, term := range strings.Fields(query) {
		if !strings.HasPrefix(term, qual) {
			continue
		}
		v := strings.Trim(strings.TrimPrefix(term, qual), `"`)
		if v != "" {
			return v, true
		}
	}
	return "", false
}

// stripRepoQualifier removes a `repo:` qualifier from a query that is
// about to be pinned with `--repo`. Leaving it in would ask gh for the
// same repository twice, and the two spellings can disagree.
func stripRepoQualifier(query string) string {
	fields := strings.Fields(query)
	kept := fields[:0]
	for _, f := range fields {
		if strings.HasPrefix(f, "repo:") {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

// The two answers to the scope question. Only "widen" proceeds: a note, an
// empty answer, a canceled turn, or nobody there at all all mean the
// search stays where it was.
const (
	ghScopeWiden = "widen"
	ghScopeStay  = "stay"
)

// confirmScope asks the user whether a search may leave the current
// repository.
//
// It returns true only for an explicit "widen". Everything else — no sink,
// no answer, a typed note, an error — is a "no": the search stays inside,
// because a wider answer the user never gave is worse than a narrower one
// they can widen with a word.
func (t *GithubTool) confirmScope(ctx context.Context, current string, out ghOutside) bool {
	sink := t.askSink()
	if sink == nil {
		return false
	}
	req := AskRequest{
		Question: fmt.Sprintf("This search reaches outside %s (%s). Run it?", ghScopeLabel(current), out.Desc),
		Options: []AskOption{
			{Label: ghScopeWiden, Description: "run the search across " + out.Desc},
			{Label: ghScopeStay, Description: "keep the search inside " + ghScopeLabel(current)},
		},
		Recommended: []string{ghScopeStay},
	}
	resp, err := sink.Ask(ctx, req)
	if err != nil {
		return false
	}
	for _, l := range resp.Labels {
		if l == ghScopeWiden {
			return true
		}
	}
	return false
}

// ghScopeLabel names the current repository for a question, including the
// case where it could not be resolved.
func ghScopeLabel(current string) string {
	if current == "" {
		return "the current repository (which could not be resolved)"
	}
	return current
}
