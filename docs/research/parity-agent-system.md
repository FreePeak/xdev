> omp feature-parity scout reports, 2026-09-09. Source: omp:// harness docs (agent-hub, task-agent-discovery, collab, hooks, prewalk, advisor-watchdog, slash-command-internals, cli-reference, session-operations, models, providers, approval-mode, secrets, lsp-config, mcp-config, marketplace, memory, skills, theme, tui).
> Companion to docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md and docs/PRD.md feature-parity matrix.

# Parity scout: subagents / hub / collab / hooks / prewalk / advisor / vibe / magic keywords

## 1. Task agents (subagents)

**What**: `task` tool dispatches named agent definitions (markdown + YAML frontmatter) as subprocess sessions with own transcripts/artifacts.

**Definition shape** (task-agent-discovery.md):
- Required: `name`, `description`, `systemPrompt`. Optional: `tools` (CSV/array; `yield` auto-added), `spawns` (`*` or CSV allowlist), `model` (prioritized list; role aliases `@role` expand via `modelRoles`), `thinkingLevel`, `output` (opaque output schema), `blocking: true` (parent waits even when async), `autoloadSkills`, `readSummarize: false` (subagent `read` returns verbatim, no structural summary), `prewalk` (bool or model/role target), `advisor` (bool or model pattern).
- Missing name/description => invalid, file skipped with warning (one bad file never aborts discovery). Bundled defs parse fatal.

**Discovery & precedence** (first-wins by exact case-sensitive name): 1) project `.omp/agents`, 2) user `~/.omp/agent/agents`, 3) OMP extension roots, 4) Claude plugin roots (gated), 5) bundled (`scout`, `reviewer`, `security-reviewer`, `task`, `sonic`). Cross-harness dirs (`.claude/agents` etc.) intentionally skipped.

**Execution-time policy** `resolveEffectiveSubagentPolicy()`: atomic settings reload -> resolve name (omitted defaults to `task`, or first of restricted `spawns`) -> depth guard (`task.maxRecursionDepth` default 2; child at cap loses `task` tool, empty spawn policy) -> blocked-self-recursion via `PI_BLOCKED_AGENT` env -> `task.disabledAgents` -> plan-mode restrictions -> output-schema/model/isolation policy. Missing name fails preflight, no subprocess.

**Model precedence**: `task.agentModelOverrides[name]` > frontmatter `model` list > parent active model. Output schema precedence: task item `outputSchema` > agent `output` > parent session's.

**Plan mode parent**: child gets read-only tools (`read,grep,glob,web_search`), plan subagent prompt, spawns cleared, prewalk cleared.

**Surface**: task tool JSON `{tasks:[{agent, task, context…}]}`; `/agents` per-agent property strips.

**Parity**: **CORE**. Go port: markdown+frontmatter defs, first-wins merge, subprocess with isolated settings override, JSON wire schema, depth/spawn-policy guards. Straightforward stdlib (frontmatter parsing, os/exec or in-process session).

## 2. Agent Hub (TUI)

**What**: TUI roster of session subagents: status (running/idle/parked/aborted), model, activity, cost/tokens/tool-calls; inspector with current tool, context-window use, lineage, artifact paths. Controls: select, open (focus transcript & steer via normal prompt path), revive parked (`r`), kill (`x`), flat/tree toggle (`t`). Discovers persisted subagent JSONLs on resume; advisor transcripts show as read-only, non-messagable rows. `/jobs` prints async-job snapshot; `history://`/`agent://` internal URLs; `hub` tool exposes roster+send to the coding agent.

**Parity**: roster + steering **CORE**; rich inspector + revival polish **NICE**. Go: agent registry, parked JSONL scan, TUI panel.

## 3. Collab (live session sharing)

**What**: `/collab` hosts session over E2E-encrypted relay (AES-256-GCM; link = roomId.key; full 48-byte link = read+write, view-only = key only). Guests render natively in their own TUI, can prompt/interrupt/control subagents (Hub) with full link. Frame types: welcome/snapshot-chunk, entry, event, state (footer), bus, agents, ui-request. Host-authoritative hub topology; guests never peer. Guest prompts badged by display name. Production relay not published; local ws stand-in exists.

