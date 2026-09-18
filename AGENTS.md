# xdev — Repository conventions

## No co-authored-by trailers from other AI agents

**Never attach a `Co-Authored-By:` trailer naming another AI model
(Claude, Claude Code, Claude Opus, Copilot, Grok, Gemini, …) to a commit
in this repo.**

The repo's author is `linhdmn <mnhatlinh.doan@gmail.com>` and that is the
only name that may appear in commit metadata. An AI agent that helped
writes code; it does not co-author the commit. The commit is a human's
commit, authored by the human, full stop.

### What this means in practice

- When finishing a commit, strip any `Co-Authored-By:` trailer that a
  harness or tooling added — including ones that arrived from a previous
  session. A commit with a Claude trailer is a broken commit, not a
  courtesy.
- If a commit already in the history carries one, rewrite it away with
  `git filter-repo --message-callback` (never `--no-verify`, never
  hand-edit). Match `^Co-Authored-By:\s+Claude( Code| Opus 4\.5)?\s+`
  and drop the line, then re-trailing-newline the message.
- This is a standing convention, not a one-time cleanup. It applies to
  every commit, in every branch, forever.

### Why

Git history is the record of who did the work. An AI model did not do the
work — it produced a diff under the human's direction. Letting a model's
name sit in the author/trailer chain misattributes ownership, pollutes
`git blame`, and makes the history harder to reason about later. The
human is the author; the tool is a tool.