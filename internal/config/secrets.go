package config

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Secrets redaction (M13 #55, parity-tools-providers §G). Values and patterns
// declared in secrets.yml are replaced in provider-visible text with
// deterministic, reversible $$HASH8$$ placeholders: the placeholder is what
// reaches the model and what lands in the session JSONL, while the
// hash -> value mapping stays in this process. The hash is an HMAC under a
// per-install key (PlaceholderKeyPath), so a transcript reader cannot
// dictionary-hash a placeholder back to its secret.
//
// Everything here is best-effort by design: an absent, empty, malformed, or
// partially-invalid secrets.yml is skipped with a warning, because redaction
// must never be the reason xdev fails to start.

// SecretEntry is one secrets.yml row: exactly one of Value (a literal secret
// to match) or Pattern (a regex, scanned globally), plus the Name used in
// warnings. A bare scalar row ("- sk-abc123") is accepted as a literal value
// and named after its position.
type SecretEntry struct {
	Name    string `yaml:"name"`
	Value   string `yaml:"value"`
	Pattern string `yaml:"pattern"`
}

// PlaceholderKeyPath is the per-install HMAC key for placeholder hashes
// (<agent data dir>/secret-placeholder.key). The key never leaves the host and
// is what makes a placeholder stable across sessions without being invertible
// offline.
func PlaceholderKeyPath() string { return filepath.Join(DataDir(), "secret-placeholder.key") }

// GlobalSecretsPath is <agent data dir>/secrets.yml: secrets for every project.
func GlobalSecretsPath() string { return filepath.Join(DataDir(), "secrets.yml") }

// projectSecretsPath is <cwd>/.xdev/secrets.yml: secrets for this project.
func projectSecretsPath(cwd string) string {
	return filepath.Join(cwd, ".xdev", "secrets.yml")
}

// warnf routes one diagnostic to the injected sink. A nil sink discards it:
// the config package owns no logger, and every caller of this file is on a
// startup path that must not fail.
func warnf(warn func(string), format string, args ...any) {
	if warn != nil {
		warn(fmt.Sprintf(format, args...))
	}
}

// LoadSecrets layers the two secrets.yml files — agent dir first, project
// second — where the project file may ADD entries and never replace one the
// profile already defines (#114): the redactor matches on the value, so a
// clone that shadows a well-known name would disarm redaction of a secret it
// never had. Absent, empty or unparsable files are skipped with one warning
// line each, because every caller here is on a startup path that must not
// fail.
func LoadSecrets(cwd string, warn func(string)) []SecretEntry {
	var (
		out   []SecretEntry
		index = map[string]int{}
	)
	for _, path := range []string{GlobalSecretsPath(), projectSecretsPath(cwd)} {
		for _, e := range readSecretsFile(path, warn) {
			if _, ok := index[e.Name]; ok {
				warnf(warn, "secrets: %s: %s is already set by the profile; ignored", path, e.Name)
				continue
			}
			index[e.Name] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// readSecretsFile decodes one secrets.yml. Every failure is a warning, never
// an error; within a file the first of two same-named entries wins.
func readSecretsFile(path string, warn func(string)) []SecretEntry {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			warnf(warn, "secrets: %s: %v (skipped)", path, err)
		}
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		warnf(warn, "secrets: %s: empty file (skipped)", path)
		return nil
	}
	entries, err := decodeSecrets(raw, warn)
	if err != nil {
		warnf(warn, "secrets: %s: %v (skipped)", path, err)
		return nil
	}
	if len(entries) == 0 {
		warnf(warn, "secrets: %s: no usable entries (skipped)", path)
		return nil
	}
	seen := map[string]bool{}
	out := entries[:0]
	for _, e := range entries {
		if seen[e.Name] {
			warnf(warn, "secrets: %s: duplicate entry %q (skipped)", path, e.Name)
			continue
		}
		seen[e.Name] = true
		out = append(out, e)
	}
	return out
}

// decodeSecrets accepts both spellings of the file — a bare list of entries
// and a mapping with a `secrets:` list — so either float works.
func decodeSecrets(raw []byte, warn func(string)) ([]SecretEntry, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil // comments only
	}
	root := doc.Content[0]
	switch root.Kind {
	case yaml.SequenceNode:
		return secretsFromSeq(root, warn), nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value != "secrets" {
				continue
			}
			if root.Content[i+1].Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("`secrets:` is not a list")
			}
			return secretsFromSeq(root.Content[i+1], warn), nil
		}
		return nil, fmt.Errorf("no `secrets:` list")
	default:
		return nil, fmt.Errorf("expected a list of entries")
	}
}

