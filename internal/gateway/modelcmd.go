package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
)

// /model — show or switch the model THIS chat's turns run on.
//
// Why per chat and not per daemon: one bot can serve several people, and a
// model chosen in one conversation must not silently move another's answers.
// The choice is persisted in the chat registry (gateway/sessions.json), so it
// survives a daemon restart, and it is resolved at SPAWN time (modelFor) so a
// ref the operator later removes from models.yml cannot wedge a chat.
//
// The ref reaches the worker as the ordinary `--model` flag, so every guard
// the CLI already has applies unchanged: a bare id expands against the
// catalog, a `:effort` suffix is split off, and an unknown provider fails
// loudly in the worker's own error path.

// loadModels reads the catalog /model resolves against. The daemon holds the
// loader so a test drives /model against a fixed catalog rather than whatever
// models.yml happens to be on the machine.
func (d *Daemon) loadModels() (*config.Config, error) {
	if d.models != nil {
		return d.models()
	}
	return config.LoadModelsLayered()
}

// modelCommand answers /model: with no argument it reports this chat's model
// and what else the catalog offers; with one it pins that ref.
func (d *Daemon) modelCommand(ctx context.Context, chat int64, text string) {
	arg := strings.TrimSpace(strings.TrimPrefix(text, "/model"))
	if arg == "" {
		_ = d.reply(ctx, chat, d.modelReport(chat))
		return
	}
	if arg == "default" || arg == "reset" {
		if err := d.chats.SetModel(chat, ""); err != nil {
			d.logFn("gateway: clear model for chat %d: %v", chat, err)
			_ = d.reply(ctx, chat, "Could not save the model choice: "+err.Error())
			return
		}
		ref := d.modelFor(chat)
		if ref == "" {
			ref = "the run's configured default"
		}
		_ = d.reply(ctx, chat, "This chat now uses "+ref+".")
		return
	}
	ref, err := d.resolveModelArg(arg)
	if err != nil {
		_ = d.reply(ctx, chat, err.Error())
		return
	}
	if err := d.chats.SetModel(chat, ref); err != nil {
		d.logFn("gateway: pin model for chat %d: %v", chat, err)
		_ = d.reply(ctx, chat, "Could not save the model choice: "+err.Error())
		return
	}
	_ = d.reply(ctx, chat, "This chat now runs on "+ref+". The next message uses it.")
}

// resolveModelArg turns what the user typed into a ref, refusing the two
// mistakes that would otherwise fail one turn later inside the worker: an
// unknown provider, and a bare id that matches several catalog entries.
//
// The catalog check stays advisory for an explicit provider/model — a gateway
// serves ids models.yml never pinned, and rejecting one here would make them
// unusable. Only a bare id has to resolve, because nothing else disambiguates
// it.
func (d *Daemon) resolveModelArg(arg string) (string, error) {
	// A `:effort` suffix belongs to the request, not the model id. The worker
	// splits it off itself; stripping it here is only so the bare-id lookup
	// below sees the id.
	base, effort := arg, ""
	if b, suffix, ok := strings.Cut(arg, ":"); ok && isEffortName(suffix) {
		base, effort = b, suffix
	}
	cfg, err := d.loadModels()
	if err != nil {
		// A broken models.yml still lets an explicit ref through: the worker
		// reports the real problem, and refusing here would hide it.
		d.logFn("gateway: models.yml: %v", err)
		return arg, nil
	}
	switch {
	case !strings.Contains(base, "/"):
		expanded, err := expandBareID(cfg, base, d.catalog(cfg))
		if err != nil {
			return "", err
		}
		base = expanded
	default:
		provider, _, perr := config.ParseModelRef(base)
		if perr != nil {
			return "", fmt.Errorf("%q is not a model reference: use provider/model", arg)
		}
		if _, ok := cfg.Providers[provider]; !ok {
			return "", fmt.Errorf("unknown provider %q — send /model to list what is configured", provider)
		}
	}
	if effort != "" {
		return base + ":" + effort, nil
	}
	return base, nil
}