**Parity**: **NICE (v2)** — big scope (relay, web client, crypto). Core protocol simple: WebSocket relay + encrypted frames + replica resume; port ws relay + Go guest/host without web client first.

## 4. Hooks

**What**: JS/TS modules with default-export factory `hook(pi)`; loaded via extension runner (`--hook` alias of `--extension`; discovered from `.omp/hooks/...`, configured paths, plugins). `pi.on(event, handler)`, `pi.sendMessage`, `pi.appendEntry`, `pi.registerCommand`, `pi.registerMessageRenderer`, `pi.exec`, `pi.logger`, schema builders.

**Events** (key ones): session lifecycle (`session_start/before_switch/switch`, `before_branch`, `before_compact`/`compacting`/`compact`, `before_tree`, `shutdown`); agent (`context` -> `{messages}` replacement chain, `before_agent_start` -> injected message, `agent_start/end`, `turn_start/end`, `auto_compaction_*`, `auto_retry_*`); tools (`tool_call` pre -> `{block, reason, input}` arg override; `tool_result` post -> `{content, details}` override).

**Semantics**: block/throw = fail-closed; last-wins for input/result overrides; `context` chained; first block short-circuits; handler errors mostly caught + reported (tool_call errors propagate -> block). HookUI: select/confirm/input/editor/notify/setStatus (no-op without TUI). `session_before_*` can cancel; compaction hooks can substitute CompactionResult/context.

**Parity**: **CORE** (lifecycle events + tool_call/tool_result interception). Go port decision: event bus + either embedded JS runtime (goja) or shell-command hooks (event -> exec configured command with JSON stdin). Command registration & UI interactions **NICE**.

## 5. Prewalk

**What**: one-shot model handoff: starting (big) model plans, then after first `edit`/`write` the session switches to a faster/cheaper model. Gate opens on any successful `todo` call when the `todo` tool is active; switch happens after first completed edit/write; one-shot, self-disarms. Target defaults to `@smol` role; unresolved target => warn, start unarmed. No-op switch skipped when target already active.

**Config**: `prewalk.enabled` (yaml), session flags `--prewalk` / `--no-prewalk` / `--prewalk-into <model-or-role>`, `/prewalk` slash command (targets `@smol`). Subagents: agent frontmatter `prewalk` (+ custom target), `task.prewalk` (default off), `task.agentPrewalk` per-agent overrides. Plan mode clears child prewalk.

**Parity**: **CORE** — cheap: armed flag + todo/edit/write watch + model switch.

## 6. Advisor / WATCHDOG

**What**: background reviewer model(s) attached to the session. Each primary transcript delta (rendered with reasoning, tool intent, dedup'd constraint context; advisor's own prior advice filtered out) is fed to the advisor, which has an isolated ToolSession (default tools `read/grep/glob`; roster may grant any built-in incl. mutating, still under approval policies) and an `advise` tool. NOT a retry-critic: it steers, it never approves or mutates primary state.

**advise severities**: `nit` (batched aside at step boundary), `concern` (interrupting steer; late terminal-answer concern => visible card), `blocker` (steers even terminal answers). Delivery constraints: plan mode / deferred-ACP => preserved as cards; `advisor.immuneTurns` (default 3) cooldown routes subsequent interrupts to asides. In-progress review withholds nit/concern (only blocker interrupts partial work); duplicate suppression visible, guard suppression silent.

**Emission guard**: NFKC/normalize dedupe (FIFO 4096), content-free phrase filter ("lgtm" etc.), max 1 note per advisor update; guard state clears on advisor reset (compaction/switch/new).

**Resets**: advisor context rewinds on compaction/session switch/branch/re-prime; mid-session enable seeds cursor (no replay).

