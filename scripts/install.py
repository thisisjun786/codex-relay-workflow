#!/usr/bin/env python3
"""Link this repository's skills into Codex without replacing existing work."""

import argparse
import os
from pathlib import Path
import sys


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true", help="Inspect links without writing")
    mode.add_argument("--apply", action="store_true", help="Create missing links only")
    codex_dir = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    parser.add_argument("--dest", type=Path, default=codex_dir / "skills")
    args = parser.parse_args()
    source_root = Path(__file__).resolve().parent.parent / "skills"
    sources = sorted(p for p in source_root.iterdir() if (p / "SKILL.md").is_file())
    if not sources:
        parser.error(f"No skills found in {source_root}")
    destination = args.dest.expanduser().absolute()
    pending = []
    conflicts = []
    for source in sources:
        target = destination / source.name
        if target.is_symlink() and target.resolve() == source.resolve():
            print(f"LINKED {target} -> {source}")
        elif target.exists() or target.is_symlink():
            conflicts.append(target)
            print(f"CONFLICT {target}", file=sys.stderr)
        else:
            pending.append((source, target))
            print(f"MISSING {target}")
    if conflicts:
        print("Existing paths were left untouched. Resolve conflicts before installing.", file=sys.stderr)
        return 1
    if args.check:
        return int(bool(pending))
    destination.mkdir(parents=True, exist_ok=True)
    for source, target in pending:
        # symlink_to refuses a path created after the preflight as well.
        target.symlink_to(source, target_is_directory=True)
        print(f"CREATED {target} -> {source}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except OSError as exc:
        print(f"Installation failed: {exc}. Completed links are retained; rerun after resolving the error.", file=sys.stderr)
        raise SystemExit(1)