// secretsFromSeq decodes the entry list, skipping individually invalid rows:
// one bad entry must not cost the user every other secret.
func secretsFromSeq(seq *yaml.Node, warn func(string)) []SecretEntry {
	out := make([]SecretEntry, 0, len(seq.Content))
	for i, node := range seq.Content {
		e, err := entryFromNode(node, warn)
		if err != nil {
			warnf(warn, "secrets: entry %d: %v (skipped)", i+1, err)
			continue
		}
		out = append(out, e)
	}
	return out
}

// entryFromNode decodes one row with known fields, so a typo'd key ("vale:")
// is reported instead of leaving a secret silently in the clear. An unknown
// field warns and the row keeps the fields it did spell correctly.
func entryFromNode(node *yaml.Node, warn func(string)) (SecretEntry, error) {
	// Shorthand: a bare string row is a literal value.
	if node.Kind == yaml.ScalarNode {
		if node.Tag != "!!str" {
			return SecretEntry{}, fmt.Errorf("expected a mapping with name/value/pattern")
		}
		return SecretEntry{Value: node.Value}, nil
	}
	if node.Kind != yaml.MappingNode {
		return SecretEntry{}, fmt.Errorf("expected a mapping with name/value/pattern")
	}
	var e SecretEntry
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		var dst *string
		switch key.Value {
		case "name":
			dst = &e.Name
		case "value":
			dst = &e.Value
		case "pattern":
			dst = &e.Pattern
		default:
			warnf(warn, "secrets: unknown field %q (ignored)", key.Value)
			continue
		}
		if err := val.Decode(dst); err != nil {
			return e, fmt.Errorf("%s: %w", key.Value, err)
		}
	}
	return e, nil
}

// normalizeEntries names unnamed entries and drops rows that can never match.
// Dropping is the last resort (a dropped row is a secret left in the clear),
// so anything understood is kept: an entry carrying both a value and a pattern
// keeps the literal and reports the pattern as unused.
func normalizeEntries(entries []SecretEntry, warn func(string)) []SecretEntry {
	out := make([]SecretEntry, 0, len(entries))
	for i, e := range entries {
		e.Name = strings.TrimSpace(e.Name)
		if e.Name == "" {
			e.Name = fmt.Sprintf("entry-%d", i+1)
			warnf(warn, "secrets: unnamed entry %d (named %q)", i+1, e.Name)
		}
		if e.Value == "" && e.Pattern == "" {
			warnf(warn, "secrets: %s: no value or pattern (skipped)", e.Name)
			continue
		}
		if e.Value != "" && e.Pattern != "" {
			warnf(warn, "secrets: %s: both value and pattern set; using value", e.Name)
			e.Pattern = ""
		}
		if e.Value != "" && len(e.Value) < 4 {
			// Not skipped: the user asked for it. But a 1-3 character
			// "secret" rewrites ordinary prose, so say so once.
			warnf(warn, "secrets: %s: value is only %d characters and will match ordinary text", e.Name, len(e.Value))
		}
		out = append(out, e)
	}
	return out
}

// envSecretNames are the environment name fragments that mark a value as
// secret-bearing (matched case-insensitively).
var envSecretNames = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "PASS", "AUTH", "CREDENTIAL", "PRIVATE", "OAUTH"}

// EnvEntries collects the process environment's secret-bearing variables as
// literal entries (issue #55: env vars matching KEY/SECRET/TOKEN patterns).
// Values shorter than 8 characters are ignored — they are far more likely to
// be "true", "1", or a path than a credential — and repeated values collapse
// to one entry. Callers that want the environment covered prepend these to
// LoadSecrets' result; nothing here reads the environment by itself.
func EnvEntries(environ []string) []SecretEntry {
	var out []SecretEntry
	seen := map[string]bool{}
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < 8 || seen[value] {
			continue
		}
		upper := strings.ToUpper(name)
		hit := false
		for _, frag := range envSecretNames {
			if strings.Contains(upper, frag) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		seen[value] = true
		out = append(out, SecretEntry{Name: name, Value: value})
	}
	return out
}

