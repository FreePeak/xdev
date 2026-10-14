//go:build windows

package chromeext

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func nativeManifestPath() string {
	// On Windows the path lives next to the data-dir copy; the registry points at it.
	// Probe uses this as "is the registry key present?" via a side file we always write.
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "AppData", "Local", "xdev", hostManifestName)
}

func registerNativeHost(hostMan string) (browsers []string, primary string, err error) {
	// Chromium looks up HKCU\Software\Google\Chrome\NativeMessagingHosts\<name>
	// (and Edge/Brave equivalents) for a default value = absolute path to the JSON.
	keys := []struct{ name, key string }{
		{"Chrome", `HKCU\Software\Google\Chrome\NativeMessagingHosts\` + NativeHostName},
		{"Edge", `HKCU\Software\Microsoft\Edge\NativeMessagingHosts\` + NativeHostName},
		{"Brave", `HKCU\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\` + NativeHostName},
	}
	abs, err := filepath.Abs(hostMan)
	if err != nil {
		return nil, "", err
	}
	for _, k := range keys {
		cmd := exec.Command("reg", "add", k.key, "/ve", "/t", "REG_SZ", "/d", abs, "/f")
		if out, err := cmd.CombinedOutput(); err != nil {
			// Best-effort: missing browser hives are fine.
			_ = out
			continue
		}
		browsers = append(browsers, k.name)
	}
	if len(browsers) == 0 {
		return nil, "", fmt.Errorf("chromeext: reg add failed for every Chromium browser hive")
	}
	return browsers, abs, nil
}

func unregisterNativeHost() error {
	keys := []string{
		`HKCU\Software\Google\Chrome\NativeMessagingHosts\` + NativeHostName,
		`HKCU\Software\Microsoft\Edge\NativeMessagingHosts\` + NativeHostName,
		`HKCU\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\` + NativeHostName,
	}
	var last error
	for _, k := range keys {
		cmd := exec.Command("reg", "delete", k, "/f")
		if err := cmd.Run(); err != nil {
			last = err
		}
	}
	return last
}
