#!/usr/bin/env python3
"""Choose CRW checks from Git evidence; an unregistered path cannot pass CI."""
import argparse
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys

DOCS = {"README.md", "CONTRIBUTING.md", "POLICY.md", "SECURITY.md", "AGENTS.md", "LICENSE"}
FULL = {".gitignore", ".gitleaks.toml", "pyproject.toml", "uv.lock", "plugins/crw/LICENSE",
        "conftest.py"}
PREFIXES = ("scripts/", "packages/", "plugins/crw/wiring/", "plugins/crw/.codex-plugin/",
            ".agents/", ".github/", "contract/", "docs/port/")
REASONS = {"paths", "empty", "base-unavailable", "dispatch"}
FIELDS = {"version", "event", "base", "head", "base_ref", "ref", "changed", "unknown",
          "unsafe", "reason", "selected"}


def classify(path):
    if path in DOCS or (PurePosixPath(path).parent == PurePosixPath("docs")
                        and path.endswith(".md")):
        return "docs"
    if path == "skills" or path.startswith("plugins/crw/skills/"):
        return "skill"
    if path in FULL or path.startswith(PREFIXES):
        return "full"
    return "unknown"


def context(event, base_ref, ref):
    if event == "pull_request":
        if not base_ref or base_ref == "main":
            raise ValueError("PRs must target a development branch; main is a release mirror")
    elif event == "push":
        if ref != "refs/heads/dev":
            raise ValueError("Only dev push CI supplies release evidence")
    elif event != "workflow_dispatch":
        raise ValueError("Unsupported CI event")


def selected(changed, unknown, unsafe, reason):
    kinds = {classify(path) for path in changed}
    full = reason != "paths" or bool(unknown or unsafe) or "full" in kinds
    return {"tests": full or "skill" in kinds, "packages": full}


def validate_selection(value):
    if not isinstance(value, dict) or set(value) != FIELDS or type(value["version"]) is not int or value["version"] != 1:
        raise ValueError("Invalid selection schema")
    for key in ("event", "base", "head", "base_ref", "ref", "reason"):
        if not isinstance(value[key], str):
            raise ValueError(f"Invalid selection {key}")
    if not re.fullmatch(r"[0-9a-f]{40}", value["head"]):
        raise ValueError("Invalid candidate SHA")
    if value["base"] and not re.fullmatch(r"[0-9a-f]{40}", value["base"]):
        raise ValueError("Invalid base SHA")
    context(value["event"], value["base_ref"], value["ref"])
    for key in ("changed", "unknown", "unsafe"):
        paths = value[key]
        if not isinstance(paths, list) or any(not isinstance(p, str) or not p
                or p.startswith("/") or ".." in p.split("/") for p in paths):
            raise ValueError(f"Invalid {key} paths")
        if paths != sorted(set(paths)):
            raise ValueError(f"Unordered or duplicated {key} paths")
    if value["reason"] not in REASONS:
        raise ValueError("Invalid selection reason")
    if value["reason"] == "paths" and (not value["changed"] or not value["base"]):
        raise ValueError("Path selection requires a base and nonempty diff")
    if value["event"] == "workflow_dispatch" and value["reason"] != "dispatch":
        raise ValueError("Manual CI must run all checks")
    if not set(value["unsafe"]).issubset(value["changed"]):
        raise ValueError("Unsafe paths must belong to the diff")
    jobs = value["selected"]
    if not isinstance(jobs, dict) or set(jobs) != {"tests", "packages"} or any(type(v) is not bool for v in jobs.values()):
        raise ValueError("Selection outputs must be booleans")
    if jobs != selected(value["changed"], value["unknown"], value["unsafe"], value["reason"]):
        raise ValueError("Selected jobs disagree with path evidence")
    if value["unknown"] or any(classify(p) == "unknown" for p in value["changed"]):
        raise ValueError("Unregistered paths require an explicit verification mapping")
    return value


def git(root, *args):
    return subprocess.check_output(["git", *args], cwd=root, stderr=subprocess.PIPE)


def resolve(root, revision):
    return git(root, "rev-parse", "--verify", "--end-of-options", revision + "^{commit}").decode().strip()


def tree(root, revision):
    entries = {}
    for record in git(root, "ls-tree", "-rz", revision).split(b"\0"):
        if record:
            metadata, path = record.split(b"\t", 1)
            entries[path.decode()] = metadata.split()[0].decode()
    return entries


def select(root, *, base, head, event, base_ref, ref):
    context(event, base_ref, ref)
    candidate = resolve(root, head)
    inventory = tree(root, candidate)
    before = {}
    base_sha = ""
    changed = []
    reason = "paths"
    try:
        if not base or base == "0" * 40:
            raise ValueError("No comparison base")
        base_sha = resolve(root, base)
        before = tree(root, base_sha)
        changed = sorted(set(filter(None, git(root, "diff", "--no-renames", "--name-only",
                      "-z", base_sha, candidate, "--").decode().split("\0"))))
        if not changed:
            reason = "empty"
    except (ValueError, subprocess.CalledProcessError):
        reason = "base-unavailable"
    if event == "workflow_dispatch":
        reason = "dispatch"
    unknown = sorted(p for p in set(inventory) | set(changed) if classify(p) == "unknown")
    unsafe = sorted(p for p in changed if
                    (p in before and p in inventory and before[p] != inventory[p])
                    or (classify(p) == "docs" and any(m != "100644" for m in
                        (before.get(p), inventory.get(p)) if m is not None)))
    return dict(version=1, event=event, base=base_sha, head=candidate, base_ref=base_ref,
                ref=ref, changed=changed, unknown=unknown, unsafe=unsafe, reason=reason,
                selected=selected(changed, unknown, unsafe, reason))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "head", "event", "base-ref", "ref"):
        parser.add_argument("--" + name, default="")
    args = parser.parse_args()
    try:
        result = select(Path.cwd(), base=args.base, head=args.head, event=args.event,
                        base_ref=args.base_ref, ref=args.ref)
        encoded = json.dumps(result, separators=(",", ":"))
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
                output.write("scope=" + encoded + "\n")
                for name, enabled in result["selected"].items():
                    output.write(name + "=" + str(enabled).lower() + "\n")
        print(encoded)
        return 0
    except (ValueError, OSError, UnicodeError, subprocess.CalledProcessError) as exc:
        print(f"Selection failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
