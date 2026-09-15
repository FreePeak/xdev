# Contributing to xdev

xdev is a lightweight coding-agent harness in Go: one static, CGO-free binary
holding the session core of [pi](https://github.com/earendil-works/pi) and omp —
provider streaming, the agent loop, JSONL session persistence, tool execution,
and a terminal UI.

The source of truth for scope, architecture and status is
[docs/PRD.md](docs/PRD.md). Read it before opening anything; open an issue
before writing code that changes it.

## Ground rules

1. **Issues are the task list.** Every task is a GitHub issue. This repo has no
   `TASKS.md`/`TODO.md`, and a PR that adds one will be closed. The PRD
   references issues by number — it is a status summary, never a backlog.
2. **The PRD reflects what landed.** Any change to scope, architecture or
   status updates `docs/PRD.md` *in the same PR* — the affected milestone row
   and the `*Last updated:*` footer.
3. **Bounded everything.** Queues, buffers, caches and output sinks have hard
   limits. Code that grows with its input and has no ceiling is a bug, not a
   trade-off (PRD §1 goal 6, and the <100 MB RSS budget).
4. **Dependencies are not free.** The binary is CGO-free and ~22 MB. Prefer the
   standard library, and prefer shelling out to an existing CLI (`git`,
   `ast-grep`) over importing a module. `go.mod` growth is reviewed.
5. **No new abstractions that were not asked for.** Fix the shared function
   once rather than adding a guard at every caller.

## Setting up

```bash
git clone https://github.com/FreePeak/xdev && cd xdev
CGO_ENABLED=0 go build -o ./xdev ./cmd/xdev      # build exactly this way, CGO-free
./xdev --help
```

Configuration and data live in `~/.xdev/agent/` (`config.yml`, `models.yml`,
`sessions/`). Provider credentials and model choices are per-machine and
per-owner; nothing under `~/.xdev/` belongs in a commit.

## Pre-merge checks (run all of these yourself)

```bash
gofmt -l .                        # must print nothing
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build -o /dev/null ./cmd/xdev
```

Plus the six-platform cross matrix (the Goal-1 promise):

```bash
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -o /dev/null ./cmd/xdev || echo "FAIL $t"
done
```

Platform-specific syscalls need a **build tag** (or a `_windows.go` stub), not a
runtime `GOOS` check — a runtime guard cannot stop
`syscall.SysProcAttr{Setpgid: true}` from failing the Windows compile, and the
first version of that bug shipped green because the gate only built Linux.
`govulncheck ./...` is a reason to bump a dependency, never to add an ignore.


End-to-end against a configured provider:

```bash
./scripts/smoke.sh "Create a file named smoke.txt containing 'xdev was here'"
```

## Pull requests

- One behavioral change per PR.
- Subject: `feat(tui): …`, `fix(session): …`, `perf(agent): …`, `ci: …`,
  `docs(prd): …`, `test(edit): …`. The text after the colon says **what changed
  and why it matters** — `fix(session): a torn write can never brick a session
  or fork one` — not a ticket number dressed up as a description.
- Body explains the *why*: constraints, alternatives rejected, what was
  measured. Link the issue (`closes #123`).
- Claims need evidence. "Tests pass" is a command's output, not an
  argument; paste the run, or say what you could not run and why.
- Fix the source, not the symptom. Suppressing a warning or special-casing an
  input to close a report is not a fix.
- A test earns its place where a plausible bug would fail it. Tests that pin
  wiring, field copies, incidental defaults or the exact wording of an error
  message get deleted rather than re-pinned.

## Agent-assisted contributions

xdev *is* a coding agent, so many commits here are agent-authored. That is
welcome, and the rules above apply harder, not looser: the human who submits the
PR owns every line, must be able to explain the change, and must have run the
checks. Unrequested abstraction, defensive special cases for inputs that cannot
occur, and "while I'm here" reformatting will be asked to shrink.

## What will not be accepted

- An in-process plugin VM, a JS/Bun runtime, or Rust N-API natives.
- Built-in permission prompts or pattern-matched "security". xdev is YOLO by
  design; containment belongs to external sandboxing, which the docs cover.
- A `stats.db`-scale sidecar database (a tiny rollup table only).
- Tree-sitter embedding, an in-tree git library, or a marketplace.

Reasoning is in PRD §1 (Non-goals) and `docs/decisions/`. Disagree? Open an
issue first — a decision can change with an argument, but not with a surprise PR.
