# Pre-publication security audit — 2026-09-15

Two things are recorded here: the sweep run against the repository before it
could go public (§1–§3), and the history rewrite that sweep led to (§4). The
commit SHAs quoted in §2 and §3 are pre-rewrite — they stop resolving once
`main` is rewritten, which is the point. The findings they support do not
change.

Scope: everything a public `FreePeak/xdev` would expose. Question asked: *is
there a secret file, or a secret value, in any commit or any current file?*

## 1. Secrets: none found

| Detector | Coverage | Result |
|---|---|---|
| `gitleaks detect` (history mode) | 348 commits, 7.1 MB | 6 findings — all fixtures, itemised below |
| `gitleaks detect --no-git` (worktree incl. untracked) | 6.3 MB | the same 6 |
| Full object-DB sweep, 23 provider rules | **2,029 blobs** (1,888 reachable + 141 unreachable), 475 commits | 0 real secrets |
| Shannon-entropy sweep over credential-shaped tokens | same | 0 beyond the fixtures |
| Secret **filenames** across all 70 ref trees | `.env`, `credentials.json`, `secrets.yml`, `*.pem/key/p12`, `.npmrc`, `id_*`, service-account JSON | never committed |
| GitHub-side state (`gh api`) | secrets, vars, deploy keys, webhooks, environments, releases, issue/PR bodies + comments | 0 secrets |
| `go vet` / `gofmt` / `go build` / tests on the pushed tip | archive of `origin/main` | clean, green |
| `govulncheck` on the pushed tip | reachable symbols | 0 vulnerabilities affecting code |
| Workflow review | `ci.yml`, `release.yml` | no injection sink, scoped permissions (§3) |

Every hit the scanners produced is a value that exists in order to be tested,
not to be used. Values are described rather than quoted, so this document adds
nothing for a scanner to find:

1. `docs/research/fx-internals.md` ×2 — an environment **variable name** from
   another project (`FX_DISABLE_KEYCHAIN=…`), which carries no value in it.
2. `internal/config/secrets_test.go` — the example access-key ID published in
   AWS's own documentation, plus a short `sk-…` string used as an env-parsing
   fixture.
3. `internal/config/credentials_test.go` — the `sk-…` payload the file-mode
   test writes into a temporary credentials file.
4. `internal/oauth/oauth_test.go` — the fake refresh token the redaction test
   asserts is never echoed back.

Reinforcing evidence, not just pattern absence:

- 18 of 23 provider rules (PEM/OpenSSH private keys, `ghp_*`, Slack, Google,
  Stripe, Anthropic, JWT, basic-auth URLs, npm `_authToken`, GCP service-account
  JSON, SSH public keys, Telegram, Twilio, SendGrid, Discord, GitLab, kubeconfig,
  AWS secret-context) matched **zero** objects — including the unreachable blobs
  left behind by earlier history rewrites.
- No commit message or author metadata carries a credential. `.git/config`,
  `packed-refs`, `FETCH_HEAD` and `info/exclude` are clean and are never pushed.
- The repository had **no** Actions secrets, variables, deploy keys, webhooks or
  environments configured, and release `v0.1.0` carries only built binaries.
  Those assets were re-scanned byte-for-byte: no build path of this machine and
  no credential-shaped string survives in them (`-trimpath` did its job).
- Shipped code never defaults a key: credentials resolve at runtime from env →
  `credentials.json` (0600 and `nlink == 1`, re-verified on every read) →
  Keychain (read-only source, never a sink). `scripts/install.sh` and
  `internal/dist` verify every download against the release's `SHA256SUMS` over
  HTTPS.
- A negative control was run: a freshly generated real-shaped key dropped into a
  throwaway clone is caught by the gate (exit 1), so the clean result is not the
  result of a scanner that cannot fire.

**Rotation is not required.** Nothing needs to be revoked.

## 2. Publish blockers found (in history, so they needed a rewrite)

Not secrets, but things you cannot un-publish. All were reachable from
`origin/main` and already on the private remote.

1. **15 MB self-built binaries in history.** `xdev` was swept in by `git add -A`
   in `a6e2913`, `00373b1` and `90e2371`, removed in `6f588e0`. Deleting the file
   removes it from the tree, not from the object database: ~45 MB of blob data
   stayed permanent, and every clone carried it forever. Each binary also embeds
   ~230 absolute build paths naming this machine.
2. **Real personal paths and a real session transcript.**
   - `internal/session/testdata/omp-sample.jsonl` — a genuine session from this
     machine: the `cwd` and tool arguments under the user's config directory, a
     tool-result directory listing, and assistant thinking text.
     `internal/session/interop_test.go` asserts on that exact path, so the
     fixture and the assertion had to be scrubbed together.
   - `docs/research/claude-code/cc-bundle-forensics.md`,
     `claude-code/cc-resweep-2026-09-14.md`, `hermes-internals.md`,
     `omp-todos-internals.md` — absolute paths into the local Claude project
     directory, including one belonging to a **different private project**, plus
     session UUIDs, a real tool-call ID and a local virtualenv path.
   - `docs/PRD.md` and one TUI test named the working directory under a company
     folder.
