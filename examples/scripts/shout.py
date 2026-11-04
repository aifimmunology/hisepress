#!/usr/bin/env python3
"""A tiny exec step: read JSON from stdin, print JSON to stdout."""
import json
import sys

req = json.load(sys.stdin)
text = req["args"]["text"]
print(json.dumps({"upper": text.upper(), "length": len(text)}))
