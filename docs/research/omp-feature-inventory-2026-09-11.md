# omp current feature inventory — full-parity sweep (2026-09-11)

Primary sources: all 131 `omp://` harness docs, read live on 2026-09-11. This doc is the
authoritative current-state feature list behind the PRD §2 feature-parity matrix refresh.
Rows marked **NEW** were absent from the 2026-09-09 parity research docs.

## 1. Session core & compaction

- JSONL tree store: 256-byte title slot, append-only entries + mutable leaf, blob store,
  listing via 4 KiB prefix (+32 KiB tail), lifecycle status (`complete|interrupted|aborted|error|pending`), orphaned-`.bak` repair. [xdev M2/M8: shipped]
- **Compaction triggers (6)**: manual `/compact`, overflow recovery, incomplete-output (`stopReason=length`) recovery, threshold maintenance, **mid-turn** maintenance, **idle** compaction (`compaction.idleEnabled`, default off). [NEW mid-turn/idle vs M5]
- **Compaction method ladder**: `remote` (provider-native streaming v2, `remoteStreamingV2Enabled` default on, `v2RetainedMessageBudget=64000`), `snapcompact` (bitmap PNG imaging of discarded history for vision models), `handoff`, `shake` (mechanical content elision), `soft` (pruning: `supersedeReads`, `dropUseless`), `methodOrder` default `[remote, snapcompact, handoff, shake, soft]`. **NEW**: snapcompact/shake/remote/soft methods + streaming v2.
- **`/handoff` + handoff method** [NEW]: cache-aligned side-request doc generation (`toolChoice:"none"` with auto retry), commits as a normal `CompactionEntry` on the current session, `handoffSaveToDisk` artifacts, async speculative pre-threshold generation (`compaction.asyncEnabled`).
- **Branch summaries**: `branch_summary` entries on abandoned branches, `branchSummary.enabled/reserveTokens` settings.
- **context_notes / new_context tools** [NEW]: experimental notes-backed context windows (`compaction.experimentalContextManagement`, 16,384-byte notebook, `experimental_context_notes` entries, rollover keeps notebook + recent tail, raw history reachable via `history://current/full`).
- **Experimental context reconstruction**: notebook + recent messages; reset hides earlier notebook revisions.

## 2. Providers, models, auth

- 4+ wire transports (anthropic-messages, openai-completions, openai-responses, google-generative-ai) + v2 set: azure-openai-responses, openai-codex-responses, google-vertex, gemini-cli, CCA/Antigravity, ollama/ollama-cloud.
- **Tool-call conversion matrix** (toolconv): per-model formats — anthropic, deepseek, gemini, gemma, glm-4.5, harmony (GPT-5 + ERRATA), hermes, kimi-k2, minimax, pi-native, qwen3, xml.
- **Unified schema normalizer** [NEW detail]: `normalizeSchemaFor{Google,CCA,MCP,Moonshot,Ollama,Grammar,OpenAIResponses}`, OpenAI strict-mode sanitize+enforce pipeline (`PI_NO_STRICT` bypass), `toolSchemaFlavor` per host.
- Model roles (9 built-in roles incl. `tiny`, `task`, `advisor`), `@role` aliases, `:effort` suffixes, `modelRoleStorage: global|project`, `modelTags`, `cycleOrder`, path-scoped `enabledModels/providers` arrays.
- Credential chain: runtime override → models.yml → OAuth (Claude Pro/Max, Codex PKCE, quota rotation) → `/login` → env + `.env`; auth-broker/gateway ecosystem; **install-id** [NEW] per-install UUID (`~/.omp/install-id`, 0600, O_EXCL) for Codex `installationId` / Claude `device_id`.
- Retry/fallback [NEW depth]: `retry.fallbackChains` per-role, per-model (`provider/model` keys), `provider/*` wildcard entries; usage-aware fallback with `confirm|auto` reserve threshold; `fallbackRevertPolicy: cooldown-expiry`; credential rotation on usage limits; stale-replay provider-session reset; `retryRecovery` markers (`recovered`/`superseded`) on persisted error entries; jittered capped backoff (500 ms→8 s, max 5 min).

## 3. Rules, TTSR, context files

