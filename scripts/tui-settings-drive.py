#!/usr/bin/env python3
"""Drive the settings overlay in a real pty and report what the panel did.

Same pyte approach as tui-tabs-drive.py (a full-screen TUI repaints every dirty
cell, so "the string is in the capture" proves nothing). On top of it this
prints the settings file after each key, because the user's complaint is that
nothing PERSISTS — the panel can repaint correctly and still not write.
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
    "enter": "\r",
    "up": "\x1b[A",
    "down": "\x1b[B",
    "tab": "\t",
    "esc": "\x1b",
    "r": "r",
    "q": "q",
    "slash-settings": "/settings\x0d",
    "slash-settings-overlay": "/settings overlay\x0d",
}


def snapshot(screen, cols, rows):
    return [
        [(screen.buffer[y][x].data, screen.buffer[y][x].underscore) for x in range(cols)]
        for y in range(rows)
    ]


def render(buf, rows=ROWS):
    out = []
    for y in range(min(rows, len(buf))):
        line = "".join(d for d, _ in buf[y]).rstrip()
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
    ap.add_argument("--keys", default="alt-comma")
    ap.add_argument("--label", default="")
    ap.add_argument("--settle", type=float, default=1.2)
    ap.add_argument("--agent-dir", default="/tmp/xdev-settings-drive/agent")
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

    def config_text():
        try:
            with open(cfg) as f:
                return f.read().strip()
        except FileNotFoundError:
            return "<no config.yml yet>"

    try:
        pump(args.settle)
        print("=== %s ===" % (args.label or args.binary))
        print("--- before any key ---")
        print("config: %s" % config_text().replace("\n", " | "))
        print("\n".join(render(snapshot(screen, COLS, ROWS))))
        for key in [k for k in args.keys.split("+") if k]:
            os.write(fd, KEYS[key].encode())
            pump(args.settle)
            print("\n--- after %s ---" % key)
            print("config: %s" % config_text().replace("\n", " | "))
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