**Config**: `advisor.enabled: true`, `modelRoles.advisor`, `--advisor` flag (headless; 10-min final-review drain, 30s error drain), `/advisor on|off|status|dump|configure`. `advisor.syncBacklog` (off/1/3/5): primary waits <=30s for advisor catch-up above threshold. Failure policy: 3 retries then drop backlog; 3 dropped cycles halt; quarantine path for unsafe advisor output (destructive-shell/hazard classifiers) with reset-and-reprime then drop.

**WATCHDOG.md**: advisor-only guidance appended to advisor system prompt (user level + all project levels up to repo root — not nearest-only; `@` imports; `<attention>` wrapper; leaf-most most prominent). **WATCHDOG.yml**: advisor roster — shared `instructions`, `advisors[]`: name, enabled, model (default `modelRoles.advisor`), tools, instructions; slug -> `__advisor.<slug>.jsonl` transcript files in session artifacts (usage attribution, Hub read-only rows). Subagents opt-in per agent via frontmatter `advisor` / `task.agentAdvisor`.

**Parity**: advisor runtime (delta feed, advise tool, severity routing, emission guard) **CORE**; roster/WATCHDOG.yml multi-advisor **NICE**; quarantine classifiers **NICE**; WATCHDOG.md **NICE** (simple file concat).

## 7. Vibe mode

**What**: `/vibe [prompt]` turns main session into director: active tools reduced to `read` + parent-owned `todo` + 5 worker tools; spawns persistent worker subagents (tiers: `fast` -> bundled `sonic` / `@smol`, `good` -> bundled `task` / `@task`) that do all real work. Tools: `vibe_spawn {cli, prompt, name?}`, `vibe_send {session, message}` (steer/queue/start), `vibe_wait {sessions?, timeout?=30s}`, `vibe_kill {session}`, `vibe_list {}`. Results self-deliver via async job manager; full output at `agent://<id>`. Director verifies via `read`.

**Lifecycle**: exit kills all workers + restores toolset; mutually exclusive with plan/goal modes (active AND paused); resume rehydrates completed workers; status-line `Vibe` indicator; worker ids scoped to parent session.

**Parity**: **NICE (v2)** — thin layer atop task subagents + async jobs: 5 tools + toolset restriction + director prompt.

## 8. Magic keywords

**What**: standalone exact-lowercase prose words in user prompt inject hidden user-attributed custom notices for that turn: `ultrathink` (careful reasoning notice + max reasoning effort for the turn), `orchestrate` (multi-agent orchestration contract: scope, parallel delegation, verify each phase), `workflowz` (deterministic multi-subagent workflow via `eval` kernel helpers; only when `eval`+`task` active).

**Matching**: exact lowercase only; standalone prose (punctuation may touch; letters/digits/paths/call syntax don't match); fenced code, inline code, HTML comments ignored; multiple keywords per prompt allowed; per-turn only. Settings: `magicKeywords.enabled` + per-keyword switches; TUI gradient highlighting independent of settings.

**Parity**: **CORE** (trivial: boundary regex + injected hidden custom message; skip TUI animation).

---

## Parity summary

| Feature | Class | Go-port note |
| --- | --- | --- |
| Task agent discovery/spawn | CORE | frontmatter md defs, first-wins merge, subprocess + policy guards |
| Hub tool (agent-facing) + async jobs | CORE | registry + send/wait; TUI Hub NICE |
| Hooks (lifecycle + tool_call/result) | CORE | event bus; embedded JS (goja) or command-style hooks |
| Prewalk | CORE | one-shot todo/edit/write-triggered model switch |
| Advisor runtime | CORE | delta feed + advise tool + severity steering + emission guard |
| Magic keywords | CORE | prompt scan + hidden notice |
| Agent Hub TUI polish | NICE | inspector, revive, persisted scan |
| WATCHDOG.md / WATCHDOG.yml roster | NICE | file discovery + prompt append / roster parse |
| Vibe mode | NICE | 5 tools atop task subagents + async jobs |
| Collab | NICE (v2) | ws relay + AES-GCM + native replica; no web client initially |

[You have received this identical output 4 times. Re-reading 'agent://ScoutAgentSystem?q=.report' will not change it — use a narrower selector (path:A-B), or proceed with the edit.]