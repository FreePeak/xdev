#!/usr/bin/env python3
"""Render the sent-message card through the real TUI and report its geometry.

Same pty+pyte approach as the other drive scripts: a full-screen TUI repaints
every dirty cell, so "the string is in the capture" proves nothing. This reports
the BG at named cells, which is the only thing a margin and a padding row are.

The mock provider branches on the conversation (a role=="tool" result means the
turn already ran), not on a request counter.
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
PROMPT = "ship the band padding"


def serve(port):
    import http.server
    import threading

    def once():
        try:
            body = json.dumps({
                "id": "c1", "object": "chat.completion", "model": "mock-1",
                "choices": [{"index": 0, "finish_reason": "stop", "message": {
                    "role": "assistant",
                    "content": "\n".join("ANSWER-ROW-%02d" % i for i in range(1, 41))}}],
            }).encode()
            req = urllib.request.Request(
                "http://127.0.0.1:%d/v1/chat/completions" % port, data=body,
                headers={"Content-Type": "application/json"})
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
            done = any(m.get("role") == "tool" for m in msgs)
            if not done:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for i in range(1, 41):
                    frame = {"choices": [{"index": 0, "delta": {
                        "content": "ANSWER-ROW-%02d\n" % i}}]}
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
    ap.add_argument("--settle", type=float, default=2.0)
    ap.add_argument("--port", type=int, default=8941)
    ap.add_argument("--agent-dir", default="/tmp/xdev-band-drive/agent")
    ap.add_argument("--scroll", type=int, default=0, help="wheel notches up after the turn")
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

    def row(y):
        return "".join(screen.buffer[y][x].data for x in range(COLS))

    def bg(y, x):
        return screen.buffer[y][x].bg

    def find(needle):
        for y in range(ROWS):
            r = row(y)
            if needle in r:
                return y
        return -1

    EDGES = (0, 1, 2, 3, COLS - 4, COLS - 3, COLS - 2, COLS - 1)

    def userBandInset_probe():
        """The column the card's ❯ sits at: the first prompt row's glyph."""
        for yy in range(ROWS):
            for x in range(COLS):
                if screen.buffer[yy][x].data == "\u276f":
                    return x
        return -1

    def dump(label):
        print("\n=== %s ===" % label)
        y = find(PROMPT)
        if y < 0:
            print("the prompt is not on screen")
            for yy in range(ROWS):
                print("  %2d |%s|" % (yy, row(yy)))
            return
        top = max(0, y - 2)
        print("prompt row: %d (card rows %d..%d)" % (y, top, min(ROWS - 1, y + 2)))
        for yy in range(top, min(ROWS, y + 3)):
            cells = []
            for x in range(COLS):
                c = screen.buffer[yy][x]
                b = "#" if c.bg not in ("default",) else "."
                g = c.data if c.data.strip() else " "
                cells.append(g if g != " " else b)
            print("  %2d |%s|" % (yy, "".join(cells)))
            print("      %s" % " ".join("x%-2d=%-7s" % (x, bg(yy, x)) for x in EDGES))
            if PROMPT in row(yy):
                print("      prefix cell x%d = %r" % (userBandInset_probe(), screen.buffer[yy][userBandInset_probe()].data))

    try:
        pump(2.5)
        send(PROMPT + ENTER)
        pump(3.5)
        dump("inline sent message")
        # PgUp, which the keymap binds to scroll-page-up: mouse wheel notches
        # need SGR mouse mode on, and a drive must not turn it on (every
        # keystroke then arrives as a click).
        for _ in range(args.scroll):
            send("\x1b[5~")
        pump(1.0)
        if args.scroll:
            dump("after %d wheel notches (sticky header)" % args.scroll)
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