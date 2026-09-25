#!/usr/bin/env python3
"""Require successful selected checks and explicitly skipped unselected checks."""

import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from scope import validate_selection

JOBS = {"selection", "validate", "tests", "secrets", "packages", "go-product"}


def check(env):
    needs = json.loads(env["NEEDS_JSON"])
    if not isinstance(needs, dict) or set(needs) != JOBS:
        raise ValueError("Expected exactly " + ", ".join(sorted(JOBS)) + " results")
    selection = needs["selection"]
    if not isinstance(selection, dict) or selection.get("result") != "success":
        raise ValueError("selection did not succeed")
    scope = validate_selection(json.loads(selection["outputs"]["scope"]))
    for key, field in (("EVENT_NAME", "event"), ("BASE_REF", "base_ref"),
                       ("REF", "ref"), ("HEAD_SHA", "head")):
        if env[key] != scope[field]:
            raise ValueError(f"Selection does not match {key}")
    for name, job in needs.items():
        expected = "skipped" if scope["selected"].get(name) is False else "success"
        if not isinstance(job, dict) or job.get("result") != expected:
            raise ValueError(f"{name} must report {expected}")
    return scope


def main():
    try:
        check(os.environ)
    except (KeyError, TypeError, ValueError) as exc:
        print(f"Gate failed: {exc}", file=sys.stderr)
        return 1
    print("Every selected check succeeded; unselected checks were explicitly skipped.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
