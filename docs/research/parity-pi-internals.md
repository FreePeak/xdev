# Pi (earendil-works) Internals: Tools, Tool-Call Mechanics, and System Prompt

> **Package**: `@earendil-works/pi-coding-agent` v0.85.0 (installed 2026-09-04)  
> **Binary**: `~/.local/bin/pi` → `~/.local/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js`  
> **Data dir**: `~/.pi/agent/`  
> **Source structure**: unbundled `dist/` with `.js` + `.d.ts` + `.map`; no source maps expose original TS, but d.ts is fully annotated  
> **Core deps**: `@earendil-works/pi-agent-core` (loop), `@earendil-works/pi-ai` (providers/validation), `@earendil-works/pi-tui`

---

## 1. Built-in Tool Inventory

Pi ships **8 built-in tools**, enumerated in `dist/core/tools/index.js` as `allToolNames`:

| Tool | Purpose | Prompt snippet |
|------|---------|----------------|
| `read` | File/image reader | "Read file contents" |
| `bash` | Shell execution (bash) | "Execute bash commands (ls, grep, find, etc.)" |
| `edit` | Multi-target oldText/newText replacement | "Make precise file edits with exact text replacement, including multiple disjoint edits in one call" |
| `write` | File create/overwrite | "Create or overwrite files" |
| `grep` | ripgrep-backed content search | "Search file contents for patterns (respects .gitignore)" |
| `find` | fd-backed glob search | "Find files by glob pattern (respects .gitignore)" |
| `ls` | Directory listing | "List directory contents" |
| `powershell` | PowerShell execution (Windows) | "Execute PowerShell commands" |

**Tool sets** defined in `dist/core/tools/index.js`:
- `createCodingTools`: `read`, `bash`, `edit`, `write` (the 4 default)
- `createReadOnlyTools`: `read`, `grep`, `find`, `ls`
- `createAllTools`: all 8

