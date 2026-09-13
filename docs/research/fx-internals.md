# fx Internals — Vercel Labs' Zig Coding Agent (competitor teardown)

Research date: 2026-09-13. Sources: shallow clone (`--depth 50`) of
`github.com/vercel-labs/fx` at commit **`ced6180`** (2026-09-12, version
`0.0.9` per `src/main.zig:6`), 587 `.zig` files / **671,361 lines** under
`src/`, plus the published docs corpus (`fx.sh/llms-full.txt`, 37 sections /
5,181 lines), `AGENTS.md` (470), `CONTRIBUTING.md` (530), `CHANGELOG.md` (442),
`sdk/`, `examples/`, `.github/workflows/`, `benchmarks/`, `scripts/`. Every
claim carries a `path:line` into that clone; §12 lists the claims the code does
**not** support. At research time: 2,905 stars, 217 open issues, Apache-2.0,
Zig 0.16+.

Why it matters: fx is the closest live peer to xdev's thesis — tiny native
single-binary coding agent, no JS runtime, minimal tool slate, inspectable
sessions — and the only reference agent (pi, omp, Claude Code, fx) that ships a
native binary **and publishes measurable budgets**. It is the sharpest external
check on xdev goals 1–2 and on the §5 decisions.

---

## 1. Identity and inventory

- Self-description (`README.md:14`): "a coding agent CLI written in Zig: a
  6.17 MiB native binary that is open source (Apache-2.0), model-agnostic, and
  embeddable as a harness in larger systems. Its interface stays closer to a
  Unix shell than an IDE in the terminal." Banner: "Tiny, open, embeddable,
  native coding agent … ⚠ Status: Experimental".
- Install `curl -fsSL https://fx.sh/setup.sh | bash` → `~/.local/bin/fx`;
  build `zig build -Doptimize=ReleaseSafe`. No Node runtime, no root
  `package.json` (`AGENTS.md:33`); zero third-party Zig deps
  (`build.zig.zon .dependencies = .{}`) — HTTP, TLS, JSON, SSE, unicode tables
  and a VT emulator are all stdlib or hand-rolled (unicode alone: 8,945 lines of
  generated range/trie data, `unicode_display_data.zig:8945`).
- **21 top-level commands** (`src/builtins/commands.zig`): `help ask acp pr
  issue login logout setup status permissions mcp models provider teams credits
  balance sessions session background usage upgrade doctor workspace version`.
  `--json` on 12 of them, each a typed `Snapshot` rendered text-or-JSON from one
  source (`src/core/output/output_contracts.zig:27-30`), discriminated by a
  `kind` field (`{"kind":"status",…}` :1919; `sessions` carries
  `has_more`/`next_cursor:"v1:2:abc"` :2274). JSON errors go to **stdout**,
  text errors to stderr.
- **~40 slash commands** in 11 help categories
  (`src/core/slash_commands/command_specs.zig:74-96`).
- **17 tools registered, 15 advertised** (`src/builtins/tools.zig:834-866`):
  `read_file glob_files grep_files edit_file write_file shell subagent
  capability_search skill install_skill mcp_select_tool mcp_features
  ask_user_question web_fetch web_search`, plus gated `vision` and
  `read_tool_result`. Read-only subset = 3 names (:868).
- Deliberately **absent** from fx (each searched): OS sandbox, LSP,
  tree-sitter/AST, browser/CDP tools, a todo/task-list tool, a memory tool
  (removed in 0.0.8, `CHANGELOG.md:54`), an eval kernel, a plugin/marketplace
  format, session branching, cost ceilings, telemetry, an HTTP serve mode,
  regex in `grep_files`, `.gitignore` semantics, custom user slash commands,
  third-party hook processes.

## 2. Size, startup, and memory engineering

- **The only absolute product budget is 2 ms, Linux-only**:
  `benchmarks/check_budgets.py:10-18` pins 0.002 s mean for bare startup plus
  `fx help`, `fx status --json`, `fx background --json`, `fx doctor --json`,
  `fx sessions --json`; `DEFAULT_LINUX_BUDGET = 0.002` (:27) makes *any* new
  command inherit it. Non-Linux is INFO-and-pass. Measured by hyperfine
  `--runs 100 --warmup 10` against a seeded worst-case store — **8 sessions ×
  256 MiB sparse `events.jsonl`** (`session_list_fixture.py:126-127`) — with a
  `/usr/bin/true` baseline probe whitelisted to INFO and never subtracted.
- Enforced TUI numbers (`tests/e2e/tui-performance.test.ts:34-39, 968-1054`):
  frame p50/p90/p95 = **8/12/17 ms**, threads ≤ 2, descriptors ≤ 6 (7 during an
  active turn), RSS **< 16 MiB after / < 32 MiB peak**.
- **fx has no RSS or boot-memory budget at all.** No
  `setrlimit(RLIMIT_AS|RLIMIT_RSS)` (only `RLIMIT_NOFILE` raising,
  `src/core/terminal/host.zig:412-413`); memory is bounded per structure
  (transcript 256 KiB, read 256 KiB, command output 64 KiB, tool result 64 KiB —
  `src/main.zig:182-190`), never per process. xdev's hard <100 MB gate + CI
  memory proof is the stronger contract.
- Binary size: no absolute PR gate. `binary-size.yml` builds base *and* head
  (via `git worktree`) ReleaseSafe on 4 native targets and runs
  `scripts/binary_size.py --warning-bytes 52429` — a **delta warning only**,
  both binaries retained. The one hard ceiling is release-time and macOS-arm64
  only: `size_gate(ceiling_mib=7.800, preferred_headroom_mib=0.250)`
  (`scripts/pgso/model.py:79-99`) → 8,178,892 bytes inclusive, headroom also
  enforced (`distributed.py:866-870`).
- Shipped flag chain (`build.zig:50-66`): `ReleaseSafe` + `link_libc`,
  `stack_check=false`, `stack_protector=false`, `omit_frame_pointer=true`,
  `unwind_tables=.none`, `error_tracing=false`, `strip` unless Debug. Bounds are
  kept and size bought back with profile data instead of `ReleaseFast`; PGSO
  rejects any other optimize mode (`pipeline.py:89-90`).
- **PGSO** = a re-compilation pipeline, not a flag: Zig emits bitcode
  (`-Dpgso-artifact -Dpgso-ir`), pinned LLVM 21.1.8 `opt`/`llc`/`clang` re-do
  codegen with `-pgo-cold-func-opt=minsize`,
  `-profile-summary-cutoff-cold=600000`, `mergefunc`+`iroutliner`,
  `-machine-outliner-reruns=1`, `dead_strip`, `-s` (`pipeline.py:32-45,389`).
  The eligible candidate *is* the released binary: `release.yml:150-156` calls
  the PGSO workflow, and `sign-macos-arm64` refuses to sign unless
  `python3 -m scripts.pgso report` shows `status:passed / stage:complete /
  eligible:true`. Perf rides along: `MAXIMUM_REGRESSION = 0.10` on **both** p50
  and p95, ≥1,000 startup samples, ≥50 heavy samples, ≥10 alternating rounds on
  fresh machines (`qualify.py:37-43, 387-390`).
