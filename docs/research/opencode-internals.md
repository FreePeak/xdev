# OpenCode Internals — Deep Primary-Source Research (v1.18.28)

> **Source**: opencode binary at `~/.opencode/bin/opencode` (144 MB Mach-O arm64, Bun-compiled); `~/.local/share/opencode/opencode.db` (SQLite); `~/.config/opencode/opencode.json` config; embedded JS extracted via `strings` + mmap search. All prompts/tools quoted verbatim below; file paths marked `[BIN#<byte-offset>]`. Version: `opencode --version` = 1.18.28; npm plugin `@opencode-ai/plugin@1.14.22`, `@kilocode/plugin@7.2.20`.

**Existing research baseline**: No prior opencode-specific docs in-repo. This is a fresh full inventory. Cross-checked against `parity-*.md` claims; corrections noted where found.

---

## 1. Binary & Runtime Architecture

opencode compiles to a Bun standalone binary. The JS is embedded in `$bunfs/root/chunk-*.js` virtual files, minified and tree-shaken into ~12 chunks. The binary ships an embedded TUI (HTML/CSS/JS assets for Web-based terminal UI at `/$bunfs/root/_headers-*`).

**Storage backend**: SQLite at `~/.local/share/opencode/opencode.db` (migration-based schema, WAL mode). Legacy JSON files exist under `~/.local/share/opencode/storage/session_diff/` from pre-SQLite versions.

**Install paths**:
| Artifact | Path |
|---|---|
| Binary | `~/.opencode/bin/opencode` |
| Plugins (npm) | `~/.opencode/node_modules/{@opencode-ai,@kilocode}/` |
| Global config | `~/.config/opencode/opencode.json` |
| Project config | `./opencode.json` or `.opencode/opencode.json` |
| Data (SQLite) | `~/.local/share/opencode/opencode.db` |
| Agent defs | `.opencode/agent(s)/<name>.md` |
| Skills | `.opencode/skill(s)/<name>/SKILL.md` |
| Commands | `.opencode/command(s)/<name>.md` |

---

## 2. Tool Inventory & Schemas

All tools are registered via a `j(name, factory)` factory that produces `{description, parameters: Struct, execute: Effect}`. The registry is `ToolRegistry`. Below is every discovered tool ID with its parameter schema and key behavioral semantics.

### 2.1 Core File Tools

#### `read`
```ts
ReadParams = {
  filePath: string          // absolute path to file or directory
  offset?: number           // 1-indexed line number to start from
  limit?: number            // max lines (default 2000)
}
```
**Behavior**:
- Returns `<content>` blocks with lines prefixed `N: content`
- Lines >2000 chars truncated to `2000 chars...`
- Cap: 2000 lines / 51200 bytes (output truncation, saves full output to file)
- Directories: entries listed one per line, trailing `/` for subdirs
- Images and PDFs return as file attachments
- Reads `.env` files for permission ask (controlled by `read` permission patterns)
- Tool result includes `<system-reminder>` blocks from read context files (AGENTS.md etc.)

#### `write`
```ts
WriteParams = {
  filePath: string   // absolute path (must be absolute)
  content: string    // full file content
}
```
**Behavior**:
- Requires reading the file first if it exists (`Read` must precede `Write` on existing files)
- `overwrite: true` internal flag when applicable
- Does not auto-create directories; must use `bash` for that
- Runs post-write LSP diagnostics and diff generation
- Strips BOM when writing; preserves CRLF if originally present

#### `edit`
```ts
EditParams = {
  filePath: string       // absolute path
  oldString: string      // text to replace (must differ from newString)
  newString: string      // replacement text
  replaceAll?: boolean   // replace all occurrences (default false)
}
```
**Behavior — fuzzy matching pipeline** (`Ts` function at `[BIN#15492612]`):
- `oldString` must not be empty; must differ from `newString`
- **9-strategy matcher chain** (first match wins, then optional Levenshtein fallback):
  1. `ws` — exact match
  2. `hs` — line-level match (searches for contiguous lines of `oldString`)
  3. `as` — block-endpoint match (first+last line, middle lenient ±25%)
  4. `bs` — near-exact match (trimmed, whitespace-normalized)
  5. `cs` — de-indented match (strips common leading whitespace)
  6. `Os` — escape-sequence match (processes `\n`, `\t`, `\r`, `\0`, `\\`)
  7. `gs` — trim-then-exact (if trimmed `oldString` ≠ original, tries trimmed)
  8. `qs` — fuzzy-block match (≥3 lines; 50%+ lines match after trim)
  9. `$s` — raw substring match
- **Levenshtein fallback** (threshold 0.65 similarity): if all 9 strategies fail, finds the most similar substring in the file and uses it; returns an error if no match exceeds the threshold
- `replaceAll` replaces all non-overlapping occurrences
- Post-edit: runs LSP diagnostics, generates diff metadata, outputs file path + diff

> **Corrections to parity-*.md**: opencode `edit` is NOT a line-anchored tool. It is string-replace with an aggressive fuzzy recovery pipeline. This is architecturally closer to Claude Code's `Edit` than to omp's proposed `ast_edit`. The fuzzy chain makes it resilient to LLM hallucinated whitespace but adds complexity. [VERIFIED from binary]

