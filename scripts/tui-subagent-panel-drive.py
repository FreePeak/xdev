#!/usr/bin/env python3
"""Drive a real spawn that STAYS RUNNING and report what the strip paints.

The transcript drive (tui-subagent-transcript-drive.py) scores the child's
settled row and the panel its click opens. This one is the complement: the mock
child never yields, so the spawn blocks the parent turn for the whole run and
the live strip (internal/tui/agentspanel.go) is the only thing on screen saying
what is happening — which is exactly the state the strip exists for.

Scored, on the real binary in a pty with pyte:
  1. the strip paints a row naming the child while the turn is blocked;
  2. the row sits ABOVE the composer's top border (it is chrome, not content);
  3. a click on that row opens the CHILD's own transcript in the panel;
  4. Esc closes it and the session transcript is intact underneath.

Same traps as the sibling script: setsid + TIOCSCTTY (tcell opens /dev/tty
itself), one probe per process, hard os._exit, a per-run agent dir, and a mock
that branches on the CONVERSATION rather than on a request counter.
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

sys.stdout.reconfigure(line_buffering=True)

COLS, ROWS = 110, 34
ENTER = "\r"
CHILD_MARK = "CHILD-PROMPT-MARKER"
CHILD_SAYS = "CHILD-WORKING-MARKER"
PARENT = "PARENT-TURN-MARKER"
READ_PATH = "internal/tui/agentspanel.go"


def serve(port):
    import http.server
    import threading

    def sse(payloads):
        body = b""
        for p in payloads:
            body += ("data: " + json.dumps(p) + "\n\n").encode()
        # [DONE] LAST: the openai stream reader stops on the terminator, so a
        # body that ends with it before finish_reason reads as a provider
        # outage and the app retries the turn forever.
        body += b"data: [DONE]\n\n"
        return body

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("content-length", 0))
            raw = self.rfile.read(n).decode("utf-8", "replace")
            try:
                msgs = json.loads(raw).get("messages", [])
            except Exception:
                msgs = []
            child_conv = any(
                m.get("role") == "user" and CHILD_MARK in json.dumps(m.get("content"))
                for m in msgs
            )

            def stream(payloads):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(sse(payloads))
                self.wfile.flush()

            def complete(content):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({
                    "id": "c", "object": "chat.completion", "model": "mock-1",
                    "choices": [{"index": 0, "finish_reason": "stop", "message": {
                        "role": "assistant", "content": content}}],
                }).encode())

            if not child_conv:
                # The PARENT: one `task` call, and then it BLOCKS — the child
                # never hands back, so this conversation is asked once and no
                # more. Anything else here would make the parent answer while
                # the strip is supposed to be showing a running child.
                stream([
                    {"choices": [{"index": 0, "delta": {"tool_calls": [{
                        "index": 0, "id": "call-task-1", "type": "function",
                        "function": {"name": "task", "arguments": json.dumps({
                            "name": "scout", "prompt": CHILD_MARK})}}]}}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]},
                ])
                return

            # The CHILD: says its marker once, then keeps calling a tool so the
            # strip has a live "what it is doing" line and the run never ends.
            said = any(CHILD_SAYS in json.dumps(m) for m in msgs)
            if not said:
                stream([
                    {"choices": [{"index": 0, "delta": {"content": CHILD_SAYS}}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
                ])
                return
            stream([
                {"choices": [{"index": 0, "delta": {"tool_calls": [{
                    "index": 0, "id": "call-read-1", "type": "function",
                    "function": {"name": "read", "arguments": json.dumps(
                        {"path": READ_PATH})}}]}}]},
                {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]},
            ])

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--port", type=int, default=8947)
    ap.add_argument("--settle", type=float, default=1.2)
    ap.add_argument("--dump", action="store_true", help="print the whole screen")
    ap.add_argument("--agent-dir", default="/tmp/xdev-subagent-panel/agent")
    args = ap.parse_args()

    agent_dir = args.agent_dir
    os.system("rm -rf %s" % agent_dir)
    os.makedirs(agent_dir, exist_ok=True)
    with open(os.path.join(agent_dir, "models.yml"), "w") as f:
        f.write("providers:\n  mock:\n    baseUrl: http://127.0.0.1:%d/v1\n"
                "    apiKey: mock\n    api: openai-completions\n"
                "    models:\n      - id: mock-1\n        name: mock\n" % args.port)
    srv = serve(args.port)

    env = dict(os.environ)
    env["XDEV_AGENT_DIR"] = agent_dir
    # debugMouse paints every mouse event on the status row: without it a click
    # that did nothing and a click the app never RECEIVED are the same symptom.
    with open(os.path.join(agent_dir, "config.yml"), "w") as f:
        f.write("debugMouse: true\nstatusLine:\n  segments:\n    - debugMouse\n")
    env["TERM"] = "xterm-256color"
    env["COLORTERM"] = "truecolor"
    env.pop("NO_COLOR", None)

    pid, fd = pty.fork()
    if pid == 0:
        try:
            os.setsid()
        except OSError:
            pass
        try:
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)
        except OSError:
            pass
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

    def rows_with(needle):
        out = []
        for y in range(ROWS):
            row = "".join(screen.buffer[y][x].data for x in range(COLS))
            if needle in row:
                out.append((y, row.rstrip()))
        return out

    def dump(title):
        print("---- %s ----" % title)
        for y in range(ROWS):
            row = "".join(screen.buffer[y][x].data for x in range(COLS)).rstrip()
            if row:
                print("  y=%2d | %s" % (y, row))

    def sgr_click(x, y, button=0):
        # ESC [ < b ; x ; y M, 1-based. tcell v2 decodes ONLY the SGR form;
        # X10 is silently dropped, which reads as "clicking does nothing".
        os.write(fd, ("\x1b[<%d;%d;%dM" % (button, x + 1, y + 1)).encode())

    def sgr_release(x, y):
        os.write(fd, ("\x1b[<0;%d;%dm" % (x + 1, y + 1)).encode())

    rc = 0
    try:
        pump(3.0)
        send("map the app shell" + ENTER)
        # The child stays in flight, so this window is the whole point: the
        # strip has to be the thing on screen.
        pump(8.0)
        if args.dump:
            dump("screen while the child runs")

        print("== while the child runs ==")
        composer = None
        for y, row in rows_with("Type a message"):
            composer = y
        strip = [(y, r) for y, r in rows_with("scout")]
        print("  composer top row        : %s" % composer)
        print("  strip row naming scout  : %s" % ("yes" if strip else "NO"))
        for y, r in strip:
            print("    y=%d | %s" % (y, r[:100]))
        if composer is not None and strip:
            above = all(y < composer for y, _ in strip)
            print("  strip above the composer: %s" % ("yes" if above else "NO"))
            if not above:
                rc = 1
        else:
            rc = 1

        # Click the strip's own row (not a transcript row): the strip paints a
        # spinner gutter at x=1 and the label from x=3.
        if strip:
            y = strip[0][0]
            print("  clicking the strip row y=%d" % y)
            sgr_click(6, y)
            pump(0.3)
            sgr_release(6, y)
            pump(2.0)
            if args.dump:
                dump("screen after the strip click")
            title = rows_with("transcript ·")
            kid = rows_with(CHILD_SAYS)
            print("== after the click ==")
            print("  panel opened the child's transcript : %s" % ("yes" if title and kid else "NO"))
            for y, r in title:
                print("    y=%d | %s" % (y, r[:100]))
            for y, r in kid:
                print("    y=%d | %s" % (y, r[:100]))
            if not (title and kid):
                rc = 1

        # Esc closes the panel and the session transcript is intact.
        os.write(fd, b"\x1b")
        pump(1.5)
        still = rows_with("transcript ·")
        print("== after Esc ==")
        print("  panel closed              : %s" % ("yes" if not still else "NO"))
        print("  session transcript intact : %s" % ("yes" if rows_with("map the app shell") else "NO"))
        if still:
            rc = 1
    finally:
        try:
            os.write(fd, b"\x18\x11")
            time.sleep(0.2)
        except OSError:
            pass
        try:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        except (ProcessLookupError, ChildProcessError):
            pass
        srv.shutdown()
    os._exit(rc)


if __name__ == "__main__":
    main()
