#!/usr/bin/env python3
"""Drive the built `xdev gateway` against a fake Bot API and a fake provider,
and read out of the fake API what the chat actually saw.

Why this exists: the streaming preview is a timing behaviour — "the answer
grows in ONE message" — and no unit test can observe the sequence a phone
sees. This stands up both servers, runs the real binary, and asserts on the
recorded calls: one sendMessage, then editMessageText calls carrying the same
message_id, and a final edit whose text is the whole answer.

Hermetic by construction: everything lives under a temp XDEV_AGENT_DIR, so the
operator's ~/.xdev/agent/config.yml, models.yml and gateway.token are never
read or written. The bot token below is invented.

Usage:  python3 scripts/gateway-stream-drive.py [path-to-xdev]
Exit 0 when the chat saw a growing message; 1 with the recorded transcript.
"""

import http.server
import json
import os
import pathlib
import socketserver
import subprocess
import sys
import tempfile
import threading
import time

TOKEN = "111111111:INVENTEDtokenINVENTEDtokenINVENTEDtoken"
CHAT = 4242
# The answer streams in four pieces with a pause between them, so the preview
# has time to paint mid-turn rather than only at the end.
PIECES = ["**Fleet status**\n\n", "xdev  7 commits\n", "be    3 commits\n", "done.\n"]


class BotAPI(http.server.BaseHTTPRequestHandler):
    """getMe / getUpdates / sendMessage / editMessageText / sendChatAction."""

    sent_ids = []
    calls = []
    updates = [{
        "update_id": 1,
        "message": {"message_id": 1, "date": 0, "text": "status?",
                    "chat": {"id": CHAT, "type": "private"},
                    "from": {"id": CHAT, "username": "tester"}},
    }]
    served = False
    lock = threading.Lock()

    def log_message(self, *a):
        pass

    def _ok(self, result):
        body = json.dumps({"ok": True, "result": result}).encode()
        try:
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except BrokenPipeError:
            # The daemon was terminated while a long poll was open — that is
            # how this script stops it, not a failure of the drive.
            pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        payload = json.loads(self.rfile.read(n) or b"{}")
        method = self.path.rsplit("/", 1)[-1]
        with BotAPI.lock:
            BotAPI.calls.append((method, payload))
        if method == "getMe":
            return self._ok({"id": 42, "username": "faketestbot", "first_name": "fake"})
        if method == "getUpdates":
            with BotAPI.lock:
                if BotAPI.served:
                    return self._ok([])
                BotAPI.served = True
                out = BotAPI.updates
            return self._ok(out)
        if method == "sendMessage":
            # The id the API RETURNS is the edit target, so it is recorded
            # separately from the request payload (a request never carries
            # one) — that is what the assertion compares against.
            with BotAPI.lock:
                BotAPI.sent_ids.append(1000 + len(BotAPI.sent_ids) + 1)
                n = BotAPI.sent_ids[-1]
            return self._ok({"message_id": n})
        return self._ok(True)


class Provider(http.server.BaseHTTPRequestHandler):
    """A minimal openai-completions SSE endpoint that streams PIECES."""

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(n)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for piece in PIECES:
            chunk = {"id": "c1", "object": "chat.completion.chunk", "created": 0,
                     "model": "mock-1",
                     "choices": [{"index": 0, "delta": {"content": piece}}]}
            self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
            self.wfile.flush()
            time.sleep(0.6)
        done = {"id": "c1", "object": "chat.completion.chunk", "created": 0, "model": "mock-1",
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}
        self.wfile.write(b"data: " + json.dumps(done).encode() + b"\n\n")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    xdev = sys.argv[1] if len(sys.argv) > 1 else "xdev"
    bot = Server(("127.0.0.1", 0), BotAPI)
    prov = Server(("127.0.0.1", 0), Provider)
    threading.Thread(target=bot.serve_forever, daemon=True).start()
    threading.Thread(target=prov.serve_forever, daemon=True).start()
    bot_url = f"http://127.0.0.1:{bot.server_address[1]}"
    prov_url = f"http://127.0.0.1:{prov.server_address[1]}"

    tmp = pathlib.Path(tempfile.mkdtemp(prefix="xdev-gateway-stream-"))
    (tmp / "models.yml").write_text(
        "providers:\n"
        "  mock:\n"
        f"    baseUrl: {prov_url}/v1\n"
        "    api: openai-completions\n"
        "    apiKey: invented-not-a-secret\n"
        "    models:\n"
        "      - id: mock-1\n"
        "        contextWindow: 100000\n"
    )
    (tmp / "config.yml").write_text(
        "gateway:\n"
        "  enabled: true\n"
        f"  allowedChats: ['{CHAT}']\n"
        f"  workspace: {tmp}\n"
        "defaultModel: mock/mock-1\n"
    )
    env = dict(os.environ)
    env.update({"XDEV_AGENT_DIR": str(tmp), "TELEGRAM_BOT_TOKEN": TOKEN,
                "NO_COLOR": "1", "TERM": "dumb"})
    proc = subprocess.Popen([xdev, "gateway", "run", "--api", bot_url],
                            env=env, cwd=str(tmp),
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    deadline = time.time() + 90
    try:
        # Wait for the TURN to finish, not for a paint count: killing the
        # daemon mid-turn is what a real interruption looks like, and it would
        # make the final edit fail for a reason unrelated to the feature.
        while time.time() < deadline:
            with BotAPI.lock:
                edits = [p for m, p in BotAPI.calls if m == "editMessageText"]
            if edits and "done." in edits[-1].get("text", ""):
                time.sleep(1.0)  # let the last paint settle
                break
            time.sleep(0.3)
    finally:
        proc.terminate()
        try:
            log = proc.communicate(timeout=10)[0]
        except subprocess.TimeoutExpired:
            proc.kill()
            log = proc.communicate()[0]
    bot.shutdown()
    prov.shutdown()

    sends = [(m, p) for m, p in BotAPI.calls if m == "sendMessage"]
    edits = [(m, p) for m, p in BotAPI.calls if m == "editMessageText"]
    print(f"sendMessage={len(sends)} editMessageText={len(edits)}")
    for m, p in sends + edits:
        print(f"  {m} id={p.get('message_id')} text={p.get('text')!r}")
    print("--- gateway log ---")
    print(log.strip()[-2000:])

    ok = True
    if len(sends) < 1:
        print("FAIL: the answer never reached the chat")
        ok = False
    if len(edits) < 2:
        print("FAIL: the answer did not grow in place (want >= 2 edits)")
        ok = False
    first_sent_id = BotAPI.sent_ids[0] if BotAPI.sent_ids else None
    if edits and first_sent_id is not None and edits[-1][1].get("message_id") != first_sent_id:
        print("FAIL: the final edit targeted a different message than the first send")
        ok = False
    whole = "".join(PIECES).replace("**", "")
    if edits:
        final = edits[-1][1].get("text", "")
        for word in ["Fleet status", "7 commits", "3 commits", "done."]:
            if word not in final:
                print(f"FAIL: the final message is missing {word!r}")
                ok = False
    if len(sends) > 1:
        print("FAIL: the finished answer was sent as a new message instead of edited in place")
        ok = False
    print("PASS" if ok else "FAIL", f"(want the whole answer {whole!r} in one message)")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
