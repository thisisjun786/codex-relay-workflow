"""What is on this host, per surface, with the owner of each thing named.

Every reading answers its own question and says when it could not. Nothing here writes, and the two
readings that run another program run it read-only: scripts/install.py --check, which is how
runtime_install.py already reads the skill-link layer, and the relay's status command, which is
asked only when the store it would open already exists.
"""

import json
import os
import re
import subprocess
import sys
from pathlib import Path

from crw_runtime import bridgerecord, codexconfig, completion, hooks, pointer, reading

# The skills a CRW checkout carries, read from the checkout rather than listed here.
SKILL_PREFIX = "crw-"
SERVER_NAME = "codex-thread-bridge"
PLUGIN_NAME = "crw"
# The declared hook document, as the manifest names it. A trust key in config.toml carries this
# relative path, so this is what identifies trust for THIS hook rather than for some other plugin's.
HOOK_DOCUMENT = "wiring/hooks/stop-recording-completion.json"
# <destination>/current/bin/<script>: what a settings document records, and what it is stripped back
# to in order to learn the destination an existing install used.
POINTER_SEGMENTS = (pointer.POINTER_NAME, "bin")
RELAY_SCRIPT = "codex-session-relay"
ADAPTER_SCRIPT = "crw-completion-hook"
INTERPRETER_SCRIPT = "python3"

REPO_MARKERS = ("plugins/crw/.codex-plugin/plugin.json", "scripts/crw_runtime/completion.py")

STATE_KEY = re.compile(r'^\s*\[hooks\.state\."([^"]+)"\]\s*$')
PLUGIN_KEY = re.compile(r'^\s*\[plugins\."([^"]+)"\]\s*$')
# Any table header at all, because what ends a table is the next one starting, not the next table
# of the same kind. Reading past it attributed a later table's keys to this one.
TABLE = re.compile(r"^\s*\[")


def checkout_of(path):
    """The repository a path belongs to, or None. Proof, not a guess.

    A basename is not authorship: the Stop registration this repository writes names
    <checkout>/scripts/completion_hook.py, and any number of other programs could be called that.
    So the answer is the directory two levels up ONLY when that directory holds the two files a CRW
    checkout must have.
    """
    try:
        settled = Path(path).expanduser().resolve()
    except OSError:
        return None
    for candidate in list(settled.parents):
        if all((candidate / marker).is_file() for marker in REPO_MARKERS):
            return candidate
    return None


def config_path(codex_home):
    return Path(codex_home) / "config.toml"


def read_config_text(codex_home):
    return reading.read_text(config_path(codex_home), "the Codex configuration")


def table_span(text, header):
    """The exact lines of one TOML table, header to the line before the next table, or None.

    Proof of authorship is equality with what this repository renders, and equality needs a whole
    span: a table that keeps the rendered command and args and appends another field CONTAINS the
    rendered block, so a containment test calls it ours and a removal then deletes two of its three
    lines and leaves the rest orphaned under no table at all.
    """
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if line.strip() != header:
            continue
        collected = [line]
        for following in lines[index + 1:]:
            if TABLE.match(following):
                break
            collected.append(following)
        while collected and not collected[-1].strip():
            collected.pop()
        return "\n".join(collected) + "\n"
    return None

