package dist

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRepo is a GitHub releases API stand-in: /releases/latest, /releases
// and the asset host, so the update path is exercised over real HTTP
// without touching github.com.
type fakeRepo struct {
	tag        string
	prerelease bool
	binary     []byte
	// sumsBody is the manifest served as SHA256SUMS; "" publishes a release
	// with no manifest at all (the unverifiable case).
	sumsBody string
	// assetName overrides the published platform asset (default: this host's).
	assetName string
	// rejectToken answers 401 to any request carrying an Authorization
	// header (the stale-GITHUB_TOKEN case).
	rejectToken bool
	assetHits   atomic.Int32
	seen        atomic.Value // string: last path requested
}

func (f *fakeRepo) name() string {
	if f.assetName != "" {
		return f.assetName
	}
	return PlatformAssetName()
}

func (f *fakeRepo) releaseJSON(host string) string {
	assetURL := "http://" + host + "/assets/" + f.name()
	assets := fmt.Sprintf(`{"name":%q,"url":%q,"size":%d}`, f.name(), assetURL, len(f.binary))
	if f.sumsBody != "" {
		assets += fmt.Sprintf(`,{"name":"SHA256SUMS","url":%q,"size":%d}`,
			"http://"+host+"/assets/SHA256SUMS", len(f.sumsBody))
	}
	return fmt.Sprintf(`{"tag_name":%q,"name":%q,"draft":false,"prerelease":%t,"assets":[%s]}`,
		f.tag, f.tag, f.prerelease, assets)
}

