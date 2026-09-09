# Claude Code Internals — Deep Research: Tools, Rename, Cross-Session Communication

> Primary-source research on Claude Code 2.1.263 (199MB Mach-O arm64 Bun-compiled binary at `~/.local/bin/claude`; live state in `~/.claude/`). All claims grounded in binary string extraction, live JSONL transcript analysis, and `settings.json` inspection unless marked [INFERENCE].

## Deltas vs prior cc-*.md docs

| Area | Prior docs (`cc-*.md`) | This doc (new/corrected) |
|---|---|---|
| **Bundle architecture** | `cc-bundle-forensics.md`: 190MB, two embedded bundle copies, prompt byte offsets | Updated: 199MB binary (v2.1.263 > v2.1.263 reported earlier — same version, fresh file size confirmed via `wc -c`). Byte offsets verified stable. |
| **Tool inventory** | `cc-mainprompt.txt`: Read, Grep, WebSearch, WebFetch, Write, AskUserQuestion | Expanded: **19 built-in tools** enumerated below (adds Edit, Glob, Bash, Task, TodoWrite, EnterPlanMode, ExitPlanMode, Monitor, BashOutput, KillShell, NotebookEdit, Workflow, Skill, StructuredOutput). Tool names extracted from binary constants. |
| **System prompt** | `cc-mainprompt.txt` has verbatim Read/Grep/WebSearch/WebFetch/Write/AskUserQuestion blocks | New: prompt assembly order documented (§3); title-related prompt strings; skill listing injection; prompt-caching behavior strings found. |
| **Session rename** | Not covered in any prior doc | **Entirely new** (§1): ai-title generation, custom-title via `/rename`, metadata re-append resolution order, `custom-title.json` on-disk artifact. |
| **Cross-session communication** | Not covered in any prior doc | **Entirely new** (§2): SendMessage tool, teammate mailboxes, InboxPoller, structured frame protocol, plan approval flow, permission-gated delivery. |
| **Subagent system** | `cc-explore.txt`: Explore/Plan subagent prompts | Expanded: full subagent lifecycle (§2C) — Agent tool schema, `fork` type, `disallowedTools`, `outputSchema`/StructuredOutput, stall detection, worktree isolation, background auto-resume. |
| **Hooks** | `cc-design-decisions.md`: PreToolUse/PostToolUse/SessionStart | Expanded: live `~/.claude/settings.json` hooks shown with real commands; new events found in binary: `SubagentStart`, `TaskCompleted`, `TeammateIdle`, `FileChanged`, `Stop`. |
| **Compaction** | `cc-compact.txt`: 9-section compaction prompt | No change; compaction prompt verbatim in prior doc remains authoritative. |
| **Env vars** | `cc-envvars.txt`: 600+ env var census | No change; prior census is exhaustive. |
| **Design decisions** | `cc-design-decisions.md`: 14 decisions with verdicts | No change; verdicts remain valid. This doc provides new evidence that reinforces several (AskUserQuestion, hooks, subagent isolation, plan mode). |

---

## 1. Session Rename Flow (priority)

### 1.1 Architecture

Claude Code stores session transcripts as newline-delimited JSONL in:
```
~/.claude/projects/<escaped-project-path>/<session-uuid>.jsonl
```
Subagent transcripts are nested:
```
~/.claude/projects/<escaped-project-path>/<session-uuid>/subagents/agent-<id>.jsonl
```
Each subagent has a companion `agent-<id>.meta.json`.

The **session title** displayed in the resume picker is resolved from three sources in priority order:

### 1.2 Custom Title (`/rename` command)

**Trigger**: User types `/rename` in the TUI.

**Flow** (extracted from binary strings at offset ~325663):
1. TUI shows prompt: `Rename session:` with input field `Enter new session name`
2. User enters new name
3. Appended to session JSONL as:
   ```json
   {"type":"custom-title","customTitle":"My Custom Name","sessionId":"<uuid>"}
   ```
4. On resume-pick display, custom-title entries are scanned `findLast`:
   ```
   .findLast(_e => _e.includes('"type":"custom-title"') && _e.includes('"customTitle":"'))
   ```

**Priority**: Custom title **always wins** over AI title. If `customTitle` is empty/absent, falls through to ai-title.

### 1.3 AI Auto-Title Generation

**Trigger**: Automatic after the first few user turns. The xdev session shows ai-title entries starting at line 16 of 405 (after ~16 messages).

