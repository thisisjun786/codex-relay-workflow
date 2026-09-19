#!/usr/bin/env python3
"""Link this repository's skills into Codex without replacing existing work."""

import argparse
import json
import os
from pathlib import Path
import sys

LEGACY_NAMES = (
    "linear-focus", "linear-next", "linear-plan",
    "linear-run", "linear-check", "linear-logic", "crw-focus",
)
MANIFEST = Path(__file__).resolve().parent.parent / "plugins/crw/.codex-plugin/plugin.json"


def declared_skills(manifest_path):
    """Read the skills location the plugin manifest declares, or fail with a reason."""
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise ValueError(f"{manifest_path}: {exc}") from exc
    declared = manifest.get("skills") if isinstance(manifest, dict) else None
    if not isinstance(declared, str) or not declared.startswith("./"):
        raise ValueError(f"{manifest_path}: skills must be declared as a ./ relative path")
    relative = Path(declared[2:].strip("/"))
    if not relative.name or relative.is_absolute() or ".." in relative.parts:
        raise ValueError(f"{manifest_path}: the declared skills path must stay inside the plugin")
    root = (manifest_path.parent.parent / relative).resolve()
    plugin_root = manifest_path.parent.parent.resolve()
    if not root.is_relative_to(plugin_root):
        # A symlinked component would resolve outside the package the installer links from.
        raise ValueError(f"{root}: the declared skills path resolves outside {plugin_root}")
    if not root.is_dir():
        raise ValueError(f"{root}: the declared skills directory does not exist")
    return root


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true", help="Inspect links without writing")
    mode.add_argument("--apply", action="store_true", help="Create missing links only")
    codex_dir = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    parser.add_argument("--dest", type=Path, default=codex_dir / "skills")
    args = parser.parse_args()
    # The plugin manifest declares where the skills live, so the linked
    # installation and the packaged installation cannot drift apart.
    try:
        source_root = declared_skills(MANIFEST)
    except ValueError as exc:
        parser.error(str(exc))
    sources = sorted(p for p in source_root.iterdir() if (p / "SKILL.md").is_file())
    if not sources:
        parser.error(f"No skills found in {source_root}")
    destination = args.dest.expanduser().absolute()
    pending = []
    conflicts = []
    legacy = []
    for name in LEGACY_NAMES:
        target = destination / name
        if target.exists() or target.is_symlink():
            legacy.append(target)
            kind = "symlink" if target.is_symlink() else "directory" if target.is_dir() else "file"
            print(f"LEGACY {target} ({kind})", file=sys.stderr)
    if legacy:
        print("Legacy entries are preserved. See README: Retired skill migration; "
              "inspect ownership and move retired entries outside skill discovery. "
              "--apply installs new names; --check fails while legacy entries remain.", file=sys.stderr)
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
        return int(bool(pending or legacy))
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
