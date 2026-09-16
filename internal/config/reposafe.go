// This file is the repository-trust boundary (#114). Configuration arrives
// from two authorities with opposite rights: the user's profile under
// ~/.xdev, chosen by the human, and <cwd>/.xdev/*, which arrives with a
// `git clone` and was therefore written by a stranger. These two must not
// share a key set, because the settings schema contains keys that name an
// executable (bash.interceptor, hooks, lsp, debug), an endpoint that receives
// the whole conversation (baseUrl, hindsight, webSearch), a credential
// (apiKey, apiKeys, headers, authHeader) or the approval policy itself
// (approvalMode, toolsApproval, bashPatterns).
//
// So the project layer is an ALLOWLIST, not a denylist: a field added to
// Settings or ProviderConfig is repository-unreachable until someone puts it
// here deliberately. A denylist would hand every future key to clones by
// default, which is how this defect class gets reintroduced.
//
// What a repository may still configure is the part with no authority in it:
// how the interface looks and how much a session may spend.
package config

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The two repository-supplied configuration files, spelled once so the load
// path and the startup notice cannot drift apart.
const (
	projectSettingsName = ".xdev/config.yml"
	projectModelsName   = ".xdev/models.yml"
)

// repoSafeSettingsKeys are the top-level keys <cwd>/.xdev/config.yml may set.
// Everything else the file names is dropped and reported.
var repoSafeSettingsKeys = []string{
	// Interface: how the transcript and HUD look.
	"theme", "colorBlindMode", "statusLine", "showThinking", "sidebarMode",
	// Resource caps: how much a session may spend. A repo may lower these,
	// never widen what it can do.
	"memoryLimit", "maxTurns", "compaction", "branchSummary",
	"experimentalContextManagement", "models",
	// Per-tool knobs that name neither a binary nor an endpoint.
	"ask", "tts",
}

// init sorts the allowlists so keepMapping's membership test is a binary
// search: the lists stay grouped by purpose for the reader, and the boundary
// cannot be widened by a later edit that breaks the order.
func init() {
	slices.Sort(repoSafeSettingsKeys)
	slices.Sort(repoSafeProviderKeys)
	slices.Sort(repoSafeModelKeys)
}

// repoSafeProviderKeys are the fields a project .xdev/models.yml may set per
// provider: how to talk to it, not where it lives or what it costs.
var repoSafeProviderKeys = []string{"api", "models", "discovery", "toolsFormat"}

// repoSafeModelKeys are the fields a project may set per model entry. The
// per-model baseUrl/apiKey/headers overrides are absent on purpose: they are
// the provider-level authority in miniature.
var repoSafeModelKeys = []string{"id", "name", "reasoning", "vision", "contextWindow", "maxTokens"}

// readYAMLLayer reads one configuration file; ok=false when it is absent.
func readYAMLLayer(path string) (raw []byte, ok bool, err error) {
	raw, err = os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return raw, true, nil
}

// parseYAMLLayer decodes bytes into out with unknown keys rejected — the same
// strictness every layer has always had.
func parseYAMLLayer(raw []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// pruneTo decodes an untrusted layer through an allowlist: parse the document,
// drop every key `keep` rejects, then decode what is left. It returns the
// dotted names it dropped. Re-marshaling rather than decoding the node is
// deliberate: yaml.Node.Decode ignores unknown fields, so pruning would cost
// the layer the typo check that every other layer keeps.
func pruneTo(raw []byte, out any, keep func(body *yaml.Node) []string) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	body := documentBody(&doc)
	ignored := keep(body)
	pruned, err := yaml.Marshal(body)
	if err != nil {
		return nil, err
	}
	if err := parseYAMLLayer(pruned, out); err != nil {
		return nil, err
	}
	return ignored, nil
}

// documentBody unwraps the document node yaml.Unmarshal produces (one with a
// single DocumentNode child) so callers walk the mapping itself.
func documentBody(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0]
	}
	return doc
}

