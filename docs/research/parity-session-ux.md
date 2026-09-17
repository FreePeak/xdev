# ScoutSessionUX — feature specs (session/UX surface, 12 omp docs)

## 1. Slash-command internals + markdown custom commands
**What**: `/cmd args` dispatch: built-in registry, extension/custom/MCP-prompt commands, and markdown file commands discovered from several tool conventions; expansion runs before every prompt.
**Semantics** (slash-command-internals.md):
- Capability keyed by command name; providers priority-desc, first-wins dedup (shadowed kept + marked `_shadowed`): native 100 → omp-plugins 90 → claude 80 → claude-plugins 70 / agents 70 / codex 70 → opencode 55. Built-ins reserve names before all file commands.
- Native roots: `<cwd>/.omp/commands/*.md` then `~/.omp/agent/commands/*.md` (project beats user); non-recursive glob, gitignore honored, hidden dirs skipped.
- Claude provider: recursive `**/*.md` under `~/.claude/commands` + `<cwd>/.claude/commands`; subdir files also get alias `foo:bar`; **user beats project** (opposite of native). Codex (`~/.codex/commands`) and opencode (`~/.config/opencode/commands`) user-first; frontmatter `name` overrides filename. claude-plugins names commands `<plugin>:<command>`.
- Description = frontmatter `description` else first non-empty body line (≤60 chars + `…`); body = prompt template. Frontmatter parse: native = fatal, others = warn + fallback key/value parse.
- Pipeline order: built-in → extension → custom/MCP prompt → file slash → prompt template → delivery (idle = send; streaming = steer/followUp queue per `streamingBehavior`; extension cmds run immediately even while streaming).
- Args: `$1..$n`, `$@[start]` / `$@[start:length]` (1-based), `$ARGUMENTS`, `$@`; quote-aware split (single/double quotes, no backslash escapes, unmatched quote not an error); if template lacks an inline placeholder, raw args appended as fallback.
- Unknown `/foo` is **never rejected** — falls through as literal prompt text. Autocomplete merges all sources; refresh on init, cwd change, editor swap, plugin reload — no file watcher.
**Surface**: markdown files in roots above; `/`-autocomplete; toggles `commands.enableClaudeUser/enableClaudeProject`, `commands.enableOpencodeUser/enableOpencodeProject`.
**Go port**: stdlib fs walk + minimal frontmatter parse + string template; ~1 file.
**Parity: CORE** (no-code user extension point). Minimal v1 = native roots + arg substitution; multi-provider discovery NICE.

## 2. CLI surface
**What**: `omp [cmd] [flags] [msgs...]`; first non-flag arg that isn't a subcommand = initial prompt (default `launch`); `--` ends flags; non-TTY stdin = prompt (no `-` marker); `@path` attaches files/images to initial message.
**Launch flag groups**: session/workspace (`--cwd --add-dir --allow-home --profile --alias --config⟨rep⟩ --session-dir --no-session`); history (`--continue/-c`, `--resume [id]/-r`, `--fork <id|path>`, `--from-claude`, `--from-codex`, `--export <file>`, `--no-title`); models (`--model`, `--smol/--slow/--plan`, `--models`, `--provider`, `--api-key`, `--provider-session-id`, `--prompt-cache-key`, `--service-tier`); thinking (`--thinking off|minimal|low|medium|high|xhigh|max|auto`, `--hide-thinking`, `--print-thoughts`, `--external-thinking`); prewalk/plan (`--prewalk/--no-prewalk/--prewalk-into`, `--plan-yolo/--plan-yolo-into`); tools/approvals (`--tools/--no-tools`, `--no-lsp`, `--no-pty`, `--approval-mode`, `--yolo/--auto-approve`, `--advisor`, `--max-time`); extensions (`--extension/-e`, `--hook`, `--trusted-extension`, `--plugin-dir`, `--no-extensions`, `--skills`, `--no-skills`, `--no-rules`); prompt (`--system-prompt`, `--append-system-prompt`); output (`--mode text|json|rpc|rpc-ui|acp`); `-p/--print` headless (stream stdout, exit; `--mode json` = event stream; `echo x | omp -p`).
**Subcommands (~40)**: launch, acp, auth-broker, auth-gateway, agents, bench, browser-relay, cleanse, commit, completions, compress, config, dry-balance, gc, grep, gallery, git, grievances, if-bench, images/img, install, join, models, plugin, ps, say, share, setup, shell, read, render, ssh, stats, update, usage, tiny-models, token, ttsr, worktree/wt, search/q.
**Go port**: custom arg walker (positional-defaults-to-launch); `-p` = run loop + stdout render; `--mode json` = newline JSON events.
**Parity**: CORE = launch/-p/--continue/--resume/--fork/--model/--thinking/--mode/-h/-v/--config/--profile; NICE = --from-claude/--from-codex, --export, commit, models, config, completions, update, stats, gc, worktree; SKIP = bench/if-bench/gallery/grievances/dry-balance/render/tiny-models/say/ttsr/browser-relay (dev/test/novelty, no daily-driver value).

