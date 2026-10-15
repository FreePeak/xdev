# Chrome extension bridge (personal-profile browser control)

**Status:** accepted (2026-04-15) · **Issue:** #112

## Context

xdev already had a CDP `browser` tool (attach / autolaunch private profile) and
a `browser-relay` daemon for token-gated CDP upstreams. Neither could drive the
user's **existing logged-in Chrome** without starting Chrome with
`--remote-debugging-port` (and, on Chrome 136+, a non-default user-data-dir).

Claude Code solves this with "Claude in Chrome": an MV3 extension + native
messaging host. The agent speaks high-level MCP tools; the extension uses
`chrome.debugger`.

## Decision

Ship a first-party MV3 extension (`extensions/chrome`) and a Go native host
(`internal/chromeext`) that:

1. Speaks Chrome native messaging (`com.freepeak.xdev_browser`) with the extension.
2. Exposes a **CDP-compatible** loopback face on `127.0.0.1:9224` (`/json/list` +
   `/devtools/page/<tabId>`).
3. Leaves the existing `browser` tool ops unchanged — it just attaches to 9224
   when the bridge is up (prefer over 9222 when nothing is configured).

Install path: `xdev chrome install` copies the extension under the data dir,
writes the host wrapper (`xdev --chrome-native-host`), and registers the native
messaging manifest for Chromium browsers.

## Non-goals

- No Chrome Web Store publish in v1 (unpacked load).
- No reimplementation of Claude's MCP tool surface (`tabs_context_mcp`, etc.);
  xdev keeps its smaller CDP op set.
- No pairing crypto / multi-device bridge (loopback only).

## Consequences

- Users must Load unpacked once; extension id is stable via the public `key` in
  `manifest.json`.
- The private key used to mint that id is **not** in the repo (public SPKI only).
- `browser-relay` remains for raw upstream CDP; the extension path is the
  personal-profile path.
