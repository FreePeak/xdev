# How xdev compares to other harnesses

Seven harnesses were read from primary sources — installed binaries and
bundles, full clones, SQLite stores, live request dumps — and each
produced a written adopt / verify / **reject** verdict in [docs/PRD.md](https://github.com/FreePeak/xdev/blob/main/docs/PRD.md) §5
instead of imitation. Teardowns: [pi](docs/research/parity-pi-internals.md),
[omp](docs/research/parity-omp-internals.md),
[Claude Code](docs/research/claude-code-internals.md),
[OpenCode](docs/research/opencode-internals.md),
[DeepSeek Harness](docs/research/dsh-internals.md),
[fx](docs/research/fx-internals.md), [hermes](docs/research/hermes-internals.md);
the dispositioned cross-check is
[docs/parity/harness-cross-check-2026-09-14.md](docs/parity/harness-cross-check-2026-09-14.md).

| Peer | Their bet | In xdev | Refused |
|---|---|---|---|
| **pi** | Minimalism as spec: a measured ~460–510-token prompt, four core tools, no permission system, container recipes in docs, no todo tool | The value system, the stream contract, the session model | pi's own refusals: `todo` ships (omp's 9-op engine), and the long tail ships behind disclosure |
| **omp** | Pi's core plus everything: Bun, Rust N-API natives, in-process TS extensions, 29 tools ≈ 12–15k prompt tokens, six compaction paths, hub/subagents, SQLite sidecars | Semantics ported with the same entry names, so sessions interop: the tree store (`message` / `compaction` / `reset_boundary`), the compaction ladder — `snapcompact`/`shake`/`soft` as opt-in members beside the model s…[+237b] | The in-process stack (Go report), the SQLite transcript (JSONL is the replayable record) |
| **Claude Code** | A thin layer over the model wrapped in a large product surface: approval modes, Agent Teams, OTel export, enterprise policy tiers | Plan mode with an explicit gate, `ask` (a batch is one card, not N interruptions), checkpoint/rewind, hooks exit-2 blocking, prompt-cache markers on the request prefix | Telemetry (xdev ships none), managed-policy tiers, in-process permission UI…[+27b] |
| **OpenCode** | SQLite as the source of truth — WAL, migrations, todos as a table — plus an LSP manager that is unconditional on the write path | Its diagnostic, not its code: a bad edit should surface in the same turn. That is #263, still open | Making a database the transcript. JSONL stays the replayable record, and SQLite appears only where a query engine is the point (mnemopi's FTS5 m…[+48b] |
| **DeepSeek Harness** | "Everything is a plugin": ~57 renameable model-facing tools, none privileged — even the loop and the adapters are plugins; a *linear* event log with a monotonic `seq` | The tree stays, so `/fork`, `/branch` and `rewind` exist at all; a logged request envelope and ignorable-entry forward-compat are ticketed (#265) | The in-process VM: foreign code must not be able to kill…[+44b] |
| **fx** (Vercel Labs, Zig) | A tiny native binary that publishes enforceable budgets: 6.17 MiB, a 2 ms boot gate, a 7.800 MiB ceiling, compaction ratios as exact integers | The enforce-the-promise discipline (see the open tickets below), plus the durable-write set and use-time credential verification (#122, #123, closed) and a repo-safe config allowlist stricter than fx's three keys | A linear lo…[+158b] |
| **hermes** (Nous Research) | A self-improving loop in a `uv` venv: 45+ tools, a 61,810-char three-tier cache, skills grown from experience, one process serving six chat platforms | Progressive disclosure: the deferred catalog — `tool_search` → `tool_describe` → `tool_call` — is the seam that keeps a wide surface off a small prompt | The venv (one static binary instead), the chat gat…[+40b] |

**The tension, named.** A later directive asked for omp's whole feature
surface, which pulls against pi's minimalism. It resolved mechanically,
not ideologically: the prompt stays small and the four core tools stay
the only ones in the request, while the long tail is registered and
*disclosed on demand*. MCP stays optional for pi's reason — a CLI beats
an MCP server when the CLI exists.

**What the comparison did not finish.** "We studied it" is not "xdev
shipped it", so the open half is named here instead of glossed:
post-edit LSP diagnostics on the `edit`/`write` result (#263,
OpenCode's cheapest win), the startup and binary-size gates that would
make those promises machine-enforced rather than prose (#118, #119), the
tape-replay tier that turns a user's render bug into a checked-in golden
with no PTY (#120), and `<untrusted_tool_result>` wrapping of injected
web, browser and MCP output — the hermes pattern PRD §5 adopts and the
code does not yet do. Hiding that in a section about how well studied
the design was would be this repo's own defect class, in prose.

**The method, in one line.** Primary sources or it didn't happen: a
peer claim without a `path:line` does not enter the plan. The rule
caught us as well as the peers — an audit of the dsh/OpenCode pass found
**41 of 85** quoted "verbatim" spans were paraphrases wearing
quotation marks, one cited a file absent from the corpus, and the
correction is recorded in PRD §5.4 rather than quietly amended.