## 3. Keybindings
**What**: remappable TUI chords; `/hotkeys` shows live bindings incl. remaps + extension additions.
**Semantics** (keybindings.md): `~/.omp/agent/keybindings.yml` (NOT in config.yml); map action-id → chord string or array; empty array disables. Named profile: default profile's file loads first, active profile overrides per action. Chords case-insensitive UI notation (`Ctrl+P`, `Alt+Shift+P`, `Shift+Enter`). Legacy unqualified names migrated; `keybindings.json`/`.yaml` still accepted+migrated.
**Core action IDs**: model cycle `Ctrl+P` / back `Shift+Ctrl+P` / temp pick `Alt+P` / selector `Alt+M`; plan toggle `Alt+Shift+P`; history search `Ctrl+R`; tool expand `Ctrl+O` / visibility `Ctrl+Shift+O`; thinking toggle `Ctrl+T` / cycle `Shift+Tab`; external editor `$VISUAL/$EDITOR` `Ctrl+G`; followUp `Ctrl+Q`+`Ctrl+Enter`; dequeue `Alt+Up`/`Shift+Up`; retry `Alt+R`; reset display `Alt+L`; copy line/prompt `Alt+Shift+L`/`Alt+Shift+C`; paste raw `Ctrl+Shift+V`/`Alt+Shift+V`, paste image `Ctrl+V`(+`Cmd+V` mac); live voice `Ctrl+L`; agents hub `Alt+A`; STT unbound (hold Space = push-to-talk). Windows Terminal quirks handled with fallback chords (Ctrl+V/Ctrl+Enter swallowing); OSC 5522 enhanced paste + bracketed paste + pasted image-file path loading.
**Go port**: YAML map + chord parser + table-driven key handler; defaults compiled in, remap layer thin.
**Parity: CORE** for mechanism + editing/model/tool actions; NICE for voice/STT/live/hub chords (depend on features xdev may skip).

