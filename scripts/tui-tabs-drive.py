#!/usr/bin/env python3
"""Drive the real xdev TUI in a pty and print the frames pyte reconstructs.

    scripts/tui-tabs-drive.py ./xdev --keys leader-n,leader-n,ctrl-1,ctrl-3

Why this exists: a full-screen TUI repaints every dirty cell each frame, so
"the string is somewhere in the capture" is repaint debris, not evidence of
what is on screen. pyte replays the raw byte stream into the cell grid, so the
pictures printed here are the ones a human would have been looking at.

The child gets an isolated XDEV_AGENT_DIR with a models.yml naming a local
mock provider, so the TUI opens (a model has to resolve) and no turn needs a
gateway.

Reading the output: the tab strip underlines the CURRENT tab (drawTabStrip:
"Underline rather than invert"), so the `^` row under the strip IS the answer
to "which tab is focused" — the titles are all `print <timestamp>`, so plain
text cannot tell two same-second tabs apart. Use --gap to keep the titles
distinct anyway.

Requires pyte: python3 -m venv /tmp/pyte-venv && /tmp/pyte-venv/bin/pip install pyte
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

COLS, ROWS = 110, 30

# kitty CSI-u — the encoding tcell's own csiUKeys table decodes. A bare
# 0x01..0x09 would collide with Ctrl-A..Ctrl-I, and modifyOtherKeys
# (CSI 27 ; mods ; keycode ~) is only decoded for the keys it knows, so CSI-u
# is what a Ctrl+digit / Ctrl+Tab actually arrives as on a CSI-u terminal.
KEYS = {
    "ctrl-tab": "\x1b[9;5u",
    "ctrl-shift-tab": "\x1b[9;6u",
    "ctrl-1": "\x1b[49;5u",
    "ctrl-2": "\x1b[50;5u",
    "ctrl-3": "\x1b[51;5u",
    "leader-1": "\x18\x1b[49;5u",
    "leader-2": "\x18\x1b[50;5u",
    "leader-n": "\x18n",
    "enter": "\r",
    "esc": "\x1b",
}


def snapshot(screen, cols, rows):
    """A frozen copy of the cell grid: [(data, underline), ...] per row.

    pyte mutates its rows in place, so a frame captured by reference is every
    frame showing the LAST state — which reads as "nothing moved".
    """
    return [
        [(screen.buffer[y][x].data, screen.buffer[y][x].underscore) for x in range(cols)]
        for y in range(rows)
    ]


def render(buf, rows=8):
    """The chrome rows, with the strip's underline extent marked beneath."""
    out = []
    for y in range(min(rows, len(buf))):
        line = "".join(data for data, _ in buf[y]).rstrip()
        if not line.strip():
            continue
        under = "".join("^" if u else " " for _, u in buf[y])[: len(line)].rstrip()
        out.append("r%d| %s" % (y, line))
        if under.strip():
            out.append("  | %s" % under)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--keys", default="", help="comma-separated: " + ", ".join(KEYS))
    ap.add_argument("--label", default="")
    ap.add_argument("--settle", type=float, default=2.0, help="seconds to read after each key")
    ap.add_argument(
        "--gap",
        type=float,
        default=0.0,
        help="extra seconds before each key: the /new title stamp is a timestamp, "
        "so two tabs opened in the same second are indistinguishable on screen",
    )
    ap.add_argument("--raw", default="", help="write the raw pty capture here")
    ap.add_argument("--full", action="store_true", help="print the whole final frame")
    args = ap.parse_args()

    agent_dir = "/tmp/xdev-tabs-drive/agent"
    os.makedirs(agent_dir, exist_ok=True)
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
    raw = open(args.raw, "wb") if args.raw else None

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
            if raw:
                raw.write(data)
            stream.feed(data.decode("utf-8", "replace"))

    try:
        pump(args.settle)
        frames = []

        for key in [k for k in args.keys.split(",") if k]:
            if args.gap:
                time.sleep(args.gap)
            os.write(fd, KEYS[key].encode())
            pump(args.settle)
            frames.append((key, snapshot(screen, COLS, ROWS)))

        if args.full:
            print("=== final frame (%s) ===" % (args.label or args.binary))
            print("\n".join(line.rstrip() for line in screen.display))
        else:
            print("=== chrome after each key (%s) ===" % (args.label or args.binary))
            for key, buf in frames:
                print("\n--- after %s ---" % key)
                print("\n".join(render(buf)))
    finally:
        if raw:
            raw.close()
        try:
            os.write(fd, b"\x18\x11")  # C-x C-q: leader quit
            time.sleep(0.4)
        except OSError:
            pass
        try:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        except (ProcessLookupError, ChildProcessError):
            pass


if __name__ == "__main__":
    main()
