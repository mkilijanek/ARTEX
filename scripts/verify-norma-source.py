#!/usr/bin/env python3
"""Verify declared upstream hashes against the Go checksum database module download."""
import hashlib
import json
import os
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
manifest = json.loads((root / "third_party/norma/SOURCE.json").read_text())
env = dict(os.environ, GOSUMDB="sum.golang.org", GOPRIVATE="", GONOSUMDB="", GONOPROXY="")
result = json.loads(subprocess.check_output(
    ["go", "mod", "download", "-json", manifest["module"] + "@" + manifest["version"]],
    env=env, cwd=root))
if result.get("Sum") != manifest["module_sum"]:
    raise SystemExit("upstream module checksum mismatch")
for rel, expected in manifest["original_sha256"].items():
    actual = hashlib.sha256((Path(result["Dir"]) / rel).read_bytes()).hexdigest()
    if actual != expected:
        raise SystemExit("upstream source mismatch: " + rel)
print("Verified upstream module sum and all original file hashes")
