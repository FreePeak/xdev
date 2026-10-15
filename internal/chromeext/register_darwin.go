//go:build darwin

package chromeext

import (
	"fmt"
	"os"
	"path/filepath"
)

// nativeMessagingDirs are the Chromium host-manifest locations on macOS.
func nativeMessagingDirs() []struct{ name, dir string } {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []struct{ name, dir string }{
		{"Chrome", filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts")},
		{"Chrome Beta", filepath.Join(home, "Library", "Application Support", "Google", "Chrome Beta", "NativeMessagingHosts")},
		{"Chrome Canary", filepath.Join(home, "Library", "Application Support", "Google", "Chrome Canary", "NativeMessagingHosts")},
		{"Chromium", filepath.Join(home, "Library", "Application Support", "Chromium", "NativeMessagingHosts")},
		{"Edge", filepath.Join(home, "Library", "Application Support", "Microsoft Edge", "NativeMessagingHosts")},
		{"Brave", filepath.Join(home, "Library", "Application Support", "BraveSoftware", "Brave-Browser", "NativeMessagingHosts")},
	}
}

func nativeManifestPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts", hostManifestName)
}

func registerNativeHost(hostMan string) (browsers []string, primary string, err error) {
	raw, err := os.ReadFile(hostMan)
	if err != nil {
		return nil, "", err
	}
	var first string
	for _, b := range nativeMessagingDirs() {
		if err := os.MkdirAll(b.dir, 0o755); err != nil {
			continue
		}
		dst := filepath.Join(b.dir, hostManifestName)
		if err := os.WriteFile(dst, raw, 0o644); err != nil {
			return browsers, first, fmt.Errorf("chromeext: write %s: %w", dst, err)
		}
		browsers = append(browsers, b.name)
		if first == "" {
			first = dst
		}
	}
	if first == "" {
		return nil, "", fmt.Errorf("chromeext: no Chromium NativeMessagingHosts directory could be written")
	}
	return browsers, first, nil
}

func unregisterNativeHost() error {
	var last error
	for _, b := range nativeMessagingDirs() {
		path := filepath.Join(b.dir, hostManifestName)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			last = err
		}
	}
	return last
}
