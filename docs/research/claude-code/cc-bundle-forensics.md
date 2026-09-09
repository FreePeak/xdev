# Claude Code 2.1.263 — local install forensics (bundle, prompts, session JSONL)

> Scout-extracted ground truth for the xdev Claude Code cross-check. Verbatim prompt extracts live in this directory (cc-mainprompt.txt, cc-compact.txt, cc-enterplan.txt, cc-gitstyle.txt, cc-explore.txt, cc-envvars.txt).

## Summary
Claude Code 2.1.263 is a 190MB native Mach-O arm64 Bun-compiled bundle; system prompts, tool schemas, env surface, and session JSONL format extracted verbatim via byte-offset strings on the binary plus live ~/.claude state. Full report with adopt/verify/reject verdicts mapped to xdev M0-M14 below.

## Architecture
Single self-contained Bun-compiled Mach-O binary containing the JS bundle (chunk-*.js under /$bunfs/root), shipped at ~/.local/share/claude/versions/<ver> with symlink shim; state in ~/.claude (projects JSONL transcripts, file-history checkpoints, shell-snapshots, plugins, skills, agents, hooks, plans, daemon). Prompts are template functions in the bundle: identity + task rules + tool descriptions assembled per session; two embedded copies of the bundle in the binary (70-77MB, 159-170MB).

## Evidence artifacts

- `/Users/linh.doan/.local/share/claude/versions/2.1.263` — Real claude entry: 190MB Mach-O arm64 Bun-compiled binary; prompt text at byte offsets ~70.3-77.0MB and 159-170MB (two embedded bundle copies)
- `/Users/linh.doan/.claude/settings.json` — Live settings: env{ANTHROPIC_*}, permissions.defaultMode, hooks, statusLine, theme, enabledPlugins, extraKnownMarketplaces
- `/Users/linh.doan/.claude/projects/-Users-linh-doan-work-harvey-freepeak-onegw/5331a24c-a528-4336-8c92-46d835d8ed3b.jsonl` — Real session transcript documenting JSONL schema: last-prompt/mode/permission-mode/atis-latch headers; user/assistant entries with parentUuid,isSidechain; attachment and reminder records
- `/tmp/cc-mainprompt.txt` — Scratch extract: identity header, Read/Grep/WebSearch/WebFetch/Write/AskUserQuestion tool descriptions, MCP-over-REPL notes
- `/tmp/cc-compact.txt` — Scratch extract: verbatim 9-section compaction summary prompt with analysis wrapper
- `/tmp/cc-enterplan.txt` — Scratch extract: EnterPlanMode description with 7 use-conditions and when-NOT-to-use list
- `/tmp/cc-gitstyle.txt` — Scratch extract: git commit/PR workflow prompts and lean tool-routing prompt (embedded JS source)
- `/tmp/cc-explore.txt` — Scratch extract: Explore (file search specialist) and Plan (architect) subagent prompts with READ-ONLY prohibitions
- `/tmp/cc-envvars.txt` — Scratch extract: census of 600+ distinct CLAUDE_CODE_*/CLAUDE_* env var names