## 4. Session ops — export / dump / share
**What**: `/dump` text transcript (+ tmp `omp-llm-request-<id>.json` sidecar: model/thinking/tier/system prompt/wire tool schemas/LLM messages — contains secrets, flagged) to clipboard; `/export [--themes] [path]` self-contained HTML (whitespace tokenization — no quoted paths); `--export <file>` pre-start CLI export; `/share` E2E-encrypted snapshot → viewer link.
**Semantics**: export embeds header/entries/leaf + current systemPrompt + tool descriptions; subagent transcripts `<session>/<AgentId>.jsonl` (recursive) embedded as subSessions with breadcrumb overlay; no entries appended by any of these. Share: gzip + AES-256-GCM (`[12B IV][ct+tag]`), store `blob` (default share server, 1 MB cap; trim inline images → long strings 32K→512B → oldest entries) or `gist` (needs `gh`, 5 MB sealed, `session.ompshare.txt`), link `<server>/<id>#<b64url-key>` — key lives only in URL fragment, client-side decrypt; typed redaction pass default on (`share.redactSecrets`); opaque replay/extension payload fields dropped. Custom share handler `~/.omp/agent/share.{ts,js,mjs}` default-export `(htmlPath)=>url|{url,message}` overrides TUI flow; load/exec failure = error, **no fallback** to default; headless always default flow; works for `--no-session` (live entries); abort is UI-level only, upload not killed.
**Surface**: `/dump /export /share`, `--export`; settings `share.store`, `share.serverUrl`, `share.redactSecrets`.
**Go port**: HTML = stdlib html/template with simple pre-rendered tool cards (skip React renderer generation); crypto = crypto/aes + compress/gzip; share server optional blob POST.
**Parity**: dump CORE (debug aid); export NICE; share NICE (needs hosted viewer — keep-optional).

## 5. Session ops — lifecycle (new/fresh/clear/drop/fork/resume/continue/restart)
**Semantics** (session-operations-export-share-fork-resume.md):
- `/new`: new session identity + transcript path. `/fresh`: rotate **provider-facing state only** — close cached provider sessions, mint new provider session id, re-key memory, invalidate append-only ctx (next turn resends full transcript); transcript/file/header untouched; rejected while streaming.
- `/clear`: reset in place (drops live msgs, queues, pending tools, checkpoint/rewind, continuation; rotates provider state; re-primes advisor) + durable `reset_boundary` entry; transcript JSONL keeps pre-reset history; TUI-only; blocked while streaming/foreground exec; aborts active compaction first.
- `/drop`: best-effort delete session JSONL + artifacts (not a guaranteed erasure boundary), then new session.
- `/fork`: new file; header rewritten (new id/ts, `parentSession` set, cwd kept); `providerPromptCacheKey` inherited; artifacts dir copied best-effort; blocked while streaming; non-persistent ⇒ fails. `--fork <id|path>` startup: prefix resolve → fork into current scope; cache-key inheritance **dropped** if `--model/--thinking/--system-prompt/--append-system-prompt/--tools/--no-tools` change prompt/tool shape.
- `--continue`: breadcrumb-first (`~/.omp/agent/terminal-sessions/<terminal-id>`; id from TTY path or `ZELLIJ_PANE_ID TMUX_PANE CMUX_SURFACE_ID KITTY_WINDOW_ID WEZTERM_PANE TERM_SESSION_ID WT_SESSION`); fresh-marker `fresh` on missing target = start new; nested subagent breadcrumbs walk up ≤8 to parent; re-root into cwd when recorded dir gone and cwd has no own sessions; else cwd-matching breadcrumb; else newest mtime; else create. `autoResume` setting reuses same resolution. `--continue <UUID>` normalized to `--resume`.
- `/restart`: relaunch with original launch flags, resume in place.
**Go port**: JSONL append + header rewrite; breadcrumb = file keyed by TTY/pane env; `reset_boundary` = entry type; `fresh` = provider-session-id rotation.
**Parity**: /new /fork --fork /fresh /clear --continue CORE; /drop NICE; /restart NICE; autoResume NICE.

