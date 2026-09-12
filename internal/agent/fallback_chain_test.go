package agent

import (
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// chainCatalog is the model universe the resolver tests resolve against.
func chainCatalog() ConfigCatalog {
	return ConfigCatalog{Config: &config.Config{Providers: map[string]*config.ProviderConfig{
		"onegw":      {Models: []config.ModelConfig{{ID: "free", ContextWindow: 200000}, {ID: "dev", ContextWindow: 128000}}},
		"other":      {Models: []config.ModelConfig{{ID: "dev", ContextWindow: 64000}}},
		"openrouter": {Models: []config.ModelConfig{{ID: "google/gemini-2.5-pro", ContextWindow: 1000000}}},
	}}}
}

func chainSettings(chains map[string][]string) *config.Settings {
	return &config.Settings{
		Retry:      config.RetrySettings{FallbackChains: chains},
		ModelRoles: map[string]string{"slow": "other/dev"},
	}
}

func selectors(ts []ChainTarget) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Selector())
	}
	return out
}

// TestResolveFallbackChainPrecedence pins the specificity order (M5 #25):
// exact model key > role key > provider wildcard > the existing chain.
func TestResolveFallbackChainPrecedence(t *testing.T) {
	existing := []ChainTarget{{Provider: "existing", Model: "m"}}
	cases := []struct {
		name   string
		chains map[string][]string
		want   []string
	}{
		{
			name: "exact model key wins",
			chains: map[string][]string{
				"onegw/free": {"other/dev"},
				"smol":       {"openrouter/google/gemini-2.5-pro"},
				"onegw/*":    {"other/dev"},
			},
			want: []string{"other/dev"},
		},
		{
			name: "role key beats the wildcard",
			chains: map[string][]string{
				"smol":    {"openrouter/google/gemini-2.5-pro"},
				"onegw/*": {"other/dev"},
			},
			want: []string{"openrouter/google/gemini-2.5-pro"},
		},
		{
			name:   "provider wildcard is the last configured rung",
			chains: map[string][]string{"onegw/*": {"other/dev"}},
			want:   []string{"other/dev"},
		},
		{
			name:   "no configured key keeps the existing chain",
			chains: map[string][]string{"unrelated": {"other/dev"}},
			want:   []string{"existing/m"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveFallbackChain(chainSettings(tc.chains), "smol", "onegw", "free", chainCatalog(), existing)
			if diff := selectors(got); !equalStrings(diff, tc.want) {
				t.Fatalf("chain = %v, want %v", diff, tc.want)
			}
		})
	}
}

// TestFallbackChainProviderWildcardKeepsModelID: "other/*" swaps the provider
// and keeps the failing id, resolved against that provider's catalog.
func TestFallbackChainProviderWildcardKeepsModelID(t *testing.T) {
	s := chainSettings(map[string][]string{"onegw/dev": {"other/*"}})
	got := ResolveFallbackChain(s, "", "onegw", "dev", chainCatalog(), nil)
	if diff := selectors(got); !equalStrings(diff, []string{"other/dev"}) {
		t.Fatalf("chain = %v, want [other/dev]", diff)
	}
	if got[0].ContextWindow != 64000 {
		t.Fatalf("window = %d, want the catalog's 64000", got[0].ContextWindow)
	}
}

// TestFallbackChainFuzzyResolve: a near-miss id still resolves, and the bare
// id carries onto an aggregator-prefixed catalog entry.
func TestFallbackChainFuzzyResolve(t *testing.T) {
	cases := []struct{ entry, want string }{
		{"openrouter/google/gemini-2.5-pro", "openrouter/google/gemini-2.5-pro"}, // exact
		{"openrouter/gemini-2.5-pro", "openrouter/google/gemini-2.5-pro"},        // basename
		{"openrouter/google/gemini-2-5-pro", "openrouter/google/gemini-2.5-pro"}, // near miss
	}
	for _, tc := range cases {
		s := chainSettings(map[string][]string{"onegw/free": {tc.entry}})
		got := ResolveFallbackChain(s, "", "onegw", "free", chainCatalog(), nil)
		if diff := selectors(got); !equalStrings(diff, []string{tc.want}) {
			t.Fatalf("entry %q resolved to %v, want %s", tc.entry, diff, tc.want)
		}
	}
}