**Flow** (extracted from binary at offset ~346245):
1. Claude Code calls a dedicated LLM endpoint with `querySource: "generate_session_title"`
2. Uses structured output schema:
   ```json
   {
     "type": "json_schema",
     "schema": {
       "type": "object",
       "properties": { "title": { "type": "string" } },
       "required": ["title"],
       "additionalProperties": false
     }
   }
   ```
3. Response parsed via Zod-like validator; on success:
   ```json
   {"type":"ai-title","aiTitle":"OMP todos in xdev PRD","sessionId":"c908aaaa-5dab-4ef5-a2a9-6acd03cd0ca9"}
   ```
4. Appended to session JSONL. **Repeated** on every metadata re-append (the xdev session has 19 identical ai-title entries across 405 lines — same title, re-stamped as transcript metadata).

**Key code path** (binary ~512873):
```javascript
// planReAppendSessionMetadata — title resolution
let customTitle = _.findLast(_e => _e.includes('"type":"custom-title"'));
let aiTitle = _.findLast(_e => _e.includes('"type":"ai-title"'));
// customTitle takes precedence; if both present, currentSessionTitle updated from customTitle first
```

**Telemetry**: `tengu_session_title_generated` event fired on generation.

**Error handling**: `generateSessionTitle failed:` and `updateSessionTitle` log messages found in binary; `[updateSessionTitle] Error:` / `[updateSessionTitle] Failed with status` error strings.

### 1.4 Fallback: First User Prompt

If neither custom-title nor ai-title is present, the session display title falls back to the **first meaningful user prompt**, truncated to 200 chars with ellipsis:
```javascript
let truncated = title.length > 200 ? oe(title, 200).trim() + "…" : title
```
Verified from binary ~416697: `"Display title for the session: custom title, auto-generated summary, or first prompt."`

### 1.5 Title Storage Summary

| Source | JSONL type key | Field | Precedence | Persistence |
|---|---|---|---|---|
| `/rename` command | `custom-title` | `customTitle` | **1 (highest)** | In JSONL + `custom-title.json` on-disk artifact |
| Auto-generated | `ai-title` | `aiTitle` | 2 | In JSONL; re-stamped on metadata re-append |
| First user prompt | N/A (derived) | `last-prompt` type entry | 3 (fallback) | Already in JSONL as first user message |

### 1.6 `--resume` / `--continue` Session Picker

Binary strings confirm the resume picker UI:
```
Resume session
No sessions match "
No conversations found.
No conversations found in this project.
```
With filter modes: `only show current repo`, `only show current branch`, `show all branches`, `only show current worktree`, `show all worktrees`.

The picker reads the JSONL headers (first 3 lines) which include `last-prompt` with `leafUuid` pointing to the last user message UUID, and `mode` / `permission-mode` / `atis-latch` for session state.

### 1.7 Session Metadata Schema (as documented in binary ~416700)

From the binary's type documentation strings:
- `sessionId`: UUID
- `title`: "custom title, auto-generated summary, or first prompt"
- `gitBranch`: "Git branch at the end of the session"
- `cwd`: "Working directory for the session"
- `createdAt`: integer ms since epoch
- `lastModifiedAt`: integer ms since epoch
- `size`: file size in bytes (local storage only)
- `firstPrompt`: "First meaningful user prompt in the session"
- `tag`: "User-set session tag"

---

## 2. Cross-Session Communication (priority)

### 2.1 SendMessage Tool (Inter-Session Messaging)

**Trigger**: Model calls `SendMessage` tool (binary offset ~320728).

**Description** (from binary):
> `SendMessage` — Send a message to another session on this machine.

**Delivery path**:
1. Sender resolves target session by name/id
2. Writes message to recipient's **mailbox** at `.claude/mailbox/` path
3. Recipient's `InboxPoller` polls for unread messages
4. On idle, InboxPoller delivers queued messages as system turns

**Message schema** (from binary ~507925):
```javascript
{
  to: "<session-name-ref>",  // "name [ref]" token
  summary: "...",            // short description for approval dialog
  message: "...",            // full content (may be truncated in preview)
  notify_when_idle: boolean  // optional delivery timing
}
```
Reserved keys: `to`, `summary`, `message`, `notify_when_idle`.

**Permission-gated delivery**:
- Recipient session may require approval before Claude sees the message
- Approval dialog shown with sender info:
  ```
  A message from another session needs your approval
  ```
  With fields: `verifiedPeerPid`, `claimedName`, `preview`, `previewSummary`, `dialogBody`

**`crossSessionInbound` setting** (3 values):
- `"accept"` — auto-deliver incoming messages
- `"hold"` — queue for manual approval
- Off/absent — refuse incoming messages

**Managed policy override** (binary ~314408):
```
Your organization's managed settings set "crossSessionInbound" to "hold";
your own "accept" cannot override managed policy
```

