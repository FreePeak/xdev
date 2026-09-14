# Decision: the macOS Keychain as a credential source, never a credential sink

*Issue: [#123](https://github.com/FreePeak/xdev/issues/123) · Milestone: M9 (providers, auth, config) · Date: 2026-09-14 · Status: **accepted (scope amended by measurement)***

**Decision: xdev reads credentials out of the macOS Keychain and never writes to it. `providers.<p>.apiKey: keychain:<service>[/<account>]` is a credential source on macOS; the store xdev writes stays the verified `0600`/`nlink==1` `credentials.json`. The issue's first box — "store `/login` and OAuth credentials in the Keychain" — is rejected as unsalvageable through `/usr/bin/security`, for the measured reason in §3.**

---

## 1. Context

`/usr/bin/security add-generic-password` — the documented way to keep a secret out of a file on macOS — accepts the secret in exactly two places:

| Path | How | Cost |
| --- | --- | --- |
| argv (`-w <secret>`) | trivially scriptable | the secret is in the process table, readable by any other user with `ps`. Strictly worse than the `0600` file it replaces |
| the prompt (`-w`, no value) | read from stdin (or the tty) twice, for confirmation | **silently truncates at 128 bytes** |

fx reaches for the prompt path and drives it under `expect` with a PTY (`spawn -noecho /usr/bin/security add-generic-password -a $account -s $service -U -w`) so the value never appears in argv. That is the shape #123 asked xdev to copy.

## 2. What was measured

All numbers below are from `/usr/bin/security` on this machine (macOS 26.6, arm64), re-measured inside the Go process that would use it, not from documentation.

Prompt-fed write, offered N bytes, item holds:

```
wrote=100 read=100     wrote=127 read=127     wrote=1000 read=128
wrote=120 read=120     wrote=128 read=128     wrote=4096 read=128
wrote=126 read=126     wrote=129 read=128     wrote=8192 read=128
```

The exit status on every truncated write was **0**, and `security` printed nothing to stderr. The same 400-byte payload offered through the prompt inside the Go test process stored 128 bytes.

Read: a 2 000-byte JWT-shaped value written by argv (the human/1Password path) came back byte-exact, and `ResolveCredential` resolved it end to end with every other rung (a stored login key and `ONEGW_API_KEY`) deliberately present and unused.

The `expect` route, tried in a shell: `security add-generic-password … -w` under `spawn -noecho` with `expect "password data"` / `send` reported success and **the item did not exist afterwards** (`find-generic-password` exit 44). Not investigated further — see §4.

## 3. Why the write lane is rejected

1. **The truncation is silent.** 129 offered, 128 stored, status 0, no diagnostic. An API key survives; an OAuth access or refresh token (a JWT, typically 1–2 KB) does not. The user would be told the login worked, and the token would be corrupt from the first refresh. That is a *worse* failure than a file permission problem, because it looks like success.
2. **A read-back check makes it safe but not useful.** Writing, reading, and comparing turns the silent corruption into a loud refusal — but then every OAuth login fails to store, and the file remains the only working sink. That is the shipped behavior with extra steps and an extra subprocess on the login path.
3. **argv is not a fallback.** The whole point of the Keychain lane is to keep a long-lived secret off disk; passing it through the process table trades a `0600` file for a machine-wide `ps` read.
4. **The parity target's mechanism is not portable here.** Reaching for `expect` (a Tcl interpreter, present on macOS today but not something xdev's six-platform matrix can assume) to drive an interactive prompt, for a path that measured as writing nothing, is not a foundation for a credential store.

So: xdev reads and never writes. Writing an item stays with tools built for it — Keychain Access.app, `1password --read`, `op read`, a secrets agent, or the user's own `security -w` invocation, where the argv exposure is their decision made in their own shell.

## 4. What ships instead

- `providers.<p>.apiKey: keychain:<service>[/<account>]` resolves through `/usr/bin/security find-generic-password -s <service> -a <account> -w` at credential-resolution time. The lookup keys are argv; the secret travels on stdout only (`TestKeychainReadShape/the argv never carries the secret` pins that argv shape).
- A reference that cannot be satisfied is an **error naming the reference**, never a fall-through to the next rung. The rungs below it are different credentials, which means a different account, quota or team — the same "explicit choice is an exact authority" rule that #114 applied to configuration and that this issue's fourth box asked for.
- `XDEV_DISABLE_KEYCHAIN=1` turns lookups off; a disabled or non-mac host fails with that reason rather than pretending the item is missing.
- The item name is not derived from a repository: `apiKey` is profile-owned since #114, so a clone cannot point xdev's Keychain lookups anywhere.
- The file store keeps the guarantees added in the first half of this issue: verified `0600` **and** `nlink == 1` on every read, refusal naming the repair, and the data directory tightened to `0700` on write.

## 5. Revisit when

- A CGO-free Keychain write path exists that is not a prompt and not argv (a linked Security.framework would break the `CGO_ENABLED=0` guarantee, PRD §1 goal 1; `security` accepting `--data-stdin` would not).
- xdev gains an app-scoped identity (a signed bundle with a stable code signature), where a keychain item's ACL can restrict readers to *this* binary rather than to `/usr/bin/security`. Today that ACL benefit is unavailable to an unsigned single-file binary anyway, which removes most of the remaining argument for writing there.
- If a future `apiKeyHelper` implementation (#177) exists, a Keychain read becomes one command away for anyone who wants it (`apiKey: ${...}` plus a helper script), which is the composable version of this feature.

## 6. Evidence

- `internal/config/keychain.go` — the read lane, with these measurements in its package comment.
- `internal/config/keychain_test.go` — ref grammar, stdout/stderr/exit-status handling (including the real "could not be found" wording), 8 KiB fidelity, the argv shape, and the no-fall-through table.
- Throwaway probes used to produce §2 were run against the real tool and deleted; the ceiling is reproduced by `security add-generic-password -a A -s S -U -w` fed `x`×400 twice on stdin, then `find-generic-password -w`.
