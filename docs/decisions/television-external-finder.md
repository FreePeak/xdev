# Decision: television (`tv`) as an external file finder

*Reference: [alexpasmantier/television](https://github.com/alexpasmantier/television) (MIT, Rust, v0.15.9 + docs on `main`, 2026-10-07), this repo's in-process fuzzy layer (`internal/tui/picker.go`, `internal/tui/suggest.go`, `internal/tui/pathindex.go`) · Date: 2026-10-12 · Status: **deferred — recorded, not built** (see §4)

**Decision in one line: `tv` is usable and low-risk as an external subprocess picker (MIT, pipe-in/selection-out, `scr.Suspend`/`Resume` is available, `--expect=ctrl-x` gives an escape hatch), but every in-app picking surface xdev has today is better served by the in-process picker, and the one thing `tv` is genuinely better at — an any-location repo-file finder — needs an fd-style whole-repo index that does not exist yet. Ship nothing now; the upgrade path is recorded in §5.**

---

## 1. Context — what `tv` is

`tv` is a fast, portable, hackable fuzzy finder in Rust: it reads lines from a source (a command, a channel, or stdin), live-filters them (fuzzy / `'substring` / `^prefix` / `suffix$` / `!negate`, combined with AND), shows an optional preview, and **prints the selection to stdout on Enter** — so it composes like any unix picker (`vim $(tv files)`). Configuration is TOML channels in `~/.config/television/cable/*.toml` (source command, preview command, keybindings, custom actions, themes); built-in channels wrap `fd`/`bat`/`rg`. `tv files` / `tv text` / `tv env` / `tv git-repos` ship by default.

Verified against the live binary (macOS arm64 Homebrew 0.15.9) at evaluation time:

| Fact | Measurement |
| --- | --- |
| Ad-hoc mode | `tv -s '<cmd>'` replaces the channel; preview via `-p <cmd>` with `{}` substitution; selection printed to stdout on Enter. |
| Startup | ~25 ms to the alternate screen; small Homebrew binary. |
| Escape hatch | `--expect='ctrl-x'` confirms on an extra key and prints `<key>\n<selection>` — survives a top-level key that xdev already owns. |
| Batch/API hides | `--select-1` / `--take-1` / `--take-1-fast` confirm a single/first match with zero extra keystrokes (the fzf-style integration trick). |
| Exit | Esc quits cleanly (exit 0, no selection); the tty is restored even under a pty probe. |
| Input source | Reads keys from the controlling tty, not stdin — a fullscreen modal, not an inline zsh/fzf-style panel. |

## 2. Context — what xdev already has

xdev's fuzzy layer is **in-process and input-memory-bounded** on purpose:

- `internal/tui/picker.go` — the modal picker (`/model`, `/resume`, tab pickers): filter-as-you-type, section headers, tabs, mouse hit-tables, 33 ms-tick integration. `refresh()` re-ranks with the shared `fuzzyScore`.
- `internal/tui/suggest.go` — the subsequence scorer (exact-prefix > word-start > consecutive > sparse), shared by the slash-command menu.
- `internal/tui/paths.go` + `pathindex.go` — **`@`-file completion, directory-scoped**: one `os.ReadDir` per keystroke, plus a background index of every file (sorted by lowercased basename, binary search) so `@pa` offers `internal/tui/paths.go` from memory. This is a hard-won design (#329, PRD §3 editor row, 2026-09-17): the walk was deleted off the keystroke path — 5.7 s/keystroke on a 226k-file polyrepo — and the PRD rules out "fuzzy over a repo walk".
- Every picker is memory-bounded within the <100 MB RSS budget (PRD goal 2) and theme-matched.

Consequence: the *experience* of fuzzy picking is not missing from xdev; only an **any-location repo-file finder** (vim-style, not `@name`-prefix) is. That is the one surface where `tv` genuinely excels.

## 3. The options considered

**A. Interface swap** — feed `PickerView.Items` into `tv` instead of the in-process picker. Rejected for today: xdev's pickers are data-in-memory menus (model names, sessions, recent sessions) where the in-process picker wins — keystroke-cheap, zero forks, no tty hand-off — and `tv` is a filesystem/pipe finder, not a menu component.

**B. Subprocess dialogue** — the pi/omp pattern xdev already uses for `!cmd` shell mode, MCP servers and LSP binaries: `scr.Suspend()` → spawn `tv` with the tty handed over → read the printed selection → `scr.Resume()`. Verified-plausible: `tcell.Screen` implements both `Suspend` and `Resume` (tcell v2.13.10), and xdev already runs external terminal sessions. This is the shape any future integration takes.

**C. Grep-driven** — `xdev search` already does full-text session search; hopping to `$EDITOR` on a result uses the same seam. Zero new code.

## 4. Decision

**Defer.** Do not wire `tv` into the TUI in this milestone. Reasons, one line each:

1. **Every existing picker is better served in-process** — model/session/tab menus are small, hot, in-memory lists; a fork + tty hand-off per interaction costs more than it saves.
2. **The one real win needs groundwork that does not exist** — a whole-repo file finder needs an fd-style index with the same memory/stall accounting as `pathindex.go`; without it, `tv` is just a prettier `@` completion over a walk the repo deliberately refuses.
3. **Runtime dependency in a product line that promises "one binary, no install-time toolchain"** (PRD goal 3) — `tv` is not part of the xdev artifact; taking it as a dependency means every channel user must install it, and `tv`'s first-class channels also want `fd`/`bat`/`rg`.
4. **Scope** — the user's ask was "learn about television, then check if we can use it": the answer to *can we* is yes (option B is proven feasible), but the *should-we-today* answer is no.

The decision is **recorded, not built** — the same handling `local-tiny-models.md` uses for an opt-in it ships a seam for but does not enable.

## 5. Upgrade path (when a `/find`-style file picker is actually wanted)

1. **Add the index first, in-process** — reuse the `pathindex.go` walk pattern (background, off the keystroke path, atomic publish, `indexSkipDir`) or share `fscache`'s cached walk for a >100k-file tree; deliver an any-location file suggestion list in the existing picker. This removes reason 2 with no new dependency.
2. **Only then, if the in-process list is not enough** — the B seam: `/find` (or an unbound key) → `scr.Suspend()` → `tv -s 'fd -t f' -p 'bat -n --color=always {}' --expect='ctrl-x'` → read the selection → `scr.Resume()`. A `ctrl-x` confirm (not Enter) both terminates and distinguishes *picked* from *cancelled* (Esc prints nothing). Default channel `files` already exists on macOS via Homebrew; `tv update-channel` pulls community channels.
3. **Keep the runtime dependency optional** — LookPath-gate `tv` (the same way `ast-grep` is), and degrade to the in-process picker when it is absent. Never ship the Rust binary inside the artifact.

## 6. Non-goals and recorded ceilings

- **No `tv` dependency in the shipped binary** — `ponytail:` ceiling: the artifact stays one CGO-free binary (PRD goal 1 / goal 3); any future integration is a LookPath-gated subprocess, per §5.
- **No replacement of the in-process picker / `/model` / `/resume` menus** — they are memory-bounded and theme-matched; a subprocess adds a fork and a tty hand-off for no gain on small in-memory lists.
- **No inline (zsh/fzf-style) mode** — `tv` is a fullscreen modal; matching its inline look would mean forking and repainting inside the alt screen, which is strictly worse than the on-screen dropdowns xdev already paints.

---

*Last updated: 2026-10-12 (recorded the television evaluation — feasible, deferred; see docs/PRD.md §2 M-file-finder row and §5 key-decisions bullet).*