func (f *fakeRepo) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/FreePeak/xdev/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if f.rejectToken && r.Header.Get("Authorization") != "" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		f.seen.Store(r.URL.Path)
		if f.prerelease {
			http.NotFound(w, r) // GitHub's /latest excludes pre-releases
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, f.releaseJSON(r.Host))
	})
	mux.HandleFunc("/repos/FreePeak/xdev/releases", func(w http.ResponseWriter, r *http.Request) {
		f.seen.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "[%s]", f.releaseJSON(r.Host))
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		switch name := path.Base(r.URL.Path); name {
		case "SHA256SUMS":
			fmt.Fprint(w, f.sumsBody)
		case f.name():
			f.assetHits.Add(1)
			w.Write(f.binary)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("XDEV_UPDATE_API", srv.URL)
	t.Setenv("XDEV_UPDATE_REPO", "FreePeak/xdev")
	return srv
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// withTarget points the update flow at a temp binary instead of the test
// binary, returning its path and the bytes it started with.
func withTarget(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "xdev")
	if err := os.WriteFile(p, content, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := updateTarget
	updateTarget = func() (string, error) { return p, nil }
	t.Cleanup(func() { updateTarget = prev })
	return p
}

func runUpdate(t *testing.T, args []string, current string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errw bytes.Buffer
	code = updateMain(args, current, &out, &errw)
	return code, out.String(), errw.String()
}

// fakeBinary is a stand-in release artifact that can actually execute, so
// the post-install self-check is exercised rather than stubbed.
func fakeBinary() []byte {
	return []byte("#!/bin/sh\necho \"xdev v9.9.9\"\n")
}

func TestUpdateInstallsVerifiedAsset(t *testing.T) {
	bin := fakeBinary()
	f := &fakeRepo{tag: "v9.9.9", binary: bin, sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	target := withTarget(t, []byte("old binary"))

	code, out, errw := runUpdate(t, nil, "1.0.0")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bin) {
		t.Errorf("installed bytes differ:\n got %q\nwant %q", got, bin)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: %v", fi.Mode())
	}
	for _, want := range []string{"update available: 1.0.0 → v9.9.9", "installed " + target} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if runtime.GOOS != "windows" && !strings.Contains(out, "verified: xdev v9.9.9") {
		t.Errorf("stdout missing post-install self-check:\n%s", out)
	}
	assertNoTempLeftovers(t, target)
}

func TestUpdateRefusesTamperedSums(t *testing.T) {
	bin := fakeBinary()
	// The manifest lists a hash for different bytes: a tampered or truncated
	// release must never reach the install path.
	f := &fakeRepo{tag: "v9.9.9", binary: bin, sumsBody: sha256Hex([]byte("something else")) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	original := []byte("old binary")
	target := withTarget(t, original)

	code, _, errw := runUpdate(t, nil, "1.0.0")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %s)", code, errw)
	}
	if !strings.Contains(errw, "checksum mismatch") {
		t.Errorf("stderr does not report the mismatch:\n%s", errw)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, original) {
		t.Errorf("target was modified despite the mismatch: %q", got)
	}
	assertNoTempLeftovers(t, target)
}

func TestUpdateUpToDateSkipsDownload(t *testing.T) {
	bin := fakeBinary()
	f := &fakeRepo{tag: "v1.0.0", binary: bin, sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	target := withTarget(t, []byte("old binary"))

	code, out, _ := runUpdate(t, nil, "v1.0.0")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("stdout does not report up-to-date:\n%s", out)
	}
	if n := f.assetHits.Load(); n != 0 {
		t.Errorf("asset downloaded %d times, want 0", n)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old binary" {
		t.Errorf("target was replaced while up to date: %q", got)
	}
}

func TestUpdateCheckReportsWithoutInstalling(t *testing.T) {
	checkDir(t) // --check records; keep the run out of the real state dir
	bin := fakeBinary()
	f := &fakeRepo{tag: "v9.9.9", binary: bin, sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	target := withTarget(t, []byte("old binary"))

	code, out, _ := runUpdate(t, []string{"--check"}, "1.0.0")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "update available: 1.0.0 → v9.9.9") || !strings.Contains(out, "run `xdev update`") {
		t.Errorf("--check output:\n%s", out)
	}
	if n := f.assetHits.Load(); n != 0 {
		t.Errorf("--check downloaded the asset %d times, want 0", n)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old binary" {
		t.Errorf("--check modified the target: %q", got)
	}
}

func TestUpdateRefusesUnwritableTarget(t *testing.T) {
	bin := fakeBinary()
	f := &fakeRepo{tag: "v9.9.9", binary: bin, sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	target := withTarget(t, []byte("old binary"))
	if err := os.Chmod(target, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(target, 0o755) })

	code, _, errw := runUpdate(t, nil, "1.0.0")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	// The exact hint the issue asks for: the operator must be told what to
	// run, not just that the install failed.
	if !strings.Contains(errw, "chmod u+w "+target) {
		t.Errorf("stderr lacks the chmod hint:\n%s", errw)
	}
	if n := f.assetHits.Load(); n != 0 {
		t.Errorf("asset downloaded %d times before the writability check, want 0", n)
	}
	if got, _ := os.ReadFile(target); string(got) != "old binary" {
		t.Errorf("unwritable target was modified: %q", got)
	}
}

func TestUpdateRefusesReleaseWithoutManifest(t *testing.T) {
	f := &fakeRepo{tag: "v9.9.9", binary: fakeBinary()}
	f.start(t)
	target := withTarget(t, []byte("old binary"))

	code, _, errw := runUpdate(t, nil, "1.0.0")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errw, "no SHA256SUMS manifest") {
		t.Errorf("stderr does not explain the refusal:\n%s", errw)
	}
	if got, _ := os.ReadFile(target); string(got) != "old binary" {
		t.Errorf("target replaced without a verifiable checksum: %q", got)
	}
}

func TestUpdateRefusesReleaseWithoutPlatformAsset(t *testing.T) {
	bin := fakeBinary()
	f := &fakeRepo{tag: "v9.9.9", binary: bin, assetName: "xdev_plan9_386", sumsBody: sha256Hex(bin) + "  xdev_plan9_386\n"}
	f.start(t)

	code, _, errw := runUpdate(t, nil, "1.0.0")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errw, "has no asset "+PlatformAssetName()) || !strings.Contains(errw, "xdev_plan9_386") {
		t.Errorf("stderr should name the missing asset and list what exists:\n%s", errw)
	}
}

