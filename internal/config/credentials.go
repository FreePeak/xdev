package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Credential resolution (M9 #10, parity-tools-providers §F). Ordered, and
// the order is the contract:
//
//	1. CLI --api-key            (explicit for this run; never persisted)
//	2. models.yml provider/model apiKey (${VAR} already expanded by LoadModels)
//	3. stored OAuth access token (refreshed if expired)
//	4. key saved by /login      (credentials.json)
//	5. environment             (PROVIDER_API_KEY, then ONEGW_KEY-style names)
//
// A higher source returning an empty value falls through; an error (a
// refresh that failed) is reported, never silently skipped, because a
// missing credential must not masquerade as a working one.

// CredentialsPath is <dataDir>/credentials.json.
func CredentialsPath() string { return filepath.Join(DataDir(), "credentials.json") }

// StoredCredential is one provider's persisted authentication. OAuth fields
// are empty for plain API keys saved by /login.
type StoredCredential struct {
	Kind         string `json:"kind"` // api_key | oauth
	APIKey       string `json:"apiKey,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"` // unix millis; 0 = never
	Email        string `json:"email,omitempty"`
	UpdatedAt    int64  `json:"updatedAt,omitempty"`
}

// Expired reports whether the stored token needs a refresh attempt.
func (c StoredCredential) Expired() bool {
	if c.Kind != "oauth" || c.ExpiresAt == 0 {
		return false
	}
	// Refresh a minute early: an expired-mid-request token fails worse than
	// one refreshed a little soon.
	return time.Now().UnixMilli() > c.ExpiresAt-60_000
}

// CredentialStore is the parsed credentials.json (provider key → entry).
type CredentialStore map[string]StoredCredential

// LoadCredentials reads the store; an absent file is empty, a corrupt one is
// an error (never silently ignored — losing every login is not a fallback),
// and a file whose ownership guarantees no longer hold is refused (#123): the
// 0600 that SaveCredential set is a fact about the write, not about now.
func LoadCredentials() (CredentialStore, error) {
	path := CredentialsPath()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CredentialStore{}, nil
		}
		return nil, err
	}
	if err := verifySecretFile(path, fi); err != nil {
		return nil, fmt.Errorf("credentials: refusing to read %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out CredentialStore
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("credentials: %s unparseable: %w", CredentialsPath(), err)
	}
	for k, v := range out {
		if v.Kind == "" {
			v.Kind = "api_key"
			out[k] = v
		}
	}
	return out, nil
}

// SaveCredential persists one provider entry with 0600 (these are secrets).
func SaveCredential(provider string, c StoredCredential) error {
	store, err := LoadCredentials()
	if err != nil {
		return err
	}
	if c.UpdatedAt == 0 {
		c.UpdatedAt = time.Now().UnixMilli()
	}
	if c.Kind == "" {
		c.Kind = "api_key"
	}
	store[provider] = c
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := enforceDirPrivacy(DataDir(), 0o700); err != nil {
		return err
	}
	return writeFilePrivate(CredentialsPath(), append(raw, '\n'))
}

// DeleteCredential removes one provider's stored entry.
func DeleteCredential(provider string) error {
	store, err := LoadCredentials()
	if err != nil {
		return err
	}
	if _, ok := store[provider]; !ok {
		return nil
	}
	delete(store, provider)
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return writeFilePrivate(CredentialsPath(), append(raw, '\n'))
}

// writeFilePrivate creates the file with 0600 from the start: an
// open/create-then-chmod window briefly exposes a secret.
func writeFilePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ResolvedCredential is the winning credential plus where it came from.
type ResolvedCredential struct {
	Value  string
	Source string // cli | models.yml | oauth | login | env:<NAME>
	Kind   string // api_key | oauth
	Header string // auth header name, defaulting to the provider's
}

// CredentialRequest is one resolution call.
type CredentialRequest struct {
	Provider    string
	Model       string
	ProviderCfg *ProviderConfig // may be nil
	CLIKey      string
	Store       CredentialStore
	// Refresh, when set, is called for an expired OAuth entry and must
	// return a fresh access token (and optionally a new refresh token).
	Refresh func(StoredCredential) (StoredCredential, error)
	// PoolIndex rotates among the provider's models.yml credentials
	// (M5 #25): 0 (the default) resolves the ordinary chain, ≥1 uses
	// CredentialPool's i-th sibling. An index past the pool is an error —
	// an exhausted rotation must not silently reuse the primary key.
	PoolIndex int
}

