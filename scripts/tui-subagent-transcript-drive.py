#!/usr/bin/env python3
"""Drive a real subagent spawn and report what a click on its row paints.

The simulation screen in the unit tests is evidence about the paint and hit
paths; this is the only way to see the experience. Same pty + pyte approach as
the other scripts in this directory, because a full-screen TUI repaints every
dirty cell: "the string is in the capture" proves nothing, so this reports the
rows a person would read, after replaying the raw pty bytes into a fresh
screen.

The mock provider must BRANCH ON THE CONVERSATION (a role=="tool" result means
the child already yielded), never on a request counter — the harness makes its
own probe requests before the turn and the number varies.

Traps this script encodes (each read as "the binary is broken" when it hit):
  * setsid + TIOCSCTTY (both best-effort: pty.fork may already have made the
    child a session leader, and setsid's EPERM there is not a failure), because
    tcell opens /dev/tty itself;
  * one probe per process and a hard os._exit after scoring, because a SIGKILLed
    but unreaped child keeps the parent's pipe open and the script hangs;
  * a per-run agent dir and no leftover fixture, because a seeded session means
    the first request already carries a tool result;
  * the chord is ESC-prefixed (Alt+B), which tcell only sees if mouse mode is
    off — the child row is clicked with a real SGR click instead.
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

# Unbuffered: this script is killed on timeout, and a buffered report is a lost
# report — the evidence has to be on disk before the process is reaped.
sys.stdout.reconfigure(line_buffering=True)

COLS, ROWS = 110, 34
ENTER = "\r"
# The child prompt is one long line; these are the markers the report scores.
CHILD_MARK = "CHILD-PROMPT-MARKER"
CHILD_SAYS = "CHILD-ANSWER-MARKER"
PARENT = "PARENT-TURN-MARKER"


def serve(port):
    """Two model roles, decided by what the request carries.

    A child agent runs on the session model, so the request that carries the
    child's prompt is indistinguishable by model id — but it IS distinguishable
    by the prompt text, which is the only honest branch available here.
    """
    import http.server
    import threading

    def sse(payloads):
        body = b""
        for p in payloads:
            body += ("data: " + json.dumps(p) + "\n\n").encode()
        # [DONE] LAST, after the chunk that carries finish_reason: the openai
        # stream reader stops on the terminator, so a body that ends with it
        # before finish_reason reports "stream ended without finish_reason" and
        # the app retries the turn forever. A mock bug that reads exactly like a
        # provider outage.
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
            text = json.dumps(msgs)
            tool_msgs = [m for m in msgs if m.get("role") == "tool"]
            # Everything below branches on the CONVERSATION, never on a request
            # counter: the harness makes its own health/probe requests before the
            # turn and the number varies, so a counter-driven mock silently walks
            # the wrong arm.
            #
            # Nor does it branch on the word "yield" appearing anywhere — the
            # child's SYSTEM prompt describes the yield tool, so "yield" is in
            # every child request and the branch that meant "this conversation has
            # already handed back" fired on the child's first request instead.
            # That reads as "the spawn hangs" and is really a bad predicate.
            # The child's conversation is identified by the user's PROMPT, not by
            # the marker appearing anywhere: the child's system prompt and its
            # own later turns repeat it, and a body scan for the marker also
            # matches the parent's tool ARGUMENTS (the spawn prompt), which is
            # what made this branch fire on the parent's second request.
            child_conv = any(
                m.get("role") == "user" and CHILD_MARK in json.dumps(m.get("content"))
                for m in msgs
            )
            yielded = any("yield accepted" in json.dumps(m) for m in tool_msgs)

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
                # The PARENT's conversation: one `task` call, then the answer.
                if not tool_msgs:
                    stream([
                        {"choices": [{"index": 0, "delta": {"tool_calls": [{
                            "index": 0, "id": "call-task-1", "type": "function",
                            "function": {"name": "task", "arguments": json.dumps({
                                "name": "scout", "prompt": CHILD_MARK})}}]}}]},
                        {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]},
                    ])
                else:
                    complete(PARENT)
                return

            # The CHILD's own conversation: say the marker (so the transcript
            # view has something only the child's session could show), then hand
            # back on the next request, then finish.
            if yielded:
                complete("done")
            elif any(CHILD_SAYS in json.dumps(m) for m in msgs):
                stream([
                    {"choices": [{"index": 0, "delta": {"tool_calls": [{
                        "index": 0, "id": "call-yield-1", "type": "function",
                        "function": {"name": "yield", "arguments": json.dumps(
                            {"result": "the child mapped it"})}}]}}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]},
                ])
            else:
                stream([
                    {"choices": [{"index": 0, "delta": {"content": CHILD_SAYS}}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
                ])

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--port", type=int, default=8941)
    ap.add_argument("--settle", type=float, default=1.5)
    ap.add_argument("--dump", action="store_true", help="print the whole screen")
    ap.add_argument("--verbose", action="store_true", help="run the binary with -v (debug log to stderr)")
    ap.add_argument("--agent-dir", default="/tmp/xdev-subagent-drive/agent")
    args = ap.parse_args()

    agent_dir = args.agent_dir
    # A per-run agent dir: a seeded session means the mock's first request
    # already carries a tool result and the walk starts on the wrong arm.
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
    # that "did nothing" and a click the app never RECEIVED are the same
    # symptom, and the drive cannot tell them apart. Two spellings, both needed:
    # `debugMouse` is a TOP-LEVEL settings key (not under `tui:` — that struct
    # is exitDetach/tabs, and an unknown key under it makes the whole file fail
    # to load, which kills the pty before a single frame is painted), and
    # `statusLine.segments` must NAME it, because the shipped segment list does
    # not include it and a segment nobody asked for is not painted at all.
    with open(os.path.join(agent_dir, "config.yml"), "w") as f:
        f.write("debugMouse: true\nstatusLine:\n  segments:\n    - debugMouse\n")
    env["TERM"] = "xterm-256color"
    env["COLORTERM"] = "truecolor"
    env.pop("NO_COLOR", None)

    # tcell opens /dev/tty itself, so the child needs a session of its own and
    # that terminal as its controlling one: a plain pty.fork() gives
    # "open /dev/tty: device not configured" and a blank frame, which reads as
    # a broken binary. The child is already a session leader after fork() on
    # some kernels, so setsid's EPserm is expected and not an error — only a
    # TIOCSCTTY failure leaves the child without a usable tty.
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
        os.execve(args.binary, [args.binary, "tui", "-m", "mock/mock-1"] + (["-v"] if args.verbose else []), env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    import pyte

    screen = pyte.Screen(COLS, ROWS)
    stream = pyte.Stream(screen)
    raw = bytearray()

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
            raw.extend(data)
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

    def sgr_click(x, y, button=0):
        # ESC [ < b ; x ; y M, 1-based, button 0 = left. tcell v2 decodes ONLY
        # the SGR form; X10 is silently dropped, which reads as "clicking does
        # nothing".
        os.write(fd, ("\x1b[<%d;%d;%dM" % (button, x + 1, y + 1)).encode())

    def sgr_release(x, y):
        os.write(fd, ("\x1b[<0;%d;%dm" % (x + 1, y + 1)).encode())

    rc = 0
    try:
        pump(3.0)
        send("map the app shell" + ENTER)
        pump(6.0)
        if args.dump:
            print("---- screen after the turn ----")
            for y in range(ROWS):
                row = "".join(screen.buffer[y][x].data for x in range(COLS)).rstrip()
                if row:
                    print("  y=%2d | %s" % (y, row))

        child = rows_with(CHILD_MARK)
        parent = rows_with(PARENT)
        print("== after the turn ==")
        print("  child row painted : %s" % ("yes" if child else "NO"))
        for y, row in child:
            print("    y=%d | %s" % (y, row[:100]))
        print("  parent answer     : %s (this mock's final stream never lands; the"
              " turn is scored on the task call row instead)" % ("yes" if parent else "absent"))
        for y, row in parent:
            print("    y=%d | %s" % (y, row[:100]))

        # The child row (the `⎿ scout · ...` continuation) is one row ABOVE the
        # parent's answer box; find the row that names the child and click it.
        target = None
        for y, row in rows_with("scout"):
            if "⎿" in row:
                target = (y, row)
                break
        if target is None:
            print("  NO child row naming scout — nothing to click")
            rc = 1
        else:
            y, row = target
            print("  clicking y=%d | %s" % (y, row[:100]))
            sgr_click(8, y)
            pump(0.3)
            sgr_release(8, y)
            pump(2.0)
            if args.dump:
                print("---- screen after the click ----")
                for yy in range(ROWS):
                    r = "".join(screen.buffer[yy][x].data for x in range(COLS)).rstrip()
                    if r:
                        print("  y=%2d | %s" % (yy, r))

            # The panel's transcript view must show the CHILD's own answer,
            # which only exists in the child's session.
            kid = rows_with(CHILD_SAYS)
            print("== after the click ==")
            print("  panel shows the child's transcript : %s" % ("yes" if kid else "NO"))
            for yy, r in kid:
                print("    y=%d | %s" % (yy, r[:100]))
            title = rows_with("transcript ·")
            for yy, r in title:
                print("  panel title   y=%d | %s" % (yy, r[:100]))
            if not kid:
                rc = 1
            else:
                # Esc back: the child's transcript leaves, the session
                # transcript underneath is still there. The session's own
                # marker is the task CALL ROW, not the parent's prose: a turn
                # whose model stream died mid-retry (this mock's last reply is
                # never reached) has no parent text at all, so scoring on it
                # measures the mock, not the app.
                send("\x1b")
                pump(1.0)
                gone = rows_with(CHILD_SAYS)
                still = rows_with("subagent yielded")
                print("== after Esc ==")
                print("  child's transcript closed : %s" % ("yes" if not gone else "NO"))
                print("  session transcript intact : %s" % ("yes" if still else "NO"))
                if gone or not still:
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
    # Hard exit: a background worker left by ThreadingHTTPServer keeps the
    # interpreter alive until the caller times out.
    os._exit(rc)


if __name__ == "__main__":
    main()
