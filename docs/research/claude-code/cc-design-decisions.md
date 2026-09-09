## Claude Code DESIGN DECISIONS vs pi/omp minimalism → verdicts for xdev

Format: Decision → What CC does → Evidence → Verdict for xdev (milestone).
Evidence classes: [A] anthropic.com engineering blog, [G] github.com/community system-prompt teardowns (anthropics/claude-code CHANGELOG is primary where cited).

1. TodoWrite as a first-class TOOL (not a TODO.md file)
CC exposes TodoWrite/TodoRead as model-callable tools; the model maintains a structured task list in-conversation, rendered by the TUI. pi/omp instead write a file-based plan/TODO.md. CC's approach gives the model explicit state-update discipline and the UI a live checklist; the file approach is more inspectable/versionable.
Evidence: anthropics/claude-code CHANGELOG (TodoWrite improvements, v1.0.x); community teardowns (github.com/asgeirtj/system_prompts_leaks — Claude Code system prompt mandates TodoWrite usage before complex tasks).
Verdict: VERIFY for xdev (M5/M7). omp already has TODO.md; file-based keeps xdev boring and grep-able. Consider an optional structured tool only if the file approach shows state-drift in practice.

2. Plan Mode + ExitPlanMode approval gate
CC has a distinct plan mode: read-only exploration, then a presented plan, then ExitPlanMode asks the user to approve before any write/execute. It is product surface (Shift+Tab cycles modes).
Evidence: Claude Code docs 'plan mode' (code.claude.com/docs/en/plan-mode); CHANGELOG plan-mode entries.
Verdict: ADOPT (M14 modes or M8 agent loop): read-only flag + one 'approve plan' gate tool. Cheap (permission system already exists), high user-trust value. omp has no equivalent.

3. AskUserQuestion tool (structured mid-task clarification)
CC lets the model block and present multiple-choice options via a tool rather than emitting a question in prose. Deterministic UI, recordable answers.
Evidence: CHANGELOG (AskUserQuestion); system-prompt teardowns ('If you need to ask the user a question, use AskUserQuestion').
Verdict: ADOPT (M7/M9 hub/tui): a single tool that surfaces options in the TUI and injects the answer as tool result. Small surface, big UX win over prose questions.

4. Permission modes as product surface (default / acceptEdits / plan / bypassPermissions)
CC makes permission posture a user-visible, switchable mode, not just config. Tool-use interrupts via canUseTool callback in the SDK.
Evidence: Claude Code docs 'permission modes' + IAM docs; agent-SDK posts.
Verdict: VERIFY (M14): xdev runs YOLO-inside-external-sandbox (like omp). Do NOT rebuild in-process permission UI; instead expose a thin 'mode' concept only if sandbox integrations demand it. Otherwise REJECT as scope creep.

5. Hooks as deterministic guardrails (PreToolUse/PostToolUse/Stop/SessionStart...)
CC's hooks run shell commands at lifecycle events with JSON in/out; they can block tool calls, rewrite behavior. GA'd v1.0.
Evidence: anthropic.com/engineering/claude-code-hooks; docs/en/hooks.
Verdict: ADOPT (M9 hooks): match CC's event taxonomy (PreToolUse blocking via exit code / decision JSON is the key semantic), but keep execution boring (sh -c, stdin JSON, exit-code contract).

6. Subagent context isolation + parallel Task tool
CC's subagents run in fresh context windows, return only final summaries to the parent; multiple Tasks run in parallel. 'How we built our multi-agent research system' documents orchestrator-worker with isolated contexts and structured handoffs as the core lesson.
Evidence: anthropic.com/engineering/multi-agent-research-system.
Verdict: ADOPT (M10 task agents/hub): hard rule — subagent transcript NEVER streams into parent context; only the final yield does. xdev's hub design already implies this; make it a contract test.

7. Auto-compact thresholds + /context microcompact
CC auto-compacts near a context threshold (historically ~92-95%), warns at ~60% ('context low'), and offers /compact + /context inspection; 'microcompact' clears old tool outputs while keeping turns.
Evidence: CHANGELOG auto-compact/microcompact entries; community docs.
Verdict: ADOPT (M6 compaction): two-tier behavior — warning line in status bar, auto-compact at high-water mark, plus tool-output-only eviction (microcompact) before full summarization. omp has compaction but microcompact-as-pre-step is the divergence worth copying.

