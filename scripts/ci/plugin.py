#!/usr/bin/env python3
"""Validate the Codex plugin package this repository publishes.

The installer copies a plugin root into its version cache verbatim, so what sits
in that directory is what ships. This check rebuilds the release payload from a
Git revision, reads every packaged fact from that same revision, and refuses the
shapes that would install silently wrong: a symlink the copier drops, an
untracked file it publishes, a personal path, a skill the manifest never declared,
a version that two different payloads could both claim.

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
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[2]
PLUGIN_ROOT = ROOT / "plugins/crw"
MARKETPLACE = ".agents/plugins/marketplace.json"
MANIFEST = ".codex-plugin/plugin.json"
# What may sit at the plugin root regardless of what the manifest declares. Everything else
# has to be named by a declaration, and the set is derived from the manifest rather than kept
# here as a list, because a component nobody declared does not load (measured) while the
# installer still copies it.
ALWAYS = {".codex-plugin", "LICENSE"}
REQUIRED_FILES = (MANIFEST, "LICENSE")
LICENSE_ID = "MIT"
IDENTIFIER = r"(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
SEMVER = re.compile(r"^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
                    r"(?:-" + IDENTIFIER + r"(?:\." + IDENTIFIER + r")*)?"
                    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$")
TEXT_FIELDS = ("name", "version", "description", "license", "repository", "skills")
INTERFACE_FIELDS = ("displayName", "shortDescription", "longDescription", "developerName",
                    "category", "capabilities", "defaultPrompt")
# The bundled ingestion validator rejects keys it does not know, so a manifest with a
# stray key can pass every other rule here and still be refused when it is published.
MANIFEST_KEYS = {"name", "version", "description", "author", "homepage", "repository",
                 "license", "keywords", "skills", "hooks", "mcpServers", "apps", "interface"}
INTERFACE_KEYS = set(INTERFACE_FIELDS) | {"websiteURL", "privacyPolicyURL", "termsOfServiceURL",
                                          "brandColor", "composerIcon", "logo", "logoDark",
                                          "screenshots"}
AUTHOR_KEYS = {"name", "email", "url"}
# Measured, not chosen. The host names this set in its own rejection text, quoted from an isolated
# home: "unknown variant `...`, expected one of `auto`, `prompt`, `writes`, `approve`".
APPROVAL_MODES = ("auto", "prompt", "writes", "approve")
# The only key a declared tool gate may hold here. Fail closed, because the host's answer to a
# plugin declaration it dislikes was measured and it is silence: plugin add exits 0, mcp list exits
# 0, and the server is simply absent with no disabled_reason. Nothing downstream would report it.
APPROVAL_KEYS = {"approval_mode"}
# The gates this package must keep declaring. A per-tool approval the host grants today survives
# the transition only because the declaration reproduces it, so dropping one here is the whole
# defect CRW-142 exists to close, arriving as an ordinary edit that nothing else would catch.
REQUIRED_TOOL_APPROVALS = {
    "codex-thread-bridge": {"create_thread": "approve", "send_message_to_thread": "approve"},
}
# A personal home path names a real account; documentation placeholders such as
# <worktree-root> carry characters these patterns deliberately exclude.
HOME_PATHS = (re.compile(r"(?<![A-Za-z0-9._-])/home/[A-Za-z0-9._-]+/"),
              re.compile(r"(?<![A-Za-z0-9._-])/Users/[A-Za-z0-9._-]+/"),
              re.compile(r"(?<![A-Za-z0-9._-])/root/"),
              re.compile(r"[A-Za-z]:\\Users\\[A-Za-z0-9._-]+"))
FORBIDDEN_NAMES = re.compile(
    r"^(\.git|\.codexclaw|\.env(\..*)?|id_(rsa|dsa|ecdsa|ed25519)(\..*)?"
    r"|\.netrc|\.pgpass|\.htpasswd|\.npmrc|authorized_keys"
    r"|.*credentials?(?:[._-].*)?|.*secrets?(?:[._-].*)?"
    r"|.*\.(sqlite3?|db|pem|key|p12|pfx))$", re.IGNORECASE)
HTTPS_FIELDS = ("websiteURL", "privacyPolicyURL", "termsOfServiceURL")
ASSET_FIELDS = ("composerIcon", "logo", "logoDark")
BRAND_COLOR = re.compile(r"^#[0-9A-Fa-f]{6}$")
# The largest timeout a hook may be registered with. Above this the host clamps at discovery
# and the clamped value is not measured, so a larger number is not the deadline it looks like.
HOOK_TIMEOUT_SECONDS = 10
# Hex characters of the payload digest the version carries as build metadata. Long enough that
# two payloads of this package will not collide by accident, short enough to read in a plugin
# listing and in the cache path, which is the version.
PAYLOAD_SUFFIX_LENGTH = 12


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
            # An empty directory holds no file to check, and the installer still
            # copies it, so it would ship unseen by every payload rule.
            if not any(path.iterdir()):
                errors.append(name + ": an empty directory still ships; remove it")
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


def split_version(version):
    """A version's release part and the payload suffix it carries; either may be empty."""
    release, _, suffix = str(version).partition("+")
    return release, suffix


