#!/usr/bin/env python3
"""Fail closed unless every required CI job succeeded for a valid PR target."""

import json
import os
import sys

JOBS = {"validate", "tests", "secrets"}


def check(env):
    needs = json.loads(env["NEEDS_JSON"])
    if not isinstance(needs, dict) or set(needs) != JOBS:
        raise ValueError("Expected exactly validate, tests and secrets results")
    for name, job in needs.items():
        if not isinstance(job, dict) or job.get("result") != "success":
            raise ValueError(f"{name} did not succeed")
    for key in ("BASE_REF", "HEAD_REF", "BASE_REPO", "HEAD_REPO"):
        if not isinstance(env.get(key), str) or not env[key].strip():
            raise ValueError(f"Missing {key}")
    expected = env["EXPECTED_RELEASE"]
    if expected not in ("true", "false"):
        raise ValueError("EXPECTED_RELEASE must be true or false")
    release = env["BASE_REF"] == "main"
    if release != (expected == "true"):
        raise ValueError("Gate does not match PR target")
    if release and (env["HEAD_REF"] != "dev" or env["HEAD_REPO"] != env["BASE_REPO"]):
        raise ValueError("main accepts only same-repository dev promotions")


def main():
    try:
        check(os.environ)
    except (KeyError, TypeError, ValueError) as exc:
        print(f"Gate failed: {exc}", file=sys.stderr)
        return 1
    print("All required checks succeeded; release authorization remains a separate review.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
