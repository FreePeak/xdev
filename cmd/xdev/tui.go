package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/tui"
)

// runTUI drives the interactive TUI mode (M4).
func runTUI(opts printOptions, themeName string) (exitCode int, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return 2, err
	}

	// Config & model (same path as print mode).
	cfg, err := config2Load()
	if err != nil {
		return 2, err
	}
	modelRef := opts.Model
	if modelRef == "" {
		modelRef = os.Getenv("XDEV_MODEL")
	}
	if modelRef == "" {
		modelRef = cfg.DefaultModelRef()
	}
	if modelRef == "" {
		return 2, fmt.Errorf("no model configured: add ~/.xdev/agent/models.yml or pass --model provider/model")
	}
	provName, modelName, err := config.ParseModelRef(modelRef)
	if err != nil {
		return 2, err
	}
	pc, ok := cfg.Providers[provName]
	if !ok {
		return 2, fmt.Errorf("unknown provider %q (have: %v)", provName, providerKeys(cfg))
	}
	prov, err := buildProvider(provName, pc)
	if err != nil {
		return 2, err
	}

	// Tools + system prompt (shared with print mode).
	reg := newToolRegistry(cwd, prov, modelName)
	sys := opts.SystemPrompt
	if sys == "" {
		sys = agent.SystemPromptBase
	}
	defs := reg.Defs()
	named := make([]agent.NamedToolDef, 0, len(defs))
	for _, d := range defs {
		named = append(named, agent.NamedToolDef{Name: d.Name, Description: d.Description})
	}
	sys = agent.BuildSystemPrompt(sys, agent.LoadContextFiles(cwd), named)
	if opts.AppendSystem != "" {
		sys += "\n\n" + opts.AppendSystem
	}

	// Session.
	store, err := openSession(cwd, opts.ContinueLast, opts.ResumePrefix)
	if err != nil {
		return 2, fmt.Errorf("session: %w", err)
	}
	saveBreadcrumb(store.Path())
	wireTaskParent(reg, store)
	defer func() {
		_ = store.Append(&session.ModelChangeEntry{Model: modelRef})
		_ = store.Append(&session.CustomEntry{CustomType: "session_exit", Data: map[string]any{"mode": "tui", "code": exitCode}})
		if cerr := store.Close(); cerr != nil {
			logx.Errorf("session close: %v", cerr)
		}
	}()

	// Screen.
	th := theme.Load(themeName)
	scr, err := tcell.NewScreen()
	if err != nil {
		return 2, fmt.Errorf("tui: screen: %w", err)
	}
	setCursorColor(th.Get(theme.AccentUser)) // OSC 12 (survives into raw mode)
	if err := scr.Init(); err != nil {
		return 2, fmt.Errorf("tui: init: %w", err)
	}
	// Wheel events drive the in-app transcript scroll, not the host
	// terminal's own scrollback (#17 follow-up, user-reported 2026-09-10).
	scr.EnableMouse()
	defer scr.Fini()
	defer setCursorReset()

	app := tui.New(scr, th, modelRef, store.ID())

	// Replay resumed history as read-only blocks (text only).
	if opts.ContinueLast {
		if res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{}); err == nil {
			for _, m := range res.Messages {
				switch m.Role {
				case ai.RoleUser:
					if txt := m.Text(); txt != "" {
						app.AddUserBlock(txt)
					}
				case ai.RoleAssistant:
					if txt := m.Text(); txt != "" {
						app.AddAssistantBlock(txt)
					}
				}
			}
		}
	}
	// Live conversation is the store: user/assistant/toolResult messages
	// are persisted by the hooks and Agent.persist, so each submit rebuilds
	// history from the store (compaction entries replay correctly through
	// BuildContext). Seeding from a separate slice went stale and dropped
	// assistant turns (conversation amnesia).
	rebuildHistory := func() []ai.Message {
		res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
		if err != nil {
			return nil
		}
		return res.Messages
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()

	var running atomic.Bool
	// Serializes conversation accumulation across turns (one run at a time;
	// guarded for the UI thread that reads nothing here).
	var sessMu sync.Mutex

	ts := &tuiSession{store: store, app: app, model: modelName, api: prov.API(), provider: provName}

	var swapStoreTo func(*session.Store) error

	// swapStore closes the current session and opens a fresh one (issue #11).
	// drop=true deletes the old file first. The transcript clears and the
	// live hooks/agent point at the new store (single source of truth: the
	// captured `store` variable, which all closures re-read).
	swapStore := func(drop bool) error {
		old := store
		ns, err := openSession(cwd, false, "")
		if err != nil {
			return err
		}
		if drop && old.Path() != "" {
			_ = old.Close()
			if rmErr := os.Remove(old.Path()); rmErr != nil && !os.IsNotExist(rmErr) {
				logx.Errorf("drop session file: %v", rmErr)
			}
		} else {
			_ = old.Close()
		}
		store = ns
		ts.store = ns
		app.Reset()
		saveBreadcrumb(ns.Path())
		app.AddSystemBlock("· new session " + shortSessionID(ns.ID()))
		return nil
	}

	// swapStoreTo adopts an already-open store (fork/resume): replays its
	// transcript and points hooks/agent at it.
	swapStoreTo = func(ns *session.Store) error {
		old := store
		store = ns
		ts.store = ns
		wireTaskParent(reg, ns)
		app.Reset()
		saveBreadcrumb(ns.Path())
		if res, err := session.BuildContext(ns.Entries(), ns.LeafID(), session.SystemPrompt{}); err == nil {
			for _, m := range res.Messages {
				switch m.Role {
				case ai.RoleUser:
					if txt := m.Text(); txt != "" {
						app.AddUserBlock(txt)
					}
				case ai.RoleAssistant:
					if txt := m.Text(); txt != "" {
						app.AddAssistantBlock(txt)
					}
				}
			}
		}
		app.AddSystemBlock("· session " + shortSessionID(ns.ID()) + " — " + ns.Title())
		_ = old
		return nil
	}

	// Session lifecycle (issue #11): /new swaps in a fresh session file,
	// /clear resets in place (durable reset_boundary, history kept on
	// disk), /drop deletes the file and starts fresh. All refuse while a
	app.SetLocation(cwd)
	// turn is in flight.
	app.SetSessionOps(&tui.SessionOps{
		Fork: func() error {
			if running.Load() {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			// A fresh session lives memory-only until its first
			// assistant message — materialize it so the fork has a
			// source file to copy.
			if store.Path() == "" {
				if _, err := store.EnsureOnDisk(
					session.SessionFilePath(config.DataDir(), cwd, time.Now(), store.ID()), session.Options{}); err != nil {
					return err
				}
			}
			fork, err := session.ForkSession(store.Path(),
				session.SessionFilePath(config.DataDir(), cwd, time.Now(), session.NewSessionID()), "")
			if err != nil {
				return err
			}
			return swapStoreTo(fork)
		},
		Dump: func() (string, error) {
			return dumpSession(store)
		},
		Resume: func(query string) error {
			if running.Load() {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			if query == "" {
				return fmt.Errorf("usage: /resume <session-id-prefix>")
			}
			path, err := resolveResumeID(cwd, query)
			if err != nil {
				return err
			}
			resumed, err := session.Open(path)
			if err != nil {
				return err
			}
			return swapStoreTo(resumed)
		},
		New: func() error {
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			return swapStore(false)
		},
		Clear: func() error {
			if running.Load() {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			if err := store.ResetLeaf(); err != nil {
				return err
			}
			app.Reset()
			return nil
		},
		Drop: func() error {
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			return swapStore(true)
		},
	})
	app.SetCommandDir(cwd)

	app.SetHandlers(
		func(text string) {
			if !running.CompareAndSwap(false, true) {
				app.AddSystemBlock("a turn is already running — Esc cancels it")
				return
			}
			sessMu.Lock()
			msg := ai.Message{
				Role:        ai.RoleUser,
				Content:     []ai.Block{ai.TextBlock{Text: text}},
				Attribution: "user",
				UserTS:      time.Now().UnixMilli(),
			}
			sessMu.Unlock()
			if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
				logx.Errorf("persist user message: %v", err)
			}
			ctx, cancel := context.WithCancel(baseCtx)
			go func() {
				defer cancel()
				defer running.Store(false)
				app.SetRunning(true)
				ag := &agent.Agent{
					Provider:   prov,
					Tools:      reg,
					Hooks:      &tuiHooks{ts: ts},
					MaxTokens:  opts.MaxTokens,
					MaxTurns:   opts.MaxTurns,
					Model:      modelName,
					Store:      store,
					Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, provName, modelName)},
					Failovers:  failoverChain(cfg, provName, modelName),
				}
				sessMu.Lock()
				hist := rebuildHistory() // store mirror is authoritative
				sessMu.Unlock()
				_, err := ag.Run(ctx, sys, hist)
				app.EndAssistant()
				app.FinishRun()
				if err != nil {
					if ctx.Err() != nil {
						app.AddSystemBlock("· turn canceled")
					} else {
						app.AddSystemBlock("error: " + err.Error())
					}
				}
			}()
		},
		func() { baseCancel() }, // Esc: abort the in-flight turn (all runs share baseCtx)

		func() { app.Quit() },
	)

	app.Run() // blocks until Quit
	return 0, nil
}