- **The corpus driving it is a mandatory test inventory**
  (`scripts/pgso/corpus.json`): every root E2E file is *training* (31 files + 5
  direct commands), *verification-only* (18), or *excluded with a written
  reason* (13); `corpus.py:328-363` hard-fails on missing/duplicate/stale/
  unclassified — checked cheaply on every PR while the expensive run is
  release-only.
- Allocators: `c_allocator` (under `link_libc`) or `page_allocator`
  (`src/main.zig:3525-3528`), arenas per request/turn, one `FX_BENCH`
  parse-and-exit fast path before terminal init
  (`app_entry_runtime.zig:189-213`) and `exitFast` via `std.c._exit`
  (`main.zig:3558`) — that hook is what makes a 2 ms budget measurable.

## 3. Session core: a linear log, not a tree

- `~/.fx/sessions/<id>/`: exactly one append-only artifact (`events.jsonl`) plus
  atomically-replaced sidecars — `session.json` (manifest ≤64 KiB, schema 4),
  `display.json` (title/preview ≤16 KiB), `usage-v2.json` (≤256 KiB),
  `permissions.json`, `recovery.json`, `background/`, `subagent/`, `logs/`.
- IDs: 9 random bytes → 12-char base64url (`session_layout.zig:5-34`),
  validated `[A-Za-z0-9._-]` ≤255 with `.`/`..` rejected (:17-26). No timestamp
  prefix, so listing must sort. Shell sessions `shell-` + 16 bytes (:8,36-46).
- **No branching of any kind**: searching `src/core/session` for
  `leaf|fork|branch|rewind|ancestry` returns only subagent ownership
  (`session_store.zig:1005-1141`, `parent_id`) and a test string. The frame
  envelope is `{schema_version, seq, timestamp_ms, event}` with exactly **8**
  conversation tags (`session_event.zig:158-179`) and a transition validator.
  fx therefore cannot express `/tree`, `/branch`, rewind or a leaf re-point —
  the central design split with xdev.
- Durability (`session_log.zig:443-517`): a whole turn batch is encoded once,
  `file.length() == committed_bytes` is asserted (mismatch = permanent
  `SessionWriterChanged`, i.e. foreign-writer detection), bytes go in with
  `writePositionalAll`, then `file.sync()`; failure does a `setLength` rollback;
  an unfinished trailing turn is discarded at open (:144-200). Dirs 0700 / files
  0600, lock deadline 2 s (:23-24, 518).
- Listing is the weak spot: `classifyConversationCandidate` opens `session.json`
  **and line-parses the entire `events.jsonl`** to count turns and detect a
  checkpoint (`session_discovery.zig:309-390`) — O(total bytes) per session —
  and the log is never compacted, rotated or GCed (only the 35-day usage ledger
  has retention). The picker alone is indexed: `.resume-catalog`, magic
  `fx-resume-catalog-v4\n` + sha256 + rows, ≤100k/64 MiB, keyed by a stat
  fingerprint with before/after re-stat race rejection
  (`session_catalog_cache.zig:13-16, 336-368`). xdev's 4 KiB prefix reads +
  stat cache are the better design.
- **Compaction is an appended checkpoint event, never a rewrite**:
  `context_checkpoint{covers_through_seq, summary}` (`session_event.zig:158-161`,
  appended at `session_log.zig:303-321`). The boundary is a monotone watermark
  into history, so lost in-memory state is recoverable by re-scan and
  compaction can never destroy the transcript.
- Titles, three layers: manifest `title` ≤240 B; `display.json` title ≤240 B /
  8 words + preview ≤240 B / 2 lines; a background LLM generator capped at
  **≤60 B, 2,048 B excerpt, 15 s, 128 output tokens**, sanitizing *before*
  stripping (`session_title_generation.zig:16-20`).
- Cost: token totals ride every `history_turn_committed` frame
  (`session_event.zig:731-736`) into the manifest for cheap listing;
  authoritative billing is a ≤256 KiB `session_usage.Snapshot` with per-model
  breakdown and a pending/fail/complete reservation protocol; 35-day profile
  ledger at 8 MiB (`profile_usage_store.zig:14-15`). **No cost ceiling or
  budget abort exists** — report-only.
- Recovery UX: `fx session recover <id>` copies a damaged session into a new one
  leaving the source intact; `fx session migrate <id>` (`--allow-large`) rewrites
  an old format; `fx doctor` inspects saved sessions and **prints the exact
  recovery command** for anything it can fix.

## 4. Context assembly and compaction

- Thresholds are exact integer ratios in one block
  (`src/core/agent/runtime/prompt_context.zig:13-20`):
  `usable = context_window − max_output_tokens` (:423-433); high-water
  `usable*4/5`; target `usable/10`; recent tail `usable/20`; soft ceiling
  `usable/4`; source reduction `/8`; generation multiplier `×4`; plus a
  512-token reserve, ≤64 chunks, 120 s (`context_compaction.zig:20-22`). One
  decision function, `planCompaction` (:49-108).
- **The trigger is measured on the serialized request body**, not estimated from
  the message list: `measureProviderRequest(alloc, request_body, …)` →
  `RequestCost{serialized_bytes, text_tokens, image_identity,
  estimated_input_tokens}` fed per attempt into `planCompaction`
  (`orchestrator.zig:6831-6908`), then calibrated against real returned
  `usage.input_tokens` (`calibrateProviderRequest`, :366-384). A
  `just_rebuilt_request` guard stops double compaction. Elsewhere estimation is
  bytes/4 rounded up (`token_estimate.zig:28-30`) + 8 per field
  (`prompt_context.zig:150, 222-235`).
- Retention cuts only at whole **execution step** boundaries: "Selects complete
  execution steps. Payloads are measured, never shortened"
  (`prompt_context.zig:121-208`); a `refine_budget` loop halves the window until
  it fits (`orchestrator.zig:5516-5540`).
- Large tool results offload **at write time**: spill at 16 KiB, 4 KiB preview,
  handle `result-<tool>-<callhash8>-<contenthash8>.txt`, ≤8 MiB per artifact,
  re-read 8 KiB default / 64 KiB max (`src/core/session/result_store.zig:12-17,
  500-514`), shown to the model as `<tool_result_preview …>` (:331-336) and
  paged by `read_tool_result` with byte ranges *or* a literal search (:372-377).
- Injected content is budgeted **by name**: 11 `context_limits` keys (skill
  description 1 KiB, skill file 1 MiB, MCP description 1 KiB, MCP selected
  schema 64 KiB, MCP server instructions 2 KiB, MCP search results 16 KiB,
  project instruction file 64 KiB, project total 128 KiB, image adapter 20 KiB…)
  (`src/core/config/context_limits.zig:20-29`), each settable to `"off"` behind
  a 64 MiB `emergency_ceiling_bytes` (:3), overridable per process with
  repeatable leading `--context-limit name=bytes|off`, skill catalog defaulting
  to ~2% of the model window (8,000 chars if unknown), and — decisively —
  *"Truncated or omitted context is reported to the runtime rather than silently
  treated as complete"*.

## 5. Providers, auth, model catalog

