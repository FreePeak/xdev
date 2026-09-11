package tui

import (
	"os/exec"
	"runtime"
)

// copyToClipboard puts s on the system clipboard.
//
// Two layers, both the same text: tcell's OSC 52 path (Screen.SetClipboard
// — works over SSH and in modern terminals such as ghostty, kitty, iTerm2,
// WezTerm; the simulation screen records it, so tests can assert it) and a
// best-effort native utility for the local case (Terminal.app ignores
// OSC 52). The last successful writer wins.
func (a *App) copyToClipboard(s string) error {
	a.scr.SetClipboard([]byte(s))
	return nativeCopy(s)
}

// nativeCopy shells out to the platform clipboard utility, if present.
func nativeCopy(s string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		switch {
		case has("wl-copy"):
			cmd = exec.Command("wl-copy")
		case has("xclip"):
			cmd = exec.Command("xclip", "-selection", "clipboard")
		case has("xsel"):
			cmd = exec.Command("xsel", "--clipboard", "--input")
		}
	case "windows":
		cmd = exec.Command("clip")
	}
	if cmd == nil {
		return nil // SetClipboard already tried; nothing more to do.
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_, _ = in.Write([]byte(s))
	_ = in.Close()
	return cmd.Wait()
}

func has(bin string) bool { _, err := exec.LookPath(bin); return err == nil }