**Permission mode mismatch** (binary ~314412):
```
The sending session's permission mode class doesn't match this session's.
Review it below, or set "crossSessionInbound" to "accept".
```

**Delivery failure modes**:
| Failure | Message |
|---|---|
| Recipient refuses | `that session is not accepting cross-session messages (the feature is off there, or a setting or policy there refuses them); it was not delivered.` |
| Permission mismatch | `The sending session's permission mode class doesn't match this session's.` |
| Unknown sender | `an unidentified session` |
| Expired without approval | `expired without approval` |
| Dropped | `dropped at that session's inbox and NOT delivered` |

**Delivery notice string**:
```
[Cross-session delivery notice] 'message' held for the recipient user's approval.
Not delivered to that session's Claude yet; its user must approve first.
Do not wait for a reply; continue, or choose another approach.
```

**Session name resolution** (binary ~512861):
```
Session names are self-chosen and unverified, so confirm with the user which one
they mean (describe them by where they run, as listed) before messaging;
then use ${t} with that session's exact "name [ref]" token as to:.
Do not guess between them.
```

### 2.2 Teammate System (In-Process Multi-Agent)

Claude Code has a full teammate orchestration system for running multiple concurrent Claude sessions on the same machine.

**Key functions** (from binary ~299648–303524):
- `spawnInProcessTeammate`, `startInProcessTeammate`, `spawnTeammate`
- `getTeammateColor`, `getTeammateContext`, `isTeammate`, `isInProcessTeammate`
- `hasActiveInProcessTeammates`, `waitForTeammatesToBecomeIdle`
- `captureTeammateModeSnapshotIfEnabled`, `syncTeammateMode`
- `removeTeammateFromTeamFile`, `teammateIdentities`
- `getTeammateAgentId`, `setTeammateAgentId`

**Teammate message schemas** (binary ~302940):
```
PermissionResponseMessageSchema
PlanApprovalRequestMessageSchema
PlanApprovalResponseMessageSchema
ShutdownApprovedMessageSchema
ShutdownRejectedMessageSchema
ShutdownRequestMessageSchema
TaskCompletedMessageSchema
TeammateTerminatedMessageSchema
```

**Structured frame protocol** (`STRUCTURED_FRAME_RECEIVE_SPECS` at binary ~302943):
- `isStructuredProtocolMessage` — type guard for inbound frames
- `isModeSetRequest`, `isPermissionRequest`, `isPermissionResponse`
- `isPlanApprovalRequest`, `isPlanApprovalResponse`
- `isSandboxPermissionRequest`, `isSandboxPermissionResponse`
- `isShutdownRequest`, `isShutdownApproved`
- `isTaskAssignment`, `isTeamPermissionUpdate`
- `isTeammateWakeupPrompt`, `isIdleNotification`
- `clearMailbox`, `readMailbox`, `readUnreadMessages`, `writeToMailbox`
- `markMessagesAsRead`, `flushPendingMailboxPrunes`
- `createIdleNotification`, `createPermissionResponseMessage`

**Mailbox path**: `getInboxPath` resolves to `.claude/mailbox/` under the session's project directory (binary ~405788):
```
[TeammateMailbox] getInboxPath: agent=<id>, team=<team>, fullPath=<resolved>
```

**InboxPoller** (binary ~317495–319239):
- Polls for new messages in the teammate's inbox
- On idle: `InboxPoller] Session idle, delivering`
- Handles plan approval responses from team lead
- Cleans up removed entries: `[InboxPoller] Removed`
- Drops schema-invalid entries: `[TeammateMailbox] dropping schema-invalid inbox entry`

### 2.3 Task/Subagent System

**Tool**: `Task` (display name `Agent` in some contexts) — binary constant `Vh="Task"`, `mt="Agent"`.

**Description** (binary ~409990): "Launch a new agent to handle complex, multi-step tasks"

**Parameters** (from binary ~506393 and tool description at ~510540):
| Parameter | Type | Description |
|---|---|---|
| `subagent_type` | string | Agent type: `"fork"` (inherits context) or any defined type (fresh context) |
| `description` | string | Short (3-5 word) task description |
| `prompt` | string | Complete, self-contained task instruction |
| `run_in_background` | boolean | Background (default) or synchronous |
| `name` | string | Optional agent name for addressing |
| `model` | string | Override model for this spawn |
| `isolation` | `"worktree"` or `"remote"` | Isolated git worktree or cloud environment |
| `outputSchema` | object | JSON Schema for StructuredOutput |
| `disallowedTools` | string[] | Tools to deny (supports `mcp__*` wildcard) |
| `bashCommandClamp` | string[] | Bash command prefix restrictions |

