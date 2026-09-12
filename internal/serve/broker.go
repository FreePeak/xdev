package serve

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Broker is the local credential vault (issue #71).
//
// Values are encrypted at rest with AES-256-GCM under a per-install key file
// (<data dir>/serve/auth-broker.key, 0600). The provider name is the AEAD's
// additional data, so a ciphertext moved into another provider's slot fails to
// authenticate instead of decrypting into the wrong credential.
//
// Endpoints (bearer token; /healthz is open):
//
//	GET    /credentials/{provider}   read one value
//	PUT    /credentials/{provider}   store/replace {"value":"..."}
//	DELETE /credentials/{provider}   forget one
//	POST   /usage/observed           record one observed provider call
//	GET    /usage                    install-id + hostname stamped usage report
type Broker struct {
	token     string
	tokenPath string
	version   string
	options   Options
	vault     *vault
	usage     *usageLog
	installID string
	hostname  string
	h         http.Handler
	logf      func(string, ...any)
}

// BrokerOptions configures the vault service.
type BrokerOptions struct {
	Options
	// MaxBody bounds a credential request body (default 256 KiB — a token or
	// a small JSON credential blob, not a file).
	MaxBody int64
}

// maxValueBytes caps one stored credential value.
const maxValueBytes = 64 << 10

// usageHistory caps the persisted observation list.
//
// ponytail: the newest 5000 observations, rewritten as one JSON file per
// report. That is a rolling window, not an audit log — the upgrade path is a
// rollup table keyed by (install, provider, model, hour) if this ever has to
// account for real spend.
const usageHistory = 5000

// NewBroker opens the vault and builds the service.
func NewBroker(opts BrokerOptions) (*Broker, error) {
	token, err := resolveToken(serviceBroker, opts.Options)
	if err != nil {
		return nil, err
	}
	dir, err := serveDir(opts.Options)
	if err != nil {
		return nil, err
	}
	v, err := openVault(filepath.Join(dir, serviceBroker+".vault.json"), filepath.Join(dir, serviceBroker+".key"))
	if err != nil {
		return nil, err
	}
	u, err := openUsage(filepath.Join(dir, serviceBroker+"-usage.json"))
	if err != nil {
		return nil, err
	}
	id, err := installID(opts.Options)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname() // a nameless host is not a reason to fail startup
	b := &Broker{
		token:     token,
		tokenPath: tokenPath(dir, serviceBroker),
		version:   opts.Version,
		options:   opts.Options,
		vault:     v,
		usage:     u,
		installID: id,
		hostname:  host,
		logf:      opts.Options.logf,
	}
	b.h = requireToken(token, b.mux(opts.maxBody()))
	return b, nil
}

// Token is the bearer token this broker requires (for a co-located client).
func (b *Broker) Token() string { return b.token }

// Handler serves the vault API.
func (b *Broker) Handler() http.Handler { return b.h }

// Run serves until ctx is done.
func (b *Broker) Run(ctx context.Context) error {
	b.logf("%s: vault %s, key %s, token %s", serviceBroker, b.vault.path, b.vault.keyPath, b.tokenPath)
	return serveHTTP(ctx, serviceBroker, b.options.addr(defaultBrokerListen), b.h, b.logf)
}

func (o BrokerOptions) maxBody() int64 {
	if o.MaxBody > 0 {
		return o.MaxBody
	}
	return 256 << 10
}

func (b *Broker) mux(maxBody int64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthPayload{Status: "ok", Service: serviceBroker, Version: b.version})
	})
	mux.HandleFunc("GET /credentials/{provider}", b.handleGetCredential)
	mux.HandleFunc("PUT /credentials/{provider}", b.handlePutCredential(maxBody))
	mux.HandleFunc("DELETE /credentials/{provider}", b.handleDeleteCredential)
	mux.HandleFunc("POST /usage/observed", b.handleObservedUsage(maxBody))
	mux.HandleFunc("GET /usage", b.handleUsage)
	return mux
}

