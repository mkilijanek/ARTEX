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

upstream = Path(result["Dir"])
# The snapshot intentionally omits some upstream tests, but never production Go.
production = {str(p.relative_to(upstream)) for p in upstream.rglob("*.go")
              if not p.name.endswith("_test.go")}
listed = {p for p in manifest["original_sha256"] if p.endswith(".go")}
if production != listed:
    raise SystemExit("upstream production inventory mismatch: " + repr(production ^ listed))
for rel, expected in manifest["extra_sha256"].items():
    source = upstream / rel
    if source.is_file() and hashlib.sha256(source.read_bytes()).hexdigest() != expected:
        raise SystemExit("copied upstream test differs: " + rel)
print("Verified complete production inventory and copied upstream tests")
