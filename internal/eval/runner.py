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
import sys
import traceback

ENV = {"__name__": "__main__", "__builtins__": __builtins__}

# Frames are written on the real stdout; sys.stdout is rebound to a frame
# emitter while a cell runs, so print() is captured.
_OUT = sys.__stdout__

BINARY_MIMES = ("image/png", "image/jpeg", "image/gif", "application/octet-stream")

# Max characters of one stdout/stderr frame (mirrors the Go reader's limit).
_CHUNK = 65536

CUR_ID = 0


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
            frame = {"type": "display", "id": CUR_ID, "mime": mime, "data": data}
            if b64:
                frame["enc"] = "base64"
            emit(frame)


class _Display:
    """IPython-compatible no-op shell: `from IPython.display import ...` is
    common in the wild, but only the prelude display() above is wired."""

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
                "display": display, "get_ipython": lambda: _Display()})


def main():
    global CUR_ID
    emit({"type": "started", "python": sys.version.split()[0]})
    while True:
        try:
            line = sys.stdin.readline()
        except KeyboardInterrupt:
            continue
        if not line:
            return  # EOF: the parent (xdev) is gone — exit, leave no orphan
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except ValueError:
            continue
        if not isinstance(req, dict):
            continue
        CUR_ID = req.get("id") or 0
        cmd = req.get("cmd") or "exec"
        if cmd == "reset":
            reset_env()
            emit({"type": "done", "id": CUR_ID, "status": "ok"})
        else:
            try:
                run_cell(CUR_ID, req.get("code") or "")
            except KeyboardInterrupt:
                emit({"type": "done", "id": CUR_ID, "status": "error"})


main()
