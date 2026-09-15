#!/usr/bin/env python3
"""Render the real xdev TUI to a README screenshot (truecolor, cell-accurate).

    scripts/tui-shot.py [--rows 28] [--cols 100] [--width 1600]
                        [--bin ./xdev] [--out assets/screenshots/welcome.png]

How it works, since a plain screen grab is not available in every environment:
run `xdev tui` in a detached tmux session, ask tmux for the pane with its
colors (`capture-pane -p -e` re-emits the SGR runs for the live cell grid), and
repaint that grid as an SVG, one glyph per cell, then rasterize it with
rsvg-convert. Nothing is faked: every cell comes from what the TUI actually
painted. Colors survive only when the TUI is not in NO_COLOR mode, so the
child is started with NO_COLOR unset and COLORTERM=truecolor.

Requires: tmux, and rsvg-convert for the PNG step (brew install tmux librsvg).
Pass --keep-svg to also keep the vector, or --out shot.svg for the vector alone.
The welcome screen animates (a sheen sweeps the wordmark), so --tries frames
are sampled and the one with the sweep off the mark is kept.
"""
import argparse
import os
import re
import shutil
import subprocess
import sys
import time
import unicodedata

SGR = re.compile(r"\x1b\[([0-9;]*)m")
FONTS = "'JetBrainsMono NFM','JetBrains Mono',Menlo,monospace"


def wide(ch):
    return 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1


def apply_sgr(style, params, default_fg, default_bg, palette):
    """Fold one SGR parameter list into the running style dict. 38/48 consume
    the following 2 (truecolor) or 1 (256-color) tokens."""
    toks = (params or "0").split(";")
    i = 0
    while i < len(toks):
        p = toks[i]
        if p in ("", "0"):
            style.update(fg=default_fg, bg=default_bg, bold=False, rev=False)
        elif p == "1":
            style["bold"] = True
        elif p in ("22", "21"):
            style["bold"] = False
        elif p == "7":
            style["rev"] = True
        elif p == "27":
            style["rev"] = False
        elif p == "39":
            style["fg"] = default_fg
        elif p == "49":
            style["bg"] = default_bg
        elif p in ("38", "48") and toks[i + 1 : i + 2] == ["2"]:
            style["fg" if p == "38" else "bg"] = "#%02x%02x%02x" % tuple(
                int(v) for v in toks[i + 2 : i + 5]
            )
            i += 4
        elif p in ("38", "48") and toks[i + 1 : i + 2] == ["5"]:
            style["fg" if p == "38" else "bg"] = palette[int(toks[i + 2])]
            i += 2
        i += 1


def xterm_palette():
    """The 16 system colors, the 6x6x6 cube, then the 24-step gray ramp."""
    sysc = [
        (0, 0, 0), (205, 0, 0), (0, 205, 0), (205, 205, 0), (0, 0, 238),
        (205, 0, 205), (0, 205, 205), (229, 229, 229), (127, 127, 127),
        (255, 0, 0), (0, 255, 0), (255, 255, 0), (92, 92, 255),
        (255, 0, 255), (0, 255, 255), (255, 255, 255),
    ]
    cube = [(r, g, b) for r in (0, 95, 135, 175, 215, 255)
            for g in (0, 95, 135, 175, 215, 255) for b in (0, 95, 135, 175, 215, 255)]
    gray = [(v, v, v) for v in range(8, 239, 10)]
    return ["#%02x%02x%02x" % c for c in sysc + cube + gray]


def build_rows(grid, default_fg, default_bg):
    """Turn captured lines into rows of (col, char, style) cells."""
    palette = xterm_palette()
    rows = []
    for line in grid.rstrip("\n").split("\n"):
        style = {"fg": default_fg, "bg": default_bg, "bold": False, "rev": False}
        cells, col = [], 0
        for tok in re.split(r"(\x1b\[[0-9;]*m)", line):
            if m := SGR.fullmatch(tok):
                apply_sgr(style, m.group(1), default_fg, default_bg, palette)
                continue
            for ch in tok:
                cells.append((col, ch, dict(style)))
                col += wide(ch)
        rows.append(cells)
    return rows