func TestUpdateCanaryPicksPrereleaseAndStableSkipsIt(t *testing.T) {
	checkDir(t) // likewise: the canary --check run must not reach ~/.xdev
	bin := fakeBinary()
	f := &fakeRepo{tag: "v2.0.0-canary.1", prerelease: true, binary: bin, sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	withTarget(t, []byte("old binary"))

	code, out, errw := runUpdate(t, []string{"--channel", "canary", "--check"}, "1.0.0")
	if code != 0 {
		t.Fatalf("canary exit = %d, stderr = %s", code, errw)
	}
	if !strings.Contains(out, "update available: 1.0.0 → v2.0.0-canary.1 (channel canary,") {
		t.Errorf("canary output:\n%s", out)
	}
	if got := f.seen.Load(); got != "/repos/FreePeak/xdev/releases" {
		t.Errorf("canary queried %v, want the releases list", got)
	}

	// The same repository through the stable channel has nothing published.
	code, out, _ = runUpdate(t, []string{"--channel", "stable"}, "1.0.0")
	if code != 0 {
		t.Fatalf("stable exit = %d, want 0", code)
	}
	if !strings.Contains(out, "no published release for channel stable") {
		t.Errorf("stable output:\n%s", out)
	}
	if got := f.seen.Load(); got != "/repos/FreePeak/xdev/releases/latest" {
		t.Errorf("stable queried %v, want releases/latest", got)
	}
}

func TestUpdateRejectsUnknownChannel(t *testing.T) {
	withTarget(t, []byte("old binary"))
	code, _, errw := runUpdate(t, []string{"--channel", "nightly"}, "1.0.0")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errw, "unknown channel") {
		t.Errorf("stderr:\n%s", errw)
	}
}