// tuiSession accumulates the live conversation and persists messages.
type tuiSession struct {
	store    *session.Store
	app      *tui.App
	model    string
	api      string
	provider string
}

// tuiHooks implements agent.TurnHooks for the TUI.
type tuiHooks struct {
	ts *tuiSession
}

func (h *tuiHooks) OnStart(req ai.StreamRequest) {}

func (h *tuiHooks) OnEvent(ev ai.Event) {
	switch ev.Type {
	case ai.EventTextStart:
		h.ts.app.BeginAssistant()
	case ai.EventTextDelta:
		h.ts.app.AppendAssistant(ev.Delta)
	case ai.EventTextEnd:
		h.ts.app.EndAssistant()
	case ai.EventThinkingStart:
		h.ts.app.BeginThinking()
	case ai.EventThinkingDelta:
		h.ts.app.AppendThinking(ev.Delta)
	case ai.EventThinkingEnd:
		h.ts.app.EndThinking()
	case ai.EventDone:
		h.ts.app.EndAssistant()
		if ev.Usage != nil {
			h.ts.app.AddUsage(ev.Usage.Input, ev.Usage.Output)
		}
	case ai.EventError:
		h.ts.app.AddSystemBlock("stream error: " + ev.Err.Error())
	}
}

// shortSessionID renders the first 8 chars of a session id (matches the TUI
// status line convention).
func shortSessionID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (h *tuiHooks) OnToolStart(call ai.ToolCallBlock) {
	preview := strings.Join(strings.Fields(string(call.Arguments)), " ")
	if len(preview) > 120 {
		preview = preview[:120] + "…"
	}
	h.ts.app.AddToolBlock(call.Name, preview)
}