## 6. Session switching & recent listing
**Semantics** (session-switching-and-recent-listing.md):
- Storage `~/.omp/agent/sessions/<encoded-cwd>/*.jsonl` (path-encoded cwd bucket: `-rel` under home, `-tmp-rel` under temp, `--abs--` otherwise).
- Two listing pipelines: cheap 4 KiB-prefix `RecentSessionInfo` (welcome) vs `SessionInfo` (4 KiB prefix + 32 KiB tail; id/cwd/title/parent/dates/size/previews/status complete|interrupted|aborted|error|pending; mtime desc; stat-cached scan, bounded parallel workers; orphaned `.bak` repaired).
- Display name: title → first user msg → `Untitled · <time>`; raw id never used; sanitization (first line, control chars stripped).
- ID resolution: case-insensitive startsWith on session id / full JSONL filename / id suffix after `<timestamp>_`; first match in mtime-desc order, no ambiguity UI. Cross-project match opens in recorded project (process cwd + scoped settings reloaded), not fork; if recorded dir gone: interactive re-root prompt (`[Y/n]`), non-TTY errors.
- Picker: alternate-screen TUI; Tab toggles folder/all-projects scope (all-projects lazy, **never auto-switched**); multi-token search + history.db fuzzy promotion; Delete/empty-Backspace confirm-delete; mouse wheel/click.
- Runtime `switchSession`: cancellable `session_before_switch` hook (reasons new|fork|resume) → abort work → full rollback snapshot → set file → rebuild (messages, model/thinking/tiers restore, memory/tool reset) → `session_switch` event → reconciler (re-enter plan mode etc.); mid-throw = full state restore + rethrow; cross-project switch without cwd-change callback rejected; ENOENT/invalid header = new empty session at that path (recovery, not failure).
**Surface**: `--resume [id]`, `--continue`, `/resume [id|@claude|@codex]` (`@claude/@codex` = foreign import picker), `omp share`.
**Go port**: direct (JSONL scan; history.db analog optional SQLite).
**Parity**: CORE = resume/continue + basic picker + switch runtime + hooks; NICE = foreign import, fuzzy/history search, pinned markers.

## 7. Session tree — /tree, /branch, branching model
**Semantics** (session-tree-plan.md, tree.md):
- Append-only entries carry `id`/`parentId`; `leafId` = active position; appends become children of leaf; branching never rewrites history. Primitives: `branch(id)` (move leaf, no write), `resetLeaf()` (null leaf → next append is new root), `branchWithSummary(id, summary)` (moves leaf + appends `branch_summary` at the **new** position).
- `/tree` (`navigateTree`): user-message select → leaf = its parent + editor draft restore (text + image attachments); custom/skill-prompt or other entries → leaf = entry id; ask-result re-open appends sibling toolResult. Rebuild context from new leaf; reset branch-scoped todos/advisor/checkpoints; emit `session_before_tree` / `session_tree`.
- `/branch`: source must be a **user message**; root selection → newSession w/ `parentSession` + inherited title; else `createBranchedSession(parentId)` = copy root→leaf path to new file (exclude `label` entries, regenerate from label map). Non-persistent = in-memory replacement.
- `doubleEscapeAction` (`rewind` default | `tree` | `none`): double-Esc on empty editor opens rewind/tree selector; `tree` makes the `/branch` registry entry reuse `navigateTree` (same-file navigation).
- Tree UI: timestamp-sorted children; active path bullet; labels `[label]` (Shift+L set/clear; labeled-only filter `Alt+L`); filters default→no-tools→user-only→labeled-only→all (Ctrl+O cycle, Alt+D/T/U/L/A direct; default hides label/custom/model_change/thinking_level_change); tool-only assistant messages hidden in every mode unless leaf or error/aborted; search-as-you-type; `max(5, floor(height/2))` rows; Shift+Enter = summarize+switch directly.
- Auto session naming: humanize plan title (`migrate-mcp-loader` → `Migrate mcp loader`), only when unnamed (`auto` source never overwrites user names); title normalization contract (first line, strip quotes/`<title>`, `none` ⇒ unnamed, >80 chars or >12 words rejected).
**Go port**: parent-pointer tree over JSONL — trivial; UI = list selector; context rebuild = root→leaf replay.
**Parity**: id/parentId/leaf data model CORE (cheap, enables fork/rewind); `/tree` selector NICE; labels/filters NICE; `/branch` NICE (--fork covers most need).