- **Three hand-rolled routes** (`src/core/config/model_provider.zig:4-7` =
  `{gateway, codex, grok}`): AI Gateway language-model spec v4 →
  `POST https://ai-gateway.vercel.sh/v4/ai/language-model` with
  `ai-gateway-protocol-version: 0.0.1` (`src/builtins/gateway.zig:46`); Codex →
  `chatgpt.com/backend-api/codex/responses`; Grok →
  `…/v1/responses` (`src/gateway/openai_codex.zig`, `xai_grok.zig`). **No
  Anthropic Messages transport, no OpenAI chat-completions transport, no
  ollama/local** — `/v1/chat/completions` appears only in a loopback fixture
  (`client.zig:7259`). Other vendors are model-id *prefixes* behind the gateway
  with prefix-keyed capability policy (`vercel_model_policy.zig:6,39-41`).
  ~480 KB of gateway code for 3 routes.
- Events: `Event = union(enum){ content_delta, reasoning_delta,
  tool_started{id,name,label,arguments_json}, tool_input_delta }`, emitted
  synchronously via `EventSink.emit`
  (`src/core/agent/stream_provider.zig:24-44`). **No transport delta coalescing**
  (the UI coalesces render requests) and **no client-side inter-token idle
  watchdog** — xdev's watchdogs + partial-stream retain-and-continue cover a
  case fx does not.
- Two-layer retry with an owner switch (`ProviderAttemptOwner{transport,agent}`):
  transport `retry_count = 3`, linear `(attempt+1)*150 ms`, Retry-After cap 5 s,
  statuses 429/500/502/503/504 (`client.zig:192-194, 2140-2160`); agent layer
  `default_max_provider_attempts = 10`, 250 ms/1 s/exp ≤30 s, Retry-After cap
  30 s (`model_response_recovery.zig:3-4, 182-192`). The strongest idea is the
  taxonomy: `DeliveryCertainty` + admission + tool-evidence reconciliation
  decide whether a failed attempt's tool calls may already have executed before
  a retry is permitted. No model fallback chain.
- Timeouts/caps: connect 30 s, response head 120 s, SSE event line 32 MiB,
  transfer buffer 256 KiB, failure detail 600 B, auth file ≤64 KiB
  (`client.zig:193-200`). Gateway requests may run 30 minutes; a timed-out
  stream pauses rather than retrying (`CHANGELOG.md:113`).
- Auth precedence `VERCEL_OIDC_TOKEN` → `AI_GATEWAY_API_KEY` → login session →
  stored key (`src/core/auth/credentials.zig:432-457`), with an explicitly
  chosen source as an **exact authority that never silently falls back**
  (:400-403) — a billing/team-boundary rule. Vercel login is OAuth *device
  code* (`oauth.zig:155`, poll interval default 5 s :254); Codex/Grok are
  browser PKCE S256 (`chatgpt_oauth.zig:969-970`). State files
  `~/.fx/{auth.json,chatgpt-auth.json,grok-auth.json,api-key}`
  (`profile_paths.zig:6-11`), created 0600 and re-verified
  `mode == 0600 && nlink == 1` before use (`session_presence.zig:56`,
  `oauth_session.zig:173`), parent forced 0700 else
  `PrivateStatePermissionsUnsupported` (:829-836). macOS: Keychain via
  `/usr/bin/security` (`FX_AI_GATEWAY_API_KEY`, `FX_OAUTH_SESSION_V1`,
  `src/core/hosts/native_keychain.zig`), tokens zeroized, `FX_DISABLE_KEYCHAIN=1`
  opts out.
- Catalog: live authenticated `GET /coding-agent/v1/models`
  (`builtins/gateway.zig:47,51`) cached in `core/app/model_cache_runtime.zig`;
  entries carry `id, model_type, has_reasoning, reasoning_efforts[…]` — fx
  parses the **advertised effort list per model**, while xdev pins effort per
  provider. A public fallback exists for catalog reads but strict authenticated
  requests never take it (`model_catalog.zig:744`). **No price/cost data at
  all** (`web_search_price` is the only price field, :295). Default model
  `moonshotai/kimi-k3`. There is **no bundled catalog**, so fx cannot boot
  offline the way xdev does.
- Diagnostics: opt-in scoped trace (`FX_TRACE`, `FX_TRACE_LOG`,
  `FX_TRACE_SCOPES`, `FX_TRACE_STDERR`, `src/core/shared/debug_trace.zig:414-417`)
  to `~/.fx/logs/trace.log`, 2 MiB rotation, logging *sizes and attempts*
  (`open attempt=2/3 url=… payload_bytes=…`, `client.zig:624`) and never
  request/response bodies.

## 6. Tools and the file-mutation contract

- **`edit_file(path, old_string, new_string)`** — exact string, must occur
  exactly once; three verbatim failures (`old_string and new_string are
  identical`, `old_string not found in file`, `old_string is not unique (found
  {d} occurrences), provide more context`) at
  `src/core/tooling/file_mutation.zig:1436-1475`. No line numbers, no hashlines,
  no multi-edit batch, no delete/rename tool (`write_file` says "When NOT to
  use: … deleting files", `builtins/tools.zig:44`).
- **Staleness is a cryptographic two-phase commit**, not read-before-edit:
  prepare canonicalizes the path with per-directory **inode identity**,
  evaluates permission targets under a 32 KiB / 256-component proof budget
  (`file_mutation_contract.zig:6-8`), and SHA-256s both the raw arguments JSON
  (byte-exact — key reordering changes the digest, test :2077) and the file
  preimage; apply re-verifies device+inode+preimage before writing. Content and
  postimage caps 4 MiB (`edit_file.zig:6`, `file_mutation.zig:1474`). The
  read-before-edit gate does **not** exist in production: `ReadTracker` records
  `{mtime_ns, sha256, coverage}` (`read_file.zig:330-336`) with no consumer.
- Caps: `read_file` 2,000 lines / 256 KiB view / 10 MiB snapshot; `grep_files`
  literal-only (no regex), 200 rendered / 2,000 collected matches,
  `context_lines ≤ 5`, context file ≤200 KiB; `glob_files` pattern ≤4,096 B,
  ≤100 results, candidate index ≤100k from
  `git ls-files -z --cached --others --exclude-standard` with recursive-walk
  fallback and 32 MiB git stdout cap (`workspace_files.zig:10-11`), rebuilt on
  every search (no persistent index); web: body ≤10 MiB, ≤10 hops, 60 s/hop,
  headers 64 KiB, URL ≤2,000 B (`http_fetch.zig:10-13`, `url_policy.zig:30`).
- **`shell` is a managed-session tool with 3 actions**
  (`run`/`interact`/`stop`): `yield_time_ms` default/max 30,000, `wait` default
  5,000 / ceiling 300,000; `yield_time_ms:0` returns a handle = background job;
  sessions persist **across turns**, 64 live + 32 tombstones
  (`managed_execution_contract.zig:5-10`); `interact` re-observes or sends exact
  characters (`\u0003` = Ctrl-C); `stop` = SIGTERM then force. Capture route:
  pipes, `pgid=0`, 100 ms poll, group-wide SIGTERM→SIGKILL after 800 ms, settle
  cap 5 s, with tests proving `setsid`/double-fork escapees are killed
  (`command_runner.zig:4428-4597`); `tty=true` route uses a persistent PTY.
  Preview keeps `head = (cap+1)/2` (:732-737); full output goes to a
  content-addressed artifact.
- Args pipeline: typed schema DSL with a **1,024-byte cap per tool description**
  (`model_tool_schema.zig:3-4`), descriptions following "What / When to use /
  When NOT to use" and stating caps in prose. No general JSON-repair parser —
  two deliberate repairs only: a re-parsed stringified object
  (`normalizeCompositeObjectValue`) and literal `"null"` read as field-absence
  (`tool_args.zig`), with large corrective failure messages.
- Prompt budget: **one baked Markdown file, 5,450 bytes**
  (`src/builtins/system_prompt.md`, `@embedFile`) ≈ 1.3k tokens [INFERENCE:
  bytes/4] — independent confirmation of xdev's <1,000-token goal (omp's full
  slate measures 12–15k).

