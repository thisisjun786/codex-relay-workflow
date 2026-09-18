#!/usr/bin/env python3
"""Validate the Codex plugin package this repository publishes.

The installer copies a plugin root into its version cache verbatim, so what
sits in that directory is what ships. This check reads the marketplace entry
and the manifest, rebuilds the release payload from a Git revision rather than
from the working tree, and refuses the shapes that would install silently
wrong: a dropped symlink, an untracked file, a personal path, a skill the
manifest does not declare.

It validates structure and hygiene. It does not install anything, and it is not
evidence that an installed plugin loaded on any host.
"""

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
MARKETPLACE = ROOT / ".agents/plugins/marketplace.json"
PLUGIN_ROOT = ROOT / "plugins/crw"
MANIFEST = "  .codex-plugin/plugin.json"
TOP_LEVEL = {".codex-plugin", "skills", "LICENSE"}
SEMVER = re.compile(r"^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)*$")
MANIFEST_FIELDS = ("name", "version", "description", "author", "license", "skills", "interface")
INTERFACE_FIELDS = ("displayName", "shortDescription", "longDescription", "developerName",
                    "category", "capabilities", "defaultPrompt")
# A personal home path names a real account; documentation placeholders such as
# <worktree-root> carry characters these patterns deliberately exclude.
HOME_PATHS = (re.compile(r"(?<![A-Za-z0-9._-])/home/[A-Za-z0-9._-]+/"),
              re.compile(r"(?<![A-Za-z0-9._-])/Users/[A-Za-z0-9._-]+/"),
              re.compile(r"(?<![A-Za-z0-9._-])/root/"),
              re.compile(r"[A-Za-z]:\\Users\\[A-Za-z0-9._-]+"))
FORBIDDEN_NAMES = re.compile(r"^(\.git|\.codexclaw|\.env|id_rsa.*|.*\.(sqlite3?|pem|key))$")


def run(*args, cwd=ROOT):
    return subprocess.run(args, cwd=cwd, capture_output=True, check=True).stdout


def revision_payload(revision, plugin_root):
    """Release bytes come from the revision's tree, never from the working copy."""
    relative = plugin_root.relative_to(ROOT).as_posix()
    listing = run("git", "ls-tree", "-r", "-z", revision, "--", relative).decode()
    payload, errors = {}, []
    requests = []
    for record in listing.split("\0"):
        if not record:
            continue
        meta, _, path = record.partition("\t")
        mode, kind, sha = meta.split(" ", 2)
        name = PurePosixPath(path).relative_to(relative).as_posix()
        if kind != "blob":
            errors.append(f"{path}: the package may not contain a {kind}")
            continue
        if mode == "120000":
            errors.append(f"{path}: the installer drops symlinks, so the package may not contain one")
            continue
        requests.append((name, mode, sha))
    if requests:
        batch = subprocess.run(["git", "cat-file", "--batch"], cwd=ROOT, check=True,
                               input="\n".join(sha for _, _, sha in requests).encode(),
                               stdout=subprocess.PIPE).stdout
        offset = 0
        for name, mode, sha in requests:
            header_end = batch.index(b"\n", offset)
            size = int(batch[offset:header_end].split(b" ")[2])
            start = header_end + 1
            payload[name] = (mode, batch[start:start + size])
            offset = start + size + 1
    return payload, errors


def worktree_payload(plugin_root):
    payload, errors = {}, []
    for path in sorted(plugin_root.rglob("*")):
        if path.is_symlink():
            errors.append(f"{path.relative_to(plugin_root)}: the installer drops symlinks, "
                          "so the package may not contain one")
            continue
        if path.is_dir():
            continue
        mode = "100755" if path.stat().st_mode & 0o111 else "100644"
        payload[path.relative_to(plugin_root).as_posix()] = (mode, path.read_bytes())
    return payload, errors


def digest(payload):
    lines = [f"{mode} {hashlib.sha256(data).hexdigest()} {name}"
             for name, (mode, data) in sorted(payload.items())]
    return hashlib.sha256("\n".join(lines).encode()).hexdigest()


