#!/usr/bin/env python3
"""Render a subagent report through the real TUI and report each row's inks.

The mock provider makes the model call `task` with a report as its result, so
this drives the exact path a `scout`/`reviewer` answer takes: the call row, the
children it grows, and the settled result box. What it reports is the FG of every
row's first content cell, because "colourised" is a claim about inks, not about
the string being on screen.

The mock branches on the CONVERSATION (a role=="tool" result means the turn
already ran), never on a request counter.
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

COLS, ROWS = 110, 60
ENTER = "\r"
PROMPT = "recon the band"

REPORT = "\n".join([
    "subagent yielded",
    "",
    "## Findings",
    "",
    "- the band margin is userBandMargin",
    "- a fenced sample follows",
    "",
    "```go",
    "const userBandMargin = 1",
    "```",
])

REPORT_ARGS = json.dumps({"prompt": PROMPT, "name": "scout"})

# What the child yields: the handoff payload, which is what a subagent is asked
# to produce. renderSubagentResult prefixes it with "subagent yielded".
REPORT_ARGS_YIELD = json.dumps({"result": REPORT})


def serve(port):
    import http.server
    import threading

    def call(name, args):
        return {"id": "call-1", "type": "function",
                "function": {"name": name, "arguments": args}}

    REPORT_MSG = {"role": "tool", "tool_call_id": "call-1", "content": REPORT}

    def once():
        try:
            body = json.dumps({
                "id": "c1", "object": "chat.completion", "model": "mock-1",
                "choices": [{"index": 0, "finish_reason": "stop", "message": {
                    "role": "assistant", "content": "the report is above"}}],
            }).encode()
            req = urllib.request.Request(
                "http://127.0.0.1:%d/v1/chat/completions" % port, data=body,
                headers={"Content-Type": "application/json"})
            urllib.request.urlopen(req, timeout=10).read()
        except Exception:
            pass

    class H(http.server.BaseHTTPRequestHandler):
        def sse(self, obj):
            self.wfile.write(("data: " + json.dumps(obj) + "\n\n").encode())
            self.wfile.flush()

        def do_GET(self):  # the provider health probe before the turn
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"data": [{"id": "mock-1"}]}).encode())

        def do_POST(self):
            n = int(self.headers.get("content-length", 0))
            raw = self.rfile.read(n).decode("utf-8", "replace")
            try:
                req = json.loads(raw)
                msgs = req.get("messages", [])
                tools = req.get("tools") or []
            except Exception:
                msgs, tools = [], []
            # A CHILD conversation is the one carrying `yield` (children get it,
            # the parent does not). It answers with prose and no tool call, so
            # the subagent yields its report and the parent sees a tool result.
            child = any(t.get("function", {}).get("name") == "yield" for t in tools)
            if child:
                # The child conversation streams like any other, but it ends on
                # a `yield` tool call, not a plain stop: a subagent that replies
                # in prose is REMINDED to yield, and the report a user reads is
                # then the loose reply rather than a structured handoff.
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.sse({"choices": [{"index": 0, "delta": {"tool_calls": [
                    {"index": 0, "id": "call-y", "type": "function",
                     "function": {"name": "yield", "arguments": ""}}]}}]})
                for piece in REPORT_ARGS_YIELD[:30], REPORT_ARGS_YIELD[30:]:
                    self.sse({"choices": [{"index": 0, "delta": {"tool_calls": [
                        {"index": 0, "function": {"arguments": piece}}]}}]})
                    time.sleep(0.01)
                self.sse({"choices": [{"index": 0, "delta": {},
                                       "finish_reason": "tool_calls"}]})
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                return
            done = any(m.get("role") == "tool" for m in msgs)
            if not done:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.sse({"choices": [{"index": 0, "delta": {"tool_calls": [
                    {"index": 0, "id": "call-1", "type": "function",
                     "function": {"name": "task", "arguments": ""}}]}}]})
                # Arguments stream in pieces, the way a real tool call arrives.
                for piece in REPORT_ARGS[:20], REPORT_ARGS[20:]:
                    self.sse({"choices": [{"index": 0, "delta": {"tool_calls": [
                        {"index": 0, "function": {"arguments": piece}}]}}]})
                    time.sleep(0.02)
                self.sse({"choices": [{"index": 0, "delta": {},
                                       "finish_reason": "tool_calls"}]})
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                threading.Timer(1.5, once).start()
                return
            # The parent asked with stream=true, so the answer streams too: a
            # plain JSON body here is a protocol error, not a shortcut.
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            self.sse({"choices": [{"index": 0, "delta": {
                "content": "the report is above"}}]})
            self.sse({"choices": [{"index": 0, "delta": {},
                                   "finish_reason": "stop"}]})
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            once()

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--settle", type=float, default=3.0)
    ap.add_argument("--port", type=int, default=8951)
    ap.add_argument("--agent-dir", default="/tmp/xdev-report-drive/agent")
    args = ap.parse_args()

    srv = serve(args.port)

    agent_dir = args.agent_dir
    os.makedirs(agent_dir, exist_ok=True)
    for f in os.listdir(agent_dir):
        os.remove(os.path.join(agent_dir, f))
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

    try:
        pump(2.5)
        os.write(fd, (PROMPT + ENTER).encode())
        pump(args.settle + 6.0)
        print("=== the report box, row by row (text | FG of its first ink cell) ===", flush=True)
        for y in range(ROWS):
            r = "".join(screen.buffer[y][x].data for x in range(COLS)).rstrip()
            if not r:
                continue
            inks = []
            for x in range(COLS):
                c = screen.buffer[y][x]
                if c.data.strip() and c.data not in "│╭╰│─▁":
                    inks.append("%s@%d" % (c.fg, x))
            print("  %2d |%-8s| %s" % (y, ",".join(inks[:6]), r[:COLS]), flush=True)
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