## 7. Permissions, execution, containment

- `PermissionMode = enum { ask, auto, yolo }` (`src/core/shared/types.zig:1982`;
  `full-access` parses as `yolo`, :1990). **Config default is `.auto`**
  (`config_runtime.zig:18`) though the mode registry default id is `"ask"`
  (`builtins/modes.zig:12`). UI says "full access"; settings/wire still
  serialize `"yolo"`.
- Admission pipeline (`src/core/tooling/tool_admission.zig`): configured rules
  (deny first) → exact saved-session grants → deterministic lexical command plan
  (`src/core/shell_command/command_effect.zig`: fixed read-only allowlist vs a
  12-value `ApprovalReason` taxonomy, :248-261) → LLM reviewer (auto) → human
  prompter → **fail closed**; 9 `ShellAuthorizationSource` values
  (`command_admission.zig:51-61`).
- **`auto` = a second-model security reviewer**
  (`src/core/permissions/auto_classifier.zig`): `permission_decision` tool call,
  `maxOutputTokens 2048`, `toolChoice` required, to a *fixed* reviewer per
  provider — gateway/codex `openai/gpt-5.6-luna`
  (`src/gateway/permission_reviewer.zig:14`, `openai_codex_models.zig:20`),
  grok the session model. Packet ≤16 KiB, rationale ≤240 B, per-turn budgets
  **64 holds + 64 unavailable attempts** (`tool_admission.zig:33-34`).
  `caution` is returned **only for concrete prompt injection or malicious
  activity**; destructive/external/remote actions clear when not malicious
  (AGENTS.md:147). A `clear` review mints authority for *the exact unchanged
  action*; a `caution`, incomplete evidence, or an unavailable reviewer holds
  that one action and returns guidance. Prior review output is excluded from
  later security evidence by a stored `review_feedback` marker that "must never
  be inferred from output text" (AGENTS.md:151).
- Fail-closed specifics: no prompter (headless or ACP without a client) →
  `.permission_required` denial, traced `mode=headless approval_required=true`
  (`tool_admission.zig:1389-1393, 2383-2389`); unavailable reviewer → deny +
  `denial_reason = .review_unavailable` (:948-951); session grants are bound to
  the shell-environment identity and revalidated, so a stale fingerprint cannot
  re-authorize.
- **Containment is lexical, not OS-level: there is no sandbox.** Zero hits for
  `seatbelt|sandbox-exec|bwrap|landlock|seccomp|unshare|pledge` in `src/`; the
  `sandbox` key is proven inert (`config_runtime.zig:3141`, `test "legacy
  sandbox keys are inert unknown data"`). What exists instead is the read-only
  allowlist plus **direct-exec with a scrubbed 3-variable environment**
  (`PATH=/usr/bin:/bin`, **no `HOME`**, git env neutralized; 9 vars for the git
  subset, `direct_command.zig:412-428`) — auto-run commands have provably no
  ambient authority.
- Execution: `std.process.spawn`/posix_spawn behind `can_spawn`
  (`command_runner.zig:53-55`), one `ChildWaiter` thread per child publishing
  via `std.Io.Future` (:304-340), 100 ms output poll, approved shell via
  `sh -lc` with the script on **stdin** (:1591-1593), `pgid=0`, a foreground
  `setsid` supervisor, and PID-reuse-safe tree witness (macOS kernel-fd
  `DarwinProcessWitness`, `execution/process_tree.zig`). Background registry:
  64 + 32 tombstones, one thread per entry, state-machine lifecycle.
- Modes (`src/core/modes/mode_registry.zig`):
  `ModeSpec{id,name,description,permission_mode,tool_policy:full|read_only,
  denial_message}` — a mode *is* (permission mode × model-visible tool
  projection × runtime gating with pre-tool-use blocked-JSON denial) (:22-65).
  Built-ins `code`(auto/full), `ask`(ask/full); ACP exposes `ask`/`code`.
- Redaction is display-only: `text_utils.maskSecrets` (:430) masks basic-auth
  URLs, AWS keys, `token`-preceded ≥36-char runs, `gh._` tokens and
  `KEY=value` for password/api_key/secret/private_key/access_key (:616-835);
  `redactUrlForDisplay` masks `sig|authorization|cookie|credential|…`. Sensitive
  command output is not saved with the session, "including secrets split across
  output chunks or oversized lines" (`CHANGELOG.md:296`). No prompt-injection
  scanner on input beyond the reviewer's rules.
- Self-update: on by default (`app_lifecycle.zig:141`), manifest ≤4,096 B from
  `https://releases.fx.sh/latest` or `dev/manifest.json`, streamed tar.gz
  verified against a **same-origin `.sha256`** (`auto_upgrade.zig:255-283`),
  atomic self-replace with an 8-byte random temp suffix. No signature check;
  codesign is build-time only.
- Telemetry: none. `feedback/runtime.zig` is 5 lines (a URL);
  `agent/runtime/telemetry.zig` is local accumulation. Update checks carry no
  machine identifier.

## 8. Terminal UI

- Architecture is a **shadow-VT diff**, not a frame buffer: `render_request`
  coalesces reasons `{first_frame, transcript, footer, modal, subagent_panel,
  animation, notification, resize, external_damage}` into one `FrameAttempt`
  with commit/restore rollback (`src/ui/render_request.zig:6-58`) → `paint_plan`
  assigns row bands to `CellOwner = {preserved_shell, transcript, gap, activity,
  footer, diagnostic_clear}` (`paint_plan.zig:9-16`) → `frame_surface` builds the
  target grid with per-cell owner + hyperlink/combining pools and *typed*
  violation errors (`WriteOutsideBand`, `PreservedShellMutation`) →
  `terminal_diff.flushFrame` emits changed cells + scroll inside
  `\x1b[?2026h … \x1b[?2026l` with the cursor hidden, then **feeds accepted bytes
  back through its own VT emulator** and repairs partially-applied scrolls
  (`terminal_diff.zig:353`, `core/terminal/engine.zig`).
- **Chat runs inline on the main screen**: rows above `owned_top_row` belong to
  the shell and are unwritable by construction; scrolled-off transcript rows go
  into the terminal's real scrollback (`document_append`), and only Ctrl+O /
  approval / catalog screens take `?1049h`. That is why fx keeps native
  scrollback and exits with a clean terminal.
- Cadence: animation heartbeat 50 ms (20 fps ceiling, `render_request.zig:68`),
  blink half-period 10 frames; input poll 8 ms active / 1 ms focused-worker /
  16 ms wasm-idle; resize debounce 100 ms (`src/main.zig:177-182`); no steady
  repaint when idle.
- Resize: the SIGWINCH handler sets one atomic bit and mutates nothing
  (`ResizeApprovalInterlock`, `resize_runtime.zig:47-86`; `SA_RESTART` at
  `shell_runtime.zig:143-152`); the loop debounces, then runs phased resize with
  a **DSR cursor probe** measurement and three redraw modes.
  `src/ui/resize_tests.zig` is ~8.6k lines of in-process terminal-emulator tests
  — render regression coverage without a PTY.
