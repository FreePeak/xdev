#!/usr/bin/env python3
"""Drive the real xdev TUI and MEASURE whether an overlay respects the sidebar.

The question is geometric — with the context panel open, does the "/" dropdown
(or the ledger, the roster, the pickers) paint into the panel's own columns —
so the answer is read off the CELL GRID, not the text stream. Two facts make
the naive version of this useless, and both are handled here:

  * a full-screen TUI repaints every dirty cell, so "the command name is in the
    capture" proves nothing: the text is in the capture either way. The capture
    is replayed through pyte (a terminal emulator) and the resulting grid is
    measured.
  * GLYPHS are not the defect. A full-width overlay mostly leaves a BACKGROUND
    fill behind in the panel's columns (its own rows are short, so no glyph
    lands there) — which is exactly how the panel visually disappears under the
    dropdown while a glyph-counting probe reports "clean". So the score counts
    cells with a non-default background too.

Setup that matters (each cost a wrong-looking result first):
  * tcell opens /dev/tty, so the child needs its own session AND the pty slave
    as its CONTROLLING terminal (setsid + TIOCSCTTY). A plain pty.fork() gives
    "open /dev/tty: device not configured" and a blank frame — indistinguishable
    from a broken binary.
  * the panel needs a transcript (dockOn requires blocks), so every probe sends
    one prompt first. The mock provider never answers, which is fine.
  * one probe per process, hard-exit when the grid is scored: a child that has
    not reaped holds the run open past any timeout and the capture is lost.

Usage:
    /tmp/pyte-venv/bin/python scripts/tui-overlay-width-drive.py BIN [--cols 160]
"""
import argparse
import fcntl
import os
import pty
import select
import shutil
import signal
import struct
import sys
import termios
import time

DOCK_COLS = 42
BOX = set("│─┌┐└┘╭╮╰╯├┤┬┴┼")


def spawn(binary, agent_dir, cols, rows):
    """Start the TUI on a pty it CONTROLS; return (pid, master_fd)."""
    shutil.rmtree(agent_dir, ignore_errors=True)
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
    # sidebarMode: show pins the panel open at any width, so the measurement is
    # the same on a narrow terminal as on a wide one.
    with open(os.path.join(agent_dir, "config.yml"), "w") as f:
        f.write("sidebarMode: show\n")

    env = dict(os.environ)
    env["XDEV_AGENT_DIR"] = agent_dir
    env["TERM"] = "xterm-256color"
    env["COLORTERM"] = "truecolor"
    env.pop("NO_COLOR", None)

    master, slave = pty.openpty()
    pid = os.fork()
    if pid == 0:
        os.setsid()
        fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
        os.dup2(slave, 0)
        os.dup2(slave, 1)
        os.dup2(slave, 2)
        if slave > 2:
            os.close(slave)
        os.close(master)
        os.execve(binary, [binary, "tui", "-m", "mock/mock-1"], env)
        os._exit(127)
    os.close(slave)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    return pid, master


def capture(binary, agent_dir, keys, cols, rows, settle=1.4):
    """Type keys into a live TUI and return the raw pty bytes."""
    pid, fd = spawn(binary, agent_dir, cols, rows)
    buf = bytearray()

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
            buf.extend(data)

    try:
        pump(2.5)
        for k in keys:
            os.write(fd, k.encode())
            pump(settle)
    finally:
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    return bytes(buf)


def score(raw, cols, rows, print_grid=False):
    """Replay the capture into a fresh screen and count what landed in the panel."""
    import pyte

    scr = pyte.Screen(cols, rows)
    pyte.Stream(scr).feed(raw.decode("utf-8", "replace"))
    edge = cols - DOCK_COLS
    glyphs = fill = ink = 0
    fill_rows = []
    for y in range(rows):
        glyphs += sum(1 for ch in scr.display[y][edge:] if ch in BOX)
        fill += sum(1 for x in range(edge, cols) if scr.buffer[y][x].bg != "default")
        ink += sum(1 for ch in scr.display[y][edge:] if ch not in " " and ch not in BOX)
        if any(scr.buffer[y][x].bg != "default" for x in range(edge, cols)):
            fill_rows.append(y)
        if print_grid:
            print("r%02d|%s|" % (y, scr.display[y]), flush=True)
    return glyphs, fill, ink, fill_rows


# One prompt first: the panel is only drawn once the session has a block.
PRELUDE = ["hello there\r"]
PROBES = [
    ("none", []),
    ("slash", ["/"]),
    ("slash-filtered", ["/t"]),
    ("trajectory", ["/trajectory"]),
    ("hub", ["/hub"]),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--cols", type=int, default=160)
    ap.add_argument("--rows", type=int, default=41)
    ap.add_argument("--agent-dir", default="/tmp/xdev-overlay-width/agent")
    ap.add_argument("--out-dir", default="/tmp/xdev-overlay-width")
    ap.add_argument("--print-grid", action="store_true")
    args = ap.parse_args()

    name = os.path.basename(args.binary)
    failures = 0
    for label, keys in PROBES:
        raw = capture(args.binary, os.path.join(args.agent_dir, label), PRELUDE + keys,
                      args.cols, args.rows)
        path = os.path.join(args.out_dir, "raw-%s-%s.bin" % (name, label))
        os.makedirs(args.out_dir, exist_ok=True)
        with open(path, "wb") as f:
            f.write(raw)
        glyphs, fill, ink, fill_rows = score(raw, args.cols, args.rows, args.print_grid)
        print("%-9s %-14s panel glyphs=%-3d fill=%-4d panel-ink=%-4d fill-rows=%s  (%s)"
              % (name, label, glyphs, fill, ink, fill_rows[:6], path), flush=True)
        # The panel is the surface the user reads: no overlay glyph, and no
        # overlay fill, may reach its columns. Its own ink must still be there.
        if glyphs or fill:
            print("   !! %s/%s painted into the panel's columns" % (name, label))
            failures += 1
        if not ink:
            print("   !! %s/%s left the panel blank — something covered it" % (name, label))
            failures += 1
    print("\n%d probe(s) with a geometry failure" % failures, flush=True)
    sys.stdout.flush()
    # The child was killed, not reaped: exit hard so a zombie cannot hold the run.
    os._exit(1 if failures else 0)


if __name__ == "__main__":
    main()