// keepMapping drops the entries of one mapping node that `keep` rejects,
// recording each dropped key (dotted, prefixed) into ignored.
func keepMapping(n *yaml.Node, keep []string, prefix string, ignored *[]string) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	held := make([]*yaml.Node, 0, len(n.Content))
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if slices.Contains(keep, key.Value) {
			held = append(held, key, val)
			continue
		}
		*ignored = append(*ignored, prefix+key.Value)
	}
	n.Content = held
}

// mappingPairs returns a mapping's key/value pairs; nil for any other node
// kind, so a malformed layer prunes to nothing instead of panicking.
func mappingPairs(n *yaml.Node) [][2]*yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([][2]*yaml.Node, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, [2]*yaml.Node{n.Content[i], n.Content[i+1]})
	}
	return out
}

// mappingValue returns the node a mapping key holds, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	for _, p := range mappingPairs(n) {
		if p[0].Value == key {
			return p[1]
		}
	}
	return nil
}

// pruneProjectSettings is the keep function for <cwd>/.xdev/config.yml.
func pruneProjectSettings(body *yaml.Node) []string {
	var ignored []string
	keepMapping(body, repoSafeSettingsKeys, "", &ignored)
	return ignored
}

// pruneProjectModels is the keep function for <cwd>/.xdev/models.yml: model
// metadata stays, every destination and every credential goes.
func pruneProjectModels(body *yaml.Node) []string {
	var ignored []string
	keepMapping(body, []string{"providers"}, "", &ignored)
	for _, ent := range mappingPairs(mappingValue(body, "providers")) {
		id, block := ent[0].Value, ent[1]
		prefix := "providers." + id + "."
		keepMapping(block, repoSafeProviderKeys, prefix, &ignored)
		if models := mappingValue(block, "models"); models != nil && models.Kind == yaml.SequenceNode {
			for _, m := range models.Content {
				keepMapping(m, repoSafeModelKeys, prefix+"models[].", &ignored)
			}
		}
	}
	return ignored
}

// ignoredKeyNotice is the startup notice naming what a repository tried to
// configure and was refused. An empty ignored list prints nothing. The
// boundary is deliberately loud: a silently dropped key is how a user ends up
// believing a clone configured something it did not.
func ignoredKeyNotice(file string, ignored []string) string {
	if len(ignored) == 0 {
		return ""
	}
	names := slices.Clone(ignored)
	slices.Sort(names)
	names = slices.Compact(names)
	return fmt.Sprintf("xdev: %s came with this repository, so %d key(s) it named were ignored: %s\n"+
		"     a clone may configure how xdev looks and how much a session may spend; authority — approval\n"+
		"     mode, models, hooks, endpoints, credentials — belongs in %s",
		file, len(names), strings.Join(names, ", "), GlobalSettingsPath())
}

// projectModelsNotice covers the other file a repository can ship: it reads
// only .xdev/models.yml, because the notice has to be printable at startup
// while the layered registry is loaded later, by whichever mode needs it.
func projectModelsNotice() string {
	raw, ok, err := readYAMLLayer(projectModelsName)
	if err != nil || !ok {
		return ""
	}
	var cfg Config
	ignored, err := pruneTo(raw, &cfg, pruneProjectModels)
	if err != nil {
		return "" // an unparseable layer is reported by the load, with the path
	}
	return ignoredKeyNotice(projectModelsName, ignored)
}

// RepoTrustNotices is what a repository's own configuration tried to set and
// the trust boundary refused (#114). One startup call, one line per file, and
// nothing at all when the repository asked for nothing it was not entitled
// to — a key dropped on the user's behalf must never go quiet.
func RepoTrustNotices(s *Settings) []string {
	var out []string
	if n := s.RepoTrustNotice(); n != "" {
		out = append(out, n)
	}
	if n := projectModelsNotice(); n != "" {
		out = append(out, n)
	}
	return out
}