- Capabilities, verbatim (`src/ui/terminal/terminal.zig:4-5`): interactive
  enable `\x1b[>4;2m\x1b[>1u\x1b[?2004h\x1b[?7l` (kitty disambiguate + event
  types, CSI-u push, bracketed paste, **autowrap OFF**), with a tmux variant that
  omits `\x1b[>1u` ("leaves kitty negotiation to tmux"); theme negotiation
  OSC 11 background query + `\x1b[?996;2031h` with a 75/200 ms reply grace
  (`theme_monitor.zig:7-8`); COLORTERM truecolor policy
  (`theme_protocol.zig`); mouse enabled **per overlay**, not globally. No kitty
  graphics, no OSC 52 clipboard, no IME preedit.
- Streaming: deltas queue in `AssistantPacer` (`pacer.zig:137`); `tick()` emits
  the whole complete prefix (no fake pacing — `pause()` is a literal no-op,
  :142); an incomplete trailing ANSI/UTF-8 tail is neutralized as U+FFFD +
  `\x1b[0m` rather than emitted half (:250-274); SGR spans carry across frames.
- Transcript is **provenanced entries**, not a block-state enum:
  `TranscriptBlockKind` has 15 values (`user_turn, assistant_turn,
  assistant_table, assistant_code_block, assistant_thematic_rule, turn_summary,
  welcome, tool_status, command_output, diff_block, system_notice, error_notice,
  cancel_notice, subagent_status, unknown`); `collapse_tool_calls` folds a group
  to one summary in the compact view while Ctrl+O shows all; command output
  folds to ≤5 rows (`transcript/command_output_runtime.zig:1625`); transcript
  cache 256 KiB.
- Chrome: 4-row footer (`footer_rows = 4`, `main.zig:177`), statusline segments
  `workspace · branch · model · perm · context · session-title` with
  width-budgeted middle-clip and a 32-cell title cap (`src/ui/render.zig:180-300`),
  welcome reserving 11 rows, a shimmer activity row, desktop notifications
  (macOS default on), and **cuelume AAC chimes** materialized to `TMPDIR` and
  played by `afplay` with a terminal-bell fallback
  (`src/core/notifications/sound.zig`).
- UI/state separation is contract-enforced: `src/ui/transcript/store.zig:1-16`
  documents the core-owned "Host contract" fields and methods, the functions
  take `self: anytype`, and input crosses into core only as typed
  `Action`/`TerminalInputEvent` (`core/input/input_action.zig`). Composer state
  is layout-independent bytes+cursor (`editor_state.zig:29-41`) with separate
  visual row math, kill ring, undo, stash, edit history, word boundaries, and an
  8 MiB paste cap (`paste_framing.zig:17-21`). Double-press windows: Ctrl-C exit
  3,000 ms, Esc clear 500 ms, Esc interrupt 1,000 ms
  (`gesture_state.zig:3-5`).

## 9. Extensibility

- **Skills**: 13 discovery roots — 7 workspace (`.fx/skills`, `skills/`,
  `.opencode|.codex|.claude|.agents|.claw/skills`) scanned at the workspace root
  **and every ancestor up to `$HOME`** (`skill_runtime.zig:518-528`), 5 home
  compat, 1 managed `~/.fx/skills` (`builtins/skills.zig:16-40`). Two
  frontmatter keys, frontmatter ≤64 KiB, name ≤256 B (`skill_contract.zig:4-5`).
  Locations are **opaque** — `skill:<ns>:<idx>/<leaf>` — so a renamed skill
  invalidates stale handles by namespace; the body returns as
  `<skill_content name= location=>` with entity-escaped display names
  (`skill_invocation.zig`). `skill_search` and `mcp_search_tools` were retired
  into one `capability_search` (`builtins/tools.zig:1097-1098`); `install_skill`
  is the entire "marketplace".
- **MCP**: transports `.stdio` (modern/legacy negotiation,
  `protocol_negotiation.zig`), `.http` Streamable, `.sse` legacy
  (`server_transport.zig:59-90`), plus `docker_run.zig` for containerized stdio
  servers; discovery response capped 1 MiB (:32). Names
  `mcp_<normalized-server>_<tool>` with collision suffixes **and retired-name
  reservation** (`tool_names.zig:12`, cap 65,536). **No MCP tool is ever
  advertised**: `capability_search` retrieves over metadata, `mcp_select_tool`
  promotes one tool, whose schema is projected under the 64 KiB budget and
  rejected with a machine-readable `context_limit_rejection`
  (`selected_schema.zig`). Resources/prompts ride one 7-action `mcp_features`
  tool. Real OAuth: PRM challenge + dynamic client registration + browser
  callback, validated issuer, zeroized tokens, 60 s refresh skew
  (`mcp_auth.zig:13-14`). Project `.mcp.json` requires per-server **trust**
  (`menu_state.zig`, `fx mcp trust approve|reject|approve-all|reset`) and
  `fx mcp list` reports config + saved auth **without connecting**
  (`--connect` for live health). Conformance is ratcheted against pinned
  `@modelcontextprotocol/conformance@0.2.0-alpha.10` with an
  `expected-failures.yml` that fails on a new failure **or** on a listed check
  starting to pass.
- **Hooks: four in-process events, no third-party hooks.** `PreToolUse`
  (`continue | rewrite_arguments | block(reason)`), `Stop`
  (`allow | continue_once(context)` — one synthetic continuation),
  `PostTurnEnd` (observe), `AttentionRequired` (observe; kinds
  `permission|question|route_recovery`) — `definitions.zig:4-50, 112-200` —
  delivered by compiled-in handlers on a frozen registry (duplicate or
  post-freeze registration is an error). Limits: handler name ≤128 B, block
  reason ≤4 KiB, stop context ≤64 KiB, args ≤1 MiB (:73-78). No user-configurable
  hook, no subprocess protocol, no exit-code contract.
- **Subagents**: same binary, real `std.Thread` children (`managed_owner.zig:37`),
  each its own resumable session. Contract `subagent{action: run|message}`
  (`tools/agent/subagent.zig:105-122`) — `run` one-shot, `message` a **named
  persistent child** that takes mid-task feedback without interrupting its
  current tool, keyed by `work_id`/generation so stale results cannot apply
  twice. Children get an *authority snapshot* minus the `subagent` tool
  (`cloneToolsWithoutSubagent`, `authority.zig:255` — depth 1), ≤256 children,
  child state ≤512 KiB (`child_state.zig:16-17`), approvals routed to the
  parent with a hashed `stableApprovalId` (`approval_registry.zig`). "Delegation
  cannot grant a child broader access" is enforced.
- **Commands**: static `TopLevelSpec`/`SlashSpec` tables over a generic
  `CommandRegistry` (`core/mods/registry.zig` — the whole of "mods": two
  registries, **not** a plugin format), 11 help categories, ACP child chat
  restricted to an exact 3-command subset (`command_specs.zig:177`); text and
  JSON render from one `Snapshot`. No user-defined commands, no `$ARGUMENTS`.