// hashLen is the placeholder hash width ($$HASH8$$); it grows only to dodge a
// collision with another secret or matcher.
const hashLen = 8

// placeholderRE matches what Apply emits. Expansion ignores anything else, so
// prose that happens to contain $$...$$ is left alone.
var placeholderRE = regexp.MustCompile(`\$\$[0-9A-F]{8,64}\$\$`)

// Redactor replaces registered secrets with reversible placeholders and puts
// them back on the way in. A nil *Redactor is inert, so callers may wire one
// in unconditionally, and a Redactor with no entries is a pass-through.
// It is safe for concurrent use once constructed.
type Redactor struct {
	mu     sync.Mutex
	key    []byte
	byVal  map[string]string // secret text (literal or pattern match) -> placeholder
	byHash map[string]string // placeholder hash -> secret text
	lits   []string          // literal values, longest first
	pat    []*regexp.Regexp  // compiled patterns, in declaration order
}

// NewRedactor builds a redactor from entries, reporting dropped or dubious
// entries through warn (nil discards). It never fails: an unusable per-install
// key degrades to a process-local one.
func NewRedactor(entries []SecretEntry, warn func(string)) *Redactor {
	r := &Redactor{
		key:    placeholderKey(warn),
		byVal:  map[string]string{},
		byHash: map[string]string{},
	}
	lits := map[string]bool{}
	for _, e := range normalizeEntries(entries, warn) {
		if e.Pattern != "" {
			re, err := regexp.Compile(e.Pattern)
			if err != nil {
				warnf(warn, "secrets: %s: %v (skipped)", e.Name, err)
				continue
			}
			r.pat = append(r.pat, re)
			continue
		}
		if !lits[e.Value] {
			lits[e.Value] = true
			r.lits = append(r.lits, e.Value)
		}
	}
	// Longest first, so a value that contains another ("sk-abc123" over
	// "sk-abc") is matched whole.
	sort.SliceStable(r.lits, func(i, j int) bool { return len(r.lits[i]) > len(r.lits[j]) })
	// Give every literal its placeholder up front: that is what keeps the same
	// value hashing the same way on every call, and it makes Expand able to
	// resolve a placeholder stored by an earlier session.
	for _, v := range r.lits {
		r.placeholderFor(v)
	}
	return r
}

// OpenRedactor is the one call a startup path needs: load the layered
// secrets.yml files and build the redactor from them.
func OpenRedactor(cwd string, warn func(string)) *Redactor {
	return NewRedactor(LoadSecrets(cwd, warn), warn)
}

// active reports whether a redactor has anything to do (nil-safe).
func (r *Redactor) active() bool {
	return r != nil && (len(r.lits) > 0 || len(r.pat) > 0)
}

// placeholderFor returns the stable placeholder for one secret. The hash is
// extended past hashLen when the shorter form would collide with another
// value's hash or would itself be matched by a registered matcher — a
// placeholder that a later pass re-redacts, or that contains another secret,
// would corrupt the round-trip.
func (r *Redactor) placeholderFor(value string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ph, ok := r.byVal[value]; ok {
		return ph
	}
	sum := hmac.New(sha256.New, r.key)
	sum.Write([]byte(value))
	digest := strings.ToUpper(hex.EncodeToString(sum.Sum(nil)))
	var ph string
	for n := hashLen; n <= len(digest); n += 4 {
		h := digest[:n]
		if other, taken := r.byHash[h]; taken && other != value {
			continue
		}
		if r.placeholderMatched("$$" + h + "$$") {
			continue
		}
		ph = "$$" + h + "$$"
		break
	}
	if ph == "" {
		// 256 bits of HMAC: unreachable for a real secret.
		ph = "$$" + digest + "$$"
	}
	r.byVal[value] = ph
	r.byHash[ph[2:len(ph)-2]] = value
	return ph
}

// placeholderMatched reports whether another registered matcher would touch
// this candidate placeholder. Callers hold r.mu.
func (r *Redactor) placeholderMatched(ph string) bool {
	for _, v := range r.lits {
		if strings.Contains(ph, v) {
			return true
		}
	}
	for _, re := range r.pat {
		if re.MatchString(ph) {
			return true
		}
	}
	return false
}

