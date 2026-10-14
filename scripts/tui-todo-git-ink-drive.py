#!/usr/bin/env python3
"""Drive the real xdev TUI and read the TODO markers' and GIT rows' inks off the
cell grid.

The unit tests (`internal/tui/todomarker_test.go`, `internal/tui/gitout_test.go`)
prove the renderer resolves the palette slots; this proves the running binary
does, through a real turn. The question is chromatic — which SGR a marker cell
and a porcelain state cell actually carry — so the answer is read off the CELL
GRID, not the text stream. A full-screen TUI repaints every dirty cell, so "the
row is in the capture" proves nothing: the text is in the capture either way, and
only the SGR at a cell is evidence.

Same pty harness as scripts/tui-report-drive.py, and the same traps it documents,
each of which reads as "the binary is broken":

  * tcell opens /dev/tty, so the child needs its own session AND the pty slave as
    its CONTROLLING terminal (setsid + TIOCSCTTY). A plain pty.fork() gives
    "open /dev/tty: device not configured" and a blank frame.
  * the mock branches on the CONVERSATION (which tool results are already in the
    request), never on a request counter.
  * one probe per process, hard-exit when the grid is scored: a child that has
    not been reaped holds the run open past any timeout and the capture is lost.
  * `pyte`'s `screen.buffer[y]` is a sparse defaultdict keyed by column, so a row
    must be read as `buffer[y][x] for x in range(cols)`; iterating the slice
    directly hands back `str`, not `Cell`.

The expected inks are PINNED LITERALS, never read back out of the theme — the
same rule internal/tui/rosepine_test.go records, and for the same reason: a probe
that asks the theme what it should have painted passes on the palette it
replaced.

Usage:
    /tmp/pyte-venv/bin/python scripts/tui-todo-git-ink-drive.py BIN [--cols 120]
"""
import argparse
import fcntl
import json
import os
import pty
import select
import shutil
import signal
import struct
import sys
import termios
import time
import urllib.request

# Rosé Pine's own inks, as groknightSlots resolves them (the launch theme).
#   comment   syntax_comment     #6e6a86   done / chrome
#   keyword   syntax_keyword     #31748f   in progress / a porcelain state
#   string    syntax_string      #f6c177   blocked
#   punct     syntax_punctuation #908caa   dropped
#   type      syntax_type        #9ccfd8   the branch, and a section heading
#   function  syntax_function    #ebbcba   a path
TODO_INK = {
    "[x]": "6e6a86",
    "[/]": "31748f",
    "[!]": "f6c177",
    "[-]": "908caa",
}
GIT_STATE_INK = "31748f"
GIT_PATH_INK = "ebbcba"
GIT_BRANCH_INK = "9ccfd8"
BODY_INK = "c8c8c8"

# One task list, driven to one row per state. The tool auto-promotes the next
# PENDING task, so the fifth keeps a "[ ]" row alive to the end.
TODO_OPS = [
    json.dumps({"op": "init", "items": ["wire the marker", "another task", "a third", "a fourth", "a fifth"]}),
    json.dumps({"op": "done", "task": "wire the marker"}),
    json.dumps({"op": "block", "task": "another task", "reason": "waiting on review"}),
    json.dumps({"op": "drop", "task": "a third"}),
]

GIT_ARGS = json.dumps({"command": "git status --short --branch"})
GIT_OUT = "\n".join([
    "## work/example...origin/work/example",
    " M internal/tui/render.go",
    "?? docs/notes.md",
])

COLS, ROWS = 130, 60
ENTER = "\r"
PROMPT = "list the work"

# Rosé Pine's own inks, as groknightSlots resolves them (the launch theme).
#   comment   syntax_comment   #6e6a86   done / chrome
#   keyword   syntax_keyword   #31748f   in progress / a porcelain state
#   string    syntax_string    #f6c177   blocked
#   punct     syntax_punctuation #908caa dropped
#   type      syntax_type      #9ccfd8   the branch, and a section heading
#   function  syntax_function  #ebbcba   a path
TODO_INK = {
    "[x]": "6e6a86",
    "[/]": "31748f",
    "[!]": "f6c177",
    "[-]": "908caa",
}
GIT_STATE_INK = "31748f"
GIT_PATH_INK = "ebbcba"
GIT_BRANCH_INK = "9ccfd8"
BODY_INK = "c8c8c8"