## 8. Context files (AGENTS.md hierarchy)
**Semantics** (context-files.md):
- Discovered Markdown → opening project prompt as `<repo-rules>` with `<file path>` children (default template). Provider registry priorities: native 100 > omp-plugins 90 > claude 80 > agent-plugins 75 > agents/claude-plugins/codex 70 > gemini 60 > opencode 55 > cursor/windsurf 50 > cline 40 > github 30 > vscode 20 > agents-md/claude-md 10 > mcp-json/ssh-json 5 > builtin-defaults 1.
- Native: `~/.omp/agent/AGENTS.md` + `<nearest-non-empty>/.omp/AGENTS.md` walking cwd→repo root — nearest non-empty `.omp/` owns discovery; missing file does NOT continue upward; empty files contribute nothing. Others: `.claude/CLAUDE.md` (cwd config dir only, no walk-up), `~/.codex/AGENTS.md` (user-only), `.gemini/GEMINI.md` (cwd only), `~/.config/opencode/AGENTS.md` (user-only), `.github/copilot-instructions.md` + `~/.copilot/…` (`COPILOT_HOME`, `COPILOT_CUSTOM_INSTRUCTIONS_DIRS`), `.agent/.agents/AGENTS.md` (walk-up), standalone `AGENTS.md`/`CLAUDE.md` (walk-up to repo root + workspace dirs below home).
- Dedup/shadowing: one user file total (native wins); one project file per directory depth (config subdirs same depth as ancestor; higher priority wins at that depth); across depths multiple survive; byte-identical collapsed (closest to cwd survives). Injection order: farther ancestors first → cwd-nearest last (most prominent) → user file last.
- `@path` imports: relative to importing file, `~/` to home; left literal inside code fences/spans; `git@…`/email tokens never imported (`@` must start line or follow space/tab); trailing punctuation trimmed; depth ≤5; cycles skipped; missing target left literal.
- Deeper non-loaded `AGENTS.md` → `<dir-context>` pointer block ("read before editing").
- `RULES.md` sticky: only native locations (user agent dir + nearest non-empty project `.omp/`); loaded as **always-apply rule** re-attached near current turn (survives long conversations); frontmatter cannot unstick; both synthesized as rule name `RULES` — user shadows project (not concatenated).
- Disabling: `disabledProviders` = whole source (also drops its MCP/commands/skills/hooks/settings; namespace shared with model providers — `google` ≠ `gemini`); path-scoped entries `{path|paths|pathPrefix, providers}`. `disabledExtensions` id `context-file:<user|project>:<basename>` = single file (project id hits every depth; disabled file doesn't claim scope → shadowed file loads).
**Go port**: file walking + dedup rules, stdlib only.
**Parity**: CORE = native `.omp` + standalone AGENTS.md/CLAUDE.md + `.github/copilot-instructions.md` + `@` imports + depth dedup; NICE = remaining third-party providers, per-file disable, path-scoped disabling, RULES.md stickiness (needs rule re-attachment machinery).

## 9. System prompt customization / roles
**Semantics** (system-prompt-customization.md):
- Inputs: `SYSTEM.md` (switches to bundled custom template — keeps generated context files/skills/rules/append + project footer, drops default tool/workflow/personality guidance) and `APPEND_SYSTEM.md` (append text); flags `--system-prompt` / `--append-system-prompt` take text-or-file (single-line value: try file path first, else literal; newline ⇒ literal) and win over files; SDK `systemPrompt` = full replacement (CLI never sets it).
- Discovery: project-first then user; config-base order `.omp → .claude → .codex → .gemini`; **no ancestor walk**; profile relocates user base. Append placement: end of project footer (no SYSTEM.md) or right after custom text (with SYSTEM.md).
- Per-request bytes (date/cwd) moved out of system prompt into first-turn `<system-reminder>` (prefix-cache friendly across midnight/provider tool-schema layouts).
- Plain-text contract: user content inserted verbatim into Handlebars templates — no user-facing templating.
- `TITLE_SYSTEM.md` overrides title-gen prompt (same discovery); output normalization: first line, strip quotes/`<title>`/punctuation, `none`/`<title/>` = no title, >80 chars or >12 words rejected.
- `PERSONALITY.md` (user agent dir only) replaces selected `personality` preset text (`default|friendly|pragmatic|none`; subagents always `none`; empty/unreadable → preset + warning).
**Go port**: text/template with two bundled templates + shared config-file discovery helper.
**Parity**: APPEND_SYSTEM.md + flags CORE; SYSTEM.md CORE; TITLE_SYSTEM.md NICE; PERSONALITY.md NICE.

## 10. Settings & config layering
**Semantics** (settings.md, config-usage.md):
- Layers lowest→highest: schema defaults ← global `~/.omp/agent/config.yml` (`.yaml` compat; legacy `settings.json` + agent.db migrated once) ← project `<cwd>/.omp/settings.json` then `config.yml` (cwd's `.omp/` only — **no ancestor walk**; empty `.omp/` ignored; other discovery providers contribute read-only project settings) ← `--config` overlays (repeatable, later wins; missing/invalid/non-mapping = hard error) ← runtime overrides (CLI flags + feature env vars, never persisted). `PI_CONFIG_FILES` path-list loads before `--config`.
- Merge: objects deep-merged; **scalars and arrays replaced wholesale** (project array must restate complete list). Top level must be a mapping; invalid persistent file → `.broken-*` backup + exit.
- `omp config list|get|set|reset|path|init-xdg` — schema-typed value parsing (bool/number/enum/array-JSON/record-JSON/string), exact schema paths only; creds masked in `list`, unmasked in `get`; `reset` persists the default (doesn't delete).
- Profiles: `--profile <name>` / `OMP_PROFILE` (wins, even empty) / `PI_PROFILE` relocate the entire native user base (config, sessions, agent.db, rules, prompts…); external-tool bases (`~/.claude` etc.) and project dirs not profile-scoped; keybindings merge default+profile.
- Path-scoped arrays: `enabledModels`/`enabledProviders`/`disabledProviders` accept `{path|paths|pathPrefix|pathPrefixes, models|providers|values|items}`; apply when cwd equals/under path; resolved after layer merge. `disabledProviders` = one shared namespace for model backends AND discovery sources. `enabledProviders` gates foreign user-level sources (default empty; `*`/`all` enables).
- Notable groups: `modelRoles` (default/smol/slow/vision/plan/commit/tiny/task/advisor + custom; `:thinking` suffix), `modelRoleStorage` global|project, `cycleOrder`, `tools.approvalMode` (always-ask|write|yolo) + `tools.approval` record + `bash.patterns` (`*` wildcards; compound `&&` opt-in `bash.allowCompoundCommands`; deny>prompt resolution), `bashInterceptor`, compaction.* (`methodOrder remote,snapcompact,handoff,shake,soft`), `memory.backend` (off|local|hindsight|mnemopi), `retry` + `fallbackChains` (role / `provider/model` / `provider/*` keys), thinking budgets, `steeringMode`/`followUpMode`/`interruptMode`, `plan.enabled`/`plan.defaultOnStartup`, `autoResume`, `doubleEscapeAction`, `extendedContext`, themes/statusLine/tui.*.
**Go port**: YAML lib + deep-merge + schema-driven get/set; direct port of precedence engine.
**Parity**: CORE = layering engine, global+project config, `omp config` CLI, approval policy, modelRoles, thinking, interaction modes; NICE = profiles, path-scoped arrays, XDG, legacy field migrations, provider-order/fallback chains (v2).

## 11. Environment variables
**Semantics** (environment-variables.md):
- `$env` lookup: process env → project `.env` → agent `~/.omp/agent/.env` → config-root `~/.omp/.env` → `~/.env` (each fills only unset keys; name must be shell identifier; values literal). Inside each dotenv file, every `OMP_*` key mirrored to its `PI_*` alias, mirror replacing same-file `PI_*` value.
- Groups: provider creds (~60 vars: `ANTHROPIC_OAUTH_TOKEN`>`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, aliases like `KIMI_API_KEY`→`MOONSHOT_API_KEY` fallback); auth broker `OMP_AUTH_BROKER_URL/TOKEN/SNAPSHOT_*`; proxy chain `PI_PROXY_<PROVIDER>` > `PI_PROXY` > `HTTPS/HTTP/ALL_PROXY` (+`NO_PROXY`; `PI_PROXY` is process-wide, provider-scoped covers only that provider); Foundry/Bedrock/Vertex/Azure cloud sets; model roles `PI_SMOL_MODEL`/`PI_SLOW_MODEL`/`PI_PLAN_MODEL` (=`--smol/--slow/--plan`); toggles `PI_NO_PTY PI_NO_TITLE PI_PY PI_JS PI_TINY_DEVICE/DTYPE PI_CONFIG_DIR PI_CODING_AGENT_DIR PI_CODING_AGENT_SESSION_DIR PI_CONFIG_FILES PI_PROFILE/OMP_PROFILE PI_TASK_MAX_OUTPUT_BYTES/LINES PI_WALK_WORKERS PI_SUBPROCESS_CMD`; debug `PI_TIMING PI_DEBUG_STARTUP`; shell `PI_BASH_NO_CI/PI_BASH_NO_LOGIN/PI_SHELL_PREFIX` (+`CLAUDE_*` aliases when unset); TUI `PI_NOTIFICATIONS PI_FORCE_IMAGE_PROTOCOL PI_HARDWARE_CURSOR PI_TUI_*`; terminal breadcrumb ids (see §5); storage `OMP_WORKTREE_DIR OMP_GITHUB_CACHE_DB`; OTEL export group.
- Precedence nuance: env vars are per-feature overrides, not a settings layer; flags > env for model roles.
**Go port**: ~50-line dotenv loader + table-driven per-feature lookups.
**Parity**: CORE = dotenv layering, model-role vars, PI_NO_PTY/NO_TITLE, config-dir/profile/session-dir vars, proxy chain, terminal-id set; NICE = cloud/foundry/debug/provider-quirk toggles; SKIP = TTS/STT/image-protocol/browser-relay vars (no matching xdev features).

## 12. Parity matrix (PRD/issue ingestion)
| Feature | Class | First-cut scope |
|---|---|---|
| Slash commands (native markdown) | CORE | discovery (project+user `.omp/commands`) + expansion + autocomplete |
| Multi-provider command discovery | NICE | claude/codex/opencode paths + toggles |
| CLI launch core + headless -p/--mode json | CORE | flags in §2 |
| Keybinding remap layer | CORE | YAML + ~20 core actions |
| /dump | CORE | formatSessionAsText + sidecar |
| /export HTML, /share | NICE | template export; share keep-optional (crypto stdlib) |
| /new /fresh /clear /drop /fork --fork | CORE | lifecycle in §5 |
| --continue + breadcrumb + terminal ids | CORE | |
| --resume + picker + switchSession runtime | CORE | basic list picker; fuzzy/history search NICE |
| Session tree data model (id/parentId/leaf) | CORE | enables fork/rewind/branch |
| /tree /branch UI, labels, filters, rewind | NICE | after tree model |
| Context files: native + AGENTS.md + imports | CORE | walk + depth dedup + `@` imports |
| Third-party context providers, per-file disable | NICE | |
| RULES.md sticky rules | NICE | needs rule re-attachment |
| SYSTEM.md / APPEND_SYSTEM.md + flags | CORE | |
| TITLE_SYSTEM.md / PERSONALITY.md | NICE | |
| Settings layering + `omp config` CLI | CORE | §10 engine |
| Profiles, path-scoped arrays, XDG | NICE | v2 |
| Env-var framework + core vars | CORE | §11 |

[You have received this identical output 3 times. Re-reading 'agent://ScoutSessionUX?q=.report' will not change it — use a narrower selector (path:A-B), or proceed with the edit.]