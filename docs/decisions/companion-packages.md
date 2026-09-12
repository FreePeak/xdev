# Companion packages — Go equivalents (M15 #72)

Reference: `omp://user-facing-packages.md`. Decision recorded 2026-09-12.
Rule applied to every package below: build it only when the Go equivalent has
a **consumer inside the agent**; otherwise record the seam and stop. No empty
stubs, no sidecar databases (PRD §1 non-goal: "no `stats.db`-scale sidecar
accumulation — a tiny rollup table only").

| Package | omp shape | Decision | xdev form |
| --- | --- | --- | --- |
| `omp-stats` | session-JSONL usage dashboard, port 3847, `--summary`/`--json`, `stats.db` rollup | **SHIPPED** | `xdev stats` (+ loopback dashboard) |
| `mnemopi` CLI | store/recall/update/delete/stats/sleep/export/import/scratchpad/bank over the memory backend | **SHIPPED (subset)** | `xdev memory <sub>` |
| `metaharness` | benchmark manager: Harbor/edit/SnapCompact adapters, SQLite store, REST+SSE dashboard | **DEFERRED** | seam: `xdev bench` (§ Bench harness) |
| `robomp` | GitHub webhook triage: resume an RPC session per issue, comment or open a fix PR | **DEFERRED** | seam: `xdev serve` webhook route (§ Webhook triage) |
| `omptype` | schema authoring library + validation | **REJECTED** | none needed — see below |
| `snapcompact` | bitmap image compaction API (Rust/PNG natives) | **REJECTED** | none needed — see below |

## Shipped: `xdev stats`

`internal/stats` + `cmd/xdev/statscmd.go`. Aggregates the session JSONL store
into sessions / turns / user messages / tool calls / tokens (input, output,
cache read, cache write) / reported cost / per-model / per-tool / per-day, with
nearest-rank percentiles over sessions.

- `xdev stats` — plain-text table; `--json` — the same `Report` struct.
- `xdev stats --serve` — dashboard on `127.0.0.1:3847`: server-rendered totals
  plus an SSE stream that swaps the fragment, self-contained (inline CSS/JS, no
  external assets, no build step). Non-loopback binds are refused: the page
  shows session titles, cwds and model usage with no auth.
- **Rollup instead of `stats.db`**: `<agent dir>/stats/rollup.json` is keyed by
  session path `(size, mtime)` and only carries files the current scan touched,
  so it cannot grow past the scan cap. Repeat scans re-read only changed files;
  the dashboard's 5s tick is cheap.
- **Bounded scan**: newest 2000 files / 512 MiB of JSONL by default (`--limit`,
  `--max-bytes`), reported in the output as `truncated` when hit.
- **Cost is what the providers reported** (`usage.cost.total` summed), not a
  price table: a provider that reports nothing contributes 0 and
  `pricedTurns` says how many turns actually carried a price. This is why a
  local onegw-backed store shows `$0.0000` today.
- Sessions are decoded through a lean wire struct rather than
  `session.Open`: one long session is tens of megabytes of tool output that
  stats never reads, so peak memory stays at one line (RSS budget).

## Shipped: `xdev memory`

`internal/memory/cli.go` (CLI-facing helpers) + `cmd/xdev/memorycmd.go`. Every
subcommand reads/writes exactly the files the runtime already uses — the
injected `MEMORY.md`, the append-only `learned.md` the `learn` tool writes, and
a new bounded `scratchpad.md` (16 KiB, over-cap writes are rejected rather than
silently truncated). No second store, no second format.

`show [--injected]` · `stats [--json]` · `lessons [--json] [--limit N]` ·
`add [--context C] <text|->` · `edit -` (stdin → `MEMORY.md`) ·
`export [-o file]` · `import [--merge] <file|->` · `scratchpad [--clear|<text|->]` ·
`clear --yes`.

Omitted from the mnemopi surface, with the reason:

- **`bank`** (multiple named stores) — one local backend has one namespace; a
  second bank has no consumer until profiles need per-profile memory.
- **`recall`** — already the `memory://root` read seam plus prompt injection.
- **`sleep`** — already the two-phase pipeline (`memoryPipeline: on`,
  `internal/memory/pipeline.go`) with its own lease/watermark.
- **`store/update/delete` by id** — lessons are an append-only log, and
  `MEMORY.md` is a single consolidated file: `add` + `edit` + `clear --yes`
  cover the real operations without inventing stable ids.

Import/export is a lossless JSON bundle (`version`, `summary`, `lessons`,
`scratchpad`); `--merge` keeps the existing summary and appends only lessons
whose content is not already present, so re-importing is idempotent. A rejected
bundle (bad version, over-cap scratchpad) leaves the store untouched.

## Deferred: `metaharness` (benchmark manager)

**What it is:** a harness that runs a benchmark suite against the agent
(Harbor/edit/SnapCompact adapters), stores runs in SQLite, and serves a
REST+SSE dashboard.

**Why not today:** the value is the *evaluation*, and xdev already has the
evaluator — the `eval` tool's persistent NDJSON kernel plus the RPC mode
(`xdev rpc`) which drives the agent end-to-end. What is missing is the run
matrix and the score store, i.e. a second SQLite database (explicit PRD
non-goal) and a second HTTP surface, for a workflow nobody has asked to run
locally yet. Building the adapters before the benchmark corpus exists would be
stubs with extra steps.

**Seam:** `xdev bench` — a subcommand that (1) takes a task list (a directory of
prompts + assertions), (2) drives one RPC session per task, reusing
`internal/rpc` and the child-session machinery the `task` tool already uses,
(3) writes one JSON row per run into `<agent dir>/bench/runs.jsonl` — the same
tiny-rollup stance as `xdev stats`, so the two dashboards can share
`internal/stats`' renderer, and (4) exposes the run table through the existing
`xdev stats --serve` page instead of a second server.

## Deferred: `robomp` (GitHub webhook triage)

**What it is:** an always-on service that receives GitHub webhooks, resumes an
RPC session per issue, and comments on or opens a fix PR.

**Why not today:** it is the only package that must be reachable from outside
the machine. That buys an inbound listener, a shared secret, a durable queue,
`gh` auth on the server, and a retry/idempotency story — a large attack surface
and an unbounded background RSS footprint for a workflow that needs a hosted
deployment xdev does not have. The half that is genuinely useful locally
(RPC session per issue, comment/PR via `gh`) is already reachable by hand: the
`github` tool wraps `gh`, and `xdev rpc` is the session driver.

**Seam:** `xdev serve` — one multi-command local server (issue #71 already
plans `xdev serve` for the auth-broker/browser-relay services) with a
`/webhook/github` route that verifies `X-Hub-Signature-256`, maps
`issues.opened`/`issue_comment` to a queue file, and spawns one headless
`xdev rpc` child per issue keyed by issue number for idempotency. Until that
exists, deliberately nothing listens.

## Rejected: `omptype`

**What it is:** a schema-authoring library (typed tool definitions + runtime
validation).

**Why not:** xdev's tool contract already *is* typed: `tool.Tool` carries a
`json.RawMessage` JSON Schema produced from ordinary Go structs, the registry
in `cmd/xdev/print.go` is the single registration point, and the model-facing
schemas are emitted from it. A separate authoring library would add a second
way to declare the same thing (and a reflection/validation layer at every tool
dispatch) with no consumer: extensions ship tools over the extension protocol,
and MCP servers ship their own schemas.

## Rejected: `snapcompact`

**What it is:** bitmap-image compaction (encode/decode + shrink bitmaps),
implemented in omp with Rust natives.

**Why not:** porting it is technically easy in Go (`image/png` is stdlib, no
CGO), but nothing in xdev renders or compacts bitmaps: the compaction ladder is
summary-based text (`internal/agent` thresholds → summary messages), the
session store keeps images as blob references, and the TUI never rasterizes
them. A port would be dead code with a test suite.

**Seam, if a bitmap method is ever added to the compaction ladder:** it takes a
`[]byte` image and returns a compacted `[]byte` plus its dimensions; the
existing sha256 blob store (`internal/session/blob.go`) is where the result
lands. Add it then, behind that one function.