// providerRe is the trust boundary for a vault key: the provider name travels
// in a URL path and becomes an AES additional-data string, so it must not
// carry separators, whitespace, or control bytes.
var providerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:-]{0,63}$`)

func validProvider(p string) bool { return providerRe.MatchString(p) }

// credentialResponse is one stored value. This is the only endpoint that
// returns a secret, and it exists because the gateway is its only client.
type credentialResponse struct {
	Provider  string `json:"provider"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func (b *Broker) handleGetCredential(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !validProvider(provider) {
		httpError(w, http.StatusBadRequest, "invalid provider name %q", provider)
		return
	}
	value, updatedAt, err := b.vault.get(provider)
	switch {
	case errors.Is(err, errNoCredential):
		httpError(w, http.StatusNotFound, "no credential stored for provider %q", provider)
	case err != nil:
		httpError(w, http.StatusInternalServerError, "vault: %v", err)
	default:
		writeJSON(w, http.StatusOK, credentialResponse{Provider: provider, Value: value, UpdatedAt: updatedAt})
	}
}

func (b *Broker) handlePutCredential(maxBody int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := r.PathValue("provider")
		if !validProvider(provider) {
			httpError(w, http.StatusBadRequest, "invalid provider name %q", provider)
			return
		}
		var body struct {
			Value string `json:"value"`
		}
		if err := readJSON(w, r, maxBody, &body); err != nil {
			httpError(w, http.StatusBadRequest, "bad request body: %v", err)
			return
		}
		// Trim on the way in: a stored trailing newline becomes an illegal
		// header value at the gateway, where the failure is far from here.
		value := strings.TrimSpace(body.Value)
		if value == "" {
			httpError(w, http.StatusBadRequest, "value is required")
			return
		}
		if len(value) > maxValueBytes {
			httpError(w, http.StatusRequestEntityTooLarge, "value exceeds %d bytes", maxValueBytes)
			return
		}
		if err := b.vault.put(provider, value); err != nil {
			httpError(w, http.StatusInternalServerError, "vault: %v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (b *Broker) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !validProvider(provider) {
		httpError(w, http.StatusBadRequest, "invalid provider name %q", provider)
		return
	}
	switch err := b.vault.delete(provider); {
	case errors.Is(err, errNoCredential):
		httpError(w, http.StatusNotFound, "no credential stored for provider %q", provider)
	case err != nil:
		httpError(w, http.StatusInternalServerError, "vault: %v", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (b *Broker) handleObservedUsage(maxBody int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var e UsageEntry
		if err := readJSON(w, r, maxBody, &e); err != nil {
			httpError(w, http.StatusBadRequest, "bad request body: %v", err)
			return
		}
		if !validProvider(e.Provider) {
			httpError(w, http.StatusBadRequest, "invalid provider name %q", e.Provider)
			return
		}
		if len(e.Model) > 128 {
			httpError(w, http.StatusBadRequest, "model name exceeds 128 bytes")
			return
		}
		if e.InputTokens < 0 || e.OutputTokens < 0 {
			httpError(w, http.StatusBadRequest, "token counts must be non-negative")
			return
		}
		// The stamp is the server's: a client clock is not evidence.
		e.ObservedAt = time.Now().UTC().Format(time.RFC3339)
		if err := b.usage.record(e); err != nil {
			httpError(w, http.StatusInternalServerError, "usage log: %v", err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (b *Broker) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, b.Report())
}

// UsageEntry is one observed provider call.
type UsageEntry struct {
	Provider     string `json:"provider"`
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	// ObservedAt is the server's stamp (RFC3339, UTC).
	ObservedAt string `json:"observed_at,omitempty"`
}

// UsageReport is GET /usage: the install identity plus every observation.
// InstallID and Hostname are what let a collector attribute usage to one
// machine without conflating two installs that share a hostname pattern.
type UsageReport struct {
	InstallID   string       `json:"install_id"`
	Hostname    string       `json:"hostname"`
	GeneratedAt string       `json:"generated_at"`
	Usage       []UsageEntry `json:"usage"`
}

// Report renders the current usage report.
func (b *Broker) Report() UsageReport {
	return UsageReport{
		InstallID:   b.installID,
		Hostname:    b.hostname,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Usage:       b.usage.snapshot(),
	}
}

// errNoCredential reports a provider with nothing stored.
var errNoCredential = errors.New("no credential stored")

// vault is the encrypted credential file.
type vault struct {
	path    string
	keyPath string
	key     []byte
	mu      sync.Mutex
	doc     vaultDoc
}

type vaultEntry struct {
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

type vaultDoc struct {
	Version     int                   `json:"version"`
	Credentials map[string]vaultEntry `json:"credentials"`
}

// vaultVersion is the only schema this build understands. A future version
// refuses to load rather than guessing at the layout.
const vaultVersion = 1

func openVault(path, keyPath string) (*vault, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	v := &vault{path: path, keyPath: keyPath, key: key, doc: vaultDoc{Version: vaultVersion, Credentials: map[string]vaultEntry{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &v.doc); err != nil {
		return nil, fmt.Errorf("credential vault %s: %w", path, err)
	}
	if v.doc.Version != vaultVersion {
		return nil, fmt.Errorf("credential vault %s: unsupported version %d", path, v.doc.Version)
	}
	if v.doc.Credentials == nil {
		v.doc.Credentials = map[string]vaultEntry{}
	}
	return v, nil
}

// loadOrCreateKey reads the 32-byte AES key, minting it on first use. A key of
// the wrong size is an error, never silently replaced: replacing it would make
// every stored credential undecryptable while the service still looked
// healthy.
func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("credential key %s: want 32 bytes, got %d", path, len(b))
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := createPrivate(path, key); err != nil {
		if b, rerr := os.ReadFile(path); rerr == nil && len(b) == 32 {
			return b, nil // lost the race against another daemon
		}
		return nil, err
	}
	return key, nil
}

func (v *vault) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// get decrypts one provider's value.
func (v *vault) get(provider string) (string, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.doc.Credentials[provider]
	if !ok {
		return "", "", errNoCredential
	}
	gcm, err := v.gcm()
	if err != nil {
		return "", "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(entry.Nonce)
	if err != nil {
		return "", "", fmt.Errorf("credential %s: bad nonce: %w", provider, err)
	}
	ct, err := base64.StdEncoding.DecodeString(entry.Ciphertext)
	if err != nil {
		return "", "", fmt.Errorf("credential %s: bad ciphertext: %w", provider, err)
	}
	plain, err := gcm.Open(nil, nonce, ct, []byte(provider))
	if err != nil {
		return "", "", fmt.Errorf("credential %s: %w", provider, err)
	}
	return string(plain), entry.UpdatedAt, nil
}

// put encrypts and persists one provider's value.
func (v *vault) put(provider, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	gcm, err := v.gcm()
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, []byte(value), []byte(provider))
	v.doc.Credentials[provider] = vaultEntry{
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	return v.saveLocked()
}

// delete forgets one provider.
func (v *vault) delete(provider string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.doc.Credentials[provider]; !ok {
		return errNoCredential
	}
	delete(v.doc.Credentials, provider)
	return v.saveLocked()
}

// saveLocked writes the vault; the caller holds the lock.
func (v *vault) saveLocked() error {
	b, err := json.MarshalIndent(v.doc, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(v.path, append(b, '\n'))
}

// usageLog is the observed-usage history behind GET /usage.
type usageLog struct {
	path    string
	mu      sync.Mutex
	entries []UsageEntry
}

func openUsage(path string) (*usageLog, error) {
	u := &usageLog{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return u, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &u.entries); err != nil {
		// A corrupt usage file costs history, not credentials: start over
		// rather than refusing to serve the vault.
		u.entries = nil
	}
	return u, nil
}

func (u *usageLog) record(e UsageEntry) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.entries = append(u.entries, e)
	if len(u.entries) > usageHistory {
		u.entries = append([]UsageEntry(nil), u.entries[len(u.entries)-usageHistory:]...)
	}
	b, err := json.Marshal(u.entries)
	if err != nil {
		return err
	}
	return writePrivate(u.path, append(b, '\n'))
}

// snapshot copies the history; callers must not see a concurrent append.
func (u *usageLog) snapshot() []UsageEntry {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]UsageEntry, len(u.entries))
	copy(out, u.entries)
	return out
}

// installID returns the per-install UUID (lowercase RFC 4122 v4), minting and
// persisting one at <data dir>/install-id on first use. Unlike agent state,
// this survives wiping sessions; an unwritable data dir is not fatal, the id
// is still stable for the lifetime of the process.
func installID(o Options) (string, error) {
	dir := o.dataDir()
	path := filepath.Join(dir, "install-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); uuidRe.MatchString(id) {
			return strings.ToLower(id), nil
		}
		// Stale garbage would make the exclusive create below fail forever.
		_ = os.Remove(path)
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return id, nil // not persisted; still stable for this process
	}
	if err := createPrivate(path, []byte(id+"\n")); err != nil {
		if b, rerr := os.ReadFile(path); rerr == nil {
			if got := strings.TrimSpace(string(b)); uuidRe.MatchString(got) {
				return strings.ToLower(got), nil
			}
		}
		return id, nil
	}
	return id, nil
}

var uuidRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newUUID mints a lowercase v4 UUID from crypto/rand.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