#### `apply_patch`
```ts
PatchParams = {
  patchText: string    // codex-style patch text
}
```
**Patch format**:
```
*** Begin Patch
*** Add File: path/to/file
+Line content
*** Update File: path/to/existing
*** Move to: path/renamed
@@ context @@
-old line
+new line
*** Delete File: obsolete.txt
*** End Patch
```
- Parses via internal `Vo.parsePatch`
- Supports: `add`, `delete`, `update`, `move` (rename) operations
- Hunks use `@@` markers with context lines (optional `change_context`)
- Verification: checks file exists before updating, validates no hunks are orphaned
- Post-patch: LSP diagnostics + diff summary

### 2.2 Search Tools

#### `glob`
```ts
GlobParams = {
  pattern: string     // glob pattern (e.g. "**/*.ts")
  path?: string       // directory to search (default: cwd)
}
```
**Description** (verbatim from prompt):
> Fast file pattern matching tool that works with any codebase size. Supports glob patterns like `**/*.js` or `src/**/*.ts`. Returns matching file paths. When doing an open-ended search that may require multiple rounds of globbing and grepping, use the Task tool instead.

#### `grep`
```ts
GrepParams = {
  pattern: string     // regex pattern to search for
  path?: string       // directory (default: cwd)
  include?: string    // file pattern filter (e.g. "*.ts", "*.{ts,tsx}")
}
```
**Description** (verbatim):
> Fast content search tool. Searches file contents using regular expressions. Supports full regex syntax (eg. "log.*Error", "function\\s+\\w+"). Filter files by pattern with the include parameter. When doing an open-ended search that may require multiple rounds of globbing and grepping, use the Task tool instead. If you need to identify/count the number of matches within files, use the Bash tool with `rg` directly.

### 2.3 Shell Tool

#### `bash`
```ts
ShellParams = {
  command: string     // command to execute
  timeout?: number    // timeout in milliseconds
  workdir?: string    // working directory (default: cwd)
}
```
**Key behaviors**:
- **Tool id / permission key**: `bash` (config docs confirm `permission: { "bash": ... }`); the
  executing component is `ShellTool` (internal service name).
- **Shell variants**: bash/zsh on macOS/Linux; dedicated PowerShell (`ji`) and
  cmd.exe (`ki`) prompt renderers on Windows with shell-specific quoting,
  directory-verification (`Test-Path`, `if exist`) and output examples.
- **Persistent shell session** per project (per-shell mutex map keyed by resolved path).
- **Output truncation**: `tool_output.max_lines` / `tool_output.max_bytes`
  (default 200 lines / 8192 bytes per config docs); on overflow the full output
  is saved to a file (`Full output saved to: ${path}`), head- or tail-mode; the
  tool result includes `<shell_metadata>` with `outputPath`.
- Truncation message: "Use Grep to search the full content or Read with
  offset/limit to view specific sections"; in explore-agent contexts additionally:
  "Use the Task tool to have explore agent process this file with Grep and Read
  (with offset/limit). Do NOT read the full file yourself - delegate to save context."
- **Git/PR guidance embedded in description**: inspect `git status`/`git diff`/
  `git log --oneline -10` before committing; never skip hooks, force-push, or
  amend failed commits; use `gh` for GitHub and return the PR URL.
- All modifying commands require user confirmation (permission model).

Full-output files are cleaned up periodically (1-hour delay, then hourly sweep).

### 2.4 Task (Subagent) Tool

#### `task`
```ts
TaskParams = {
  description: string           // 3-5 word description
  prompt: string                // full task for agent
  subagent_type: string         // agent type: "general", "explore", "build", "plan"
  task_id?: string              // resume previous subagent session
  command?: string              // triggered command
  background?: boolean          // run in background (requires experimental flag)
}
```
**Agent types (built-in)**:

| Agent | Mode | Purpose | Tools Available |
|---|---|---|---|
| `build` | primary | Default agent, all tools | All (per permission) |
| `plan` | primary | Plan mode, disallows edits | Read-only + plan_exit + question + task(general:deny) |
| `general` | subagent | Multi-step parallel tasks | All except todowrite |
| `explore` | subagent | Codebase exploration | grep, glob, list, bash, webfetch, websearch, read (NO edit/write) |
| `compaction` | hidden/primary | Context summarization | None (prompt-only) |
| `title` | hidden/primary | Title generation (temp=0.5) | None (prompt-only) |
| `summary` | hidden/primary | PR-style summary | None (prompt-only) |

**Task result format** (returned to parent):
```xml
<task id="${sessionID}" state="${state}">
<summary>${summary}</summary>
<task_result>
${text}
</task_result>
</task>
```
On error:
```xml
<task id="${sessionID}" state="error">
<task_error>
${text}
</task_error>
</task>
```

**Session resumption**: `task_id` parameter enables continuing a previous subagent session instead of creating fresh context.

**Background subagents**: behind `OPENCODE_EXPERIMENTAL_BACKGROUND_SUBAGENTS=true` flag; uses `zs` struct (adds `background` boolean). Returns async notification when complete.

**Default subagent selection**: searches agents by mode !== "subagent" first, then falls back to `build`; looks for default_agent from config, otherwise picks `build`.

### 2.5 Todo Tool

#### `todowrite`
```ts
TodoWriteParams = {
  todos: Info[]    // updated todo list
}
// Info = { content: string, status: "pending"|"in_progress"|"completed"|"cancelled", priority: "low"|"medium"|"high"|"urgent" }
```
**Schema** (verbatim from prompt):
> Create and maintain a structured task list for the current coding session. Tracks progress, organizes multi-step work, and surfaces status to the user.
>
> States: pending, in_progress (exactly ONE at a time), completed, cancelled.
>
> Rules: Update status in real time; don't batch completions. Mark completed only after work is actually done, including verification. Keep exactly one in_progress while work remains.

