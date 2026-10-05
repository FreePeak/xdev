#!/usr/bin/env python3
"""Render the real reasoning box through /theme and report the painted cells.

Same pty+pyte approach as tui-theme-drive.py, because a full-screen TUI
repaints every dirty cell: "the string is in the capture" proves nothing, so
this reports the SGR AT named cells of the thinking frame.

The mock provider must produce a thinking block, so it branches on the
conversation (a role=="tool" result means the turn already ran), not on a
request counter.
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
    """One turn of reasoning + one assistant line, over the chat-completions shape."""
    import threading

    def once():
        try:
            body = json.dumps({
                "id": "c1", "object": "chat.completion", "model": "mock-1",
                "choices": [{"index": 0, "finish_reason": "stop", "message": {
                    "role": "assistant", "content": "here is the answer"}}],
            }).encode()
            req = urllib.request.Request(
                "http://127.0.0.1:%d/v1/chat/completions" % port, data=body,
                headers={"Content-Type": "application/json"})
            urllib.request.urlopen(req, timeout=10).read()
        except Exception:
            pass

    import http.server

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("content-length", 0))
            raw = self.rfile.read(n).decode("utf-8", "replace")
            try:
                msgs = json.loads(raw).get("messages", [])
            except Exception:
                msgs = []
            done = any(m.get("role") == "tool" for m in msgs)
            if not done:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for chunk in [
                    {"choices": [{"index": 0, "delta": {"reasoning_content": "REASONING-TEXT-HERE"}}]},
                    {"choices": [{"index": 0, "delta": {"content": "ANSWER-TEXT-HERE"}}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
                ]:
                    self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
                    self.wfile.flush()
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                threading.Timer(0.35, once).start()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({
                "id": "c2", "object": "chat.completion", "model": "mock-1",
                "choices": [{"index": 0, "finish_reason": "stop", "message": {
                    "role": "assistant", "content": "here is the answer"}}],
            }).encode())

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--themes", default="rose-pine,dracula,one-dark,groknight")
    ap.add_argument("--settle", type=float, default=1.5)
    ap.add_argument("--port", type=int, default=8931)
    ap.add_argument("--agent-dir", default="/tmp/xdev-thinkink-drive/agent")
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

    def send(text):
        os.write(fd, text.encode())
        pump(args.settle)

    def find(needle):
        for y in range(ROWS):
            row = "".join(screen.buffer[y][x].data for x in range(COLS))
            if needle in row:
                return y, row
        return -1, ""

    def report(label):
        y, row = find("REASONING-TEXT-HERE")
        print("\n=== %s ===" % label)
        if y < 0:
            print("REASONING-TEXT-HERE is not on screen — the box never rendered")
            return
        top = max(0, y - 1)
        for label2, yy in (("frame top", top), ("body", y)):
            cells = []
            for x in range(COLS):
                c = screen.buffer[yy][x]
                if c.data.strip():
                    cells.append((x, c.fg, c.bg, c.data))
            uniq = []
            for x, fg, bg, d in cells:
                if uniq and uniq[-1][1] == fg and uniq[-1][2] == bg:
                    uniq[-1] = (uniq[-1][0], fg, bg, uniq[-1][3] + d)
                else:
                    uniq.append((x, fg, bg, d))
            print("  %-10s %s" % (label2, " | ".join("x=%d %s on %s %r" % u for u in uniq[:8])))

    try:
        pump(2.5)
        send("why is the sky blue" + ENTER)
        pump(3.0)
        for name in [t for t in args.themes.split(",") if t]:
            send("/theme %s%s" % (name, ENTER))
            report("/theme %s" % name)
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


    # Stop the mock and the pty: ThreadingHTTPServer.serve_forever keeps a
    # non-daemon worker alive, so a drive that only kills the child hangs
    # until the caller times out (measured: 2 min for five frames).
    srv.shutdown()
    sys.exit(0)

if __name__ == "__main__":
    main()