// Apply replaces every registered secret in text with its placeholder —
// literals first (longest first), then patterns. It is the choke point for
// anything on its way to a provider: prompt text, tool result text, and
// (via ApplyValue) argument or file payloads. A nil or empty Redactor returns
// text unchanged.
func (r *Redactor) Apply(text string) string {
	if !r.active() || text == "" {
		return text
	}
	for _, v := range r.lits {
		if strings.Contains(text, v) {
			text = strings.ReplaceAll(text, v, r.placeholderFor(v))
		}
	}
	for _, re := range r.pat {
		if re.MatchString(text) {
			text = re.ReplaceAllStringFunc(text, r.placeholderFor)
		}
	}
	return text
}

// Expand restores every known placeholder in model-authored text (tool
// arguments, commands, follow-up prose). Unknown placeholders stay verbatim:
// expansion never invents a value. Best-effort by construction — a placeholder
// first minted for a *pattern* match by an earlier process is not in this
// process's map and cannot be recovered.
func (r *Redactor) Expand(text string) string {
	if !r.active() || !strings.Contains(text, "$$") {
		return text
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return placeholderRE.ReplaceAllStringFunc(text, func(ph string) string {
		if v, ok := r.byHash[ph[2:len(ph)-2]]; ok {
			return v
		}
		return ph
	})
}

// ApplyValue redacts every string leaf of a decoded payload — tool arguments,
// extension payloads. Best-effort: string, json.RawMessage, []byte, []any and
// map[string]any are walked; anything else is returned as it was.
func (r *Redactor) ApplyValue(v any) any {
	if !r.active() {
		return v
	}
	return walkStrings(v, r.Apply)
}

// ExpandValue restores placeholders in every string leaf of a decoded payload
// (model-authored tool arguments) before the tool runs.
func (r *Redactor) ExpandValue(v any) any {
	if !r.active() {
		return v
	}
	return walkStrings(v, r.Expand)
}

// walkStrings rebuilds a JSON-shaped value with f applied to its string leaves.
// Raw JSON is transformed as text so a payload a tool never decoded (a file
// write, a provider body) is still covered.
func walkStrings(v any, f func(string) string) any {
	switch x := v.(type) {
	case string:
		return f(x)
	case json.RawMessage:
		return json.RawMessage(f(string(x)))
	case []byte:
		return []byte(f(string(x)))
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = walkStrings(e, f)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = walkStrings(e, f)
		}
		return out
	default:
		return v
	}
}

var (
	placeholderKeyOnce sync.Once
	placeholderKeyRaw  []byte
)

// placeholderKey returns the per-install HMAC key, creating it on first use.
// One process shares one key, so two redactors (TUI, subagent) agree on hashes.
func placeholderKey(warn func(string)) []byte {
	placeholderKeyOnce.Do(func() { placeholderKeyRaw = loadPlaceholderKey(warn) })
	return placeholderKeyRaw
}

// loadPlaceholderKey reads (or mints) PlaceholderKeyPath with 0600. Every
// failure degrades to a process-local random key with a warning: redaction
// keeps working, it just stops surviving a restart.
func loadPlaceholderKey(warn func(string)) []byte {
	path := PlaceholderKeyPath()
	if raw, err := os.ReadFile(path); err == nil {
		// The file holds the key hex-encoded, so a read round-trips to the
		// same bytes a mint produced.
		if key, derr := hex.DecodeString(strings.TrimSpace(string(raw))); derr == nil && len(key) >= 16 {
			return key
		}
		warnf(warn, "secrets: %s: unusable key (regenerating; older placeholders will not expand)", path)
	} else if !os.IsNotExist(err) {
		warnf(warn, "secrets: %s: %v", path, err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		warnf(warn, "secrets: no placeholder key available: %v (using a time-derived one)", err)
		return []byte(fmt.Sprintf("xdev-%d", time.Now().UnixNano()))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		if err := os.WriteFile(path, append([]byte(hex.EncodeToString(key)), '\n'), 0o600); err != nil {
			warnf(warn, "secrets: %s: %v (key is session-local)", path, err)
		}
	} else {
		warnf(warn, "secrets: %s: %v (key is session-local)", path, err)
	}
	return key
}