3. **`.selftest/` scratch committed to history** (added in `95daac3`/`04ee8e8`,
   deleted in `08b5b04`; 5 blobs still reachable from `origin/main`, and still at
   the tip of the pushed `feat/cmd-fix-model-picker`,
   `fix/model-picker-discovery-warm` and `fix/seed-model-alignment`). Content is
   benign placeholder text — history noise, not exposure.
4. **Author identity in 401 commits**, in four spellings (`linh.doan`,
   `Linh Doan`, `linhdmn`, and a GitHub noreply address on one commit) — the
   same person, which reads as sloppiness and makes the commit graph hard to
   attribute. Two commit messages also carried a `Co-Authored-By:` trailer naming
   a third-party tool.

Decisions taken with the owner: keep the personal Gmail address as the one
author identity (it is already the identity of the published GitHub account),
keep `onegw` (it is a local gateway name and leaks nothing usable — no host, no
port beyond `127.0.0.1:8080`, no key), and scrub the home-directory and
company-folder prefixes from every object.

## 3. Pipeline review

`ci.yml` and `release.yml` are sound as they stand: top-level
`permissions: contents: read`, the only `contents: write` is the publish job; the
Apple signing secrets reach the runner through `env:` and are never interpolated
into a shell line; the single literal `${{ github.event.inputs.version }}`
interpolation is a maintainer-only `workflow_dispatch` string, and
`${GITHUB_REF}` is read from the environment rather than injected. Nothing here
blocks publication.

What was missing is a gate for the next accidental `git add -A`: no workflow ran
a secret scan, and GitHub's own secret scanning / push protection is not enabled
for the repository. Added in this pass: a `gitleaks` CI job in `ci.yml` (pinned
version, `fetch-depth: 0` so the scan sees history and not one commit) plus
`.gitleaks.toml` for the fixtures.

`.gitleaks.toml` uses a **path** allowlist, not `.gitleaksignore` fingerprints:
fingerprints are computed from content, so the rewrite would have invalidated
every one of them. The known ceiling is that the four allowlisted files go blind
to new secrets in a scan; they are all test fixtures whose whole job is to hold
fake credential-shaped strings, and their tests are read in review.

## 4. The rewrite that was run

`git filter-repo` over a throwaway clone of the remote, with:

- `--invert-paths`: dropped `xdev`, `*.test`, and `.selftest/` **by path, across
  all history** — not just at the tip, which is what `.gitignore` does.
- `--replace-text` (`==>` form, so it rewrites blob **content**): the home
  prefix → `/home/dev`, and the company folder segment → `example/<same leaf>`.
  The second rule is deliberately one component shorter than the folder path so
  a test that asserts on the tail-abbreviated form of a long path still matches.
- `--replace-message`: the same rules applied to commit messages.
- `--name-callback` / `--email-callback`: every author and committer in the
  graph unified to the one identity.
- `--message-callback`: the two third-party `Co-Authored-By:` trailers removed.

Verified afterwards, against the rewritten object database rather than the tip:

| Check | Result |
|---|---|
| Objects parsed (blobs / trees / commits) | 1,870 / 1,720 / 405 |
| Largest blob in the repository | 180 KB (was 15 MB) |
| Pack size | 3.0 MB (was ~20 MB) |
| Home prefix, company folder, project slug — any object, incl. commit messages | 0 |
| Third-party author identity | 0 (405/405 unified) |
| `xdev` / `.selftest` / `*.test` in any tree, on `main` or at `v0.1.0` | 0 |
| `gitleaks detect` over the rewritten history | `no leaks found` |
| `go build` / `go vet` / `gofmt` | clean |
| `go test ./...` incl. `-race` on session, tui, config, oauth, serve | green |

## 5. Follow-ups

1. **Old commits stay reachable on GitHub through `refs/pull/*`.** Force-pushing
   `main` does not delete them: the head refs of PRs #74, #76, #77 and #270 still
   point at pre-rewrite commits, so the binaries and the personal paths remain
   fetchable by SHA while those refs exist. There are no forks and no watchers.
   To actually expire them, GitHub's support process for cached objects has to
   run after every branch and PR ref is gone — that is a support ticket, not a
   git command.
2. Enable **secret scanning + push protection** (Settings → code security →
   analysis). Free once public, and it is the gate that fires at push time
   rather than after.
3. Re-point the `v0.1.0` GitHub Release at the rewritten tag so its "source code"
   link resolves after the rewrite.
4. The local-only branches (`w*`, `goal-fanout`, `w1-*`, `xdev-edits-floor`,
   `fix/config-set-strict-roundtrip`, …) still carry pre-rewrite history with the
   junk and the paths. Rebase or recreate them on the new `main` **before** any
   of them is pushed; a single push of one of them re-publishes the whole old
   graph.
5. The unmerged work on the old `fix/config-set-strict-roundtrip` branch was not
   dropped: it is the last commit on the new `main`.