def read_plugin(codex_home, *, name=PLUGIN_NAME):
    """Whether the plugin is installed AND registered, and what its cache actually holds.

    Two facts, kept apart, because a cache directory is not a registration and neither is proof the
    package can serve what a transition is about to remove. The payload is checked against the
    manifest the package ships rather than against a list here.
    """
    answer = {"configEntry": reading.ABSENT, "entryKey": None, "enabled": None,
              "cacheVersion": None, "payload": {}, "skills": [], "detail": None,
              "trustKeys": [], "trusted": None}
    text = read_config_text(codex_home)
    if not text.usable:
        answer["configEntry"] = text.state
        answer["detail"] = text.detail
    else:
        block = None
        for line in text.value.splitlines():
            key = PLUGIN_KEY.match(line)
            if key:
                block = key.group(1) if key.group(1).split("@")[0] == name else None
                if block:
                    answer["configEntry"] = reading.PRESENT
                    answer["entryKey"] = block
                continue
            if TABLE.match(line):
                block = None
            trust = STATE_KEY.match(line)
            if trust and trust.group(1).split(":")[0].split("@")[0] == name:
                answer["trustKeys"].append(trust.group(1))
            if block and line.strip().startswith("enabled"):
                answer["enabled"] = line.split("=", 1)[1].strip() == "true"
        # Trust is positional and recorded per hook identity. A key naming this plugin and this
        # hook document is the only evidence available from a file; nothing here can grant it.
        # The key shape is <plugin>@<marketplace>:<document>:<event>:<matcher>:<index>, and the
        # document segment is compared as a whole. A substring test anywhere in the key would
        # accept trust recorded for a different document whose path merely contains this one.
        answer["trusted"] = any(
            len(key.split(":")) == 5 and key.split(":")[1] == HOOK_DOCUMENT
            for key in answer["trustKeys"])

    cache = Path(codex_home) / "plugins" / "cache"
    found = sorted(cache.glob(name + "/" + name + "/*")) if cache.is_dir() else []
    # glob is a declared omission: an unreadable cache directory yields nothing, which is reported
    # as no version rather than as a version that could not be read.
    # Which cached version a session loads is the host's answer, not this reader's, so a host
    # carrying more than one is reported rather than guessed at by sort order.
    answer["cacheVersions"] = [str(path) for path in found]
    version = found[0] if len(found) == 1 else None
    if len(found) > 1:
        answer["detail"] = ("more than one cached version is present (" 
                            + ", ".join(p.name for p in found)
                            + "), and which one a session loads is not readable from here")
    if version is not None:
        answer["cacheVersion"] = str(version)
        manifest = version / ".codex-plugin" / "plugin.json"
        answer["payload"]["manifest"] = manifest.is_file()
        declared = None
        if manifest.is_file():
            try:
                document = json.loads(manifest.read_text(encoding="utf-8"))
                declared = document.get("skills")
            except (OSError, ValueError) as error:
                answer["payload"]["manifest"] = False
                answer["detail"] = "the cached manifest could not be read: " + str(error)
        root = (version / str(declared)[2:].strip("/")) if isinstance(declared, str) \
            and declared.startswith("./") else None
        answer["payload"]["skills"] = bool(root and root.is_dir())
        if root and root.is_dir():
            answer["skills"] = sorted(p.name for p in root.iterdir()
                                      if (p / "SKILL.md").is_file())
        answer["payload"]["hookDocument"] = (version / HOOK_DOCUMENT).is_file()
        answer["payload"]["mcpDocument"] = (version / "wiring" / "mcp.json").is_file()
    return answer


def read_skill_links(codex_home, repo_root):
    """The link layer, read by running its own installer, never by copying what it does."""
    destination = Path(codex_home) / "skills"
    argv = [sys.executable, str(Path(repo_root) / "scripts" / "install.py"),
            "--check", "--dest", str(destination)]
    answer = {"command": argv, "linked": [], "missing": [], "conflict": [], "legacy": [],
              "crwOwned": [], "foreign": [], "unreadable": None}
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        answer["unreadable"] = type(error).__name__ + ": " + str(error)
        return answer
    answer["exitCode"] = done.returncode
    for line in (done.stdout + done.stderr).splitlines():
        for word, field in (("LINKED ", "linked"), ("MISSING ", "missing"),
                            ("CONFLICT ", "conflict"), ("LEGACY ", "legacy")):
            if line.startswith(word):
                answer[field].append(line[len(word):].split(" -> ")[0])
    # Ownership is decided per path, from the link itself, not from the installer's verdict: a
    # CONFLICT can be somebody else's directory, and those are never touched.
    if destination.is_dir():
        for entry in sorted(destination.iterdir()):
            if not entry.name.startswith(SKILL_PREFIX):
                continue
            owner = checkout_of(entry) if entry.is_symlink() else None
            if owner is not None and (entry.resolve() / "SKILL.md").is_file():
                answer["crwOwned"].append({"path": str(entry), "checkout": str(owner),
                                           "target": str(entry.resolve())})
            else:
                answer["foreign"].append({"path": str(entry),
                                          "why": "not a symlink into a CRW checkout"})
    return answer


def canonical_command(argv):
    """The command this repository's own writer would emit for these words, or None.

    Authorship is byte equality with that writer, including its quoting. The name test alone is
    defeatable: names_this_adapter returns the first word whose basename matches, wherever it sits,
    so a foreign command that merely PASSES our adapter as an argument would pass a name check.
    """
    if not argv or len(argv) not in (2, 3):
        return None
    script = argv[1]
    if Path(script).name != completion.ENTRY_POINT_NAME:
        return None
    return completion.command_for(argv[0], script, argv[2] if len(argv) == 3 else None)


