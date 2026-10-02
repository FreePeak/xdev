"""xdev eval kernel — NDJSON driver (issue #47).

Reads one JSON request per line on stdin, executes the cell in a persistent
namespace, and writes one JSON frame per line on stdout:

  {"type":"started","python":"3.14.7"}            once, at boot
  {"type":"started","id":N}                       cell begins
  {"type":"stdout","id":N,"text":"..."}           printed output (per write)
  {"type":"display","id":N,"mime":"...","data":"...","enc":"base64"?}
  {"type":"result","id":N,"value":"repr","mime":{...}}   last expression
  {"type":"error","id":N,"name":"...","value":"...","traceback":"..."}
  {"type":"done","id":N,"status":"ok"|"error"}

Requests: {"id":N,"code":"..."} runs a cell, {"id":N,"cmd":"reset"} wipes the
namespace.

The kernel exits when stdin reaches EOF, so a dead xdev process never leaves
an orphan kernel behind.
"""

import ast
import base64
import json
import queue
import sys
import threading
import traceback

ENV = {"__name__": "__main__", "__builtins__": __builtins__}

# Frames are written on the real stdout; sys.stdout is rebound to a frame
# emitter while a cell runs, so print() is captured.
_OUT = sys.__stdout__

BINARY_MIMES = ("image/png", "image/jpeg", "image/gif", "application/octet-stream")

# Max characters of one stdout/stderr frame (mirrors the Go reader's limit).
_CHUNK = 65536


# --- host tool calls (M13 #268) -------------------------------------------
#
# A cell reaches the harness through one object, `tools`: the call leaves the
# kernel as a frame and the answer comes back on stdin, so the Go side can run
# it on its own goroutine. That is the whole point — the kernel never holds its
# lock across a dispatch, and a cell cancelled mid-call gets an exception
# instead of hanging on a host answer that will never come.
#
# ONE reader thread owns stdin. Two would race for the same fd, and the answer
# path has to work exactly when the request path is busy, which is while a cell
# is running — so cell requests are queued for the main loop, not read there.
_REQUESTS = queue.Queue()
_PENDING = {}
_PENDING_LOCK = threading.Lock()
_CALL_LOCK = threading.Lock()
_NEXT_CALL = [0]
_CUR_CELL = [0]


class ToolError(RuntimeError):
    """The harness answered a tool call with a failure the cell must see."""


def _next_call_id():
    with _CALL_LOCK:
        _NEXT_CALL[0] += 1
        return _NEXT_CALL[0]


def _invoke(name, args, kwargs):
    """Emit one tool_call frame and wait for its answer."""
    if len(args) > 1:
        raise TypeError("tools.%s takes one dict or keyword arguments" % name)
    if args and kwargs:
        raise TypeError("tools.%s: pass a dict or keywords, not both" % name)
    if args:
        if not isinstance(args[0], dict):
            raise TypeError("tools.%s expects a dict of arguments" % name)
        params = args[0]
    else:
        params = kwargs
    call_id = _next_call_id()
    slot = {"event": threading.Event(), "answer": None}
    with _PENDING_LOCK:
        _PENDING[call_id] = slot
    try:
        emit({"type": "tool_call", "id": _CUR_CELL[0], "call_id": call_id,
              "name": name, "args": params})
        # Polled, not waited on: the cell clock and the agent abort both land
        # as signals on THIS thread, and a blocked wait() would only surface
        # them once the host answered. Polling turns both into a
        # KeyboardInterrupt right here, which run_cell already reports.
        while not slot["event"].wait(0.25):
            pass
    finally:
        with _PENDING_LOCK:
            _PENDING.pop(call_id, None)
    answer = slot["answer"] or {}
    if answer.get("error"):
        raise ToolError("%s: %s" % (name, answer["error"]))
    return {"text": answer.get("text", ""),
            "details": answer.get("details"),
            "is_error": bool(answer.get("is_error"))}


class _Tools:
    """tools.read({...}) — every name dispatches to the harness, not to the
    local registry, so a bridged call takes the same plan-mode, approval and
    hook path a direct model call takes. An unknown name is dispatched too and
    comes back as the harness's own error: one failure shape for the cell.

    ONE instance for the kernel's lifetime, reading the running cell id at call
    time — a per-cell object would be one stale reference away from naming the
    wrong cell, since the namespace deliberately outlives the cell that made
    it."""

    def __getattr__(self, name):
        if name.startswith("_"):
            raise AttributeError(name)

        def call(*args, **kwargs):
            return _invoke(name, args, kwargs)

        call.__name__ = str(name)
        return call


_TOOLS = _Tools()


def _bind_tools(env):
    env["tools"] = _TOOLS
    return env


def _stdin_loop():
    """The one stdin reader: answers tool calls in place, queues cell requests."""
    while True:
        try:
            line = sys.__stdin__.readline()
        except (KeyboardInterrupt, ValueError):
            continue
        if not line:
            _REQUESTS.put(None)  # EOF: the parent is gone, main() returns
            return
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except ValueError:
            continue
        if not isinstance(req, dict):
            continue
        if req.get("cmd") == "tool_result":
            with _PENDING_LOCK:
                slot = _PENDING.get(req.get("call_id"))
            if slot is not None:
                slot["answer"] = req.get("result") or {}
                slot["event"].set()
            continue
        _REQUESTS.put(req)


def emit(obj):
    _OUT.write(json.dumps(obj) + "\n")
    _OUT.flush()


class _Stream:
    """Emits stdout/stderr frames for one cell."""

    encoding = "utf-8"

    def __init__(self, kind, cell_id):
        self.kind = kind
        self.id = cell_id

    def write(self, s):
        if s:
            # Chunked so no single frame can overflow the reader's line buffer.
            for i in range(0, len(s), _CHUNK):
                emit({"type": self.kind, "id": self.id, "text": s[i:i + _CHUNK]})
        return len(s)

    def writable(self):
        return True

    def flush(self):
        pass

    def isatty(self):
        return False


