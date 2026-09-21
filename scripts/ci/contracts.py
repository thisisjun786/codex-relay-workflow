#!/usr/bin/env python3
"""Run known offline contract checks when their owning component is present."""

from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
CHECKS = (
    ("hook", "plugins/crw/skills/crw-run/scripts/hook_probe.py",
     "plugins/crw/skills/crw-run/references/hook-contract.md", ["replay"]),
    ("operations", "scripts/check_operations_contract.py",
     "plugins/crw/skills/crw-run/references/operations.md", []),
    # Re-derives the committed component identity from this checkout, so the one
    # compatibility definition cannot drift away from the source it describes.
    ("runtime", "scripts/runtime_install.py",
     "scripts/crw_runtime/components.json", ["verify-definition"]),
    # The two closed-vocabulary start-policy fields are checked against the table that
    # declares them, so the checker and the contract cannot drift apart unnoticed.
    ("start policy", "plugins/crw/skills/crw-run/scripts/start_policy.py",
     "plugins/crw/skills/crw-run/references/start-policy.md", ["selftest"]),
)


def main():
    for name, script, contract, args in CHECKS:
        present = [(ROOT / p).is_file() for p in (script, contract)]
        if any(present) and not all(present):
            print(f"Incomplete {name} contract/check pair", file=sys.stderr)
            return 1
        if not any(present):
            print(f"{name}: component absent; no coverage claimed", flush=True)
            continue
        subprocess.run([sys.executable, str(ROOT / script), *args], cwd=ROOT, check=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
