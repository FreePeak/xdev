#!/usr/bin/env python3
"""Drive the tab-strip policy in a real pty: open three sessions, read the
strip, then flip tui.tabs.mode / tui.tabs.indicators from the Alt+, panel and
read the strip and the settings file again.

Same pyte approach as tui-tabs-drive.py: a full-screen TUI repaints every
dirty cell each frame, so "the string is in the capture" proves nothing about
what is on screen. The strip's underline marks the CURRENT tab, and the badge
column is what tui.tabs.indicators selects, so both are read off the cell grid.
"""
import argparse
import fcntl
import os
import pty
import select
import signal
import struct
import termios
import time

COLS, ROWS = 110, 34
KEYS = {
    "alt-comma": "\x1b,",
    "down": "\x1b[B",
    "enter": "\r",
    "esc": "\x1b",
    "leader-n": "\x18n",  # open a new session tab
    "home": "g",  # the panel's "jump to the first row" (its selection persists
    #                between openings, so every panel visit starts from g)
}


def cell_rows(screen):
    """Rows 0..1 of the grid as (text, underline-mask) — the strip's two facts."""
    out = []
    for y in range(2):
        text = "".join(screen.buffer[y][x].data for x in range(COLS)).rstrip()
        under = "".join("^" if screen.buffer[y][x].underscore else " " for x in range(COLS))
        out.append((text, under[: len(text)].rstrip() if text else ""))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--tabs", type=int, default=3)
    ap.add_argument("--settle", type=float, default=1.0)
    ap.add_argument("--agent-dir", default="/tmp/xdev-tabpolicy-drive/agent")
    ap.add_argument("--label", default="")
    args = ap.parse_args()

    os.makedirs(args.agent_dir, exist_ok=True)
    cfg = os.path.join(args.agent_dir, "config.yml")
    if os.path.exists(cfg):
        os.remove(cfg)
    with open(os.path.join(args.agent_dir, "models.yml"), "w") as f:
        f.write(
            "providers:\n  mock:\n    baseUrl: http://127.0.0.1:9/v1\n    apiKey: mock\n"
            "    api: openai-completions\n    models:\n      - id: mock-1\n        name: mock\n"
        )
    env = dict(os.environ)
    env["XDEV_AGENT_DIR"] = args.agent_dir
    env["TERM"] = "xterm-256color"
    env["COLORTERM"] = "truecolor"
    env.pop("NO_COLOR", None)

    pid, fd = pty.fork()
    if pid == 0:
        os.execve(args.binary, [args.binary, "tui", "-m", "mock/mock-1"], env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    import pyte

    screen = pyte.Screen(COLS, ROWS)
    stream = pyte.Stream(screen)

    def pump(seconds):
        end = time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.05)
            if not r:
                continue
            try:
                data = os.read(fd, 65536)
            except OSError:
                return
            if not data:
                return
            stream.feed(data.decode("utf-8", "replace"))

    def press(key, settle=None):
        os.write(fd, KEYS[key].encode())
        pump(args.settle if settle is None else settle)

    def config_text():
        try:
            with open(cfg) as f:
                return " | ".join(l.strip() for l in f if l.strip()) or "(empty)"
        except FileNotFoundError:
            return "(no config.yml yet)"

    def report(tag):
        print("--- %s ---" % tag)
        for text, under in cell_rows(screen):
            if text:
                print("   strip: %s" % text)
            if under.strip():
                print("   under: %s" % under)
        print("   cfg:   %s" % config_text())

    def panel_row(label, downs):
        """Alt+, then walk to the named row: the panel scrolls, so name-check."""
        press("alt-comma")
        press("home")
        for _ in range(downs):
            press("down", 0.2)
        line = [screen.display[y].rstrip() for y in range(ROWS) if label in screen.display[y]]
        press("esc", 0.3)
        return line[0] if line else "(row %r not on screen after %d downs)" % (label, downs)

    try:
        pump(2.0)
        print("=== %s ===" % (args.label or args.binary))
        for _ in range(args.tabs):
            press("leader-n")
        report("%d tabs open (shipped defaults)" % args.tabs)

        # tui.tabs.indicators -> numbers (row 6 in the ALL view).
        row = panel_row("Tab badges", 6)
        print("   panel: %s" % row)
        press("alt-comma")
        press("home")
        for _ in range(6):
            press("down", 0.2)
        press("enter")
        press("esc")
        report("after Tab badges -> numbers")

        # tui.tabs.mode -> off (row 5): the strip goes, the chords stay.
        press("alt-comma")
        press("home")
        for _ in range(5):
            press("down", 0.2)
        press("enter")  # auto -> on
        press("esc")
        report("after Session tabs -> on")
        press("alt-comma")
        press("home")
        for _ in range(5):
            press("down", 0.2)
        press("enter")  # on -> off
        press("esc")
        report("after Session tabs -> off (strip hidden, chords still live)")
        # C-1 must still switch with the strip hidden.
        os.write(fd, b"\x1b[49;5u")
        pump(args.settle)
        report("after C-1 with the strip off")
    finally:
        try:
            os.write(fd, b"\x18\x11")
            time.sleep(0.3)
        except OSError:
            pass
        try:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        except (ProcessLookupError, ChildProcessError):
            pass


if __name__ == "__main__":
    main()
