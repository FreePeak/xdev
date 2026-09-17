package main

import (
	"context"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
)

// Ai-title cascade (M10 #32 consumer, parity finding #107).
//
// The session title used to be purely mechanical ("print 2026-09-13 04:10"),
// which made /resume a list of timestamps. This asks the session model for a
// few-word title over the first exchange, using TITLE_SYSTEM.md as the system
// prompt when the host ships one (the file was discovered and exposed via
// agent.SystemPromptOverrides.TitleSystemPrompt() with no consumer at all).
//
// It is best-effort by design: a title is a nicety, so every failure path
// (unresolvable model, provider error, timeout, unusable answer) logs at debug
// and leaves the mechanical title standing. A user's /rename always wins —
// see shouldGenerateTitle.

// titleRequestCap bounds the title request: a few tokens of prose, and a run
// must never wait on it.
const (
	titleRequestCap = 12 * time.Second
	titleMaxTokens  = 32
	titleMaxRunes   = 48
)

// The title request runs on the session's own model: a one-line job on an
// already-warm provider, one 32-token request, and the cost warning lives in
// the prompt (titleMaxTokens).

// shouldGenerateTitle reports whether the session wants a generated title:
// never for a subagent (its title marks the parent), never over a manual
// rename, and only while the title still looks mechanical.
func shouldGenerateTitle(store *session.Store) bool {
	if store == nil || store.Path() == "" {
		return false // nothing on disk to rewrite
	}
	title := store.Title()
	if title == "" {
		return false
	}
	// Manual renames and subagent titles are authoritative.
	_, source, _ := session.ParseTitleSlotSource(readTitleLine(store))
	if source == session.TitleSourceManual || source == session.TitleSourceSubagent {
		return false
	}
	// Mechanical titles start with the mode word; anything else was set by a
	// person or a previous generation pass.
	for _, prefix := range []string{"print ", "continued print ", "tui ", "imported: "} {
		if strings.HasPrefix(title, prefix) {
			return true
		}
	}
	return false
}

// readTitleLine returns the store's line-1 slot bytes (nil when the session is
// not on disk yet).
func readTitleLine(store *session.Store) []byte {
	return session.ReadTitleSlot(store.Path())
}

// generateTitle asks the title cascade for a short title over the opening
// exchange and stamps it. Safe to call on a goroutine: the store's own lock
// serializes the slot rewrite against appends.
func generateTitle(cfg *config.Config, settings *config.Settings, cwd, provider, model string, store *session.Store, history []ai.Message) {
	if !shouldGenerateTitle(store) {
		return
	}
	firstUser, lastAssistant := titleSeed(history)
	if firstUser == "" {
		return
	}
	system := agent.LoadSystemPromptOverrides(cwd).TitleSystemPrompt()
	if strings.TrimSpace(system) == "" {
		system = defaultTitleSystem
	}
	ctx, cancel := context.WithTimeout(context.Background(), titleRequestCap)
	defer cancel()
	targets := []string{sessionModelRef(provider, model)}
	for _, ref := range targets {
		if ref == "" {
			continue // no live session model to fall back to
		}
		prov, model, err := titleProvider(cfg, settings, ref)
		if err != nil {
			logx.Debugf("ai-title: %s unavailable: %v", ref, err)
			continue
		}
		msg, err := ai.Complete(ctx, prov, model, system, titlePrompt(firstUser, lastAssistant), titleMaxTokens)
		if err != nil {
			logx.Debugf("ai-title: %s request failed: %v", ref, err)
			continue
		}
		title := cleanTitle(msg.Text())
		if title == "" {
			logx.Debugf("ai-title: %s returned no usable title", ref)
			continue
		}
		if err := store.Rename(title, session.TitleSourceAuto); err != nil {
			logx.Debugf("ai-title: stamp failed: %v", err)
			return
		}
		logx.Debugf("ai-title: %s -> %q", ref, title)
		return
	}
}

// sessionModelRef names the session's own provider/model as a "provider/model"
// ref so the cascade can fall back to it through the same resolver. Empty when
// the caller has no live target (a hand-built agent).
func sessionModelRef(provider, model string) string {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return ""
	}
	return provider + "/" + model
}

// defaultTitleSystem is the built-in title instruction, replaced by
// TITLE_SYSTEM.md when the host ships one.
const defaultTitleSystem = `You write session titles. Reply with ONLY the title: two to five words, lowercase, no quotes, no trailing punctuation, no sentence. It names what the session is about so a list of sessions can be scanned.`

// titlePrompt renders the opening exchange into the request. It is capped to
// a few hundred runes per side: the title needs the subject, not the thread.
func titlePrompt(firstUser, lastAssistant string) string {
	cut := func(s string, n int) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	}
	var b strings.Builder
	b.WriteString("User: " + cut(oneLine(firstUser), 400))
	if lastAssistant != "" {
		b.WriteString("\nAssistant: " + cut(oneLine(lastAssistant), 200))
	}
	return b.String()
}

// titleSeed picks the exchange the title is written from: the first user
// message and the last assistant reply so far.
func titleSeed(history []ai.Message) (string, string) {
	firstUser, lastAssistant := "", ""
	for _, m := range history {
		switch m.Role {
		case ai.RoleUser:
			if firstUser == "" {
				firstUser = m.Text()
			}
		case ai.RoleAssistant:
			if t := m.Text(); t != "" {
				lastAssistant = t
			}
		}
	}
	return firstUser, lastAssistant
}

// titleProvider resolves one model reference into a usable provider.
func titleProvider(cfg *config.Config, settings *config.Settings, ref string) (ai.Provider, string, error) {
	resolved, _, err := resolveModel(ref, cfg, settings)
	if err != nil {
		return nil, "", err
	}
	pName, mName, err := config.ParseModelRef(resolved)
	if err != nil {
		return nil, "", err
	}
	pc, ok := cfg.Providers[pName]
	if !ok {
		return nil, "", &configError{pName}
	}
	prov, err := buildProvider(pName, pc, mName, cfg)
	if err != nil {
		return nil, "", err
	}
	return prov, mName, nil
}

type configError struct{ provider string }

func (e *configError) Error() string { return "unknown provider " + e.provider }

// cleanTitle normalizes a model answer into a title: first line only, quotes
// and trailing punctuation stripped, capped, and rejected when it is clearly
// not a title (empty, a whole sentence, or a refusal-shaped blob).
func cleanTitle(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.Trim(s, `"'“”‘’`)
	s = strings.TrimRight(s, ".,:;!?")
	s = oneLine(s)
	if s == "" || strings.EqualFold(s, "title") {
		return ""
	}
	// Reject first, truncate second: an answer that reads like a sentence means
	// the model ignored the format, and squeezing a paragraph into 48 runes
	// would put a mangled non-title in the picker. Keep the mechanical one.
	if strings.ContainsAny(s, "!?") || len(strings.Fields(s)) > 10 {
		return ""
	}
	if r := []rune(s); len(r) > titleMaxRunes {
		s = strings.TrimRight(string(r[:titleMaxRunes]), " ,;:")
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
