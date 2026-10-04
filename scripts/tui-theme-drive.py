#!/usr/bin/env python3
"""Drive /theme in a real pty and report the rendered screen per palette.

Same pyte approach as tui-settings-drive.py (a full-screen TUI repaints every
dirty cell, so "the string is in the capture" proves nothing). Prints the frame
the user would actually be looking at after each `/theme <name>`, plus the
config the picker wrote, so a palette is verified as painted AND as persisted.
"""
import argparse
import fcntl
import json
import os
import pty
import select
import signal
import struct
import termios
import time

COLS, ROWS = 110, 34
ENTER = "\r"


def snapshot(screen, cols, rows):
    return [[screen.buffer[y][x].data for x in range(cols)] for y in range(rows)]


def render(buf, rows=ROWS):
    out = []
    for y in range(min(rows, len(buf))):
        line = "".join(buf[y]).rstrip()
        if line.strip():
            out.append("r%d| %s" % (y, line))
    return out


def cellcolors(screen, cols, rows, probes):
    """Report the SGR colours at named cells, so "the frame looks the same" is
    never mistaken for "the palette did not apply"."""
    out = {}
    for label, (x, y) in probes.items():
        if y >= rows or x >= cols:
            continue
        c = screen.buffer[y][x]
        out[label] = "%s on %s (%s)" % (c.fg, c.bg, c.data.strip() or "space")
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--themes", default="tokyo-night,nord,one-light,catppuccin")
    ap.add_argument("--settle", type=float, default=1.4)
    ap.add_argument("--agent-dir", default="/tmp/xdev-theme-drive/agent")
    args = ap.parse_args()

    agent_dir = args.agent_dir
    os.makedirs(agent_dir, exist_ok=True)
    cfg = os.path.join(agent_dir, "config.yml")
    if os.path.exists(cfg):
        os.remove(cfg)
    with open(os.path.join(agent_dir, "models.yml"), "w") as f:
        f.write(
            "providers:\n"
            "  mock:\n"
            "    baseUrl: http://127.0.0.1:9/v1\n"
            "    apiKey: mock\n"
            "    api: openai-completions\n"
            "    models:\n"
            "      - id: mock-1\n"
            "        name: mock\n"
        )
    env = dict(os.environ)
    env["XDEV_AGENT_DIR"] = agent_dir
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

    def config_theme():
        try:
            with open(cfg) as f:
                for line in f:
                    if line.startswith("theme:"):
                        return line.strip()
        except FileNotFoundError:
            pass
        return "<no theme in config.yml>"

    def send(text):
        os.write(fd, text.encode())
        pump(args.settle)

    try:
        pump(args.settle)
        print("=== startup frame (auto) ===")
        print("\n".join(render(snapshot(screen, COLS, ROWS))))

        # /theme list first: the picker must offer every shipped palette.
        send("/theme list" + ENTER)
        print("\n=== /theme list ===")
        print("\n".join(render(snapshot(screen, COLS, ROWS))))

        # Probe named cells after every switch: the text of a frame is
        # identical whichever palette is loaded, so only the SGR at a cell
        # proves the theme actually applied (and what it applied).
        probes = {
            "composer border": (3, ROWS - 4),
            "placeholder": (6, ROWS - 3),
            "caption model": (7, ROWS - 2),
            "status path": (4, ROWS - 1),
            "aside slash": (2, 19),
        }
        for name in [t for t in args.themes.split(",") if t]:
            send("/theme %s%s" % (name, ENTER))
            print("\n=== /theme %s ===" % name)
            print("config: %s" % config_theme())
            for label, at in cellcolors(screen, COLS, ROWS, probes).items():
                print("cell %-16s %s" % (label, at))
            print("\n".join(render(snapshot(screen, COLS, ROWS))))

        # A typo must be refused, not silently applied.
        before = config_theme()
        send("/theme no-such-palette" + ENTER)
        print("\n=== /theme no-such-palette ===")
        print("config unchanged: %s (%s)" % (config_theme(), before))
        print("\n".join(render(snapshot(screen, COLS, ROWS))))
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