def version_payload(payload, version):
    """The payload read with the recorded payload suffix elided from the manifest.

    The suffix is a digest of the package and the manifest ships inside that package, so
    digesting the bytes as they stand has no fixed point: recording the answer changes the
    answer. Eliding the version this manifest declares, and nothing else, removes that
    self-reference. Every other byte of every shipped file still reaches the digest, the
    rest of the manifest included, so that suffix is the only difference between two
    release trees this digest is unable to see.
    """
    release, suffix = split_version(version)
    if not suffix or MANIFEST not in payload:
        return payload
    mode, data = payload[MANIFEST]
    recorded, plain = json.dumps(str(version)).encode(), json.dumps(release).encode()
    if data.count(recorded) != 1:
        raise ValueError(MANIFEST + " spells " + repr(str(version)) + " "
                         + str(data.count(recorded)) + " times; the suffix has to be elided"
                         " exactly once for the digest beneath it to be derived at all")
    return dict(payload, **{MANIFEST: (mode, data.replace(recorded, plain))})


def payload_version(payload, version):
    """The version a payload has to declare: its release version, then its own digest."""
    release, _ = split_version(version)
    return release + "+" + digest(version_payload(payload, version))[:PAYLOAD_SUFFIX_LENGTH]


def version_errors(manifest, payload, label):
    """An operator reads a version, never a payload, so the version has to name the payload.

    Two revisions that ship different bytes under one version are indistinguishable in
    `codex plugin list`, and the second installs over the first, because the cache directory
    is the version. Which release version to publish stays the release owner's decision; the
    suffix under it is derived from the bytes that ship and is never typed.
    """
    version = str(manifest.get("version", ""))
    try:
        expected = payload_version(payload, version)
    except ValueError as exc:
        return [label + " manifest: " + str(exc)]
    suffixes = [part for part in (split_version(expected)[1], split_version(version)[1]) if part]
    repeated = [name for name in sorted(payload) if name != MANIFEST
                and any(part.encode() in payload[name][1] for part in suffixes)]
    if repeated:
        return [label + " " + name + ": a shipped file repeats the payload suffix, so recording"
                " the digest would change the digest it records; the manifest version is the"
                " one place that suffix belongs" for name in repeated]
    if version == expected:
        return []
    return [label + " manifest: version " + repr(version) + " does not name this payload."
            " Record " + repr(expected) + "; `--record-version` writes it into the working"
            " tree manifest. The suffix is this payload's own digest, because two packages"
            " that ship different bytes may not offer one version"]


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


def inside(declared):
    """A ./ relative path that stays in the package, or None when it is neither."""
    if not isinstance(declared, str) or not declared.startswith("./"):
        return None
    relative = PurePosixPath(declared[2:].strip("/"))
    if not relative.name or relative.is_absolute() or ".." in relative.parts:
        return None
    return relative.as_posix()