def hygiene(payload, label):
    errors = []
    for name, (_, data) in sorted(payload.items()):
        parts = PurePosixPath(name).parts
        if parts[0] not in TOP_LEVEL:
            errors.append(f"{label} {name}: only {sorted(TOP_LEVEL)} may ship in the package")
        if any(FORBIDDEN_NAMES.match(part) for part in parts):
            errors.append(f"{label} {name}: operational state and credentials may not ship")
        try:
            text = data.decode()
        except UnicodeDecodeError:
            continue
        for pattern in HOME_PATHS:
            found = pattern.search(text)
            if found:
                errors.append(f"{label} {name}: contains the personal path {found.group(0)!r}; "
                              "the package must not require one account's checkout")
                break
    return errors


def skills(payload, manifest, label):
    """The declared directory is the only source for the shipped skill set."""
    declared = manifest.get("skills")
    errors = []
    if not isinstance(declared, str) or not declared.startswith("./"):
        return [f"{label}: manifest must declare skills as a relative path inside the plugin root"], {}
    prefix = PurePosixPath(declared.strip("./")).as_posix()
    if not prefix or ".." in PurePosixPath(prefix).parts:
        return [f"{label}: declared skills path must stay inside the plugin root"], {}
    found = {}
    for name in payload:
        parts = PurePosixPath(name).parts
        if len(parts) < 3 or PurePosixPath(*parts[:1]).as_posix() != prefix:
            continue
        found.setdefault(parts[1], set()).add(PurePosixPath(*parts[2:]).as_posix())
    if not found:
        errors.append(f"{label}: the declared skills path {declared} ships no skill")
    for skill, files in sorted(found.items()):
        for required in ("SKILL.md", "agents/openai.yaml"):
            if required not in files:
                errors.append(f"{label} {prefix}/{skill}: missing {required}")
    return errors, found


def manifest_errors(manifest):
    errors = []
    for field in MANIFEST_FIELDS:
        if not manifest.get(field):
            errors.append(f"manifest: missing {field}")
    if not SEMVER.match(str(manifest.get("version", ""))):
        errors.append(f"manifest: version {manifest.get('version')!r} is not semantic")
    if not isinstance(manifest.get("author"), dict) or not manifest["author"].get("name"):
        errors.append("manifest: author.name is required")
    interface = manifest.get("interface")
    if not isinstance(interface, dict):
        errors.append("manifest: interface must be an object")
    else:
        for field in INTERFACE_FIELDS:
            if not interface.get(field):
                errors.append(f"manifest: interface.{field} is required")
    for field in ("hooks", "mcpServers", "apps"):
        if field in manifest:
            errors.append(f"manifest: {field} is not declared by this package; "
                          "declaring it replaces default discovery and belongs to its own change")
    return errors


