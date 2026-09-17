package dist

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// ResolveTarget follows symlinks so a Homebrew-style link
// (~/.local/bin/xdev -> ../Cellar/.../xdev) is replaced at its target
// rather than turned into a regular file that shadows the managed one.
func ResolveTarget(exe string) (string, error) {
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", fmt.Errorf("locate the running binary: %w", err)
		}
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A deleted-but-running binary still renames fine; only report the
		// failure when the path is genuinely unusable.
		if _, statErr := os.Stat(exe); statErr != nil {
			return "", fmt.Errorf("resolve %s: %w", exe, err)
		}
		return exe, nil
	}
	return resolved, nil
}

// PlatformAssetName is the release asset for the running platform, matching
// the M8 release job's naming (xdev_<goos>_<goarch>[.exe]) so a release
// built by CI is installable without a manifest lookup table.
func PlatformAssetName() string {
	return PlatformAssetNameFor(runtime.GOOS, runtime.GOARCH)
}

// PlatformAssetNameFor is PlatformAssetName for an explicit platform (the
// update test drives the darwin/linux/windows naming through it).
func PlatformAssetNameFor(goos, goarch string) string {
	name := "xdev_" + goos + "_" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// PlatformName is the "<goos>_<goarch>" pair used in per-platform release
// manifest names (SHA256SUMS_darwin_arm64.txt).
func PlatformName() string { return runtime.GOOS + "_" + runtime.GOARCH }

// InstallBinary verifies and installs a release asset over target:
//
//   - the target must be writable by this user, checked by mode bit (so the
//     check holds under a root CI runner too, where an open() would lie);
//   - the asset streams to a temp file next to the target while hashing;
//   - the hash must equal wantSHA (a mismatch removes the temp and aborts,
//     so a tampered or truncated download never reaches the install path);
//   - a rename puts it in place: atomic on POSIX, so an interrupted update
//     never leaves a half-written binary.
//
// download writes the asset bytes; it is a func rather than a []byte so a
// 15 MB binary never has to be resident in an agent that budgets its RSS.
func InstallBinary(target string, download func(w io.Writer) error, wantSHA string) error {
	if err := checkWritable(target); err != nil {
		return err
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".xdev-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w\n  hint: chmod u+w %s   (or install into a user-owned directory such as ~/.local/bin)",
			target, err, dir)
	}
	tmpPath := tmp.Name()
	// After a successful rename this path no longer exists, so the cleanup
	// is a no-op then; on every failure path it discards the partial file.
	defer os.Remove(tmpPath)

	h := sha256.New()
	if err := download(io.MultiWriter(tmp, h)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("checksum mismatch for %s:\n  want %s\n  got  %s\nrefusing to install; the download was discarded",
			filepath.Base(target), wantSHA, got)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	return replace(tmpPath, target)
}

// checkWritable reports the actionable reason a binary cannot be replaced
// in place, with the exact chmod hint the operator needs.
func checkWritable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing to replace: temp+rename creates it, which only works
			// if the directory is writable (reported there).
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory, not a binary", path)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("%s is not writable by this user\n  hint: chmod u+w %s\n  (an install owned by another user needs `sudo xdev update`, or a user-owned prefix such as ~/.local/bin)",
			path, path)
	}
	return nil
}

// replace renames tmp over target. Windows cannot rename onto a running
// executable, so the fallback moves the target aside first and keeps it as
// .old for recovery if the second rename fails.
func replace(tmp, target string) error {
	firstErr := os.Rename(tmp, target)
	if firstErr == nil {
		return nil
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("install %s: %w", target, firstErr)
	}
	old := target + ".old"
	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("install %s: %w", target, firstErr)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("install %s: %w (previous binary kept at %s)", target, err, old)
	}
	os.Remove(old) // best effort; a locked .old is harmless
	return nil
}