// TestFallbackChainSelfAndDuplicatesDropped: the active model is never its
// own fallback, and a repeated target keeps only its first position.
func TestFallbackChainSelfAndDuplicatesDropped(t *testing.T) {
	s := chainSettings(map[string][]string{"onegw/free": {"onegw/free", "other/dev", "other/dev"}})
	got := ResolveFallbackChain(s, "", "onegw", "free", chainCatalog(), nil)
	if diff := selectors(got); !equalStrings(diff, []string{"other/dev"}) {
		t.Fatalf("chain = %v, want [other/dev]", diff)
	}
}

// TestFallbackChainUnresolvableFallsBackToExisting: an entry the catalog
// cannot resolve must not shorten the ladder to nothing.
func TestFallbackChainUnresolvableFallsBackToExisting(t *testing.T) {
	existing := []ChainTarget{{Provider: "existing", Model: "m"}}
	s := chainSettings(map[string][]string{"onegw/free": {"other/nope-not-a-model"}})
	got := ResolveFallbackChain(s, "", "onegw", "free", chainCatalog(), existing)
	if diff := selectors(got); !equalStrings(diff, []string{"existing/m"}) {
		t.Fatalf("chain = %v, want the existing chain", diff)
	}
}

// TestFallbackChainRoleEntryExpands: an entry may name another role.
func TestFallbackChainRoleEntryExpands(t *testing.T) {
	s := chainSettings(map[string][]string{"smol": {"@slow"}})
	got := ResolveFallbackChain(s, "smol", "onegw", "free", chainCatalog(), nil)
	if diff := selectors(got); !equalStrings(diff, []string{"other/dev"}) {
		t.Fatalf("chain = %v, want [other/dev]", diff)
	}
}

// TestFallbackChainPrefixWildcardReprefixesID: "openrouter/google/*" hangs
// the configured prefix onto the failing id, falling back to the bare id
// when the target does not carry the prefixed one.
func TestFallbackChainPrefixWildcardReprefixesID(t *testing.T) {
	s := chainSettings(map[string][]string{"onegw/google/*": {"openrouter/google/*"}})
	got := ResolveFallbackChain(s, "", "onegw", "gemini-2.5-pro", chainCatalog(), nil)
	if diff := selectors(got); !equalStrings(diff, []string{"openrouter/google/gemini-2.5-pro"}) {
		t.Fatalf("chain = %v, want [openrouter/google/gemini-2.5-pro]", diff)
	}

	// The target does not advertise the prefixed id, but a bare-id provider
	// does: the aggregator→direct carry still resolves.
	s = chainSettings(map[string][]string{"onegw/google/*": {"other/*"}})
	got = ResolveFallbackChain(s, "", "onegw", "google/dev", chainCatalog(), nil)
	if diff := selectors(got); !equalStrings(diff, []string{"other/dev"}) {
		t.Fatalf("chain = %v, want [other/dev]", diff)
	}
}

// TestFuzzyResolveDeterministicTieBreak: equal scores resolve to the same
// candidate every time (shorter id first), never by map iteration order.
func TestFuzzyResolveDeterministicTieBreak(t *testing.T) {
	ids := []string{"prov/gemini-2.5-pro", "prov/gemini-2.5-pro-long"}
	for i := 0; i < 5; i++ {
		got, ok := fuzzyResolve(ids, "prov/gemini-2.5-pro")
		if !ok || got != "prov/gemini-2.5-pro" {
			t.Fatalf("fuzzyResolve = %q, %v", got, ok)
		}
	}
	if _, ok := fuzzyResolve([]string{"a", "b"}, "zzz"); ok {
		t.Fatal("fuzzyResolve matched an unrelated id")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestChainTargetsProjectsLiveFailovers: the CLI hands its existing
// models.yml chain to the resolver through this projection.
func TestChainTargetsProjectsLiveFailovers(t *testing.T) {
	live := []FailoverTarget{{Provider: named("other", &fakeProvider{}), Model: "dev", ContextWindow: 64000}}
	got := ChainTargets(live)
	if len(got) != 1 || got[0].Selector() != "other/dev" || got[0].ContextWindow != 64000 {
		t.Fatalf("ChainTargets = %+v", got)
	}
	if orphan := ChainTargets([]FailoverTarget{{Model: "orphan"}}); len(orphan) != 0 {
		t.Fatalf("ChainTargets kept a provider-less target: %+v", orphan)
	}
}