def to_svg(rows, font_px, bg):
    cw, ch_h = font_px * 0.6, font_px * 1.42  # JetBrains Mono advance / line height
    W = max((c + wide(ch) for r in rows for c, ch, _ in r), default=1)
    H = len(rows)
    esc = lambda s: s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    out = [
        '<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">'
        % (int(W * cw), int(H * ch_h), int(W * cw), int(H * ch_h)),
        '<rect width="100%%" height="100%%" fill="%s"/>' % bg,
        '<g font-family="%s" font-size="%g">' % (FONTS, font_px),
    ]
    glyphs = 0
    for y, cells in enumerate(rows):
        for c, ch, st in cells:
            plate = st["fg"] if st["rev"] else st["bg"]
            if plate != bg:
                out.append('<rect x="%g" y="%g" width="%g" height="%g" fill="%s"/>'
                           % (c * cw, y * ch_h, cw, ch_h, plate))
            if ch == " ":
                continue
            ink = st["bg"] if st["rev"] else st["fg"]
            # textLength pins each glyph to its cell advance, so a glyph
            # substituted from another font can never skew the grid.
            out.append(
                '<text x="%g" y="%g" textLength="%g" lengthAdjust="spacingAndGlyphs" fill="%s"%s>%s</text>'
                % (c * cw, (y + 0.85) * ch_h, cw * wide(ch), ink,
                   ' font-weight="700"' if st["bold"] else "", esc(ch))
            )
            glyphs += 1
    out += ["</g>", "</svg>"]
    return "\n".join(out), glyphs


def sheen_score(grid):
    """Cost of a frame: how many block-drawing glyphs sit in the sheen's lit
    band, which the TUI paints as a TextPrimary plate with terminal-ink glyphs.
    The sweep inverts that band as it crosses the wordmark, so a frame caught
    mid-sweep shows the mark half turned to white — unreadable in a still.
    Zero means the sweep is off the art."""
    return sum(1 for r in build_rows(grid, "#e1e1e1", "#141414") for _, ch, st in r
               if "\u2580" <= ch <= "\u259f" and st["bg"] == "#e1e1e1")


def capture(bin_path, cols, rows, session, wait, tries):
    """Run the TUI in a throwaway tmux session and return its colored grid.
    The welcome screen animates, so frames are captured and the cleanest kept
    (see sheen_score); the session is torn down once."""
    if shutil.which("tmux") is None:
        sys.exit("tmux is required (brew install tmux)")
    subprocess.run(["tmux", "kill-session", "-t", session], capture_output=True)
    subprocess.run(["tmux", "new-session", "-d", "-s", session, "-x", str(cols), "-y", str(rows)], check=True)
    env = "env -u NO_COLOR COLORTERM=truecolor "  # NO_COLOR would flatten it to mono
    subprocess.run(["tmux", "send-keys", "-t", session, f"{env}{bin_path} tui", "Enter"], check=True)
    time.sleep(wait)  # let the first frame paint
    best, frames = None, 0
    try:
        for _ in range(tries):
            frames += 1
            grid = subprocess.run(["tmux", "capture-pane", "-p", "-e", "-t", session],
                                  capture_output=True, text=True, check=True).stdout
            if "\x1b[" not in grid:
                print("warning: tmux returned no color information; the PNG will be monochrome",
                      file=sys.stderr)
                return grid
            score = sheen_score(grid)
            if best is None or score < best[0]:
                best = (score, grid)
            if score == 0:
                break
            time.sleep(0.4)  # next sweep phase
    finally:
        subprocess.run(["tmux", "kill-session", "-t", session], capture_output=True)
    print("cleanest frame: sheen cost %d (of %d captured)" % (best[0], frames), file=sys.stderr)
    return best[1]


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--bin", default="./xdev", help="xdev binary to screenshot")
    ap.add_argument("--cols", type=int, default=100)
    ap.add_argument("--rows", type=int, default=28)
    ap.add_argument("--wait", type=float, default=6.0, help="seconds to let the TUI paint")
    ap.add_argument("--font-px", type=float, default=20.0)
    ap.add_argument("--width", type=int, default=1600, help="PNG width in px")
    ap.add_argument("--out", default="assets/screenshots/welcome.png")
    ap.add_argument("--keep-svg", action="store_true", help="also keep the intermediate SVG")
    ap.add_argument("--tries", type=int, default=8, help="frames to sample; the cleanest is kept")
    ap.add_argument("--session", default="xdev-shot")
    args = ap.parse_args()

    grid = capture(args.bin, args.cols, args.rows, args.session, args.wait, args.tries)
    # GrokNight's canvas, so the shot needs no window chrome around it.
    svg, glyphs = to_svg(build_rows(grid, "#e1e1e1", "#141414"), args.font_px, "#141414")
    out = args.out
    os.makedirs(os.path.dirname(out) or ".", exist_ok=True)
    if out.endswith(".svg"):
        open(out, "w", encoding="utf-8").write(svg)
        print("%s (%d glyphs)" % (out, glyphs))
        return
    rsvg = shutil.which("rsvg-convert")
    if rsvg is None:
        sys.exit("rsvg-convert is required for the PNG step (brew install librsvg)")
    svg_path = out + ".svg"
    open(svg_path, "w", encoding="utf-8").write(svg)
    subprocess.run([rsvg, "-w", str(args.width), svg_path, "-o", out], check=True)
    if args.keep_svg:
        print("%s + %s (%d glyphs)" % (out, svg_path, glyphs))
    else:
        os.remove(svg_path)
        print("%s (%d glyphs)" % (out, glyphs))


if __name__ == "__main__":
    main()