- **Config**: five layers, later wins — compiled defaults < `<workspace>/.fx.json`
  < `~/.fx/settings.json` global < `settings.json["workspaces"][<root>]` < env,
  then process flags (`config_runtime.zig:420→451→491, 1554-1557`), with
  **per-key source tracking** so diagnostics can name the winning layer; unknown
  keys ignored, a bad value quarantining that *layer* with a startup diagnostic
  rather than killing the run; settings file capped 64 KiB. **Project
  `.fx.json` accepts exactly three repo-safe fields** (`max_agent_steps`,
  `max_tool_result_bytes`, `context`); model/effort/fast-mode, `permission`,
  credentials, `update_channel` and `context_limits` are profile-only — "This
  prevents a repository from silently changing your model preferences" (docs
  Models), and `sandbox` is accepted-but-inert (§12).
- **Workspace**: context = `AGENTS.md` only (global `~/.fx/AGENTS.md`, workspace
  + ancestors; no CLAUDE.md reading), injected as escaped
  `<global-rules from="…">` / `<project-rules from="…">` sections with
  entity-encoded attributes (`builtins/context.zig:271-288, 489`), and
  **resolved per tool-call target**: a call under `apps/web/src/` also gets
  `apps/web/AGENTS.md` as the narrower scope (docs Project instructions). Ignore
  semantics are a static 8-basename list, no `.gitignore` (`ignored_dirs.zig`).
  Additional workspace directories (≤16 absolute paths) extend **tool access
  only** — never config, AGENTS.md, skills, hooks, git identity, sessions or
  history.
- **Undo**: `core/workspace/change_tracker.zig:33` keeps the last **100** file
  operations with previous content; `/undo` reverts the most recent tracked
  change (explicitly not shell commands, git history, or out-of-band edits).

## 10. Embedding: ACP, libfx, WASM

- **One artifact, three composition roots.** The same `src/acp` server runs as
  `fx acp` on stdio, inside `fx-core.wasm`/`fx-term.wasm`, and inside the
  `libfx.node` N-API addon; all three build one `acp_runner.Config`
  (`src/core/cli/acp_runner.zig:14-50`) — **36 fields**: 3 bounded defaults, 2
  endpoint strings, 6 provider structs (`gateway_provider`, `provider_set`,
  `process_provider`, `secret_store`, `context_registry`, `mode_registry`), 7
  numeric resource caps and 3 capability booleans. The WASM root just zeroes the
  caps and swaps in `unavailable_secret_store`, an empty system prompt and
  `gateway_only` (`src/wasm_core_main.zig:29-58`).
- Port idiom: `{ context: ?*anyopaque, xxx_fn: *const fn (…) !T }` plus a
  sibling `pub const unavailable_*` (9 of them; ~32 files with `_fn: *const fn`)
  — no interface objects, no codegen: every seam is hand-written. The second,
  orthogonal axis is `core/hosts/runtime_profile.zig`: a comptime `Profile` of
  **21 bools** with `allows(comptime App, cap)`, `native` and `wasm` exact
  complements on 14 fields, guarded by tests that fail if a capability is
  enabled alongside its host-backed substitute.
- **ACP: 15 dispatch methods** by exact string compare
  (`src/acp/server.zig:53-91`): `$/cancel_request`, `initialize`,
  `session/new|load|resume|close|list|remove|prompt|cancel|set_config_option|set_mode`,
  plus private `libfx/new|checkpoint|restore`. Native omits `session/remove`
  (:1276); WASM omits `session/resume`/`session/close` (:1252) — an accidental
  two-table asymmetry rather than one table with capabilities negotiated in
  `initialize`. `initialize` emits `loadSession:true`, prompt capabilities,
  `mcpCapabilities{http,sse}`, `sessionCapabilities{list,resume,close}` and an
  **empty `authMethods`** — no in-band login: the host supplies credentials out
  of band (`acp --model`, `apiKey`, env).
- Framing is **newline-delimited JSON-RPC 2.0**, inbound frame cap 8 MiB
  (`jsonrpc.zig:275`) answered by custom code `-32000 request_frame_too_large`
  **without dropping the connection** (`server.zig:772-780`); Reader and Writer
  are `(context, fn-pointer)` pairs, which is how stdio, WASI `fd_suspension`
  and the N-API fd share one server. 9 `sessionUpdate` kinds are emitted
  (`src/acp/types.zig`), including `user_message_chunk` with base64 images and a
  16-random-byte `messageId` (:12-17, :140-213).
- Permissions cross as ACP, not as a host import: `session/request_permission`
  with exactly `allow_once | allow_always | reject_once`
  (`prompt.zig:1712-1716`), blocking the prompt thread and **denying on every
  non-happy decode path** (`server.zig:1179-1188`), one pending at a time. Tool
  arguments are re-parsed and re-stringified before display
  (`writeValidatedToolArguments`, `prompt.zig:1729-1734`) so argument text
  cannot inject presentation.
- Protocol budgets: 3 outbound requests (`session/request_permission`,
  `elicitation/create`, `libfx/tool_call`) + 2 notifications; 64 MiB per ACP
  message on both backends; ACP history 100 turns; tool result 64 KiB text /
  8 MiB rich; 32 pending outbound; libfx 64 agent steps.
- **libfx JS API**: `createFxAgent({apiKey, model?, checkpoint?, fetch?, tools?,
  mcpClients?, skills?, onEvent, onPermission})` → `{prompt, checkpoint, close}`;
  `prompt` returns an async iterable of exactly 4 events (`text_delta`,
  `reasoning_delta`, `tool_start`, `tool_end`) plus `await turn.result` →
  `{stopReason, usage}`. Output is lossless and **backpressured**: a slow reader
  pauses production (unread events ≤1 MiB/256 messages, transport output ≤8 MiB);
  one consumer per turn and breaking the iterator cancels it; transport or
  decode failures **reject** rather than returning success with missing text.
  `checkpoint()` is idle-only, opaque, ≤4 MiB/1,024 turns. Importing performs no
  MCP connect, skill scan, process spawn or FS read.
- The WASM host seam is **28 `fx_*` imports** implemented by one JS object
  (`sdk/fx-sdk.js:1006-1046`): `fx_http_request` +
  `fx_http_stream_open/status/next/close`; session storage with an optimistic
  revision CAS (`fx_session_load/commit/list/remove`); config get/set; prompt
  history; OAuth sessions; host tool calls + result read/release; open url;
  clipboard; workspace info/exec; term size/poll. All 14 `path_*`/`fd_*` WASI
  imports collapse to ENOSYS (`unavailable = () => 52`, no preopens) so
  capability enters **only** through the `fx` module; async host calls suspend
  via JSPI. Build side: `-Dwasm-surface=core|term`, `-Dnapi-surface=core`,
  wasm32-wasi `ReleaseSmall`, `single_threaded`, 1 MiB core stack
  (`build.zig:297-339`); N-API forces `ReleaseSafe`.
- Publishing: `publish-libfx.yml` on `workflow_run` of CI/Release on main,
  `--provenance`, tag==SHA equality, and a **byte-for-byte comparison of the
  downloaded registry tarball against the built artifact**; version is scraped
  from `src/main.zig`, not `package.json`. Two version axes: public JS API 2,
  internal addon ABI `libfxApiVersion = 3` (16 exports).
