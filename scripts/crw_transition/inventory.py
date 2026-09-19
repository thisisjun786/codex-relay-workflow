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
BRIDGE_SCRIPT = "codex-thread-bridge"
ADAPTER_SCRIPT = "crw-completion-hook"
INTERPRETER_SCRIPT = "python3"

REPO_MARKERS = ("plugins/crw/.codex-plugin/plugin.json", "scripts/crw_runtime/completion.py")

try:
    import tomllib
except ImportError:  # the documented 3.10 floor
    tomllib = None
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
    except (OSError, RuntimeError):
        # RuntimeError, not only OSError: before 3.13 a non-strict resolve() answers a symlink
        # loop with RuntimeError, and this repository's floor is 3.10. A looping crw-* link left
        # by anything at all would otherwise end every command in an internal error instead of
        # being classified -- and a link this cannot follow is exactly a link it does not own.
        return None
    for candidate in list(settled.parents):
        if all((candidate / marker).is_file() for marker in REPO_MARKERS):
            return candidate
    return None


def same_adapter(path, repo_root):
    """Whether the file at path IS this repository's adapter, compared byte for byte.

    Marker files prove a directory is laid out like a checkout, and a directory can be laid out
    like anything. What a registration runs is the adapter itself, so that is what is compared.
    """
    ours = Path(repo_root) / "scripts" / completion.ENTRY_POINT_NAME
    try:
        return ours.is_file() and Path(path).read_bytes() == ours.read_bytes()
    except OSError:
        return False


def settle(path):
    """One spelling for a path, so two names for one file compare equal."""
    try:
        return os.path.realpath(os.path.expanduser(str(path)))
    except (OSError, ValueError):
        return str(path)


def runs_the_bridge(command):
    """Whether a registered command starts the task bridge, whatever the table is called.

    The console script's name is the fact that travels: a registration made by this repository
    runs <destination>/current/bin/codex-thread-bridge, and an operator who renamed the SERVER did
    not rename that.
    """
    if not command:
        return False
    return Path(settle(command)).name == BRIDGE_SCRIPT

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
        child = header[:-1] + "."
        for following in lines[index + 1:]:
            if TABLE.match(following):
                # A nested table such as [mcp_servers.<name>.env] is part of the SAME registration
                # even though it starts with a header, so the span has to reach it. Stopping short
                # would prove only the parent block, and removing that block would leave the nested
                # one orphaned under no server at all. Included here, the span stops matching what
                # this repository renders, which is the correct answer: not ours, left alone.
                if not following.strip().startswith(child):
                    break
            collected.append(following)
        while collected and not collected[-1].strip():
            collected.pop()
        return "\n".join(collected) + "\n"
    return None

def removal_is_structural(text, span, name):
    """Whether removing this span is a TOML edit and not a cut through a string.

    table_span finds its header by reading lines, and a line inside a multiline string can look
    exactly like one. So the answer is checked against a parser: what is left has to parse, it has
    to no longer register this server, and every other server has to survive unchanged.
    """
    if tomllib is None:
        return False
    try:
        before = tomllib.loads(text)
        after = tomllib.loads(text.replace(span, "", 1))
    except Exception:  # noqa: BLE001 - either side failing means this is not a clean removal
        return False
    mine = before.get("mcp_servers") or {}
    theirs = after.get("mcp_servers") or {}
    if name in theirs or name not in mine:
        return False
    return {key: value for key, value in mine.items() if key != name} == theirs