**Subagent types**:
- `"fork"` (builtin, key `a6="fork"`, URI `Inr="agent:builtin:fork"`): Inherits full parent conversation context; runs on parent's model; `model` override ignored.
- `"Explore"`, `"Plan"` (built-in agent types, stored in `c4t=new Set(["Explore","Plan"])`): Fresh context; read-only restrictions.
- Custom types via `.claude/agents/*.md` frontmatter definitions.

**Lifecycle**:
1. Parent calls Task tool → subagent spawns
2. Subagent runs in fresh (or forked) context
3. On completion: `TaskCompletedMessageSchema` notification to parent
4. On stall: `[stall] agent "<id>" (retry N on the last attempt-...)` with `stallMs` timer
5. On resume from previous session: `" had no completion record after the previous Claude Code process exited, and was automatically restarted from its saved transcript."`

**Persistence** (on-disk):
```
~/.claude/projects/<project>/<session>/subagents/agent-<id>.jsonl   # transcript
~/.claude/projects/<project>/<session>/subagents/agent-<id>.meta.json  # metadata
```
Meta shape:
```json
{
  "agentType": "claude",
  "description": "Research zcode (closed-source) traces",
  "toolUseId": "call_0ab5eb44d20f4dc395b9423a",
  "spawnDepth": 1,
  "stoppedByUser": true
}
```

**Concurrency & limits** (binary ~510583):
- `spawned`, `spawned_by_subagents`, `completed`, `failed`
- `killed.parent`, `killed.user`, `killed.system`
- `refused.depth_limit`, `refused.concurrency_limit`, `refused.budget`
- `max_depth` tracking

**StructuredOutput for schema-enforced results** (binary ~317443):
- When `outputSchema` is provided, subagent must call `StructuredOutput` tool before completing
- Validation loop: up to N retries; on failure: `"the cloud agent called StructuredOutput but no attempt produced a surviving valid output"`
- On nudge failure: `"subagent completed without calling StructuredOutput (after in-conversation nudge)"`

**Background auto-resume** (binary ~320748):
- On process exit, orphaned agents are tracked
- On next session start: `"had no completion record after the previous Claude Code process exited"`
- Resume via SendMessage or automatic disk-resumable path

### 2.4 Hooks System (Event-Driven Communication)

**Live hooks in `~/.claude/settings.json`** (verified):
```
SessionStart:    herdr-agent-state.sh, orca-hook, startup hook
```

**Event taxonomy** (from binary ~300616–302948 and ~327770):
| Event | Description |
|---|---|
| `SessionStart` | Fires on session creation/resume; types: `startup`, `command`, `matcher` |
| `PreToolUse` | Before tool execution; can block via exit code |
| `PostToolUse` | After tool execution; can modify output |
| `SubagentStart` | Before subagent launch; hook runs as attachment in JSONL |
| `TaskCompleted` | After subagent finishes; `getTaskCompletedHookMessage()` |
| `TeammateIdle` | When a teammate becomes idle |
| `FileChanged` | On watched file change (paths from config) |
| `UserPromptSubmit` | Before prompt is submitted to model |
| `UserPromptExpansion` | During prompt expansion phase |
| `Stop` | On session end; `stop_hook_summary` system entry with `hookCount` |

**Hook JSONL attachment format** (from live JSONL):
```json
{
  "parentUuid": "<turn-uuid>",
  "attachment": {
    "type": "hook_success",
    "hookName": "SessionStart:startup",
    "toolUseID": "<uuid>",
    "hookEvent": "SessionStart",
    "content": "",
    "stdout": "{}\n",
    "stderr": "",
    "exitCode": 0,
    "command": "if [ -z \"${HOME-}\" ]; ..."
  },
  "type": "attachment",
  "uuid": "<uuid>",
  "timestamp": "<ISO8601>"
}
```

### 2.5 Steering Interrupts

Binary evidence (offset ~306679):
- `lastInterruptedAssistantAPIMessageId` — tracks the message that was interrupted
- `interrupted_message_id` — in event schema
- `last_interrupted_assistant_api_message_id` — telemetry key
- `sendInterrupt` — function to inject a mid-turn user message
- Admin steering via `captureAdmin3PSteeringSnapshot` / `getAdmin3PSteeringSnapshot`

This confirms Claude Code supports mid-turn message injection (steering) from external sources (IDE, Claude.ai web, admin policies), with the interrupted message tracked for consistency.

### 2.6 Session Forking