func (h *tuiHooks) OnToolEnd(call ai.ToolCallBlock, res tool.Result, dur time.Duration) {
	h.ts.app.FinishTool(call.Name, res.IsError, res.Text, dur.Round(time.Millisecond).String())
}

// OnMessageEnd persists the assistant message (persistence on message_end).
func (h *tuiHooks) OnMessageEnd(msg *ai.Message) {
	if msg == nil || msg.Role != ai.RoleAssistant {
		return
	}
	if err := h.ts.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist assistant message: %v", err)
	}
}

func (h *tuiHooks) OnToolResultMessage(msg *ai.Message) {
	if err := h.ts.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist toolResult: %v", err)
	}
}

func (h *tuiHooks) OnTurnEnd(reason ai.StopReason, err error) {}
func (h *tuiHooks) OnCompaction(tokensBefore int64) {
	h.ts.app.AddSystemBlock(fmt.Sprintf("· context compacted (~%d tokens)", tokensBefore))
}

// config2Load is a tiny alias so tui.go shares print.go's loader.
func config2Load() (*config.Config, error) { return config.LoadModelsLayered() }

// setCursorColor writes the OSC 12 cursor-color sequence to the terminal
// before tcell takes over raw mode.
func setCursorColor(c theme.Color) {
	fmt.Fprintf(os.Stdout, "\033]12;#%02x%02x%02x\033\\", c.R, c.G, c.B)
}

// setCursorReset restores the terminal's default cursor color (OSC 112).
func setCursorReset() {
	fmt.Fprint(os.Stdout, "\033]112\033\\")
}