**DB table**: `todo(session_id, content, status, priority, position, time_created, time_updated)` — position-based ordering, composite PK on (session_id, position).

**Permission**: `todowrite` requires ask + always allow per session.

### 2.6 Web Tools

#### `webfetch`
```ts
WebFetchParams = {
  url: string                    // fully-formed URL
  format?: "text"|"markdown"|"html"   // default: markdown
  timeout?: number               // default: 30000ms
}
```
**Limits**: max response 5MB (`$t=5242880`); timeout 30s default, 120s max. HTTP auto-upgraded to HTTPS.

#### `websearch` (conditional — requires Exa MCP)
```ts
WebSearchParams = {
  query: string
  numResults?: number           // default: 8
  livecrawl?: "fallback"|"preferred"
  type?: "auto"|"fast"|"deep"
  maxCharacters?: number
}
```
Activated via `EXA_API_KEY` env var pointing to Exa MCP endpoint.

### 2.7 Extension Tools

#### `skill`
```ts
SkillParams = {
  name: string    // must match a skill listed in system prompt
}
```
Loads skill SKILL.md content into conversation. Includes `<skill_content>` and `<skill_files>` XML tags in output.

#### `lsp`
```ts
LspParams = {
  operation: "goToDefinition"|"findReferences"|"hover"|
             "documentSymbol"|"workspaceSymbol"|
             "goToImplementation"|"prepareCallHierarchy"|
             "incomingCalls"|"outgoingCalls"
  filePath: string
  line: number      // 1-based
  character: number // 1-based
  query?: string    // workspaceSymbol only
}
```
Built-in LSP integration; no external LSP binary required (uses WASM-based tree-sitter for syntax intelligence).

#### `question`
```ts
QuestionParams = {
  questions: Question[]
  // Question = { question: string, options?: string[], multiple?: boolean, custom?: boolean }
}
```
Interactive ask — prompts user via TUI picker. `custom: true` (default) adds "Type your own answer" option.

#### `plan_exit`
```ts
PlanExitParams = {}    // empty
```
Exits plan mode; triggers user confirmation dialog to switch to build agent. Must be called at end of plan phase.

#### `invalid`
```ts
InvalidParams = {
  tool: string
  error: string
}
```
Internal error handler when tool call has invalid arguments.

### 2.8 Tool Output Truncation Rules

| Tool | Max Lines | Max Bytes | Overflow Behavior |
|---|---|---|---|
| `read` (file) | 2000 | 51200 (50KB) | Truncation message + file path for offset continuation |
| `read` (dir) | 50 | — | `(Showing N of M entries. Use 'offset' to continue)` |
| `bash` | configurable | configurable | Full output saved to file; message instructs Read/Grep |
| `webfetch` | — | 5MB | Content summarized |

Full-output files are cleaned up periodically (1-hour delay, then hourly sweep).

---

## 3. System Prompt Assembly

### 3.1 Prompt Architecture

opencode assembles its system prompt from multiple layers, concatenated in order:

```
[1] Mode prompt (model-dependent)
[2] AGENTS.md rules (discovered from filesystem)
[3] Skills instructions
[4] MCP server instructions
[5] References section (if configured)
[6] User-provided system prompt (from config)
[7] Plan-mode reminder (injected as <system-reminder> when active)
```

### 3.2 Mode Prompts

opencode selects different system prompts based on the **model provider** (detected via `api.id`):

| Variable | Trigger Model Pattern | Description |
|---|---|---|
| `Qi` | `gpt-*` (non-codex), Claude, default | Full interactive mode with 4-line conciseness |
| `Vi` | `gpt-4`, `o1`, `o3` | Agent mode (aggressive, never yields) |
| `Ki` | `gemini-*` | Build mode with extended workflow |
| `Zi` | `gpt-codex` | Codex-specific mode |
| `$i` | other GPT models | Lightweight GPT mode |
| `zi` | `muse-glimmer`, `muse-spark` | Muse-specific (template `{{MODEL_NAME}}`) |

**Model selection function** (`ud` at `[BIN#127130]`):
```js
if (e.api.id.includes("gpt-4") || e.api.id.includes("o1") || e.api.id.includes("o3"))
  return [Vi];              // agent mode
if (e.api.id.includes("gemini-"))
  return [Ki];              // build mode
if (e.api.id.includes("claude"))
  return [Qi];              // default interactive mode
```

### 3.3 Prompts — Verbatim Excerpts

**Default interactive mode** (`Qi` — most common for Claude models):
> You are opencode, an interactive CLI tool that helps users with software engineering tasks. Use the instructions below and the tools available to you to assist the user.
>
> **Tone**: concise, direct, to the point. Fewer than 4 lines of text per response, unless user asks for detail. One word answers are best.
>
> **Code style**: DO NOT ADD ***ANY*** COMMENTS unless asked.
>
> **Task Management**: Use TodoWrite tools VERY frequently to track tasks.
>
> **AGENTS.md**: Markdown files named `AGENTS.md` usually contain the background, structure, coding styles, user preferences and other relevant information about the project.
>
> **Tool policy**: Prefer specialized tools over bash. Use Task tool for open-ended codebase searches. Maximize parallel tool calls.
>
> **Proactiveness**: allowed, but only when user asks. Don't jump into actions without answering questions first.