def declared_hooks(manifest):
    """The hook documents this manifest declares, as a list however it spelled them.

    A list and a single string both load, measured on codex-cli 0.154.0. A hooks value that is
    neither is not a third spelling: an inline document was measured not to load at all, so it
    is a declaration that ships a file nothing reads.
    """
    declared = manifest.get("hooks")
    if declared is None:
        return []
    if isinstance(declared, str):
        return [declared]
    if isinstance(declared, list):
        return declared
    raise ValueError("hooks must be a ./ relative path or a list of them; an inline document"
                     " does not load")


def declared_components(manifest):
    """Every path the manifest declares, and the top-level names they occupy.

    Returned together because both answers come from one reading. The package may hold exactly
    these roots: anything else installs without loading, and anything declared but missing is a
    component the manifest promises and the package does not ship.
    """
    paths, roots, errors = [], set(ALWAYS), []
    try:
        skills_path = declared_skills_path(manifest)
        roots.add(PurePosixPath(skills_path).parts[0])
    except ValueError as exc:
        errors.append(str(exc))
    try:
        hooks = declared_hooks(manifest)
    except ValueError as exc:
        errors.append(str(exc))
        hooks = []
    declared_mcp = manifest.get("mcpServers")
    if isinstance(declared_mcp, dict):
        errors.append("mcpServers must name a ./ relative file: an inline table would ship a"
                      " server this check never reads")
        declared_mcp = None
    for field, value in [("hooks", path) for path in hooks] \
            + ([("mcpServers", declared_mcp)] if declared_mcp is not None else []):
        relative = inside(value)
        if relative is None:
            errors.append(field + " " + repr(value) + " must be a ./ relative path inside the"
                          " plugin root")
            continue
        paths.append((field, relative))
        roots.add(PurePosixPath(relative).parts[0])
    if "apps" in manifest:
        errors.append("apps is not declared by this package")
    return paths, roots, errors


def hook_document_errors(name, data, label):
    """A declared hook file has to be the document a host reads, not merely valid JSON."""
    errors = []
    try:
        document = json.loads(data.decode())
    except (UnicodeDecodeError, ValueError) as exc:
        return [label + " " + name + ": " + str(exc)]
    events = document.get("hooks") if isinstance(document, dict) else None
    if not isinstance(events, dict) or not events:
        return [label + " " + name + ": a hook file must hold a nonempty hooks object"]
    for event, groups in sorted(events.items()):
        if not isinstance(groups, list) or not groups:
            errors.append(label + " " + name + ": " + event + " must hold a nonempty list")
            continue
        for group in groups:
            entries = group.get("hooks") if isinstance(group, dict) else None
            if not isinstance(entries, list) or not entries:
                errors.append(label + " " + name + ": every " + event
                              + " group must hold a nonempty hooks list")
                continue
            for entry in entries:
                if not isinstance(entry, dict) or entry.get("type") != "command":
                    errors.append(label + " " + name + ": every hook must be a command hook")
                    continue
                if not nonempty(entry.get("command")):
                    errors.append(label + " " + name + ": every hook needs a command")
                timeout = entry.get("timeout")
                if not isinstance(timeout, int) or isinstance(timeout, bool) or timeout <= 0:
                    errors.append(label + " " + name + ": every hook needs a positive integer"
                                  " timeout")
                elif timeout > HOOK_TIMEOUT_SECONDS:
                    # The host clamps an over-long timeout at discovery and the clamped value is
                    # not measured, so a larger number is not the deadline it appears to be.
                    errors.append(label + " " + name + ": a hook timeout may not exceed "
                                  + str(HOOK_TIMEOUT_SECONDS) + " seconds")
    return errors


