package agent

import (
	"sort"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
)

// Fallback chains (M5 #25, docs/research/omp-context-resilience §1.6).
//
// retry.fallbackChains maps a chain KEY to an ordered list of model refs.
// Keys and entries share one grammar:
//
//	onegw/free            exact model selector (key) / literal ref (entry)
//	smol                  role name (key comes from the active role)
//	onegw/*               provider wildcard: swap provider, keep the id
//	openrouter/google/*   provider + id prefix: re-prefix the failing id
//	@slow                 role reference (entry only)
//
// Resolution specificity: exact model key → role key → provider wildcard →
// the caller's existing models.yml outage chain.

// ChainTarget is one resolved fallback as a model ref plus the metadata the
// chain needs to order itself. Provider construction (HTTP client,
// credential, auth header) stays with cmd/xdev, so the resolver works on
// refs rather than live providers and stays testable without a network.
type ChainTarget struct {
	Provider      string
	Model         string
	ContextWindow int
}

// Selector renders the target as the "provider/model" key used by cooldowns
// and model_change bookkeeping.
func (t ChainTarget) Selector() string { return t.Provider + "/" + t.Model }

// ChainCatalog is the model universe chain entries resolve against: the ids
// each provider advertises, and their context windows.
type ChainCatalog interface {
	Models(provider string) []string
	Window(provider, model string) int
}

// ConfigCatalog is the models.yml-backed ChainCatalog: the pinned per-provider
// model list plus each entry's declared context window.
type ConfigCatalog struct{ Config *config.Config }