def read_plugin(codex_home, *, name=PLUGIN_NAME):
    """Whether the plugin is installed AND registered, and what its cache actually holds.

    Two facts, kept apart, because a cache directory is not a registration and neither is proof the
    package can serve what a transition is about to remove. The payload is checked against the
    manifest the package ships rather than against a list here.
    """
    answer = {"configEntry": reading.ABSENT, "entryKey": None, "entryKeys": [], "enabled": None,
              "cacheVersion": None, "payload": {}, "skills": [], "detail": None,
              "trustKeys": [], "trusted": None}
    text = read_config_text(codex_home)
    if not text.usable:
        answer["configEntry"] = text.state
        answer["detail"] = text.detail
    elif tomllib is None:
        # Refused rather than approximated, the way this repository's own configuration reader
        # refuses: a line-oriented scan reads the contents of a multiline string as structure.
        answer["configEntry"] = reading.UNREADABLE
        answer["detail"] = ("reading a Codex configuration needs tomllib, so whether this plugin"
                            " is registered was not established on this interpreter")
    else:
        try:
            parsed = tomllib.loads(text.value)
        except Exception as error:  # noqa: BLE001 - a file that will not parse is a reading
            answer["configEntry"] = reading.UNREADABLE
            answer["detail"] = "this file is not readable TOML: " + type(error).__name__ + ": " + str(error)
            parsed = None
        if parsed is not None:
            plugins = parsed.get("plugins")
            entry = None
            if isinstance(plugins, dict):
                # Every entry, not the first: Codex identifies an installation by <plugin>@<market>,
                # so two marketplaces can each register crw and both declarations keep loading.
                # Stopping at the first validated one marketplace's cache and left the other one
                # running, which is the double fire this transition exists to end.
                matching = [(key, value) for key, value in sorted(plugins.items())
                            if key.split("@")[0] == name and isinstance(value, dict)]
                answer["entryKeys"] = [key for key, _value in matching]
                if matching:
                    entry, answer["entryKey"] = matching[0][1], matching[0][0]
                    answer["configEntry"] = reading.PRESENT
                if len(matching) > 1:
                    answer["detail"] = ("this configuration registers " + name + " from more than"
                                        " one marketplace (" + ", ".join(answer["entryKeys"])
                                        + "), and every one of them loads")
            if entry is not None and "enabled" in entry:
                answer["enabled"] = entry["enabled"] is True
            state = (parsed.get("hooks") or {}).get("state")
            if isinstance(state, dict):
                answer["trustKeys"] = [key for key in sorted(state)
                                       if key.split(":")[0].split("@")[0] == name]
            # A key naming this plugin and this hook document is the only evidence a file carries,
            # and it is NOT proof the hook will fire: the recorded hash belongs to the hook as it
            # stood when trust was given, and nothing here can compute the hash Codex compares it
            # against. So this reports what it found and refuses to call it trust.
            answer["trustKeyPresent"] = any(
                len(key.split(":")) == 5 and key.split(":")[1] == HOOK_DOCUMENT
                for key in answer["trustKeys"])
            answer["trustNote"] = ("a trust key was found and its recorded hash was NOT compared"
                                   " with the installed hook, which this repository cannot do"
                                   if answer["trustKeyPresent"] else
                                   "no trust key names this plugin and this hook document")

    cache = Path(codex_home) / "plugins" / "cache"
    # The cache layout is <marketplace>/<plugin>/<version> and the entry key is
    # <plugin>@<marketplace>, so the marketplace comes from the registration. Assuming it matches
    # the plugin name would validate an old cache under another marketplace as the replacement for
    # a registration pointing somewhere else.
    marketplace = (answer["entryKey"].split("@", 1)[1]
                   if answer.get("entryKey") and "@" in answer["entryKey"] else name)
    answer["marketplace"] = marketplace
    found = sorted(cache.glob(marketplace + "/" + name + "/*")) if cache.is_dir() else []
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
            try:
                answer["skills"] = sorted(p.name for p in root.iterdir()
                                          if (p / "SKILL.md").is_file())
            except OSError as error:
                # The version cache is replaced wholesale by an install, so it can go away
                # between these two calls. Raising here left transition() with no step results
                # at all -- not even the warning that the completion registration is already
                # gone -- because the readiness recheck runs after the standdown. It is a
                # payload this run could not read, which the refusal path already handles.
                answer["payload"]["skills"] = False
                answer["skills"] = []
                answer["detail"] = ("the cached skills at " + str(root) + " could not be listed ("
                                    + type(error).__name__ + ": " + str(error) + ")")
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
    recognised = 0
    for line in (done.stdout + done.stderr).splitlines():
        for word, field in (("LINKED ", "linked"), ("MISSING ", "missing"),
                            ("CONFLICT ", "conflict"), ("LEGACY ", "legacy")):
            if line.startswith(word):
                answer[field].append(line[len(word):].split(" -> ")[0])
                recognised += 1
    if done.returncode != 0 and not recognised:
        # A nonzero exit on its own is the ORDINARY answer here -- --check exits 1 while a link is
        # missing -- so it cannot stand for unreadable. A nonzero exit that printed none of the
        # four kinds is different: the installer stopped before it inspected anything, and its
        # four lists are empty because nothing was looked at rather than because nothing is there.
        # crwOwned and foreign are decided below by reading the directory, which is why ownership
        # survives this, but the installer's own verdict must not be reported as an empty one.
        answer["unreadable"] = ("scripts/install.py --check exited " + str(done.returncode)
                                + " without inspecting any link: "
                                + (done.stdout + done.stderr).strip()[:300])
    # Ownership is decided per path, from the link itself, not from the installer's verdict: a
    # CONFLICT can be somebody else's directory, and those are never touched.
    if destination.is_dir():
        try:
            found = sorted(destination.iterdir())
        except OSError as error:
            # The link directory can go away or stop being listable between these two calls --
            # a manual install cleaning up beside this run is the ordinary way. This reader is
            # also the one skill_unlink re-reads at the END, after the hook and the bridge have
            # already moved, and raising there threw away every step result and the sentence
            # that says the host is half transitioned. Unreadable is an answer the refusal path
            # already knows how to report; an exception is not.
            answer["unreadable"] = (str(destination) + " could not be listed ("
                                    + type(error).__name__ + ": " + str(error) + ")")
            found = []
        for entry in found:
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
    if not all(os.path.isabs(word) for word in argv[1:]):
        # The hook fires from each session's own workspace and this command runs somewhere else, so
        # a relative path names one file here and another one there. Unproven rather than resolved.
        return None
    return completion.command_for(argv[0], script, argv[2] if len(argv) == 3 else None)


