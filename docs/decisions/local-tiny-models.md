# Decision: local tiny models for session titles and memory workers

*Issue: [#70](https://github.com/FreePeak/xdev/issues/70) · Milestone: M15 (full-parity tail) · Date: 2026-09-12 · Status: **accepted***

**Decision: option (a) stays the shipped path — tiny tasks run on the configured session model — and option (b) is recorded as the upgrade path, made reachable now through the `internal/tiny` seam. Option (c) is rejected for generation and kept only as a note for non-generative classification work.**

---

## 1. Context

### What omp does (the parity target)

omp serves three small-LLM tasks from on-device models (`omp://local-models.md`, `providers.tinyModel` / `providers.memoryModel` / `providers.autoThinkingModel`, all defaulting to `online`):

| omp task | Recipe | Measured cost on the reference machine |
| --- | --- | --- |
| Session title (first user message → 3–7 words) | transformers.js v4 on the native `onnxruntime-node` backend, prefill `<title>`, stop `</title>`, greedy | LFM2.5-230M q4: 214 MB cache, 93 ms warm mean / 194 ms p95; 21/28 usable titles |
| Mnemopi memory extraction + consolidation | same worker protocol, `llama3.2:3b` / `gemma-3-1b` / `lfm2-1.2b` / `qwen2.5-1.5b` | q4 weights 150 MB–1.1 GB downloaded on first use; warm load sub-3 s; one extraction ≈ 200 ms (1.7B) |
| `auto` thinking-level classifier | shares the memory-model registry | — |

The mechanism that makes this tolerable is process shape, not model size: **exactly one worker process per model** owns a unix socket (`~/.omp/run/tiny/<model>-<backend>.sock`), is spawned detached by the first omp instance that needs it, exits after 15 min idle, and is reused by every other instance. Native addons `dlopen` `libstdc++.so.6`/`libgcc_s.so.1`, which is why omp needs `OMP_NATIVE_LIBRARY_PATH` on non-FHS distros. On Apple silicon an MLX variant swaps the worker for a pinned `mlx-lm` venv (`~/.omp/agent/cache/tiny-mlx-runtime/`, installed via `uv`), adds a second 4-bit export, and reports 15.7 s cold install+load.

### What xdev actually has

- **Memory pipeline: exists.** `internal/memory/pipeline.go` (M12 #13) runs phase 1 (extraction over changed sessions) and phase 2 (consolidation into `MEMORY.md`/`learned.md`) behind two func seams, `Pipeline.Complete` and `Pipeline.Consolidate`, resolved from the **session model** at wiring time (`cmd/xdev/print.go` `buildMemoryPipeline` — `memoryComplete`, which resolves the run model). This is exactly where an on-device backend would plug in.
- **Session titles: do not exist as a model task.** New sessions get mechanical titles — `openSession` in `cmd/xdev/print.go` sets `"print 2026-09-12 14:03"` / `"continued …"`, imported sessions get `"imported: <kind> <first user text>"` (`internal/session/import.go`). `TITLE_SYSTEM.md` is discovered but unused; the recorded read point for a generated title is `agent.SystemPromptOverrides.TitleSystemPrompt()`. **There is no title call site to swap for a local model today.**
- Model-role aliases no longer exist at all (*removed 2026-09-17*: `internal/config/roles.go` became `effort.go`, keeping only the `:effort` vocabulary), so there is no role slot to hand a tiny model to.
- **Auto thinking classifier: no xdev counterpart.**
- No local inference code, no weights, no worker, no new dependency.

### Invariants this decision must not break

1. **One CGO-free static binary** (`CGO_ENABLED=0`, six-platform cross matrix in CI). No cgo, no C deps, no `dlopen`.
2. **< 100 MB RSS** for the agent process (M8 release gate re-measures it).
3. **Startup latency**: the TUI must come up immediately; nothing may block on downloads or model loads.
4. **Stdlib first**: a pure-Go dependency only when unavoidable.
5. **Offline/privacy capability**: a user must be able to keep session-derived text off the network.

## 2. Options

### (a) API-only tiny role — *current behavior*

Tiny tasks run on the configured role over the same provider stack as everything else.

- **Cost:** zero bytes, zero startup cost, zero dependencies, works on every platform and CPU.
- **Limit:** every title and every memory extraction leaves the machine; fails offline.
- **Verdict:** the only option compatible with all five invariants without qualification. **Keep as the default.**

### (b) Opt-in local runtime behind a build tag, shipped as a separate artifact

`onnxruntime_go` or cgo `llama.cpp` gives the real thing (sub-second warm loads, q4 weights) but requires `CGO_ENABLED=1`, a linked runtime, and per-platform native assets. That is a **different artifact**, not a flag on the static one.

- **Cost:** a second build/release lane (cgo + per-platform runtime libs), a first-run weight download (150 MB–1.1 GB), a load-time/RSS budget paid by whoever opts in, and the NixOS-style `dlopen` trap omp documents.
- **Buys:** offline titles + memory extraction, no per-token API cost, and prompt text that never leaves the host.
- **Verdict:** correct shape for the opt-in, wrong shape for the default. **Recorded, not built** (see §3).

**B2 — subprocess worker (the recommended concrete form of B).** A local backend that speaks NDJSON to a user-installed worker binary (`llama-server`, or an omp-style model worker) over a unix socket keeps the *main* artifact CGO-free, single, and static, and moves the weights and the RSS cost into a separate process — precisely the construction omp already proved out (one worker per model, 15-min idle exit, socket reuse). It is the cheapest way to get (b) without touching invariants 1, 2, or 3; the seam in this change is already shaped for it (`tiny.LocalBackend` is an interface, `Complete` is prompt→text).

### (c) Pure-Go small transformers

- **Cost/limit:** there is no pure-Go quantized decoder-LLM stack. What exists is narrow: `gorgonia`/`goml`-class tensor libraries (no tokenizer/chat-template ecosystem), embeddings-only ports, or wrapping `tiktoken`-style tokenizers. Reproducing omp's GGUF/ONNX q4 decoder path (quantization, KV cache, sampling) in pure Go is a multi-month project with a large constant-factor slowdown on CPU — at which point RSS and latency both lose to option (b)'s subprocess anyway.
- **Buys:** CGO-free on-device inference for *classification/similarity* tasks (embeddings, dedup, an "is this lesson new?" filter) — not for generation.
- **Verdict:** rejected for the title/memory generation tasks; recorded as the only viable pure-Go on-device path should a non-generative task ever appear.

## 3. Decision

**Ship option (a) unchanged and make option (b) a clean, honest opt-in later.**

The seam — `internal/tiny` — is the whole of the "make (b) cheap" work:

| Element | Contract |
| --- | --- |
| `tiny.LocalBackend` | `Name() string`, `Available() bool`, `Complete(ctx, prompt) (string, error)` — one interface, no cgo, no new dependency |
| `tiny.Backend()` | the compiled-in backend; **never nil**, and in the shipped artifact a stub whose `Available()` is false and whose `Complete` returns `ErrNotBuilt` |
| `tiny.Requested()` | reads `XDEV_TINY_LOCAL` (`on`/`1`/`true`/`yes` vs `off`/`0`/`false`/`no`/unset); any other value is an error, so a typo cannot silently mean "off" |
| `tiny.Select(task, api)` | returns `api` **unchanged** unless local was requested; with the opt-in and no runtime it returns `(nil, ErrNotBuilt)` and **never substitutes the API completer** |
| `-tags tinycgo` | wires the alternate `compiledIn()`; the placeholder in this checkout reports unavailability too, so the opt-in artifact can never be a silently empty stub |

### Rationale, one line per invariant

1. **CGO-free static binary:** the default artifact links no runtime, no cgo file is compiled, `go.mod` is untouched, and `CGO_ENABLED=0 go build ./...` still produces the same single binary. The tag path is opt-in and out of the release build.
2. **< 100 MB RSS:** no weights are loaded, no worker is spawned, and no goroutine is started in the default build; the weights/worker cost of option (b) lands in a *separate* process (B2), never on the agent.
3. **Startup latency:** the only work the seam does at startup is one `os.Getenv` plus a switch at the wiring site; nothing waits on a download or a model load.
4. **Stdlib first:** no dependency at all; the seam is a Go interface, and the future worker client would be `net`/`encoding/json` from the stdlib.
5. **Offline/privacy:** the opt-in direction is strictly "keep it on the host". When the user asks for local and the binary cannot deliver, the pipeline **refuses** (`ErrNotBuilt`, logged, pipeline disabled) rather than quietly sending the same text over the network. That is deliberate: silently falling back to the API would violate the reason the user set the flag.

**Why not build (b) now:** the only *existing* tiny task in xdev is the memory pipeline, memory consolidation is best-effort background work, and the title task does not exist — so the on-device win today is one background pipeline, paid for with a second release lane and a 150 MB–1.1 GB download. Demand, not parity symmetry, should pay that bill; the seam means the day demand appears, the cost is a build-tagged backend file, not a refactor.

## 4. What this change ships

- `internal/tiny/tiny.go` — the seam, the env switch, `Select`, and the no-silent-fallback contract.
- `internal/tiny/local_stub.go` (`//go:build !tinycgo`) — the shipped artifact's honest stub.
- `internal/tiny/local_cgo.go` (`//go:build tinycgo`) — the option-B placeholder: the tag wires a backend, no runtime is linked, so it still refuses with `ErrNotBuilt`.
- Tests: the default path returns the API completer unchanged; the opt-in without a runtime fails with `ErrNotBuilt` **and never calls the API completer**; the stub reports unavailability; a bad `XDEV_TINY_LOCAL` value fails loudly; under `-tags tinycgo` the placeholder still refuses.

**Where the seam is consulted (wiring, one call site):** `cmd/xdev/print.go`, `buildMemoryPipeline`, on the completion seam `memoryComplete` resolves from the session model:

```go
	complete, selErr := tiny.Select(tiny.TaskMemoryExtract, complete)
	if selErr != nil {
		logx.Errorf("memory pipeline: %v", selErr)
		return nil // never fall back to the API after an explicit local opt-in
	}
```

Observed end-to-end with that wiring on the default CGO-free build (a configured default model, `XDEV_TINY_LOCAL=on`): the run completes normally and the startup wiring logs one actionable line — `ERROR memory pipeline: tiny: memory-extract: no local tiny backend built into this binary (backend "none (CGO-free static build)"; see docs/decisions/local-tiny-models.md)` — while the same run without the env var logs nothing and keeps the API path unchanged.

(`task`/title wiring, when the title generator exists: same call with `tiny.TaskTitle` at the `TitleSystemPrompt` read point.)

## 5. Upgrade path (when option B is actually taken)

1. Add `internal/tiny/local_onnx.go` with `//go:build tinycgo` implementing `LocalBackend` (cgo link, or preferably the B2 stdlib-socket worker client) and delete the placeholder `local_cgo.go`.
2. Build the opt-in as its own artifact — `CGO_ENABLED=1 go build -tags tinycgo -o xdev-tiny ./cmd/xdev` — and keep the default release lane CGO-free.
3. Download weights on first use into `<agent dir>/cache/tiny-models/`, never at startup, never blocking the TUI (omp's first-run model).
4. Keep `Available()` honest (no runtime, no weights, dead worker ⇒ `false`) so `Select` keeps refusing instead of degrading to the API.
5. Re-measure the M8 RSS gate for the opt-in artifact; the default artifact's budget is unaffected.

## 6. Non-goals and recorded ceilings

- **No local inference in the shipped binary** — `ponytail:` ceiling marked in `internal/tiny/local_stub.go`; upgrade path is §5.
- **No `tinycgo` runtime** — the tag compiles the seam, not inference; `ponytail:` ceiling marked in `internal/tiny/local_cgo.go`.
- **No new setting, provider, or role.** `XDEV_TINY_LOCAL` (env / dotenv chain) is the whole opt-in surface; the API fallback remains the session model and is unchanged in behavior.
- **No title-generation work.** This issue does not create the title task; the decision doc records that xdev titles are mechanical and that `TaskTitle` is the future consultation point.
- **No dependency added.** If option B is taken with a *linked* runtime, the report must say plainly that cgo was introduced and confined to the tagged artifact.
