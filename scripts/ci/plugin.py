#!/usr/bin/env python3
"""Validate the Codex plugin package this repository publishes.

The installer copies a plugin root into its version cache verbatim, so what sits
in that directory is what ships. This check rebuilds the release payload from a
Git revision, reads every packaged fact from that same revision, and refuses the
shapes that would install silently wrong: a symlink the copier drops, an
untracked file it publishes, a personal path, a skill the manifest never declared.

It validates structure and hygiene. It installs nothing, and it is not evidence
that an installed plugin loaded on any host.
"""

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
PLUGIN_ROOT = ROOT / "plugins/crw"
MARKETPLACE = ".agents/plugins/marketplace.json"
MANIFEST = ".codex-plugin/plugin.json"
TOP_LEVEL = {".codex-plugin", "skills", "LICENSE"}
REQUIRED_FILES = (MANIFEST, "LICENSE")
LICENSE_ID = "MIT"
IDENTIFIER = r"(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
SEMVER = re.compile(r"^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
                    r"(?:-" + IDENTIFIER + r"(?:\." + IDENTIFIER + r")*)?"
                    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$")
TEXT_FIELDS = ("name", "version", "description", "license", "repository", "skills")
INTERFACE_FIELDS = ("displayName", "shortDescription", "longDescription", "developerName",
                    "category", "capabilities", "defaultPrompt")
# A personal home path names a real account; documentation placeholders such as
# <worktree-root> carry characters these patterns deliberately exclude.
HOME_PATHS = (re.compile(r"(?<![A-Za-z0-9._-])/home/[A-Za-z0-9._-]+/"),
              re.compile(r"(?<![A-Za-z0-9._-])/Users/[A-Za-z0-9._-]+/"),
              re.compile(r"(?<![A-Za-z0-9._-])/root/"),
              re.compile(r"[A-Za-z]:\\Users\\[A-Za-z0-9._-]+"))
FORBIDDEN_NAMES = re.compile(
    r"^(\.git|\.codexclaw|\.env(\..*)?|id_rsa.*|.*credentials.*|.*secrets?"
    r"|.*\.(sqlite3?|db|pem|key|p12|pfx))$", re.IGNORECASE)


class PackageError(Exception):
    """A package fact could not be read at all, so no verdict is possible."""


def git(*args, binary=False):
    result = subprocess.run(["git", *args], cwd=ROOT, capture_output=True)
    if result.returncode != 0:
        raise PackageError(result.stderr.decode(errors="replace").strip())
    return result.stdout if binary else result.stdout.decode()


def revision_payload(revision, relative):
    """Release bytes come from the revision tree, never from the working copy."""
    payload, errors, requests = {}, [], []
    for record in git("ls-tree", "-r", "-z", revision, "--", relative).split("\0"):
        if not record:
            continue
        meta, _, path = record.partition("\t")
        mode, kind, sha = meta.split(" ", 2)
        name = PurePosixPath(path).relative_to(relative).as_posix()
        if kind != "blob":
            errors.append("release " + path + ": the package may not contain a " + kind)
            continue
        if mode == "120000":
            errors.append("release " + path + ": the installer drops symlinks, so the package "
                          "may not contain one")
            continue
        requests.append((name, mode, sha))
    if requests:
        batch = subprocess.run(["git", "cat-file", "--batch"], cwd=ROOT, check=True,
                               input="\n".join(sha for _, _, sha in requests).encode(),
                               stdout=subprocess.PIPE).stdout
        offset = 0
        for name, mode, _ in requests:
            header_end = batch.index(b"\n", offset)
            size = int(batch[offset:header_end].split(b" ")[2])
            start = header_end + 1
            payload[name] = (mode, batch[start:start + size])
            offset = start + size + 1
    return payload, errors


def directory_payload(plugin_root):
    payload, errors = {}, []
    for path in sorted(plugin_root.rglob("*")):
        name = path.relative_to(plugin_root).as_posix()
        if path.is_symlink():
            errors.append("installed " + name + ": the installer drops symlinks, so the package "
                          "may not contain one")
            continue
        if path.is_dir():
            continue
        mode = "100755" if path.stat().st_mode & 0o111 else "100644"
        payload[name] = (mode, path.read_bytes())
    return payload, errors


def digest(payload):
    """Length-prefixed framing, so no two distinct payloads share a digest."""
    blocks = []
    for name, (mode, data) in sorted(payload.items()):
        encoded = name.encode()
        blocks.append(b"%d:%s %s %s" % (len(encoded), encoded, mode.encode(),
                                        hashlib.sha256(data).hexdigest().encode()))
    return hashlib.sha256(b"\n".join(blocks)).hexdigest()


def read_manifest(payload, label):
    if MANIFEST not in payload:
        raise PackageError(label + ": " + MANIFEST + " is missing from the package")
    try:
        manifest = json.loads(payload[MANIFEST][1].decode())
    except (UnicodeDecodeError, ValueError) as exc:
        raise PackageError(label + " " + MANIFEST + ": " + str(exc)) from exc
    if not isinstance(manifest, dict):
        raise PackageError(label + " " + MANIFEST + ": the manifest must be a JSON object")
    return manifest


def safe_manifest(payload, label):
    """Read a manifest without ending the run, so both trees can be reported."""
    try:
        return read_manifest(payload, label), []
    except PackageError as exc:
        return None, [str(exc)]


def declared_skills_path(manifest):
    """One rule for where skills live, shared by every consumer of the manifest."""
    declared = manifest.get("skills")
    if not isinstance(declared, str) or not declared.startswith("./"):
        raise ValueError("manifest must declare skills as a ./ relative path")
    relative = PurePosixPath(declared[2:].strip("/"))
    if not relative.name or relative.is_absolute() or ".." in relative.parts:
        raise ValueError("declared skills path must stay inside the plugin root")
    return relative.as_posix()


def manifest_errors(manifest, plugin_root_name, label):
    """plugin_root_name is None for an installed tree, whose directory is the version."""
    errors = []
    for field in TEXT_FIELDS:
        if not isinstance(manifest.get(field), str) or not manifest.get(field):
            errors.append(label + " manifest: " + field + " must be a nonempty string")
    if not isinstance(manifest.get("keywords"), list) or not manifest.get("keywords"):
        errors.append(label + " manifest: keywords must be a nonempty list")
    elif not all(isinstance(word, str) and word for word in manifest["keywords"]):
        errors.append(label + " manifest: keywords must be nonempty strings")
    if manifest.get("license") != LICENSE_ID:
        errors.append(label + " manifest: license " + repr(manifest.get("license"))
                      + " must be " + repr(LICENSE_ID) + ", the license this repository ships")
    author = manifest.get("author")
    if not isinstance(author, dict) or not isinstance(author.get("name"), str) or not author["name"]:
        errors.append(label + " manifest: author.name is required")
    if plugin_root_name is not None and manifest.get("name") != plugin_root_name:
        errors.append(label + " manifest: name " + repr(manifest.get("name"))
                      + " must match the plugin directory " + repr(plugin_root_name))
    if not SEMVER.match(str(manifest.get("version", ""))):
        errors.append(label + " manifest: version " + repr(manifest.get("version"))
                      + " is not a semantic version")
    try:
        declared_skills_path(manifest)
    except ValueError as exc:
        errors.append(label + " manifest: " + str(exc))
    interface = manifest.get("interface")
    if not isinstance(interface, dict):
        errors.append(label + " manifest: interface must be an object")
    else:
        for field in INTERFACE_FIELDS:
            value = interface.get(field)
            if field in ("capabilities", "defaultPrompt"):
                valid = (isinstance(value, list) and value
                         and all(isinstance(item, str) and item for item in value))
            else:
                valid = isinstance(value, str) and value
            if not valid:
                errors.append(label + " manifest: interface." + field
                              + " must be a nonempty string" + (" list" if field in
                              ("capabilities", "defaultPrompt") else ""))
    for field in ("hooks", "mcpServers", "apps"):
        if field in manifest:
            errors.append(label + " manifest: " + field + " is not declared by this package; "
                          "declaring a component replaces default discovery and belongs to its "
                          "own change")
    return errors


def marketplace_errors(catalog, manifest, plugin_relative):
    errors = []
    if catalog.get("name") != manifest.get("name"):
        errors.append("marketplace: name " + repr(catalog.get("name"))
                      + " must match the plugin name " + repr(manifest.get("name")))
    interface = catalog.get("interface")
    declared = manifest.get("interface")
    expected = declared.get("displayName") if isinstance(declared, dict) else None
    if not isinstance(interface, dict) or interface.get("displayName") != expected:
        errors.append("marketplace: interface.displayName must match the manifest display name "
                      + repr(expected))
    entries = [e for e in catalog.get("plugins", []) if isinstance(e, dict)
               and e.get("name") == manifest.get("name")]
    if len(entries) != 1:
        return errors + ["marketplace: expected exactly one entry named "
                         + repr(manifest.get("name"))]
    entry = entries[0]
    source = entry.get("source")
    if not isinstance(source, dict):
        errors.append("marketplace: entry source must be an object")
        source = {}
    if source.get("source") != "local":
        errors.append("marketplace: entry source.source must be local")
    path = source.get("path")
    # Compared exactly: a traversal or absolute spelling would install another directory.
    if path != "./" + plugin_relative:
        errors.append("marketplace: entry source.path " + repr(path)
                      + " must be " + repr("./" + plugin_relative))
    policy = entry.get("policy")
    if not isinstance(policy, dict):
        errors.append("marketplace: entry policy must be an object")
        policy = {}
    if policy.get("installation") != "AVAILABLE":
        errors.append("marketplace: entry policy.installation must be AVAILABLE")
    if policy.get("authentication") != "ON_USE":
        errors.append("marketplace: entry policy.authentication must be ON_USE; installation "
                      "does not create credentials")
    if not entry.get("category"):
        errors.append("marketplace: entry category is required")
    return errors


def hygiene(payload, label):
    errors = []
    for required in REQUIRED_FILES:
        if required not in payload:
            errors.append(label + ": " + required + " must ship with the package")
    for name, (_, data) in sorted(payload.items()):
        parts = PurePosixPath(name).parts
        if parts[0] not in TOP_LEVEL:
            errors.append(label + " " + name + ": only " + ", ".join(sorted(TOP_LEVEL))
                          + " may ship in the package")
        if any(FORBIDDEN_NAMES.match(part) for part in parts):
            errors.append(label + " " + name + ": operational state and credentials may not ship")
        try:
            text = data.decode()
        except UnicodeDecodeError:
            continue
        for pattern in HOME_PATHS:
            found = pattern.search(text)
            if found:
                errors.append(label + " " + name + ": contains the personal path "
                              + repr(found.group(0)) + "; the package must not require one "
                              "account checkout")
                break
    return errors


def skills(payload, manifest, label):
    """The declared directory is the only source for the shipped skill set."""
    try:
        prefix = declared_skills_path(manifest)
    except ValueError as exc:
        return [label + ": " + str(exc)], {}
    errors, found = [], {}
    for name in payload:
        parts = PurePosixPath(name).parts
        if len(parts) < 3 or parts[0] != prefix:
            continue
        found.setdefault(parts[1], set()).add(PurePosixPath(*parts[2:]).as_posix())
    if not found:
        errors.append(label + ": the declared skills path ships no skill")
    for skill, files in sorted(found.items()):
        for required in ("SKILL.md", "agents/openai.yaml"):
            if required not in files:
                errors.append(label + " " + prefix + "/" + skill + ": missing " + required)
    return errors, found


def compatibility_link_errors(revision, manifest, plugin_relative):
    """The repository root link is what keeps installations made before the move working."""
    listing = git("ls-tree", "-z", revision, "--", "skills")
    record = listing.split("\0")[0]
    if not record:
        return ["skills: the repository root must keep a link to the packaged skills"]
    meta, _, _ = record.partition("\t")
    mode, kind, sha = meta.split(" ", 2)
    if mode != "120000" or kind != "blob":
        return ["skills: the repository root entry must be a symlink to the packaged skills"]
    target = git("cat-file", "blob", sha, binary=True)
    expected = (plugin_relative + "/" + declared_skills_path(manifest)).encode()
    if target != expected:
        return ["skills: the root link points at " + repr(target.decode(errors="replace"))
                + " instead of " + repr(expected.decode())
                + "; both installation paths must read one source"]
    return []


def report_payload(payload, manifest, found, extra):
    result = {
        "digest": digest(payload),
        "files": len(payload),
        "version": manifest.get("version"),
        "skills": sorted(found),
        "expectedSkillNames": sorted(manifest.get("name", "") + ":" + skill for skill in found),
    }
    result.update(extra)
    return result


def check_installed(path):
    payload, errors = directory_payload(path)
    manifest = read_manifest(payload, "installed")
    # The installed directory is named by version, so only the packaged facts apply here.
    errors += manifest_errors(manifest, None, "installed")
    errors += hygiene(payload, "installed")
    skill_errors, found = skills(payload, manifest, "installed")
    return errors + skill_errors, report_payload(payload, manifest, found,
                                                 {"source": "payload", "path": str(path)})


def check_revision(revision):
    resolved = git("rev-parse", revision).strip()
    plugin_relative = PLUGIN_ROOT.relative_to(ROOT).as_posix()
    release, errors = revision_payload(resolved, plugin_relative)
    manifest = read_manifest(release, "release")
    errors += manifest_errors(manifest, PLUGIN_ROOT.name, "release")
    errors += hygiene(release, "release")
    skill_errors, found = skills(release, manifest, "release")
    errors += skill_errors
    try:
        catalog = json.loads(git("show", resolved + ":" + MARKETPLACE))
        if not isinstance(catalog, dict):
            raise ValueError("the marketplace file must be a JSON object")
        errors += marketplace_errors(catalog, manifest, plugin_relative)
    except (PackageError, ValueError) as exc:
        errors.append("marketplace: " + str(exc))
    license_blob = git("show", resolved + ":LICENSE", binary=True)
    if release.get("LICENSE", ("", b""))[1] != license_blob:
        errors.append("release LICENSE: the package copy must match the repository license")
    errors += compatibility_link_errors(resolved, manifest, plugin_relative)
    # A local marketplace installs the working tree, so it gets the same checks.
    working, working_errors = directory_payload(PLUGIN_ROOT)
    errors += working_errors + hygiene(working, "working tree")
    working_manifest, manifest_read_errors = safe_manifest(working, "working tree")
    errors += manifest_read_errors
    if working_manifest is not None:
        errors += manifest_errors(working_manifest, PLUGIN_ROOT.name, "working tree")
        working_skill_errors, working_found = skills(working, working_manifest, "working tree")
        errors += working_skill_errors
        # A local marketplace installs this tree, so a skill deleted or added here
        # would be published even though the committed revision is complete.
        for name in sorted(set(found) - set(working_found)):
            errors.append("working tree: " + name + " ships in the revision but is missing here")
        for name in sorted(set(working_found) - set(found)):
            errors.append("working tree: " + name + " is not part of the revision payload")
        try:
            catalog_now = json.loads((ROOT / MARKETPLACE).read_text(encoding="utf-8"))
            if not isinstance(catalog_now, dict):
                raise ValueError("the marketplace file must be a JSON object")
            errors += marketplace_errors(catalog_now, working_manifest, plugin_relative)
        except (OSError, ValueError) as exc:
            errors.append("working tree marketplace: " + str(exc))
        if working.get("LICENSE", ("", b""))[1] != (ROOT / "LICENSE").read_bytes():
            errors.append("working tree LICENSE: the package copy must match the repository license")
        link = ROOT / "skills"
        try:
            expected_link = plugin_relative + "/" + declared_skills_path(working_manifest)
            if not link.is_symlink() or str(link.readlink()) != expected_link:
                errors.append("working tree skills: the repository root link must be a symlink to "
                              + expected_link)
        except ValueError:
            pass
    for line in git("status", "--porcelain", "--ignored", "--", plugin_relative).splitlines():
        if line[:2] in ("??", "!!"):
            errors.append("working tree " + line[3:].strip() + ": untracked or ignored files "
                          "inside the plugin root are copied into the cache; commit or remove it")
    drift = sorted(name for name in set(release) | set(working)
                   if release.get(name) != working.get(name))
    return errors, report_payload(release, manifest, found, {
        "source": "revision", "revision": revision, "resolved": resolved,
        "worktreeDigest": digest(working), "worktreeDrift": drift,
    })


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="HEAD",
                        help="Revision whose tree is the release payload (default HEAD)")
    parser.add_argument("--payload", type=Path,
                        help="Validate an installed payload directory instead of this checkout")
    parser.add_argument("--json", action="store_true", help="Print the machine-readable result")
    args = parser.parse_args()
    try:
        if args.payload:
            errors, result = check_installed(args.payload.resolve())
        else:
            errors, result = check_revision(args.revision)
    except PackageError as exc:
        print(str(exc), file=sys.stderr)
        return 1
    if errors:
        print("\n".join(sorted(set(errors))), file=sys.stderr)
        return 1
    if args.json:
        print(json.dumps(result, indent=2, sort_keys=True))
        return 0
    print("Package " + str(result["version"]) + " at "
          + str(result.get("resolved", result.get("path"))) + ": " + str(result["files"])
          + " files, digest " + result["digest"][:16])
    print("Skill names under the plugin namespace: " + ", ".join(result["expectedSkillNames"]))
    if result.get("worktreeDrift"):
        print("Working tree differs from the revision payload: "
              + ", ".join(result["worktreeDrift"]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
