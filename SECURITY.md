# Security Policy

xdev is a coding agent that runs arbitrary code by design, on your machine, with
your credentials. That single sentence defines both what makes a real
vulnerability here and what does not.

## Reporting

Open a **private report** — do not file a public issue for anything in scope
below.

- When the repository is public: the **Security** tab → *Report a vulnerability*
  (GitHub private vulnerability reporting), which creates a private advisory
  thread.
- While the repository is private, or if the above is unavailable: open an issue
  titled `security: <short summary>` with **only** the reproduction path and no
  exploit detail, and the maintainer will convert it to a private thread.

You will get a reply from [@FreePeak](https://github.com/FreePeak). Fix
timelines are personal-project honest, not contractual: severity-first, and
you will be told if a report is not going to be fixed.

## Out of scope

These are documented design properties, not vulnerabilities
([README](README.md#philosophy), PRD §1):

- **The agent runs commands, writes files and talks to the network.** YOLO by
  default is the product. "It executed something I did not intend" is a prompt
  or a usage problem, and the answer is external containment (container,
  micro-VM, seatbelt) — not a permission dialog.
- **Prompt injection** from repository content, model output, tool results or
  web pages. xdev ships no injection defense and claims none.
- **Anything reachable by code execution as your user.** If a process can read
  `~/.xdev/agent/`, it already owns your API keys and sessions; no bug needed.
- Weak or shared `GITHUB_TOKEN`/provider keys in your own config, and secrets
  you committed to your own repository.

## In scope

- **Credential handling.** An API key, OAuth token or keychain reference
  reaching logs, a session transcript, argv, an error message, or a network
  endpoint other than the provider you configured. Note `xdev` shells out for
  keychain reads only under an explicit `keychain:` reference, and refuses to
  store a secret in a place it says it will not.
- **The update and install path.** Any way to get xdev to execute a binary that
  did not come from the release it claims: a checksum that verifies against the
  wrong manifest, `SHA256SUMS` skipped on a re-run, a plain-HTTP download
  accepted where HTTPS was configured, or a symlink followed outside the
  install target in [`scripts/install.sh`](scripts/install.sh).
- **Session-file handling of hostile input.** A crafted JSONL transcript,
  import, or `--resume` target that corrupts another session, escapes a bounded
  buffer, or injects terminal control sequences that survive into a render path.
- **Bounded-resource failures.** A transcript, grep result, tool output or MCP
  message that grows without a ceiling and exhausts memory or disk (the <100 MB
  RSS budget is a security property as well as a performance one: it is what
  keeps a hostile session from becoming an escape hatch for a sandbox).
- **The extension/subprocess and MCP protocol.** A foreign process's JSON that
  makes xdev run something it was never asked to run, or that bypasses the
  fail-closed `tool_call` policy for dynamically registered tools
  (`ext_*`/`mcp_*`).
- **Harness-owned path protection.** The `edit`/`write` tools refuse to clobber
  files the harness owns; a way around that refusal is in scope.

## Version support

| Version | Supported |
|---|---|
| newest release tag | ✅ |
| `main` | ✅ |
| anything older | ❌ upgrade first — most reports resolve into "already fixed" |

`xdev version` prints the release you are on. CI stamps it at build time.