**Agent mode** (`Vi` — for reasoning models like o1/o3):
> You are opencode, an agent - please keep going until the user's query is completely resolved, before ending your turn and yielding back to the user.
>
> Your thinking should be thorough. You MUST iterate and keep going until the problem is solved.
>
> You MUST plan extensively before each function call, and reflect extensively on the outcomes of the previous function calls. DO NOT do this entire process by making function calls only.
>
> **Workflow**: 1) Fetch URLs via webfetch, 2) Understand problem, 3) Investigate codebase, 4) Research on internet, 5) Develop step-by-step plan, 6) Implement incrementally, 7) Debug as needed, 8) Test frequently, 9) Iterate until root cause fixed, 10) Reflect and validate.

**Build mode** (`Ki` — Gemini models):
> You are opencode, an interactive CLI agent specializing in software engineering tasks. Your primary goal is to help users safely and efficiently.
>
> **Core Mandates**: Rigorously adhere to existing project conventions. NEVER assume a library/framework is available. Verify first.
>
> **Security**: Before executing bash commands that modify the system, explain purpose and impact.
>
> **Tool usage**: Always use absolute paths. Execute multiple independent tool calls in parallel.

### 3.4 Plan Mode

Plan mode is a first-class sub-state of `build` agent. When activated:

**Plan-mode system reminder** (injected via `SessionReminders`):
```xml
<system-reminder>
Plan mode ACTIVE - you are in READ-ONLY phase. STRICTLY FORBIDDEN:
ANY file edits, modifications, or system changes.
## Responsibility
Your current responsibility is to think, read, search, and delegate explore agents
to construct a well-formed plan.
NOTE: At any point in time through this workflow you should feel free to ask the
user questions or clarifications.
</system-reminder>
```

**Plan exit confirmation** (`plan_exit` tool description):
> This tool will ask the user if they want to switch to build agent to start implementing the plan. Call this tool after you have written a complete plan to the plan file.

**Plan file**: stored at `<Path.data>/plans/<session-slug>.md`; `plan` agent's `edit` tool is scoped to `opencode/plans/*.md` only.

### 3.5 AGENTS.md Loading

opencode discovers `AGENTS.md` files up the directory tree from cwd to worktree root:
- Loaded files become `<file>` blocks in the system prompt
- Named files are excluded from re-loading (dedup by path)
- `OPENCODE_DISABLE_PROJECT_CONFIG` env disables project-level AGENTS.md discovery
- Plan agent's AGENTS.md scope is limited to `<Path.data>/plans/*.md`

### 3.6 Prompt Size Estimate

- Default mode prompt: ~2,500 tokens (tight, concise style)
- Agent mode prompt: ~4,000 tokens (verbose workflow + examples)
- Build mode prompt: ~3,500 tokens (extended workflow + security rules)
- AGENTS.md rules: variable, typically 200-2,000 tokens per file
- MCP instructions: variable per server
- Skills: variable per loaded skill
- **Estimated total base**: 3,000–10,000 tokens depending on model/agent and loaded context

---

## 4. Message & Part Format (Session Model)

### 4.1 Part Types (DB: `part` table)

opencode models all conversation content as **parts** within **messages**. Every message has one or more typed parts.

| Part `type` | Purpose | Key Fields |
|---|---|---|
| `text` | User/assistant text | `text: string` |
| `tool` | Tool call + result | `tool: string`, `state: {status, input, output, error, attachments}` |
| `reasoning` | Model reasoning/thinking | `text: string` |
| `step-start` | Marks start of an assistant turn | — |
| `step-finish` | Marks end of assistant turn | — |
| `file` | File attachment (image, etc.) | `mime: string`, `url: string`, `filename?: string` |
| `compaction` | Compaction summary marker | — |
| `patch` | Patch output metadata | `files: [{filePath, type, patch, additions, deletions}]` |

**Part data JSON** (from DB samples):
```json
{"type":"text","text":"# Archify\n\nCreate a self-contained HTML diagram..."}
{"type":"tool","tool":"read","state":{"status":"completed","input":{...},"output":"..."}}
{"type":"reasoning","text":"Let me think about this..."}
```

**Message info** (stored in `message.data`):
```json
{
  "role": "user" | "assistant",
  "id": "msg_...",
  "parentID": "msg_...",
  "sessionID": "ses_...",
  "mode": "chat" | "compaction",
  "agent": "build" | "plan" | "general" | "compaction" | ...,
  "variant": "...",
  "summary": boolean,
  "path": {"cwd": "...", "root": "..."},
  "cost": number,
  "tokens": {"output": N, "input": N, "reasoning": N, "cache": {"read": N, "write": N}},
  "modelID": "...",
  "providerID": "...",
  "time": {"created": ms, "completed"?: ms},
  "error"?: ErrorObject,
  "finish"?: "stop" | "error" | "tool-error"
}
```

### 4.2 Message Rendering (for LLM)

Messages are rendered to LLM format by `vd` and `Me` functions:

**User messages**:
```
[User]: ${text content}
[Attached ${mime}: ${filename}]    // for file parts
```

**Assistant messages** (flat text):
```
[Assistant]: ${text}
[Assistant reasoning]: ${reasoning}
[Assistant tool call]: ${tool}(${JSON.stringify(input)})
[Tool result]: ${output}
[Tool error]: ${error}
```

