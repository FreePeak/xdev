package dist

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// updateTarget resolves the binary `xdev update` replaces. It is a variable
// so the test can point the flow at a temp file instead of the running
// binary (which, under `go test`, is the test binary).
var updateTarget = func() (string, error) { return ResolveTarget("") }

// updateMain implements `xdev update [--channel stable|canary] [--check]
// [--timeout D]` and the `job` subcommand: resolve the channel's newest
// release, compare it with the running version, and install the platform asset
// after verifying its SHA-256 entry. Nothing is installed when the release
// carries no manifest.
//
// A --check run also records its answer for the next session start (check.go);
// a bare `xdev update` installs and records nothing, because an install is not
// a check. The record is what the `job` verb's schedule feeds.
func updateMain(args []string, version string, out, errw io.Writer) int {
	if len(args) > 0 && args[0] == "job" {
		// `xdev update job …` owns the twice-a-day schedule; see job.go.
		return jobMain(args[1:], version, out, errw)
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(errw)
	channel := fs.String("channel", channelEnv(), "release channel: stable | canary")
	check := fs.Bool("check", false, "report whether a newer release exists without installing it")
	timeout := fs.Duration("timeout", 10*time.Minute, "bound this run (the scheduled check passes a shorter one)")
	fs.Usage = func() {
		fmt.Fprint(errw, `usage: xdev update [--channel stable|canary] [--check] [--timeout D]
       xdev update job <install|remove|status>

Resolves the newest release for the channel from GitHub releases, compares
it with the running version, verifies the platform asset against the
release's SHA256SUMS, and atomically replaces this binary. --check records
its answer under the state dir, so the next session start can report it
without touching the network; the job verb schedules that check twice a day.

  XDEV_CHANNEL   default channel (stable)
  XDEV_UPDATE_REPO  owner/repo to update from (default FreePeak/xdev)
  XDEV_UPDATE_API   API root for mirrors (default https://api.github.com)
  GITHUB_TOKEN   optional; raises the anonymous API rate limit. When unset,
              xdev uses the token of a logged-in gh CLI, and failing that
              updates anonymously (60 requests/h per IP).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ch := strings.ToLower(strings.TrimSpace(*channel))
	if ch == "" {
		ch = ChannelStable
	}
	if ch != ChannelStable && ch != ChannelCanary {
		fmt.Fprintf(errw, "xdev: unknown channel %q (want stable|canary)\n", *channel)
		return 2
	}

	target, err := updateTarget()
	if err != nil {
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}
	c := &Client{Repo: envOr("XDEV_UPDATE_REPO", DefaultReleaseRepo), APIBase: os.Getenv("XDEV_UPDATE_API"), Token: githubToken()}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	rel, err := c.Latest(ctx, ch)
	if err != nil && c.Token != "" && errIsAuth(err) {
		// A stale GITHUB_TOKEN in the environment must not break an
		// otherwise-anonymous release lookup.
		fmt.Fprintf(errw, "xdev: GitHub rejected GITHUB_TOKEN (%v); retrying unauthenticated\n", err)
		c.Token = ""
		rel, err = c.Latest(ctx, ch)
	}
	record := *check
	if err != nil {
		if record {
			recordCheck(ch, "", version, err)
		}
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}
	if rel == nil {
		fmt.Fprintf(out, "no published release for channel %s in %s yet — nothing to do\n", ch, c.repo())
		if record {
			recordCheck(ch, "", version, nil)
		}
		return 0
	}
	latest := strings.TrimSpace(rel.Tag)
	assetName := PlatformAssetName()
	fmt.Fprintf(out, "latest %s release: %s\n", ch, latest)
	if record {
		// Before the up-to-date branch returns: a record saying "checked,
		// nothing newer" is exactly as useful as one naming a release.
		recordCheck(ch, latest, version, nil)
	}

	if cmp := Compare(version, latest); cmp >= 0 {
		fmt.Fprintf(out, "xdev %s is up to date (channel %s, latest %s)\n", displayVersion(version), ch, latest)
		return 0
	}
	asset := rel.AssetByName(assetName)
	if asset == nil {
		fmt.Fprintf(errw, "xdev: release %s has no asset %s (assets: %s)\n", latest, assetName, strings.Join(rel.AssetNames(), ", "))
		return 1
	}
	fmt.Fprintf(out, "update available: %s → %s (channel %s, %s, %.1f MB)\n",
		displayVersion(version), latest, ch, assetName, float64(asset.Size)/(1<<20))
	if *check {
		fmt.Fprintln(out, "run `xdev update` to install it")
		return 0
	}
	if !Valid(version) {
		fmt.Fprintf(errw, "xdev: warning: running version %q is not a release tag; installing %s anyway\n", version, latest)
	}

	wantSHA, err := c.SumsFor(ctx, rel, assetName, PlatformName())
	if err != nil {
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}
	err = InstallBinary(target, func(w io.Writer) error {
		return c.Download(ctx, *asset, w)
	}, wantSHA)
	if err != nil {
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}
	fmt.Fprintf(out, "installed %s (%s)\n", target, latest)
	verifyInstalled(ctx, target, out, errw)
	return 0
}

// recordCheck writes the background-check record for a --check run. A failed
// check is still a check: CheckedAt is set so the throttle covers it too, or a
// host with no route to GitHub would re-try on every single launch.
func recordCheck(channel, latest, version string, checkErr error) {
	rec := CheckRecord{CheckedAt: time.Now(), Channel: channel, Version: latest, Running: version}
	if checkErr != nil {
		rec.Error = checkErr.Error()
	}
	// A check that cannot record its own result is not news the caller must
	// act on: the printed report already said what happened.
	_ = WriteCheck(rec)
}

// verifyInstalled runs the freshly installed binary's `version` command:
// proof that the bytes that landed are executable, not just present. A
// failure is reported as a warning — the install itself already happened,
// and a stale binary on PATH (or a sandbox that blocks exec) is not a
// reason to report the update as failed.
func verifyInstalled(ctx context.Context, target string, out, errw io.Writer) {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, target, "version")
	cmd.Stdin = nil
	b, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(errw, "xdev: warning: %s did not run: %v\n", target, err)
		return
	}
	fmt.Fprintf(out, "verified: %s\n", strings.TrimSpace(string(b)))
}

func displayVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unknown)"
	}
	return v
}

func channelEnv() string {
	if v := strings.TrimSpace(os.Getenv("XDEV_CHANNEL")); v != "" {
		return v
	}
	return ChannelStable
}

// githubToken resolves the credential to update with, in the order the user
// already controls: the environment, then the gh CLI they are logged into,
// then nothing. The last step is not a failure — an anonymous update works
// until the 60 req/h per-IP quota runs out, and a machine with no GitHub
// credentials at all is the ordinary case, not an exception.
func githubToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ghCLIToken()
}

// ghCLIToken asks an installed, logged-in gh for its token, the same way
// internal/tool already drives gh. Bounded and silent: no gh, no login, a
// keyring prompt, or a slow binary all read as "no token", which is the
// anonymous path that worked before this existed. A variable so the tests can
// stand in for it — the machine running them is not the machine under test.
var ghCLIToken = func() string {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "auth", "token")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