CLI flag `--fork-session` (binary ~305320):
- Creates a copy of another session's transcript as the starting point
- `forkSessionId`, `adoptForkSessionMetadata`, `buildForkAdoptionMeta` functions found
- Constraint: `--session-id` with `--continue`/`--resume` requires `--fork-session`
- Worktree isolation returns path + branch info

**Warning** (binary ~317529):
```
This conversation was forked from a session that is still working in this checkout (F...)
```

### 2.7 Communication Mechanism Matrix

| Mechanism | Trigger | Payload | Direction | Delivery | Failure Mode |
|---|---|---|---|---|---|
| **SendMessage tool** | Model tool call | `{to, summary, message, notify_when_idle}` | Inter-session (same machine) | Mailbox → InboxPoller → system turn | Refused/expired/dropped/mode-mismatch |
| **Teammate mailboxes** | In-process teammate spawn | Structured frames (schema-validated) | Team lead ↔ teammates | `clearMailbox`/`readMailbox`/`writeToMailbox` | Schema-invalid entry dropped |
| **Task/subagent** | Model tool call | `subagent_type, description, prompt, outputSchema` | Parent → child → parent (yield) | Background notification; disk-resumable | Stall retry; orphan auto-resume; StructuredOutput validation loop |
| **Hooks** | Lifecycle event | JSON in → JSON out (stdin/stdout) | Host ↔ Claude | Synchronous (with timeout) | Timeout; hook failure blocks tool |
| **Steering interrupts** | External (IDE/admin) | Injected user message | External → session | Mid-turn injection | Interrupted message ID tracked |
| **Session fork** | `--fork-session` CLI | Full transcript copy | CLI → new session | Transcript adopted | Conflict warning if source still running |

---

## 3. System Prompt Assembly

### 3.1 Assembly Order

Evidence from binary string extraction and prior `cc-mainprompt.txt`:

1. **Identity header** — Claude Code version, commercial terms disclaimer, copyright
2. **Tool description blocks** — each tool gets a template-generated description with dynamic values (current date, session model, available agent types)
3. **Permission context** — current permission mode, bypass status
4. **Environment context** — working directory, git branch, CLAUDE.md content
5. **Skill/tool listing** — `skill_listing` attachment injected with available skills
6. **MCP instructions delta** — `mcp_instructions_delta` attachment for MCP tool context
7. **Agent listing** — `agent_listing_delta` attachment listing available subagent types
8. **Task reminders** — `task_reminder` attachments (active todo items)
9. **Token budget** — `total_tokens_reminder` injection: `<total_tokens>15000000 tokens left</total_tokens>` (verified from live JSONL)
10. **System-reminder tags** — injected dynamically for agent type availability, model info, etc.

### 3.2 Distinctive Prompt Sections

**Task tool description** (binary ~510540, full prompt):
```
**Do not spawn agents unless the user asks.** Each spawn starts cold and re-derives
context you already have — it's the expensive path on this plan. A task with "multiple
angles," "thorough," or several parts is not a request to spawn; handle it inline with
your own tools. Only use this tool when the user explicitly says to use a subagent,
or names one of the available agent types.
```

**Grep tool description** (binary ~507577):
```
A powerful search tool built on ripgrep.
Usage:
- ALWAYS use Grep for search tasks. NEVER invoke `grep` or `rg` as a Bash command.
  The Grep tool has been optimized for correct permissions and access.
- Supports full regex syntax (e.g., "log.*Error", "function\s+\w+")
```

**WebFetch tool description** (binary ~507595):
```
IMPORTANT: WebFetch WILL FAIL for authenticated or private URLs. Before using this tool,
check if the URL points to an authenticated service (e.g. Google Docs, Confluence, Jira, GitHub).
If so, look for a specialized MCP tool that provides authenticated access.
```

**Bash tool** (binary ~396300):
```
- Foreground `sleep` is blocked; use Monitor with an until-loop to wait on a condition.
- Commands are cheap to run and their errors are informative: run the straightforward
  command rather than perfecting it mentally first, and adjust from what it prints.
```

**Edit tool** (binary ~316242):
Parameters: `file_path`, `edits` (array of `{old_string, new_string, replace_all}`)

**Status line prompt** (binary ~310980):
```
You write the terminal status line for an AI coding agent, in its voice, from a digest
of its current turn.
Write the status line the user glances at while they wait: ONE line, at most 14 words,
no label, no markdown, nothing else.
```

### 3.3 Prompt Size

Prior docs noted CC's system prompt is substantially larger than pi/omp's minimal approach. The full assembled prompt (identity + tools + skills + agents + MCP + environment + token budget) is estimated at ~10K–15K tokens based on the number of tool descriptions and injected context. CC explicitly communicates context budget to the model: `CLAUDE_CODE_MAX_CONTEXT_TOKENS` default (this machine: 998000 via env override).