When rendered for the AI SDK / provider:
- `role: "system"` → concatenated system prompts
- `role: "user"` / `role: "assistant"` → messages with parts

### 4.3 Tool Call Execution Flow

Tool calls go through:
1. **Permission check**: `ask({permission: "edit", patterns: [file], always: ["*"]})`
2. **Zod validation**: parameters validated against tool's `Struct` schema
3. **ToolExecutor**: calls `execute(params, {toolCallId, messages, abortSignal})`
4. **Error handling**: `ToolInvalidArgumentsError` triggers automatic retry with schema hint
5. **Output postprocessing**: truncation, LSP diagnostics, diff generation
6. **Part update**: tool result written as a new `part` row

---

## 5. Permission Model

### 5.1 Permission Schema

```ts
PermissionRule = {
  permission: string    // tool name or action name
  action: "allow" | "deny" | "ask"  // "all" = allow+ask+deny combined
  pattern?: string      // glob pattern for resource matching
}
```

DB table: `permission(project_id, action, resource, time_created, time_updated)` — persisted per-project "always allow" decisions.

### 5.2 Built-in Permission Defaults

**Base permission set** (variable `g` — from `[BIN#66253200]`):
```
"*" → "allow"
doom_loop → "ask"
external_directory → "ask" (with cwd-based allows)
question → "deny"
plan_enter → "deny"
plan_exit → "deny"
read → "allow" (except *.env → "ask", *.env.* → "ask", *.env.example → "allow")
```

**Per-agent overrides**:
- `build`: +question:allow, +plan_enter:allow
- `plan`: +question:allow, +plan_exit:allow, -task(general):deny, -external_directory(coordinator plans):allow, -edit:deny (except `opencode/plans/*.md`:allow)
- `general`: -todowrite:deny
- `explore`: "-" (deny everything), +grep:allow, +glob:allow, +read:allow, +bash:allow, +webfetch:allow, +websearch:allow

### 5.3 External Directory Boundary

All agents get `external_directory:*:allow` appended at the end, meaning the user can always explicitly allow external directories. The tool prompt provides pre-approved temp directory access.

### 5.4 Rule Evaluation Order

From the embedded config docs (verbatim):

> Per-tool value forms: `"allow"` shorthand (treated as `{"*": "allow"}`), or an object `{ pattern: action }`. Within an object, **insertion order matters**. opencode evaluates the LAST matching rule, so put broad rules first and narrow rules last.

> Known permission keys: `read, edit, glob, grep, list, bash, task, external_directory, todowrite, question, webfetch, websearch, lsp, doom_loop, skill`. Some of these (`todowrite, question, webfetch, websearch, doom_loop`) only accept a flat action, not a per-pattern object.

> `external_directory` patterns are filesystem paths (use `~/`, absolute paths, or globs like `~/projects/**`). Per-agent `permission:` overrides top-level `permission:`. Plan Mode lives on the `plan` agent's permission ruleset (`edit: deny *`).

This **last-match-wins** semantics is the opposite of Claude Code's deny-first-wins ordering — a subtle but critical difference for the xdev port.

### 5.5 Escape Hatches (config recovery)

```bash
OPENCODE_DISABLE_PROJECT_CONFIG=1    # skip project opencode.json
OPENCODE_CONFIG=/path/to/file.json   # additional explicit config
OPENCODE_CONFIG_CONTENT='{"..."}'    # inline JSON final merge
OPENCODE_DISABLE_DEFAULT_PLUGINS=1
OPENCODE_PURE=1                      # skip external plugins entirely
OPENCODE_DISABLE_EXTERNAL_SKILLS=1   # skip ~/.claude + ~/.agents skill scans
OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1
```

---

## 6. Sessions & Storage

### 6.1 Database Schema (SQLite)

**`session`**:
```sql
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  workspace_id TEXT,
  parent_id TEXT,           -- fork/branch parent
  slug TEXT NOT NULL,
  directory TEXT NOT NULL,
  path TEXT,                -- session file path (JSONL backup)
  title TEXT NOT NULL,
  version TEXT NOT NULL,
  share_url TEXT,           -- if shared publicly
  summary_additions INTEGER,
  summary_deletions INTEGER,
  summary_files INTEGER,
  summary_diffs TEXT,       -- JSON
  metadata TEXT,            -- JSON
  cost REAL DEFAULT 0,
  tokens_input INTEGER DEFAULT 0,
  tokens_output INTEGER DEFAULT 0,
  tokens_reasoning INTEGER DEFAULT 0,
  tokens_cache_read INTEGER DEFAULT 0,
  tokens_cache_write INTEGER DEFAULT 0,
  revert TEXT,              -- JSON (snapshot for revert)
  permission TEXT,          -- JSON (per-session permission overrides)
  agent TEXT,               -- current agent name
  model TEXT,               -- current model ID
  time_created INTEGER,
  time_updated INTEGER,
  time_compacting INTEGER, -- compaction timestamp
  time_archived INTEGER,
  FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
);
```

**`message`**:
```sql
CREATE TABLE message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  time_created INTEGER,
  time_updated INTEGER,
  data TEXT NOT NULL,       -- JSON (role, tokens, cost, etc.)
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
);
CREATE INDEX idx ON message(session_id, time_created, id);
```

