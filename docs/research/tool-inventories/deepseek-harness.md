# DeepSeek Harness (`dsh`) — Tool Inventory and Tool-Call Mechanics

Research date: 2026-09-09. Sources: repo at https://github.com/deepseek-ai/deepseek-harness
(verified live via GitHub API) and its docs, fetched from raw.githubusercontent.com on branch `master`.
No URL correction needed: the user-provided URL resolves.

## 1. Identity

- **What it is.** `dsh` ("DeepSeek Harness") is an open-source agent harness from DeepSeek AI,
  tagline "DeepSeek Harness: Everything is a Plugin" (repo `description`, GitHub API;
  https://github.com/deepseek-ai/deepseek-harness/). It is not a model or an API client SDK; it is
  the agent-side runtime (loop, tools, sessions, UI) that a model drives.
- **Relationship to DeepSeek models.** Model adapters register through the provider-neutral
  `ctx.llm` seam (`llm/llm` package, `docs/architecture.md`), so non-DeepSeek providers are
  supported; an `llm-pi-ai` adapter exists (mentioned in
  `docs/deepseek-llm-api-wire-extensions.md`). DeepSeek-specific wire extensions (below) are
  additive telemetry only, explicitly "outside `messages`, system prompts, and tool schemas."
  It is "powered by Cordis" (https://github.com/cordiverse/cordis), a plugin framework whose
  approach is tied to an arXiv paper on "spatiotemporal composability" (`README.md`).
- **Maturity.** Explicitly "developer preview", "iterating rapidly", "THERE WILL BE
  COMPATIBILITY-BREAKING CHANGES" (`README.md`); `SAFETY.md` calls it "experimental
  developer-preview software" that has "not undergone a security audit". No version number in
  README. Repo created 2026-08-13, last pushed 2026-09-08, 16,089 commits, 216.6k stars /
  25.6k forks, license MIT (`THIRD_PARTY_NOTICES.md` for third-party) — GitHub API + repo page.
- **Language / stack.** TypeScript (~35.3 MB vs Python ~0.42 MB, GitHub API `languages`).
  pnpm monorepo; tsdown, vitest, oxlint; Python only for tests (`pytest.ini`).
  Entry: `npx @deepseek-ai/dsh web` → Web UI at `http://127.0.0.1:3080` (`README.md`).
- **Architecture.** "There is no privileged core to patch"; plugins (bundles) compose a profile
  with reversible effects on a shared `ctx`; applications ship as templates `web`, `headless`,
  `sdk`, `sdk-minimal`, `acp`, plus an Electron desktop app (`docs/architecture.md`;
  `packages/boot/app-boot`, `apps/desktop`). Core packages: `core/session` (durable
  `SessionEvent` log), `core/system-prompt` (prompt + tool-schema assembly), `core/tools`
  (scoped registry + guarded execution), `core/agent`, `core/agent-loop` (default driver),
  `core/scope`, `llm/llm`, `webhook/webhook`. Capability "seams" (filesystem, subprocess, LSP,
  terminal, web) are swappable; e.g. moving Bash/PTY/LSP to a sandbox together.
- **External coverage (low confidence).** A web search for announcements surfaced a claimed
  deepseek.com blog post, an HN thread, and a r/LocalLLaMA thread, but all fetches were
  login-walled or garbled, and the search itself returned inconsistent alternate repo names
  (`DeepSeek-Agent-Harness`, `deepseek-ai/harness`, PyPI `dsh`). Those are treated as unreliable;
  everything below is grounded in the canonical repo verified via the GitHub API.

## 2. Built-in tools

Key finding: **`dsh` has no privileged built-in tools.** Every model-facing tool ships as a plugin
package (`packages/*/tool-*`; 55 packages in `packages/`, verified via API) and registers via
`ctx.tools` (`docs/tool-catalog.md`, `docs/architecture.md`). The catalog is generated from live
schemas at boot with a completeness guard that fails the build if any tool package goes
undocumented, so the list below is authoritative. Tool names can be config-driven
(e.g. `tool-subagent`'s `toolName`), so deployments may rename them. 26 tool packages, ~60
model-facing registrations (~57 unique names; Agent Teams reuses three control names).

### Interaction and planning
- `ask_user_question` (`dsh-tool-ask-user`) — asks the human one or more questions, blocks until a
  UI provider answers. `questions[]`: `id`, `question`, optional `header`, `options[]`
  `{label, description?}` (recommended first), `multi_select?`.
- `exit_plan_mode` (`dsh-plan-mode`) — plan-mode only; submits a complete markdown plan for
  approve / keep-planning review. Param: `plan`. Schema stays registered outside plan mode, but
  execution rejects calls made when planning is inactive.
- `run_code` (`dsh-tools`, PTC = programmatic tool calling) — runs a TypeScript async-function body
  (top-level `await`/`return`) that invokes other tools via `await tools.name(args)`; only
  printed/returned values become output. Params: `code`, `description`.

### Code and shell execution
- `bash` (`dsh-tool-bash`) — `bash -c`, fresh shell per call (persist cwd via `workdir`, not `cd`).
  Params: `command`, `description`, `timeoutMs?`, `workdir?`, `run_in_background?` (background →
  job id for `job_output`/`job_kill`; flag hidden if `enableRunInBackground` disabled). Non-zero
  exit reported as `[exit code: N]`; sandbox denials are policy, not bugs.
- `pwsh` (`dsh-tool-pwsh`) — same shape for PowerShell on Windows; native `C:\` paths,
  `$env:NAME`, forced kills settle as exit code 1.
- Persistent `bash` / `pwsh` (`dsh-tool-bash-persistent`, `dsh-tool-pwsh-persistent`) —
  owner-isolated PTY variants; cwd and exported env persist across calls for that agent.
  Single param: `command`.
- `terminal_open|list|read|send|signal|close` (`dsh-tool-terminal`, opt-in, 6 tools) — persistent
  owner-isolated PTY sessions. `send` waits for prompt/silence/timeout unless backgrounded (job id);
  `signal` allows SIGINT/SIGTERM/SIGKILL/SIGTSTP/SIGHUP to the foreground process group (shell-
  targeted SIGKILL rejected). TUI features deliberately absent from the schema.

### Filesystem and search
- `str_replace_editor` (`dsh-tool-str-replace-editor`) — commands `view` (cat -n style, optional
  `view_range`; dirs list 2 levels), `create` (`file_text`), `str_replace` (unique `old_str` →
  `new_str`), `insert` (after `insert_line`); null placeholders count as omitted.
- `edit` / `write` / `read` / `read_image` (`dsh-tool-fs`) — literal `old_string`/`new_string`/
  `replace_all`; full-file create/replace; line-numbered read (`offset`/`limit`); PNG/JPEG/WebP/GIF
  viewing (needs image-input model + `ctx.attachments`). Read-before-write enforcement is a separate
  event-gate plugin, not this package.
- `glob` / `grep` (`dsh-tool-fs-search`) — ripgrep-powered (bundled `@vscode/ripgrep`), always
  foreground. Glob: up to 100 paths in mtime order (over-cap sampled and spilled to storage).
  Grep: first 250 matching lines; optional single `include` glob (no negation).
- `lsp` (`dsh-tool-lsp`) — `goToDefinition`, `findReferences`, `goToImplementation`, `hover` at
  1-based UTF-16 `line`/`character`; provider-swappable; without a provider returns
  `LSP_UNAVAILABLE` instead of deregistering.

### Goals, scheduling, orchestration
- `create_goal` / `get_goal` / `update_goal` (`dsh-tool-goal`) — same-session long-running
  objectives across autonomous rounds; mutations need direct-human authority; `blocked` requires a
  reason plus an admitted-round minimum (default 3); `update_goal` is compare-and-set on
  `goal_id` + `revision`, actions `edit|pause|resume|complete|blocked`.
- `schedule_create` / `schedule_delete` / `schedule_list` (`dsh-schedule`) — session-local
  reminders using exactly one selector: `after_seconds`, absolute `at` (RFC 3339 or
  `{date,time,time_zone}`), or `every_seconds` >= 300; delivery only while the session is live.
- `ralph` (`dsh-tool-ralph`) — foreground fresh-agent loop: each round spawns a contextless child
  agent over a shared workspace ("the workspace is the memory"). Params: `objective`,
  `maxRounds?`; for explicitly requested fresh-agent iteration only.
- `workflow` (`dsh-tool-workflow`) — runs a plain-JavaScript orchestration script (no TS, no
  `export const meta`) with hooks `agent(prompt, opts?)`, `pipeline(items, ...stages)`,
  `parallel(thunks)`, `phase()`, `log()`, plus global `args`. Params: `script`, `meta` (JSON:
  `name`, `description`, optional `whenToUse`, `phases[]`), `args?`. No fs/network/timers; misuse
  kills the script; concurrency and total-agent caps.

### Delegation and coordination
- `subagent` (+ alias `subagent_fork`) and `list_subagent_models` (`dsh-tool-subagent`) —
  delegate a self-contained task (`description`, `prompt`) to a fresh child that never sees the
  parent conversation; returns the result, or a job id with `run_in_background`. Model discovery
  is advisory catalog membership.
- `send_message` / `interrupt_agent` / `list_agents` (`dsh-tool-subagent-control`) — steer
  continuable background children, cancel only the current turn, list children/descendants with
  live vs. resumable status.
- Agent Teams (`dsh-experimental-tool-agent-team`, experimental, 9 tools): adds
  `spawn_teammate` (durable named teammates, `fresh`|`fork` context), `team_task_create|get|list|update`
  (shared task board with CAS transitions `claim|release|edit|set_dependencies|complete|reopen|
  reassign|delete`), `wait_agent`, plus the same `send_message`/`interrupt_agent`/`list_agents`
  names. Disabled in the shipped `dsh-base` bundle; enabled by an "Agent Teams" profile patch that
  disables the legacy control names.
- `skill` (`dsh-tool-skill`) — loads full instructions of a named skill from the session catalog;
  param `name` (progressive disclosure, Anthropic-skill style).

### Jobs, history, web, dynamic plugins
- `job_kill` / `job_list` / `job_output` (`dsh-tool-jobs`) — kind-agnostic background-job control
  (bash, PTY sends, subagents). `job_output` streams incremental output for live jobs or final
  results for settled ones; `wait: true` blocks up to a cap.
- `session_search`, `session_event_search`, `session_event_read`, `session_event_trace`,
  `session_trace` (`dsh-tool-session-query`, read-only, opt-in) — cross-session search (strongest
  match per session), full-text event search with seq/time/type/surface filters, single-event reads
  with neighbor summaries, replacement/relationship tracing, lineage traversal. All authorize from
  the immutable calling agent session.
- `web_fetch` (`url`) decodes an HTTP(S) page to text; `web_search` (`queries[]`, 1-4 queries)
  returns merged results + source URLs (`dsh-tool-web`). Provider choice sits behind `ctx.web` so
  schemas survive backend swaps.
- Cordis dynamic tools (`dsh-tool-cordis`, opt-in, not in any shipped tree): `cordis_define`
  (validate/syntax-check only), `cordis_run` (activate; unauthorized client packages await
  approval), `cordis_stop`, `cordis_undefine`, and read-only `cordis_inspect_list|query|self`.
  A running package may register additional model-visible tools until stopped/removed — i.e. the
  model can extend its own toolset at runtime behind an approval gate.

## 3. Tool-call mechanics

- **Schema format.** Standard HTTP + JSON chat-completions with SSE streaming; tool schemas are
  assembled by `core/system-prompt` (`ctx.systemPrompt`) into each request ("tool-schemas-in-prompt-
  assembly" architecture note). Harness wire extensions are reserved snake-case `dsh_`-prefixed
  sibling body fields (`dsh_plugin_packages`, `dsh_session_log`, each independently versioned) and
  lowercase kebab headers (`x-deepseek-harness-user-id|session-id|compact`); they never enter model
  input. Provider-neutral `llm/llm` and `llm-pi-ai` do not implement the extensions
  (`docs/deepseek-llm-api-wire-extensions.md`).
- **Agent loop** (`docs/agent-lifecycle.md`, `docs/architecture.md`). A *turn* = zero or more
  *steps*; a *step* = one model request plus the tools it calls. Step sequence: assemble system
  prompt → `agent/pre-step` hook waterfall → admit system/user messages → "derive and freeze
  request from the log" → LLM call (`agent/request`, `llm/stream`) → tool execution → `step/end`;
  then `agent/turn-stopping` (serial terminal checkpoint) → `turn/end` when the next-step inbox is
  empty. The session log is the context source of truth: "Model-visible means logged" — every
  model-visible input must be a durable `SessionEvent`. Compaction (`dsh-compaction-basic`) fires
  on pre-step pressure or canonical context-overflow errors; optional tool-result pruning precedes
  summary selection; retries reuse the same frozen rendered assembly.
- **Streaming.** Live: `agent/assistant-stream` frames (start/chunk/end) carry incrementality to
  the UI but are transient. Durable: successful calls settle into `assistant/message` embedding "the
  exact compact timed stream"; failed/retried/cancelled attempts settle as `assistant/attempt`.
  At the wire, the adapter runs an `accept()` transaction before reading the SSE body.
- **Execution pipeline** (`docs/tool-execution-pipeline.md`). Model tool-call block → logged as
  `tool/call` before execution → UI pending card (`presentCall(args)`) → `tools/pre-execute`
  waterfall (hooks, permission, sandbox; allow/deny/ask) → monotonic guards (deny-or-abstain,
  order-fixed, identity-protected; a deny skips the tool body) → `tools/execute` (timeout, retry,
  metrics wrappers around `execute()`) → `tools/post-execute` (may accept/block/replace/add
  context). Inside bodies, filesystem mutations pass `fs/write-intent` / `fs/edit-intent` gates.
  Any throw normalizes to an `isError` result — the loop never crashes on tool failure. Calls run
  in batches: classified by `executionMode`, "barriers and bounded rolling pool"; ordered post-hooks
  fire results in model order; exactly one `tool/result` event per call; after the batch settles,
  `additionalContexts` are injected FIFO as a user message after the recorded results (call/result
  adjacency preserved). PTC (`run_code`) serializes its sub-calls through the same pipeline with a
  parent token (`tool/ptc-dispatch` events).
- **Permission / approval model.** Pre-execute hosts the permission/sandbox stage; approval is
  `ctx.approval` — a one-shot prompt where *absence or unanswerability means deny*; "allowed-once"
  proceeds to guards; reject/cancel denies. Hooks span tool families "without coupling the tools to
  one policy service." Read-before-edit and similar cross-tool policies are separate event-gate
  plugins (see `docs/capability-seams.md`). Caveat: `SAFETY.md` specifies **no concrete default
  approval behavior** for shell/edit; it warns sandbox+prompts "do not guarantee isolation,"
  recommends disposable VM/container and minimal privileges. (Profile/bundle config determines the
  shipped defaults per app; the docs reviewed here do not state dsh-base's approval default.)
- **Vs. coding CLIs (comparison = analysis).** Against Claude Code: same core tool families with
  renamed surfaces — `bash`+`job_*` ≈ Bash/background bash, `read|write|edit` ≈ Read|Write|Edit
  (identical literal-string contract), `glob|grep` ≈ Glob|Grep (same ripgrep basis), `todo_write` ≈
  TodoWrite, `subagent` ≈ Task, `ask_user_question` ≈ AskUserQuestion, `exit_plan_mode` ≈
  plan-mode ExitPlanMode, `skill` ≈ progressive-disclosure skills, `web_fetch|web_search` ≈
  WebFetch|WebSearch. dsh adds PTY `terminal_*`, PTC `run_code`, self-extending `cordis_*`,
  `lsp`, `ralph`, `workflow`, goals/schedule; Claude Code adds first-party MCP tool hosting (dsh's
  analogue is plugins + `ctx.tools` directly). Permission posture differs: Claude Code documents
  explicit allow/ask deny-by-default rules; dsh leaves defaults to profiles and flags itself
  unaudited. Against OpenCode: same TypeScript + client/server + plugin direction, but dsh pushes
  "no privileged core" further (even the loop and model adapters are plugins) and is Web-UI-first
  with headless/SDK/ACP templates. Against pi: dsh explicitly bridges to pi's ecosystem via an
  `llm-pi-ai` provider adapter, while keeping pi's minimalism out — pi ships a tiny fixed toolset
  for one terminal TUI, dsh ships ~57 renameable plugin tools across Web/desktop/headless apps.
  Common ground across all four: JSON-schema tool definitions in a chat-completions-style request,
  SSE token streaming, batch tool execution with results appended as tool messages, and
  human-approval gates around destructive tools.

## 4. Todo / task-tracking contract (`dsh-tool-todo`)

- Single tool: `todo_write`. **Full replacement semantics** — each call overwrites the entire
  list; no partial/patch updates.
- Item shape: `{content, status}`; statuses exactly `pending` | `in_progress` | `completed`.
- Behavioral contract (from schema description): mark tasks completed immediately rather than
  batching completions, and keep at least one `in_progress` while work remains.
- Config variant: `allowParallelInProgress: true` (shown in the generated catalog) permits multiple
  concurrent `in_progress` items; `false` yields a schema variant demanding exactly one active task.
- Execution emits a tool-owned `todo/write` event (pipeline doc), so writes are durable session
  facts. Adjacent-but-separate state systems: goals (CAS `update_goal`), Agent-Teams shared task
  board (CAS claim/complete transitions), and schedules — `todo_write` itself is per-session.

## 5. Source index

- https://github.com/deepseek-ai/deepseek-harness (repo page; description, stats, topics
  `ai-agents|cordis|dsh|dsh-plugin`)
- GitHub API: repo metadata + `/languages` + `git/trees` + `contents/packages` (55 pkgs)
- `README.md` (what/quick-start/maturity) · `SAFETY.md` (no-audit warning, no stated defaults)
- `docs/tool-catalog.md` (generated; every tool above) · `docs/tool-execution-pipeline.md`
- `docs/agent-lifecycle.md` · `docs/architecture.md` · `docs/deepseek-llm-api-wire-extensions.md`
- `docs/capability-seams.md`, `docs/config-catalog.md` (not read in full) · user docs: `docs/user/`