def event_registrations(document, event):
    """Every registration under this event as its identity and the command it runs.

    hooks.inventory() answers identity, matcher and trusted hash, deliberately: it exists to
    compare positions and trust, and a command is none of those. A reader that needs the command
    has to walk the document, and the positions are built the same way that inventory builds them
    so the two agree about what an identity means.
    """
    found = []
    for matcher_index, group in enumerate((document.get("hooks") or {}).get(event) or []):
        for hook_index, hook in enumerate((group or {}).get("hooks") or []):
            found.append({
                "identity": hooks.identity(hooks.SOURCE, event, matcher_index, hook_index),
                "command": str((hook or {}).get("command") or ""),
            })
    return found


def read_hook(codex_home, event=None, *, destination=None, repo_root=None):
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
        identical = bool(len(argv) > 1 and repo_root and same_adapter(argv[1], repo_root))
        proven = bool(canonical is not None and canonical == entry["command"]
                      and checkout is not None and identical)
        answer["entries"].append({**entry, "argv": argv, "proven": proven,
                                  "checkout": str(checkout) if checkout else None,
                                  "canonical": canonical,
                                  "adapterIsOurs": identical,
                                  "why": None if proven else
                                  "the command is not what this repository's writer emits for the"
                                  " words it names, or the file it runs is not this repository's"
                                  " own adapter"})
    # A registration naming the PACKAGED adapter cannot be seen by names_this_adapter, because that
    # matcher knows one file name. No command in this repository can write one, so its presence
    # means a hand edit -- and an unreported hand edit is the silence this detector exists to break.
    #
    # Walked over the document rather than over hooks.inventory(): that inventory answers identity,
    # matcher and trusted hash and carries no command at all, so reading a command out of it was
    # reading an absent key. Every entry compared as the empty string, none of them matched, and
    # this detector reported nothing on every host it was meant to catch.
    for item in event_registrations(document.value, event):
        command = item["command"]
        if completion.names_this_adapter(command):
            continue
        if ADAPTER_SCRIPT in command or (destination and str(destination) in command):
            answer["unrecognised"].append({"identity": item["identity"], "command": command,
                                           "why": "names the packaged adapter or the destination"
                                                  " but is not a registration this repository"
                                                  " wrote"})
    # What actually shifts, and nothing else. Removal pops an entry out of ITS OWN matcher group's
    # hooks list, and an emptied group is left in place, so matcher indices never move: only hooks
    # later in the SAME group take a new index. A flattened list also counted a foreign hook in a
    # later group, which refused a transition that shifts nothing and claimed a trust was detached
    # when it was not.
    answer["later"] = shifted_identities(inventory_all, answer["entries"])
    return answer


def identity_position(identity):
    parts = identity.split(":")
    return int(parts[-2]), int(parts[-1])


def shifted_identities(inventory_all, entries):
    """Which recorded identities take a new index when these entries are removed.

    Removal pops out of its own matcher group and an emptied group is left in place, so matcher
    indices never move: only hooks later in the SAME group shift. Computed from a document rather
    than remembered, because the file is not locked when the first reading is taken and a hook that
    lands in between shifts without anyone having consented to it.
    """
    ours = {entry["identity"] for entry in entries}
    removed = [identity_position(identity) for identity in ours]
    shifted = []
    for item in inventory_all:
        if item["identity"] in ours:
            continue
        matcher, index = identity_position(item["identity"])
        if any(matcher == cut_matcher and index > cut_index
               for cut_matcher, cut_index in removed):
            shifted.append(item["identity"])
    return shifted