**`part`**:
```sql
CREATE TABLE part (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL,
  session_id TEXT NOT NULL,  -- denormalized for efficient queries
  time_created INTEGER,
  time_updated INTEGER,
  data TEXT NOT NULL,        -- JSON (type, text/tool/reasoning content)
  FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE
);
CREATE INDEX idx ON part(message_id, id);
CREATE INDEX idx ON part(session_id);
```

**`session_message`** (alternate ordering/view):
```sql
CREATE TABLE session_message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,         -- message type
  seq INTEGER NOT NULL,       -- sequence number
  time_created INTEGER,
  time_updated INTEGER,
  data TEXT NOT NULL,
  UNIQUE(session_id, seq)
);
```

### 6.2 Session Lifecycle

| Operation | DB Effect |
|---|---|
| **New session** | Row in `session`, workspace_id optional |
| **Fork** | New session with `parent_id = source session ID` |
| **Compact** | Updates `time_compacting`, creates summary message |
| **Archive** | Sets `time_archived`, retains DB rows |
| **Share** | Creates row in `session_share(session_id, id, secret, url)` |


### 6.3 Context Epochs

```sql
CREATE TABLE session_context_epoch (
  session_id TEXT PRIMARY KEY,
  baseline TEXT NOT NULL,        -- baseline context hash
  snapshot TEXT NOT NULL,        -- current context snapshot
  baseline_seq INTEGER NOT NULL,
  -- ... more fields
);
```
Used for context window management — tracks which parts of the conversation have been compacted vs. fresh.

### 6.4 Todo Table

```sql
CREATE TABLE todo (
  session_id TEXT NOT NULL,
  content TEXT NOT NULL,
  status TEXT NOT NULL,         -- "pending"|"in_progress"|"completed"|"cancelled"
  priority TEXT NOT NULL,       -- "low"|"medium"|"high"|"urgent"
  position INTEGER NOT NULL,
  time_created INTEGER,
  time_updated INTEGER,
  PRIMARY KEY (session_id, position),
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
);
```

---

## 7. Compaction

opencode uses **auto-compaction** when context window approaches limit:

**Trigger**: `compaction: { auto: true, tail_turns: 15 }` — preserves last 15 turns, compacts rest.

**Compaction agent**: hidden `compaction` agent (prompt `re`):
> You are a context summarization agent. You are given a conversation between a user and an agent. Your goal is to produce a structured summary matching the format specified so another coding agent can continue the work.
> Always follow the exact output structure requested by the user prompt. Keep every section, preserve exact file paths and identifiers when known, and prefer terse bullets over paragraphs.

**Flow**:
1. Detects context overflow or auto threshold
2. Creates compaction message with `mode: "compaction"`
3. Older messages summarized by compaction model (small/fast model)
4. Summary part injected as `<system-reminder>` into next user turn
5. Original messages retained in DB (for replay/debugging)

**Overflow handling**: If session exceeds model context even after compaction → `ContextOverflowError` → session enters error state.

**Tail continuation**: After compaction, model receives: "Continue if you have next steps, or stop and ask for clarification if you are unsure how to proceed."

---

## 8. Configuration System

### 8.1 Config Schema (from opencode.ai/config.json)

```ts
{
  $schema: "https://opencode.ai/config.json",
  username?: string,
  model?: "provider/model-id",
  small_model?: "provider/model-id",
  default_agent?: string,
  shell?: string,
  share?: "manual" | "auto" | "disabled",
  autoupdate?: boolean | "notify",
  snapshot?: boolean,
  instructions?: string[],          // extra context files
  agent?: Record<string, AgentConfig>,
  command?: Record<string, CommandConfig>,
  provider?: Record<string, ProviderConfig>,
  mcp?: Record<string, McpConfig>,
  permission?: Record<string, string | Record<string, string>>,
  tool_output?: { max_lines: number, max_bytes: number },
  compaction?: { auto: boolean, tail_turns: number },
  experimental?: {
    primary_tools?: string[],
    mcp_timeout?: number,
  },
  formatter?: boolean | string,
  lsp?: boolean | string,
  plugin?: Array<string | [string, object]>,
  references?: Record<string, ReferenceConfig>,
}
```

### 8.2 Provider Config (OpenAI-compatible example)

```json
{
  "provider": {
    "onegw": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "onegw Local",
      "options": {
        "baseURL": "http://127.0.0.1:8080/v1",
        "apiKey": "sk-...",
        "timeout": 600000,
        "chunkTimeout": 120000
      },
      "models": {
        "free": {
          "name": "onegw free",
          "reasoning": true,
          "limit": { "context": 1000000, "output": 65536 }
        }
      }
    }
  }
}
```

Uses `@ai-sdk/openai-compatible` npm package for custom providers; built-in providers use native SDK adapters (Anthropic, OpenAI, Google, etc.).

---

### 8.3 Plugin Hook Surface (verified from embedded docs)

A plugin module exports `default` (or any named export) of type
`Plugin = (input: PluginInput, options?) => Promise<Hooks>`. Hook surface
(mutate `output` in place; return `void`):

- `event(input)` — every bus event
- `config(cfg)` — once on init with the merged config
- `chat.message`, `chat.params`, `chat.headers`
- `tool.execute.before`, `tool.execute.after` (mutate `output.args` pre-run)
- `tool.definition`
- `command.execute.before`
- `shell.env`
- `permission.ask`
- `experimental.chat.messages.transform`, `experimental.chat.system.transform`,
  `experimental.session.compacting`, `experimental.compaction.autocontinue`,
  `experimental.text.complete`

