#!/usr/bin/env python3
"""Mechanical launch-flag diff: omp --help vs xdev -h (see docs/parity/cli.md).

Usage:  python3 docs/parity/cli-flagdiff.py [xdev-binary]
Default xdev binary is /tmp/xdev-test (build with:
    go build -o /tmp/xdev-test ./cmd/xdev).
"""
import json
import os
import re
import subprocess
import sys

XDEV = sys.argv[1] if len(sys.argv) > 1 else "/tmp/xdev-test"
ENV = dict(os.environ)
ENV.setdefault("XDEV_AGENT_DIR", "/tmp/parity-cli")


def omp_flags():
    out = subprocess.run(["omp", "--help"], capture_output=True, text=True, env=ENV).stdout
    got = {}
    for line in out.splitlines():
        m = re.match(r"\s*((?:-[a-zA-Z0-9],\s*)?--[a-z0-9-]+)(?:=<?value>?)?\s{2,}(.*)", line)
        if m:
            names = [n.strip().lstrip("-") for n in m.group(1).split(",")]
            got[names[-1]] = {"aliases": names[:-1], "desc": m.group(2).strip()}
    return got


def xdev_flags():
    out = subprocess.run([XDEV, "-h"], capture_output=True, text=True, env=ENV).stderr
    got = {}
    for line in out.splitlines():
        m = re.match(r"\s+-([a-zA-Z0-9-]+)(?:\s+(string|int|value|handoff|duration))?\s*$", line)
        if m:
            got[m.group(1)] = {"type": m.group(2) or "bool"}
    return got


def main():
    omp, xdev = omp_flags(), xdev_flags()
    shared, missing, extra = sorted(set(omp) & set(xdev)), sorted(set(omp) - set(xdev)), sorted(set(xdev) - set(omp))
    print(f"omp flags: {len(omp)}  xdev flags: {len(xdev)}")
    print(f"\nMISSING in xdev ({len(missing)}):")
    for f in missing:
        print(f"  --{f:26s} {omp[f]['desc']}")
    print(f"\nXDEV-only ({len(extra)}): " + ", ".join("--" + e for e in extra))
    print(f"\nSHARED ({len(shared)}): " + ", ".join(sorted(shared)))
    json.dump({"omp": omp, "xdev": xdev, "missing": missing, "extra": extra},
              open("/tmp/parity-cli/flags.json", "w"), indent=1)


if __name__ == "__main__":
    main()