def read_hook(codex_home, event=None, *, destination=None):
    """Every registration of this adapter in the hook file, with authorship and what follows it."""
    event = event or completion.EVENT
    path = Path(codex_home) / "hooks.json"
    answer = {"hookFile": str(path), "reading": None, "entries": [], "later": [],
              "unrecognised": [], "event": event}
    document = hooks.read(path)
    if not document.usable:
        answer["reading"] = document.refusal()
        return answer
    inventory_all = hooks.inventory(document.value, event)
    entries = completion.adapter_entries(document.value, event)
    for entry in entries:
        argv = completion.registered_argv(entry["command"]) or []
        canonical = canonical_command(argv)
        checkout = checkout_of(argv[1]) if len(argv) > 1 else None
        proven = bool(canonical is not None and canonical == entry["command"]
                      and checkout is not None)
        answer["entries"].append({**entry, "argv": argv, "proven": proven,
                                  "checkout": str(checkout) if checkout else None,
                                  "canonical": canonical,
                                  "why": None if proven else
                                  "the command is not what this repository's writer emits for the"
                                  " words it names, or its script is not inside a CRW checkout"})
    # A registration naming the PACKAGED adapter cannot be seen by names_this_adapter, because that
    # matcher knows one file name. No command in this repository can write one, so its presence
    # means a hand edit -- and an unreported hand edit is the silence this detector exists to break.
    for item in inventory_all:
        command = str(item.get("command") or "")
        if completion.names_this_adapter(command):
            continue
        if ADAPTER_SCRIPT in command or (destination and str(destination) in command):
            answer["unrecognised"].append({"identity": item["identity"], "command": command,
                                           "why": "names the packaged adapter or the destination"
                                                  " but is not a registration this repository"
                                                  " wrote"})
    identities = [item["identity"] for item in inventory_all]
    # The union across every entry being removed, and only identities that are not themselves being
    # removed. Assigning per entry kept the last one's list and under-reported what would shift.
    ours = {entry["identity"] for entry in answer["entries"]}
    shifted = []
    for entry in answer["entries"]:
        if entry["identity"] not in identities:
            continue
        position = identities.index(entry["identity"])
        shifted += [name for name in identities[position + 1:]
                    if name not in ours and name not in shifted]
    answer["later"] = shifted
    return answer


