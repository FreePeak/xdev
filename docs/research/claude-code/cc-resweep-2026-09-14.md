# Claude Code v2.1.263 — full feature re-sweep for xdev (2026-09-14)
Exhaustive sweep of Claude Code (CC) feature surface beyond the 2026-09-09 study (cc-official-docs.md), run as three workflow rounds over 10 baseline domains + a completeness critic + 4 critic follow-up domains (auth/providers/gateway, telemetry/cost/ops, TUI-input surface, model/effort/advisor, agents/teams/workflows, slash-CLI-TUI, changelog-net), with an adversarial dedup-verify stage per domain: every candidate checked against `gh issue --search` over all 127 xdev issues, PRD §2/§5 baselines, and the Go tree for shipped implementations. Sources: code.claude.com/docs (live pages incl. llms.txt index), the v2.1.0–2.1.270 CHANGELOG, `grep -a` forensics on the installed ~/.local/share/claude/versions/2.1.263 binary, and real ~/.claude/projects transcripts. CC 2.1.263 is current-minus-7 (upstream 2.1.270); version-gated facts flagged inline. The gateway killed several emit stages mid-sweep; payloads were recovered from agent transcripts and re-verified, and per-domain notes here record residual coverage caveats.
Outcome: 197 verified gap drafts -> curated into 112 issues (42 clusters + 70 single) with scope-extension comments on 5 open issues (#91/#92/#105/#117/#123 families). Filed issue numbers are linked per row; the issue bodies carry the portable contracts (names/defaults/limits) verbatim.

## Per-domain record

### plugins-marketplaces — 10 confirmed gaps; 24 dup/rejected; 2 covered; 3 rejected-at-research
Method/caveats: Coverage: fetched all 8 plugin-related slugs from the docs map (plugins, plugin-marketplaces, plugins-reference, discover-plugins, plugin-dependencies, plugin-hints, plugin-relevance, plugin-evals) plus the live 2.1.263 CLI (--help for plugin, marketplace, eval, eval init, list, details, validate, tag). plugins-reference.md truncated twice on fetch — version-management chain reconstructed from marketplace.md §13 + plugin-dependencies.md (`{name}--v{version}` tag, confirmed by `claude plugin tag --help`); the plugins-reference 'Debugging and development tools' + output-styles/standalone sections were only partially captured. plugin-evals.md now LIVE on code.claude.com (contradicts the embedded reference's 'no public docs page yet' — the docs caught up), but per the embedded offline reference `claude plugin eval` is still early-access/NOT-enabled in THIS session, AND the live doc requires…
**Confirmed gaps (filed):**
- [#152] plugin.json manifest schema [CORE/M13] — [M13] Claude manifest compatibility: plugin.json + marketplace.json field set, strict merge, component roots
- [#152] component namespacing /plugin:name [NICE/M13] — [M13] Namespaced plugin components: <plugin>:<skill|command|agent> at registration
- [#153] marketplace plugin-entry source types [CORE/M13] — [M13] Plugin source types: archive (HTTPS zip) + git-subdir sparse; download guards (redirect SSRF, sha256, 256 MiB)
- [#153] interpolation variables (PLUGIN_ROOT/DATA/PROJECT_DIR) [CORE/M13] — [M13] Plugin env contract: XDEV_PLUGIN_ROOT/DATA/PROJECT_DIR export + command-string interpolation
- [#154] monitors.json (background monitors) [NICE/M15] — [M15] Plugin monitors: declarative background watchers feeding stdout lines as events
- [#135] channels field (message injection) [DEFERRED/M15] — [M15] Decision record: plugin channels (MCP-server-backed message injection)
- [#154] plugin dependencies (plugin→plugin) + release tags [NICE/M15] — [M15] Plugin dependencies: semver ranges, {name}--v{version} tags, bundles, cross-marketplace allowlist
- [#152] per-plugin scopes + enabledPlugins + extraKnownMarketplaces [CORE/M13] — [M13] Plugin enable/disable state + install scopes: plugins.enabled keying plugin@marketplace
- [#152] /reload-plugins + prompt-cache interaction [NICE/M13] — [M13] /reload-plugins: re-scan plugin roots mid-session and report counts
- [#153] plugin trust posture + env scrubbing [NICE/M13] — [M13] Strip credential-shaped env vars for project- and plugin-sourced helper commands

**Cross-checked away:**
- plugin directory layout :: DUPLICATE :: [M13] Claude manifest compatibility (this batch, candidate 'plugin.json manifest schema') Layout facts (root-level components, .claude-plugin/ manifest-only, extra roots, single-skill-at-root) are one bullet-set of the manifest-compat issue. xdev already dete…
- --plugin-dir / --plugin-url dev loading :: IMPLEMENTED :: xdev ships repeatable --plugin-dir with real consumers (cmd/xdev/main.go:211 → marketplace.SetExtraRoots → all four discovery paths; roots_consumed_test.go proves wiring; PRD §4 #105 residual marked landed). CC deltas checked against plugins.md: precedence-ov…
- skills-directory plugins (@skills-dir) :: DUPLICATE :: [M13] Per-plugin enable/disable state + scopes (this batch) Verified in plugins-reference.md + plugins.md (init scaffolds ~/.claude/skills/<name>/, loads as <name>@skills-dir next session, project scope trust-gated, MCP per-server approval, monitors skipped).…
- marketplace.json schema :: DUPLICATE :: [M13] Claude manifest compatibility (this batch, candidate 'plugin.json manifest schema') Required name/owner/plugins[], $schema-ignored, metadata.pluginRoot, replace-on-re-add, marketplace-root-relative paths — all verified verbatim and folded into the compa…
- strict mode (component authority) :: DUPLICATE :: [M13] Claude manifest compatibility (this batch, candidate 'plugin.json manifest schema') Verified verbatim (true = supplement+merge; false = entire definition, conflict fails load; headersHelper requires false). It is a merge-rule row of the compat issue's v…
- userConfig + plugin settings interpolation :: DUPLICATE :: [M13] Plugin env contract (this batch, candidate 'interpolation variables') Type/option schema and CLAUDE_PLUGIN_OPTION_<KEY> env export are the same subprocess-env contract; the load-bearing safety facts (shell-form commands reject ${user_config.*}; project-…
- plugin settings.json (agent + subagentStatusLine) :: DUPLICATE :: [M13] Per-plugin enable/disable state + scopes (this batch) Verified: plugin-root settings.json applies defaults when enabled; only `agent` (activates a plugin agent as the MAIN thread) + `subagentStatusLine`; beats plugin.json's settings block; unknown keys …
- output styles as plugin component :: REJECTED :: PRD §2 rows: SYSTEM.md/APPEND_SYSTEM.md (line 87), Theme engine (line 109) The packaging fact (outputStyles{} manifest field, output-styles/*.md applied while enabled, official examples) is verified but moot: xdev ships no output-style feature to package — pe…
- experimental components (themes, monitors, evals) :: DUPLICATE :: [M13] Claude manifest compatibility (this batch, candidate 'plugin.json manifest schema') Verified (themes/monitors/evals under experimental{}; top-level still works but validate warns and a future release requires the nesting; non-object experimental = ignor…
- claude plugin tag (release-tagging verb) :: DUPLICATE :: [M15] Plugin dependencies (this batch) The tag verb exists only to produce the {name}--v{version} dependency convention; flags verified from the 2.1.263 binary and folded into the deps issue.
- version resolution / pinning :: DUPLICATE :: #85 (plugin verb set incl. `update` semantics) Verified: resolved version is the cache key — equal version ⇒ /plugin update and auto-update SKIP; a declared version pins every source type except command; git sources without version fall back to the commit SHA…
- marketplace renames map :: DUPLICATE :: [M13] Claude manifest compatibility (this batch, candidate 'plugin.json manifest schema') Verified (renames{} v2.1.193+; old-name key followed once, notice printed, enabledPlugins+pluginConfigs keys rewritten in user/project/local; null = removed; plugin-cach…
- auto-update configuration :: DUPLICATE :: #85 (marketplace.autoUpdate absent — named in its own evidence list) Verified deltas worth porting when the verb lands: background refresh after startup with a random delay ≤10 min (running session keeps launch versions); enabled by default ONLY for official …
- /plugin UI tabs :: ALREADY-DOCUMENTED :: PRD §2 line 148 (Marketplace-compatible plugin manager row, NICE M13) Verified (Discover/Installed/Marketplaces/Errors/Stats; details pane = context-cost estimate + last-updated + will-install inventory; problems-first sort, favorites, Not-used-recently after…
- claude plugin CLI verbs (full surface) :: DUPLICATE :: #85 ('Gap-fill the plugin verb set' — its open scope item) Verb inventory re-verified verbatim against the local 2.1.263 binary: details, disable[-a,--scope,--json], enable[--scope,--json], eval, init|new, install|i[-s/--scope,--config k=v,-y], list[--availab…
- claude plugin validate :: DUPLICATE :: #85 A verb of the same set. Distinct semantics worth folding into #85's validate bullet: warnings-not-errors on unrecognized fields with near-miss suggestions; wrong-type keywords = error; --strict → exit 1 for CI; works on a bare skills/agents dir (v2.1.233+…
- plugin cache layout + orphan grace :: REJECTED :: Verified (cache/<marketplace>/<plugin>/<version>/ per-version dirs; update/uninstall orphans the old dir, background sweep ~14 days later so concurrent sessions keep running; sweep only while ≥1 plugin installed; symlinked dev checkouts never orphaned; Glob/G…
- command source + per-run acceptance :: REJECTED :: Verified in full (one stdout line = absolute plugin dir, exit 0; printable ASCII ≤500 chars no 4-space runs; timeout 60/600s; copy vs link modes with 256MiB/20k-entry caps; refuses cwd/parent, no top-level plugin content, UNC; explicit user acceptance of the …
- private-repo auth + CI/CD + seed dirs :: REJECTED :: Verified (interactive add/install/update use git credential helpers, background pull disables them and falls back to re-clone, KEEP_MARKETPLACE_ON_FAILURE, SSH needs known_hosts+agent; CLAUDE_CODE_PLUGIN_SEED_DIR layered read-only seed with known_marketplaces…
- reserved marketplace names :: REJECTED :: Verified (17-name Anthropic list + impersonation variants; re-checked on EVERY load, not just add; a newly-reserved name stops loading and reports 'registered from an untrusted source'). The list itself is pure Anthropic brand surface; the only portable piece…
- plugin recommendation hint (CLI→CC) :: REJECTED :: Verified (stderr `<claude-code-hint v="1" type="plugin" value="name@marketplace" />`; gate on CLAUDECODE=1 / CLAUDE_CODE_CHILD_SESSION=1; CC strips it before the model, prompts only for OFFICIAL-marketplace plugins, once per plugin+session, interactive-only, …
- plugin relevance suggestions (org) :: REJECTED :: Verified limits (relevance{}: topic ≤64 chars; signals cwd/cli/hosts ≤20×128/filesRead ≤10×256/manifestDeps globs+regexes; matching local, no telemetry; gated behind pluginSuggestionMarketplaces admin allowlist; throttled 1-per-3-sessions, ≤2 per session star…
- plugin eval harness (claude plugin eval) :: REJECTED :: Draft's own evidence disqualifies it: the docs REQUIRE v2.1.269 while this study covers 2.1.263 — the binary's `plugin eval --help` exists but the feature is early-access/gated, so the contract is not stable parity material. Also correctly flags it must NOT b…
- /skill-doctor (skill usage + context cost) :: REJECTED :: Verified via discover-plugins Stats tab ("what each of your skills costs in context and how often it gets used", 7-day window, never-invoked warnings) — but the substrate is missing on xdev's side: no per-skill usage counter exists (grep SkillUsage/UsedAt → z…

**Already covered (traceability):**
- skills/hooks/agents/MCP bundled by plugins (component semantics) -> Component semantics live in cc-official-docs.md §2 hooks, §3 subagents, §4 skills, §9 MCP; §5.1 adopted list already has hooks exit-2, subagent isolation, MCP naming. Only…
- output styles / themes component contracts -> PRD line 59: theme engine is M12 CORE; only the plugin-packaging angle is new (see 'output styles as plugin component').

**Rejected with reasons:**
- managed marketplace restrictions (enterprise policy) :: Pure Anthropic MDM/managed-settings platform surface for enterprise org control; §1 non-goals exclude permission theater + central policy. Rejected unless xdev ever ships an admin-con…
- Node.js package dependency auto-install :: §1 explicitly bans a JS/Bun runtime and in-process plugin VM; xdev extensions are Go subprocesses. The npm-source plugin itself is likewise off-scope. Rejected by non-goal.
- /install-slack-app and /install-github-app :: Domain scoping: app-installers ≠ plugin packages. Claude Tag/GitHub App are separate cross-check domains; M15 platform surface, out of xdev scope.

### platforms-integrations — 9 confirmed gaps; 1 dup/rejected; 9 covered; 8 rejected-at-research
Method/caveats: Sweep coverage: all 13 brief slugs fetched and mined except none 404ed; additionally followed in-domain links into platforms, deep-links, channels-reference, scheduled-tasks, routines, ultrareview, gitlab-ci-cd, github-enterprise-server, desktop-scheduled-tasks. Deliberately NOT deep-swept (adjacent domains, thin relevance here): cloud-environments internals (network levels/allowlist rows), self-hosted-environments + testing, computer-use (M15 row #65 already tracks), worktrees, feature-availability matrix, analytics/monitoring-usage, llm-gateway-connect, claude-apps-gateway, GitHub-Actions-cloud-providers (bedrock/vertex/foundry input details summarized from the github-actions page link, the dedicated page not fetched), the full claude-code-action repo docs (usage.md inputs list beyond those the docs page names — only the doc-listed inputs are asserted; `allowed_bots`/`allowed_non_writ…
**Confirmed gaps (filed):**
- [#170] claude-code-action workflow contract (GitHub Actions) [NICE/M13] — [M13] Publish CI headless contract: GitHub Actions + GitLab job recipes for xdev -p
- [#146] /ide command + built-in IDE MCP server (ws lockfile protocol) [NICE/M14] — [M14] IDE attach contract: lockfile-discovered loopback ws MCP bridge with getDiagnostics feed
- [#146] Deep links: claude-cli:// handler + vscode:// open/install-plugin URLs [NICE/M14] — [M14] xdev-cli:// deep-link handler: q/cwd/repo grammar + OS registration + disable key
- [#171] --teleport cross-device session handoff [DEFERRED/M15] — [M15] Teleport-shaped handoff: pull a remote session + branch onto a clean checkout
- [#132] .claude/launch.json preview contract [NICE/M13] — [M13] Adopt a launch.json-shaped run/preview contract for the browser + verify flows
- [#162] /loop + CronCreate/CronList/CronDelete scheduling contract [NICE/M11] — [M11] Session-scoped scheduler: cron tools + /loop with jitter, expiry and loop.md
- [#135] Channels contract (MCP push events + permission relay) [NICE/M14] — [M14] Channels: inbound push events over MCP/ext protocol + relayed approval verdicts
- [#132] Browser tool vocabulary + plan-mode gating [NICE/M13] — [M13] Browser tool contract: readonly/state-changing split with per-call and batch gating
- [#172] Claude Security / security-review [NICE/M12] — [M12] Bundle a security-review skill: LLM checklist over pending branch diffs, advisory only

**Cross-checked away:**
- /install-github-app quick-setup flow :: DUPLICATE :: same-batch CI issue ([M13] Publish CI headless contract) — folded in as its optional gh-script provisioning slice Verified against github-actions.md §Quick setup (github.com-only, gh prereq, secret naming, workflow branch + browser PR, Update workflow file op…

**Already covered (traceability):**
- Local /code-review command -> Repo already ships the equivalent as a skill (this workspace's /code-review: Standards+Spec two-axis review, parallel sub-agents) — skills M12 covers the mechanism; no compiled command needed.
- claude setup-token / CLAUDE_CODE_OAUTH_TOKEN -> xdev M9 credential chain already covers long-lived OAuth tokens (PKCE + refresh + stored OAuth); token-scoping caveat (narrow tokens can't authorize non-model surfaces) is worth a doc note on…
- VS Code extension surface -> xdev's equivalent editor surface is ACP (#100) + RPC (#60) — the GUI itself is a proprietary client of the same engine; covered by §2 ACP row (M14). Portable contract fragments live in the /ide, deep-links and …
- JetBrains plugin surface -> Terminal-runner surface; xdev is terminal-native and ACP covers embedding. Its only portable mechanism is the shared /ide contract (next finding).
- Remote Control (server mode, flags, relay transport, push) -> xdev's equivalent surface = collab (E2E AES-256-GCM WS relay, host-authoritative, #59/#99 open incl. guest-input arbitration) + RPC host bridge (#60). Mechanism differs (Anthrop…
- Desktop scheduled tasks (file-backed durable scheduler) -> Contract details folded into the scheduling new-gap below (durable file-backed tier with SKILL.md prompts is one of its acceptance bullets); Desktop UI itself rejected above.
- Chrome integration contract (native messaging host + browser tool gating) -> The browser-extension half is exactly residual #112 (open: Go relay daemon shipped, browser-side extension missing) — mark covered, but post into #112: the native…
- GitHub Enterprise Server support -> GHES itself is a hosted-feature concern; the xdev-relevant keys are `extraKnownMarketplaces` (already configured here, maps to M13 marketplace-compat row) and `strictKnownMarketplaces.hostPattern` — wort…
- Platform surface taxonomy (where each integration runs) -> Meta-finding: the mechanism split above is what §2 parity rows should cite when mapping remaining platform issues (#59/#99/#60/#100/#112) — no new work beyond the individual gaps.

**Rejected with reasons:**
- Claude Code on the web (cloud sessions, --cloud, CCR bundle rules) :: Proprietary Anthropic-hosted compute (per-session VMs, claude.ai auth, org policy) — §1: xdev containment is external sandboxing and hosting is the user's (§2 "you host"…
- Auto-fix pull requests :: Tied to cloud sessions + the Claude GitHub App (proprietary). Portable mechanism — event-driven wakeups into a live session — is exactly what the channels finding (#16 below) and hub jobs cover; /web-setup (ships …
- Routines (cloud scheduled/API/GitHub-triggered sessions) :: Anthropic-hosted scheduler+webhooks = cloud service (xdev analog deferred to M15 ecosystem rows). Two conventions worth stealing into the channels/scheduling issues: the `<routine…
- Code Review (managed PR-review product) :: Managed cloud service. Portables noted: the severity triple + JSON check-run trailer for xdev CI review flows, and `REVIEW.md` as a review-scope guidance file (trivial to honor inside the existing…
- Ultrareview (/code-review ultra, claude ultrareview) :: The remote sandbox is the product; xdev's /batch + review skills cover the local half. Worth stealing: the `--json`-on-stdout / progress-on-stderr split, the 45-min default timeout, a…
- Desktop app (Code tab, environments, Dispatch, computer use) :: Proprietary GUI (Electron) — conflicts with §1 single-binary/TUI and RSS budget; its portable seams are split out as launch.json (below), file-backed scheduler (folded into sc…
- Slack app (per-user Claude Code in Slack) :: Slack→cloud-session plumbing is entirely Anthropic-side; Claude Tag (org identity, per the task's own docs pointer) is a different domain. xdev shape for chat-app entry points is Channels above.
- Mobile app (Code tab, Dispatch, attachments, push) :: Client app + cloud pairing (Trusted Devices, push tokens, 18h biometric freshness) are proprietary surface; the portable bits (push on decision-needed, dialog expiry) ride the Remote Co…

### sessions-mcp — 16 confirmed gaps; 3 dup/rejected; 4 covered; 3 rejected-at-research
Method/caveats: Coverage: fetched sessions.md, mcp.md (x4 attempts — page exceeds fetcher window), checkpointing.md, headless.md, cli-reference.md (flags through ~--session-id; tail truncated), commands.md, settings-reference.md (key table; per-key blocks truncated), interactive-mode.md (saved 87.7KB extract at /Users/linh.doan/.claude/projects/-Users-linh-doan-work-harvey-freepeak-xdev/801a282d-4ccf-40f9-ac25-a78438f8d2df/tool-results/call_4b9bc94b489c4d4186eacd23.txt), docs map, CHANGELOG (only 2.1.250–2.1.270 visible in the fetch window), plus binary grep of ~/.local/share/claude/versions/2.1.263 and measurement of TWO real transcripts (801a282d…, 3688a02c… + c908aaaa dir) and their sidecar trees. UNVERIFIED / gaps: (1) mcp.md sections 'Use MCP prompts as commands', 'Use/Reference MCP resources', 'Respond to MCP elicitation requests', sampling and output-style-adjacent prose never arrived verbatim d…
**Confirmed gaps (filed):**
- [#158] Resume entry points and --continue exclusion rules [NICE/M10] — [M10] Resume cascade: --continue exclusions, id/name/abs-path resolution, PR-linked picker filter
- [#158] Resume picker scope widening, preview, rename, grouping [NICE/M10] — [M10] Resume picker keys: worktree/branch scopes, Space preview, in-row rename, fork grouping
- [#158] Permission-mode and state restoration matrix on resume [NICE/M10] — [M10] Resume state contract: what restores (mode/model/agent/goal) and what must be re-passed
- [#158] Session naming: -n flag, collision variants, plan-accept auto-title, default display names [NICE/M10] — [M10] Session naming: -n flag, live-name collision suffix, plan-accept auto-title, display-name split
- [#133] Resume-from-summary dialog (cache-expired heuristic) [NICE/M5] — [M5] Resume-time compaction offer when the prompt cache is provably cold
- [#134] Transcript storage layout and project-dir naming contract [NICE/M2] — [M2] Claude import: exact CC bucket slug (non-alnum→'-', 200-char+hash) and CLAUDE_CONFIG_DIR
- [#134] New session entry types beyond internals: queue-operation, system subtypes, file-history-snapshot, cost-state, queue/metadata user fields [NICE/M2] — [M2] CC transcript schema census: queue-operation, system subtypes, file-history records, toolUseResult — importer fidelity + drift fixture
- [#134] Per-session sidecar tree: subagents/workflows, tool-results spill, workflows/scripts, memory/ [NICE/M2] — [M2] Session sidecar layout: workflow transcripts, tool-results spill naming, scripts archive
- [#159] cleanupPeriodDays semantics and companion retention keys [NICE/M2] — [M2] Transcript retention: aging pass, purge verb, session-delete-leaves-transcript
- [#158] /branch vs --fork-session inheritance matrix [NICE/M10] — [M10] Fork vs branch writer semantics: what carries over, running-task retargeting, interleave warning
- [#237] stream-json session protocol additions: capabilities, mcp_server_errors, api_retry no_response, plugin_install [NICE/M10] — [M10] Ready/init frame: capabilities[] feature-detection and structured skipped-config errors
- [#238] Slash-command surface for MCP prompts [NICE/M10] — [M10] MCP prompts as /mcp__server__prompt slash commands with arg validation
- [#239] MCP resource tools and @-mention reality [NICE/M7] — [M7] MCP resource tools seam (list/read/read-dir) and @-completer merge of live sessions
- [#149] Per-server timeout stack and idle watchdog [NICE/M6] — [M6] MCP timeout stack: 30s startup, wall execution cap, 60s per-request floor, transport-split idle watchdog on progress
- [#149] MCP output budget + spill-to-file overflow [NICE/M6] — [M6] MCP result budget: 25k-token cap, fixed 10k warn, spill to session tool-results file, per-tool size opt-out
- [#149] headersHelper auth hook [NICE/M6] — [M6] headersHelper: per-connect shell command, JSON headers on stdout, 10s cap, override-static + 401 rerun-retry

**Cross-checked away:**
- Checkpoint/rewind contract detail (2.1.263) :: DUPLICATE :: #109 All edge contracts verified true against checkpointing.md (mid-turn messages uncheckpointed; per-file FIRST snapshot retained for VS Code diff baseline; 'Restored the code, but skipped N files' symlink/hardlink skip with debug log ~/.claude/debug/<sessio…
- /clear naming and conversation-handoff list :: DUPLICATE :: #107 Facts verified (/clear [name] labels the previous conversation; no-arg keeps a user-set --name//rename name but drops the AI-generated title; /new /reset aliases). xdev already has /clear ('reset context in place, history kept on disk' — internal/tui/com…
- Headless session lifecycle: SIGTERM, bg-task grace, resume of unfinished turn :: ALREADY-DOCUMENTED :: Core contract already in the baseline verbatim: cc-official-docs §10 says 'SIGTERM→143, leaves turn unfinished, resume continues it'; §8 already has the ~5s bg-shell kill and the 10-min/CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS cap + §8 VERIFY item resolved. Verif…

**Already covered (traceability):**
- /export contract -> Already mapped as omp-sourced rows (PRD §2: /dump CORE M10, /export HTML NICE M14); CC's added stance worth copying: declare xdev export the stable contract vs. raw JSONL.
- Subagent transcript streaming via parent_tool_use_id + --forward-subagent-text -> Already flagged VERIFY in cc-official-docs §10; this sweep confirms the contract and exact flag/env names for M11 yield-isolation tests.
- mcp__ naming rules incl. plugin servers and wildcards -> New detail worth appending to §9 verdict: the [^A-Za-z0-9_-]→_ sanitization and 'bare mcp__server matches nothing' footgun.
- Transcript format stability stance vs /export -> Adopt the inverse stance in docs/reference/session-format.md (xdev declares STABILITY) — strengthens the interop promise that CC refuses.

**Rejected with reasons:**
- MCP output styles :: Brief asked to check 'MCP output styles' — negative finding recorded.
- claude.ai connectors and org connector tool controls :: Pure Anthropic-platform surface (§1 non-goal); the transports/scope facts already captured; xdev stays BYO-endpoint.
- Cloud/Remote-Control session surfaces (--cloud, ccpool_ environments, RC name prefix, /teleport, /desktop, /autofix-pr) :: Anthropic-hosted platform bundles, not local session lifecycle; xdev equivalents are rpc+hub (#91+) — M15 tail.

### telemetry-cost-ops — 8 confirmed gaps; 2 dup/rejected; 4 covered; 36 rejected-at-research
Method/caveats: Sweep executed 2026-09-14 against installed build ~/.local/share/claude/versions/2.1.263 plus current live docs; docs describe post-263 behavior where noted (2.1.269 OTEL_METRICS_INCLUDE_REPOSITORY, gRPC-scheme approval-dialog fix, /insights session-model fallback — flagged inline; the 2.1.263 section itself is 'Bug fixes and reliability improvements'). All six briefed slugs fetched successfully (monitoring-usage, analytics, costs, data-usage, zero-data-retention plus fast-mode, advisor, statusline, settings-reference, cli-reference, interactive-mode, commands as follow-on links); monitoring-usage.md was fetched via direct curl because WebFetch truncated mid-events-section — the full page (153 KB) was read from /tmp/monitoring-usage.md. Binary forensics: env-var census and claude_code.* string inventory via grep -ao on the 2.1.263 Mach-O; undocumented embedded spans (claude_code.bash.su…
**Confirmed gaps (filed):**
- [#167] /usage command: Session block contract [NICE/M10] — [M10] /usage session block: cost, API-vs-wall duration, line deltas, per-model breakdown
- [#133] Prompt cache statistics: /usage line + status line prompt_cache object [NICE/M12] — [M12] prompt_cache session stats: hit ratio, miss-cause diagnosis, status-line object
- [#173] modelPricing managed table (contracted rates for all cost figures) [NICE/M9] — [M9] client-side rate table: built-in list prices + models.yml pricing overrides feeding all cost figures
- [#174] --max-budget-usd (print mode spend cap with subagent rollup) [NICE/M10] — [M10] --max-budget-usd print-mode cap with subagent rollup and spawn denial
- [#175] Usage-limit auto-continue wait [NICE/M9] — [M9] usage-limit wait: auto-continue at reset with esc-cancel, re-arm cap, sleep-safe resume
- [#159] Retention sweep (cleanupPeriodDays) + sweep event [NICE/M10] — [M10] retention sweep for session transcripts: startup delete older than retention.days, pause on bad config
- [#176] /insights local usage report [NICE/M14] — [M14] /insights: LLM analytics over local session history written to one self-contained HTML report
- [#167] /usage plan-attribution breakdown + Loops rows [NICE/M10] — [M10] per-extension usage attribution: skills/subagents/MCP-server shares rolled up locally for /usage + stats

**Cross-checked away:**
- Background/idle token consumers + goal check-in cap :: DUPLICATE :: candidates #1 (/usage session block) and #8 (per-extension attribution) REJECTED as a standalone gap after code checks. (a) CC's background-consumer list (costs.md §Background token usage) is pricing guidance, not a port contract — the only hard numbers are '…
- Interactive usage commands: /usage /cost /stats /usage-credits /fast /insights :: DUPLICATE :: candidates #1 (aliases + session block) and #2 (cache-break warning) DUPLICATE — no independent work survives. /cost and /stats aliases are one bullet of the /usage session-block issue (#1, included there); the /model and /effort cache-miss warning is a read-…

**Already covered (traceability):**
- Fable advisor spend accounting -> xdev M11 advisor/watchdog already opted-in + cost-disclosed via §5.2 fx adopt #117; the concrete addition here (advisor spend must roll into the same session totals as main-model spend) is captured in the …
- Agent-team cost guidance -> PRD §5.1 already rejects uncapped fan-out citing ~15× orchestrator cost and keeps hub depth/width caps (M11); default-off matches xdev spawn policy.
- OTel non-inheritance into subprocesses -> Matches xdev's env hardening in M3 (extension subprocess env is explicit, not inherited) — recorded for traceability: xdev's existing scrub-then-inject rule already implements the stricter version.
- Cost reporting: list-price math + data residency + gateway parity -> total_cost_usd in -p JSON already in cc-official-docs §10; the ONE-math-feeding-all-surfaces rule is embodied in the modelPricing and /usage issues above.

**Rejected with reasons:**
- Telemetry master switch + exporter selectors :: Pure OTLP-export surface; xdev §1 ships no telemetry and extensions are subprocesses — an enterprise OTel story would live in an external collector wrapper, never in the binary.
- OTLP endpoint/protocol/header precedence ::
- Export interval defaults ::
- Content-logging privacy gates ::
- OTel content length caps ::
- Metric cardinality controls ::
- Standard attribute set on all metrics/events ::
- Repository attributes (vcs.*) derivation ::
- Event-only (unbounded-cardinality) correlation attributes :: The prompt.id-per-turn correlation idea mirrors xdev's event-stream turn ids; already in xdev data model.
- Metric inventory (8 counters) with attribution set ::
- Event inventory (~25 claude_code.* log events) :: CC's event.name/event.timestamp/event.sequence envelope + redaction-by-default content gates are the reference shape xdev's JSONL event stream already implements internally.
- Span hierarchy + low-cardinality safe attrs (beta traces) ::
- Managed-settings OTLP destination lock ::
- otelHeadersHelper dynamic headers ::
- mTLS + exporter SDK tuning vars (binary census) ::
- Undocumented spans in 2.1.263 binary ::
- Export debug signals ::
- Retry-exhaustion observability contract :: xdev M5 already has the retry/failover taxonomy; the 'terminal error event, attempt count, no per-retry events' shape is a cheap addition to xdev debug log if ever wanted.
- Resource identity block ::
- Anthropic operational telemetry (default-on, separate from OTel) :: xdev makes zero non-essential calls (pi-minimal); the conditional 'on only when 4 predicates hold' pattern is the closest reusable idea and is moot without a phone-home.
- Provider default matrix + host-managed flip ::
- Session quality surveys contract ::
- /feedback, /bug, /share submission paths + Claude-drafted feedback :: xdev issue reports already go via gh to the repo; local-archive-on-third-party-provider is the one portable wrinkle (not worth an issue).
- Data retention + training policy ::
- WebFetch domain safety preflight :: Anthropic-blocklist-dependent; xdev web tool stays provider-agnostic (no phone-home). The one-portable fact: a safety check must be independent of the telemetry kill switch — matches xdev's 'fail-closed …
- Zero data retention (ZDR) ::
- Org analytics dashboard (Teams/Enterprise) ::
- Console analytics + analytics APIs ::
- Claude apps gateway spend/telemetry relay :: xdev M15 lists auth-broker/gateway services; if built, mirror the 'client exports directly when managed settings name a collector' simplification.
- Usage-credits / budget-exhaustion escalation paths :: claude.ai billing flow; xdev's equivalent is the M9 quota rotation + the auto-continue issue above. The diagnostic taxonomy (window limit vs model limit vs spend limit vs context warnin…
- Fast mode (pricing, billing surfaces, env, fallback) :: Anthropic-only product mode; the portable sliver is 'premium-mode rejection falls back to standard speed per request with a stream notification' — matches xdev's model-tier failover, …
- Enterprise cost benchmarks + rate-limit sizing :: Anthropic-platform ops guidance; no xdev surface. Retained only as the sweep's 'org-level vs per-user enforcement' distinction, already implicit in xdev's per-credential quota rotation.
- DO_NOT_TRACK / standard opt-out honored :: xdev ships default-off and zero non-essential traffic; honoring a DO_NOT_TRACK var costs one line of code if a stats extension ever phones home.
- Bedrock monitoring reference ::
- Telemetry-disabled behavior deltas :: Sweep-note only: xdev has no dual path; the pattern 'core features must not depend on the telemetry channel' is already satisfied by construction (extensions-over-SDK, offline-first).
- Prometheus scrape endpoint ::

### auth-providers-gateway — 12 confirmed gaps; 0 dup/rejected; 5 covered; 5 rejected-at-research
Method/caveats: Swept primary sources: authentication.md, third-party-integrations.md, llm-gateway.md, llm-gateway-connect.md (full, incl. per-surface + troubleshooting tables), llm-gateway-protocol.md, network-config.md, amazon-bedrock.md, google-vertex-ai.md, microsoft-foundry.md, claude-platform-on-aws.md, settings-reference.md (apiKeyHelper/awsAuthRefresh/awsCredentialExport/gcpAuthRefresh/forceLogin*/policyHelper/otelHeadersHelper/processWrapper entries), env-vars.md (auth/network/gateway rows), gateways.md, claude-apps-gateway.md, CHANGELOG.md (2.1.203–2.1.270 auth/gateway lines), plus grep -a forensics on the installed 2.1.263 binary and xdev code state (internal/config/{env,credentials,discovery}.go, internal/oauth/oauth.go). Caveats: (1) claude-apps-gateway-config/-deploy/-on-aws/-on-gcp and llm-gateway-rollout NOT mined — deliberate per the domain brief (admin/server halves); rollout page con…
**Confirmed gaps (filed):**
- [#177] apikeyhelper-contract [CORE/M9] — [M9] apiKeyHelper contract: command-minted credential in both headers, TTL cache, 401 re-run, 3-strike failure
- [#comment->#123] file-descriptor-credential-injection [NICE/M9] — [M9] Secret-injection via file descriptor + host creds file: keep tokens out of the inherited environment
- [#130] headless-auth-surface [NICE/M10] — [M10] Auth CLI depth: status --json, setup-token mint, refresh-token env exchange, expiring-login warning
- [#136] provider-credential-refresh-helpers [NICE/M14] — [M14] awsAuthRefresh/awsCredentialExport/gcpAuthRefresh contract: pre-checks, 3-min kill, expiration cache
- [#136] cloud-provider-env-surface [NICE/M14] — [M14] Cloud-provider env surface: selection/skip-auth vars, region ladders, workspace IDs, pins, Mantle
- [#144] gateway-wire-contract-client-side [NICE/M9] — [M9] Gateway client etiquette: x-xdev identity headers + custom-header replace semantics
- [#144] gateway-model-discovery-contract [NICE/M9] — [M9] Gateway model discovery: redirect-fail, dual headers, id filter, last-good cache, alias collapse
- [#178] capability-rejected-recovery [CORE/M5] — [M5] Capability-rejection recovery: capability_rejected token, thinking retry-once, per-conversation disable
- [#166] mtls-ca-network-auth [NICE/M9] — [M9] mTLS client certs with rotation reload, CA-append policy, proxy validation, process-wrapper seam
- [#166] proxy-auth-helper-seam [NICE/M9] — [M9] Proxy auth helper seam: command-minted Proxy-Authorization with TTL cache for NTLM/SSO-gated proxies
- [#179] apps-gateway-reference-protocol [DEFERRED/M15] — [M15] Apps-gateway client protocol: device flow, private-host /login, TLS pin, fail-closed startup
- [#130] provider-setup-wizards [NICE/M10] — [M10] Provider setup wizard (bedrock/vertex): verify + pin models, write user-settings env block

**Already covered (traceability):**
- authentication-precedence-chain -> PRD §2 M9 'credential resolution chain (CLI flag → models.yml → stored OAuth → /login key → env)'; #123 adds strict no-fall-through for explicitly named sources; internal/config/credentials.go implements …
- credential-storage-keychain-fallback -> #123 carries the Keychain-shellout + 0600/nlink verification design. NEW detail #123 omits: keychain entry keyed by config-dir so a session with a different data dir reads a different credential — wo…
- env-precedence-trust-gating -> #114 (repo is not a configuration authority) + #121 (provenance) cover the policy; internal/config env.go already layers process env → project .env → agent .env. New sub-fact worth a line on #114: settings en…
- watchdog-clamp-contract -> xdev ships first-progress + idle watchdogs (M1, PRD §2). The portable deltas to record as acceptance criteria on #25/#84, not a new issue: silent clamp semantics and counting SSE pings/comments as liveness.
- provider-model-pins -> Model-config domain's sweep owns the alias side; the provider half is already listed in the M14 env-surface issue's acceptance criteria. #75/#77/#105 cover picker/seed behavior. Deliberately not double-filed.

**Rejected with reasons:**
- anthropic-profiles-wif-federation :: Pure Anthropic console/ant-CLI/WIF surface. The one portable idea — an explicitly named credential is an exact authority ranked above ambient; a leftover ambient login must not silently override the nam…
- forcelogin-org-policy :: MDM/console surface per taxonomy; xdev has no org-policy plane (#71 M15 is the home if policy ever lands). 2.1.269 items post-date the 2.1.263 sweep.
- platform-adjacent-env :: §1 non-goal (no Anthropic telemetry/platform surface); xdev equivalents are the container/sandbox reference doc and the caching domain. otelHeadersHelper would matter only if xdev ships OTLP — M15 decision, unfiled.
- per-surface-gateway-config :: xdev ships CLI+RPC only; IDE/Action/desktop halves don't exist. The SDK env semantics matter to xdev's RPC hosts — already documented in the extension/embedding contract (#100-line ACP/RPC rows), no new issue.
- post-263-auth-changelog :: Nothing here becomes an xdev issue at 2.1.263 parity; timeout var noted so xdev keeps its own 4s constant without chasing a 2.1.269 knob.

### security-sandbox-permissions — 11 confirmed gaps; 6 dup/rejected; 8 covered; 4 rejected-at-research
Method/caveats: Sources swept: sandboxing.md, permissions.md, permission-modes.md, security.md, security-guidance.md, sandbox-environments.md, auto-mode-config.md, network-config.md, data-usage.md, managed-settings.md (partial), settings.md, commands.md (truncated), settings-reference.md (index table only — per-key type/default rows were truncated by the fetcher; where defaults matter I verified against the 2.1.263 binary: autoAllowBashIfSandboxed defaults true, credentials mode deny|mask, sandbox.enabledPlatforms exists in-binary but is undocumented in the fetched pages), and CHANGELOG main. Local forensics: `grep -a` on /Users/linh.doan/.local/share/claude/versions/2.1.263 for the tool_decision event shape, decisionClassification enum, strippedDangerousRules, the restrictive-key merge array, and Seatbelt op-codes. Unverified/could-not-verify: (1) seed slug `iam` was never fetched — permissions.md con…
**Confirmed gaps (filed):**
- [#161] Critical-path rm circuit breaker (no mode can approve) [NICE/M3] — [M3] Critical-path rm floor: rm/rmdir on root, top-level dirs, home, cwd ancestors, or glob-under-empty-var never auto-approved, even in yolo
- [#161] Protected paths never auto-approved (config self-write) [NICE/M3] — [M3] Protected-path floor: writes to .git/.xdev/.husky, shell rc files, PM rc files, and MCP config are never pre-approved by allow rules
- [#180] Bash rule matching engine (compound split, wrapper strip, read-only set, redirects) [CORE/M9] — [M9] Bash matcher semantics: wrapper/assignment stripping, quoted-substitution parsing, read-only command set, redirect-as-file-op checks
- [#160] Read/Edit path rule grammar (gitignore-style, four anchors, symlink pair) [NICE/M9] — [M9] Path-scoped Read/Edit allow/ask/deny rules: four gitignore anchors, allow-vs-deny depth asymmetry, symlink two-path pair, deny propagation
- [#160] Tool(param:value) matching + tool-name globs [NICE/M9] — [M9] Rule grammar add-on: Tool(param:value) deny/ask matching with primary-field ban, canonical names, and anchored tool-name globs
- [#comment->#105] Working directories: --add-dir / additionalDirectories / /add-dir semantics [NICE/M9] — [M9] Pin the additional-directory contract: extra roots grant file access + context files only, never config, hooks, or approval authority
- [#129] Rule-save behavior on 'Yes, and don't ask again' [NICE/M9] — [M9] Approval write-back: 'remember' option saves per-repo grants to a user-owned gitignored local layer at the git root, file-edit grants stay session-only
- [#129] Permission-rule hot-apply, provenance, and prompt comments [NICE/M9] — [M9] Approval grants apply from the next tool call; denial prompts carry a user reason into the model message
- [#161] blockReadsOutsideWorkingDirectories + first-read-outside prompt [NICE/M3] — [M3] blockReadsOutsideWorkspace: user-only opt-in flag refusing file-tool (and read-only-bash) reads outside the working directories in every mode
- [#181] Session JSONL permission decision records (tool_decision) + mode persistence [NICE/M2] — [M2] tool_decision session entries: structured {decision, source, rule, reason} per approval verdict + permission_denials in print-mode results
- [#comment->#105] Headless hardening flags: dontAsk, --permission-prompts none, --restricted, root guard [NICE/M10] — [M10] Unattended-mode flags: --deny-prompts naming today's headless behavior, and a --restricted hostile-repo profile (defaults-only config, exec/web tools rem…

**Cross-checked away:**
- Permission rule evaluation order (deny > ask > allow, first-match, specificity-blind) :: IMPLEMENTED :: Core contract already shipped. internal/tool/policy.go Decide() resolves by category — full deny scan first, then per-tool, then prompt scan, then mode default — which is exactly CC's deny>ask>allow specificity-blind order; package doc and policy_test.go:102 …
- Cd rules + /cd session move semantics :: REJECTED :: PRD §1 goal 4 / §2 CLI-parity row (no /cd surface) The ported surface has no xdev target: xdev has no in-session directory-move command (grep found only launch-time homeSwitchDir in cmd/xdev/launchflags.go; /cd is not in omp's flag/command inventory and is on…
- Auto-mode classifier config surface (autoMode keys + CLI + cache/thresholds) :: DUPLICATE :: #117 All facts verified (auto-mode-config.md + permission-modes.md) but the material is the configuration/cache contract AROUND the classifier #117 adopts, and #117 is open and unimplemented — the candidate itself recommends extending #117 rather than filing …
- Rule-file startup validation warnings :: DUPLICATE :: #121 Facts verified (permissions.md wildcard-before-subcommand warning, never-consulted Write(path)/NotebookEdit(path)/Glob(path) rules, Bash(command:…) primary-field param rules ignored with startup warning, parenthesized mcp__ settings rules 'skipped… liste…
- Security-guidance plugin: three-layer self-review wired on hooks :: REJECTED :: PRD §1 goal 5 (extensions are processes) / #67 + hooks bus Real capability, verified in full (security-guidance.md: the hook wiring table SessionStart/UserPromptSubmit/PostToolUse Edit|Write|NotebookEdit/Stop(≤30 files, ≤3 consecutive re-fires)/PostToolUse Ba…
- Pre-approved network/proxy/TLS contracts :: REJECTED :: PRD §2 'first-progress + idle watchdogs' row / Go stdlib ProxyFromEnvironment Verified the facts are true (network-config.md: proxy order https_proxy→HTTPS_PROXY→http_proxy→HTTP_PROXY first-set-wins, NO_PROXY space- or comma-separated with bare `*`, loopback …

**Already covered (traceability):**
- acceptEdits mode auto-approve set -> cc-official-docs.md §6 already lists this exact set and verdict.
- Permission modes set + starting-mode cascade + shift+tab cycle -> §6 prior study verdicts the mode set; repo-cannot-select-permissive-modes is exactly #114's key-tiering fix; dontAsk/CI flags raised separately below.
- Workspace trust + pre-trust content matrix + hasTrustDialogAccepted -> #114 already files the repo-safe-key-set + per-workspace trust-file design; CC's git-root keying, worktree→main-checkout resolution, and the pre-trust content table are…
- WebFetch(domain:) matching grammar -> cc-official-docs.md §1 records `WebFetch(domain:…)` rules + preapproved domains + redirect refusal; wildcard-depth grammar and bare-vs-domain asymmetry are new detail — fold into the path-grammar issue…
- /security-review on-demand command -> Present in the user's own skill list (/security-review is a shipped CC command); for xdev it is one bundled-skill prompt on top of the diff plumbing from the plugin-review issue — no separate issue; no…
- Prompt-injection defense contracts -> PRD §5 'Injected content is hostile until scanned' (hermes pattern) already commits the concept; the two portable deltas (strip tool results from any reviewer's input; never feed poisoned output to an …
- EndConversation deny/ask immunity -> Trivial carve-out; note it in xdev's rule-engine acceptance list (already folded into the evaluation-order issue).
- Sandbox environments menu incl. @anthropic-ai/sandbox-runtime -> xdev's containment menu is docs/reference/container-and-sandbox.md (already has sandbox-exec + container recipes); add a paragraph citing sandbox-runtime's path-deny set as t…

**Rejected with reasons:**
- Built-in OS sandbox engine (Seatbelt / bubblewrap / proxy) and its settings keys :: §1 non-goal: 'No built-in permission/security system; containment is external sandboxing'. xdev must not embed a Seatbelt/bwrap/proxy engine. The externall…
- Sandbox-network domain/permission-rule merge :: Engine-internal merge order; only matters if xdev ever ships a sandbox — M15 at most.
- Managed settings delivery + enterprise lock keys :: Pure MDM/console surface (explicitly in the reject class per the brief). The one portable idea — 'deny entries only ever narrow; no scope can remove another's deny' merge semantics — is f…
- Telemetry/data-usage/trust-center surface :: Anthropic-platform marketing/telemetry; cleanupPeriodDays already covered §7; xdev ships no telemetry (PRD).

### models-auth-admin — 11 confirmed gaps; 0 dup/rejected; 1 covered; 0 rejected-at-research
Method/caveats: Sources swept: 404/OK — model-config, authentication, settings, settings-reference, costs, monitoring-usage, network-config, amazon-bedrock, google-vertex-ai, llm-gateway, llm-gateway-connect, llm-gateway-protocol, feature-availability, third-party-integrations, claude-platform-on-aws, server-managed-settings, managed-settings, fast-mode, env-vars all fetched; `administration`, `analytics` (fetched secondarily via links), `data-usage`, `zero-data-retention`, `microsoft-foundry`, `gateways`, `claude-apps-gateway*`, `advisor`, `llm-gateway-rollout`, `settings-example` NOT fetched (thin/blocked budget) — analytics/data-usage findings are therefore based on cross-references from costs.md, feature-availability.md and network-config.md only. Two WebFetch attempts to proxy-hosted mirrors returned 403; re-fetched from canonical code.claude.com URLs instead. Version caveats: the target build is …
**Confirmed gaps (filed):**
- [#151] model-alias-set-and-1m-suffix-grammar [CORE/M9] — [M9] Model alias table + [1m] suffix grammar + compiled default-model constants
- [#168] per-model-context-windows-and-autocompact-thresholds [CORE/M5] — [M5] Per-model context-window table + auto-compact threshold grammar and overrides
- [#151] model-priority-order-five-tiers [CORE/M9] — [M9] Five-tier model-resolution precedence with per-pair env-vs-settings arbitration
- [#151] alias-pinning-env-vars-and-small-fast-deprecation [NICE/M9] — [M9] Alias-pinning env vars (4 aliases, dual-role opusplan) + SMALL_FAST_MODEL deprecation pattern
- [#151] availablemodels-enforce-and-merge-rules [NICE/M9] — [M9] Model allowlist with prefix/wildcard matching, per-callsite enforcement and merge rules
- [#141] fallbackmodel-chain-and-fallback-exclusions [NICE/M5] — [M5] Fallback-chain guards: 3-model cap after dedup, turn-scoped switch, error-class exclusions, no-shrink-during-compaction
- [#151] recognition-check-and-custom-model-option [NICE/M9] — [M9] Model-id recognition check (first-party only, typed/programmatic switches) + custom model option entry
- [#140] effort-lenum-ladder-and-resolution-order [CORE/M9] — [M9] Effort ladder incl. xhigh rung, clamp rule, 4-step resolution order, output_config rejection probe
- [#140] effort-config-keys-and-admin-cap [NICE/M9] — [M9] Effort persistence: per-model saved levels, effortLevel/maxEffortLevel keys, frontmatter effort
- [#182] ultracode-effort-mode [NICE/M11] — [M11] 'ultracode': one boolean arming xhigh effort + standing dynamic-workflow orchestration
- [#140] extended-thinking-config-surface [CORE/M5] — [M5] alwaysThinkingEnabled + MAX_THINKING_TOKENS ladder and the thinking-off ⇒ effort-clamp rule

**Already covered (traceability):**
- thinking-display-and-charging -> xdev shipped `showThinking` (PRD §Known gaps #20, 108c63e replay parity) covering render+gate; CC adds only a summaries key and the 'charged even when collapsed' wording, which xdev's usage accounting shoul…

### memory-context — 12 confirmed gaps; 10 dup/rejected; 4 covered; 2 rejected-at-research
Method/caveats: Sweep date 2026-09-14, CC v2.1.263. Sources: official docs memory.md, context-window.md, how-claude-code-works.md, model-config.md, prompt-caching.md, hooks.md, settings-reference.md, data-usage.md (all fetched live); CHANGELOG.md (raw, searched for #/dream/microcompact/thinking/compact-window — none of those terms appear, confirming # is removed, dreaming is undocumented, autoDreamEnabled is server-gated); local binary strings/grep at ~/.local/share/claude/versions/2.1.263 for autoDreamEnabled, microcompact_boundary/compact_micro_keep_recent/toolsCleared/toolsKept, thinking_strip, and /context category labels. NOT VERIFIED / thin: (1) the microcompact keep-recent numeric threshold — a `5000` constant (Lje=5000) sits near keep-recent logic but its UNIT (tokens vs chars vs count) is unconfirmed from the minified bundle; treat the count fields (toolsCleared/toolsKept) as the reliable cont…
**Confirmed gaps (filed):**
- [#138] External-import approval dialog [NICE/M10] — [M10] Gate out-of-tree @imports and symlinked rules behind a one-time approval
- [#138] CLAUDE.md HTML-comment stripping [NICE/M10] — [M10] Strip block-level HTML comments from context files before injection
- [#138] Path-scoped rules: brace-expansion budget [NICE/M10] — [M10] Rule globs: brace expansion under a 1,000-pattern/4 MiB budget, fail-soft
- [#comment->#91] Subagent auto memory (memory field) + isolation [NICE/M11] — [M11] Task-agent memory: opt-in private dir via frontmatter; fresh children stay isolated
- [#137] What survives compaction (detailed contract) [CORE/M5] — [M5] Post-compaction survival: re-read top-5 touched files, re-inject plan + invoked skills under 5k/25k
- [#137] Compaction summarization inherits extended-thinking config [NICE/M5] — [M5] Pass the session thinking state into the compaction summarize request
- [#133] Prompt caching TTL buckets + cache accounting [CORE/M9] — [M9] Emit Anthropic cache breakpoints with two TTL buckets; report cache tokens + hit ratio
- [#183] /context local estimate + per-category display [NICE/M10] — [M10] /context: per-category table from provider usage + local estimate, no round trip
- [#168] autoCompactWindow value grammar + config surface [NICE/M10] — [M10] autoCompactWindow: 100K–1M with k/M/bare grammar + 3-surface precedence
- [#comment->#92] InstructionsLoaded hook event [NICE/M11] — [M11] InstructionsLoaded event keyed on the load-reason enum
- [#184] skillListingBudgetFraction / skillListingMaxDescChars [NICE/M12] — [M12] Cap the skill listing: context-fraction budget + per-description char cap
- [#138] /clear semantics [NICE/M10] — [M10] Reload context files + rules on /clear (and applied /compact)

**Cross-checked away:**
- auto-dream (background memory consolidation) :: IMPLEMENTED :: CC fact verified in bundle strings (autoDreamEnabled, '[autoDream] lock held by live PID', dream prompt; absent from memory.md as claimed). But xdev already ships the contract: internal/memory/pipeline.go consolidation pass with a lease file refreshed by hear…
- # quick-add-to-memory shortcut :: REJECTED :: Negative fact, not a gap: CC 2.1.263 has no '#' memory prefix (memory.md shows natural-language remember + /memory only; binary has only the unrelated spell-fix regex; CHANGELOG clean). xdev's M12 extraction + learn tool already cover remember-by-natural-lang…
- /import command (foreign-agent config migration) :: ALREADY-DOCUMENTED :: PRD §2 Third-party-context-files + Rulebook rows (M10/M11); issues #97, #103 Fact verified (memory.md: /import 'appends a one-time copy of instruction files such as AGENTS.md to the matching CLAUDE.md and carries over MCP servers, commands, subagents, and ski…
- Memory loading from --add-dir :: REJECTED :: Fact verified (memory.md: extra-dir CLAUDE.md not loaded unless CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1, then four file kinds; local respects --setting-sources). xdev ships the superset ungated by deliberate choice: print.go:902-910 loads the AGENTS.md…
- Rules discovery: recursion, symlinks, setting-sources, priority :: ALREADY-DOCUMENTED :: PRD §2 Rulebook row; internal/rules/rules.go (multi-source priority + enabledProviders gate) Verified (memory.md: rules discovered recursively; user before project, project wins; project-excluded setting-sources skips rules incl. on-demand post-2.1.211; symli…
- Auto-memory dir naming via CLAUDE_CODE_PROJECT_DIR_NAME :: ALREADY-DOCUMENTED :: cc-official-docs.md §5 (per-repo/worktree derivation, autoMemoryDirectory); cc-envvars.txt (PROJECT_DIR_NAME); #114 (trust rule) Verified (memory.md: PROJECT_DIR_NAME override v2.1.234+; autoMemoryDirectory abs-or-~/, any scope, project-scope under the hooks …
- Memory files exempt from retention sweep :: REJECTED :: Fact verified (memory.md: transcript sweep 'excludes the memory files'; CHANGELOG 'Fixed session cleanup deleting contents inside a project's memory folder'). xdev has no transcript-age GC — gcKinds = blob, artifact, subagent, dump (cmd/xdev/gccmd.go:39-40); …
- Microcompact keep-recent internals :: ALREADY-DOCUMENTED :: PRD §5.1 adopt bullet (microcompact — do-not-re-raise list); rung trims belong in open issue #83 Bundle strings verified (compact_micro_keep_recent, microcompact_boundary, toolsCleared/toolsKept; the 5000 constant unit admittedly unconfirmable; no docs page n…
- Auto-compact thresholds per model :: ALREADY-DOCUMENTED :: cc-official-docs.md §12 (200k near limit, Sonnet 5 ~967k) + its VERIFY line; PRD §2 models.yml overrides row All facts verified (model-config.md: Sonnet 4.6/Opus 4.6 no-extended-context and Opus 4.8/5 on 200K windows compact at 200K; native-1M ~967K; DISABLE_…
- PreCompact / PostCompact hooks; SessionStart compact+fork sources :: DUPLICATE :: #92 Facts verified (hooks.md: PreCompact/PostCompact matcher manual|auto; SessionStart enum startup|resume|clear|compact|fork, compact-matching hooks re-inject after compaction; common inputs prompt_id 2.1.196+, scratchpad_dir 2.1.257+, effort.level low|mediu…

**Already covered (traceability):**
- Deferred MCP tool schemas (ENABLE_TOOL_SEARCH) -> Already a §2 matrix row ('deferred tool catalog tool_search/tool_describe/tool_call bridge, M7') and cc-official-docs.md §1/§12 (deferred MCP tool names ~120, ToolSearch). New detail is the…
- CLAUDE.md hierarchy, @imports depth-4, rules paths:, MEMORY.md 200-line/25KB, autoMemoryEnabled/Directory, DISABLE_AUTO_MEMORY, post-compact root re-inject -> All present in cc-official-docs.md §5 and PRD §5.1 'CLAUDE.md as alias (M10)' + …
- Auto-memory recall mechanics + MEMORY.md near/over-limit -> Prior §5 said 'over-limit write→error'; this confirms the near-limit reminder + MEMORY.md-only cap + 4 MiB CLAUDE.md skip. Covered by M12 memory backend; no new issue — note the 4…
- Fast-mode / plugin-reload / model-switch cache invalidation triggers -> These are cache-invalidation details subordinate to the TTL/accounting finding above (the actionable port for xdev is: keep system+project prefix stable, treat appends…

**Rejected with reasons:**
- Managed CLAUDE.md (claudeMd key, managed policy paths, cannot-be-excluded) :: Pure enterprise-MDM surface (managed-settings.json + OS-specific policy dirs). Already partially in cc-official-docs.md §5/§11 as covered; §1 non-goals put manag…
- Data retention / training policy / telemetry :: Anthropic-platform / telemetry / claude.ai training surface — pure §1 reject (no telemetry, marketplace/ecosystem services to M15). The ONE portable fact — cleanupPeriodDays default 30d + mem…

### hooks-settings-sweep — 12 confirmed gaps; 1 dup/rejected; 15 covered; 6 rejected-at-research
Method/caveats: Sources fetched live 2026-09-14: code.claude.com/docs/en/hooks (322KB, full page — the real reference; the sweep brief's 'hooks-reference' slug 404s, /docs/en/hooks.md is canonical), /hooks-guide, /settings, /settings-reference (437KB), /env-vars (487KB, 356 documented vars), /sandboxing, /statusline, plus the docs map. CHANGELOG.md not separately consulted — version gates quoted come from the docs pages themselves. The settings index yields 228 distinct keys (extracted to /tmp/cc-keys.txt); per-key type/default lines were harvested with an awk pass, so a few long Description cells were cut at ~200 chars — defaults for effortLevel/modelSettings/promptCacheTtl/tui/viewMode/theme/editorMode enum values were NOT fully captured (types known, value lists truncated); sandbox network proxy-port defaults likewise unverified numerically. Sweep-brief guesses corrected: PreModelUse/PostModelUse ar…
**Confirmed gaps (filed):**
- [#145] Hook IO contract — universal fields, caps, timeouts, per-event exit-2 table [CORE/M11] — [M11] Hook IO contract beyond exit-2: JSON-on-any-exit, 10k spill cap, timeout table, per-event matrix
- [#145] Hook handler field completeness (exec form, once, statusMessage, shell) [CORE/M11] — [M11] Hook handler completeness: argv exec form, per-hook timeout, statusMessage, once, asyncRewake, dedupe
- [#185] HTTP/MCP-tool hook security gates [NICE/M13] — [M13] Pattern record: URL-allowlist + env-interpolation gates for any future HTTP hook transport
- [#186] Subprocess environment contract [CORE/M3] — [M3] Pin the subprocess env contract: identity vars and an env-file preamble (scrub already shipped)
- [#187] Numeric limits registry (defaults + clamps) [NICE/M8] — [M8] docs/reference/limits.md: one table of every xdev cap with CC's verified defaults as the cross-check column
- [#188] MessageDisplay streaming transform [NICE/M12] — [M12] MessageDisplay-equivalent display transform: ext rewrites streamed text per batch, display-only
- [#133] Model-switch cache economics (Pre/PostModelSwitch + sessionTitle) [NICE/M5] — [M5] Compute CC's cache-economics fields for model switches and resume
- [#comment->#92] Worktree hook protocol [NICE/M14] — [M14] Worktree lifecycle events + returned-path guards for hook/ext-owned worktrees
- [#150] MCP elicitation hook seams [NICE/M6] — [M6] MCP elicitation seam: ext-answered forms on the bus, fail-closed decline, pre-send rewrite
- [#150] Defer decision for headless permission handoff [NICE/M6] — [M6] Defer verdict: park an unapproved tool call in print mode and resume it in a fresh process
- [#comment->#117] classifierContext annotation channel to the auto-tier reviewer [NICE/M9] — [M9] classifierContext-equivalent: per-call review_note channel into the #117 reviewer packet
- [#189] processWrapper argv prefix [NICE/M9] — [M9] processWrapper: single argv-prefix knob for child spawns (external-containment seam)

**Cross-checked away:**
- Settings merge exceptions + trust gates :: DUPLICATE :: #114 (primary — repo-safe key set + ignore profile-owned keys) and #121 (provenance per key + quarantining diagnostics) All three live checks: the CC facts ARE new relative to the §11 baseline line (whole-value exceptions confirmed at settings.md:646-648 — fa…

**Already covered (traceability):**
- Hook event taxonomy (35 events) -> cc-official-docs.md §2 (~35 events, 3 cadences) + PRD §5.1 hooks adopt + §2 hooks event bus row. New deltas this sweep: exact event NAMES PostToolBatch/DirectoryAdded/Elicitation/ElicitationResult/Pre-Pos…
- Five handler types + async variants -> cc-official-docs.md §2 named all five; xdev stance keeps command-shape (subprocess JSONL) only — prompt/agent hook types stay post-M14. continueOnBlock/impossible are new contract details worth portin…
- Matcher grammar -> cc-official-docs.md §2 covered exact|list|unanchored-regex. New: the narrow-set exception for FileChanged/StopFailure and version gates; Go regexp ports cleanly (RE2 drops unanchored .test nuance — test('' semantics = Fi…
- `if` permission-rule pre-filter and Bash subcommand analysis -> §5.1 adopted 'two-stage matcher + if permission-syntax pre-filter' and cc-official-docs §2 has the matching table; xdev #92/#94 hooks interception should reuse one shared matc…
- Hook config locations, merge, disableAllHooks/allowManagedHooksOnly -> cc-official-docs §2 covered locations+merge+allowManagedHooksOnly. New: disableAllHooks also gates statusLine/fileSuggestion; /hooks browser; skill-vs-agent frontmatter…
- Path placeholders exported to hooks -> §5.1/skills covered ${CLAUDE_SKILL_DIR}-style vars; xdev ext protocol should mirror: XDEV_PROJECT_DIR + cwd-follows-worktree distinction.
- PermissionRequest decision + permission-writeback entries -> cc-official-docs §2 flagged 'permission-destination writes' ADOPT; xdev's YOLO stance means only the session-destination + setMode parts are relevant (maps to #117 opt-in tier an…
- SessionStart/SubagentStart/UPS context-injection outputs -> cc-official-docs §2 covered 'stdout = context for these events' + hookSpecificOutput. New contracts to fold into xdev hooks design: reloadSkills, watchPaths, initialUserMessage, s…
- Notification event matcher values -> Earlier §2 listed Notification generally. xdev ext protocol's AttentionRequired-style event (#92/#94 fx mapping) should carry the permission_prompt/idle_prompt/elicitation subset; quota_* are claude.ai-…
- ConfigChange / CwdChanged / FileChanged / DirectoryAdded / InstructionsLoaded / Setup / Worktree* / SessionEnd / PostCompact input contracts -> Event existence covered by cc-official-docs §2; the per-event input schemas are the port materi…
- Tool-event input schemas (tool_input shapes per tool) -> Prior §1 covered tool schemas; the hook-visible subsets (esp. subagent telemetry fields resolvedModel/modelsUsed/usage) are NEW and feed #91/#94 (named persistent children) and #107'…
- Status line contract -> PRD §2 'Status-line/HUD segments' M12 CORE + user's own statusLine key. xdev HUD is in-process, so the JSON-spawn contract matters mainly for xdev's external-status consumer; take the FIELD LIST (esp. context_window…
- Settings precedence + merge semantics detail -> cc-official-docs §11 covered the 5 layers + strict-wins security keys + array merge. NEW (issue N14 below): the 4 whole-value exceptions, the trust-gated key list + -p trust trap, broken-file…
- Hook security model — workspace trust asymmetry -> This is the exact hazard #114 (repo-safe project-config boundary) exists to fix; xdev's extension discovery must NOT repeat the -p auto-trust: default trust = user/managed scope only, repo…
- Hook env vars beyond project-dir -> Folded into the subprocess-env-contract issue above; the 'no tty for hooks' fact is the justification for xdev routing notices through the bus instead of writes.

**Rejected with reasons:**
- Sandbox settings tree (sandbox.*) :: §1 non-goal: 'No built-in permission/security system … containment belongs to external sandboxing' — CC's in-process OS-sandbox is exactly what xdev documents externally (docs/reference/container-and-sa…
- Managed-policy / MDM / enterprise key family :: Pure Anthropic enterprise console/MDM surface; xdev has no org server and no MDM concept; version-pin keys (minimum/required*) are the one pattern xdev may later reuse for its own canary chan…
- OpenTelemetry config + telemetry kill switches :: §1 goal: no telemetry; the env-SCRUB half (stripping OTEL_* from children) is kept as part of the subprocess env contract finding below.
- Cloud-provider auth env surface :: xdev's models.yml per-provider baseUrl/alias/discovery (§2 CORE M9) is the equivalent mechanism; CC's per-model DEFAULT_*_MODEL_NAME/DESCRIPTION/SUPPORTED_CAPABILITIES 4-tuple is the same 'custom model op…
- Feature-flag fetching dependency (client posture) :: Anthropic-hosted flag service = platform surface. Design takeaway for xdev: every capability must have a deterministic default or explicit config key, never a fetched flag (matches xdev'…
- claude.ai-platform settings & notification keys :: Subscription/desktop/artifact platform surface; xdev ships none of claude.ai, artifacts (#15 decision), or voice (M15 DEFERRED at most).

### agents-teams-workflows — 7 confirmed gaps; 11 dup/rejected; 6 covered; 1 rejected-at-research
Method/caveats: Method: bundle forensics on ~/.local/share/claude/versions/2.1.263 (null-stripped python extraction, since tool prompts are UTF-16-mixed) plus the prior-sweep env-var catalogue; WebFetch was deliberately limited to bounded queries — never fetched slash-commands/hooks-reference/agent-teams pages whole to avoid the prior stall. Unverified/unrecovered (say so before porting): numeric defaults for MAX_CONCURRENT_SUBAGENTS/SPAWN_DEPTH/PER_SESSION (env names confirmed, values GrowthBook-driven); Workflow script API grammar (only scriptPath/transcriptDir + budget field names recovered); team config.json / mailbox wire format (internals doc covers partially); CronCreate exact param list (durable/recurring/next_run_at inferred); docs slug agent-teams.md content not re-read this sweep. TodoWrite v2 field-level schema taken from omp-todos-internals + bundle literals, not official docs.
**Confirmed gaps (filed):**
- [#190] Agent tool — background-by-default contract [NICE/M11] — [M11] Port CC spawn-economy prompt rules: no-spawn-unless-asked, one-message parallel, never predict pending jobs
- [#191] Subagent spawn caps — three env knobs + GrowthBook depth [NICE/M11] — [M11] Machine-readable subagent cap refusals: live-concurrency, depth, per-session totals, optional spend gate (CC no-retry contract)
- [#149] TaskOutput blocked inside subagents (hook note) [NICE/M13] — [M13] Auto-background slow MCP tool calls past a configurable threshold (CC CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS shape)
- [#162] Local cron/scheduled-tasks contract [NICE/M13] — [M13] Scheduled tasks: 5-field local-tz cron, session-only vs durable store, 7-day recurring expiry, missed-one-shot catch-up (CC CronCreate contract)
- [#192] .claude state-file surface for agents/teams [NICE/M9] — [M9] Deny model Edit/Write to harness state paths: session store, subagent transcripts, and repo-local .xdev config (CC deny-glob parity)
- [#169] Dynamic workflow size controls [NICE/M13] — [M13] Workflow runner: plan script + per-run transcript dir, budget caps (agentCap/tokenCap), size warnings, extension-contributed workflows
- [#193] Built-in agent types (Explore/Plan/general-purpose) contract [NICE/M11] — [M11] Advertise discovered agent types via system-reminder, per-spawn model override, relay + trust-but-verify prompt lines

**Cross-checked away:**
- Agent tool — resume-by-name and fork semantics :: ALREADY-DOCUMENTED :: PRD §2 Subagents line ("fork (context-inheriting) vs fresh agent types", in-scope); internals §2.1/§2.3; resume half IMPLEMENTED: internal/agent/hub.go Send/Revive continue a job by id with prior output re-spawned in Strings verified in bundle (@75.09-75.10MB…
- Agent budget exhaustion contract :: DUPLICATE :: merged into candidate-3's caps issue (subagent_budget_exhausted as fourth reason code + optional spend gate) The `refused.budget` taxonomy line already sits in internals §2.3; the error contract ports as part of the single caps issue rather than a separate co…
- Anti-race / no-fabrication notification contract :: DUPLICATE :: merged into candidate-1's spawn-economy prompt issue (never-fabricate rule + internal-metadata marker are its AC items) Prompt-rule halves folded into #1. The output-file partial-vs-final detail doesn't transfer 1:1: xdev hub jobs keep results in memory (pull…
- Canonical tool rename map (v2.1.263) :: REJECTED :: fact recorded for #103 foreign-import context Alias table `var i={Task:"Agent",…}` verified verbatim (@157.2MB, lookup `function Tu`) — but it solves CC's own back-compat for legacy CC transcripts/permission rules. xdev never shipped those names (its tool is …
- ScheduleWakeup + RemoteTrigger (scheduling siblings) :: DUPLICATE :: merged into candidate-8's scheduled-tasks issue (one-shot wake = recurring:false, already its AC; RemoteTrigger is Anthropic-hosted surface — §1 non-goal, stays under #22) Candidate itself concedes the merge ("one-shot timer wake as part of the scheduled-task…
- Notifications drain tool (queued out-of-band events) :: DUPLICATE :: #81 (prompt-injection scanning — its AC already names `<untrusted_tool_result>` wrapping; add the count-is-authoritative + spoofed-delimiter + authority-by-named-sender framing there); queue mechanics shipped via #42/#90 mailbox+InboxPoller Mechanism verified…
- Agent teams gating + telemetry vocabulary :: REJECTED :: #22 (M15 full-parity umbrella covers agent-teams when they land) Telemetry keys (tengu_hasUnseenTeamArtifacts, AUTO_REPLIES_* rows) are Anthropic platform surface — §1 reject. The one portable concept, teardown park-grace (CLAUDE_CODE_TEAM_TEARDOWN_PARK_TIMEO…
- ultracode effort mode :: IMPLEMENTED :: internal/agent/keywords.go (ultrathink/orchestrate/workflowz, landed M11; PRD §2 magic-keywords row) CC's ultracode rung = budget raise + orchestration flip (verified @70.5-72.7MB); xdev ships the same pair as keyword notices: `ultrathink` (budget ladder) and…
- Workflow tool I/O schema + plugin workflows dir :: DUPLICATE :: merged into candidate-14's workflow-runner issue (scriptPath/transcriptDir is its opening contract; the plugin `workflows:` key is an AC there, cross-ref #85) Same feature; splitting I/O schema from budget controls would file two issues against one tool that …
- SELF_HOSTED_RUNNER_TOOLS set :: DUPLICATE :: merged into candidate-14 as its runner-owned-tools design note (the same partition also scopes candidate-3/8's registry ownership) Verified symbol (@70.26MB), but it's a classification fact, not a capability; candidate itself says fold if slot pressure. The c…
- Forked-skill scoping sidecar + adopted-agent validation :: REJECTED :: cc-official-docs.md §4 VERIFY line (skill-fork context: fork maps to xdev subagent skills — revisit if that ever ships) Strings verified (.forked-skill.json + scoping errors @75.10/72.56MB), but they guard CC's adopted-agent registry machinery; xdev has no sk…

**Already covered (traceability):**
- Agent tool — isolation worktree/remote contract -> claude-code-internals.md already tables isolation worktree|remote; delta is only the auto-clean-if-unchanged + path/branch-in-result detail (fold into the fork issue).
- Spawn-related hook events (taxonomy delta) -> Event names already in §2 baseline; NEW delta = worktree create/remove hooks + teammate-idle message plumbing + 'non-blockable hook' error concept. Fold into hooks milestone, don't file separat…
- Task tools v2 (TaskCreate/Get/List/Update) + TodoWrite statuses -> §1 recorded the gate and tool names; the 5-status enum (blocked/skipped) is the delta to note in the existing task-tools issue rather than a new one.
- SendMessage/ListAgents (covered baseline) -> Already mapped via internals + PRD §2 cross-session messaging row.
- Agent view / adopt ergonomics envs -> Names catalogued; /agents UI issues already exist. Delta worth noting in existing issue: EXPLORE_PLAN_AGENTS kill-switch as the parity hook for built-in agent types.
- Doc-source caveat -> Orchestrator: treat numeric defaults in notes as unverified; behavior/error contracts are quoted.

**Rejected with reasons:**
- Server-side routines (hosted scheduled agents) :: Anthropic-hosted account surface (claude.ai routines); xdev self-hosts everything; §1 non-goals. Local cron issue covers the portable half.

### model-effort-advisor — 5 confirmed gaps; 0 dup/rejected; 4 covered; 1 rejected-at-research
Method/caveats: Salvage re-run (write-to-file emission). Caveats: fast mode server-side config research-preview/undocumented — client contracts only; advisor pairing table release-drifting, xdev design uses ordinal check; 2.1.265/2.1.267 facts bleed past 2.1.263 build.
**Confirmed gaps (filed):**
- [#142] fast-mode-surface [NICE/M9] — [M9] Speed-mode toggle contract: capability-gated fast mode with switch-coupling and cache-cost disclosure
- [#194] advisor-server-tool-contract [NICE/M11] — [M11] Advisor parity: pull-mode consult tool with capability-pairing validation and headless command form
- [#133] cache-key-membership-and-prefix-scope [NICE/M9] — [M9] Cache-membership contract: what may change mid-session without breaking the prefix (scope, deferral, fan-out hold)
- [#168] unknown-id-window-and-capability-correction [NICE/M9] — [M9] Unknown-model-ID correction surface: per-ID window override, capability declaration, and error-recovery compaction mode
- [#141] flagged-request-fallback-and-safe-mode [NICE/M9] — [M5] Policy-refusal (flagged) fallback class in the retry chain + ask-before-switch note

**Already covered (traceability):**
- effort-default-effort-hold
- pinning-env-and-capability-declaration
- ttl-config-and-stats-surface
- model-picker-lineup

**Rejected with reasons:**
- org-console-model-controls

### changelog-net — 8 confirmed gaps; 0 dup/rejected; 1 covered; 3 rejected-at-research
Method/caveats: 1630 non-fix changelog lines, 417 reviewed vs corpus. Newest CC = 2.1.270 (build under sweep 2.1.263). Cross-domain conflicts resolved at merge: OTel export REJECTED (telemetry domain rejected the same surface on §1 pi-minimal grounds; two independent analysts + policy). Managed-policy tier REJECTED (MDM/console precedent). status-line stdin DEFERRED to the tui-input domain which owns it.
**Confirmed gaps (filed):**
REJECTED AT MERGE (conflict with telemetry-domain §1 reject: no OTLP exporter in the pi-minimal binary): OpenTelemetry exporter surface (env-var ladder, event/span catalog, child scrubbing, TRACEPARENT) — see CHANGELOG-net notes parity: OTEL_* env ladder, canonical event names, child-process scrubbing
DEFERRED TO OWNER: status-line stdin schema is carried by the tui-input domain draft (#228/#229 cluster)
- [#195] bashEditDiffEnabled: changed-files diff appended to Bash tool result [NICE/M3] — [M3] Bash-result edit visibility: optional changed-file diff when bash mutates the tree
- [#196] MCP connect surface: pre-provisioned OAuth client-id/secret, authServerMetadataUrl, alwaysLoad, nonblocking -p connect [NICE/M6] — [M6] MCP connect contract: static OAuth client creds, metadata-URL override, alwaysLoad pin, headless nonblocking
REJECTED AT MERGE (enterprise-MDM precedent from the security domain): managed/admin policy tier depth: drop-ins, refresh gates, kill-switch keys, marketplace governance enforcement [NICE/M13] — [M13] Managed policy surface: drop-in merge order, fail-closed fetch, bypass/auto-mode kill switches, marketplace allow/deny enforcement
- [#197] System-prompt composition controls: includeGitInstructions, commit-trailer default, print-mode prompt flags [NICE/M3] — [M3] Prompt-composition knobs: git-instructions toggle, file-based subagent prompt append, per-request prompt re-render
- [#198] claude plugin eval / eval init: scored, reproducible plugin test harness [NICE/M13] — [M13] Extension eval harness: prompt.md + grader cases, mock MCP servers, CI-gateable scored runs
- [#154] Plugin dependency GC: claude plugin prune / uninstall --prune [NICE/M13] — [M13] Plugin dependency GC: prune orphaned auto-installs, uninstall cascade, skip-reason surfacing

**Already covered (traceability):**
- Background-agent hub fleet semantics -> PRD M11/M14 hub rows

**Rejected with reasons:**
- sandbox.* settings block :: §1 non-goal, external containment only
- otel-export :: conflicts with telemetry-domain reject; stays out of binary
- managed-policy :: enterprise MDM surface, prior reject precedent

### slash-cli-tui — 44 confirmed gaps; 0 dup/rejected; 11 covered; 4 rejected-at-research
Method/caveats: Re-run with write-to-file emission after 4 stalled attempts. 44 new-gap + 11 covered + 4 reject. Hidden-flag findings carry binary-string provenance; docs slightly ahead of installed 2.1.263 flagged inline; statusline/keybindings cross-referenced to the tui-input domain.
**Confirmed gaps (filed):**
- [#199] slash-command-menu-matching [CORE/M10] — [M10] Command-menu matcher: alias-aware prefix highlight, typo fallback, hidden-command full-name reveal
- [#200] skill-chain-invocation [NICE/M12] — [M12] Skill chaining: up to 6 `/skill-a /skill-b ... args` loaded jointly, args passed to each
- [#157] command-queue-during-turn [NICE/M10] — [M10] Mid-turn slash command policy: queue by default, run-now set (/status,/tasks,/usage)
- [#201] copy-command [NICE/M10] — [M10] /copy [N]: clipboard of last/Nth response, code-block picker, `w` writes to file
- [#202] btw-side-question [NICE/M10] — [M10] /btw: side-question channel answered without polluting the transcript
- [#203] cd-command [NICE/M10] — [M10] /cd <path>: relocate session cwd in place, re-derive project context
- [#131] exit-quit-alias-detach [NICE/M10] — [M10] /exit (/quit): detach-not-stop when attached to a background session
- [#204] subtask-forked-background-subagent [NICE/M11] — [M11] /subtask: forked subagent inheriting full transcript, results return to parent conversation
- [#205] reload-skills [NICE/M12] — [M12] /reload-skills: re-scan skill/command roots mid-session and report deltas
- [#206] config-inline-keyvalue [NICE/M9] — [M9] /config key=value: inline non-interactive settings writes with shorthand aliasing
- [#207] session-color [NICE/M12] — [M12] /color: per-session prompt-bar accent from an 8-color set, random default, persisted in session header
- [#208] skills-list-interaction [NICE/M12] — [M12] /skills: filterable listing with token-cost sort and visibility cycling
- [#209] import-foreign-config [CORE/M9] — [M9] /import codex|gemini|cursor: one-shot migration of foreign agent config (instructions, MCP, commands, subagents, skills)
- [#210] code-review-family [NICE/M14] — [M14] /code-review (alias /review): effort-laddered diff/PR review with --fix and --comment
- [#211] fewer-permission-prompts [NICE/M9] — [M9] /fewer-permission-prompts: transcript-mined read-only allowlist into project settings
- [#212] heapdump-diagnostics [NICE/M8] — [M8] Hidden /heapdump: pprof heap profile + RSS breakdown to a file, conversation redacted
- [#142] fast-mode-toggle [NICE/M9] — [M9] /fast on|off: latency-tier model toggle applying from the next request
- [#213] skill-doctor-report [NICE/M12] — [M12] /skill-doctor: per-skill context-cost x usage report driving disable decisions
- [#169] workflows-progress-view [NICE/M11] — [M11] /workflows: run progress view with pause/resume/save over hub jobs
- [#214] release-notes-picker [NICE/M14] — [M14] /release-notes: changelog version picker since installed build
- [#215] command-availability-gating [NICE/M10] — [M10] Command availability gating: hidden vs present-but-refusing with a reason
- [#131] background-session-cli-supervisor [CORE/M10] — [M10] Background-session fleet: --bg + agents/attach/logs/stop/rm/respawn + on-demand supervisor
- [#216] system-prompt-snapshot [CORE/M2] — [M2] Record system prompt once per conversation; replays ignore later flag text until compaction
- [#217] exclude-dynamic-system-prompt-sections [NICE/M5] — [M5] Static-prompt split: --no-dynamic-prompt (per-machine env block into first user message)
- [#218] safe-mode [CORE/M9] — [M9] --safe-mode: config-bisect boot (all user/project customization layers inert, managed still applies)
- [#219] debug-log-surface [NICE/M8] — [M8] --debug category filters (incl. !negation), --debug-file precedence, mid-session start
- [#220] internal-child-cli-contract [NICE/M10] — [M10] Fork/respawn state-carry census: enumerate what a parent passes to a spawned sibling session
- [#221] print-mode-surface-flags [CORE/M10] — [M10] Print-mode contract gaps: --session-id, --init/--maintenance bootstrap, --disable-slash-commands (+ flag-name parity for shipped --max-turns/--no-session…
- [#156] prompt-suggestions [NICE/M10] — [M10] --prompt-suggestions: predicted-next-user-prompt emission as stream message
- [#222] append-subagent-system-prompt [NICE/M11] — [M11] Global subagent prompt append (nested, fork-exempt)
- [#223] session-purge-command [NICE/M10] — [M10] `xdev project purge`: per-project state deletion with explicit inventory
- [#comment->#105] worktree-launch-flag [NICE/M10] — [M10] --worktree [name]: boot the session inside <repo>/.claude/worktrees/<name> with auto-name
- [#147] mid-prompt-command-completion [NICE/M10] — [M10] Mid-prompt slash completion: ghost text +N, Tab accept/list, bare-name plugin matching, insert full name
- [#163] shell-mode-bang [CORE/M10] — [M10] `!` shell mode: direct exec, output-to-transcript, auto-response, bang-history + path completion
- [#157] queue-during-turn-semantics [CORE/M3] — [M3] Mid-turn queue: visible pending list, boundary injection, oldest-becomes-next-turn, Up-recall, per-command immediacy
- [#155] prompt-history-and-ctrlr [NICE/M10] — [M10] History contract: per-cwd prompt log, dup-collapse, paste replay, Ctrl+R inline/dialog with scope cycle
- [#147] emoji-shortcodes [NICE/M10] — [M10] Emoji shortcode completion: inline replace on :name:, popup at 2 chars, boundary-gated, settings kill switch
- [#164] input-spellcheck [NICE/M14] — [M14] Input-box spellcheck via external dictionary subprocess, modes-suppressed
- [#224] session-recap [NICE/M10] — [M10] /recap + unfocused-terminal one-line recap (>=3min idle, 400-char cap)
- [#143] pr-mr-footer-badge [NICE/M13] — [M13] Footer PR/MR badge: state-colored link, push/CLI-triggered refresh, gh/glab shell-out
- [#225] issue-ref-linkification [NICE/M10] — [M10] Issue-ref autolinking: owner/repo#123 only, code-span exclusion, host-from-remote table
- [#139] diff-panel-viewer [NICE/M10] — [M10] /diff: turn-indexed diff views with selection-to-prompt attach and compare-basis cycling
- [#226] output-styles-persona-layer [NICE/M10] — [M10] Output styles: named persona files (keep-coding-instructions, plugin force) selectable in config
- [#227] brief-send-user-message [NICE/M11] — [M11] SendUserMessage-equivalent: opt-in agent->user channel outside turn boundaries

**Already covered (traceability):**
- session-lifecycle-command-set
- model-context-command-set
- config-permission-plugin-command-set
- bundled-skills-inventory
- tui-chrome-commands-owned-elsewhere
- flag-covered-cluster
- btw-overlay-mechanics
- task-list-view-and-sharing
- input-mode-keys-covered
- usage-limit-wait-covered-ref
- vim-editor-mode

**Rejected with reasons:**
- removed-commands
- platform-gated-command-cluster
- feedback-bug-commands
- flag-reject-cluster

### tui-input-surface — 32 confirmed gaps; 18 dup/rejected; 7 covered; 11 rejected-at-research
Method/caveats: Salvaged 57-findings payload from a transcript whose StructuredOutput emit died; verify stage ran gh-search + PRD/code greps per item (32 CONFIRMED of 50 candidates).
**Confirmed gaps (filed):**
- [#228] tui-item-01: User status line as a subprocess command over the session JS [CORE/M12] — [M12] User status line as a subprocess command over the session JSON payload
- [#229] tui-item-02: subagentStatusLine: command-fed row override for the hub ros [NICE/M12] — [M12] subagentStatusLine: command-fed row override for the hub roster
- [#148] tui-item-03: Context-scoped keybinding file with action names, null-unbin [CORE/M10] — [M10] Context-scoped keybinding file with action names, null-unbind and hot reload
- [#148] tui-item-04: Named scroll/selection actions so wheel, keyboard and select [NICE/M4] — [M4] Named scroll/selection actions so wheel, keyboard and selection are rebindable
- [#148] tui-item-05: Chord bindings: multi-keystroke sequences, 3 s window, prefi [CORE/M10] — [M10] Chord bindings: multi-keystroke sequences, 3 s window, prefix reservation
- [#148] tui-item-06: Keystroke grammar: modifier aliases, case-insensitive keys,  [CORE/M10] — [M10] Keystroke grammar: modifier aliases, case-insensitive keys, Super/cmd reachability
- [#148] tui-item-07: Vim layer independence: Esc owns mode-switch, keybindings st [DEFERRED/M4] — [M4] Vim layer independence: Esc owns mode-switch, keybindings stay a separate layer
- [#148] tui-item-08: Readline word-editing keys, punctuation boundaries and a Ctr [CORE/M10] — [M10] Readline word-editing keys, punctuation boundaries and a Ctrl+Y kill ring
- [#165] tui-item-09: Click-to-position, click-to-expand, click-to-accept: mouse c [CORE/M4] — [M4] Click-to-position, click-to-expand, click-to-accept: mouse contract over the transcript
- [#165] tui-item-10: Clipboard chain completion: PRIMARY, tmux buffer, Set-Clipbo [NICE/M4] — [M4] Clipboard chain completion: PRIMARY, tmux buffer, Set-Clipboard, screen guardrail
- [#165] tui-item-11: Wheel scroll-speed multiplier with a /scroll-speed dialog an [NICE/M4] — [M4] Wheel scroll-speed multiplier with a /scroll-speed dialog and acceleration toggle
- [#165] tui-item-12: Synchronized output: DEC 2026 capability probe, force env, a [NICE/M4] — [M4] Synchronized output: DEC 2026 capability probe, force env, and stray-reply guard
- [#165] tui-item-13: $EDITOR round trip: clean suspend, full repaint, no leaked p [NICE/M4] — [M4] $EDITOR round trip: clean suspend, full repaint, no leaked paste markers
- [#165] tui-item-14: tmux recipe + limitations documented in the container/sandbo [NICE/M4] — [M4] tmux recipe + limitations documented in the container/sandbox reference
- [#230] tui-item-15: /terminal-setup: idempotent per-terminal config writer with  [NICE/M10] — [M10] /terminal-setup: idempotent per-terminal config writer with backups
- [#231] tui-item-16: Notification channels: preferredNotifChannel enum, terminal  [NICE/M12] — [M12] Notification channels: preferredNotifChannel enum, terminal bell, OSC 9;4 progress
- [#232] tui-item-17: Terminal input quirks: ^H backspace platform rule and Option [NICE/M10] — [M10] Terminal input quirks: ^H backspace platform rule and Option-as-Meta diagnostics
- [#233] tui-item-18: Paste collapse placeholders, paste cache, and never-submit-a [CORE/M10] — [M10] Paste collapse placeholders, paste cache, and never-submit-a-dead-placeholder
- [#234] tui-item-19: Custom theme deltas: base fallthrough, ansi256()/ansi:<name> [NICE/M12] — [M12] Custom theme deltas: base fallthrough, ansi256()/ansi:<name> values, custom:<slug> id
- [#143] tui-item-20: footerLinksRegexes: origin-pinned, scheme-allowlisted footer [NICE/M12] — [M12] footerLinksRegexes: origin-pinned, scheme-allowlisted footer badges with a 5 cap
- [#128] tui-item-21: Screen-reader render mode: flat-text output, label vocabular [CORE/M4] — [M4] Screen-reader render mode: flat-text output, label vocabulary, reader-timing waits
- [#165] tui-item-22: OSC 133 turn-boundary markers for terminal prompt-jumping [NICE/M4] — [M4] OSC 133 turn-boundary markers for terminal prompt-jumping
- [#128] tui-item-23: prefersReducedMotion switch, daltonized presets, magnifier c [NICE/M12] — [M12] prefersReducedMotion switch, daltonized presets, magnifier cursor
- [#235] tui-item-24: Draft stash (Ctrl+S), input undo (Ctrl+_), image-paste chips [NICE/M10] — [M10] Draft stash (Ctrl+S), input undo (Ctrl+_), image-paste chips (Ctrl+V)
- [#147] tui-item-25: Mid-prompt trigger rules: `:` emoji, `?` help affordance, ba [NICE/M10] — [M10] Mid-prompt trigger rules: `:` emoji, `?` help affordance, bare-name skill match, ghost-vs-list
- [#163] tui-item-26: Shell mode: `!` prefix runs without the model; output lands  [CORE/M10] — [M10] Shell mode: `!` prefix runs without the model; output lands in context
- [#157] tui-item-27: Queue-during-turn UI: listing, delivery points, take-back, i [CORE/M10] — [M10] Queue-during-turn UI: listing, delivery points, take-back, immediate exceptions
- [#155] tui-item-28: Persistent per-cwd prompt history with Ctrl+R inline and dia [CORE/M10] — [M10] Persistent per-cwd prompt history with Ctrl+R inline and dialog search
- [#156] tui-item-29: Next-prompt suggestions: git-history opener + cached-prompt  [DEFERRED/M10] — [M10] Next-prompt suggestions: git-history opener + cached-prompt follow-up, demand-gated
- [#164] tui-item-30: Spell-check seam: aspell → hunspell → ispell subprocess with [NICE/M13] — [M13] Spell-check seam: aspell → hunspell → ispell subprocess with circuit breakers
- [#139] tui-item-31: Diff viewer keys and line-selection-attaches-to-prompt [NICE/M10] — [M10] Diff viewer keys and line-selection-attaches-to-prompt
- [#236] tui-item-32: Dialog-context key precedence: per-surface catalogs for pick [NICE/M10] — [M10] Dialog-context key precedence: per-surface catalogs for pickers, tabs, footers, sliders

**Cross-checked away:**
- 1 :: DUPLICATE :: [M12] User status line as a subprocess command over the session JSON payload
- 2 :: DUPLICATE :: [M12] User status line as a subprocess command over the session JSON payload
- 3 :: DUPLICATE :: [M12] User status line as a subprocess command over the session JSON payload
- 6 :: DUPLICATE :: [M10] Context-scoped keybinding file with action names, null-unbind and hot reload
- 10 :: DUPLICATE :: [M10] Context-scoped keybinding file with action names, null-unbind and hot reload
- 13 :: ALREADY-DOCUMENTED :: 
- 14 :: ALREADY-DOCUMENTED :: 
- 15 :: ALREADY-DOCUMENTED :: 
- 16 :: ALREADY-DOCUMENTED :: 
- 19 :: DUPLICATE :: [M4] Click-to-position, click-to-expand, click-to-accept: mouse contract over the transcript
- 21 :: ALREADY-DOCUMENTED :: 
- 22 :: ALREADY-DOCUMENTED :: 
- 23 :: IMPLEMENTED :: 
- 24 :: REJECTED :: 
- 31 :: DUPLICATE :: [M10] Terminal input quirks: ^H backspace platform rule and Option-as-Meta diagnostics
- 34 :: DUPLICATE :: [M12] Custom theme deltas: base fallthrough, ansi256()/ansi:<name> values, custom:<slug> id
- 37 :: DUPLICATE :: [M4] Screen-reader render mode: flat-text output, label vocabulary, reader-timing waits
- 38 :: DUPLICATE :: [M4] Screen-reader render mode: flat-text output, label vocabulary, reader-timing waits

**Already covered (traceability):**
- 13
- 14
- 15
- 16
- 21
- 22
- 23

**Rejected with reasons:**
- 1
- 2
- 3
- 6
- 10
- 19
- 24
- 31
- 34
- 37
- 38

---
*Generated 2026-09-14 from the cc-feature-resweep workflow family (runs wf_48cd71bb-adb, wf_7174b607-a3c, wf_5bee629d-ccf + salvage agents). Baselines consulted: cc-official-docs.md, cc-bundle-forensics.md, claude-code-internals.md, PRD §5.1/§5.2.*

## Wave summary

112 issues filed #128-#239 (2026-09-14) + scope comments on #91/#92/#105/#117/#123.
Milestone distribution: M10×32, M11×10, M12×13, M13×8, M14×6, M15×3, M2×4, M3×4, M4×2, M5×5, M6×3, M7×1, M8×3, M9×18.