def serve(port):
    import http.server
    import threading

    def sse(wfile, obj):
        wfile.write(("data: " + json.dumps(obj) + "\n\n").encode())
        wfile.flush()

    def tool_call(wfile, call_id, name, args):
        sse(wfile, {"choices": [{"index": 0, "delta": {"tool_calls": [
            {"index": 0, "id": call_id, "type": "function",
             "function": {"name": name, "arguments": ""}}]}}]})
        for piece in args[:24], args[24:]:
            sse(wfile, {"choices": [{"index": 0, "delta": {"tool_calls": [
                {"index": 0, "function": {"arguments": piece}}]}}]})
            time.sleep(0.01)
        sse(wfile, {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]})
        wfile.write(b"data: [DONE]\n\n")
        wfile.flush()

    def text(wfile, body):
        sse(wfile, {"choices": [{"index": 0, "delta": {"content": body}}]})
        sse(wfile, {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
        wfile.write(b"data: [DONE]\n\n")
        wfile.flush()

    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # the provider health probe before the turn
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"data": [{"id": "mock-1"}]}).encode())

        def do_POST(self):
            n = int(self.headers.get("content-length", 0))
            try:
                msgs = json.loads(self.rfile.read(n).decode("utf-8", "replace")).get("messages", [])
            except Exception:
                msgs = []
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            # Which tool results are already in the conversation says where the
            # turn is: this is the only state the mock keeps.
            results = [m for m in msgs if m.get("role") == "tool"]
            if len(results) < len(TODO_OPS):
                tool_call(self.wfile, "call-todo-%d" % len(results), "todo", TODO_OPS[len(results)])
                return
            if len(results) == len(TODO_OPS):
                tool_call(self.wfile, "call-git", "bash", GIT_ARGS)
                return
            text(self.wfile, "the list is above")

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--settle", type=float, default=4.0)
    ap.add_argument("--port", type=int, default=8953)
    ap.add_argument("--agent-dir", default="/tmp/xdev-todo-git-ink/agent")
    args = ap.parse_args()

    import pyte

    srv = serve(args.port)
    agent_dir = args.agent_dir
    shutil.rmtree(agent_dir, ignore_errors=True)
    os.makedirs(agent_dir, exist_ok=True)
    with open(os.path.join(agent_dir, "models.yml"), "w") as f:
        f.write("providers:\n  mock:\n    baseUrl: http://127.0.0.1:%d/v1\n"
                "    apiKey: mock\n    api: openai-completions\n"
                "    models:\n      - id: mock-1\n        name: mock\n" % args.port)
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

    failures = 0
    try:
        pump(2.5)
        os.write(fd, (PROMPT + ENTER).encode())
        pump(args.settle + 12.0)

        rows = []
        for y in range(ROWS):
            cells = [screen.buffer[y][x] for x in range(COLS)]
            text = "".join(c.data for c in cells).rstrip()
            if text.strip():
                rows.append((y, text, cells))
        print("=== the transcript, row by row (text | FG per run) ===", flush=True)
        for y, text, cells in rows:
            runs = []
            prev = None
            for c in cells:
                if not c.data.strip():
                    continue
                if c.fg != prev:
                    runs.append("%s@%d" % (c.fg, text.index(c.data) if c.data in text else -1))
                    prev = c.fg
            print("  r%02d |%-40s| %s" % (y, ",".join(runs[:8]), text[:110]), flush=True)

        def cell_fg(y, needle, off=0):
            # The FG of the cell `off` columns past the first `needle` in row y.
            _, text, cells = rows[y]
            i = text.find(needle)
            if i < 0:
                return None
            x = i + off
            return cells[x].fg if 0 <= x < COLS else None

        def find(needle):
            """The y of the first row containing needle, or -1.

            A result box frames every row with a rail and a border, and the
            LAST tool box is still being replaced while the turn streams, so the
            newest occurrence is the settled one: scan from the bottom.
            """
            for y in range(len(rows) - 1, -1, -1):
                if needle in rows[y][1]:
                    return y
            return -1

        def want(label, y, needle, off, ink):
            nonlocal failures
            if y < 0:
                print("  !! %s: %r is not on screen" % (label, needle), flush=True)
                failures += 1
                return
            got = cell_fg(y, needle, off)
            if got != ink:
                failures += 1
            print("  %-22s %r -> fg=%s%s" % (label, needle, got, "" if got == ink else "  !! want %s" % ink),
                  flush=True)

        # The todo markers: each one's own ink, and the text beside it in the
        # ordinary body ink (so a marker is a coloured cell, not a coloured row).
        # The rows come from the todo tool's own result box, which is where the
        # markers and their inks are visible together.
        for marker, ink in TODO_INK.items():
            y = find(marker + " ")
            want("todo marker " + marker, y, marker, 0, ink)
            want("todo text " + marker, y, marker, 4, BODY_INK)
        # "[ ]" deliberately wears the body ink: pending is the resting state.
        want("todo marker [ ]", find("[ ] "), "[ ]", 0, BODY_INK)
        # The git listing, read off the bash result the mock returned.
        y = find(" M internal/tui/")
        want("git state  M", y, " M", 0, GIT_STATE_INK)
        want("git path", y, " M", 2, GIT_PATH_INK)
        want("git branch", find("## "), "## ", 3, GIT_BRANCH_INK)
        # The call row is chrome, not a git row: it must NOT wear a git ink.
        y = find("bash · git status")
        want("call row is chrome", y, "bash", 0, BODY_INK)
    finally:
        try:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        except (ProcessLookupError, ChildProcessError):
            pass
        srv.shutdown()

    print("\n%d assertion(s) failed" % failures, flush=True)
    sys.stdout.flush()
    # The child was killed, not reaped: exit hard so a zombie cannot hold the run.
    os._exit(1 if failures else 0)


if __name__ == "__main__":
    main()