def mcp_document_errors(name, data, payload, label):
    """A declared MCP file, checked against how a plugin server was measured to start.

    Three of these rules are measurements rather than taste. A server inherits no plugin-root
    variable, so a variable in a command or an argument arrives as literal text and the server
    never starts. A relative command resolves only when cwd is set. And the package may not
    name an absolute path, because it ships to hosts it has never seen.
    """
    errors = []
    try:
        document = json.loads(data.decode())
    except (UnicodeDecodeError, ValueError) as exc:
        return [label + " " + name + ": " + str(exc)]
    servers = document.get("mcpServers") if isinstance(document, dict) else None
    if not isinstance(servers, dict) or not servers:
        return [label + " " + name + ": an MCP file must hold a nonempty mcpServers object"]
    for server, declared in sorted(servers.items()):
        where = label + " " + name + " " + server + ": "
        if not isinstance(declared, dict):
            errors.append(where + "a server must be an object")
            continue
        command = declared.get("command")
        if not nonempty(command):
            errors.append(where + "a server needs a command")
            command = None
        if declared.get("cwd") != ".":
            errors.append(where + "cwd must be \".\": a relative command resolves against it,"
                          " and a server declared without one never starts")
        arguments = declared.get("args")
        if not isinstance(arguments, list) or not all(isinstance(w, str) for w in arguments):
            errors.append(where + "args must be a list of strings")
            arguments = []
        # Only values the checks above accepted as strings. A truthy non-string command such as
        # 1 or true would otherwise reach the membership tests below and end the whole package
        # check in a TypeError, hiding this finding and every other one in the run.
        for word in ([command] if command else []) + list(arguments):
            if "$" in word or "%" in word:
                errors.append(where + repr(word) + " carries a variable; a plugin MCP server"
                              " runs without a shell and inherits no plugin root, so it would"
                              " arrive as literal text")
            elif word.startswith("/"):
                errors.append(where + repr(word) + " is an absolute path; the package ships to"
                              " hosts it has not seen")
            elif word.startswith("./") and word[2:] not in payload:
                errors.append(where + repr(word) + " names a file the package does not ship")
        errors += tool_approval_errors(server, declared, where)
    return errors


def tool_approval_errors(server, declared, where):
    """The per-tool approval policy a declaration carries, checked before it can ship.

    This is the one check standing between a typo and a host with no task bridge. Measured: a bad
    approval_mode in a PLUGIN declaration makes plugin add exit 0 and mcp list exit 0 with zero
    entries -- the server vanishes and no disabled_reason is recorded, because there is no entry
    left to carry one. The same mistake in a user config is a loud error naming the valid set. So
    the host will not tell anyone; this has to.
    """
    errors = []
    required = REQUIRED_TOOL_APPROVALS.get(server) or {}
    if "tools" not in declared:
        if required:
            errors.append(where + "declares no tools, and this server must gate "
                          + ", ".join(tool + " with " + repr(mode)
                                       for tool, mode in sorted(required.items()))
                          + ". The user configuration this package replaces carries that gate, and"
                          " the declaration is what serves the server once the table is removed")
        return errors
    tools = declared["tools"]
    if not isinstance(tools, dict) or not tools:
        return [where + "tools must be a nonempty object; what a host does with an empty one is"
                       " not measured"]
    for tool, gate in sorted(tools.items()):
        named = where + "tools." + str(tool) + ": "
        if not nonempty(tool):
            errors.append(where + "a tool name must be a nonempty string")
            continue
        if not isinstance(gate, dict):
            errors.append(named + "a tool gate must be an object")
            continue
        unknown = sorted(set(gate) - APPROVAL_KEYS)
        if unknown:
            errors.append(named + "carries " + ", ".join(repr(key) for key in unknown)
                          + "; only " + ", ".join(sorted(APPROVAL_KEYS)) + " is checked here, and"
                          " a declaration this host dislikes disappears without a word")
            continue
        mode = gate.get("approval_mode")
        if not isinstance(mode, str) or mode not in APPROVAL_MODES:
            errors.append(named + "approval_mode " + repr(mode) + " is not one of "
                          + ", ".join(APPROVAL_MODES))
    for tool, mode in sorted(required.items()):
        gate = tools.get(tool) if isinstance(tools, dict) else None
        carried = gate.get("approval_mode") if isinstance(gate, dict) else None
        if carried != mode:
            errors.append(where + "must gate " + tool + " with " + repr(mode) + ", and it"
                          " declares " + repr(carried))
    return errors


def nonempty(value):
    """Ingestion treats a whitespace-only value as absent, so this check does too."""
    return isinstance(value, str) and bool(value.strip())


def yaml_scalar(text):
    """One quoted or bare scalar, the way this repository's metadata check reads one."""
    text = text.strip()
    if text.startswith("\""):
        return json.loads(text)
    if text.startswith("'") and text.endswith("'") and len(text) > 1:
        return text[1:-1].replace("''", "'")
    return text