// isEffortName reports whether s names a reasoning rung — the same list the
// CLI splits a `model:effort` ref on.
func isEffortName(s string) bool {
	for _, e := range config.EffortLevels {
		if s == e {
			return true
		}
	}
	return false
}

// catalog is every provider/model the catalog offers, sorted: models.yml
// pinning merged with live discovery, the same merge a run resolves through.
// Discovery is best-effort (a down server costs a few rows, never the
// command), and it is asked ONCE per call so /model does not probe each
// provider twice.
func (d *Daemon) catalog(cfg *config.Config) []string {
	var out []string
	for name, pc := range cfg.Providers {
		if pc == nil {
			continue
		}
		for _, m := range d.providerModels(name, pc) {
			out = append(out, name+"/"+m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// providerModels is a provider's pinned models plus whatever its discovery
// endpoint lists.
func (d *Daemon) providerModels(name string, pc *config.ProviderConfig) []config.ModelConfig {
	out := pc.Models
	if pc.Discovery == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), config.DiscoveryTimeout)
	defer cancel()
	found, err := config.DiscoverModels(ctx, pc)
	if err != nil {
		d.logFn("gateway: discovery %s: %v", name, err)
		return out
	}
	return append(append([]config.ModelConfig(nil), out...), found...)
}

// expandBareID resolves a bare model id against the catalog, the same
// convenience `xdev --model dev` offers. One match wins; zero or several are
// an error naming the candidates, because guessing between two providers
// would run the chat on a model nobody chose.
//
// ponytail: this is cmd/xdev's expandBareModelID, which lives in package main
// and carries that package's provider cache. If a third caller appears, lift
// one copy into internal/config.
func expandBareID(cfg *config.Config, id string, catalog []string) (string, error) {
	var hits []string
	for _, ref := range catalog {
		provider, model, err := config.ParseModelRef(ref)
		if err != nil {
			continue
		}
		if strings.EqualFold(model, id) {
			hits = append(hits, provider+"/"+model)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("unknown model %q — send /model to list what is available", id)
	default:
		return "", fmt.Errorf("model %q matches %s — use provider/model", id, strings.Join(hits, ", "))
	}
}

// modelFor resolves the ref this chat's next turn runs on: the chat's own
// choice, else the daemon-wide gateway.model, else "" (the worker's own
// configured default, i.e. no --model flag at all).
func (d *Daemon) modelFor(chat int64) string {
	if ref := d.chats.Model(chat); ref != "" {
		return ref
	}
	return d.model
}

// modelReport is what bare /model answers: the model in force, and the
// catalog to choose from. The list is capped — a chat message is not a
// picker, and a gateway serving 100+ ids would otherwise flood the phone.
func (d *Daemon) modelReport(chat int64) string {
	var b strings.Builder
	current := d.modelFor(chat)
	switch {
	case d.chats.Model(chat) != "":
		fmt.Fprintf(&b, "model (this chat): %s\n", current)
	case d.model != "":
		fmt.Fprintf(&b, "model: %s (gateway.model)\n", current)
	default:
		b.WriteString("model: the run's configured default\n")
	}
	cfg, err := d.loadModels()
	if err != nil {
		fmt.Fprintf(&b, "\ncould not read models.yml: %v", err)
		return b.String()
	}
	refs := d.catalog(cfg)
	if len(refs) == 0 {
		b.WriteString("\nno models configured: add models.yml (or run `xdev setup`)")
		return b.String()
	}
	const shown = 20
	b.WriteString("\navailable (send /model <ref>):\n")
	for i, ref := range refs {
		if i == shown {
			fmt.Fprintf(&b, "  … %d more (see `xdev models`)\n", len(refs)-shown)
			break
		}
		mark := "  "
		if ref == current {
			mark = "→ "
		}
		fmt.Fprintf(&b, "%s%s\n", mark, ref)
	}
	return strings.TrimRight(b.String(), "\n")
}
