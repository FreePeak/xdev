// Provider connect (xdev connect / /connect): one command turns an account you
// already have into a working xdev provider — endpoint, wire protocol,
// credential variable, and the models worth pinning.
//
// The catalog (connect_catalog.go, generated from a models.dev snapshot) is
// static data and never carries a key. A credential reaches xdev through one of
// two doors: the environment variable the provider documents — referenced from
// models.yml as ${VAR}, so the config file stays safe to paste into an issue —
// or a key passed on the command line, which goes to credentials.json (0600)
// like every other stored login.

package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// connectModel is one pinned model row of the catalog.
type connectModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
	Reasoning     bool
	Vision        bool
}

// connectEntry is one connectable provider.
type connectEntry struct {
	Title   string
	BaseURL string
	API     string
	// Env names the credential variables this host reads, in preference
	// order; the reference written to models.yml is the first of them.
	Env []string
	Doc string
	// Models is the pinned set: the ids worth declaring in models.yml, each
	// carrying metadata (context window, reasoning, vision) an OpenAI-style
	// /models listing does not report.
	Models []connectModel
}

// ConnectOption is one catalog row as the surfaces (the CLI listing, the
// /connect picker) show it, plus the local state that decides what the row
// means on this machine right now.
type ConnectOption struct {
	Name       string `json:"name"`
	Title      string `json:"title"`
	BaseURL    string `json:"baseUrl"`
	API        string `json:"api"`
	Env        string `json:"env,omitempty"` // the credential variable a key rides
	Doc        string `json:"doc,omitempty"`
	Models     int    `json:"models"`
	Plan       bool   `json:"plan,omitempty"`       // a flat subscription (coding plan / token plan / pass)
	Connected  bool   `json:"connected,omitempty"`  // models.yml already declares this provider
	Ready      bool   `json:"ready,omitempty"`      // a credential for it resolves right now
	ReadyFrom  string `json:"readyFrom,omitempty"`  // where that credential came from (never the value)
	DefaultRef string `json:"defaultRef,omitempty"` // "name/model" a run gets after connecting
}