---

## 4. Built-in Tool Inventory

All 19+ built-in tools, extracted from binary constants and string analysis:

| Tool Name | Binary Constant | Key Behavior |
|---|---|---|
| **Bash** | `qe="Bash"` | Shell execution; `run_in_background`, `timeout` (ms); foreground `sleep` blocked; `&` not needed with `run_in_background` |
| **Read** | `rqo="Read"` | File reading; structural summary for code; `[path#TAG]` anchors for edits; must read before edit |
| **Write** | `Mn="Write"` | File creation/overwrite; fails if existing file not read first (outside working dir) |
| **Edit** | `Bt="Edit"` | Surgical edits via `old_string`/`new_string` anchors; `replace_all` flag; file-freshness check ("File has been modified since read"); must read file first |
| **Glob** | `co="Glob"` | File/directory pattern matching |
| **Grep** | `ro="Grep"` | Ripgrep-based search; `content`/`files_with_matches`/`count` output modes; `multiline` support |
| **Task** | `Vh="Task"` / `mt="Agent"` | Subagent spawning; `fork` type inherits context; `outputSchema` for structured results; `disallowedTools`; `isolation: "worktree"` or `"remote"` |
| **WebFetch** | `Cr="WebFetch"` | URL fetch → markdown → LLM summarization; 15-min cache; fails on auth URLs; follows redirects across hosts |
| **WebSearch** | `_D="WebSearch"` | Web search with current year enforcement |
| **TodoWrite** | `XS="TodoWrite"` | Structured task list management; rendered in TUI as checklist |
| **AskUserQuestion** | `Es="AskUserQuestion"` | Structured mid-task clarification; multiple-choice options; max 12 options; `preview` field for artifacts |
| **EnterPlanMode** | `GE="EnterPlanMode"` | Switches to read-only exploration mode; presents plan for approval; `ExitPlanMode` to exit |
| **Monitor** | `ia="Monitor"` | Stream events from background process; each stdout line is a notification |
| **BashOutput** | (unnamed) | Read output from backgrounded Bash process |
| **KillShell** | (unnamed) | Terminate backgrounded shell process |
| **NotebookEdit** | (unnamed) | Jupyter notebook cell editing; `edit_mode`: insert/delete/replace; `cell_id`, `new_source`, `cell_type` |
| **Workflow** | `Yc="Workflow"` | Dynamic workflow execution; script-based agent orchestration |
| **Skill** | (unnamed) | Dynamic skill loading; `buildSkillTools` at runtime; frontmatter-defined |
| **StructuredOutput** | (unnamed) | Schema-enforced JSON output for subagents; validation loop on failure |

**Notable tool behaviors**:
- **File-freshness check**: Edit tool detects `"File has been modified since read"` and rejects the edit; error category is `"File Changed"` (binary ~518797)
- **Backgrounded Bash**: `run_in_background: true` detaches; process tracked; auto-resumes on session restart; orphan notification on resume
- **Edit anchors**: Exact string match with `old_string`/`new_string`; whitespace normalization for some languages
- **MCP tool integration**: Built-in tools get `mcp__` prefix for MCP-sourced tools; `disallowedTools` supports `mcp__<server>` and `mcp__<server>__<tool>` patterns

---

## 5. Filesystem Layout (Live State)

```
~/.claude/
├── projects/<escaped-path>/<session-uuid>.jsonl          # session transcript
├── projects/<escaped-path>/<session-uuid>/subagents/     # subagent transcripts
│   ├── agent-<id>.jsonl                                   # subagent transcript
│   └── agent-<id>.meta.json                               # {agentType, description, spawnDepth, stoppedByUser}
├── projects/<escaped-path>/memory/                        # CLAUDE.md / memory files
├── session-env/<session-uuid>/                            # session environment scripts (1599 entries)
├── tasks/<session-uuid>/                                  # TodoWrite task state
│   ├── 1.json, 2.json, 3.json...                         # {id, subject, description, status, blocks, blockedBy}
├── jobs/<job-id>/                                         # background job state
│   ├── state.json                                         # {state, detail, tempo, fan[], tokens, output, children}
│   └── timeline.jsonl                                     # state transitions
├── teams/                                                 # teammate team files
├── plans/                                                 # plan mode artifacts
├── hooks/                                                 # hook scripts (cbm-code-discovery-gate, cbm-session-reminder, etc.)
├── file-history/                                          # checkpoints for Esc-Esc rewind (149 entries)
├── shell-snapshots/                                       # shell state snapshots
├── paste-cache/                                           # clipboard paste cache
├── settings.json                                          # global config (env, permissions, hooks, statusLine, theme)
├── history.jsonl                                          # global prompt history (3578 lines)
├── plugins/                                               # installed plugins
├── jobs/pins.json                                         # pinned background jobs
├── daemon/                                                # daemon process state
├── telemetry/                                             # telemetry data
├── backups/                                               # settings backups
├── cache/                                                 # general cache
├── downloads/                                             # downloaded files
├── ide/                                                   # IDE integration state
├── backups/                                               # backup of settings
└── statusline.sh                                          # status line script (15986 bytes)
```