- Integrator cost, measured: `examples/node-chat/handler.mjs` is **26 lines**
  for a complete hosted agent (Next route 26, Nuxt 27, browser 55, readline chat
  28), and `examples/shared/gateway.mjs` is 88 lines of **fetch-seam transport
  policy** (origin/method allowlist, 32 KiB body, 512 maxOutputTokens, 30 s
  timeout) injected through the agent's `fetch` option — tenancy, quota and cost
  control with no new API surface. `examples/examples.json` pairs each example
  with the natural-language prompt that rebuilds it, and `examples.yml` runs
  them in CI.
- Headless contract (`fx ask`): 13 flags incl. `--json`, `--quiet`, `--no-save`,
  `--prompt-permissions`, `--continue-recovery`, `--resume last|<id>`, `--image`,
  `--system`, `--timeout`; output precedence json > quiet > raw-if-not-tty;
  `output` = all assistant Markdown, `final_output` = only a completed final
  response (empty on interrupt/failure/background), and input/output token
  counts. No JSON event streaming, so a one-shot script cannot render progress.

## 11. Engineering discipline

- **Two gates, not one.** `ci.yml` (PR + push to main, ubuntu only): fmt, corpus
  validation, public-surface audit, `timeout 900s zig build test`, SDK
  qualification, deterministic E2E with blanked credentials. `full-ci.yml` runs
  on push to **any non-main branch** — agents push draft branches and the
  expensive matrix is the ready-gate, not the PR check.
- **The ship gate is API-verified, not `needs`-based**: `full-suite`
  (`full-ci.yml:230-269`) queries `gh api …/actions/runs/$RUN/jobs` and
  jq-asserts the run contains *exactly* `[Native checks (ReleaseSafe, <target>)]`
  + 4 × `[E2E (… shard N/4)]`, all `conclusion == "success"`. A skipped or
  never-generated matrix row therefore fails. Full CI = 4 platforms ×
  (1 native + 4 shards) = 20 jobs + 4 ship gates on `ubuntu-24.04`,
  `ubuntu-24.04-arm`, `macos-15-intel`, `macos-15`.
- **Shard planning is data-driven**: `tests/e2e/ci-shards.ts` greedy
  longest-processing-time bin-packing over checked-in per-file weights (63
  files, weights 1–289, sum ≈2,850), failing on a missing, stale or
  double-assigned entry; files run `--max-concurrency 1`, one per process, one
  retry per file.
- Determinism comes from the environment, not the test: `AI_GATEWAY_API_KEY: ""`
  and `VERCEL_OIDC_TOKEN: ""` at job level in every deterministic lane; secrets
  reach only `e2e_acp_full_mode`, gated to `refs/heads/main`. ~20 `FX_E2E_*`
  product overrides point the real binary at local fake gateways and are
  **loopback-validated in code** (`isLoopbackHttpUrl`). tmux helpers give each
  run its own socket, `TMUX_TMPDIR`, stripped auth env,
  `FX_DISABLE_KEYCHAIN=1 FX_SKIP_ONBOARDING=1 FX_SOUND=0 NO_COLOR=1`.