8. CLAUDE.md loading + /init + @import; AGENTS.md coexistence
CC auto-loads CLAUDE.md hierarchically (enterprise > user > project > local), supports @path imports, and recently added symlink/agentic support for AGENTS.md. /init bootstraps the file.
Evidence: docs/en/memory; CHANGELOG ('Add support for symlinked AGENTS.md', v1.0.60+).
Verdict: ADOPT (M12 memory/skills): load AGENTS.md natively (it's the open standard omp targets) AND honor CLAUDE.md as an alias to stay compatible with the ecosystem. Hierarchical precedence + @import, nothing fancier (no vector memory backends).

9. Checkpoints / Esc-Esc rewind
CC checkpoints conversation+code state each user turn; double-Esc rewinds to any prior turn, optionally restoring files. Reddit/HN: one of the most-loved features.
Evidence: docs/en/checkpointing; CHANGELOG v2.0 checkpoints.
Verdict: VERIFY (post-M14, M13-adjacent): xdev already persists session JSONL (M0) and hashline edits are file ops — rewind = truncate JSONL + revert files. High value but not omp parity; defer behind an explicit decision, design JSONL turn-boundary markers from M0 so it stays cheap.

10. Think-budget keywords ('think' < 'think hard' < 'ultrathink')
CC maps specific phrases to escalating thinking token budgets, taught in system prompt and best-practices blog.
Evidence: anthropic.com/engineering/claude-code-best-practices; teardowns listing the exact keyword ladder.
Verdict: ADOPT (M5 agent loop): trivial to implement, model-visible via system prompt; map keywords to the provider's reasoning budget. Keep the ladder in one constant table.

11. 90%-context budget line in system prompt
CC's prompt reserves ~90% of the window for 'working context', instructing the model to avoid loading unneeded files — context budget is communicated to the model itself.
Evidence: system-prompt teardowns (asgeirtj/system_prompts_leaks — Claude 4.5 Sonnet CC prompt states ~90% budget).
Verdict: VERIFY (M5): the literal number belongs to CC's window size; xdev SHOULD inject a computed budget statement (usable tokens after system+tools) into the system prompt rather than copying '90%'.

12. What CC deliberately LACKS: no LSP, no AST parsing, no persistent memory DB, no background indexing
CC is 'a thin layer over the model': grep/glob/file tools only. Anthropic's stated philosophy: give the model primitives and let it figure things out; the best-practices blog emphasizes agentic search over indexes. This is the same minimalism pi/omp already follow — evidence that xdev's 4-hashline-tool core is the right shape.
Evidence: anthropic.com/engineering/claude-code-best-practices; boris talking-points (community 'Claude Code has no LSP' discussions, e.g. news.ycombinator.com threads on CC internals).
Verdict: REJECT extended infrastructure (LSP/AST/embedding memory) for xdev core; extended tools stay opt-in plugins (M13) exactly as planned. If lean KG-style graph tools are wanted, ship them as user-installed MCP servers, never built-in.

13. MCP as the extension point (but not everything is MCP)
CC ships core tools natively (fast, typed) and uses MCP only for third-party integrations; SDK added tool-use permission callbacks and 'MCP OAuth' over time.
Evidence: docs/en/mcp; CHANGELOG MCP OAuth/tool-permission entries.
Verdict: ADOPT (M13): built-in tools native; extension = MCP client config (.mcp.json) with stdio first. Do not route internal tools through MCP.

14. Known criticisms to avoid: cost/latency of multi-agent fan-out, context rot, prompt-injection surface in parallel research, and over-eager file edits without permission discipline
The multi-agent blog itself cites ~15x token cost for multi-agent research; teardowns note context rot from long sessions (hence compaction) and users complain about destructive edits when permission checks are off.
Evidence: anthropic.com/engineering/multi-agent-research-system (15x token note); community threads (HN 'Claude Code destroyed my repo' class complaints).
Verdict: ADOPT as constraints, not features: (a) cap subagent fan-out depth/width in M10 hub; (b) compaction budget from #7; (c) keep yolo-mode dependent on external sandbox never being bypassed (M3).

---
Summary counts: ADOPT 6 (#2 partial,3,5,7,8,10,13,14-constraints), VERIFY 5 (#1,4,9,11,2), REJECT 2 (#12 LSP/AST/memory-infra; #4 full permission-mode UI if sandbox suffices).
Biggest CC-vs-omp divergences worth carrying into xdev: ExitPlanMode approval gate, AskUserQuestion, microcompact-before-compact, hooks with blocking PreToolUse semantics, subagent-yield-only isolation. Deliberate non-goals (no LSP/AST/memory-DB) confirm omp-shaped minimalism.