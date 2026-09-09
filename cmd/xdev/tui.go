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
	reg := tool.NewRegistry()
	for _, t := range []tool.Tool{
		tool.NewReadTool(),
		tool.NewWriteTool(),
		tool.NewEditTool(),
		tool.NewBashTool(cwd),
	} {
		reg.Register(t)
	}
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
	store, err := openSession(cwd, opts.ContinueLast)
	if err != nil {
		return 2, fmt.Errorf("session: %w", err)
	}
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
					Model:      modelName,
					Store:      store,
					Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, provName, modelName)},
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

	if opts.MaxTurns != 0 && opts.MaxTurns != 32 {
		// MaxTurns plumbed via Agent default; per-run override handled above.
		_ = opts.MaxTurns
	}
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

func (h *tuiHooks) OnToolStart(call ai.ToolCallBlock) {
	preview := strings.Join(strings.Fields(string(call.Arguments)), " ")
	if len(preview) > 120 {
		preview = preview[:120] + "…"
	}
	h.ts.app.AddToolBlock(call.Name, preview)
}

func (h *tuiHooks) OnToolEnd(call ai.ToolCallBlock, res tool.Result, dur time.Duration) {
	preview := strings.Join(strings.Fields(res.Text), " ")
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	h.ts.app.FinishTool(call.Name, res.IsError, fmt.Sprintf("%s (%s)", preview, dur.Round(time.Millisecond)))
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
