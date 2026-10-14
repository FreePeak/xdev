#!/usr/bin/env python3
"""Measure what the TUI costs while it streams: CPU over a fixed window, and the
screen it actually painted.

    scripts/tui-perf-drive.py --bin /tmp/xdev --label base [--kb 1200] [--idle 10]

Why this exists: the frame path's cost is invisible to `go test -bench` alone.
A benchmark measures one function on a fixture; this drives the REAL binary in a
pty against a mock openai-completions provider, so the number is the loop the
user feels — including whatever the tick, the coalescing and the tty flush add.
It was written for the `perf/tui-fps` work (a streaming block is re-rendered
whole on every delta), where the benchmark said 8.7x and the process said 2.4x.

It reports three things, because a CPU number alone cannot tell "the fix worked"
from "the turn never ran":

  idle_cpu_pct        percent of one core while nothing happens (should be ~0)
  stream_cpu_pct      percent of one core while `kb` KB of markdown streams in
  rendered            whether the answer reached the screen

The fixture is invented prose with a URL, a fence, a table and a list — the
paths the renderer has, and nobody's data. Requires: python3 with `pyte`
(`python3 -m venv /tmp/tuivenv && /tmp/tuivenv/bin/pip install pyte`).
"""
import argparse
import fcntl
import json
import os
import pty
import select
import signal
import struct
import subprocess
import sys
import termios
import threading
import time

COLS, ROWS = 120, 40

PARA = (
    "the quick brown fox jumps over the lazy dog and keeps going for a while "
    "because the paragraph has to wrap several times at this width; "
)

TAIL = (
    "\n\nSee https://example.com/a/very/long/path/segment for the detail.\n\n"
    "```go\nfunc run(n int) { for i := range n { _ = i } }\n```\n\n"
    "| col | value |\n|---|---|\n| one | two |\n\n- first\n- second\n"
)


def body(kb):
    """`kb` KB of markdown: a paragraph repeated until the size is reached."""
    head = "## Findings\n\n"
    repeats = max(1, (kb * 1024 - len(head) - len(TAIL)) // len(PARA))
    return head + PARA * repeats + TAIL


def serve(port, payload):
    import http.server

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
            self.rfile.read(n)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            # Chunks the size a provider really sends, with no sleep: the
            # stream should arrive as fast as the loop can consume it, which is
            # the case a laggy UI turns into backpressure.
            for i in range(0, len(payload), 512):
                self.sse({"choices": [{"index": 0, "delta": {"content": payload[i : i + 512]}}]})
            self.sse({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def write_agent_dir(path, port):
    os.makedirs(path, exist_ok=True)
    for f in os.listdir(path):
        p = os.path.join(path, f)
        if os.path.isfile(p):
            os.remove(p)
    with open(os.path.join(path, "models.yml"), "w") as f:
        f.write(
            "providers:\n  mock:\n    baseUrl: http://127.0.0.1:%d/v1\n"
            "    apiKey: mock\n    api: openai-completions\n"
            "    models:\n      - id: mock-1\n        name: mock\n" % port
        )


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", required=True, help="the xdev binary to drive")
    ap.add_argument("--label", default="run", help="name for this run (agent dir suffix)")
    ap.add_argument("--port", type=int, default=8981)
    ap.add_argument("--kb", type=int, default=1200, help="size of the streamed answer")
    ap.add_argument("--idle", type=float, default=10.0, help="idle soak seconds")
    ap.add_argument("--stream", type=float, default=25.0, help="streaming window seconds")
    ap.add_argument("--agent-dir", default="")
    args = ap.parse_args()

    try:
        import pyte
    except ImportError:
        sys.exit("needs pyte: python3 -m venv /tmp/tuivenv && /tmp/tuivenv/bin/pip install pyte")

    payload = body(args.kb)
    srv = serve(args.port, payload)
    agent_dir = args.agent_dir or "/tmp/xdev-perf-drive-%s" % args.label
    write_agent_dir(agent_dir, args.port)

    env = dict(os.environ)
    env["XDEV_AGENT_DIR"] = agent_dir
    env["TERM"] = "xterm-256color"
    env["COLORTERM"] = "truecolor"
    env.pop("NO_COLOR", None)

    pid, fd = pty.fork()
    if pid == 0:
        os.execve(args.bin, [args.bin, "tui", "-m", "mock/mock-1"], env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    screen = pyte.Screen(COLS, ROWS)
    stream = pyte.Stream(screen)
    written = 0

    def pump(seconds):
        nonlocal written
        end = time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.02)
            if not r:
                continue
            try:
                data = os.read(fd, 1 << 22)
            except OSError:
                return
            if not data:
                return
            written += len(data)
            stream.feed(data.decode("utf-8", "replace"))

    def cpu():
        out = subprocess.run(
            ["ps", "-o", "time=", "-p", str(pid)], capture_output=True, text=True
        ).stdout.strip()
        if not out:
            return 0.0
        p = out.split(":")
        if len(p) == 3:
            return int(p[0]) * 3600 + int(p[1]) * 60 + float(p[2])
        return int(p[0]) * 60 + float(p[1])

    out = {"label": args.label, "body_kb": len(payload) // 1024}
    try:
        pump(3.0)  # boot to the welcome screen
        i0, ib0 = cpu(), written
        pump(args.idle)
        out["idle_cpu_pct"] = round((cpu() - i0) / args.idle * 100, 2)
        out["idle_tty_bytes_per_s"] = int((written - ib0) / args.idle)

        os.write(fd, b"stream a huge answer\r")
        pump(1.0)  # let the turn open
        s0, sb0 = cpu(), written
        pump(args.stream)
        out["stream_cpu_pct"] = round((cpu() - s0) / args.stream * 100, 2)
        out["stream_tty_bytes_per_s"] = int((written - sb0) / args.stream)
        text = "\n".join(screen.display)
        out["rendered"] = "quick brown fox" in text
    finally:
        try:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        except Exception:
            pass
        srv.shutdown()
    print(json.dumps(out))


if __name__ == "__main__":
    main()