def interface_errors(skill_path, payload, label):
    """A skill's interface metadata, read from the bytes the release actually ships.

    The repository's metadata check reads the working tree; this reads the payload, which is
    what a clone installs. A file broken only in the committed revision would otherwise ship
    and be found by nobody.

    What this does NOT claim: that a plugin installation reads this file. Measured on
    codex-cli 0.154.0, the model-visible skill listing takes its description from the SKILL.md
    frontmatter, and a description placed only here never appeared. So this is checked against
    the shape this repository requires of it, not against an ingestion contract nobody has
    observed.
    """
    name = PurePosixPath(skill_path).name
    try:
        text = payload[skill_path + "/agents/openai.yaml"][1].decode()
    except (KeyError, UnicodeDecodeError) as exc:
        return [label + " " + skill_path + "/agents/openai.yaml: " + str(exc)]
    interface, section = {}, None
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if not line.startswith(" "):
            section = line.strip()
        elif section == "interface:":
            key, separator, value = line.strip().partition(":")
            if not separator or key in interface:
                return [label + " " + skill_path + "/agents/openai.yaml: malformed interface"
                        " metadata"]
            try:
                interface[key] = yaml_scalar(value)
            except ValueError as exc:
                # Reported with the other findings rather than raised. A quoting mistake here
                # would otherwise abort the whole package check, and the run would end with one
                # traceback instead of the list of everything that is wrong.
                return [label + " " + skill_path + "/agents/openai.yaml: " + key + " is not a"
                        " readable scalar (" + str(exc) + ")"]
    missing = {"display_name", "short_description", "default_prompt"} - set(interface)
    if missing:
        return [label + " " + skill_path + "/agents/openai.yaml: missing interface "
                + ", ".join(sorted(missing))]
    if "$" + name not in interface["default_prompt"]:
        return [label + " " + skill_path + "/agents/openai.yaml: default_prompt must name $"
                + name]
    return []


def https_url(value):
    """An ingestible URL carries the https scheme and a host, not just the prefix."""
    if not isinstance(value, str):
        return False
    try:
        parsed = urlsplit(value)
        host = parsed.hostname
    except ValueError:
        # A malformed authority is a rejected value, not a crash.
        return False
    return parsed.scheme == "https" and bool(host)


def interface_option_errors(interface, payload, label):
    """Optional presentation fields follow the ingestion rules or they are refused."""
    errors = []
    for field in HTTPS_FIELDS:
        value = interface.get(field)
        if value is not None and not https_url(value):
            errors.append(label + " manifest: interface." + field + " must be an https URL")
    color = interface.get("brandColor")
    if color is not None and (not isinstance(color, str) or not BRAND_COLOR.match(color)):
        errors.append(label + " manifest: interface.brandColor must be #RRGGBB")
    assets = [(field, interface.get(field)) for field in ASSET_FIELDS if field in interface]
    screenshots = interface.get("screenshots")
    if screenshots is not None:
        if not isinstance(screenshots, list) or not screenshots:
            errors.append(label + " manifest: interface.screenshots must be a nonempty list")
        else:
            assets += [("screenshots", shot) for shot in screenshots]
    for field, value in assets:
        if not isinstance(value, str) or not value.startswith("./"):
            errors.append(label + " manifest: interface." + field + " must be a ./ relative path")
        elif payload is not None and value[2:] not in payload:
            errors.append(label + " manifest: interface." + field + " names " + repr(value)
                          + ", which the package does not ship")
    return errors


