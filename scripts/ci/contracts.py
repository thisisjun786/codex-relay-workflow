#!/usr/bin/env python3
"""Run known offline contract checks when their owning component is present."""

from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
CHECKS = (
    ("hook", "skills/crw-run/scripts/hook_probe.py",
     "skills/crw-run/references/hook-contract.md", ["replay"]),
    ("operations", "scripts/check_operations_contract.py",
     "skills/crw-run/references/operations.md", []),
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