Default prompt tool list (when user hasn't configured `defaultTools`): `["read", "bash", "edit", "write"]`.

---

## 2. Verbatim Tool Schemas (TypeBox → JSON Schema)

All schemas use `@earendil-works/pi-ai`'s bundled TypeBox. Exact definitions extracted from source.

### read
```typescript
// dist/core/tools/read.js
Type.Object({
  path:   Type.String({ description: "Path to the file to read (relative or absolute)" }),
  offset: Type.Optional(Type.Number({ description: "Line number to start reading from (1-indexed)" })),
  limit:  Type.Optional(Type.Number({ description: "Maximum number of lines to read" })),
})
```
**Description** (LLM-facing):  
> "Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete."

**Truncation constants** (`truncate.js`): `DEFAULT_MAX_LINES = 2000`, `DEFAULT_MAX_BYTES = 50 * 1024` (50KB).

### write
```typescript
Type.Object({
  path:    Type.String({ description: "Path to the file to write (relative or absolute)" }),
  content: Type.String({ description: "Content to write to the file" }),
})
```
**Description**:  
> "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories."

### edit
```typescript
const replaceEditSchema = Type.Object({
  oldText: Type.String({
    description: "Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call.",
  }),
  newText: Type.String({ description: "Replacement text for this targeted edit." }),
}, {});

const editSchema = Type.Object({
  path: Type.String({ description: "Path to the file to edit (relative or absolute)" }),
  edits: Type.Array(replaceEditSchema, {
    description: "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.",
  }),
}, {});
```
**Description**:  
> "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes."

**Backward compatibility**: `prepareEditArguments()` in edit.js handles legacy single-edit format: `{ oldText, newText }` is automatically wrapped into `edits: [{ oldText, newText }]`; stringified `edits` JSON is also unwrapped. No re-validation after mutation.

### bash
```typescript
Type.Object({
  command: Type.String({ description: "Shell command to execute" }),
  timeout: Type.Optional(Type.Number({ description: "Timeout in seconds (optional, no default timeout)" })),
})
```
**Description**:  
> "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds."

**Max timeout**: `2_147_483_647` ms (~24.8 days).  
**Exit code handling**: non-zero exit → throws `Error("Command exited with code ${exitCode}")` with full output text prepended (this becomes `isError: true` tool result).

### grep
```typescript
Type.Object({
  pattern:   Type.String({ description: "Search pattern (regex or literal string)" }),
  path:      Type.Optional(Type.String({ description: "Directory or file to search (default: current directory)" })),
  glob:      Type.Optional(Type.String({ description: "Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'" })),
  ignoreCase:Type.Optional(Type.Boolean({ description: "Case-insensitive search (default: false)" })),
  literal:   Type.Optional(Type.Boolean({ description: "Treat pattern as literal string instead of regex (default: false)" })),
  context:   Type.Optional(Type.Number({ description: "Number of lines to show before and after each match (default: 0)" })),
  limit:     Type.Optional(Type.Number({ description: "Maximum number of matches to return (default: 100)" })),
})
```
**Description**:  
> "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to 100 matches or 50KB (whichever is hit first). Long lines are truncated to 500 chars."

**Constants**: `DEFAULT_LIMIT = 100`, `GREP_MAX_LINE_LENGTH = 500`.

**Backend**: spawns `rg --json --line-number --color=never --hidden` (auto-downloaded on first use if absent; `--fixed-strings` for `literal`, `--ignore-case` for `ignoreCase`, `--glob` for `glob`). No Node fallback in production — absence is a hard error: "ripgrep (rg) is not available and could not be downloaded".

### find
```typescript
Type.Object({
  pattern: Type.String({
    description: "Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'",
  }),
  path: Type.Optional(Type.String({ description: "Directory to search in (default: current directory)" })),
  limit: Type.Optional(Type.Number({ description: "Maximum number of results (default: 1000)" })),
})
```
**Description**:  
> "Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to 1000 results or 50KB (whichever is hit first)."

**Backend**: spawns `fd --glob --color=never --hidden` (auto-downloaded on first use into the agent `bin` dir). Only SDK-injected `operations.glob` bypasses fd; there is no Node fallback in production.

### ls
```typescript
Type.Object({
  path:  Type.Optional(Type.String({ description: "Directory to list (default: current directory)" })),
  limit: Type.Optional(Type.Number({ description: "Maximum number of entries to return (default: 500)" })),
})
```
**Description**:  
> "List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to 500 entries or 50KB (whichever is hit first)."

### powershell
Same schema as `bash` (delegates to `createShellToolDefinition` with PowerShell config). Adds UTF-8 output prefix: `try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}`.

---

## 3. Extension Tool Surface

Extension tools are registered via `ExtensionAPI.registerTool()`. The `ToolDefinition` interface (`dist/core/extensions/types.d.ts`):

```typescript
interface ToolDefinition<TParams extends TSchema, TDetails, TState> {
  name: string;                  // LLM tool-call name
  label: string;                 // Human-readable UI label
  description: string;           // LLM description
  promptSnippet?: string;        // One-liner for system prompt "Available tools" section
  promptGuidelines?: string[];   // Extra guidelines when this tool is active
  parameters: TParams;           // TypeBox schema
  constrainedSampling?: false | ConstrainedSamplingConfig;
  renderShell?: "default" | "self";
  prepareArguments?: (args: unknown) => Static<TParams>;
  executionMode?: "sequential" | "parallel";
  execute(toolCallId, params, signal, onUpdate, ctx): Promise<AgentToolResult<TDetails>>;
  renderCall?(...): Component;
  renderResult?(...): Component;
}
```

Tools with a `promptSnippet` appear in the system prompt; tools without one are invisible there (used by `mcp` proxy tool's `directTools` registrations).

### pi-mcp-adapter (installed as npm package: `pi-mcp-adapter` v2.32.1)

The adapter is the only MCP surface. **Pi core has zero MCP support** — all MCP is extension-loaded.

**Proxy tool** (registered as `mcp`):
```typescript
Type.Object({
  tool:           Type.Optional(Type.String({ description: "Tool name to call (e.g., 'xcodebuild_list_sims')" })),
  args:           Type.Optional(Type.Union([
    Type.String({ description: "Arguments as a JSON string" }),
    Type.Object({}, { additionalProperties: true, description: "Arguments as a JSON object" }),
  ])),
  connect:        Type.Optional(Type.String({ description: "Server name to connect (lazy connect + metadata refresh)" })),
  describe:       Type.Optional(Type.String({ description: "Tool name to describe (shows parameters)" })),
  instructions:   Type.Optional(Type.String({ description: "Server name to show that server's usage instructions" })),
  search:         Type.Optional(Type.String({ description: "Search tools by name/description" })),
  regex:          Type.Optional(Type.Boolean({ description: "Treat search as regex (default: substring match)" })),
  includeSchemas: Type.Optional(Type.Boolean({ description: "Include parameter schemas in search results (default: true)" })),
  limit:          Type.Optional(Type.Number({ minimum: 1, description: "Maximum search results to return (default: 12)" })),
  offset:         Type.Optional(Type.Number({ minimum: 0, description: "Search result offset (default: 0)" })),
  server:         Type.Optional(Type.String({ description: "Filter to specific server (also disambiguates tool calls)" })),
  action:         Type.Optional(Type.String({ description: "Action: 'ui-messages', 'auth-start', or 'auth-complete'" })),
})
```

**Prompt snippet**: `"MCP gateway — status, search, describe, auth, and single MCP tool calls"` (~30 tokens).

**Direct tools mode**: When `directTools: true` in adapter config, individual MCP tools are registered as separate named tools in the agent loop, each with its own TypeBox schema cached in `~/.pi/agent/mcp-cache.json`. Each direct tool costs ~150–300 tokens.

**Tool approval**: `tool-approval.ts` implements per-call user consent (`Allow once` / `Allow for session` / `Deny`), backed by in-memory approval cache. Extensions cannot approve programmatically in headless sessions — calls fail closed.

### Other installed extensions (from settings.json `packages`)

| Package | Purpose |
|---------|---------|
| `pi-subagents` | Subagent spawning |
| `pi-background-tasks` | Background task runner |
| `@juicesharp/rpiv-todo` | Todo tracking |
| `pi-goal-x` | Goal mode (autonomous objective loop) |
| `@llblab/pi-telegram` | Telegram integration |
| `pi-cc-extensions` | Claude Code compatibility extensions |

---

## 4. Tool-Call Mechanics

### 4.1 Wire Format (Provider → Agent Loop)

Tool calls arrive as `AssistantMessage.content` blocks of type `ToolCall`:

```typescript
// pi-ai/types.d.ts
interface ToolCall {
  type: "toolCall";
  id: string;
  name: string;
  arguments: Record<string, any>;
  thoughtSignature?: string;
  namespace?: string;  // OpenAI Responses dynamic tool namespace
}
```

`StopReason` values: `"pending" | "stop" | "length" | "toolUse" | "error" | "aborted" | "deferred"`

### 4.2 Streaming Partial-JSON Args

Each provider adapter accumulates args via `block.partialJson += event.delta` and feeds them through `parseStreamingJson()` (`pi-ai/utils/json-parse.js`):

```typescript
function parseStreamingJson(partialJson) {
  // 1. Try parseJsonWithRepair (JSON.parse → repairJson → JSON.parse)
  // 2. Fallback: partial-json library's partialParse
  // 3. Fallback: partialParse(repairJson(partialJson))
  // 4. Return {} on total failure
}
```

`repairJson()` handles:
- Raw control characters inside strings (escaped to `\uXXXX`)
- Doubled backslashes before invalid escape chars
- Unterminated strings (via `partial-json` library graceful degradation)

`partialJson` is always deleted from the block before session persistence.

### 4.3 Argument Validation

`validateToolArguments()` (`pi-ai/utils/validation.js`):
1. `structuredClone(toolCall.arguments)` → clone
2. `normalizeOptionalNulls(args, schema)` → convert `null` to `undefined` for optional fields
3. `Value.Convert(tool.parameters, args)` → TypeBox coercion (e.g., string → number when schema expects number)
4. TypeBox `Compile(schema).Check(args)` → full validation
5. If TypeBox Kind is missing (raw JSON Schema fallback): `coerceWithJsonSchema()` attempts type coercion
6. Failure → `throw Error("Validation failed for tool \"${name}\":\n${errors}\n\nReceived arguments:\n${JSON.stringify}")` → `isError: true` tool result

### 4.4 Tool Execution Pipeline

```
AgentMessage arrives with toolCall[] blocks
  ↓
if stopReason === "length": fail ALL tool calls (truncated args unsafe)
  ↓
else:
  For each toolCall:
    prepareToolCall()          → validate args, run beforeToolCall hook
    if blocked by hook → immediate error result
    executePreparedToolCall()  → tool.execute(toolCallId, args, signal, onUpdate, ctx)
    finalizeExecutedToolCall() → run afterToolCall hook (can override result)
  ↓
  All toolResult messages collected, emitted as message_start/message_end
  ↓
  Agent loop checks: if every finalized result has terminate=true → stop, else continue
```

**Parallel execution** (default): all tool calls in a batch execute concurrently after sequential preparation. Tool execution updates are emitted via throttled `onUpdate` callbacks.

**Sequential execution**: triggered when `config.toolExecution === "sequential"` OR any tool has `executionMode === "sequential"`.

### 4.5 Tool Result Shape

```typescript
// agent-loop.js → createToolResultMessage()
{
  role: "toolResult",
  toolCallId: string,
  toolName: string,
  content: Array<TextContent | ImageContent>,  // always []
  details: any | undefined,                     // tool-specific metadata
  usage?: Usage,
  addedToolNames?: string[],                    // for deferred tool loading
  isError: boolean,
  timestamp: number,
}
```

`AgentToolResult` (what tools return):
```typescript
{
  content: Array<TextContent | ImageContent>;  // null → normalized to []
  details?: any;
  terminate?: boolean;                         // signal agent to stop after batch
  addedToolNames?: string[];                   // dynamic tool registration
  usage?: Usage;
}
```

### 4.6 Error Handling

Errors propagate as `isError: true` tool results with `content: [{ type: "text", text: errorMessage }]`. The agent loop catches all errors from `tool.execute()` and wraps them in `createErrorToolResult()`.

**Truncation safety**: When `stopReason === "length"`, every tool call in the batch is failed with:  
> "Tool call \"${name}\" was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments."

This prevents executing silently-corrupt partial-JSON arguments.

### 4.7 Retry & Recovery

**Two-layer retry architecture:**

**Layer 1: Provider-level** (`pi-ai/utils/retry.js` → `retryAssistantCall()`)
- Bounded exponential backoff: `baseDelayMs * 2^(attempt-1)`
- Retryable patterns (regex, case-insensitive): `"overloaded"`, `"rate.?limit"`, `"429"`, `"500"`, `"502"`, `"503"`, `"504"`, `"connection.?refused"`, `"fetch failed"`, `"timed? out"`, `"stream ended before"`, `"ResourceExhausted"`, etc.
- Explicit non-retryable: `"GoUsageLimitError"`, `"FreeUsageLimitError"`, `"insufficient_quota"`, `"out of budget"`, `"quota exceeded"`, `"billing"`
- Abort is terminal — never retried
- Callbacks: `onRetryScheduled`, `onRetryAttemptStart`, `onRetryFinished`

**Layer 2: Session-level** (`dist/core/agent-session.js` → `_prepareRetry()`)
- Triggers when `isRetryableAssistantError(message)` AND `!isContextOverflow(message)`
- Configurable: `settings.retry.maxRetries` (default 999999 in this install), `settings.retry.baseDelayMs` (500ms)
- On retry: removes the failed assistant message from context, sleeps with backoff, re-runs agent loop
- Emits `auto_retry_start` / `auto_retry_end` events for TUI rendering
- Context overflow → NOT retried; handled by compaction instead

### 4.8 Agent Loop Steer Model

```
runLoop():
  while true:
    pendingMessages ← getSteeringMessages()  // typed user input during agent work
    while hasMoreToolCalls || pendingMessages:
      inject pendingMessages as new context
      streamAssistantResponse()
      if stopReason in ("error","aborted"): emit agent_end; return
      executeToolCalls()
      lastCompletedTurn ← {message, toolResults, context}
      if shouldStopAfterTurn(lastCompletedTurn): emit agent_end; return
      pendingMessages ← getSteeringMessages()
    followUpMessages ← getFollowUpMessages()  // extension-triggered follow-ups
    if followUpMessages: pendingMessages = followUpMessages; continue
    break
```

**Queue modes**: `"all"` (drain all queued messages at drain point) or `"one-at-a-time"` (inject oldest only, re-poll on next drain point).

---

## 5. System Prompt (Verbatim)

Reconstructed by calling `buildSystemPrompt()` with default 7-tool set. Path-invariant core (with absolute paths replaced by their token-bucket equivalents):

```
You are an expert coding assistant operating inside pi, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.

Available tools:
- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)
- edit: Make precise file edits with exact text replacement, including multiple disjoint edits in one call
- write: Create or overwrite files
- grep: Search file contents for patterns (respects .gitignore)
- find: Find files by glob pattern (respects .gitignore)
- ls: List directory contents

In addition to the tools above, you may have access to other custom tools depending on the project.

Guidelines:
- Be concise in your responses
- Show file paths clearly when working with files

Pi documentation (read only when the user asks about pi itself, its SDK, extensions, themes, skills, or TUI):
- Main documentation: {readmePath}
- Additional docs: {docsPath}
- Examples: {examplesPath} (extensions, custom tools, SDK)
- When reading pi docs or examples, resolve docs/... under Additional docs and examples/... under Examples, not the current working directory
- When asked about: extensions (docs/extensions.md, examples/extensions/), themes (docs/themes.md), skills (docs/skills.md), prompt templates (docs/prompt-templates.md), TUI components (docs/tui.md), keybindings (docs/keybindings.md), SDK integrations (docs/sdk.md), custom providers (docs/custom-provider.md), adding models (docs/models.md), pi packages (docs/packages.md), environment variables (docs/environment-variables.md)
- When working on pi topics, read the docs and examples, and follow .md cross-references before implementing
- Always read pi .md files completely and follow links to related docs (e.g., tui.md for TUI API details)
Current working directory: {cwd}
```

**Measured size**: 4-tool variant = 1,910 chars; 7-tool variant = 2,050 chars (measured via `buildSystemPrompt()` reconstruction).  
**Estimated token count**: ~460–510 tokens. pi's prompt is intentionally **well under 1,000 tokens**.

### What pi's prompt omits (vs omp)

| Feature | omp | pi |
|---------|-----|-----|
| Tool descriptions in system prompt | Full per-tool `<instructions>` blocks | One-liner snippets only; full descriptions on tool definition object |
| Style/behavior directives | `§ Role`, `§ Engineering`, `§ Coop`, `§ Workflow`, `§ Delivery`, etc. | "Be concise" + "Show file paths" — nothing else |
| Safety constraints | `NEVER` / `AVOID` / `MAY` / `RECOMMENDED` levels | None in system prompt |
| Delegation patterns | Subagent spawning instructions, hub/peer messaging | None (delegated to `pi-subagents` extension) |
| Todo/task tracking | In system prompt | Extension (`rpiv-todo`) — absent from base prompt |
| Project-specific CLAUDE.md / project instructions | Appended to system prompt | Via `contextFiles` → `<project_context>` XML wrapper |

**System prompt philosophy**: pi keeps the system prompt minimal (~400 tokens) and pushes behavioral complexity to tool descriptions and extension hooks. The system prompt is a directory to pi's own docs, not a behavior specification.

---

## 6. Tool Execution Timeout Model

- **bash/powershell**: No default timeout; `timeout` field is optional. Hard max: `2,147,483,647` ms (~24.8 days).
- **read/edit/write/grep/find/ls**: No timeout parameter. Cancellation via `AbortSignal` (session abort, retry abort, user interrupt).
- **Abort handling**: Each tool checks `signal?.aborted` at multiple await points (`throwIfAborted()`) to ensure cleanup via `withFileMutationQueue` (serializes writes to same file, abort-safe).

---

## 7. Environment Variables for LLM Tools

Shell tools receive `PI_*` environment variables per session (documented in `docs/environment-variables.md`):

| Variable | Content |
|----------|---------|
| `PI_CODING_AGENT` | `true` — lets child processes detect pi |
| `PI_SESSION_ID` | Current session UUID |
| `PI_SESSION_FILE` | Absolute path to session JSONL (unset for ephemeral) |
| `PI_PROVIDER` | Selected model provider name |
| `PI_MODEL` | Selected model ID |
| `PI_REASONING_LEVEL` | `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max` |

Values resolved at command start — model/reasoning changes take effect on the next shell command.

**Sanitization mechanism** (`bash.js` → `resolveSpawnContext()`): pi first **deletes** all `PI_*` variables from the inherited environment, then re-sets them from the live session context. This prevents a stale outer `PI_MODEL` (e.g. from a parent pi process) from leaking into the child. A `spawnHook` can then rewrite `{ command, cwd, env }` before spawn. `getShellEnv()` (`utils/shell.js`) prepends the agent `bin` dir (where auto-downloaded `rg`/`fd` live) to `PATH`.

---

## 8. Deltas vs `2026-09-09-omp-pi-architecture-go-rebuild.md`

| Blueprint claim | Verified status | Evidence |
|-----------------|-----------------|----------|
| "8 built-in tools" (§IV.7) | **Confirmed + expanded** | 8 tools enumerated; exact schemas above; `powershell` not mentioned in blueprint |
| "read/write/edit/bash exactly per pi's input schemas" | **Confirmed** | Schemas quoted above — edit is `edits[]` array (multi-edit), not single oldText/newText |
| "edit UX: hashline anchors, PUT N.=M:, MV, CUT, REM" | **omp-only** | pi's edit is an `edits[]` array of exact oldText/newText matches; no hashline anchors — the hashline anchor UX is omp's own extension of the idea |
| "grep/fd shell out to rg/fd; pure-Go fallback" | **Corrected** | grep spawns `rg --json --line-number --color=never --hidden`; find spawns `fd --glob --color=never --hidden` (with git-repo-aware gitignore handling, pi issue #5960). If missing, pi **auto-downloads** the binary (`ensureTool()` in `dist/utils/tools-manager.js`; skipped in offline mode; Termux requires `pkg install`). There is no Node fallback in production — absence is a hard error. Only custom `operations.glob` (SDK/extension injection) bypasses fd |
| "Port the env-hardening list and OutputSink byte-for-byte" | **Confirmed** | OutputSink → `OutputAccumulator` (dist/core/tools/output-accumulator.js), 183 lines; session env exposed via `getShellEnv()` in `utils/shell.js` |
| "Approvals: port the tier model" | **NOT in pi core** | Pi has no built-in approval gates; approval is extension-loaded (`tool-approval.ts` in pi-mcp-adapter); base pi trusts all tools. The tier model is omp's feature |
| "Partial-JSON tool args: 256-byte throttle + relaxed repair parser" | **Confirmed + detailed** | Repair parser is 125 lines (`json-parse.js`); `partial-json` library handles incomplete JSON; no explicit 256-byte throttle in pi — streaming is unthrottled in the adapters |
| "MCP stays optional — pi ships without it" | **Confirmed** | Zero MCP in pi core; all MCP is via `pi-mcp-adapter` extension. The mcp-cache.json/mcp-onboarding.json on this machine are from the adapter, not pi core |
| "Subagents: same session core + outputSchema" | **Confirmed** | pi-subagents extension package; `yield` tool is extension-defined (not a built-in) |
| "Tool execution mode: parallel default, sequential override" | **New detail** | Agent-loop source confirms parallel by default; tools can set `executionMode: "sequential"` per-tool |
| "beforeToolCall/afterToolCall hooks" | **New detail** | `config.beforeToolCall` can block tool calls; `config.afterToolCall` can override results — used by extension `tool_call`/`tool_result` events |
| Retry settings: `baseDelayMs: 500`, `maxRetries: configurable` | **New detail** | This install has `maxRetries: 999999` (effectively infinite); exponential backoff confirmed |

---

## 9. What the Blueprint Missed

1. **Tool `promptSnippet` gating**: Extension tools without `promptSnippet` are invisible in the system prompt — used by `mcp` direct tools and `mcp_script` to avoid burning context tokens.

2. **`stopReason === "length"` safety**: When the output token limit truncates, ALL tool calls in that batch are failed without execution. This prevents silent corruption from partial-JSON args that happen to parse.

3. **`parseStreamingJson` three-layer fallback**: `JSON.parse` → `repairJson` → `partial-json` library → `partialJson(repairJson(partialJson))` → `{}`. This is the "best-effort JSON salvage parser" mentioned in the agent-loop comment.

4. **`addToolNames` in tool results**: Supports deferred tool loading — a tool can register new tools dynamically (used by `pi-mcp-adapter` for direct tool discovery).

5. **`terminate` signal in tool results**: Extensions can signal early termination via `{ terminate: true }` on blocked calls; batch stops only if EVERY finalized result is terminating.

6. **MCP adapter proxy tool schema**: The `mcp` tool is a multi-action gateway (`search`, `describe`, `connect`, `tool+args`, `action: auth-start/auth-complete`), not a simple tool proxy. Each MCP tool invocation goes through the proxy tool — no native tool-call dispatch.

7. **`PI_REASONING_LEVEL` env var**: Reasoning level is exposed as a string to shell commands, enabling commands to adapt behavior based on whether thinking is enabled.

8. **Edit backward compatibility shim**: `prepareEditArguments()` silently converts legacy `{ oldText, newText }` single-edit format and stringified JSON into the modern `edits[]` array format — models that were fine-tuned on older pi versions still work.

---

## 10. xdev Impact

| Recommendation | Milestone | Notes |
|----------------|-----------|-------|
| Port the 4 core tools (read/write/edit/bash) with pi's exact schemas | **M3** (#4) | Model familiarity with pi's tool schemas is an asset; preserve exact field names and descriptions |
| Port `parseStreamingJson` three-layer fallback for partial-JSON repair | **M3** (#4) | Port the `repairJson` logic (Go equivalent: repair malformed JSON string literals); `partial-json` library → Go partial parser |
| Port `OutputAccumulator` → Go's bounded output sink with temp-file overflow | **M3** (#4) | Line/byte dual-limit truncation (2000 lines / 50KB); keep same truncation notices for model context |
| Port `validateToolArguments` coercion → Go TypeBox-equivalent (jsonschema validation) | **M3** (#4) | Type coercion + null normalization + structured error messages |
| Implement `beforeToolCall`/`afterToolCall` extension hooks | **M3** (#4) | Agent-loop extensibility without modifying core; used by tool_call/tool_result events |
| Port `stopReason === "length"` tool-call failure safety | **M3** (#4) | Prevents executing silently-corrupt partial tool-call args |
| Port the 4-level tool-execute → prepare → validate → hook pipeline | **M3** (#4) | Clean separation of concerns; hooks can block/modify without touching core |
| Add `grep`/`find`/`ls` as optional explore tools | **M3** (#4) | Separated from core tools; toggle per-session based on config |
| Port `mcp` proxy tool pattern for MCP integration | **M7** (#7) | ~200-token proxy gateway vs 10k+ tokens for direct tool registration; on-demand discovery |
| Implement extension `registerTool` API with `promptSnippet` gating | **M7** (#7) | Tools without snippets invisible in system prompt; critical for context budget management |
| Port the minimal system prompt philosophy | **M0** (#1) | ~500 tokens base; push behavioral complexity to tool descriptions and extension hooks |
| Port `PI_*` environment variable exposure to shell tools | **M4** (#5) | Enables child processes to detect agent context and adapt |
| Implement `terminate` signal for batch early-stop | **M3** (#4) | Extensions can signal "stop after this batch" without another model call |
| Port `addedToolNames` for deferred/dynamic tool loading | **M7** (#7) | Enables MCP-style lazy tool registration without pre-loading all schemas |
| Port auto-retry with two-layer architecture (provider + session) | **M6** (#6) | Provider retry: transient errors only; session retry: full turn replay; context overflow → compaction, not retry |

---

## 11. Issues to Update

- **#1 (M0 skeleton)**: Add acceptance criterion: system prompt < 500 tokens base, with tool snippets rendered inline (not full descriptions); document the minimal-prompt philosophy from pi as a design axiom.
- **#2 (M1 providers)**: Add acceptance criterion: provider adapters stream partial-JSON tool args with `repairJson` → fallback to partial parser; when `stopReason === "length"`, all tool calls in batch must fail-safe (never execute truncated args).
- **#3 (M2 session core)**: Add acceptance criterion: session JSONL stores `ToolResultMessage` with `toolCallId`, `toolName`, `isError`, `details`, and `addedToolNames` for deferred tool loading; `partialJson` is never persisted.
- **#4 (M3 agent loop + 4 tools)**: Add acceptance criterion: agent loop implements the 4-stage tool pipeline (prepare → validate → execute → finalize) with `beforeToolCall`/`afterToolCall` hooks; tool schemas match pi's TypeBox definitions (oldText/newText `edits[]` array for edit, `timeout` optional for bash, no default timeout).
- **#5 (M4 TUI)**: Add acceptance criterion: tool execution events include `tool_execution_start`, `tool_execution_update` (throttled streaming output), and `tool_execution_end` for live rendering.
- **#6 (M5 compaction + retry)**: Add acceptance criterion: two-layer retry (provider-level transient retry with exponential backoff + session-level turn replay); context overflow routed to compaction, never to retry; retry settings configurable via `settings.retry`.
- **#7 (M6 RPC + subagents + MCP)**: Add acceptance criterion: MCP integration via proxy tool pattern (~200 tokens base cost); proxy tool supports `search`, `describe`, `connect`, `tool+args`, `action: auth-start|auth-complete`; direct tool mode available via config for high-frequency servers; tool approval is per-call with `Allow once`/`Allow for session`/`Deny` choices.