def archive_order(path, stem):
    """Sort key for a retired archive: its stamp, then its collision suffix as a NUMBER.

    Padding keeps lexical order only as far as the padding goes, and the next collision after it
    sorts back under the previous one. Parsing the suffix removes the bound rather than moving it,
    and an unparsable name sorts first so it can never be chosen as the newest.
    """
    tail = str(Path(path).name)[len(stem):]
    stamp, _, suffix = tail.partition("-")
    try:
        return (stamp, int(suffix) if suffix else 0)
    except ValueError:
        return ("", -1)


def newest_retired(codex_home):
    """The most recently retired settings document, and the file it came from.

    Retiring is what disable does and what the transition does before it writes, so this is the
    only place the operational locations survive once the live document is gone. Reading them is
    the difference between re-enabling an installation and pointing it at a fresh empty store.
    """
    home = Path(codex_home)
    stem = completion.CONFIG_NAME + ".superseded-"
    found = sorted((p for p in home.glob(stem + "*") if p.is_file()),
                   key=lambda path: archive_order(path, stem))
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


# The fields a transition must not change without being asked: where the work is recorded and which
# store it is recorded in. Two registrations naming documents that disagree about these are two
# installations, and choosing between them by hook order picks one silently.
OPERATIONAL = ("markerRoot", "dbPath", "journalRoot", "relayExecutable",
               "mode", "isolationAssertedBy", "timeoutSeconds", "journalPolicy")