---

## 6. xdev Impact

### M0 (Skeleton)
- **Adopt**: Session JSONL format is well-documented. xdev should use a similar append-only JSONL with typed entries (`type` discriminator). Include `last-prompt`, `mode`, `permission-mode` header entries. Include `cost-state` entries for budget tracking.
- **Reject**: Do NOT adopt the dual-bundle binary approach; xdev is a Go binary with clean separation.

### M1 (Providers)
- **Adopt**: `generate_session_title` pattern — a dedicated small-model call with JSON schema output for session naming. Cheap, high-UX value. Implement as a hook on first few turns.
- **Adopt**: Structured output schema pattern (`outputFormat: { type: "json_schema", schema: {...} }`) for tool-call validation.

### M2 (Session Core)
- **Adopt**: Title resolution cascade (custom > ai-generated > first-prompt fallback). Include `custom-title.json` on-disk artifact for quick title lookup without scanning JSONL.
- **Adopt**: `--resume` / `--continue` / `--fork-session` session picker with filters (repo, branch, worktree).
- **Adopt**: Session metadata re-append on every turn for crash resilience (title, branch, cost-state).

### M3 (Agent Loop)
- **Adopt**: Permission-gated tool execution with `PreToolUse` hooks that can block via exit code.
- **Adopt**: File-freshness check before writes ("modified since read").
- **Adopt**: `run_in_background` pattern for detached Bash commands with orphan tracking.

### M4 (TUI)
- **Adopt**: Resume picker UI with title display, filters (repo/branch/worktree), search.
- **Adopt**: TodoWrite task list rendered as interactive checklist in TUI.
- **Adopt**: Status line prompt pattern — one-line voice-of-agent status update.
- **Verify**: `AskUserQuestion` structured clarification — may not be needed if xdev keeps questions in prose (simpler).

### M5 (Compaction)
- **No change**: Prior compaction analysis stands.

### M6 (RPC + Subagents + MCP)
- **Adopt**: Task/subagent system with `fork` (context-inheriting) and fresh agent types. Structured output via `outputSchema` + `StructuredOutput` tool. Stall detection with retry.
- **Adopt**: Subagent persistence as JSONL + meta.json for cross-session resume.
- **Adopt**: Concurrency tracking (spawned, depth_limit, concurrency_limit, budget).
- **Verify**: `disallowedTools` deny-list pattern — valuable for safety but complex to implement in xdev's simpler model.

### M7 (Ext Protocol)
- **No change**: MCP extension pattern stands.

### M8 (Memory Audit)
- **No change**: CLAUDE.md / memory pattern stands.

### M9 (Model Roles/Auth/Config)
- **Adopt**: Per-agent-type model/effort/tool-access configuration via frontmatter (`.claude/agents/*.md`).
- **Adopt**: `CLAUDE_CODE_SUBAGENT_MODEL_FORCE` override pattern.

### M10 (Session UX)
- **Adopt**: Session rename via `/rename` with JSONL persistence.
- **Adopt**: Session metadata in resume picker: title, branch, cwd, last-modified, size.
- **Adopt**: `atis-latch` mechanism (verified in JSONL: `{"type":"atis-latch","atis":"","sessionId":"..."}`) — appears to be a state-latch for assistant-turn-initiated-session tracking.

### M11 (Agent System)
- **Adopt**: In-process teammate system with mailbox-based message passing. Key pattern: lead-coordinator + worker teammates communicating via structured frames.
- **Adopt**: Plan approval flow: lead proposes plan → teammates respond with approval/rejection → lead proceeds or adjusts.
- **Adopt**: Shutdown request/response protocol for clean teammate termination.
- **Verify**: Full structured frame protocol is complex; xdev may want a simpler message-passing model.

### M12 (Knowledge/Chrome)
- **No change**: Existing knowledge analysis stands.

### M13 (Extended Tools)
- **Adopt**: Skill tool with dynamic loading from frontmatter-defined skills (`buildSkillTools`).
- **Adopt**: Workflow tool for script-based agent orchestration.