// Models returns the provider's pinned model ids (nil when unknown).
func (c ConfigCatalog) Models(provider string) []string {
	if c.Config == nil || c.Config.Providers[provider] == nil {
		return nil
	}
	models := c.Config.Providers[provider].Models
	out := make([]string, 0, len(models))
	for _, m := range models {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// Window returns the model's declared context window (0 = unknown).
func (c ConfigCatalog) Window(provider, model string) int {
	if c.Config == nil || c.Config.Providers[provider] == nil {
		return 0
	}
	for _, m := range c.Config.Providers[provider].Models {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return 0
}

// ChainTargets projects live failover targets onto the model-ref view the
// resolver consumes; the CLI passes its existing models.yml chain as the
// last-resort rung.
func ChainTargets(ts []FailoverTarget) []ChainTarget {
	out := make([]ChainTarget, 0, len(ts))
	for _, t := range ts {
		if t.Provider == nil {
			continue
		}
		out = append(out, ChainTarget{Provider: t.Provider.Name(), Model: t.Model, ContextWindow: t.ContextWindow})
	}
	return out
}

// ResolveFallbackChain picks the fallback order for the active model
// (retry.fallbackChains). When no configured key applies — or every entry
// in the winning chain is unusable — the caller's existing chain is
// returned unchanged: an explicit chain must never leave a session with
// fewer failover targets than it had.
func ResolveFallbackChain(s *config.Settings, role, provider, model string, cat ChainCatalog, existing []ChainTarget) []ChainTarget {
	entries, ok := fallbackChainEntries(s, role, provider, model)
	if !ok {
		return dedupeTargets(existing, provider, model)
	}
	out := make([]ChainTarget, 0, len(entries))
	for _, e := range entries {
		t, ok := expandChainEntry(s, e, provider, model, cat)
		if !ok {
			logx.Errorf("retry.fallbackChains: entry %q for %s/%s resolves to no model", e, provider, model)
			continue
		}
		out = append(out, t)
	}
	out = dedupeTargets(out, provider, model)
	if len(out) == 0 {
		return dedupeTargets(existing, provider, model)
	}
	return out
}

// fallbackChainEntries returns the winning chain for the active model in
// specificity order: exact model key ("provider/model", then the bare id so
// a chain survives a role reassignment), role key, provider wildcard
// (longest prefix first).
func fallbackChainEntries(s *config.Settings, role, provider, model string) ([]string, bool) {
	if s == nil || len(s.Retry.FallbackChains) == 0 {
		return nil, false
	}
	chains := s.Retry.FallbackChains
	for _, key := range []string{provider + "/" + model, model} {
		if e := chains[key]; len(e) > 0 {
			return e, true
		}
	}
	if role != "" {
		if e := chains[role]; len(e) > 0 {
			return e, true
		}
	}
	if e := chains[provider+"/*"]; len(e) > 0 {
		return e, true
	}
	// provider/prefix/* — the longest prefix beats the plain wildcard.
	prefix, entries, found := "", []string(nil), false
	for key, e := range chains {
		p, ok := providerPrefixKey(key, provider)
		if !ok || len(e) == 0 {
			continue
		}
		if !found || len(p) > len(prefix) {
			prefix, entries, found = p, e, true
		}
	}
	return entries, found
}

// providerPrefixKey reports whether key is a "<provider>/<prefix>/*" chain
// key for provider, returning the prefix.
func providerPrefixKey(key, provider string) (string, bool) {
	rest, ok := strings.CutPrefix(key, provider+"/")
	if !ok {
		return "", false
	}
	prefix, ok := strings.CutSuffix(rest, "/*")
	if !ok || strings.Contains(prefix, "*") || strings.TrimSpace(prefix) == "" {
		return "", false
	}
	return prefix, true
}

// expandChainEntry turns one chain entry into a concrete target:
//
//	onegw/*            the failing id on another provider (fuzzy-resolved
//	                   against that provider's catalog)
//	openrouter/g/*     the failing id re-prefixed; the bare id is the
//	                   fallback when the target lacks the prefixed one
//	@role              the role's configured model, expanded as above
//	onegw/dev          a literal ref, still fuzzy-resolved so a
//	                   near-miss name points at a real model
func expandChainEntry(s *config.Settings, entry, provider, model string, cat ChainCatalog) (ChainTarget, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ChainTarget{}, false
	}
	if strings.HasPrefix(entry, "@") {
		ref, err := config.ResolveModelRef(s, entry)
		if err != nil {
			logx.Errorf("retry.fallbackChains: %v", err)
			return ChainTarget{}, false
		}
		entry = ref.Ref
	}
	prov, rest, ok := strings.Cut(entry, "/")
	if !ok || strings.TrimSpace(prov) == "" {
		return ChainTarget{}, false
	}
	var want string
	switch {
	case rest == "*":
		want = model
	case strings.HasSuffix(rest, "/*"):
		prefix := strings.TrimSuffix(rest, "/*")
		if id, ok := resolveModelID(cat, prov, prefix+"/"+model); ok {
			return target(cat, prov, id), true
		}
		want = model // aggregator→direct: the target may lack the prefix
	default:
		want = rest
	}
	id, ok := resolveModelID(cat, prov, want)
	if !ok {
		return ChainTarget{}, false
	}
	return target(cat, prov, id), true
}

// target builds a resolved target with the catalog's context window.
func target(cat ChainCatalog, provider, model string) ChainTarget {
	w := 0
	if cat != nil {
		w = cat.Window(provider, model)
	}
	return ChainTarget{Provider: provider, Model: model, ContextWindow: w}
}

// resolveModelID maps want onto one of provider's advertised ids: exact
// first, then the same near-miss ladder the fuzzy matcher uses. A nil
// catalog means "no catalog available" — the literal id is trusted, since
// refusing every entry would silently empty an explicit chain.
func resolveModelID(cat ChainCatalog, provider, want string) (string, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", false
	}
	if cat == nil {
		return want, true
	}
	ids := cat.Models(provider)
	if len(ids) == 0 {
		return want, true
	}
	return fuzzyResolve(ids, want)
}

// fuzzyResolve picks the catalog id closest to want: exact, case-folded
// exact, bare-basename exact (the "openrouter/google/x" ↔ "x" carry), then
// prefix/substring, then a bounded edit distance. Ties break on the shorter
// id, then lexicographic order — resolution must be deterministic, or a
// resumed session could fall back to a different model than it did before.
func fuzzyResolve(ids []string, want string) (string, bool) {
	if want == "" || len(ids) == 0 {
		return "", false
	}
	lw := strings.ToLower(want)
	base := lw
	if i := strings.LastIndex(lw, "/"); i >= 0 {
		base = lw[i+1:]
	}
	best, bestScore := "", -1
	for _, id := range ids {
		score := matchScore(strings.ToLower(id), lw, base)
		if score < 0 {
			continue
		}
		if score > bestScore || (score == bestScore && shorterThenLess(best, id)) {
			best, bestScore = id, score
		}
	}
	if bestScore < 0 {
		return "", false
	}
	return best, true
}

// matchScore grades one candidate (all inputs lowercased): -1 = no match.
func matchScore(id, want, wantBase string) int {
	idBase := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		idBase = id[i+1:]
	}
	switch {
	case id == want:
		return 100
	case idBase == wantBase:
		return 90
	case strings.HasPrefix(id, want), strings.HasPrefix(want, id):
		return 80
	case strings.Contains(id, want), strings.Contains(want, id):
		return 70
	case strings.HasPrefix(idBase, wantBase), strings.HasPrefix(wantBase, idBase):
		return 60
	case strings.Contains(idBase, wantBase), strings.Contains(wantBase, idBase):
		return 50
	}
	// A near-miss (a typo'd or renamed model) still resolves when the edit
	// distance is small relative to the id: scores stay well under the
	// substring band so a real prefix match always wins.
	if d := editDistance(idBase, wantBase); d <= 2 && abs(len(idBase)-len(wantBase)) <= 2 {
		return 40 - d
	}
	return -1
}

// shorterThenLess orders two candidates: shorter id first, then
// lexicographically, so equal-scoring matches resolve the same way twice.
func shorterThenLess(a, b string) bool {
	if len(a) != len(b) {
		return len(b) < len(a)
	}
	return b < a
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// editDistance is the bounded Levenshtein distance over runes.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// dedupeTargets drops targets equal to the active model and repeats,
// keeping the first occurrence (the declared order is the preference order).
func dedupeTargets(ts []ChainTarget, provider, model string) []ChainTarget {
	seen := map[string]bool{provider + "/" + model: true}
	out := make([]ChainTarget, 0, len(ts))
	for _, t := range ts {
		sel := t.Selector()
		if seen[sel] {
			continue
		}
		seen[sel] = true
		out = append(out, t)
	}
	return out
}

// FallbackChainKeys lists the configured chain keys, sorted (diagnostics:
// an unresolvable chain must be able to name what it looked at).
func FallbackChainKeys(s *config.Settings) []string {
	if s == nil {
		return nil
	}
	keys := make([]string, 0, len(s.Retry.FallbackChains))
	for k := range s.Retry.FallbackChains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// describeChain renders a chain for logs.
func describeChain(ts []ChainTarget) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, t.Selector())
	}
	return strings.Join(parts, " → ")
}
