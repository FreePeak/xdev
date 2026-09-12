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

// updateMain implements `xdev update [--channel stable|canary] [--check]`:
// resolve the channel's newest release, compare it with the running
// version, and install the platform asset after verifying its SHA-256
// entry. Nothing is installed when the release carries no manifest.
func updateMain(args []string, version string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(errw)
	channel := fs.String("channel", channelEnv(), "release channel: stable | canary")
	check := fs.Bool("check", false, "report whether a newer release exists without installing it")
	fs.Usage = func() {
		fmt.Fprint(errw, `usage: xdev update [--channel stable|canary] [--check]

Resolves the newest release for the channel from GitHub releases, compares
it with the running version, verifies the platform asset against the
release's SHA256SUMS, and atomically replaces this binary.

  XDEV_CHANNEL   default channel (stable)
  XDEV_UPDATE_REPO  owner/repo to update from (default FreePeak/xdev)
  XDEV_UPDATE_API   API root for mirrors (default https://api.github.com)
  GITHUB_TOKEN   optional; raises the anonymous API rate limit

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rel, err := c.Latest(ctx, ch)
	if err != nil && c.Token != "" && errIsAuth(err) {
		// A stale GITHUB_TOKEN in the environment must not break an
		// otherwise-anonymous release lookup.
		fmt.Fprintf(errw, "xdev: GitHub rejected GITHUB_TOKEN (%v); retrying unauthenticated\n", err)
		c.Token = ""
		rel, err = c.Latest(ctx, ch)
	}
	if err != nil {
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}
	if rel == nil {
		fmt.Fprintf(out, "no published release for channel %s in %s yet — nothing to do\n", ch, c.repo())
		return 0
	}
	latest := strings.TrimSpace(rel.Tag)
	assetName := PlatformAssetName()
	fmt.Fprintf(out, "latest %s release: %s\n", ch, latest)

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

func githubToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