// ConnectNames lists the catalog keys in sorted order: map iteration would make
// the picker's row order — and therefore its tests — random.
func ConnectNames() []string {
	out := make([]string, 0, len(connectCatalog))
	for k := range connectCatalog {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HasConnect reports whether a name is in the catalog.
func HasConnect(name string) bool { _, ok := connectCatalog[name]; return ok }

// ConnectOptions lists the catalog against local state. It writes nothing and
// requests nothing; the caller loads cfg/store so a broken file is reported
// once, in the caller's words, rather than from inside a listing.
func ConnectOptions(cfg *Config, store CredentialStore) []ConnectOption {
	out := make([]ConnectOption, 0, len(connectCatalog))
	for _, name := range ConnectNames() {
		e := connectCatalog[name]
		opt := ConnectOption{
			Name: name, Title: e.Title, BaseURL: e.BaseURL, API: e.API,
			Doc: e.Doc, Models: len(e.Models), Plan: isPlan(name),
		}
		if len(e.Env) > 0 {
			opt.Env = e.Env[0]
		}
		if len(e.Models) > 0 {
			opt.DefaultRef = name + "/" + e.Models[0].ID
		}
		if cfg != nil {
			opt.Connected = cfg.Providers[name] != nil
		}
		if _, from := connectCredential(name, e, cfg, store); from != "" {
			opt.Ready, opt.ReadyFrom = true, from
		}
		out = append(out, opt)
	}
	return out
}

// connectCredential finds the key a connected row would run on, in the
// credential chain's own order — models.yml, then the stored login, then the
// environment — so the listing answers "what will I actually run on" once
// instead of describing three places for the user to reconcile. It returns the
// source name only; the value never leaves the function.
func connectCredential(name string, e connectEntry, cfg *Config, store CredentialStore) (value, source string) {
	if cfg != nil {
		if pc := cfg.Providers[name]; pc != nil {
			if v := strings.TrimSpace(Resolve(pc.APIKey)); v != "" {
				return v, "models.yml"
			}
		}
	}
	if c := store[name]; c.Kind != "oauth" {
		if v := strings.TrimSpace(c.APIKey); v != "" {
			return v, "credentials.json"
		}
	}
	for _, ev := range e.Env {
		if v := strings.TrimSpace(os.Getenv(ev)); v != "" {
			return v, "env " + ev
		}
	}
	return "", ""
}

// isPlan names the rows that bill as a flat monthly subscription — a coding
// plan, a token plan, a pass — rather than per token. models.dev carries no such
// field; every host that sells one puts the word in its own name, which is also
// how opencode's list reads. Listing garnish: no behavior hangs on it.
func isPlan(name string) bool {
	return strings.Contains(name, "coding-plan") || strings.Contains(name, "token-plan") ||
		strings.Contains(name, "step-plan") || strings.HasSuffix(name, "-pass")
}

// ConnectProviderConfig builds the models.yml block for one catalog row. The
// credential is written as a ${VAR} reference, never as the key.
func ConnectProviderConfig(name string) (*ProviderConfig, error) {
	e, ok := connectCatalog[name]
	if !ok {
		return nil, fmt.Errorf("connect: unknown provider %q (xdev connect --list shows the catalog)", name)
	}
	pc := &ProviderConfig{BaseURL: e.BaseURL, API: e.API, Auth: "api_key"}
	if len(e.Env) > 0 {
		pc.APIKey = "${" + e.Env[0] + "}"
	}
	// Discovery is opt-in by presence (see DiscoverModels): a host that
	// publishes /models also lists the ids nobody pinned, so a model released
	// after this snapshot appears without a config edit.
	pc.Discovery = &DiscoveryConfig{Type: DiscoveryOpenAIModels}
	for _, m := range e.Models {
		pc.Models = append(pc.Models, ModelConfig{
			ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow,
			MaxTokens: m.MaxTokens, Reasoning: m.Reasoning, Vision: m.Vision,
		})
	}
	return pc, nil
}

// ConnectModelRefs lists a row's models as "name/model" refs ("" name is the
// catalog's own key).
func ConnectModelRefs(name string) []string {
	e, ok := connectCatalog[name]
	if !ok {
		return nil
	}
	refs := make([]string, 0, len(e.Models))
	for _, m := range e.Models {
		refs = append(refs, name+"/"+m.ID)
	}
	return refs
}

// ConnectDefaultRef names the model a connected provider should default to:
// the catalog keeps its pinned rows newest-first, so the first is the host's
// current model. "" when nothing is pinned.
func ConnectDefaultRef(name string) string {
	refs := ConnectModelRefs(name)
	if len(refs) == 0 {
		return ""
	}
	return refs[0]
}

// Connect writes one provider into models.yml and, when a key was passed,
// stores it in credentials.json. It returns the model refs now usable. An
// unknown name is an error; an existing block is replaced, because the catalog
// is the source of truth for these rows.
func Connect(name, key string) ([]string, error) {
	if !HasConnect(name) {
		return nil, fmt.Errorf("connect: unknown provider %q (xdev connect --list shows the catalog)", name)
	}
	pc, err := ConnectProviderConfig(name)
	if err != nil {
		return nil, err
	}
	if key != "" {
		// A pasted key belongs in the 0600 store, not in the config file: the
		// ${VAR} reference stays, and the chain falls through to the store when
		// the variable is not exported.
		if err := SaveCredential(name, StoredCredential{Kind: "api_key", APIKey: key}); err != nil {
			return nil, err
		}
	}
	if err := UpsertModelsProvider(filepath.Join(DataDir(), "models.yml"), name, pc); err != nil {
		return nil, err
	}
	return ConnectModelRefs(name), nil
}

// UpsertModelsProvider writes one provider block into a models.yml, replacing a
// block of the same name and appending it when absent, and leaving every other
// key — and every comment — alone.
//
// The node surgery is the point. Decoding into Config and re-marshaling would
// drop the comments and the ordering that make a hand-written models.yml
// readable, and would rewrite a user's own blocks on the way to adding one.
func UpsertModelsProvider(path, name string, pc *ProviderConfig) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	if len(strings.TrimSpace(string(raw))) > 0 {
		doc = &yaml.Node{}
		if err := yaml.Unmarshal(raw, doc); err != nil {
			// Refusing to edit an unparseable file beats overwriting it: those
			// bytes are the user's, and the loader has already said why.
			return fmt.Errorf("config: %s: refusing to edit unparseable file: %w", path, err)
		}
	}
	body := documentBody(doc)
	if body.Kind != yaml.MappingNode {
		return fmt.Errorf("connect: %s: top level is not a mapping", path)
	}
	providers := mappingValue(body, "providers")
	if providers == nil {
		providers = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		body.Content = append(body.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "providers", Tag: "!!str"}, providers)
	}
	if providers.Kind != yaml.MappingNode {
		return fmt.Errorf("connect: %s: providers: is not a mapping", path)
	}
	var block yaml.Node
	if err := block.Encode(pc); err != nil {
		return fmt.Errorf("connect: encode %s: %w", name, err)
	}
	for _, pair := range mappingPairs(providers) {
		if pair[0].Value == name {
			pair[1] = &block
			return writeModelsYAML(path, documentBody(doc))
		}
	}
	providers.Content = append(providers.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: name, Tag: "!!str"}, &block)
	return writeModelsYAML(path, documentBody(doc))
}

// writeModelsYAML encodes through an encoder set to two-space indents — the
// width the hand-written files use and what the loader's writers emit — and
// replaces the file atomically through a private temp. models.yml carries
// provider endpoints and occasionally an inline key, so neither a half-written
// file nor one briefly world-readable is acceptable.
func writeModelsYAML(path string, body *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(body); err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// SetModelsDefault writes the top-level defaultModel key of a models.yml,
// preserving comments and every other key the same way UpsertModelsProvider
// does. Connect's --set-default uses it; a value already present is left alone
// by the caller, because an existing default is a decision, not a gap.
func SetModelsDefault(path, ref string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	if len(strings.TrimSpace(string(raw))) > 0 {
		doc = &yaml.Node{}
		if err := yaml.Unmarshal(raw, doc); err != nil {
			return fmt.Errorf("config: %s: refusing to edit unparseable file: %w", path, err)
		}
	}
	body := documentBody(doc)
	if body.Kind != yaml.MappingNode {
		return fmt.Errorf("connect: %s: top level is not a mapping", path)
	}
	if existing := mappingValue(body, "defaultModel"); existing != nil {
		existing.SetString(ref)
	} else {
		body.Content = append(body.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "defaultModel", Tag: "!!str"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: ref, Tag: "!!str"})
	}
	return writeModelsYAML(path, body)
}
