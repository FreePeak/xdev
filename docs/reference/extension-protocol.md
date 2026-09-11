# Extension protocol

Reference for xdev's extension processes (`internal/ext`). An extension is a
**standalone executable** that xdev launches and speaks to over stdin/stdout
with one JSON object per line. It can add tools, slash commands, card/table/tree
renderers, and — most importantly — **intercept tool calls as a policy hook**.

Everything here is derived from the code. When this document and the code
disagree, the code is right — update this file in the same change.

---

## 1. Discovery and lifecycle

- Directory: `<dataDir>/extensions` (`~/.xdev/agent/extensions` by default).
- Every **executable** regular file there is started, in lexical order.
  Non-executables are skipped with a debug log; a file that crashes on start
  is recorded in `Manager.Failures()` and never blocks the session.
- The directory is optional: absent means zero extensions and zero cost.
- On shutdown the manager closes stdin and kills any process that outlives it.

Handshake (10 s default, `DefaultHandshakeTimeout`):

```
host → ext   {"type":"hello","protocol":1,"xdevVersion":"…"}
ext  → host  {"type":"capabilities","payload":{"tools":[…],"commands":[…],
              "events":[…],"renderers":[…],"failOpen":false}}
```

`protocol` is the compatibility gate (`ProtocolVersion = 1`). A mismatch or a
missing `capabilities` frame is a load failure for that extension.

## 2. Frames

One JSON object per line, in both directions. The envelope (`Frame`):

| field         | direction | meaning                                                       |
|---------------|-----------|---------------------------------------------------------------|
| `type`        | both      | `hello` \| `capabilities` \| `event` \| `response` \| `action` \| `log` |
| `id`          | both      | correlation id for `event` → `response`                        |
| `event`       | host→ext  | event name (see §4)                                            |
| `payload`     | both      | host→ext: event-specific JSON; ext→host `capabilities`: the Capabilities object (§3) |
| `allow`       | ext→host  | policy verdict for `tool_call`                                 |
| `reason`      | ext→host  | human-readable denial reason (surfaced to the model)           |
| `revise`      | ext→host  | replacement tool arguments (JSON)                              |
| `patch`       | ext→host  | replacement tool result (JSON)                                 |
| `error`       | ext→host  | hard failure answering this frame                              |
| `failOpen`    | ext→host  | tolerate this failure instead of denying                       |
| `action`      | ext→host  | runtime request: `steer` \| `followUp` \| `aside` \| `register_provider` |
| `text`        | ext→host  | action/log text                                                |
| `level`       | ext→host  | log severity                                                   |

Anything the extension writes to **stderr** is the host's log stream; stdout
carries frames only. A line that is not valid JSON is a protocol error for that
extension.

## 3. Capabilities

| field       | meaning                                                                 |
|-------------|-------------------------------------------------------------------------|
| `tools`     | tools to register. Registered as **`ext_<extension>_<tool>`** — namespaced so an extension can never shadow `read`, `bash`, or another extension's tool. |
| `commands`  | slash commands, invoked as `/ext:<name>`.                                |
| `events`    | which events the extension wants (§4). Unlisted events are never sent.   |
| `renderers` | declarative card / table / tree renderers for TUI output.                |
| `failOpen`  | **opt-in** to tolerating this extension's failures (§5).                 |

## 4. Events

| event            | when                                    | host expects                                   |
|------------------|-----------------------------------------|------------------------------------------------|
| `session_start`  | a run begins                            | nothing (notification)                         |
| `turn_end`       | a turn finished                         | nothing (notification)                         |
| `tool_call`      | **before** a tool executes               | `allow` / `reason` / `revise` — blocks or rewrites the call |
| `tool_result`    | after a tool produced its result         | `patch` — rewrites the result text             |

`tool_call` payload: `{"tool": "<name>", "arguments": {…}}` — the arguments are
exactly what the model sent, and `revise` must return them in the same shape.