- **Rulebook matching pipeline** [NEW]: canonical `Rule` shape (`globs/alwaysApply/description/condition/astCondition/scope/agents/interruptMode`); discovery providers native `.omp/rules/` (priority 100), omp-plugins (90), agents `.agent(s)/rules/` (70), cursor (50), windsurf (50), cline (40), github (30), builtin-defaults (1); name-based first-wins dedupe; sticky `RULES.md` (forced alwaysApply); `rule://<name>` read protocol.
- **TTSR (Time-Traveling Stream Rules)** [NEW]: streaming regex (`condition`) + AST (`astCondition`, via edit-tool per-file matcher digests) monitoring of text/thinking/toolcall deltas; per-rule `interruptMode always|prose-only|tool-only|never`; immediate abort → 50 ms → retry with `<system-interrupt>` injection (contextMode discard|keep); non-interrupting matches fold `<system-reminder>` into tool results; repeat policy `once|after-gap` (`repeatGap` turns); persistence via `ttsr_injection` entries + restore on resume; settings group `ttsr.{enabled,contextMode,interruptMode,repeatMode,repeatGap,builtinRules,disabledRules}`; `omp ttsr` CLI; `ttsr_triggered` extension/hook event.
- Context files: AGENTS.md hierarchy + `@path` imports; third-party sources (cursor/windsurf/cline/github/agents-md/gemini) gated by `enabledProviders`; `--no-rules`; prompt-injection scanning (hermes pattern).

## 4. Agent system