// ResolveCredential walks the chain in contract order.
func ResolveCredential(req CredentialRequest) (ResolvedCredential, error) {
	if strings.TrimSpace(req.CLIKey) != "" {
		return req.configured(req.CLIKey, "cli")
	}
	if pc := req.ProviderCfg; pc != nil {
		if req.PoolIndex > 0 {
			pool := CredentialPool(pc, req.Model)
			if req.PoolIndex >= len(pool) {
				return ResolvedCredential{}, fmt.Errorf("credentials: %s: no credential at rotation index %d (models.yml declares %d)", req.Provider, req.PoolIndex, len(pool))
			}
			return req.configured(pool[req.PoolIndex], fmt.Sprintf("models.yml (key %d)", req.PoolIndex+1))
		}
		// A per-model apiKey overrides the provider-level one.
		if req.Model != "" {
			for _, m := range pc.Models {
				if m.ID == req.Model && strings.TrimSpace(m.APIKey) != "" {
					return req.configured(m.APIKey, "models.yml (model)")
				}
			}
		}
		if strings.TrimSpace(pc.APIKey) != "" {
			return req.configured(pc.APIKey, "models.yml")
		}
		// A provider configured with only `apiKeys: [...]` still has a
		// primary credential: the first pool entry (M5 #25).
		if k := CredentialKey(pc, req.Model, 0); k != "" {
			return req.configured(k, "models.yml")
		}
	}
	stored := req.Store[req.Provider]
	// A named auth style is an exact authority (#123). Falling back from the
	// credential the user configured to one they did not can move a request to
	// a different account, quota or team, so the chain stops here and says
	// which rung is missing instead of quietly using another.
	auth := ""
	if pc := req.ProviderCfg; pc != nil {
		auth = strings.ToLower(strings.TrimSpace(pc.Auth))
	}
	switch {
	case auth == "oauth" && stored.Kind != "oauth":
		return ResolvedCredential{}, fmt.Errorf("credentials: %s is configured auth: oauth but its stored login is %q — run: xdev login %s (xdev will not fall back to an API key)", req.Provider, stored.Kind, req.Provider)
	case auth == "api_key" && stored.Kind == "oauth":
		return ResolvedCredential{}, fmt.Errorf("credentials: %s is configured auth: api_key but its stored login is an OAuth token — run: xdev logout %s and store a key, or set auth: oauth", req.Provider, req.Provider)
	}
	switch stored.Kind {
	case "oauth":
		c := stored
		if c.Expired() {
			if req.Refresh == nil {
				return ResolvedCredential{}, fmt.Errorf("credentials: %s token expired and no refresh is configured (re-login required)", req.Provider)
			}
			fresh, err := req.Refresh(c)
			if err != nil {
				return ResolvedCredential{}, fmt.Errorf("credentials: %s refresh: %w", req.Provider, err)
			}
			if err := SaveCredential(req.Provider, fresh); err != nil {
				return ResolvedCredential{}, err
			}
			c = fresh
		}
		if strings.TrimSpace(c.AccessToken) != "" {
			return ResolvedCredential{Value: c.AccessToken, Source: "oauth", Kind: "oauth", Header: authHeader(req)}, nil
		}
	case "api_key":
		if strings.TrimSpace(stored.APIKey) != "" {
			return ResolvedCredential{Value: stored.APIKey, Source: "login", Kind: "api_key", Header: authHeader(req)}, nil
		}
	}
	if auth == "oauth" {
		return ResolvedCredential{}, fmt.Errorf("credentials: %s is configured auth: oauth and its stored token is unusable — run: xdev login %s", req.Provider, req.Provider)
	}
	for _, name := range envCandidates(req.Provider) {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return ResolvedCredential{Value: v, Source: "env:" + name, Kind: "api_key", Header: authHeader(req)}, nil
		}
	}
	return ResolvedCredential{}, fmt.Errorf("credentials: no credential for provider %q (pass -api-key, set apiKey in models.yml, run /login, or export %s)", req.Provider, envCandidates(req.Provider)[0])
}

// configured turns a credential value the user wrote into the resolved
// credential. Normally that value IS the secret; the `keychain:` form instead
// names a macOS Keychain item whose secret is fetched at use time, so a
// long-lived token never has to live in a file at all (keychain.go explains why
// xdev only reads there and never writes).
//
// A reference that cannot be satisfied is an error, never a fall-through: the
// next rung down is a different credential, which means a different account.
func (req CredentialRequest) configured(value, source string) (ResolvedCredential, error) {
	ref, isRef := parseKeychainRef(value)
	if !isRef {
		return ResolvedCredential{Value: value, Source: source, Kind: "api_key", Header: authHeader(req)}, nil
	}
	named := keychainPrefix + ref.Service
	secret, err := keychainLookup(ref)
	if err != nil {
		return ResolvedCredential{}, fmt.Errorf("credentials: %s %s names %s: %w", req.Provider, source, named, err)
	}
	if strings.TrimSpace(secret) == "" {
		return ResolvedCredential{}, fmt.Errorf("credentials: %s %s names %s, which holds nothing", req.Provider, source, named)
	}
	return ResolvedCredential{Value: secret, Source: named, Kind: "api_key", Header: authHeader(req)}, nil
}

// authHeader resolves the header a bearer-style token rides, honoring the
// provider's authHeader key (omp parity) with a sane default.
func authHeader(req CredentialRequest) string {
	if pc := req.ProviderCfg; pc != nil {
		if h := strings.TrimSpace(pc.AuthHeader); h != "" {
			return h
		}
	}
	return "Authorization"
}

// envCandidates are the environment names tried for a provider, most
// specific first: <PROVIDER>_API_KEY then <PROVIDER>_KEY.
func envCandidates(provider string) []string {
	up := strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))
	return []string{up + "_API_KEY", up + "_KEY"}
}

// Providers lists the provider names in the store (for `xdev config`-style
// status output).
func (s CredentialStore) Providers() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Redacted renders a credential for display: enough to identify it, never
// the secret.
func (c ResolvedCredential) Redacted() string {
	return Redact(c.Value) + " (" + c.Source + ")"
}

// Redact keeps a short prefix/suffix of a secret for logs and list output.
func Redact(v string) string {
	v = strings.TrimSpace(v)
	if len(v) <= 8 {
		return strings.Repeat("•", len(v))
	}
	return v[:4] + "…" + v[len(v)-3:]
}
