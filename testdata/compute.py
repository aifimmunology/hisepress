#!/usr/bin/env python3
"""Test helper for hisepress exec steps.

Reads the step's JSON document from stdin, does a little work, and prints a
JSON result. args.mode selects behavior: ok (default), fail, sleep, text.
"""
import json
import os
import sys
import time

req = json.load(sys.stdin)
args = req.get("args", {})
mode = args.get("mode", "ok")
print("compute.py running, mode=%s" % mode, file=sys.stderr)

if mode == "fail":
    print("something went wrong", file=sys.stderr)
    sys.exit(3)
if mode == "sleep":
    time.sleep(30)
if mode == "text":
    print("this is not json")
    sys.exit(0)

out = {
    "sum": float(args.get("a", 0)) + float(args.get("b", 0)),
    "args": args,
    "vars": req.get("vars"),
    "outDir": req.get("outDir"),
    "envOutDir": os.environ.get("HISEPRESS_OUT_DIR"),
    "envArgA": os.environ.get("HISEPRESS_ARG_A"),
    "custom": os.environ.get("CUSTOM"),
    "cwd": os.path.basename(os.getcwd()),
}
if req.get("outDir"):
    path = os.path.join(req["outDir"], "compute-output.txt")
    with open(path, "w") as f:
        f.write("sum=%s\n" % out["sum"])
    out["_artifacts"] = [{"type": "text", "path": path}]
print(json.dumps(out))
