#!/usr/bin/env python3
"""Drive the real xdev TUI and read the SIDEBAR's inks off the cell grid.

The unit test (`internal/tui/rosepine_test.go`) proves `drawDock` resolves the
palette slots; this proves the running binary does. The question is chromatic —
which SGR a sidebar row actually carries — so the answer is read off the CELL
GRID, not the text stream. A full-screen TUI repaints every dirty cell, so
"the row is in the capture" proves nothing: the text is in the capture either
way, and only the SGR at a cell is evidence.

Same pty harness as scripts/tui-overlay-width-drive.py, and the same traps it
documents, each of which reads as "the binary is broken":

  * tcell opens /dev/tty, so the child needs its own session AND the pty slave
    as its CONTROLLING terminal (setsid + TIOCSCTTY). A plain pty.fork() gives
    "open /dev/tty: device not configured" and a blank frame.
  * the panel only paints once the session has a block (`dockOn` requires a
    transcript), so one prompt is sent first. The mock provider never answers,
    which is fine — the panel is drawn from the block, not from a reply.
  * one probe per process, hard-exit when the grid is scored: a child that has
    not been reaped holds the run open past any timeout and the capture is lost.

Two traps specific to reading this panel:

  * `pyte`'s `screen.buffer[y]` is a sparse defaultdict keyed by column, so a
    row must be read as `buffer[y][x] for x in range(edge, cols)`. Iterating
    the slice directly hands back `str`, not `Cell`, and `c.data` then raises
    AttributeError — which reads as a broken binary rather than a broken probe.
  * the title slot falls back to the session's FIRST PROMPT when the session has
    no title (`dockTitle`), so the row above `SESSION · …` is the user's own
    text, not a section, and it is not asserted.

Usage:
    /tmp/pyte-venv/bin/python scripts/tui-sidebar-ink-drive.py BIN [--cols 160]

Exit status is 0 only when every expected ink was painted and no role row wore
a different one; a failure names the row and both colours. Run against a stock
binary it exits 1 with the pre-change greys, which is how the probe is known to
measure the change rather than the theme.
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

# Per launch theme: the row role -> the SGR the panel must have painted. These
# are PINNED LITERALS, never read back out of the theme — the same rule
# internal/tui/rosepine_test.go records, and for the same reason: a probe that
# asks the theme what it should have painted passes on the palette it replaced.
#
#   heading   a section heading            syntax_type      (rpFoam / dawnFoam)
#   comment   the identity footer's rows   syntax_comment   (rpMuted / dawnMuted)
#   button    the one action row           accent_user      (the neutral grey)
ROLES = {
    "groknight": {"heading": "9ccfd8", "comment": "6e6a86", "button": "c8c8c8"},
    "grokday": {"heading": "56949f", "comment": "9893a5", "button": "444444"},
}


def spawn(binary, agent_dir, cols, rows, theme, no_color):
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
    # sidebarMode: show pins the panel open at any width, so the probe measures
    # the same panel on a narrow terminal as on a wide one.
    with open(os.path.join(agent_dir, "config.yml"), "w") as f:
        f.write("sidebarMode: show\n")
        if theme:
            f.write("theme: %s\n" % theme)

    env = dict(os.environ)
    env["XDEV_AGENT_DIR"] = agent_dir
    env["TERM"] = "xterm-256color"
    env["COLORTERM"] = "truecolor"
    env.pop("NO_COLOR", None)
    if no_color:
        env["NO_COLOR"] = "1"

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


def capture(binary, agent_dir, keys, cols, rows, theme, no_color, settle=1.4):
    pid, fd = spawn(binary, agent_dir, cols, rows, theme, no_color)
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


def sidebar_rows(scr, cols, rows):
    """Every non-blank sidebar row as (y, text, fg-of-its-first-non-blank-cell)."""
    edge = cols - DOCK_COLS
    out = []
    for y in range(rows):
        # A sparse defaultdict keyed by column: read it cell by cell.
        cells = [scr.buffer[y][x] for x in range(edge, cols)]
        text = "".join(c.data for c in cells).rstrip()
        if not text.strip():
            continue
        first = next((c for c in cells if c.data.strip()), None)
        out.append((y, text.strip(), first.fg))
    return out


def role_of(text):
    """Which role a sidebar row plays, by the row's own text.

    A heading is a section name with its count appended ("TRAJECTORY · 3
    records"), which is the split `dockSplit` makes: the all-caps name before
    the " · ". The button row is the panel's own action (dockTrajID), and the
    identity footer is the build row.
    """
    name = text.split(" · ")[0].strip()
    if name.isupper() and name.replace(" ", "").isalpha():
        return "heading"
    if text.startswith("xdev "):
        return "comment"
    if "click to open" in text:
        return "button"
    return None


def check(label, rows, want, no_color):
    """Assert the row roles carry the expected inks. Returns the failure count."""
    failures = 0
    print("%s:" % label, flush=True)
    for y, text, fg in rows:
        role = role_of(text)
        mark = ""
        if no_color:
            # NO_COLOR means "no colour output at all": every sidebar cell must
            # resolve to the terminal default, which is the flat fallback the
            # renderer promises.
            if fg != "default":
                mark = "  !! want the terminal default"
                failures += 1
        elif role:
            if fg != want[role]:
                mark = "  !! want %s (%s)" % (want[role], role)
                failures += 1
        print("  r%02d fg=%-8s %s%s" % (y, fg, text, mark), flush=True)
    if not no_color:
        # A role that never appeared means the panel is not painting what this
        # probe claims to measure, which is a failure of the probe's premise.
        seen = {role_of(t) for _, t, _ in rows}
        for role in ("heading", "comment", "button"):
            if role not in seen:
                print("  !! no %s row on screen; nothing to prove" % role, flush=True)
                failures += 1
    return failures


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--cols", type=int, default=160)
    ap.add_argument("--rows", type=int, default=41)
    ap.add_argument("--agent-dir", default="/tmp/xdev-sidebar-ink/agent")
    args = ap.parse_args()

    import pyte

    # One prompt first: the panel is only drawn once the session has a block.
    prelude = ["hello there\r"]

    failures = 0
    for theme in ("groknight", "grokday"):
        raw = capture(args.binary, os.path.join(args.agent_dir, theme), prelude,
                      args.cols, args.rows, theme, False)
        scr = pyte.Screen(args.cols, args.rows)
        pyte.Stream(scr).feed(raw.decode("utf-8", "replace"))
        failures += check(theme, sidebar_rows(scr, args.cols, args.rows),
                          ROLES[theme], False)

    raw = capture(args.binary, os.path.join(args.agent_dir, "nocolor"), prelude,
                  args.cols, args.rows, "groknight", True)
    scr = pyte.Screen(args.cols, args.rows)
    pyte.Stream(scr).feed(raw.decode("utf-8", "replace"))
    failures += check("NO_COLOR=1", sidebar_rows(scr, args.cols, args.rows), {}, True)

    print("\n%d assertion(s) failed" % failures, flush=True)
    sys.stdout.flush()
    # The child was killed, not reaped: exit hard so a zombie cannot hold the run.
    os._exit(1 if failures else 0)


if __name__ == "__main__":
    main()