- Task agents: markdown+frontmatter discovery, first-wins merge, spawn policy (allowlist, `task.maxRecursionDepth`, plan-mode read-only children), model precedence, `task.agentModelOverrides.{sonic,task}`, `omp agents` CLI.
- Hub tool + Agent Hub TUI roster (status/model/activity/cost, steer, kill, **revive**, transcript fetch, inspector).
- Hooks: lifecycle events, fail-closed `tool_call`/`tool_result` interception, CC exit-code contract (exit 2 = block), embedded-JS hooks.
- Advisor/watchdog: per-turn second-model review, nit→aside / concern→interrupt / blocker→steer, WATCHDOG.md guidance + WATCHDOG.yml roster, `advisor.syncBacklog` (off|1|3|5, 30 s cap), `advisor.immuneTurns` (3), `task.agentAdvisor` per-subagent.
- Prewalk: big→`@smol` handoff at first edit/write after plan todo exists; `--prewalk`, `--prewalk-into`, `/prewalk` toggle.
- Plan mode: read-only + `xd://propose` approval gate (PlanApprovalDetails UI / ACP elicitation / planYolo auto-approve), `--plan-yolo`, `--plan-yolo-into`.
- Magic keywords: `ultrathink`/`orchestrate`/`workflowz` + think-budget ladder (`thinkingBudgets`, `providers.autoThinkingMaxEffort`).
- **Goal mode** [NEW]: `goal` tool (create/get/resume/complete/drop, optional token_budget), `goal_updated` session event, goal persistence across turns.
- Steering: steer/followUp/**aside** (next step boundary, no tool-batch kill), queued messages preserved; `deliverAs` on all prompt APIs.
- Subagents: outputSchema (permissive/strict), `yield` tool, `requireYieldTool`, `taskDepth`, `parentTaskPrefix`, `isTerminal` on `agent_end`; events `irc_message`, `notice`, `todo_reminder`, `todo_auto_clear`.
- **Vibe mode** [NEW]: director session reduced to `read`+todo+5 tools (`vibe_spawn/send/wait/kill/list`), tiers `fast`(sonic/@smol)|`good`(task/@task), persistent blank-start workers with own transcripts, mutual exclusion with plan/goal, persistence + rehydration as idle/parked.

## 5. Session UX / TUI

- Lifecycle: `/new /fresh /clear /drop /fork`; `/handoff` [NEW]; `/compact [instructions]`.
- Resume: terminal breadcrumb (TTY + ZELLIJ/TMUX/KITTY/WEZTERM/WT ids, fresh-target semantics), `--continue`, `--resume [id]` (case-insensitive prefix match, re-root prompt on missing cwd), fullscreen picker (Tab all-projects scope lazy-load, multi-token search + `history.db` prompt matches, mouse, delete w/ confirm, pinned markers), `/resume @claude|@codex` **foreign-source import pickers** [NEW], `--from-claude`/`--from-codex` [NEW], `switchSession` runtime (snapshot rollback, provider-session close, model/tier/thinking restore, synthetic abort on interrupted tools).
- `/tree` navigator: entry tree w/ labels (`Shift+L`), filters (default/no-tools/user-only/labeled-only/all), search, summarize-and-switch (`branch_summary`), double-escape fullscreen rewind selector.
- Slash commands: markdown discovery project+user, `$1..$n/$@/$ARGUMENTS`, unknown fall-through; ACP/RPC differences.
- `/dump`, `/export` HTML, `/share` E2E-encrypted (`share.serverUrl`, `share.redactSecrets`), `/copy`, `/open`.
- Keybindings remap (`keybindings.yml` + `/hotkeys`), `app.session.tree` action binding.
- System prompts: SYSTEM.md/APPEND_SYSTEM.md, TITLE_SYSTEM.md, PERSONALITY.md presets (`default|friendly|pragmatic|none`), `--system-prompt`/`--append-system-prompt`.
- TUI: frame-plan renderer, history retirement, DSR resize recovery, alt-buffer overlays, scrollback, Grok-CLI target (xdev-specific), theme engine (66 tokens + vars + symbol presets + live reload), status-line/HUD segments, spinner frames, custom tool renderers (`renderCall`/`renderResult`), ask picker, mouse drag-selection, `Ctrl+O` expansion, kitty images, `startup.showSplash`/`startup.quiet`.
- **Session titles**: `/rename`, ai-title generation cascade, picker metadata.

## 6. Modes, CLI, config

- Modes: `text`, `json` event stream, `rpc`, `rpc-ui`, **`acp` (Agent Client Protocol server)** [NEW].
- Launch flags (full set): `--cwd --add-dir --allow-home --profile --alias --config --session-dir --no-session --continue --resume --fork --from-claude --from-codex --export --no-title --model --smol --slow --plan --models --provider --api-key --provider-session-id --prompt-cache-key --service-tier --thinking --hide-thinking --print-thoughts --external-thinking --prewalk --no-prewalk --prewalk-into --plan-yolo --plan-yolo-into --tools --no-tools --no-lsp --no-pty --approval-mode --auto-approve/--yolo --advisor --max-time --extension --hook --trusted-extension --plugin-dir --no-extensions --skills --no-skills --no-rules --system-prompt --append-system-prompt --mode --print/-p`.
- **Subcommand suite** [NEW surface]: `launch acp auth-broker auth-gateway agents bench browser-relay cleanse commit completions compress config dry-balance gc grep gallery git grievances if-bench images install join models plugin ps say setup shell read render share ssh stats update usage tiny-models token ttsr worktree search`.
- Settings layering: defaults ← global ← project ← `--config` overlays (repeatable, `PI_CONFIG_FILES`) ← runtime flags; deep-merge (arrays replace); `omp config list|get|set|reset|path|init-xdg` with schema-typed value parsing, masked credentials, legacy `settings.json` migration, `.broken-*` quarantine.
- Approval: modes always-ask|write|yolo; per-tool `tools.approval` record; `bash.patterns` ordered allow/prompt/deny; **`bash.allowCompoundCommands`** conservative `&&`-chain evaluation [NEW detail]; **`bashInterceptor`** [NEW] — redirect bash commands matching regex to dedicated tools with model-facing message.
- Env framework: process env → project `.env` → agent `.env`; env↔setting override table (PI_SMOL/SLOW/PLAN_MODEL, PI_NO_PTY, PI_PY/PI_JS, PI_TINY_*, OMP_AUTH_BROKER_*, PI_CODING_AGENT_DIR, PI_CONFIG_FILES).
- **Multi-provider discovery toggles** [NEW detail]: `enabledProviders` (default empty — foreign roots off until listed; `*`/`all`), `disabledProviders` dual namespace (model providers AND discovery sources), path-scoped arrays.
- Sampling: temperature/topP/topK/minP/penalties, `textVerbosity`, `tier.{openai,anthropic,google,subagent,advisor}`, `includeModelInPrompt`.

## 7. Memory & knowledge

- 5 memory backends: `off`, `local` (two-phase extraction→consolidation → MEMORY.md + memory_summary.md + skills/, lease+heartbeat, secret redaction, `memories.*` 15 knobs), `hindsight` [NEW — remote HTTP backend, project-tagged banks, autoRecall/autoRetain], `mnemopi` [NEW detail — local SQLite, FTS/vector/graph/fact polyphonic recall, scoping global|per-project|per-project-tagged, embeddings/LLM config, drain-on-exit 1.5 s], `sharpshooter` (friction-gated decision files).
- Tools per backend: `recall retain reflect memory_edit` (mnemopi; hindsight minus memory_edit); `memory://` read seam (root, MEMORY.md, learned.md, skills/, `<memory-id>` full-row).
- `/memory` subcommands [NEW depth]: view|stats|diagnose|queue|sync|clear|enqueue|mm (Hindsight mental-model maintenance).
- `learn` tool + `autolearn.enabled` (learned.md, 100-entry cap, redaction); managed skills.
- Skills: SKILL.md discovery (native + custom + managed first-wins), `skill://` protocol, `/skill:` commands, marketplace-compatible plugin skill discovery.

## 8. Tools

Core: read, write, edit (hashline/replace/patch/apply_patch/sloppy), bash (PTY eligibility, env hardening, OutputSink head+tail, backgrounding, `bashInterceptor` routing). Extended: grep, glob, todo (9-op), task, hub, lsp, eval (py/js persistent cells), notebook, web_search (22-provider chain), github, ast_grep, ast_edit (staged previews → `xd://resolve|reject`), browser (CDP + browser-relay), checkpoint/rewind, ask, debug (DAP), memory tools, learn, manage_skill, generate_image, security_scan, tts, computer, **context_notes, new_context** [NEW].
- Deferred tool catalog: `tool_search`/`tool_describe`/`tool_call` progressive disclosure bridge.
- Resolve devices: `xd://resolve|reject|propose` via write; preview queue + plan proposals.
- Custom tools: subprocess/TS-module, strict schemas via MCP normalizer.
- MCP client: official SDK, stdio/HTTP, config extensions (imports from claude/codex/gemini/cursor, `!command` secrets, per-server timeout, list_changed, filtering).
- **Gemini manifest extensions** [NEW]: `gemini-extension.json` discovery (user+project `.gemini/extensions`, mcpServers/tools/context metadata, priority 60 under native 100).

## 9. Extensions, plugins, marketplace

- Extensions: in-process TS modules (omp) → xdev subprocess JSONL protocol; capability handshake; fail-closed policy; per-event timeout + SIGKILL; runtime actions (steer/followUp/aside, register provider); `ttsr_triggered` + session events.
- **Plugin manager** [NEW]: `omp plugin install|uninstall|list|link|doctor|features|config|enable|disable`; npm/git (github:/gitlab:/bitbucket:/codeberg:/sourcehut:)/link specs, feature brackets `pkg[a,b]`, lockfile `omp-plugins.lock.json`, project overrides, rollback on failed validation, shell-metachar denylist.
- **Marketplace** [NEW]: Claude-compatible catalogs (`.omp-plugin/marketplace.json` + `.claude-plugin/` fallback), sources GitHub/URL/git/local, user|project scopes, `installed_plugins.json` v2, enable/disable/upgrade, `marketplace.autoUpdate off|notify|auto`, `omp-plugins` + `claude-plugins` capability discovery (skills/commands/hooks/tools/rules/prompts/MCP/agents).

## 10. Collab & ecosystem

- **Collab** [NEW depth]: `/collab [relay|view|status|stop]`, `/join`, `omp join`; link format `<roomId>.<key>` (48-byte full = AES-256-GCM key + write token; 32-byte view-only); E2E AES-256-GCM over WS relay; frame types welcome/snapshot-chunk/entry/event/state/bus/agents/ui-request; guests prompt/interrupt/answer select+editor; web client (collab-web) + `/share` blob endpoints; replica session under `~/.omp/collab/`.
- **Subagent/event bus**: daemon-supervised background processes (`omp ps` logs/stop/kill/restart).
- Ecosystem packages (SKIP-class for xdev): auth-broker/gateway, omp-stats, omptype, metaharness (+edit benchmark), robomp, browser-relay, collab-web, snapcompact, mnemopi CLI, tiny-models (local ONNX/MLX).
- **Distribution** [NEW]: macos signing/notarization, `omp update` canary/stable channels, shell completions (bash/zsh/fish), storage `gc`, `omp bench` (TTFT/decode p50/p95 dashboard), `omp setup` onboarding.

## 11. xdev mapping status (shipped vs remaining)

Shipped (per PRD §4 + commits): M0–M4 complete; M5 core (threshold/overflow/promotion ladder, retry/failover base, partial-stream retain) complete b4f9d80; M6 complete (RPC, subagents, MCP); M7 partial (FS-scan cache, grep/glob, ast shell-out, ext protocol consumers); M8 gate complete (windowed materialization 62 MB proof, fuzz, govulncheck, 6-platform artifacts); M9 complete (#10 closed); M10 mostly (slash commands, breadcrumb/--continue, --resume prefix, /fork, /dump, switchSession, /fresh, /prewalk toggle, context imports, thinking replay); M12 slices (skills, memory local backend, theme engine); M11 slices (task-agent discovery, spawn policy, magic keywords).

Remaining per feature (→ GitHub issues): compaction method-ladder tails (snapcompact/shake/remote/soft + handoff + idle/async/mid-turn + branchSummary), retry fallbackChains depth, session picker + /tree + foreign import, keybindings, full context-file/rules hierarchy + rule://, TTSR, plan mode + plan-yolo, hub tool + Agent Hub TUI, hooks, advisor runtime, goal mode, vibe mode, collab, ACP mode, CLI subcommand suite + launch-flag parity, context_notes/new_context, Hindsight/mnemopi backends + /memory subcommand depth, memory:// full seam, /export + /share, eval kernel, notebook, web_search, github tool, browser, checkpoint/rewind, LSP, marketplace + plugin manager, deferred tool catalog, secrets redaction, kitty images, custom renderers, transports v2 + toolconv + schema normalizers, install-id + XDG + profiles, macos signing + update channels + completions, bashInterceptor, bash.allowCompoundCommands, gemini manifest extensions, prompt-injection scanning, status-line/HUD chrome, ask picker, goal tool.
