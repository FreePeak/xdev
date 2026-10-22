# Decision: the agent learns its tool usage from its own failures

*Reference: `internal/memory/tooluse.go`, `internal/agent/toollesson.go` · Date: 2026-10-22 · Status: **adopted***

**Decision in one line: a failed tool call is recorded, the agent's own recovery from it (a later call that works) becomes a durable recipe, and the recipe is injected into the prompts of later sessions — so a mistake about a tool is paid for once instead of once per session.**

---

## 1. Context

The agent already has a memory subsystem (`internal/memory`): `MEMORY.md` for the
consolidated summary, `learned.md` for lessons, and a `learn` tool the model
calls on its own initiative. What none of it captured was the most common kind
of lesson there is — **how to call a tool**.

Two gaps made that visible:

| Gap | Where | Effect |
|---|---|---|
| `Store.NoteFailure(tool, errText, context)` existed and had no caller | `internal/memory/memory.go:78` | a failed tool call was never recorded anywhere but the transcript |
| The consolidation pipeline drops tool results before extraction | `internal/memory/pipeline.go:633` | "tool results are noise for a durable-fact extraction" — so a failed call never reached `MEMORY.md` either |

The result: every session rediscovered the same tool mistakes. `read` with a
relative path, `edit` with a string that does not occur in the file, `bash` with
a flag the tool does not take — the same errors, in every session, forever.

## 2. The loop

```
call fails ──▶ the failure is recorded (tool, symptom, arguments)
                    │
                    ▼
              the agent tries again
                    │
              a call works
                    │
                    ▼
         the difference between the two calls is the recipe
                    │
                    ▼
      the recipe is injected into the prompts of later sessions
```

The agent (the LLM) is what finds the working form. xdev's job is to notice
that it happened, remember it, and read it back.

## 3. What counts as a recipe

A recipe is written only when the two calls are **the same call**:

| The failing call | The working call | Recipe? | Why |
|---|---|---|---|
| `read {path: "notes.md"}` | `read {path: "/abs/notes.md"}` | yes | same call, one value changed |
| `read {}` | `read {path: "notes.md"}` | yes | the agent supplied what was missing |
| `read {file_path: "x"}` | `read {path: "x"}` | yes | a renamed argument — the mistake worth remembering most |
| `read {path: "a.md"}` | `read {path: "a.md"}` | no | the same call failed and then worked: a transient failure |
| `read {path: "a.md"}` | `bash {command: "ls"}` | no | a different call that happened to work says nothing about `read` |

A recipe that claims otherwise is worse than no recipe at all: the model is
told "use this instead" about something that was never a usage mistake.

Harness outcomes are not failures to remember. A refused, denied, blocked,
canceled or abandoned call has no form that would have worked, so it never
opens a pending failure.

## 4. Options considered

| Option | Shape | Verdict |
|---|---|---|
| **A — A dedicated tool-usage store** (chosen) | One JSONL file beside `MEMORY.md`; the recipes ride their own prompt block, filtered to the tools the session has | Chosen: the recipes are structured (tool, symptom, recipe), they need reliable recall, and they must not crowd out the free-text lesson log |
| B — `learn` into `learned.md` | Reuse the lesson store and the existing injection | Rejected: the recipes compete with project lessons for the lesson cap, and a free-text lesson log has nowhere to keep the tool/symptom/recipe triple apart |
| C — A model-written summary of every recovery | One LLM call per recovery | Rejected: a model call per failure is a cost and a latency on the tool path, and the failure/recovery pair already carries the difference structurally |
| D — Inject into the failing tool result | The recipe rides the result of the next failed call | Deferred: useful (the model is told before it retries), but it is a second surface on top of the first, and the prompt injection already puts the recipe in front of the model |

## 5. Invariants

1. **The loop never breaks a session.** A tracker that cannot write, a store
   that cannot be read, a malformed record line: all of them are skipped, and
   the run continues. Memory is heuristic context.
2. **No secrets.** The recipe quotes the call that failed, and a call can carry
   a token. The redactor scrubs the record at write time (`#88`'s rule, same
   as the `learn` tool).
3. **The prompt pays nothing for an empty store.** No recipes, no block.
4. **A recipe never names a tool the session does not have.** The block is
   filtered against the live registry, like the fanout block's `task` gate.
5. **The store is bounded.** 40 records, 8 recipes per block (at most ~1000
   characters), 3 per tool.

## 6. Where it lives

| Piece | Where |
|---|---|
| The store, the tracker, the recipe derivation | `internal/memory/tooluse.go` |
| The seam the loop reports through | `internal/agent/toollesson.go` (`ToolLessonObserver`) |
| The one place every build site goes through | `cmd/xdev/print.go` (`wireAgentMode`) |
| The prompt injection | `cmd/xdev/print.go` (`promptBuilder`) |
| The switch | `internal/config/settings.go` (`toolLessons`, default on) |

A child inherits the learner (`TaskTool.ToolLessons`): a subagent does most
of the session's tool work, and a mistake only a child made is a mistake the
next session makes again. The child learns into the same store and reads the
same recipes; its pending failures are its own.

The records live in `<data dir>/memory/tool-use.jsonl`, beside `MEMORY.md` and
`learned.md`, and do not depend on the memory backend: a recipe is operating
knowledge about this machine's tools, and it has to be readable by the session
that comes next whichever backend is configured.

`xdev memory stats` counts the recipes, `xdev memory recipes` lists them (the
failures and the recipes that answered them), `memory://root/tool-use.jsonl`
reads the raw records, `xdev memory export`/`import` carry them, and
`/memory clear` takes them with the rest of the memory.