- **Test pyramid with an escalation rule** ("pick the lowest layer that can
  observe it"): in-process Zig tests driving a bounded VT emulator
  (`core/terminal/engine.zig`) → Bun E2E driving the real binary through tmux →
  `.fxtape` byte recordings → opt-in native terminal-app scenarios.
  `FX_DEBUG_RECORD=1` auto-records `~/.fx/recordings/fx-record-<ms>-<hex>.fxtape`;
  `fx replay <tape> [--frames|--json|--golden out.txt]` needs no TTY, so any
  reviewer can replay a user's bug and goldens can be checked in
  (`CONTRIBUTING.md:402-415`).
- **PR CI doubles as a leak audit**: `scripts/check-public-surface.sh` greps the
  tracked tree for `/Users/<person>/` paths, real `team_[A-Za-z0-9]{24}` ids and
  internal slugs, and verifies a checked-in fixture tarball is root:root, free of
  AppleDouble `._` entries, carrying a `sanitized-v1` marker.
- Release: version in `src/main.zig` *is* the trigger (strict semver, no-op if
  tagged, tags only from the workflow, `build.zig.zon` version a forbidden
  placeholder); actions pinned by full SHA; `prepare-release` feeds
  `git diff prev..HEAD -- src/` (80 KB cap) to `anthropic/claude-opus-4.7` to
  draft the changelog under "the diff is the source of truth; do not trust commit
  messages", with a verified-signature commit check; macOS stable arm64 is signed
  and notarized **only after** PGSO eligibility; the dev channel writes immutable
  commit-addressed artifacts (`cli/dev/<sha>/`, 1-year cache) and only then
  atomically repoints `cli/dev.json` (max-age 60); CDN uploads are direct
  `curl -X PUT` with per-file HTTP-code checks, and `cdn-backfill.yml` is a
  dry-run-capable recovery tool. No cron, no coverage thresholds, no merge
  queue, no bot reviewers.
- Issue hygiene: one template (`fx-report.yml`) — required description, optional
  **redacted `/trace`**, and a required checkbox that secrets were removed. No
  version field.
- `AGENTS.md` "Declaring Work Ready" (:5-17): never say ready/done/complete
  until *you personally ran the binary and drove one real interaction*; a green
  suite is "necessary, not sufficient" because tests "do not always construct the
  full runtime, attach a TTY, or spawn background threads"; 5-step checklist;
  **always** `./zig-out/bin/fx`, never PATH; "Do not document intended behavior
  as if it already exists"; every feature first answers "which module owns this /
  what is the typed contract / does it persist / text+JSON / docs+tests / corpus
  classification" (`AGENTS.md:76-87`).

## 12. Docs-vs-code deviations (what fx's own claims do not survive)

The mirror image of xdev's recurring "engine landed, arming absent" class:

1. **"6.17 MiB native binary"** (`README.md:14`) is computed nowhere — `grep
   6\.17` hits only the README; `CHANGELOG.md:248,263` says **6.12 MiB** for
   macOS arm64 at an earlier version, so two docs disagree. The only
   machine-checked size number is the 7.800 MiB PGSO ceiling.
2. **`sandbox` is a documented field with no implementation.** AGENTS.md:137
   speaks of "an effective sandbox of `none`" and docs list `sandbox` among
   accepted project keys; `config_runtime.zig:3141` proves it inert. There is no
   OS containment (searched seatbelt/bwrap/landlock/seccomp/pledge: 0 hits).
3. **ACP capability claims outrun the code**: `fs/read_text_file`,
   `fs/write_text_file` and `terminal` client capabilities are parsed and stored
   (`server.zig:1630-1644, 1906-1908`) and then **never read** (zero occurrences
   of the requests anywhere in `src`); `available_commands_update` is emitted
   once per new session **hard-coded to `"[]"`** (`sessions.zig:342`) despite ~40
   commands and an existing writer (`types.zig:275-278`); `set_mode` and
   `set_config_option` ack `null` and emit no `current_mode_update` /
   `config_option_update`.
4. **Image support is asymmetric**: ACP advertises `image`, native `fx ask
   --image` works, but the SDK throws "image prompt blocks are unsupported"
   (`fx-sdk.js:1192`) and the WASM path returns "not supported in this runtime"
   (`prompt.zig:683`).
5. **"Hosts provide … permission handling"** is true but non-uniform: the other
   four capabilities are `fx_*` imports; permission is an ACP round-trip surfaced
   as the JS `onPermission` option (`fx-sdk.js:1440-1451`).
6. **`ReadTracker` is dead infrastructure**: read records
   `{mtime_ns, sha256, coverage}` (`read_file.zig:330-336`) with no production
   consumer, so read-before-edit is prompt text only (`builtins/tools.zig:42`).
7. **`benchmarks/results/` holds only `.gitkeep`**; `AGENTS.md:338`'s claim that
   benchmark results land in Vercel Blob is unimplemented, and
   `check_budgets_test.py` plus `summarize.py`'s JSON are referenced by no
   workflow.
8. **Two defaults for one setting** (mode registry `ask` vs config `.auto`), and
   `build.zig.zon .version = "0.0.0"` beside `main.zig 0.0.9`, with npm `libfx`
   published at 0.0.8 — deliberate, but the drift a docs/code audit must catch.
9. **E2E hooks are compiled into the shipped binary**:
   `acp/prompt_test_controls.zig` busy-waits on a marker file whenever
   `FX_E2E_ACP_PROMPT_TERMINAL_READY/_REAP_READY/_RELEASE` are set, called from
   the production reap path (`server.zig:1572`), with no `is_test` guard — an
   env-driven indefinite hang inside a released agent.

## 13. Where fx and xdev disagree on purpose

| Axis | fx | xdev | Verdict |
|---|---|---|---|
| Session model | linear `events.jsonl` + monotone checkpoint watermark, validator-enforced | append-only **tree** + mutable leaf | xdev keeps `/tree`, `/branch`, rewind; fx keeps replay triviality |
| Memory ceiling | per-structure caps, **no process gate** | hard <100 MB RSS + CI proof + windowed materialization | xdev stronger |
| Startup budget | 2 ms mean, Linux, hyperfine-gated | none | fx stronger |
| Size control | base-vs-head delta warning + 7.800 MiB release ceiling + PGSO | static Go build, no size CI | fx stronger as *process* |
| Providers | 3 routes, gateway-first, no local/ollama | 4 v1 + 4 v2 transports, any baseUrl, offline-bundled catalog | xdev stronger |
| Approvals | 3 modes, default `auto` with LLM reviewer, no prompter ⇒ deny | 3 tiers, default YOLO, per-pattern rules | different philosophies; fx's reviewer is the missing piece |
| Containment | lexical allowlist + 3-var scrubbed env | env-hardening list + documented external sandbox recipes | fx's scrubbed direct-exec is the cheap middle |
| Extensibility | compiled-in only; 4 in-process hook events | subprocess JSONL extensions + hooks + MCP + custom commands | xdev stronger for third parties; fx's 4-event cut is the right minimal set |
| Rendering | inline bands + shadow-VT diff, native scrollback preserved | tcell frame plan, alt-buffer overlays | fx's scrollback story is what users notice |
| Embedding | ACP + WASM + N-API from one artifact; 36-field Config | rpc + acp + print as modes of one loop | same instinct; fx adds a JS host seam |
| Tool results | offloaded at write time + `read_tool_result` paging | OutputSink head+tail, **middle bytes discarded** | fx stronger |
| Context budget | 11 named byte limits + `--context-limit` | hardcoded caps (32 KiB context files, 8/32 KiB rules) | fx stronger |
| Project config | 3 repo-safe fields, rest ignored | full schema, project layer wins | **xdev unsafe** (§14) |

## 14. What fx revealed about xdev (verified at `7c19a54`)

Three findings were reproduced against xdev's tree, not inferred:

1. **The project config layer is not repo-safe (critical).** xdev layers
   `defaults ← global ← project ← overlays` over *one* schema
   (`internal/config/settings.go:979-997`), so `<repo>/.xdev/config.yml` can set
   `approvalMode`, `defaultModel`, `modelRoles`, `toolsApproval`, `bashPatterns`
   and `bash.interceptor`; `.xdev/models.yml` is layered the same way
   (`internal/config/models.go:227-251`) and can set any provider's `baseUrl`
   and `apiKey`. A throwaway overlay probe (`internal/config`, run 2026-09-13)
   showed a temp repo's `.xdev/config.yml` producing `policy mode=yolo` and
   **executing** the project-declared interceptor (side-effect file written),
   while its `.xdev/models.yml` resolved `baseUrl=http://attacker.example:9/v1`.
   fx's counter-model — three repo-safe keys, everything profile-owned ignored —
   is the fix. → issue #114.
2. **`internal/acp/acp.go:2` still documents "LSP-style Content-Length
   framing"** although `internal/acp/framing.go:18-32` writes and reads
   newline-delimited frames (the fix from parity finding T5) and
   `framing_test.go:30` asserts no `Content-Length` appears. ACP's stdio
   transport requires newline-delimited messages, so the code is right and the
   package doc is a trap for the next reader. → fixed with this change.
3. **No retained-output path for foreground tool results.**
   `internal/tool/sink.go:9-13` states the design outright: "MVP spills no
   artifacts to disk — the truncation marker is the record". Only background
   bash jobs keep a file (`internal/tool/bash.go:248-274`). fx's 16 KiB spill +
   handle + `read_tool_result` re-read is the model to copy. → issue #115.

## 15. Verdict summary (fed into PRD §5.2)

**Adopt**: repo-safe project config allowlist (#114); credential-store hardening — Keychain shell-out, read-time `0600 && nlink == 1` re-verification, no fall-through from an explicitly named source (#123); measure the compaction
trigger on the serialized body and calibrate against returned usage (#116);
retain large tool results behind a model-facing re-read (#115); named byte
budgets for injected context with reported truncation (#116); target-scoped
AGENTS.md resolution (#108); a bounded second-model approval reviewer (#117);
startup + frame latency budgets in CI (#118); per-PR base-vs-head binary-size
delta and a public-surface leak audit (#119); an API-verified ship gate that
fails on a skipped matrix row (#119); terminal tape record/replay with goldens
(#120); resolved-config provenance plus `xdev doctor` printing the repair
command (#121); MCP deferred schemas under a budget with a machine-readable
rejection (#97); coverage-seq compaction watermark and step-boundary retention
(#83); fsync/length-guard/rollback/turn-discard durability set (#122).

**Verify**: fx's 80% high-water / 10% target / 5% tail ratios as xdev defaults
(xdev's are configurable but undocumented numbers); shadow-VT diff vs the
frame-plan (#78); effort tiers read from a provider's advertised list (#98);
per-provider model memory (#105).

**Reject**: default-on LLM reviewer (xdev stays YOLO-in-sandbox; ship it as an
opt-in `auto` tier); the linear log (branching is the product); WASM/N-API
embed surface (Go cannot ship it CGO-free); marketplace-shaped discovery;
`available_commands_update`-style half-wired capability advertising.

**xdev already ahead**, and fx is evidence for it: hard RSS budget + memory CI
proof, append-only session tree with rewind/branch/`/tree`, 4 wire transports
plus any-baseUrl/local providers, offline bundled catalog, per-model cost data,
subprocess extension protocol with third-party hooks, custom markdown slash
commands, the todo engine, LSP/AST tools, regex search via `rg`, stream
watchdogs, session listing via prefix reads (fx full-parses), and the 2 ms-class
startup *without* needing PGSO (Go's linker does the dead-code work).

---

*Research performed 2026-09-13 from `ced6180`. Cross-check verdicts live in
`docs/PRD.md` §5.2; the derived work items are issues #114–#123.*