Special object-shaped hooks (not callbacks): `tool: { my_tool: {...} }`,
`auth: {...}`, `provider: {...}`.

Plugin config entries: npm spec (`opencode-gemini-auth`), pinned
(`opencode-foo@1.2.3`), file path (`./local-plugin.ts`), file URL
(`file:///abs/plugin.js`), or tuple form (`["opencode-bar", { "key": "val" }]`).
Auto-discovered: any `*.ts`/`*.js` in `.opencode/plugin/` or `.opencode/plugins/`.

---

## 9. Session Sharing

**DB table**: `session_share(session_id, id, secret, url, time_created, time_updated)`

Sharing modes (from config):
- `"disabled"`: no sharing
- `"manual"`: user initiates with `/share`
- `"auto"`: every session gets a share URL

The `share_url` is stored on the `session` row. Sharing generates a crypto secret for access control.

**Forking**: Forks create a new session with `parent_id` pointing to source. Messages are NOT copied; fork starts fresh. The fork inherits the workspace but not the conversation history.

---

## 10. Commands & Skills Systems

### 10.1 Commands

**Discovery**: `**/*.md` in `opencode/commands/` directories (project-first, then user)
**Frontmatter**:
```yaml
description: One sentence describing what the command does.
agent: build                           # optional
model: anthropic/claude-sonnet-4-6     # optional
```
- Template body (below frontmatter) is the prompt; `$ARGUMENTS`, `$1`, `$2` replaced with user input
- Commands are invoked as `/command-name [args]`

### 10.2 Skills

**Discovery**: `**/SKILL.md` in `opencode/skills/` directories
**Frontmatter**:
```yaml
name: my-skill                # lowercase hyphen-separated, ≤64 chars
description: One sentence covering what this skill does AND when to trigger it.
license?: string
compatibility?: string
metadata?: Record<string, string>
```
- Skills without descriptions are filtered out
- `skill://<name>` protocol in system prompt references loaded skills
- External skills also discovered from `~/.claude/skills/`, `~/.agents/skills/`

### 10.3 TUI Keybindings

```json
{
  "leader_timeout": 2000,
  "keybinds": {
    "leader": "ctrl+x",
    "session_delete": "ctrl+d,<leader>w,alt+w",
    "app_exit": "ctrl+c,ctrl+d,<leader>q",
    "session_new": "<leader>n",
    "session_list": "<leader>l"
  }
}
```

---

## 11. Agent Configuration

### 11.1 Agent Definition Schema

```ts
AgentConfig = {
  name: string,
  description?: string,
  mode: "primary" | "subagent" | "all",
  native?: boolean,
  hidden?: boolean,
  topP?: number,
  temperature?: number,
  color?: string,
  permission: Ruleset,           // PermissionRule[]
  model?: { modelID: string, providerID: string },
  variant?: string,
  prompt?: string,               // from frontmatter body or inline
  options?: Record<string, any>,
  disable?: boolean,
}
```

### 11.2 Custom Agent Definition (Markdown file)

```yaml
---
description: Reviews PRs for style violations.
mode: subagent
model: anthropic/claude-sonnet-4-6
permission:
  edit: deny
  bash: ask
temperature: 0.7
---
You are a strict PR reviewer. Focus on...

```

File body becomes the agent's `prompt`.

### 11.3 Agent Selection

**Default agent** resolution (`default_agent` config → fallback to `build`):
1. Check `default_agent` config
2. Filter agents by `mode !== "subagent"` (primary only)
3. Prefer named match
4. Fall back to `build` (first primary, non-hidden)

### 11.4 User-Defined Agents

User can define custom agents in `opencode.json` or `.opencode/agent(s)/<name>.md`. Custom agents with `disable: true` remove the built-in agent. Unknown fields in frontmatter are silently routed into `options`.

---

## 12. Corrections to Prior Research (parity-*.md)

| Claim in parity doc | Correction / Evidence |
|---|---|
| parity-session-ux.md: opencode commands at `~/.config/opencode/commands` user-first | **Correct** — binary confirms this path. Project-first for native commands in other harnesses. |
| parity-tools-providers.md: opencode referenced as MCP-config import source | **Correct** — opencode supports `.mcp.json` config, importable into omp's MCP loader. |
| parity-session-ux.md: "opencode 55" command provider priority | **Plausible** — binary shows opencode commands are lower-priority in omp's provider chain; not independently verifiable from opencode source alone. |
| parity-knowledge-ui.md: opencode referenced as skill provider at priority 55 | **Confirmed** — opencode skills exist but are secondary to native omp skills. |
| [NEW] opencode `edit` is string-replace, not line-anchored | **Critical correction**: no parity doc covers opencode's edit semantics in detail. opencode uses string-replace with 9-strategy fuzzy recovery. NOT line-anchored. [VERIFIED] |
| [NEW] opencode has plan mode as first-class sub-state | Not previously documented in parity docs. Plan mode has dedicated `plan_exit` tool and `plan` agent with scoped edit permissions. [VERIFIED] |
| [NEW] opencode uses SQLite for all session storage | Legacy JSON storage exists but SQLite is the current primary backend. [VERIFIED] |

---

## 13. xdev Impact

### What xdev should port from opencode