def newest_retired(codex_home):
    """The most recently retired settings document, and the file it came from.

    Retiring is what disable does and what the transition does before it writes, so this is the
    only place the operational locations survive once the live document is gone. Reading them is
    the difference between re-enabling an installation and pointing it at a fresh empty store.
    """
    home = Path(codex_home)
    found = sorted(p for p in home.glob(completion.CONFIG_NAME + ".superseded-*") if p.is_file())
    for candidate in reversed(found):
        try:
            document = json.loads(candidate.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if not isinstance(document, dict) or completion.complaints(document):
            # A retired file that no longer reads as settings is not a source for the locations
            # an installation depends on. Restoring one would point a re-enabled install at
            # whatever survived in it.
            continue
        return document, candidate.name
    return None, None


def read_settings(codex_home):
    """This hook's settings, and who owns the registration they belong to."""
    path = completion.configuration_path(codex_home)
    document, outcome, detail, found = completion.read_configuration(path)
    return {"path": str(path), "outcome": outcome, "detail": detail,
            "owner": completion.owner_of(document) if document else None,
            "document": document,
            "state": found.state if found is not None else None}


def destination_from(document):
    """The install destination an existing settings document was written against.

    Derived from the recorded relay path rather than asked for again, so the records this transition
    writes name the runtime the host is already using.
    """
    recorded = (document or {}).get("relayExecutable")
    if not recorded:
        return None
    path = Path(recorded)
    if path.name != RELAY_SCRIPT or len(path.parents) < 3:
        return None
    if path.parent.name != POINTER_SEGMENTS[1] or path.parent.parent.name != POINTER_SEGMENTS[0]:
        return None
    return path.parent.parent.parent


def read_mcp(codex_home, *, name=SERVER_NAME):
    """The bridge registration on both sides: the configuration table, and the record."""
    path = config_path(codex_home)
    answer = {"configPath": str(path), "table": reading.ABSENT, "tableProven": False,
              "registration": None, "record": None, "recordOutcome": None,
              "recordOwner": None, "recordPath": str(bridgerecord.record_path(codex_home)),
              "detail": None}
    text = read_config_text(codex_home)
    if not text.usable:
        answer["table"] = text.state
        answer["detail"] = text.detail
    else:
        view = codexconfig.scan(text.value)
        if not view.readable:
            answer["table"] = codexconfig.UNREADABLE
            answer["detail"] = "; ".join(view.unreadable)
        else:
            present, registration = codexconfig.registration_of(view, name)
            if present:
                answer["table"] = reading.PRESENT
                answer["registration"] = registration
                # Proven when the bytes in the file are exactly what this repository renders for
                # the registration it finds there. Anything else is somebody's own edit.
                rendered = codexconfig.render(name, registration.get("command"),
                                              registration.get("args") or [])
                span = table_span(text.value, "[mcp_servers." + codexconfig.key(name) + "]")
                answer["renderedTable"] = rendered
                answer["tableSpan"] = span
                answer["tableProven"] = span is not None and span.strip() == rendered.strip()
                if span is not None and not answer["tableProven"]:
                    answer["detail"] = ("the table holds more or other than the command and"
                                        " arguments this repository renders for it")
    document, outcome, detail = bridgerecord.read(Path(answer["recordPath"]))
    answer["record"] = document
    answer["recordOutcome"] = outcome
    answer["recordOwner"] = bridgerecord.owner_of(document)
    if detail and not answer["detail"]:
        answer["detail"] = detail
    return answer


def read_pointer(destination):
    if not destination:
        return {"destination": None, "state": pointer.NO_POINTER, "target": None,
                "detail": "no destination was named or derived"}
    path = pointer.pointer_path(destination)
    found = pointer.read(path)
    resolved = None
    if found.get("target"):
        # The pointer's own answer is about the LINK. Whether the target is there is a second
        # question, and a dangling link answers LINK to the first one.
        candidate = Path(found["target"])
        if not candidate.is_absolute():
            candidate = Path(destination) / candidate
        resolved = str(candidate) if candidate.is_dir() else None
    return {"destination": str(destination), "pointer": str(path), "state": found.get("state"),
            "target": found.get("target"), "detail": found.get("detail"),
            "targetDirectory": resolved}


def read_in_flight(document):
    """Work the relay is carrying, read without ever creating a store.

    The relay's Store opens its file O_RDWR and runs the schema script on open, so asking a relay
    about a database that is not there CREATES an empty one, and an empty store answers "nothing in
    flight". That answer would be wrong in the one direction that matters, so the question is only
    asked when the file already exists, and the marker root is listed either way.
    """
    answer = {"state": reading.ABSENT, "markerEntries": None, "snapshot": None, "detail": None,
              "how": []}
    if not document:
        answer["state"] = reading.UNREADABLE
        answer["detail"] = "no settings document, so no relay or marker root was named"
        return answer
    marker = document.get("markerRoot")
    if marker:
        root = Path(marker)
        answer["how"].append("listed " + str(root))
        if root.is_dir():
            answer["markerEntries"] = sorted(p.name for p in root.iterdir())[:50]
            answer["state"] = reading.PRESENT
    database = document.get("dbPath")
    if database:
        answer["storePath"] = str(database)
        answer["storeExists"] = Path(str(database)).is_file()
    # The relay is deliberately not asked. Its status subcommand takes no store argument, so on a
    # host whose dbPath sits outside the default state directory it would answer about a different
    # store -- and on a host with no store there it would CREATE an empty one, which answers
    # "nothing in flight" for the wrong reason. Its snapshot is also nonempty for an idle relay,
    # so a truthy object is not evidence of work either. The marker root is the reading that
    # actually carries published work, and it is a directory listing.
    answer["how"].append("did not run the relay status command: it cannot be aimed at a store,"
                         " an absent store would be created by asking, and an idle relay answers"
                         " with a nonempty object")
    return answer


def snapshot(codex_home, *, repo_root, destination=None, event=None):
    """One reading of everything, taken once so every later decision sees the same host."""
    settings = read_settings(codex_home)
    document = settings.get("document")
    if document is None:
        # A host that has been disabled, or interrupted after the retire step, has no live
        # document at all. The destination and the operational locations are still recorded in the
        # file that was retired, so they are read from there rather than asked for again.
        retired, name = newest_retired(codex_home)
        settings["retiredFrom"] = name
        document = retired
    derived = destination_from(document)
    dest = destination or derived
    return {
        "codexHome": str(codex_home),
        "repoRoot": str(repo_root),
        "destination": str(dest) if dest else None,
        "destinationDerivedFrom": "the recorded relayExecutable" if derived and not destination
        else ("the caller" if destination else None),
        "plugin": read_plugin(codex_home),
        "skills": read_skill_links(codex_home, repo_root),
        "hook": read_hook(codex_home, event, destination=dest),
        "settings": settings,
        "mcp": read_mcp(codex_home),
        "pointer": read_pointer(dest),
        "inFlight": read_in_flight(document),
    }