def marketplace_errors(entry_source, manifest, plugin_root):
    errors = []
    try:
        catalog = json.loads(MARKETPLACE.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        return [f"{MARKETPLACE.name}: {exc}"]
    if not catalog.get("name"):
        errors.append("marketplace: name is required")
    if not isinstance(catalog.get("interface"), dict) or not catalog["interface"].get("displayName"):
        errors.append("marketplace: interface.displayName is required")
    entries = [e for e in catalog.get("plugins", []) if e.get("name") == manifest.get("name")]
    if len(entries) != 1:
        return errors + [f"marketplace: expected exactly one entry named {manifest.get('name')!r}"]
    entry = entries[0]
    source = entry.get("source") or {}
    if source.get("source") != "local":
        errors.append("marketplace: entry source.source must be local")
    path = source.get("path", "")
    if (MARKETPLACE.parents[2] / path).resolve() != plugin_root.resolve():
        errors.append(f"marketplace: entry source.path {path!r} does not point at the plugin root")
    policy = entry.get("policy") or {}
    if policy.get("installation") != "AVAILABLE":
        errors.append("marketplace: entry policy.installation must be AVAILABLE")
    if policy.get("authentication") != "ON_USE":
        errors.append("marketplace: entry policy.authentication must be ON_USE; "
                      "installation does not create credentials")
    if not entry.get("category"):
        errors.append("marketplace: entry category is required")
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="HEAD",
                        help="Revision whose tree is the release payload (default HEAD)")
    parser.add_argument("--payload", type=Path,
                        help="Validate an installed payload directory instead of this checkout")
    parser.add_argument("--json", action="store_true", help="Print the machine-readable result")
    args = parser.parse_args()

    if args.payload:
        plugin_root = args.payload.resolve()
        manifest_path = plugin_root / ".codex-plugin/plugin.json"
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            print(f"{manifest_path}: {exc}", file=sys.stderr)
            return 1
        payload, errors = worktree_payload(plugin_root)
        errors += manifest_errors(manifest) + hygiene(payload, "installed")
        skill_errors, found = skills(payload, manifest, "installed")
        errors += skill_errors
        result = {"source": "payload", "path": str(plugin_root), "digest": digest(payload),
                  "version": manifest.get("version"),
                  "skills": sorted(found), "files": len(payload),
                  "expectedSkillNames": sorted(f"{manifest.get('name')}:{s}" for s in found)}
    else:
        plugin_root = PLUGIN_ROOT
        manifest_path = plugin_root / ".codex-plugin/plugin.json"
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            print(f"{manifest_path}: {exc}", file=sys.stderr)
            return 1
        resolved = run("git", "rev-parse", args.revision).decode().strip()
        release, errors = revision_payload(resolved, plugin_root)
        working, working_errors = worktree_payload(plugin_root)
        errors += working_errors
        errors += manifest_errors(manifest)
        errors += marketplace_errors(MARKETPLACE, manifest, plugin_root)
        errors += hygiene(release, "release") + hygiene(working, "working tree")
        skill_errors, found = skills(release, manifest, "release")
        errors += skill_errors
        errors += skills(working, manifest, "working tree")[0]
        # Untracked or ignored files are copied by a local install, so they are a package defect.
        stray = run("git", "status", "--porcelain", "--ignored", "--",
                    plugin_root.relative_to(ROOT).as_posix()).decode().splitlines()
        for line in stray:
            state, _, name = line.partition(" ")
            if line[:2] in ("??", "!!"):
                errors.append(f"working tree {name.strip()}: untracked or ignored files inside the "
                              "plugin root are copied into the cache; commit or remove it")
        link = ROOT / "skills"
        declared = (plugin_root / manifest.get("skills", "./skills/")).resolve()
        if not link.is_symlink() or link.resolve() != declared:
            errors.append("skills: the repository root link must point at the declared skills "
                          "directory so both installation paths read one source")
        license_copy = plugin_root / "LICENSE"
        if not license_copy.is_file() or license_copy.read_bytes() != (ROOT / "LICENSE").read_bytes():
            errors.append("LICENSE: the package copy must match the repository license")
        drift = sorted(name for name in set(release) | set(working)
                       if release.get(name) != working.get(name))
        result = {"source": "revision", "revision": args.revision, "resolved": resolved,
                  "digest": digest(release), "worktreeDigest": digest(working),
                  "version": manifest.get("version"), "skills": sorted(found),
                  "files": len(release), "worktreeDrift": drift,
                  "expectedSkillNames": sorted(f"{manifest.get('name')}:{s}" for s in found)}

    if errors:
        print("\n".join(sorted(set(errors))), file=sys.stderr)
        return 1
    if args.json:
        print(json.dumps(result, indent=2, sort_keys=True))
        return 0
    print(f"Package {result['version']} at {result.get('resolved', result.get('path'))}: "
          f"{result['files']} files, digest {result['digest'][:16]}")
    print("Skill names under the plugin namespace: " + ", ".join(result["expectedSkillNames"]))
    if result.get("worktreeDrift"):
        print("Working tree differs from the revision payload: "
              + ", ".join(result["worktreeDrift"]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
