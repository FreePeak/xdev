# Chrome auto-connect (personal profile, no extension)

**Status:** accepted · **Issue:** browser auto-connect parity with chrome-devtools-mcp

## Context

xdev already had two ways to drive the user's real Chrome profile:

1. `xdev chrome install` + the MV3 extension bridge (CDP at `127.0.0.1:9224`).
2. `--remote-debugging-port` on a non-default `--user-data-dir`.

Chrome 144+ adds a third, lighter path: the user enables remote debugging in
`chrome://inspect/#remote-debugging`, and Chrome writes a `DevToolsActivePort`
file into the default profile and starts its own DevTools HTTP server. That is
what chrome-devtools-mcp's `--autoConnect` uses, and it needs no extension and
no restart of Chrome.

## Decision

Add `browser.autoConnect` (opt-in, default off) and `browser.chromeProfileDir`
(config key for a non-default profile). When on and no explicit endpoint is
given, the `browser` tool reads `<profile>/DevToolsActivePort`, verifies the
port with `/json/version`, and uses `http://127.0.0.1:<port>` as its discovery
base. Everything downstream (target list, page websocket, tool ops) is
unchanged because the same DevTools HTTP handler writes that file.

`xdev chrome connect` writes `browser.autoConnect: true` to the global config,
probes the profile, and prints the exact next step when Chrome is not ready;
`xdev chrome disconnect` removes the key; `xdev chrome status` reports it.
The profile directory is validated at probe time, not at config load, so a
custom dir can be set before Chrome exists.

Safety properties:

- Auto-connect is never the default. The default profile carries the user's
  logins and cookies; attaching to it is a deliberate opt-in, and Chrome still
  shows a per-connection permission prompt.
- When auto-connect is on, xdev **does not** launch its own headless browser
  on failure. A silent fallback to a logged-out private profile would answer
  the model with a different browser than the one it was promised.
- Auto-connect only contributes when no `browser.cdpUrl` and no
  `$XDEV_BROWSER_CDP_URL` is set. An explicit endpoint is a deliberate choice
  and is never silently replaced.
- A stale `DevToolsActivePort` (Chrome crashed) is detected by the
  `/json/version` probe and reported as stale, not used.

## Consequences

- macOS TCC may block reading `~/Library/Application Support/Google/Chrome`;
  the probe surfaces that as a permission error with the exact System Settings
  path, mirroring chrome-devtools-mcp's troubleshooting.
- macOS/Linux/Windows profile locations are covered, plus Chrome, Chromium,
  Brave and Edge data dirs.
- The extension bridge and `--remote-debugging-port` paths are untouched and
  remain the fallbacks.