def manifest_errors(manifest, plugin_root_name, label, payload=None):
    """plugin_root_name is None for an installed tree, whose directory is the version."""
    errors = []
    for field in TEXT_FIELDS:
        if not nonempty(manifest.get(field)):
            errors.append(label + " manifest: " + field + " must be a nonempty string")
    if not isinstance(manifest.get("keywords"), list) or not manifest.get("keywords"):
        errors.append(label + " manifest: keywords must be a nonempty list")
    elif not all(nonempty(word) for word in manifest["keywords"]):
        errors.append(label + " manifest: keywords must be nonempty strings")
    if manifest.get("license") != LICENSE_ID:
        errors.append(label + " manifest: license " + repr(manifest.get("license"))
                      + " must be " + repr(LICENSE_ID) + ", the license this repository ships")
    author = manifest.get("author")
    if not isinstance(author, dict) or not nonempty(author.get("name")):
        errors.append(label + " manifest: author.name is required")
    elif set(author) - AUTHOR_KEYS:
        errors.append(label + " manifest: author carries unsupported keys "
                      + repr(sorted(set(author) - AUTHOR_KEYS)))
    if isinstance(author, dict):
        if "email" in author and not nonempty(author["email"]):
            errors.append(label + " manifest: author.email must be a nonempty string")
        if "url" in author and not https_url(author["url"]):
            errors.append(label + " manifest: author.url must be an https URL with a host")
    for unsupported in sorted(set(manifest) - MANIFEST_KEYS):
        errors.append(label + " manifest: " + repr(unsupported)
                      + " is not a supported manifest key")
    if plugin_root_name is not None and manifest.get("name") != plugin_root_name:
        errors.append(label + " manifest: name " + repr(manifest.get("name"))
                      + " must match the plugin directory " + repr(plugin_root_name))
    if not SEMVER.fullmatch(str(manifest.get("version", ""))):
        errors.append(label + " manifest: version " + repr(manifest.get("version"))
                      + " is not a semantic version")
    elif payload is not None:
        errors += version_errors(manifest, payload, label)
    try:
        declared_skills_path(manifest)
    except ValueError as exc:
        errors.append(label + " manifest: " + str(exc))
    interface = manifest.get("interface")
    if not isinstance(interface, dict):
        errors.append(label + " manifest: interface must be an object")
    else:
        for unsupported in sorted(set(interface) - INTERFACE_KEYS):
            errors.append(label + " manifest: interface." + unsupported
                          + " is not a supported interface key")
        for optional in sorted(set(interface) & (INTERFACE_KEYS - set(INTERFACE_FIELDS))):
            value = interface[optional]
            valid = (all(nonempty(item) for item in value) and value
                     if optional == "screenshots" and isinstance(value, list)
                     else nonempty(value))
            if not valid:
                errors.append(label + " manifest: interface." + optional
                              + " is present but not a usable value")
        errors += interface_option_errors(interface, payload, label)
        for field in INTERFACE_FIELDS:
            value = interface.get(field)
            if field in ("capabilities", "defaultPrompt"):
                valid = (isinstance(value, list) and value
                         and all(nonempty(item) for item in value))
            else:
                valid = nonempty(value)
            if not valid:
                errors.append(label + " manifest: interface." + field
                              + " must be a nonempty string" + (" list" if field in
                              ("capabilities", "defaultPrompt") else ""))
    declared, _, component_errors = declared_components(manifest)
    errors += [label + " manifest: " + problem for problem in component_errors]
    if payload is not None:
        for field, relative in declared:
            if relative not in payload:
                errors.append(label + " manifest: " + field + " names " + repr("./" + relative)
                              + ", which the package does not ship")
                continue
            data = payload[relative][1]
            if field == "hooks":
                errors += hook_document_errors(relative, data, label)
            else:
                errors += mcp_document_errors(relative, data, payload, label)
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
    category = (declared or {}).get("category") if isinstance(declared, dict) else None
    if not isinstance(entry.get("category"), str) or entry.get("category") != category:
        errors.append("marketplace: entry category must be the manifest category "
                      + repr(category))
    return errors


