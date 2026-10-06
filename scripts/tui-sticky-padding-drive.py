#!/usr/bin/env python3
"""Walk the transcript's scroll ladder and report the transcript's first rows.

Same pty+pyte approach as the other drive scripts: a full-screen TUI repaints
every dirty cell, so "the string is in the capture" proves nothing. This reports
the BAND state of the first rows at each scroll offset, which is the only thing
a card's padding is.

The mock provider branches on the CONVERSATION (a role=="tool" result means the
turn already ran), not on a request counter: the harness makes its own requests
before the turn, so a counter lands on the wrong arm.

Scrolling is Shift+Up (scroll-up, one line) rather than the wheel, because SGR
mouse mode has to be off for a drive -- with it on, every keystroke also arrives
as a click.
"""
import argparse
import fcntl
import json
import os
import pty
import select
import signal
import struct
import sys
import termios
import time
import urllib.request

COLS, ROWS = 100, 30
ENTER = "\r"


def serve(port):
    import http.server
    import threading

    def once():
        try:
            body = json.dumps({"id": "c1", "object": "chat.completion", "model": "mock-1",
                               "choices": [{"index": 0, "finish_reason": "stop", "message": {
                                   "role": "assistant", "content": "here is the answer"}}]}).encode()
            req = urllib.request.Request("http://127.0.0.1:%d/v1/chat/completions" % port,
                                         data=body, headers={"Content-Type": "application/json"})
            urllib.request.urlopen(req, timeout=10).read()
        except Exception:
            pass

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("content-length", 0))
            raw = self.rfile.read(n).decode("utf-8", "replace")
            try:
                msgs = json.loads(raw).get("messages", [])
            except Exception:
                msgs = []
            last = ""
            for m in msgs:
                if m.get("role") == "user":
                    c = m.get("content")
                    last = c if isinstance(c, str) else json.dumps(c)
            tag = ((last or "turn").split()[0] or "turn")[:8]
            done = any(m.get("role") == "tool" for m in msgs)
            if not done:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for i in range(1, 13):
                    frame = {"choices": [{"index": 0, "delta": {"content": "%s-ROW-%02d\n" % (tag.upper(), i)}}]}
                    self.wfile.write((u"data: " + json.dumps(frame) + u"\n\n").encode())
                    self.wfile.flush()
                    time.sleep(0.01)
                self.wfile.write((u"data: " + json.dumps({
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}) + u"\n\n").encode())
                self.wfile.flush()
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                threading.Timer(0.35, once).start()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"id": "c2", "object": "chat.completion", "model": "mock-1",
                                         "choices": [{"index": 0, "finish_reason": "stop", "message": {
                                             "role": "assistant", "content": "done"}}]}).encode())

        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"object": "list", "data": [{"id": "mock-1", "object": "model"}]}).encode())

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--port", type=int, default=8957)
    ap.add_argument("--agent-dir", default="/tmp/xdev-sticky-ladder/agent")
    ap.add_argument("--turns", type=int, default=5)
    ap.add_argument("--ladder", default="0,3,6,8,12,20",
                    help="cumulative Shift+Up presses per sample")
    ap.add_argument("--rows", type=int, default=7)
    args = ap.parse_args()

    srv = serve(args.port)
    agent_dir = args.agent_dir
    os.makedirs(agent_dir, exist_ok=True)
    for f in ("config.yml", "config.yml.bak"):
        p = os.path.join(agent_dir, f)
        if os.path.exists(p):
            os.remove(p)
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

    def banded(y):
        return sum(1 for x in range(3, COLS - 2) if screen.buffer[y][x].bg not in ("default",)) > 20

    def text(y):
        return "".join(screen.buffer[y][x].data for x in range(COLS)).rstrip()

    try:
        pump(2.5)
        for i in range(1, args.turns + 1):
            os.write(fd, ("PROMPT%d ship the band padding" % i + ENTER).encode())
            pump(2.8)
        up = 0
        for n in [int(x) for x in args.ladder.split(",")]:
            for _ in range(n - up):
                os.write(fd, b"\x1b[1;2A")  # Shift+Up
                pump(0.06)
            up = n
            pump(0.9)
            print("\n=== after %d scroll-up lines ===" % n)
            for y in range(args.rows):
                tag = "band" if banded(y) else "    "
                print("  %2d [%s] %s" % (y, tag, text(y)[:70]))
    finally:
        try:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        except (ProcessLookupError, ChildProcessError):
            pass
        srv.shutdown()
    sys.exit(0)


if __name__ == "__main__":
    main()