func TestParseSums(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	got := ParseSums(strings.Join([]string{
		"# a comment",
		"",
		sum + "  xdev_darwin_arm64",
		sum + " *xdev_windows_amd64.exe",
		sum + "  dist/xdev_linux_amd64",
		"deadbeef  too-short-hash",
		"not a sums line",
	}, "\n"))
	want := map[string]string{
		"xdev_darwin_arm64":      sum,
		"xdev_windows_amd64.exe": sum,
		"xdev_linux_amd64":       sum,
	}
	if len(got) != len(want) {
		t.Fatalf("ParseSums = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("entry %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestPlatformAssetNameFor(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "xdev_darwin_arm64"},
		{"linux", "amd64", "xdev_linux_amd64"},
		{"windows", "amd64", "xdev_windows_amd64.exe"},
	} {
		if got := PlatformAssetNameFor(c.goos, c.goarch); got != c.want {
			t.Errorf("PlatformAssetNameFor(%s, %s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// A stale GITHUB_TOKEN in the environment is a normal state for a long-lived
// shell; it must not turn a working anonymous release lookup into a 401.
func TestUpdateRetriesWithoutTokenOnAuthFailure(t *testing.T) {
	bin := fakeBinary()
	f := &fakeRepo{tag: "v9.9.9", binary: bin, rejectToken: true,
		sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	t.Setenv("GITHUB_TOKEN", "stale-token")
	target := withTarget(t, []byte("old binary"))

	code, out, errw := runUpdate(t, nil, "1.0.0")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	if !strings.Contains(errw, "retrying unauthenticated") {
		t.Errorf("stderr does not report the token fallback:\n%s", errw)
	}
	if !strings.Contains(out, "installed "+target) {
		t.Errorf("stdout does not report the install:\n%s", out)
	}
}

// assertNoTempLeftovers proves the staged download is cleaned up on every
// failure path — a stray 15 MB .xdev-update-* per failed update is a leak
// users would find before we would.
func assertNoTempLeftovers(t *testing.T, target string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".xdev-update-") {
			t.Errorf("staged temp file left behind: %s", e.Name())
		}
	}
}

// TestCheckWritesTheRecord pins the contract between the two halves of the
// twice-a-day check: `--check` leaves its answer in the record file that a
// session start reads, and a plain `xdev update` (which installs) does not
// claim to have checked anything.
func TestCheckWritesTheRecord(t *testing.T) {
	bin := fakeBinary()
	f := &fakeRepo{tag: "v9.9.9", binary: bin, sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	checkDir(t)
	withTarget(t, []byte("old binary"))

	code, _, errw := runUpdate(t, []string{"--check"}, "v1.0.0")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	rec := ReadCheck()
	if rec.CheckedAt.IsZero() {
		t.Fatal("--check wrote no record")
	}
	if rec.Version != "v9.9.9" || rec.Channel != ChannelStable || rec.Running != "v1.0.0" {
		t.Errorf("record = %+v", rec)
	}
	if f.assetHits.Load() != 0 {
		t.Errorf("--check downloaded %d assets", f.assetHits.Load())
	}
	// The next session start's notice is now derivable from the record alone —
	// no network — and it says the thing that is true.
	if got := Notice("v1.0.0"); !strings.Contains(got, "v9.9.9 is available") {
		t.Errorf("Notice = %q", got)
	}
	if got := Notice("v9.9.9"); got != "" {
		t.Errorf("notice survives installing the release it named: %q", got)
	}
}

func TestUpToDateCheckRecordsNothingNewer(t *testing.T) {
	f := &fakeRepo{tag: "v1.0.0", binary: fakeBinary(), sumsBody: ""}
	f.start(t)
	checkDir(t)
	withTarget(t, []byte("old binary"))

	code, out, errw := runUpdate(t, []string{"--check"}, "v1.0.0")
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, out, errw)
	}
	if got := ReadCheck(); got.Version != "v1.0.0" || got.CheckedAt.IsZero() {
		t.Errorf("an up-to-date check wrote %+v; the throttle needs CheckedAt", got)
	}
	if n := Notice("v1.0.0"); n != "" {
		t.Errorf("up to date produced a notice: %q", n)
	}
}

func TestCheckFailureRecordsTheAttempt(t *testing.T) {
	t.Setenv("XDEV_UPDATE_API", "http://127.0.0.1:1/") // nothing listening
	t.Setenv("XDEV_UPDATE_REPO", "FreePeak/xdev")
	checkDir(t)
	withTarget(t, []byte("old binary"))

	code, _, errw := runUpdate(t, []string{"--check"}, "v1.0.0")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %s)", code, errw)
	}
	rec := ReadCheck()
	if rec.CheckedAt.IsZero() {
		t.Fatal("a failed check wrote no record: the next launch would retry immediately")
	}
	if rec.Error == "" {
		t.Errorf("failed check recorded no reason: %+v", rec)
	}
	if rec.Version != "" {
		t.Errorf("a failed check recorded a version it never resolved: %+v", rec)
	}
}

func TestInstallDoesNotWriteTheRecord(t *testing.T) {
	bin := fakeBinary()
	f := &fakeRepo{tag: "v9.9.9", binary: bin, sumsBody: sha256Hex(bin) + "  " + PlatformAssetName() + "\n"}
	f.start(t)
	checkDir(t)
	withTarget(t, []byte("old binary"))

	code, _, errw := runUpdate(t, nil, "v1.0.0")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	if rec := ReadCheck(); !rec.CheckedAt.IsZero() {
		t.Errorf("an install wrote a check record: %+v", rec)
	}
}

func TestUpdateTimeoutBoundsTheRun(t *testing.T) {
	f := &fakeRepo{tag: "v9.9.9", binary: fakeBinary(), sumsBody: "x"}
	f.start(t)
	checkDir(t)
	withTarget(t, []byte("old binary"))
	start := time.Now()
	// An already-expired deadline must fail fast rather than hang for the
	// 10-minute default the interactive path uses. 0 is deterministic where a
	// 1ms budget would race a loopback server and sometimes win.
	code, _, errw := runUpdate(t, []string{"--check", "--timeout", "0"}, "v1.0.0")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %s)", code, errw)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("--timeout 0 took %v", elapsed)
	}
	if !strings.Contains(errw, "context deadline exceeded") && !strings.Contains(errw, "timeout") {
		t.Errorf("stderr does not name the timeout:\n%s", errw)
	}
}
