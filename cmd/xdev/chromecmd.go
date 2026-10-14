package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/FreePeak/xdev/internal/chromeext"
	"github.com/FreePeak/xdev/internal/config"
)

const chromeUsage = `xdev chrome — drive your real Chrome profile from the browser tool

  xdev chrome install     copy the extension, register the native host
  xdev chrome uninstall   remove native-host registration + data-dir copy
  xdev chrome status      show install state
  xdev chrome host        run the native host in the foreground (Chrome launches this)

After install:
  1. Open chrome://extensions
  2. Enable Developer mode
  3. Load unpacked → the path printed by install (stable id ` + chromeext.ExtensionID + `)
  4. Point the agent at the bridge:

       browser:
         cdpUrl: http://` + chromeext.DefaultListen + `
         autolaunch: false

Or set $XDEV_BROWSER_CDP_URL=http://` + chromeext.DefaultListen + `.
The extension connects to the native host automatically; the host exposes a
CDP-compatible endpoint the existing browser tool already speaks.
`

func runChrome(args []string) int {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "install":
		return chromeInstall(args)
	case "uninstall":
		return chromeUninstall(args)
	case "status":
		return chromeStatus(args)
	case "host":
		return chromeHost(args)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, chromeUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "xdev chrome: unknown subcommand %q\n\n%s", sub, chromeUsage)
		return 2
	}
}

func chromeInstall(args []string) int {
	fs := flag.NewFlagSet("chrome install", flag.ContinueOnError)
	src := fs.String("extension", "", "unpacked extension directory (default: repo extensions/chrome next to the binary, or the embedded install path)")
	listen := fs.String("listen", chromeext.DefaultListen, "CDP listen address the host will bind")
	fs.Usage = func() { fmt.Fprint(os.Stderr, "usage: xdev chrome install [--extension DIR] [--listen ADDR]\n") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xdev chrome install: executable: %v\n", err)
		return 1
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xdev chrome install: eval symlinks: %v\n", err)
		return 1
	}
	extSrc := strings.TrimSpace(*src)
	if extSrc == "" {
		extSrc = findExtensionSource(bin)
	}
	if extSrc == "" {
		fmt.Fprintln(os.Stderr, "xdev chrome install: could not find extensions/chrome; pass --extension DIR")
		return 1
	}
	res, err := chromeext.Install(chromeext.InstallOptions{
		DataDir:         config.DataDir(),
		XdevBin:         bin,
		ExtensionSource: extSrc,
		Listen:          *listen,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "xdev chrome install: %v\n", err)
		return 1
	}
	fmt.Printf("xdev chrome installed\n")
	fmt.Printf("  extension:      %s\n", res.ExtensionDir)
	fmt.Printf("  extension id:   %s\n", res.ExtensionID)
	fmt.Printf("  native host:    %s\n", res.HostWrapper)
	fmt.Printf("  host manifest:  %s\n", res.NativeManifest)
	fmt.Printf("  browsers:       %s\n", strings.Join(res.Browsers, ", "))
	fmt.Printf("  CDP listen:     http://%s\n", res.Listen)
	fmt.Printf("\nNext:\n")
	fmt.Printf("  1. Open chrome://extensions\n")
	fmt.Printf("  2. Enable Developer mode\n")
	fmt.Printf("  3. Load unpacked → %s\n", res.ExtensionDir)
	fmt.Printf("  4. Set browser.cdpUrl: http://%s  (and browser.autolaunch: false)\n", res.Listen)
	// Best-effort open the extensions page.
	_ = openURL("chrome://extensions")
	return 0
}

func chromeUninstall(args []string) int {
	fs := flag.NewFlagSet("chrome uninstall", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, "usage: xdev chrome uninstall\n") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := chromeext.Uninstall(config.DataDir()); err != nil {
		fmt.Fprintf(os.Stderr, "xdev chrome uninstall: %v\n", err)
		return 1
	}
	fmt.Println("xdev chrome: native host unregistered; remove the unpacked extension in chrome://extensions if still loaded")
	return 0
}

func chromeStatus(args []string) int {
	fs := flag.NewFlagSet("chrome status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { fmt.Fprint(os.Stderr, "usage: xdev chrome status [--json]\n") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st := chromeext.Probe(config.DataDir())
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
		return 0
	}
	fmt.Printf("extension id:   %s\n", st.ExtensionID)
	fmt.Printf("native host:    %s\n", st.NativeHostName)
	fmt.Printf("CDP listen:     http://%s\n", st.Listen)
	fmt.Printf("extension dir:  %s (%s)\n", st.ExtensionDir, okStr(st.ExtensionOK))
	fmt.Printf("host wrapper:   %s (%s)\n", st.HostWrapper, okStr(st.HostWrapperOK))
	fmt.Printf("native manifest:%s (%s)\n", st.NativeManifest, okStr(st.NativeOK))
	if st.ExtensionOK && st.HostWrapperOK && st.NativeOK {
		fmt.Println("status:         installed — load the unpacked extension if Chrome does not show it yet")
		return 0
	}
	fmt.Println("status:         incomplete — run: xdev chrome install")
	return 1
}

func chromeHost(args []string) int {
	fs := flag.NewFlagSet("chrome host", flag.ContinueOnError)
	listen := fs.String("listen", envOr("XDEV_CHROME_LISTEN", chromeext.DefaultListen), "CDP listen address")
	fs.Usage = func() { fmt.Fprint(os.Stderr, "usage: xdev chrome host [--listen ADDR]\n") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := chromeext.RunNativeHost(ctx, *listen); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "xdev chrome host: %v\n", err)
		return 1
	}
	return 0
}

func okStr(ok bool) string {
	if ok {
		return "ok"
	}
	return "missing"
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// findExtensionSource walks from the binary and the working directory for
// extensions/chrome/manifest.json (dev tree or installed share layout).
func findExtensionSource(bin string) string {
	candidates := []string{
		filepath.Join(filepath.Dir(bin), "extensions", "chrome"),
		filepath.Join(filepath.Dir(bin), "..", "extensions", "chrome"),
		filepath.Join(filepath.Dir(bin), "..", "..", "extensions", "chrome"),
		filepath.Join(filepath.Dir(bin), "..", "..", "..", "extensions", "chrome"),
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, "extensions", "chrome"),
			filepath.Join(wd, "..", "extensions", "chrome"),
		)
	}
	// Worktree / repo root from source layout: cmd/xdev → repo root.
	candidates = append(candidates, filepath.Join(filepath.Dir(bin), "..", "..", "..", "..", "extensions", "chrome"))
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "manifest.json")); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}

func openURL(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