def _encode(mime, data):
    if mime in BINARY_MIMES:
        if isinstance(data, (bytes, bytearray)):
            data = bytes(data)
        else:
            data = str(data).encode("utf-8", "replace")
        return base64.b64encode(data).decode("ascii"), True
    if isinstance(data, (bytes, bytearray)):
        return bytes(data).decode("utf-8", "replace"), False
    if isinstance(data, str):
        return data, False
    return repr(data), False


def mime_bundle(obj):
    """Rich representation of obj: text/plain plus any _repr_*_ protocols."""
    bundle = {}
    for attr, mime in (
        ("_repr_html_", "text/html"),
        ("_repr_markdown_", "text/markdown"),
        ("_repr_svg_", "image/svg+xml"),
        ("_repr_png_", "image/png"),
        ("_repr_jpeg_", "image/jpeg"),
        ("_repr_json_", "application/json"),
    ):
        try:
            value = getattr(obj, attr, None)
            if callable(value):
                value = value()
            if value:
                bundle[mime] = value
        except Exception:
            continue
    try:
        bundle["text/plain"] = repr(obj)
    except Exception:
        bundle["text/plain"] = "<unreprable object>"
    return bundle


def display(*objs, **_kwargs):
    """Prelude helper: emit a display frame per representation."""
    for obj in objs:
        bundle = mime_bundle(obj)
        rich = [m for m in bundle if m != "text/plain"]
        for mime in rich or ["text/plain"]:
            data, b64 = _encode(mime, bundle[mime])
            frame = {"type": "display", "id": _CUR_CELL[0], "mime": mime, "data": data}
            if b64:
                frame["enc"] = "base64"
            emit(frame)


class _Display:

    def __getattr__(self, _name):
        return display

    def __call__(self, *objs, **kwargs):
        display(*objs, **kwargs)


ENV["display"] = display
ENV["get_ipython"] = lambda: _Display()


def _emit_value(cell_id, obj):
    if obj is None:
        return
    bundle = mime_bundle(obj)
    rich = {m: _encode(m, v)[0] for m, v in bundle.items() if m != "text/plain"}
    for mime, value in list(rich.items()):
        frame = {"type": "display", "id": cell_id, "mime": mime, "data": value}
        if mime in BINARY_MIMES:
            frame["enc"] = "base64"
        emit(frame)
    frame = {"type": "result", "id": cell_id, "value": bundle.get("text/plain", "")}
    if rich:
        frame["mimes"] = sorted(rich)
    emit(frame)


def _exec(code_obj, mode):
    if mode == "eval":
        return eval(code_obj, ENV)  # noqa: S307 — the kernel's whole job
    exec(code_obj, ENV)  # noqa: S102
    return None


def run_cell(cell_id, code):
    global _CUR_CELL
    _CUR_CELL[0] = cell_id
    _bind_tools(ENV)
    emit({"type": "started", "id": cell_id})
    out, err = _Stream("stdout", cell_id), _Stream("stderr", cell_id)
    real_out, real_err = sys.stdout, sys.stderr
    sys.stdout, sys.stderr = out, err
    status = "ok"
    try:
        tree = ast.parse(code, "<cell>", "exec")
        body = list(tree.body)
        last = None
        # REPL semantics: a trailing expression is evaluated and its value
        # reported as the cell result (None stays silent, like IPython).
        if body and isinstance(body[-1], ast.Expr):
            last = ast.Expression(body=body.pop().value)
        if body:
            _exec(compile(ast.Module(body=body, type_ignores=[]), "<cell>", "exec"), "exec")
        if last is not None:
            _emit_value(cell_id, _exec(compile(last, "<cell>", "eval"), "eval"))
    except KeyboardInterrupt:
        status = "error"
        emit({"type": "error", "id": cell_id, "name": "KeyboardInterrupt",
              "value": "interrupted", "traceback": "KeyboardInterrupt: interrupted"})
    except SystemExit as exc:
        status = "error"
        emit({"type": "error", "id": cell_id, "name": "SystemExit", "value": str(exc),
              "traceback": "SystemExit: %s" % (exc,)})
    except BaseException as exc:  # noqa: BLE001 — report, never die
        status = "error"
        tb = exc.__traceback__
        while tb is not None and tb.tb_frame.f_code.co_name in ("run_cell", "_exec"):
            tb = tb.tb_next
        emit({"type": "error", "id": cell_id, "name": type(exc).__name__,
              "value": str(exc),
              "traceback": "".join(traceback.format_exception(type(exc), exc, tb))})
    finally:
        sys.stdout, sys.stderr = real_out, real_err
    emit({"type": "done", "id": cell_id, "status": status})


def reset_env():
    ENV.clear()
    ENV.update({"__name__": "__main__", "__builtins__": __builtins__,
                "display": display, "get_ipython": lambda: _Display(),
                "tools": _TOOLS})


def main():
    emit({"type": "started", "python": sys.version.split()[0]})
    threading.Thread(target=_stdin_loop, daemon=True).start()
    while True:
        req = _REQUESTS.get()
        if req is None:
            return  # EOF: the parent (xdev) is gone — exit, leave no orphan
        cur_id = req.get("id") or 0
        if req.get("cmd") == "reset":
            reset_env()
            emit({"type": "done", "id": cur_id, "status": "ok"})
        else:
            try:
                run_cell(cur_id, req.get("code") or "")
            except KeyboardInterrupt:
                emit({"type": "done", "id": cur_id, "status": "error"})


main()
