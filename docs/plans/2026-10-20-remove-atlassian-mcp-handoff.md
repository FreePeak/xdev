# Handoff — remove Atlassian MCP server from xdev

Date: 2026-10-20
Repo: /Users/linh.doan/work/harvey/freepeak/xdev (main checkout, read-only per rules)

## What was done

Removed both Atlassian MCP server entries (`atlassian` and `atlassian-mcp`) from the user-level xdev config:

- File: `~/.xdev/agent/mcp.yml` — deleted the two server blocks (Jira + Confluence via uvx mcp-atlassian, and the remote Atlassian MCP). File no longer contains "atlassian".

## Remaining references (intentional, test data only)

- `internal/tui/mcps_test.go` (lines ~107, 114, 128) — fixtures naming "atlassian" server.
- `internal/mcpclient/config_test.go` (lines ~279, 281, 295) — fixtures.

These are test fixtures, not production code; no change needed. They will still pass since they construct their own in-memory config.

## Verification

- `grep -n "atlassian" ~/.xdev/agent/mcp.yml` → no matches.
- No production Go code references atlassian.

## Next step

Restart xdev so the MCP client picks up the new config (servers removed from the registry). No commit/PR needed — this is a user-config change outside the repo.
