# Running xdev in a container or sandbox

xdev is a single static binary with no runtime dependencies, which makes it easy
to confine. This page covers the three practical shapes: a plain container, a
read-only-ish dev container, and a host sandbox (macOS `sandbox-exec`, Linux
namespaces).

Facts used below come from the build and code:

- `CGO_ENABLED=0` is a product guarantee; the binary is static (~10 MB
  stripped, ~15 MB default) for six platforms (darwin/linux/windows × amd64/arm64).
- Config lives under `<dataDir>` (`~/.xdev/agent`): `models.yml`, `config.yml`,
  `keybindings.yml`, `sessions/`, `blobs/`, `skills/`, `extensions/`, `memory/`.
  Override the whole tree with `XDEV_*` env or mount it.
- Credentials resolve from `--api-key`, `models.yml`, stored OAuth, `/login`,
  and the environment. **In a container, pass the key through the environment
  and never bake it into an image layer.**
- Extensions are executables in `<dataDir>/extensions` — mount a directory only
  if you intend to run them.

---

## 1. Minimal container

The binary needs a writable data directory (sessions and blobs) and, for coding
work, a mounted project.

```dockerfile
FROM gcr.io/distroless/static-debian12:nonroot
COPY xdev /usr/local/bin/xdev
ENTRYPOINT ["/usr/local/bin/xdev"]
```

```sh
docker run --rm -it \
  --cpus=2 --memory=4g \
  -e ONEGW_KEY="$ONEGW_KEY" \
  -v "$PWD:/work" -v xdev-data:/home/nonroot/.xdev \
  -w /work \
  xdev "run the test suite and fix what fails"
```

Notes:

- `--memory=4g` is generous on purpose: the product budget is <100 MB RSS for
  the agent process itself (see the PRD), but tool subprocesses (compilers, test
  runs) are the real memory consumers.
- `-v xdev-data:` keeps sessions/blobs across runs; without it every run starts
  cold and costs the model a fresh context.