def hygiene(payload, manifest, label):
    errors = []
    for required in REQUIRED_FILES:
        if required not in payload:
            errors.append(label + ": " + required + " must ship with the package")
    try:
        _, roots, _ = declared_components(manifest)
    except (AttributeError, TypeError):
        roots = set(ALWAYS)
    for name, (_, data) in sorted(payload.items()):
        parts = PurePosixPath(name).parts
        if parts[0] not in roots:
            errors.append(label + " " + name + ": only " + ", ".join(sorted(roots))
                          + " may ship in the package; a component the manifest does not"
                            " declare installs without ever loading")
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
    prefix_parts = PurePosixPath(prefix).parts
    for name in payload:
        parts = PurePosixPath(name).parts
        # The declared path may be nested, so every component of it has to match.
        if len(parts) < len(prefix_parts) + 2 or parts[:len(prefix_parts)] != prefix_parts:
            continue
        found.setdefault(parts[len(prefix_parts)], set()).add(
            PurePosixPath(*parts[len(prefix_parts) + 1:]).as_posix())
    if not found:
        errors.append(label + ": the declared skills path ships no skill")
    declared, _, _ = declared_components(manifest)
    component_files = {relative for _, relative in declared}
    for name in sorted(payload):
        parts = PurePosixPath(name).parts
        if parts[0] in ALWAYS or name in component_files:
            continue
        if parts[:len(prefix_parts)] == prefix_parts:
            continue
        if any(name.startswith(PurePosixPath(relative).parts[0] + "/")
               for relative in component_files):
            # A declared component's own directory may hold what that component starts. The
            # skills path is checked as skills; this is checked by the component rules above.
            continue
        errors.append(label + " " + name + ": ships outside every declared component path")
    for skill, files in sorted(found.items()):
        for required in ("SKILL.md", "agents/openai.yaml"):
            if required not in files:
                errors.append(label + " " + prefix + "/" + skill + ": missing " + required)
        if "agents/openai.yaml" in files:
            errors += interface_errors(prefix + "/" + skill, payload, label)
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
    errors += manifest_errors(manifest, None, "installed", payload)
    errors += hygiene(payload, manifest, "installed")
    skill_errors, found = skills(payload, manifest, "installed")
    return errors + skill_errors, report_payload(payload, manifest, found,
                                                 {"source": "payload", "path": str(path)})


def record_version():
    """Write the suffix this working tree derives, so the recorded digest is never typed.

    The working tree is the only payload anyone can edit; the release payload is read from a
    revision, so this has to be committed before the check reads it there.
    """
    payload, errors = directory_payload(PLUGIN_ROOT)
    manifest = read_manifest(payload, "working tree")
    version = str(manifest.get("version", ""))
    if not SEMVER.fullmatch(version):
        errors.append("working tree manifest: version " + repr(version)
                      + " is not a semantic version, so no payload suffix can be recorded"
                        " under it")
    if errors:
        print("\n".join(sorted(set(errors))), file=sys.stderr)
        return 1
    expected = payload_version(payload, version)
    path = PLUGIN_ROOT / MANIFEST
    document = path.read_text(encoding="utf-8")
    recorded = json.dumps(version)
    if document.count(recorded) != 1:
        print(MANIFEST + " spells " + repr(version) + " " + str(document.count(recorded))
              + " times; exactly one of them is the version to rewrite", file=sys.stderr)
        return 1
    if version != expected:
        path.write_text(document.replace(recorded, json.dumps(expected)), encoding="utf-8")
    print("Version " + expected + " in " + MANIFEST
          + (": already recorded" if version == expected else ", recorded from "
             + str(len(payload)) + " shipped files. Commit it: the release payload is read"
             " from the revision, not from this tree"))
    return 0


def check_revision(revision):
    resolved = git("rev-parse", revision).strip()
    plugin_relative = PLUGIN_ROOT.relative_to(ROOT).as_posix()
    release, errors = revision_payload(resolved, plugin_relative)
    manifest = read_manifest(release, "release")
    errors += manifest_errors(manifest, PLUGIN_ROOT.name, "release", release)
    errors += hygiene(release, manifest, "release")
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
    errors += working_errors
    working_manifest, manifest_read_errors = safe_manifest(working, "working tree")
    errors += manifest_read_errors
    errors += hygiene(working, working_manifest or {}, "working tree")
    if working_manifest is not None:
        errors += manifest_errors(working_manifest, PLUGIN_ROOT.name, "working tree", working)
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
    parser.add_argument("--record-version", action="store_true",
                        help="Write the payload suffix this working tree derives into the"
                             " manifest version, then stop")
    parser.add_argument("--json", action="store_true", help="Print the machine-readable result")
    args = parser.parse_args()
    try:
        if args.record_version:
            return record_version()
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