| Feature | Priority | Milestone | Notes |
|---|---|---|---|
| **String-replace edit with fuzzy recovery** | CORE | M4 | The 9-strategy fuzzy matching is battle-tested and significantly more forgiving than naive string-replace. Port the full `Ts` matcher chain. |
| **Permission model with glob patterns** | CORE | M3 | Clean pattern-based permission system (`allow/deny/ask` per action with glob resource matching). Simpler than omp's approval modes. |
| **Part-based message format** | CORE | M2 | Type-discriminated parts (text, tool, reasoning, step-start/finish) enable clean serialization. Follow the SQLite schema pattern. |
| **Plan mode as sub-state** | CORE | M11 | First-class plan mode with `plan_exit` tool, read-only restriction, and explore-agent delegation. Port the system-reminder injection approach. |
| **Auto-compaction with tail preservation** | CORE | M6 | Tail-turns preservation + summary-injection is clean. Port the compaction agent prompt and ContextOverflowError handling. |
| **AGENTS.md discovery** | CORE | M10 | Simple upward-walk discovery from cwd to worktree root. Well-documented behavior. |
| **Skills system (SKILL.md)** | CORE | M12 | Clean file-based skill discovery with frontmatter. Port the `skill://` protocol for lazy loading. |
| **Commands system (markdown files)** | CORE | M10 | Markdown commands with `$ARGUMENTS` expansion. Cheap to implement. |
| **Task tool with session resumption** | CORE | M11 | `task_id` parameter for continuing subagent sessions is powerful. Port the `task_id` resume feature. |
| **Session sharing** | NICE | M14 | Crypto-protected URLs. Lower priority for Go rebuild. |
| **TUI keybindings** | NICE | M12 | Configurable keybinds. Simple to implement. |
| **LSP tool (built-in)** | NICE | M13 | Tree-sitter WASM integration. Consider porting if xdev needs code intelligence. |
| **External directory boundary** | NICE | M10 | Clean pattern for allowing/disallowing paths outside workspace. |

### What xdev should NOT port

| Feature | Reason |
|---|---|
| **9-strategy fuzzy matching (full complexity)** | Over-engineered for most use cases; consider porting 2-3 core strategies (exact, whitespace-normalized, trim-then-exact) and leaving Levenshtein fallback as optional. |
| **WebSearch via Exa MCP** | Too provider-specific; xdev should use configurable provider chain. |
| **opencode.json provider format** | Uses `@ai-sdk/*` npm packages; xdev should use its own Go-native provider system. |
| **TUI as embedded HTML/CSS/JS** | opencode bundles a Web-based TUI; xdev should use native terminal rendering (Bubbletea or similar). |
| **Bun standalone binary** | Bun compilation approach is not relevant for Go rebuild. |

---

## 14. Issues to Update

- **#1 (M0 skeleton)**: Add opencode as a reference architecture for tool registration pattern (`j(name, factory)` → `ToolRegistry`); recommend adopting the Struct-based parameter validation pattern (Zod/Effect schema) for Go port.
- **#2 (M1 providers)**: Add opencode's `@ai-sdk/openai-compatible` pattern as reference for custom provider configuration; note opencode uses `npm` field for adapter packages, not pure config.
- **#3 (M2 session core)**: Add opencode's SQLite schema as a reference for the message/part/event table design; specifically the `session_context_epoch` table for tracking compaction state and the denormalized `session_id` on `part` table for efficient queries.
- **#4 (M3 agent loop + 4 tools)**: Add opencode's permission model (glob-pattern-based `allow/deny/ask` rules per tool action) as a CORE reference pattern; note the `external_directory` boundary concept. Update edit-tool spec to reference opencode's string-replace with fuzzy recovery as an alternative to line-anchored edit.
- **#5 (M4 TUI)**: Add opencode's keybinding config system as reference; note TUI renders HTML/CSS not terminal-native (different approach from xdev).
- **#6 (M5 compaction + retry)**: Add opencode's auto-compaction with tail-turns preservation and compaction agent prompt as reference; note the `ContextOverflowError` handling pattern.
- **#7 (M6 RPC + subagents + MCP)**: Add opencode's task tool with `task_id` session resumption as a CORE feature to port; note background subagent support behind experimental flag.
- **#8 (M7 ext protocol)**: Add opencode's plugin system (`PluginInput → Hooks` interface) as reference for extension protocol; note the `tool.execute.before/after` hook pattern.
- **#9 (M8 memory audit)**: No direct opencode relevance (no memory system); skip.
- **#10 (M9 model roles/auth/config)**: Add opencode's per-model provider options (`timeout`, `chunkTimeout`, `reasoning` flag) as reference; note `small_model` config as a dedicated fast-model slot.
- **#11 (M10 session UX)**: Add opencode's `AGENTS.md` discovery (walk up from cwd to worktree root), commands system (`$ARGUMENTS` expansion), and skill system (`SKILL.md` + `skill://` protocol) as CORE features.
- **#12 (M11 agent system)**: Add opencode's plan mode as a first-class agent sub-state (with `plan_exit` tool, read-only restriction, and explore-agent delegation) as CORE feature; note 4 user-visible built-in agents (build, plan, general, explore) + 3 hidden internal agents (compaction, title, summary).
- **#13 (M12 knowledge/chrome)**: Add opencode's TUI keybinding config, theme support, and statusline plugin as NICE references.
- **#14 (M13 extended tools)**: Add opencode's `lsp` tool (tree-sitter WASM), `skill` loader, and `apply_patch` (codex-style patch format) as NICE features to consider porting.
- **#15 (M14 v2 modes)**: Add opencode's session sharing (crypto-protected URLs) and background subagent support as NICE v2 features.