def registered_document(hook, settings, codex_home):
    """The settings the registration actually reads, where they came from, and any disagreement.

    A manual hook records its settings path permanently, so a valid document at the fixed path is
    not necessarily the one in use -- and everything downstream, including which destination this
    host runs, has to follow the registered one rather than whichever file is easiest to find.
    """
    documents, unreadable = [], []
    for entry in hook.get("entries") or []:
        if not entry.get("proven"):
            continue
        # A proven command with no settings argument is not a registration without settings: its
        # adapter resolves the path the same way this does, from the environment and then the
        # Codex home. Skipping it compared one registration and removed two, so a host carrying a
        # default-path registration beside one naming a custom document had the default one's
        # store, marker and journal abandoned without ever being compared with the winner.
        named = entry.get("settings") or str(completion.configuration_path(codex_home))
        document, outcome, detail, _found = completion.read_configuration(Path(named))
        if document is None:
            if outcome == completion.CONFIG_ABSENT:
                # Absent is the interrupted-run state: a previous attempt archived this file and
                # stopped before writing the new one, and the recovery is exactly what the retired
                # document is for. It falls through rather than refusing.
                continue
            # Anything else is a file that is there and cannot be acted on. Erasing it and falling
            # back to the fixed path makes a registration whose settings could not be read
            # indistinguishable from one that names none, and the fallback then configures the
            # plugin from another installation's store, marker, mode and journal.
            unreadable.append(str(named) + " (" + str(outcome) + ": " + str(detail) + ")")
            continue
        documents.append((named, document))
    distinct = {tuple(document.get(field) for field in OPERATIONAL)
                for _named, document in documents}
    if unreadable or len(distinct) > 1:
        return None, None, {"disagree": [named for named, _document in documents]
                            if len(distinct) > 1 else [],
                            "unreadable": unreadable,
                            "documents": dict(documents)}
    if documents:
        return (documents[0][1], "the settings the registration names, " + documents[0][0],
                {"documents": dict(documents)})
    if settings.get("document") is not None:
        return (settings["document"], "the settings at " + str(settings["path"]),
                {"documents": {str(settings["path"]): settings["document"]}})
    retired, name = newest_retired(codex_home)
    return retired, ("the retired document " + name) if name else None, {}

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
            # register-mcp takes --name, so a manual install may have registered this same bridge
            # under another server name. Looking only for the declared name would leave that table
            # in place beside the plugin's declaration, and the host would start two bridges.
            answer["aliases"] = [
                {"name": other, "command": entry.get("command"), "args": entry.get("args")}
                for other, entry in sorted(view.servers.items())
                if other != name and runs_the_bridge(entry.get("command"))]
            present, registration = codexconfig.registration_of(view, name)
            if present:
                answer["table"] = reading.PRESENT
                answer["registration"] = registration
                # Proven when the bytes in the file are exactly what this repository renders for
                # the registration it finds there. Anything else is somebody's own edit.
                rendered = codexconfig.render(name, registration.get("command"),
                                              registration.get("args") or [])
                header = "[mcp_servers." + codexconfig.key(name) + "]"
                span = table_span(text.value, header)
                # A nested table belongs to the same registration and TOML lets it sit anywhere in
                # the file, so it is looked for everywhere rather than only after the parent.
                # Removing the parent while one exists would orphan it under no server at all.
                nested = [line.strip() for line in text.value.splitlines()
                          if line.strip().startswith(header[:-1] + ".")]
                answer["nestedTables"] = nested
                answer["renderedTable"] = rendered
                answer["tableSpan"] = span
                answer["tableProven"] = (span is not None and not nested
                                         and span.strip() == rendered.strip()
                                         and removal_is_structural(text.value, span, name))
                if span is not None and not answer["tableProven"]:
                    answer["detail"] = ("the table holds more or other than the command and"
                                        " arguments this repository renders for it"
                                        + (", including " + ", ".join(nested) if nested else ""))
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
    """What the records say about work, and what they cannot say.

    The relay's Store opens its file O_RDWR and runs the schema script on open, so asking a relay
    about a database that is not there CREATES an empty one, and an empty store answers "nothing in
    flight". That answer would be wrong in the one direction that matters, so the question is only
    asked when the file already exists, and the marker root is listed either way.
    """
    answer = {"state": reading.ABSENT, "markerHistory": None, "liveness": None, "detail": None,
              "how": [],
              "note": ("a workspace marker is created once and stays after the work it recorded"
                       " finished, so its entries are history rather than work in flight. Whether"
                       " a turn is running right now is not establishable from these records, and"
                       " this reading does not pretend otherwise: it reports what is there and"
                       " refuses nothing on the strength of it.")}
    if not document:
        answer["state"] = reading.UNREADABLE
        answer["detail"] = "no settings document, so no relay or marker root was named"
        return answer
    marker = document.get("markerRoot")
    if marker:
        root = Path(marker)
        answer["how"].append("listed " + str(root))
        try:
            if root.is_dir():
                answer["markerHistory"] = sorted(p.name for p in root.iterdir())[:50]
                answer["state"] = reading.PRESENT
        except OSError as error:
            # A directory whose metadata is visible and whose contents are not, or one that goes
            # away between the two calls. This reading is informational -- it refuses nothing --
            # so a failure here is recorded as the state it is rather than raised out of the
            # snapshot, where it would take the whole host inventory down with it and stop
            # inspect and the transition before preflight ever decided anything.
            answer["state"] = reading.ACCESS_ERROR if isinstance(error, PermissionError) \
                else reading.UNREADABLE
            answer["markerHistory"] = None
            answer["detail"] = (str(root) + " could not be listed (" + type(error).__name__
                                + ": " + str(error) + ")")
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
    # The hook is read before the destination is derived, because the destination has to come from
    # the document the registration reads. Deriving it from whatever sits at the fixed path
    # rewrote the plugin settings to run the relay and the adapter from another installation, and
    # reported success doing it. The second read carries the destination for the detector.
    first = read_hook(codex_home, event, repo_root=repo_root)
    document, carried, conflict = registered_document(first, settings, codex_home)
    if document is None and not (conflict or {}).get("disagree") \
            and not (conflict or {}).get("unreadable"):
        # A host that has been disabled, or interrupted after the retire step, has no live
        # document at all. The destination and the operational locations are still recorded in the
        # file that was retired, so they are read from there rather than asked for again.
        retired, name = newest_retired(codex_home)
        settings["retiredFrom"] = name
        document = retired
    settings["carriedFrom"] = carried
    settings["conflictingRegistrations"] = conflict
    derived = destination_from(document)
    dest = destination or derived
    # Re-derived from the reading that is RETURNED. The first read only answered which
    # destination to carry into the second; pairing one read's settings with another read's
    # inventory would configure the plugin from a registration that is no longer there.
    hook = read_hook(codex_home, event, destination=dest, repo_root=repo_root)
    document, carried, conflict = registered_document(hook, settings, codex_home)
    settings["carriedFrom"] = carried
    settings["conflictingRegistrations"] = conflict
    again = destination_from(document)
    if not destination and again is not None and str(again) != str(dest or ""):
        # The registration or its settings moved between the two reads, so the destination the
        # first read answered is not the one this document names. Reported as a changed host
        # rather than validating one installation and configuring another.
        settings["destinationChanged"] = {"first": str(dest) if dest else None,
                                          "second": str(again)}
        dest = again
    return {
        "codexHome": str(codex_home),
        "repoRoot": str(repo_root),
        "destination": str(dest) if dest else None,
        "destinationDerivedFrom": "the recorded relayExecutable" if derived and not destination
        else ("the caller" if destination else None),
        "plugin": read_plugin(codex_home),
        "skills": read_skill_links(codex_home, repo_root),
        "hook": hook,
        "registered": {"document": document, "from": carried, "conflict": conflict},
        "settings": settings,
        "mcp": read_mcp(codex_home),
        "pointer": read_pointer(dest),
        "inFlight": read_in_flight(document),
    }