### M14 (v2 Modes)
- **Adopt**: Plan mode (`EnterPlanMode` / `ExitPlanMode`) as first-class mode with read-only enforcement.
- **Adopt**: Hook events as communication surface (SessionStart, SubagentStart, TaskCompleted, FileChanged).
- **Adopt**: Steering interrupt mechanism for external tool integration.

---

## 7. Issues to update

### #1 M0 skeleton
- **Add**: "Session JSONL format includes typed entries with `type` discriminator; include `last-prompt`, `mode`, `permission-mode`, `cost-state`, `ai-title`, `custom-title` entry types for session metadata persistence."

### #2 M1 providers
- **Add**: "Implement `generate_session_title` hook: dedicated small-model call with JSON schema output for session naming after first few user turns; store as `ai-title` JSONL entry."

### #3 M2 session core
- **Add**: "Session title resolution cascade: custom-title (user-set via `/rename`) > ai-title (auto-generated) > first-user-prompt fallback. Persist `custom-title.json` on-disk artifact for quick lookup."
- **Add**: "Session metadata re-append on every turn: title, git branch, cost-state, version. Crash-resilient metadata recovery."

### #4 M3 agent loop + 4 tools
- **Add**: "File-freshness check before Edit/Write: detect if file was modified since last read and reject the operation with 'File Changed' error."
- **Add**: "Backgrounded Bash commands: `run_in_background` detaches process; tracked across turns; orphan notification on resume; `BashOutput` and `KillShell` tools for interaction."

### #5 M4 TUI
- **Add**: "Resume picker with session title display, filter modes (repo/branch/worktree), and search. Show `firstPrompt` preview alongside title."
- **Add**: "Status line: one-line agent-voice status update, maximum 14 words, rendered in terminal title bar."

### #6 M5 compaction + retry
- **No additions needed.**

### #7 M6 RPC + subagents + MCP
- **Add**: "Task/subagent system: `fork` type inherits parent context; fresh types start clean. `outputSchema` for structured results via `StructuredOutput` tool with validation loop. Concurrency limits: depth, width, budget."
- **Add**: "Subagent persistence: JSONL transcript + meta.json on disk; auto-resume from saved transcript on process restart."
- **Add**: "Stall detection: configurable timeout per subagent; retry with escalation; user-abort support."

### #8 M7 ext protocol
- **No additions needed.**

### #9 M8 memory audit
- **No additions needed.**

### #10 M10 model roles / auth / config
- **Add**: "Per-agent-type configuration via frontmatter: model, reasoning effort, tool access, `disallowedTools` deny-list. `CLAUDE_CODE_SUBAGENT_MODEL_FORCE` env override for global model forcing."
- **Add**: "Hook event taxonomy: SessionStart, PreToolUse, PostToolUse, SubagentStart, TaskCompleted, FileChanged, UserPromptSubmit, Stop. Each fires shell command with JSON stdin; PreToolUse can block via exit code."

### #11 M11 agent system
- **Add**: "In-process teammate system: lead-coordinator + worker teammates. Mailbox-based message passing via `.claude/mailbox/`. Structured frame protocol with schema-validated message types: TaskCompleted, PermissionRequest/Response, PlanApprovalRequest/Response, ShutdownRequest/Approved/Rejected."
- **Add**: "Cross-session messaging: `SendMessage` tool for inter-session communication on same machine. `crossSessionInbound` setting (accept/hold/off). Permission-mode mismatch handling. Delivery guarantees: held-for-approval, expired, dropped, delivered."
- **Add**: "InboxPoller: polls teammate inboxes for unread messages; delivers on session idle; handles plan approval responses from team lead."

### #12 M12 knowledge / chrome
- **No additions needed.**

### #13 M13 extended tools
- **Add**: "Skill tool: dynamic skill loading from frontmatter-defined `.claude/skills/` directory; `buildSkillTools` at runtime; `refreshMcpTools` for re-scan after hook installation."
- **Add**: "Workflow tool: script-based agent orchestration with resume support (`resumeFromRunId`)."

### #14 M14 v2 modes
- **Add**: "Plan mode: `EnterPlanMode` switches to read-only exploration; `ExitPlanMode` returns to normal after user approval. Enforced via permission system."
- **Add**: "Steering interrupts: external tools (IDE, Claude.ai web, admin policies) can inject mid-turn user messages; interrupted assistant message ID tracked for consistency."
- **Add**: "Session forking: `--fork-session` CLI flag copies another session's transcript as starting point; worktree isolation returns path + branch."