Per-event timeout: 5 s (`DefaultEventTimeout`), then the extension is killed
and marked dead. `tool_call` is the security-relevant hook; `tool_result` is
advisory.

`revise` replaces the arguments the model asked for; `patch` replaces the
result the model will see. Both are applied only when present — an empty
`revise`/`patch` means "no change", so an extension cannot accidentally blank a
call.

## 5. Failure policy (fail-closed)

The default is **fail-closed**: if an extension that claimed `tool_call`
cannot answer (crash, timeout, malformed frame, killed mid-round-trip), the
call is **denied** with a reason naming the extension.

That is deliberate — a dead policy hook must not silently become no policy. An
extension that genuinely cannot implement policy (e.g. a formatting helper)
sets `"failOpen": true` in its capabilities, or answers `"failOpen": true` on
the individual frame, and then its failures pass the call through.

`ErrDead` marks a killed extension: the manager prunes it from the interceptor
chain for the rest of the session rather than re-dispatching to a corpse.

## 6. Runtime actions

An extension can influence the live run without returning a frame at the
prompt boundary:

| action              | effect                                                                 |
|---------------------|------------------------------------------------------------------------|
| `steer`             | queue text into the running turn (delivered at the next step boundary)  |
| `followUp`          | start a follow-up run after the current one                            |
| `aside`             | currently routed as `steer` (no distinct surface yet)                   |
| `register_provider` | not wired: needs the provider registry (M9)                             |

## 7. Composing with hooks

Extensions and the shell-command hooks bus (`internal/hooks`, settings key
`hooks`) implement the same `agent.Interceptor` interface and run in one
chain:

```
hooks bus  →  extension manager  →  (agent)
```

The **first** `tool_call` denial in the chain short-circuits: neither the tool
nor the later interceptors run. `tool_result` patches apply in sequence (later
interceptors see the earlier patch). Events fan out to everyone.

Practical reading: put deterministic policy (a project rule) in the hooks bus,
and interactive/stateful policy in an extension.

## 8. Writing a minimal extension

A working extension reads lines, answers the handshake, and replies to every
frame it declared. This one denies `bash` calls whose command contains `rm -rf`
and allows everything else — fail-closed by default, so it must answer:

```python
#!/usr/bin/env python3
# <dataDir>/extensions/deny-rm.py   (chmod +x)
import json, sys

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        frame = json.loads(line)
    except json.JSONDecodeError:
        continue  # a bad line is not a frame; keep serving
    t = frame.get("type")
    if t == "hello":
        # Declare only what you handle. Omit failOpen: this IS a policy hook.
        print(json.dumps({"type": "capabilities",
                          # capabilities ride under "payload", not top level
                          "payload": {"events": ["tool_call", "session_start"]}}),
              flush=True)
    elif t == "event" and frame.get("event") == "tool_call":
        # payload = {"tool": "<name>", "arguments": {...as the model sent them...}}
        p = frame.get("payload", {})
        args = p.get("arguments") or {}
        cmd = str(args.get("command", "")) if p.get("tool") == "bash" else ""
        deny = "rm -rf" in cmd
        print(json.dumps({"type": "response", "id": frame.get("id"),
                          "allow": not deny,
                          "reason": "rm -rf is disabled in this project" if deny else ""}),
              flush=True)
    elif t == "event":
        # Any other event must still be answered, or the round-trip times
        # out — and with fail-closed that reads as a denial.
        print(json.dumps({"type": "response", "id": frame.get("id"),
                          "allow": True}), flush=True)
```

Conventions worth following:

- Answer **every** `event` frame, even when allowing — silence is a timeout,
  and with fail-closed that means a denial.
- Keep `tool_call` fast (well under 5 s); do slow work asynchronously.
- Use `failOpen: true` only when the extension is not a policy boundary.
- Namespace nothing yourself: tools are auto-prefixed `ext_<name>_<tool>`.
