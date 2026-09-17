# Decision: LeanKG as the xdev memory backend

*Reference: `leankg/AGENTS.md`, `leankg/docs/prd.md` v4.12.2 (#414) · Milestone: M12 (memory) follow-on · Date: 2026-09-15 · Status: **A adopted in part — X1/X2/X4 client half implemented; server side (K1-K6) still open***

**Decision in one line: the transport already exists on both sides — LeanKG's `serve --memory --hindsight-compat` mounts the exact wire xdev's `hindsight` client speaks, so integration is possible today with zero new backend code. What does not exist is the *scope contract*: LeanKG pins its memory tree under each indexed repository, xdev's bank/tag derivation diverges from LeanKG's, and the compat surface is missing the read endpoints xdev's `memory://` seam and `/memory stats` need. The features below are what closes those gaps; the first-class `memory: leankg` enum arm is deferred until they do.**

---

## 1. Context

### What already works (measured in the tree, 2026-09-15)

| Piece | Where | State |
| --- | --- | --- |
| xdev remote-memory backend | `internal/memory/hindsight.go` (1252 LOC), `hindsight_tools.go` | shipped (M12 #43) — stdlib `net/http`, offline retain queue, recall TTL cache, `Store` + `PipelineStore` seam, `recall`/`retain`/`reflect` tools |
| LeanKG server side of that wire | `leankg/internal/rest/hindsight.go:57-165`, flag at `cmd/leankg/main.go:259` | shipped (#414) — `PUT /v1/default/banks/{bank}`, `POST …/memories`, `POST …/memories/recall`, `POST …/reflect` |
| xdev backend seam | `internal/memory/memory.go:48-97` (`Store`, `PipelineStore`) | 4 implementations (`Backend`, `Mnemopi`, `SharpShooter`, `Hindsight`) satisfy it behind a compile-time assertion |
| Wiring dispatch | `cmd/xdev/print.go:1263-1292` (`buildMemory`) | one `switch settings.Memory`; every consumer is backend-agnostic *by contract* |
| Recall/retain/reflect tool registration | `cmd/xdev/print.go:1553-1568` | keyed off the concrete type |

**Consequence:** `xdev` with `memory: hindsight` + `hindsight.apiUrl: http://127.0.0.1:8080` against `leankg serve --rest :8080 --memory --hindsight-compat` is usable immediately. Auto-recall injection, cadence-based auto-retain, the offline queue, `learn`-into-LeanKG and the three model tools all light up with no code change in either repository.

### What is broken about it

**G1 — LeanKG's memory root is pinned to the repository it indexes.**
`memory.Open(dir, false)` at `cmd/leankg/main.go:308` and `internal/projects/projects.go:263` always takes the project branch, so the tree lives at `<project>/.leankg/memory` (`MEMORY.md`, `USER.md`, `topics/*.md`, `banks/<bank>.jsonl`, `index.db`). `Open`'s `global` argument — which selects `~/.leankg/memory` (`internal/memory/memory.go:67-80`) — is never reachable from `serve`. Two problems follow: agent transcripts land inside the user's working tree (git-noise, and the write is unconditional once `--memory` is on), and one bank per project means **no cross-project recall** — which is the entire reason to run a memory server instead of the local `MEMORY.md`.

**G2 — the two sides derive bank and tag names differently.**

| | xdev (`hindsight.go:455-465`, `ProjectScope`) | LeanKG (`memory/banks.go:49`, `BankName`) |
| --- | --- | --- |
| root anchor | `findRepoRoot(cwd)` — the **git** root | the **cwd**, deliberately never the git root (upstream bug #2412) |
| bank id | `<base>-<hex(sha256(root))[:12]>` (`per-project`) | `<basename>-<base36(wyhash64(cwd))>` sanitized ≤64 |
| tag | `project:<lowercased basename>` | none — tags ride in `Entry.Metadata` only if the client sent them |
| cross-project read | never (single bank) | `leankg-shared` (`memory/scope.go:24`, `SharedBank`) |

Different root, different hash, different namespace. In xdev's default `per-project-tagged` scoping the recall request carries `tags:["project:<base>"]`, and LeanKG's compat tag filter (`hindsight.go:194-213`) drops every row that does not literally carry it — so a bank written by `leankg`'s own `session_retain` reads back as empty, and vice versa. This is the one gap that must be closed in a single place, not mirrored in both repos.

**G3 — the compat surface is write-capable but not read-capable.**
`Hindsight.Read` (`hindsight.go:605-624`) resolves only `memory://root`; a `memory://<id>` read is what the injected block tells the model to use before it edits anything. `Hindsight.Stats` (`:653-668`) issues `GET …/stats`, which the compat mount does not serve, so `/memory stats` prints "server unreachable" against a healthy LeanKG. `reflect` (`rest/hindsight.go:144-165`) is a top-5 row digest and ignores the client's `tags`/`budget`/`max_tokens`.

**G4 — auth and liveness on the compat mount are wrong today.**
`isWritePath` (`internal/auth/auth.go:105-112`) guards the *native* retain route (`/api/v1/memory/banks/*/memories`) but the compat alias lives at `/v1/default/banks/*/memories` and matches nothing — retain is open to the `Viewer` role the moment tokens are configured. Conversely the whole REST listener sits behind `auth.MiddlewareWithStore` (`cmd/leankg/main.go:417`), including `GET /health` (`internal/rest/rest.go:39`), so xdev's `/memory diagnose` (`hindsight.go:991-998`) reports "unreachable: HTTP 401" for a server that is up.

### Invariants this decision must not break

1. **One CGO-free static binary** (PRD §1 goal 1). LeanKG's Go engine is likewise `CGO_ENABLED=0` by default — no cgo driver, no `dlopen`.
2. **< 100 MB RSS** (M8 gate) — a second process is fine; a second in-process store is not.
3. **A dead memory server must never break a session.** The existing client guarantees this (recall → nothing, retain → bounded queue, one warning). Keep it.
4. **`memory.backend` is a closed enum** (`config/settings.go:758-764`, validated at `:1409`, echoed by `xdev config dump` at `:1694-1698`). A new arm is a settings-schema change, and `xdev config set memory` reads its vocabulary from the same function so the two cannot drift.
5. **No secrets on the wire or in the tree** (`.env`-never rule): the token is `hindsight.apiToken` / `LEANKG_TOKEN_*` from the environment, never pasted into a committed settings file.
6. **No new Go-module dependency.** The client is stdlib `net/http` and must stay that way; sharing `internal/store` across two modules is not on the table.

---

## 2. Options

| Option | Shape | Verdict |
| --- | --- | --- |
| **A — Compat wire only** | No new backend name. `memory: hindsight`, `hindsight.apiUrl` → LeanKG. Fix G1-G4 on the server, one guard + one naming fix on the client. | **Recommended.** Smallest diff that is actually correct; the plumbing is shipped and tested on both ends. |
| **B — First-class `memory: leankg`** | New enum arm + `leankg.*` settings group; the remote machinery (turn cadence, queue, recall cache, warn sink, diagnose) is lifted out of `*Hindsight` behind the seam so both remote backends share it. | Right end state, bigger blast radius. Needs A's server fixes first, otherwise it just renames the bug. |
| **C — MCP only** | `leankg connect --target` writes the MCP entry; the model gets `set`/`get`/`status`. Zero xdev code. | Rejected as *the* memory backend: no auto-recall injection, no `memory://`, no `/memory`, no `learn`. Keep as an **additive** layer — the code-graph reads (impact radius, traceability) are worth having alongside memory. |

The three are cumulative, not exclusive: A now, C alongside it (it is one config entry), B when the remote adapter is generalized.

---

## 3. Needed features — LeanKG

Ordered by dependency, not effort. K1/K2/K4/K6 are prerequisites for anything downstream.

| # | Feature | Why | Where |
| --- | --- | --- | --- |
| **K1** | **Global/portfolio memory root for `serve`** (`--memory-root <dir>`, or `--memory-global`) | Breaks G1: banks move out of every indexed repository into `~/.leankg/memory` (or a configured dir) and cross-project recall becomes possible. `Memory.Open(dir, global)` already supports it — only the wiring is missing. Decide the single-writer rule at the same time (`leankg writer --project` vs agent retains on one JSONL file). | `cmd/leankg/main.go:308`, `internal/projects/projects.go:261-277` |
| **K2** | **First-class tags on `Entry`** | Breaks half of G2 and removes the metadata coupling: compat retain currently buries `tags[]` inside `Entry.Metadata` (`rest/hindsight.go:85-87`), so LeanKG's own scope matrix and tag filtering depend on a client's private key names. A `Tags []string` field on the JSONL row (with a read-side fallback for existing banks) makes `project:<name>` filtering explicit and keeps it out of `session_retain` rows. | `internal/memory/banks.go:20-31` (`Entry`), `internal/rest/hindsight.go:81-96` |
| **K3** | **`GET /v1/default/banks/{bank}/memories/{id}` + `GET …/memories` (list)** | Breaks G4-read half: xdev's `memory://<id>` seam and the `read before memory_edit` instruction need a row by id. Also delivers the `memory_get` / `memory_update` / `memory_forget` mirror the PRD promises (§3.5, "id-stable … mirrors") which the Go tree does not have — `core.go` memory commands cover file/`MEMORY.md` writes and `session_retain` only. | `internal/rest/hindsight.go`, `internal/rest/rest.go`, `internal/core/core.go:640-710` |
| **K4** | **`GET /v1/default/banks/{bank}/stats`** | Breaks G3 for `/memory stats`: xdev already calls it and prints `server unreachable` today. Body: entry count, banks, last-retain timestamp, memory root path, disk bytes. | `internal/rest/hindsight.go`, consumer `internal/memory/hindsight.go:653-668` |
| **K5** | **Honest `reflect`** | Compat returns a `- ` bullet digest of the top 5 recalled rows. Either (a) drive it through `internal/summarize`'s existing `LEANKG_LLM_*` provider port, or (b) keep the digest and rename the client's tool description so nobody reads a synthesis into it. Also: honor the `tags`/`budget`/`max_tokens` fields the client already sends, and the `limit` recall currently ignores (`defaultRecallLimit = 8` overrides the request). | `rest/hindsight.go:108-165`, `internal/summarize/llm.go` |
| **K6** | **Auth + liveness fixes on the compat mount** | Breaks G4: add `/v1/default/banks/` retain to the write-path rule (or key it off the method+suffix), and move `GET /health` outside the bearer gate the way `/api/v1/auth/*` already is. | `internal/auth/auth.go:97-112`, `cmd/leankg/main.go:403-420` |
| **K7** | **Recall ranking depth** | `RecallBanks` (`banks.go:423-508`) re-reads and re-tokenizes every bank JSONL line per call — O(rows) with no index, no freshness decay, no embedding tier. LeanKG already has L3 ANN in the store (`internal/embed`); recall should use it, and zero-match entries should stay excluded (that guard is load-bearing). `ponytail:` today's scan is fine to ~10⁴ rows/bank; upgrade path is an FTS5/embedding index over `banks/*.jsonl` — the markdown tree already has one (`memory/search.go`). | `internal/memory/banks.go` |
| **K8** | **Retain idempotency** | Compat documents `update_mode:"replace"` as append (`hindsight.go:16`), so a harness re-retaining one session duplicates rows forever. Dedupe on `document_id`/entry id, or make append the stated contract and drop the field. | `rest/hindsight.go:64-106`, `memory/banks.go:348-398` |
| **K9** | **`leankg connect\|install --target xdev`** | Option-C ergonomics: the client list (`cmd/leankg/connect.go:34-42`) is `claude-code\|cursor\|codex\|gemini\|opencode\|omp`, all JSON/TOML. xdev reads `mcp.yml` (`internal/mcpclient/config.go:42-68`, `Config.Servers`) and `settings.yml`, so it needs a YAML writer — ~40 lines beside the omp writer, idempotent merge like the existing ones. | `cmd/leankg/connect.go:28-160` |
| **K10** | **Docs + tracker row** | Record the pairing (`xdev memory:hindsight → leankg serve --hindsight-compat`), the scoping decision from §4, and the single-writer rule, in `docs/prd.md` + `docs/prd-task-tracker.md` (LeanKG's convention: those are the only two live docs). | `leankg/docs/` |

## 4. Needed features — xdev

| # | Feature | Why | Where |
| --- | --- | --- | --- |
| **X1** | **Bank identity parity with LeanKG** | Breaks G2 in one place. Either xdev adopts `BankName`'s rule (cwd-only, wyhash36) or LeanKG adopts xdev's (git root, sha256[:12]) — but exactly one repo changes, and the compat request should carry enough (bank id + tags) that the other cannot drift. Today a mismatch is silent: recall returns nothing and the session continues with no memory. | `internal/memory/hindsight.go:455-465` (`ProjectScope`, `NewHindsight` bank) ↔ `leankg/internal/memory/banks.go:49` |
| **X2** | **Name an unresolvable scope once** — **done (narrowed)** | Companion to X1: with no repository root, the project tag degrades to the working directory's own name — a tag unrelated sessions share — and recall silently returns nothing (measured against LeanKG: a tag no entry holds yields 0 results, not a global answer). The backend warns once through the existing warn sink, naming the URL and pointing at `hindsight.projectSelector`. Fail-fast was rejected: a scratch directory must not refuse to start a session. | `internal/memory/hindsight.go` (`warnScope`), sink `cmd/xdev/print.go:497` |
| **X3** | **Wiring + wire tests** | Repo already pins the dispatch (`cmd/xdev/memory_wire_test.go` "TestBuildMemorySelectsBackend", `memory_backend_test.go`) and the HTTP wire (`hindsight_test.go`, httptest). Extend both; add one byte-shape test against a live `leankg serve --hindsight-compat` gated on an env var, mirroring LeanKG's `LEANKG_TEST_PG_URL` convention (unit tests stay in-process — LeanKG AGENTS.md test-layer policy). | `cmd/xdev/*_test.go`, `internal/memory/hindsight_test.go` |
| **X4** | **Project selector** — **done** | LeanKG routes multi-project servers by `?project=` (`cmd/leankg/multiproject.go:41-53`); xdev never sent it, so every write landed on the server's default project. Now `hindsight.projectSelector` (env `HINDSIGHT_PROJECT`) rides as `?project=` on the bank paths only — never on `/health`, which the server answers itself, outside any project. Empty sends no query, so a single-project server is byte-unchanged. | `internal/config/settings.go`, `internal/memory/hindsight.go` (`bankPath`), `cmd/xdev/print.go` (`hindsightKey`) |
| **X4b** | **Bank pinning** — **done via config** | `hindsight.bankId` already names the bank, so an existing server-side bank (`leankg/.leankg/memory/banks/omp.jsonl`) is reachable without a new method — the fix was to stop letting a *derived* default shadow it, and to include the bank in `hindsightKey` so two settings differing only in scope do not share one cached backend. | `cmd/xdev/print.go` (`hindsightKey`), `internal/config/settings.go` |
| **X5** | **`memory://<id>` read** | Client half of K3: resolve `memory://<id>` through `GET …/memories/{id}` and keep `memory://root` as the injected view. | `hindsight.go:605-624` |
| **X6** | **First-class `memory: leankg`** *(Option B only)* | New enum arm (`settings.go:758-764` + the error text at `:1409` + the dump at `:1694`), a `leankg:` settings group mirroring `HindsightSettings` (`:201-245`), and a `buildMemory` case. Keep `hindsight` working — it is the wire, not the product. | `internal/config/settings.go`, `cmd/xdev/print.go:1263-1292` |
| **X7** | **Generalize the remote adapter** *(Option B only)* | Five call-sites type-assert `*memory.Hindsight` (`print.go:490,675,1488`; `print.go:1564`; `tui.go:1668`) for `SetWarnSink`/`NoteUserTurn`/`RetainAsync`/`EndSession`/`Diagnose`/`Enqueue`/`CompactionContext`, and `memoryTurnHooks` (`print.go:1454-1462`) only recognizes mnemopi's cadence. A second remote store would copy the queue and split the recall cache. Lift to two small interfaces — `TurnStore{NoteUserTurn, RetainAsync, EndSession}` and `Diagnostics{Diagnose, Enqueue}` — asserted against the `Store` the callers already hold; `hindsight` and `leankg` both implement them. | `internal/memory/*.go`, `cmd/xdev/print.go`, `cmd/xdev/tui.go:1640-1670` |
| **X8** | **`/memory` verb arms** | `MemoryOps.Dispatch` (`internal/tui/commands.go:219-240`) is wired per concrete backend in `tui.go:1640-1670`; one new arm routes view/stats/clear/queue/sync/enqueue/diagnose. `xdev memory <sub>` (the M15 #72 CLI) stays local-only — that is already true of `hindsight` and `sharpshooter`, not a regression. | `cmd/xdev/tui.go`, `internal/tui/commands.go` |
| **X9** | **Learn path: verify, don't build** | `LearnTool` scrubs configured secrets through `Redactor` *before* `SaveLesson` (`internal/memory/learn.go:86-97`), so a remote retain inherits the redaction. Add one test proving a lesson containing an API key reaches the server scrubbed — the injection risk is that stored secrets come back in every later session. | `internal/memory/learn_gates_test.go` |
| **X10** | **Docs + PRD row** | `docs/PRD.md` §2 matrix row for the LeanKG backend, and this file updated when the decision moves from proposed. | `docs/PRD.md` |

---

## 5. Sequencing

1. **Decide scoping** — K1 + X1 + X4 together. Recommended: one LeanKG memory root outside the repositories (K1), xdev selects a project with `?project=` (X4) and sends `scoping: per-project` so the bank *is* the scope; tags then carry only optional labels, and the naming divergence (X1) reduces to adopting `BankName(cwd)` in one place.
2. **LeanKG blockers** — K1, K2, K4, K6. Gate: a `curl` walk of ensure → retain → recall (tag-scoped) → reflect → stats against a live `serve --memory --hindsight-compat`, plus the auth regression test for the compat write path.
3. **xdev, no enum change** — X1, X2, X3, X4. After this step the whole feature is usable through `memory: hindsight` with `hindsight.apiUrl` pointed at LeanKG, which is the ship-early half.
4. **Read parity** — K3/K5/K7/K8 with X5 (`memory://<id>`), then the `/memory` surface (X8) and the learn-redaction test (X9).
5. **First-class arm** — X6/X7 once more than one remote backend exists, plus K9/K10 and the optional MCP layer (`leankg connect --target xdev` → `set`/`get`/`status` for impact radius and traceability, which the memory seam does not provide).

## 6. Rejected / out of scope

- **Sharing Go code between the modules.** `xdev` importing `github.com/FreePeak/LeanKG/internal/…` is impossible (internal package) and undesirable (a second SQLite driver, a store schema, and a 30 MB binary growth for a client that speaks HTTP).
- **Forking the enum into a pluggable `memory.backend: mcp`.** The seam already abstracts storage (`Store`/`PipelineStore`); a generic MCP-shaped backend would need per-server knowledge of recall response shapes to implement it. Two named remotes is honest; N is not a requirement yet.
- **Bulk-indexing a portfolio root into LeanKG.** LeanKG's own milestone note forbids it ("Never bulk-index the `freepeak` portfolio root", AGENTS.md). Memory banks are not exempt from the same shape: one root, scoped banks, per-project selection.
- **Editing server-side memories from xdev.** `memory_edit` is deliberately absent from the remote tool trio (`hindsight_tools.go:12-14`); K3 adds a *read*, not a write path, until LeanKG has revision semantics (K8).
- **Writing memory into the indexed tree on purpose.** If K1 is declined, `--memory` keeps creating `<project>/.leankg/memory/` with agent transcripts inside it, and every consumer of that repository inherits the git-noise. That trade-off needs an explicit owner decision, not a silent default.

## 7. Verification state at time of writing

- `leankg`: `go vet ./internal/rest/ ./internal/memory/` clean.
- `xdev`: `go build ./...` **fails in the working tree, unrelated to this plan** — `internal/ai/openai_responses.go:182` calls `imageURL(...)`, which no file in `internal/ai/` defines (`HEAD` has 0 hits for the symbol; the multimodal change is uncommitted and mid-flight). No xdev test can run until that lands, so none of X3's assertions are executed here.

### Client half measured against a live server (2026-09-16)

`leankg serve --http :9699 --rest :9700 --memory --embed-provider local --hindsight-compat --project .` (single project: no `LEANKG_PROJECT_DIRS`), bank `omp` with 21 rows:

- `/health` → 200 with or without `?project=`; `/health?project=<unknown>` still 200 — the route is not project-dispatched, which is why the client never puts a selector on it.
- `/v1/default/banks/omp/memories/stats` → **404** (K4 still open; `/memory stats` therefore prints `server unreachable`, as the plan predicted). A selector does not change that, and `?project=<unknown>` on it returns LeanKG's `unknown project … known projects: …` 404 body — the rejection is `path`-independent.
- `/v1/default/banks/omp/memories/recall` → 200; with `tags:["project:xdev"]` → **5 results**, with `tags:["project:zzz-nope"]` → **0 results**, with `tags:[]` → 8. So the tag *is* the selector that matters (K2's filtering already works through `Entry.Metadata`), and an unresolvable scope degrades to silence — which is exactly what X2 now warns about.
- Multi-project routing itself: `?project=<not registered>` → 404 with the router's message; `?project=<default dir>` → 200. Confirms `bankPath`'s selector is the right lever, and that the client must send a path the router knows.