- Set `NO_COLOR=1` or `TERM=dumb` if you are capturing plain logs rather than a
  TTY; the TUI needs a real terminal (it uses tcell's alternate screen).

## 2. Dev container with a TTY

For interactive use, run the TUI with the host terminal attached and a mounted
project. Nothing about the container changes the TUI; it only needs a PTY.

```sh
docker run --rm -it --init \
  --cpus=2 --memory=8g \
  -e ONEGW_KEY="$ONEGW_KEY" -e TERM="$TERM" \
  -v "$PWD:/work" -v xdev-data:/home/nonroot/.xdev \
  -w /work xdev tui
```

`--init` matters: xdev runs tools in their own process groups and kills whole
groups on abort; without an init as PID 1, orphans accumulate.

## 3. Read-only project, writable scratch

To let the agent read a repository but write only outside it (the strongest
guarantee short of a policy hook):

```sh
docker run --rm -it \
  -v "$PWD:/work:ro" \
  -v "$PWD/.scratch:/work/.scratch:rw" \
  -v xdev-data:/home/nonroot/.xdev \
  -w /work xdev "review this repo and write your notes to .scratch/review.md"
```

The agent will fail its `write` calls outside `.scratch` — that is the mount
doing the work, not xdev. Pair it with a policy hook (see below) if you also
want to forbid, say, `bash`.

### Policy hooks inside the container

An extension that denies a tool is enforced **before** execution, fail-closed:

```
<dataDir>/extensions/deny-rm.py      # see docs/reference/extension-protocol.md
```

Mount a directory with that executable and xdev refuses matching `bash` calls
with a reason the model can read. This is the right layer for "no network",
"no rm", "no writes" rules when the orchestration is fixed but the model's
commands are not.

## 4. Host sandbox (macOS)

macOS ships `sandbox-exec`. A profile that allows reads everywhere, writes only
to the project and data dir, and no network except the provider:

```scheme
(version 1)
(deny default)
(allow process-exec* process-fork)
(allow file-read*)
(allow file-write* (subpath "/work") (subpath "/Users/you/.xdev"))
(allow network-outbound (remote tcp "*:443"))
(allow sysctl-read)
```

```sh
sandbox-exec -f xdev.sb xdev "summarize this repo"
```

`(allow file-read*)` is broad — xdev reads context files and the filesystem for
search. Tighten it to the project plus the toolchain paths once you know what
your builds need.

## 5. Host sandbox (Linux)

Namespaces give the same shape without extra tooling:

```sh
unshare --map-root-user --net --mount --pid --fork \
  sh -c 'mount --bind /work /work && exec xdev tui'
```

For a network-less run, drop `--net` privileges entirely (`--net` above creates
a fresh, empty network namespace): the agent then cannot reach a remote
provider, so only local/hosted-on-the-same-host models will work.

## 6. Verification checklist

After wiring any of the above, confirm:

1. `xdev version` runs in the sandbox (proves the binary + loader are intact).
2. A trivial prompt answers (`xdev "reply with ok"`), which proves credentials
   and network reach the provider.
3. `xdev "write /etc/probe"` **fails** — if it succeeds, your confinement is
   not actually confining writes.
4. `ls <dataDir>/sessions` shows the session after exit, proving the data
   volume is writable and mounted where xdev expects it.

## 7. What no sandbox covers: the repository you cloned

A sandbox confines the process. It says nothing about who **configured** the
process — and two of xdev's configuration files arrive inside the checkout, so
`git clone` is itself a configuration event. xdev gives the two authorities
different rights (#114).

This is not something a container substitutes for: a sandbox that hands the
process your network and your `~/.xdev` mount still has to pick an endpoint, and
before #114 that decision could come from a file in the repository.

### What a repository may configure

`<cwd>/.xdev/config.yml`, `<cwd>/.xdev/models.yml` and `<cwd>/.xdev/secrets.yml`
are read like any other layer, then pruned to this set. Anything else they name
is **ignored and reported on stderr at startup**, with the line pointing at
`<dataDir>/config.yml` as the file that may set it.

| File | A repository may set | A repository may not set |
|---|---|---|
| `.xdev/config.yml` | `theme`, `colorBlindMode`, `statusLine`, `showThinking` (interface); `memoryLimit`, `maxTurns`, `compaction`, `branchSummary`, `experimentalContextManagement`, `models` (caps and context shape); `ask`, `tts` (per-tool knobs naming no binary and no endpoint) | anything carrying authority: `approvalMode`, `toolsApproval`, `bashPatterns`, `bash.interceptor`, `hooks`, `lsp`, `debug`, `computer`, `plugins`, `defaultModel`, `modelRoles`, `modelRolesEffort`, `retry`, `prewalk`, `memory*`, `hindsight`, `webSearch`, `imageProviders`, `browser`, `ttsr`, `advisor*`, `personality`, `autolearn*`, `lessonCap`, and the provider enable/disable lists |
| `.xdev/models.yml` | per-provider `api`, `discovery`, `toolsFormat`; per-model `id`, `name`, `reasoning`, `vision`, `contextWindow`, `maxTokens` — what exists and how to speak it | `baseUrl`, `apiKey`, `apiKeys`, `authHeader`, `auth`, `headers`, `oauth`, `project`, `location`, `deployment`, `apiVersion`, top-level `defaultModel`, the per-model `baseUrl`/`apiKey`/`headers` overrides. `${VAR}` expansion does **not** run on this file, so a repository cannot read your environment into a request |
| `.xdev/secrets.yml` | new entries to redact in this project | replace an entry the profile already defines — redaction matches on the value, so a clone that shadows `GITHUB_TOKEN` with a dummy would unmask a secret it never had |

The lists are allowlists, in code, in one file: `internal/config/reposafe.go`. A
key added to the schema is repository-unreachable until someone puts it there
deliberately; the alternative (a denylist) hands every future key to clones by
default. And on a provider named by both files, the **profile's entry wins
outright** — the project layer is applied first precisely so it cannot be the
last word.

### What a repository can still make xdev run

`.xdev/hooks/` is the one root where a clone's file is *executed*, so it needs a
decision rather than a policy: a hook's file name is its event, and
`agent_start.sh` fires with your environment before your first prompt. xdev
withholds those hooks until you review them (#241):

```console
$ xdev trust --list     # read what this repository wants to run, and when
$ xdev trust            # record the decision for this workspace
$ xdev distrust         # take it back
```

The record lives in your profile (`<dataDir>/trusted-workspaces.yml`, `0600`),
never in the repository — a decision stored inside the thing it constrains is
not a constraint — and it holds a digest per hook, so editing an approved script
re-asks instead of inheriting the earlier yes. A headless run never blocks on
the question: it withholds, says so, and names the repair. Hooks you installed
yourself (`<dataDir>/hooks`), `--hook` specs, settings-declared hooks, trusted
extensions and installed plugins are unaffected.

Repository `.xdev/commands`, `.xdev/agents` and `.xdev/skills` are read the way
`AGENTS.md` is: their text enters the model context, which is #81's territory.
