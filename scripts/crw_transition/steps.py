"""The ordered transition, and the refusals that protect a working host from it.

One rule underneath all of it: nothing is removed that this repository cannot prove runs its own
code, and nothing is removed before the replacement is proven able to serve it. Every step decides
from what is on disk, so an interrupted run converges on the next one.
"""

import contextlib
import errno
import importlib.util
import json
import os
import re
import shutil
import shlex
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

from crw_runtime import bridgerecord, codexconfig, completion, hooks, hostrecord, reading

from . import inventory

SETTLED = "settled"
ALREADY = "already_done"
WOULD = "would_change"
REFUSED = "refused"
BUSY = "busy"
NOT_REACHED = "not_reached"

# Steps that changed something are reported apart from steps that found nothing to do, because
# "converged" and "did the work" are different answers and a rerun has to be able to say which.
DONE = (SETTLED, ALREADY)

# The packaged launcher waits min(timeoutSeconds + MARGIN, MAX) seconds, with MARGIN 2 and MAX the
# number completion.py calls LAUNCHER_CEILING_SECONDS. So a budget at the ceiling is not the
# problem the ceiling was written for: every budget above MAX - MARGIN collapses the margin the
# launcher exists to keep, and at 8.999 the launcher's deadline arrives first and discards the
# record the adapter was in the middle of writing. What a plugin-owned document may record is
# therefore the ceiling minus the margin, and this is where that is enforced, because the
# validation the two adapters share lives in a module this branch does not own.
LAUNCHER_MARGIN_SECONDS = 2
MAX_GUARD_SECONDS = completion.LAUNCHER_CEILING_SECONDS - LAUNCHER_MARGIN_SECONDS


def stamp():
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def retire(path, into=None, stem=None):
    """Move a record aside under a name nothing reads, and never delete it.

    into/stem exist for one case: a manual install created with CRW_COMPLETION_HOOK_CONFIG records
    that custom path in its hook command permanently, and the variable need not still be set when
    this runs. Archiving such a file beside itself puts it somewhere the recovery does not look --
    and the recovery is what a later run needs to carry the marker root, the database and the
    journal forward. The archive of a document proven to be ours therefore goes to the Codex home
    under the name the recovery globs, with the original path recorded in the receipt.

    Retiring rather than deleting is the whole difference between a transition and a data loss: the
    old settings carry the marker root, the database and the journal an operator may still need to
    read, and this tool is not entitled to decide they are finished with.
    """
    # Second granularity is not enough on its own: two retirements of the same path within one
    # second would name the same archive and os.replace would delete the first one. The name is
    # taken with O_EXCL, so an existing archive is never the destination.
    base = str(Path(into) / (stem + ".superseded-" + stamp())) if into \
        else str(path) + ".superseded-" + stamp()
    target, suffix = base, 0
    while True:
        try:
            handle = os.open(target, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            suffix += 1
            # Zero-padded, because the archives are recovered by sorting their names: an unpadded
            # -10 sorts before -9 and the recovery would read a stale document as the newest one.
            target = base + "-%03d" % suffix
            continue
        os.close(handle)
        break
    try:
        os.replace(str(path), target)
    except OSError as error:
        if error.errno != errno.EXDEV:
            _discard(target)
            raise
        # A custom settings path can live on another filesystem -- a mounted volume is the ordinary
        # case -- and a rename cannot cross one. Copy first, then unlink, so the archive exists
        # before the original stops existing: this failure happens after the standdown, and an
        # unrecoverable source there means no completion hook at all.
        try:
            shutil.copy2(str(path), target)
        except OSError:
            _discard(target)
            raise
        os.unlink(str(path))
    return target


def _discard(target):
    """Remove this function's own placeholder, so a retry does not skip a name for no reason."""
    try:
        os.unlink(target)
    except OSError:
        pass


def symlink_complaint(path):
    """Why an artifact that is a symlink is not archived by moving it, or None.

    An archive is made by renaming, and renaming a link relocates the LINK. A relative target then
    resolves from the archive's directory rather than the original's, so the archive is dangling:
    newest_retired() skips it because it is not a file, and a run interrupted after the retire
    cannot rebuild the destination, the store or the journal from it. Dereferencing instead would
    move or copy a file this command was never pointed at -- the target can live in somebody
    else's directory entirely -- so the layout is named and refused rather than guessed at.
    """
    if not Path(path).is_symlink():
        return None
    try:
        target = os.readlink(str(path))
    except OSError as error:
        target = type(error).__name__ + ": " + str(error)
    return (str(path) + " is a symlink to " + str(target) + ", and these documents are archived by"
            " moving them. Moving a link archives the link and not the document, and a relative"
            " target then resolves from the archive's directory, so the recovery would find no"
            " document there. Replace the link with the file it names, or move it aside by hand,"
            " and rerun")


def _answer(step, outcome, detail, **extra):
    return {"step": step, "outcome": outcome, "detail": detail, **extra}


def _executable(path):
    return bool(path) and Path(path).is_file() and os.access(str(path), os.X_OK)


def _retired_record(host):
    """The most recently retired bridge record that still reads as one, or None."""
    home = Path(host["codexHome"])
    stem = bridgerecord.RECORD_NAME + ".superseded-"
    found = sorted((p for p in home.glob(stem + "*") if p.is_file()),
                   key=lambda path: inventory.archive_order(path, stem))
    for candidate in reversed(found):
        document, outcome, _detail = bridgerecord.read(candidate)
        if document is not None:
            return document
    return None

def bridge_command(host):
    """The bridge the plugin record will name: what the host already used, or the pointer path."""
    record = (host["mcp"].get("record") or {})
    named = record.get("bridgeExecutable")
    if named and bridgerecord.owner_of(record) == bridgerecord.OWNER_USER:
        return named
    registration = host["mcp"].get("registration") or {}
    if registration.get("command"):
        return registration["command"]
    if named and bridgerecord.owner_of(record) == bridgerecord.OWNER_PLUGIN:
        # A host whose bridge surface was already moved on its own, with an executable somebody
        # chose: register-mcp takes --owner plugin with --bridge-command, so this is a supported
        # partial state. Falling through to the destination's default fabricated a path the host
        # had already answered, and preflight then compared the live record with the invention
        # and refused, leaving the hook and the skill links unable to follow.
        return named
    retired = _retired_record(host)
    if retired and retired.get("bridgeExecutable"):
        return retired["bridgeExecutable"]
    destination = host.get("destination")
    return str(Path(destination) / "current" / "bin" / "codex-thread-bridge") \
        if destination else None


def adapter_paths(host):
    destination = host.get("destination")
    if not destination:
        return None, None
    base = Path(destination) / "current" / "bin"
    return str(base / inventory.INTERPRETER_SCRIPT), str(base / inventory.ADAPTER_SCRIPT)


def relay_command(host):
    """The relay the settings will record, under the same pointer the adapter is recorded under.

    Probed like the rest because the adapter does not answer a Stop by itself: it runs this as a
    subprocess for the guard decision. With it missing, every Stop reaches a working adapter and
    releases with guard_unreachable, which is a host that looks installed and judges nothing.
    """
    destination = host.get("destination")
    return str(Path(destination) / "current" / "bin" / inventory.RELAY_SCRIPT) \
        if destination else None


def mcp_refusals(mcp):
    """Every reason the bridge surface cannot be transitioned, as a list.

    A function rather than a stretch of preflight because it is asked twice: once on the reading
    preflight decided from, and again on the reading taken inside the ownership lock. A surface
    that changed in between -- an aliased table registered while this was running, say -- would
    otherwise reach the steps having bypassed these checks entirely.
    """
    found = []
    record = mcp.get("record") or {}
    registration = mcp.get("registration") or {}
    if mcp.get("recordOwner") in bridgerecord.OWNERS and registration:
        # Either valid owner, because what makes this dangerous is that both surfaces are live,
        # not which one wrote the record. With a plugin-owned record disagreeing with a canonical
        # table, the retire answers already_done, the standdown removes the table, and the record
        # install refuses -- leaving new sessions started from a record that names another
        # executable than the one they were running a moment ago. A record no valid owner claims
        # is left out: nothing runs it, and it is retired rather than compared.
        # The table is what current sessions actually run and the record is what the plugin launcher
        # would run. Choosing between them silently would replace a working table with a record that
        # starts something else, so a disagreement is reported rather than resolved here.
        divergent = [field for field, mine, theirs in (
            ("bridgeExecutable", record.get("bridgeExecutable"), registration.get("command")),
            ("args", list(record.get("args") or []), list(registration.get("args") or [])),
            ("serverName", record.get("serverName"), inventory.SERVER_NAME))
            if mine != theirs]
        if divergent:
            found.append("the bridge record at " + mcp["recordPath"] + " and the "
                            + inventory.SERVER_NAME + " table in " + mcp["configPath"]
                            + " disagree about " + ", ".join(divergent)
                            + " (record " + repr(record.get("bridgeExecutable")) + " "
                            + repr(record.get("args") or []) + ", table "
                            + repr(registration.get("command")) + " "
                            + repr(registration.get("args") or []) + "). This command does not"
                            " choose between them: settle which one this host runs first")
    named = (record or {}).get("bridgeExecutable") or registration.get("command")
    if named and not os.path.isabs(str(named)):
        # A user-owned record and a configuration entry may both carry a relative command, and a
        # plugin-owned record may not: the packaged launcher runs from the installed package
        # directory, so a relative command resolves inside the version cache. Discovered at the
        # write, this refused with the settings, the record and the table already retired.
        found.append("the bridge is registered as " + repr(str(named)) + ", which is relative."
                     " A plugin-owned record has to name an absolute path, because the packaged"
                     " launcher runs from the installed package directory. Re-register it with an"
                     " absolute command first")
    for alias in mcp.get("aliases") or []:
        found.append("the table [mcp_servers." + str(alias["name"]) + "] in "
                        + mcp["configPath"] + " starts the same bridge under another name ("
                        + str(alias["command"]) + "). Leaving it while the plugin declares its own"
                        " would start two bridges, and renaming somebody's server is not this"
                        " command's to do: remove or rename that table first")
    if mcp["table"] == reading.PRESENT and not mcp["tableProven"]:
        found.append("the " + inventory.SERVER_NAME + " table in " + mcp["configPath"]
                        + " is not the block this repository renders for the registration it"
                          " holds, so it is somebody's own edit and is left in place"
                        + (": " + str(mcp["detail"]) if mcp.get("detail") else ""))
    if mcp["recordOutcome"] not in (None, bridgerecord.ABSENT):
        found.append("the bridge record at " + mcp["recordPath"] + " could not be acted on ("
                        + str(mcp["recordOutcome"]) + ")")
    return found


def registered_settings(host):
    """The document the registered hook actually reads, and where it came from.

    A manual install can name a custom path permanently while a valid document also sits at the
    fixed path. Building the plugin-owned settings from the fixed one silently replaces the marker
    root, the database and the journal the registration was using, and reports success doing it.
    What the registration names wins.
    """
    registered = host.get("registered") or {}
    if registered.get("document") is not None or registered.get("conflict"):
        return registered.get("document"), registered.get("from")
    document = host["settings"].get("document")
    if document is not None:
        return document, "the settings at " + str(host["settings"]["path"])
    return None, None


def payload_complaints(repo_root, cache_version):
    """Whether what is installed passes this repository's own payload contract, and what was run.

    One function, two callers: preflight and the recheck in front of every removal. The version
    cache is replaced wholesale on every install, so a replacement landing after preflight can
    leave a single version that is malformed -- one missing wiring/mcp.json, say -- and a check
    that lives in only one of the two places passes exactly the removal it exists to stop.
    Written once rather than twice because two copies of a contract do not stay in agreement.
    """
    argv = [sys.executable, str(Path(repo_root) / "scripts" / "ci" / "plugin.py"),
            "--payload", str(cache_version)]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=300)
    except (OSError, subprocess.SubprocessError) as error:
        return (["the installed payload could not be validated: "
                 + type(error).__name__ + ": " + str(error)],
                {"payloadCheck": argv, "exitCode": None})
    if done.returncode != 0:
        return (["the installed payload at " + str(cache_version) + " did not pass"
                 " scripts/ci/plugin.py --payload, so what is installed is not a package this"
                 " transition can rely on: " + (done.stdout + done.stderr).strip()[:400]],
                {"payloadCheck": argv, "exitCode": done.returncode})
    return (declaration_complaints(repo_root, cache_version),
            {"payloadCheck": argv, "exitCode": done.returncode})


# Python options that do not consume the word after them. Anything outside this set -- -c and -m
# most of all, which make the next word source text or a module name rather than a file to run --
# means this command's shape is not one whose executed script can be read positionally, and a
# declaration whose shape cannot be read is not one this transition may rely on.
SAFE_INTERPRETER_FLAGS = ("-u", "-E", "-s", "-S", "-B", "-I", "-O", "-OO", "-q", "-b", "-bb", "-d")
# The names a Python executable actually has. "starts with python3" also accepts
# python3-does-not-exist, which is not a Python and need not even be on the host: a declaration
# naming one passes every structural check and starts nothing.
INTERPRETER_NAMES = re.compile(r"^python(3(\.\d+)?)?$")


def _relative(word):
    """A declared path as the package's own relative one, whichever way it was spelled."""
    text = str(word).strip("\"'")
    for prefix in ("${PLUGIN_ROOT}/", "$PLUGIN_ROOT/", "./"):
        if text.startswith(prefix):
            text = text[len(prefix):]
    return text


def _script(words):
    """The script a declared command will actually execute, or None.

    Positional, because that is how execution works: an interpreter runs its first non-option
    argument and nothing else. Collecting every token that ends in .py accepted a command that
    merely mentions the launcher -- true crw_stop_hook.py, or an argument list whose first entry
    is a different script -- as though it ran it.
    """
    if not words:
        return None
    program = Path(str(words[0]).strip("\"'")).name
    if not INTERPRETER_NAMES.fullmatch(program):
        # Something other than a Python starts this. What it does with a file name that follows
        # is its own business, and it is not this launcher being declared.
        # fullmatch, because $ also matches before a trailing newline and "python3\n" is not the
        # name of anything Codex can execute.
        return None
    for word in words[1:]:
        text = str(word).strip("\"'")
        if text in SAFE_INTERPRETER_FLAGS:
            continue
        if text.startswith("-"):
            # -c takes source text and -m takes a module name, so the path that follows either is
            # not a file Python runs. Every other unrecognised option could do the same, and
            # guessing which is exactly the guess this function exists to stop making.
            return None
        return _relative(text)
    return None


def _shape(words):
    """The whole thing a declaration runs, interpreter and script together, or None.

    Compared as a pair because half of it is not a launcher. A versioned name this checkout does
    not ship -- python3.999999 -- is a valid spelling of a Python that need not exist on the host,
    and the only thing that makes a cached declaration the replacement is that it is the same
    declaration this checkout ships.
    """
    script = _script(words)
    if script is None:
        return None
    return Path(str(words[0]).strip("\"'")).name + " " + script


def _plugin_checker(repo_root):
    """This repository's own packaging check, loaded as a module for its manifest rules."""
    path = Path(repo_root) / "scripts" / "ci" / "plugin.py"
    spec = importlib.util.spec_from_file_location("crw_ci_plugin", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _declared(root, repo_root):
    """The hook and server launchers the MANIFEST at this root declares, and what could not be read.

    Following the manifest is the whole point: Codex loads the documents it names and ignores
    every other file in the package, so reading fixed paths let a cache whose manifest declares
    other documents satisfy this comparison with stale files nothing ever loads.
    """
    events, servers, unread = {}, {}, []
    manifest_path = Path(root) / ".codex-plugin" / "plugin.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as error:
        return events, servers, [str(manifest_path) + " could not be read ("
                                 + type(error).__name__ + ": " + str(error) + ")"]
    try:
        hook_paths = _plugin_checker(repo_root).declared_hooks(manifest)
    except Exception as error:  # noqa: BLE001 - a manifest this cannot read declares nothing
        return events, servers, [str(manifest_path) + " does not declare readable hooks ("
                                 + type(error).__name__ + ": " + str(error) + ")"]
    for relative in hook_paths:
        path = Path(root) / _relative(relative)
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as error:
            unread.append(str(path) + " is declared and could not be read ("
                          + type(error).__name__ + ": " + str(error) + ")")
            continue
        for event, groups in ((document or {}).get("hooks") or {}).items():
            for group in groups or []:
                for hook in (group or {}).get("hooks") or []:
                    try:
                        words = shlex.split(str((hook or {}).get("command") or ""))
                    except ValueError:
                        words = []
                    shape = _shape(words)
                    # The matcher and the timeout travel with the command. A declaration carrying
                    # the right launcher under a restrictive matcher fires on some turns and not
                    # others, and one carrying a second of timeout is killed before the adapter's
                    # own budget can answer -- both are replacements that do not replace, and
                    # both pass a comparison of command strings.
                    #
                    # A command whose shape cannot be read is COUNTED rather than dropped. Skipped
                    # ones were invisible to the comparison while still being declarations Codex
                    # runs: env python3 <launcher> starts the same launcher a second time, and a
                    # comparison that cannot see it reports the surface as matching.
                    events.setdefault(event, []).append(
                        (shape + " matcher=" + repr((group or {}).get("matcher"))
                         + " timeout=" + repr((hook or {}).get("timeout"))) if shape else
                        ("a command this cannot read: "
                         + repr(str((hook or {}).get("command") or ""))))
    named = manifest.get("mcpServers")
    if isinstance(named, str) and named.strip():
        path = Path(root) / _relative(named)
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as error:
            unread.append(str(path) + " is declared and could not be read ("
                          + type(error).__name__ + ": " + str(error) + ")")
            document = {}
        for name, entry in ((document or {}).get("mcpServers") or {}).items():
            words = [str((entry or {}).get("command") or "")]
            words += [str(word) for word in ((entry or {}).get("args") or [])]
            shape = _shape(words)
            servers[name] = shape or ("a command this cannot read: " + repr(" ".join(words)))
    return events, servers, unread


def declaration_complaints(repo_root, cache_version):
    """Whether the installed package declares the surfaces this transition is handing over.

    The payload contract answers whether the package is well formed: its hook check accepts any
    nonempty event and command, and its server check any nonempty name. Well formed is not the
    question here. What is about to be removed is a working Stop hook and a working bridge
    registration, and the only thing that makes their removal safe is that the installed package
    declares the launchers this repository ships. So the cached declarations are compared with
    this checkout's own, by event, by server name, and by which program each one runs.
    """
    ours_events, ours_servers, ours_unread = _declared(Path(repo_root) / "plugins" / "crw",
                                                       repo_root)
    cached_events, cached_servers, unread = _declared(cache_version, repo_root)
    found = list(unread)
    if ours_unread:
        # This checkout's own package is the thing being compared against. If it cannot be read
        # here, the comparison proves nothing and must not pass by being empty.
        found.append("this checkout's plugin package could not be read, so what the installed one"
                     " declares was compared with nothing: " + "; ".join(ours_unread))
    for event, wanted in sorted(ours_events.items()):
        # Counted, not merely contained. A cached document declaring the launcher twice answers a
        # subset test and fires two adapters on every Stop, which is the duplicate execution this
        # whole command exists to end.
        got = cached_events.get(event) or []
        if sorted(got) != sorted(wanted):
            found.append("the installed package declares " + str(event) + " hooks running "
                         + (", ".join(sorted(got)) if got else "nothing this checkout ships")
                         + ", and this checkout declares " + ", ".join(sorted(wanted))
                         + ". The replacement has to be the same surface, once each: anything"
                         " else either leaves the Stop unanswered or answers it twice")
    if cached_servers != ours_servers:
        # The whole map, not each name this checkout happens to use. A cached file carrying the
        # expected entry PLUS a second name running the same launcher passes every per-name test
        # and loads two bridge servers, which is the one-owner handoff defeated by addition.
        found.append("the installed package declares the servers "
                     + (", ".join(name + " -> " + shape
                                  for name, shape in sorted(cached_servers.items()))
                        if cached_servers else "nothing this checkout ships")
                     + ", and this checkout declares "
                     + ", ".join(name + " -> " + shape
                                 for name, shape in sorted(ours_servers.items()))
                     + ". The replacement has to be that same map: another entry starting the"
                     " same launcher is a second bridge, and a different one is not this bridge")
    return found


def runtime_complaints(host):
    """Whether the runtime this would record can still run, as a list.

    One function, two callers, for the reason the payload contract has one: preflight establishes
    this once and every destructive step runs afterwards. A version replacement landing in between
    takes the adapter out from under the pointer, and the settings written after it would name a
    program that is not there -- a declared hook that releases every Stop in silence while the run
    reported success. The executable and interpreter probes are live; the pointer reading is the
    caller's and may be older, which costs nothing here because a pointer that stopped resolving
    makes the adapter under it fail its own probe.
    """
    if not host.get("destination"):
        return ["no install destination was named or derivable from the recorded relayExecutable,"
                " so no adapter path could be recorded"]
    point = host["pointer"]
    if point.get("state") != "LINK" or not point.get("targetDirectory"):
        return ["the pointer at " + str(point.get("pointer")) + " is " + str(point.get("state"))
                + " (" + str(point.get("detail")) + "). A recorded adapter path has to resolve,"
                " and a dangling pointer still reads as a link"]
    found = []
    interpreter, adapter = adapter_paths(host)
    for label, path in (("the adapter", adapter), ("its interpreter", interpreter),
                        ("the bridge", bridge_command(host)),
                        ("the relay", relay_command(host))):
        if not _executable(path):
            found.append(label + " at " + str(path) + " is not an executable file, so recording it"
                         " would name something that cannot run")
    if _executable(interpreter):
        try:
            # Executable is not the question. /bin/true is executable, exits 0, and would be
            # recorded happily; every Stop would then run it, reach no adapter, and write no
            # journal entry while the install reported success. The installer asks the candidate
            # to be a Python before registering it as one, and so does this.
            completion.interpreter_for(interpreter, run=True)
        except ValueError as error:
            found.append("the interpreter at " + str(interpreter) + " did not answer as a Python"
                         " this adapter can run: " + str(error))
    return found


def preflight(host, options):
    """Every reason not to start, collected before anything is touched.

    Ordered by what it protects: first that the plugin can actually serve what is about to be
    removed, then that the recorded runtime exists, then that this tool owns what it would change,
    then that no work is in flight.
    """
    refusals = []
    notes = []
    plugin = host["plugin"]

    if plugin["configEntry"] != reading.PRESENT:
        refusals.append("the plugin is not registered in " + str(inventory.config_path(
            host["codexHome"])) + " (" + str(plugin["configEntry"]) + "), so removing the manual"
            " install would leave this host with no CRW skills, no hook and no bridge")
    if plugin.get("enabled") is not True:
        # Not "is False". An entry with no enabled key, or one carrying something that is not a
        # boolean, leaves whether Codex loads this plugin unestablished, and the whole order here
        # rests on the replacement being able to serve what is about to be removed. Unknown is
        # answered as unknown rather than as yes.
        refusals.append(
            "the plugin entry " + str(plugin.get("entryKey")) + " is disabled, so its skills,"
            " hook and server would not load" if plugin.get("enabled") is False else
            "the plugin entry " + str(plugin.get("entryKey")) + " does not record enabled = true"
            " (" + repr(plugin.get("enabled")) + "), so whether Codex loads its skills, hook and"
            " server was not established")
    if not plugin.get("cacheVersion"):
        # The reader's own reason travels with the refusal. Without it, a host carrying two cached
        # versions is reported as a host carrying none, which sends the operator to the wrong repair.
        refusals.append("no single installed plugin version could be named under "
                        + str(Path(host["codexHome"]) / "plugins" / "cache")
                        + (": " + str(plugin["detail"]) if plugin.get("detail") else ""))
    else:
        # The repository already owns a payload contract. A file census is not it: an empty hook
        # document and an empty mcp.json satisfy existence and leave no hook and no bridge.
        complaints, note = payload_complaints(host["repoRoot"], plugin["cacheVersion"])
        notes.append(note)
        refusals.extend(complaints)
        linked = {Path(item["path"]).name for item in host["skills"]["crwOwned"]}
        missing = sorted(linked - set(plugin.get("skills") or []))
        if missing:
            refusals.append("the installed package does not carry " + ", ".join(missing)
                            + ", which the links being removed provide")

    if not options.get("accept_hook_trust_gap"):
        # Always, not only when no key was found. A trust key records a hash for the hook as it
        # stood when trust was given, and nothing here can compute the hash Codex compares it
        # against, so a stale or fabricated record is indistinguishable from a current one. An
        # untrusted declared hook fires zero times, so getting this wrong turns the stated window
        # into a permanent absence of any completion hook. The operator acknowledges it.
        refusals.append(str(plugin.get("trustNote")) + ". Whether the plugin's declared hook will"
                        " actually fire cannot be established from here, and an untrusted declared"
                        " hook fires zero times, so removing a working registration now may leave"
                        " no completion hook at all. Trust the hook and confirm it fires, then"
                        " pass --accept-hook-trust-gap")

    refusals.extend(runtime_complaints(host))

    # Nothing to carry forward is not the same as nothing to do. The plugin-owned settings are
    # built from the document this host is using, and with no live document, none named by a
    # registration and no archive left, the marker root, the store and the journal cannot be
    # established at all. The standdown would still remove the registration, and the install
    # would then refuse with the hook already gone.
    carried, _source = registered_settings(host)
    if carried is None:
        refusals.append("no settings document could be found to carry forward: the registration"
                        " names none that can be read, the fixed path holds none and no archived"
                        " one remains, so the marker root, the store and the journal this host"
                        " uses cannot be established and the plugin settings cannot be built"
                        " from anything")

    # The comparison the record writer will make, made before anything is removed. A plugin-owned
    # record is never retired -- it already names the plugin -- so the install compares it on
    # identity and refuses when it differs, and the table is removed before that write. Reaching
    # it means stopping with no table and a record this cannot replace, and no rerun gets further.
    # Absent and empty arguments are the same registration to every reader here and different ones
    # to that writer, which is why this asks the writer rather than asking again in its own words.
    projected = mcp_record_install(host, options, apply=False)
    if projected["outcome"] == REFUSED:
        refusals.append("the bridge record already installed is not the one this would write, and"
                        " the table is removed before the write: " + str(projected["detail"])
                        + ". Settle that record first")

    # Checked here, before the standdown, because the packaged launcher reads one fixed path and
    # ignores this override: with it set, the plugin-owned document would be written where no
    # launcher looks. Discovering that at the write means discovering it after the working
    # registration has been removed and both settings files moved aside, which leaves the host with
    # no completion hook and an explanation. A populated override also passes every other reading,
    # so nothing else here catches it.
    refusals.extend(completion.override_complaints(completion.OWNER_PLUGIN))

    if len(plugin.get("entryKeys") or []) > 1:
        refusals.append("this host registers the plugin from more than one marketplace ("
                        + ", ".join(plugin["entryKeys"]) + "). Every one of them loads, so"
                        " transitioning onto one leaves the others running beside it: settle which"
                        " installation this host keeps first")

    registered = host.get("registered") or {}
    fixed_document = host["settings"].get("document")
    carried = registered.get("document")
    if (fixed_document is not None and carried is not None
            and completion.owner_of(fixed_document) == completion.OWNER_PLUGIN
            and completion.owner_of(carried) == completion.OWNER_USER
            and any(fixed_document.get(field) != carried.get(field)
                    for field in inventory.OPERATIONAL)):
        # Both owners are live: the plugin already owns the fixed path while a proven registration
        # still reads a user-owned document that says something else. Reading only the fixed one
        # made the retire step answer ALREADY, so the custom document was never archived, the
        # registration was removed anyway, and the retry -- which can no longer find that
        # document -- settled on the plugin configuration and abandoned the manual store.
        refusals.append("the settings at " + str(host["settings"]["path"]) + " already name the"
                        " plugin while " + str(registered.get("from")) + " still names the "
                        + completion.OWNER_USER + " and says something else about "
                        + ", ".join(field for field in inventory.OPERATIONAL
                                    if fixed_document.get(field) != carried.get(field))
                        + ". Two owners are live here: settle which installation this host keeps")
    unreadable = (registered.get("conflict") or {}).get("unreadable") or []
    if unreadable:
        refusals.append("a registration names settings that could not be read, so what it is"
                        " running was not established and no other document stands in for it: "
                        + "; ".join(unreadable))
    conflict = (registered.get("conflict") or {}).get("disagree") or []
    if conflict:
        # Two registrations reading documents that disagree about where work is recorded and which
        # store it goes to are two installations. Taking the first by hook order removes both
        # registrations, archives both documents and configures one of them, and reports success.
        refusals.append("these registrations name settings that disagree about "
                        + ", ".join(inventory.OPERATIONAL) + ": " + ", ".join(conflict)
                        + ". This command does not choose which installation this host keeps")

    # The document registered_settings() will carry, because that is the one whose budget and
    # journal policy have to be expressible as a plugin-owned document. Validating the fixed file
    # while carrying the custom one let a faults_only policy or a 9-second budget through preflight
    # and refuse at the write, with the registration already removed.
    document, _source = registered_settings(host)
    budget = (document or {}).get("timeoutSeconds")
    if isinstance(budget, (int, float)) and not isinstance(budget, bool) \
            and budget > MAX_GUARD_SECONDS:
        # Checked here rather than at the write. The packaged launcher caps its own deadline at
        # that ceiling and has to outlast the adapter it runs, so these settings cannot become
        # plugin-owned -- and finding that out after the registration has been removed would leave
        # the host with no completion hook and a refusal.
        refusals.append("the settings record a guard budget of " + str(budget) + "s, and a"
                        " plugin-owned document has to stay at or under "
                        + str(MAX_GUARD_SECONDS) + "s: the packaged launcher waits the budget"
                        " plus " + str(LAUNCHER_MARGIN_SECONDS) + "s capped at "
                        + str(completion.LAUNCHER_CEILING_SECONDS) + "s, so anything above that"
                        " leaves it no margin and its deadline arrives while the adapter is still"
                        " recording. Lower it before transitioning")
    policy = (document or {}).get("journalPolicy")
    if policy and policy != completion.EVERY_INVOCATION:
        refusals.append("the settings record journalPolicy " + str(policy) + ", and the document"
                        " this command builds always records " + completion.EVERY_INVOCATION
                        + ", so transitioning would change what this host records without being"
                          " asked")
    shifted = host["hook"].get("later") or []
    if shifted and not options.get("accept_hook_renumbering"):
        # Asked here as well as in the lock, because the settings are archived before the
        # standdown: refusing only at the standdown would leave the registration in place with its
        # configuration already moved aside, which is a command that refused and still broke the
        # host.
        refusals.append("removing this adapter's registration shifts the index of "
                        + ", ".join(shifted) + ", and Codex recorded trust against those"
                        " positions. Pass --accept-hook-renumbering to do it knowingly")
    if host["settings"].get("destinationChanged"):
        refusals.append("the destination changed while this host was being read ("
                        + json.dumps(host["settings"]["destinationChanged"])
                        + "), so nothing was validated against the installation this document"
                          " names; rerun to decide against the host as it now stands")
    for entry in host["hook"]["entries"]:
        if not entry["proven"]:
            refusals.append("the registration " + entry["identity"] + " runs a program this"
                            " repository cannot prove is its own adapter (" + str(entry["why"])
                            + "), so it is left alone. Edit or remove it by hand: "
                            + entry["command"])
    for entry in host["hook"]["unrecognised"]:
        refusals.append("the registration " + entry["identity"] + " names the packaged adapter or"
                        " the destination and is not one this repository wrote, so this"
                        " transition will not decide what happens to it: " + entry["command"])
    if host["hook"]["reading"] is not None:
        refusals.append("the hook file could not be read, so whether this adapter is registered"
                        " was not established: " + json.dumps(host["hook"]["reading"])[:300])

    settings = host["settings"]
    if settings.get("retiredFrom") and host["hook"]["entries"]:
        # The live document is gone while a registration that runs our adapter is still there.
        # That is a host in the middle of somebody else's transition, or one whose settings were
        # retired underneath this run, and reading the retired file as though it were live would
        # act on a state nobody established.
        refusals.append("the live settings are absent and " + str(settings["retiredFrom"])
                        + " was read instead, while a registration of this adapter is still in the"
                          " hook file. Another run may be mid-transition: nothing was changed")
    if settings["outcome"] not in (None, completion.CONFIG_ABSENT):
        refusals.append("the settings at " + settings["path"] + " could not be acted on ("
                        + str(settings["outcome"]) + ": " + str(settings["detail"]) + ")")
    elif settings["document"] and settings["owner"] == completion.OWNER_PLUGIN \
            and host["hook"]["entries"]:
        notes.append({"alreadyPluginOwned": True,
                      "why": "the settings already name the plugin while a registration remains,"
                             " which is the double fire this transition removes"})

    mcp = host["mcp"]
    if mcp["table"] not in (reading.PRESENT, reading.ABSENT):
        # A configuration that could not be read is not a configuration with no table in it.
        # Read as absence it would let the plugin record be written while the file still
        # registers the bridge, which is two bridges. On the documented 3.10 floor this is
        # where the run stops, because the reader that answers this question needs tomllib.
        refusals.append("the Codex configuration at " + mcp["configPath"] + " could not be"
                        " read (" + str(mcp["table"]) + ": " + str(mcp["detail"]) + "), so"
                        " whether this host registers " + inventory.SERVER_NAME + " was not"
                        " established")
    refusals.extend(mcp_refusals(mcp))

    flight = host["inFlight"]
    # Reported, never a refusal. A marker entry is created once and outlives the work it recorded,
    # so refusing on its presence permanently blocks every host that has ever run a managed turn,
    # and the reading it rests on was never about liveness in the first place. What the operator
    # gets instead is the history, the store path, and a statement that whether a turn is running
    # now was not established here.
    notes.append({"work": {"markerHistory": flight.get("markerHistory"),
                           "storePath": flight.get("storePath"),
                           "storeExists": flight.get("storeExists"),
                           "liveness": flight.get("liveness"),
                           "why": flight.get("note")}})

    return _answer("preflight", REFUSED if refusals else SETTLED,
                   "; ".join(refusals) if refusals else "nothing blocks this transition",
                   refusals=refusals, notes=notes)


def hook_standdown(host, options, *, apply=False):
    """Remove the registration that runs our adapter, and nothing else in that file."""
    entries = host["hook"]["entries"]
    if not entries:
        return _answer("hook standdown", ALREADY, "no registration of this adapter is in the file")
    path = Path(host["hook"]["hookFile"])
    event = host["hook"]["event"]
    document = hooks.read(path)
    if not document.usable:
        return _answer("hook standdown", REFUSED, "the hook file could not be read",
                       reading=document.refusal())
    before = hooks.inventory(document.value)
    shifted = [entry["identity"] for entry in host["hook"]["later"]] \
        if host["hook"]["later"] and isinstance(host["hook"]["later"][0], dict) \
        else list(host["hook"]["later"])
    if shifted and not options.get("accept_hook_renumbering"):
        return _answer("hook standdown", REFUSED,
                       "removing " + entries[0]["identity"] + " shifts the index of "
                       + ", ".join(shifted) + ", and Codex recorded trust against those"
                       " positions, so they would need trusting again. Pass"
                       " --accept-hook-renumbering to do it knowingly",
                       shiftedIdentities=shifted)
    if not apply:
        return _answer("hook standdown", WOULD,
                       "would remove " + ", ".join(e["identity"] for e in entries),
                       identities=[e["identity"] for e in entries], shiftedIdentities=shifted)
    removed = []
    with hostrecord.Locked(path):
        again = hooks.read(path)
        if not again.usable or json.dumps(again.value, sort_keys=True) != json.dumps(
                document.value, sort_keys=True):
            return _answer("hook standdown", REFUSED,
                           "the hook file changed after it was read, so nothing was removed")
        groups = (again.value.get("hooks") or {}).get(event) or []
        # Ordered by the positions as NUMBERS and removed from the back, so removing one does not
        # shift the index of another still to be removed. Sorting the identity strings put :10:
        # before :2: and left a copy behind on any host with ten or more hooks in one event.
        def position(entry):
            parts = entry["identity"].split(":")
            return int(parts[-2]), int(parts[-1])

        # Re-derived from the document about to be written, never carried from the snapshot.
        # document and again are both reads taken AFTER any change, so they agree with each other
        # while the identities decided on are already stale, and popping a stale index removes
        # somebody else's hook and leaves ours registered.
        current = completion.adapter_entries(again.value, event)
        if sorted(item["command"] for item in current) != sorted(item["command"] for item in entries):
            return _answer("hook standdown", REFUSED,
                           "the registrations of this adapter changed after they were read, so"
                           " nothing was removed; rerun to decide against the file as it stands",
                           wanted=sorted(item["identity"] for item in entries),
                           found=sorted(item["identity"] for item in current))
        # The proof is re-run, not merely the command strings compared. A command string says which
        # file a registration runs; it says nothing about what is in that file. An adapter replaced
        # in the checkout after the snapshot leaves every string identical, so the multiset above
        # still matches and this would remove a registration whose target is no longer this
        # repository's adapter -- with its settings already archived by the step before it.
        reproved = inventory.read_hook(host["codexHome"], event,
                                       destination=host.get("destination"),
                                       repo_root=host["repoRoot"])
        if reproved["reading"] is not None:
            return _answer("hook standdown", REFUSED,
                           "the hook file could not be read again under the lock, so whether these"
                           " registrations still run this repository's adapter was not"
                           " established and nothing was removed", reading=reproved["reading"])
        unproven = [item["identity"] for item in reproved["entries"] if not item.get("proven")]
        if unproven:
            return _answer("hook standdown", REFUSED,
                           "the file " + ", ".join(unproven) + " runs is no longer this"
                           " repository's own adapter, so nothing was removed: the program"
                           " changed after it was proved, and removing the registration now would"
                           " take away somebody else's hook", identities=unproven)
        if reproved["unrecognised"]:
            # The same question preflight asks, asked here where it is still free to answer no.
            # This reading already has it, and leaving it to the recheck at the end meant every
            # manual surface was gone by the time anyone refused.
            return _answer("hook standdown", REFUSED,
                           "a registration naming the packaged adapter or the destination is in"
                           " the hook file ("
                           + ", ".join(item["identity"] for item in reproved["unrecognised"])
                           + ") and this transition did not write it, so nothing was removed:"
                           " taking ours away now would leave that one firing beside the plugin's"
                           " declaration on every " + str(event) + ". Decide what happens to it"
                           " by hand",
                           identities=[item["identity"] for item in reproved["unrecognised"]])
        # The consent question is asked again here, against the file being written. The list the
        # snapshot carried was computed before anything was locked, so a foreign hook that landed
        # in the same group since then would have its index moved, and its recorded trust detached,
        # without anyone having agreed to it.
        # Restricted to the event being modified: identities carry an event as well as two numbers,
        # shifted_identities compares only the numbers, and removing a Stop hook cannot renumber
        # another event's positions.
        shifted_now = inventory.shifted_identities(hooks.inventory(again.value, event), current)
        if shifted_now and not options.get("accept_hook_renumbering"):
            return _answer("hook standdown", REFUSED,
                           "removing " + ", ".join(item["identity"] for item in current)
                           + " shifts the index of " + ", ".join(shifted_now)
                           + ", and Codex recorded trust against those positions. This changed"
                           " after the file was first read, so it is asked again rather than"
                           " assumed: pass --accept-hook-renumbering to do it knowingly",
                           shiftedIdentities=shifted_now)
        for entry in sorted(current, key=position, reverse=True):
            matcher, index = position(entry)
            if matcher < len(groups) and index < len(groups[matcher].get("hooks") or []):
                groups[matcher]["hooks"].pop(index)
                removed.append(entry["identity"])
        # An emptied group is LEFT in place. Removing it would renumber every later matcher and
        # detach the trust recorded against those identities; an empty group renumbers nothing.
        hostrecord.atomic_write(path, json.dumps(again.value, indent=2) + "\n")
        back = hooks.read(path)
    if not back.usable:
        return _answer("hook standdown", REFUSED, "the file was written and could not be read"
                       " back", applied=True, wrote=True, removed=removed)
    after = {item["identity"]: item["trustedHash"] for item in hooks.inventory(back.value)}
    kept = [item for item in before if item["identity"] not in removed]
    preserved = all(any(other["trustedHash"] == item["trustedHash"] for other in
                        hooks.inventory(back.value)) for item in kept)
    return _answer("hook standdown", SETTLED, "removed " + ", ".join(removed),
                   applied=True, wrote=True, removed=removed,
                   otherHooksPreserved=preserved, remaining=sorted(after))


def settings_retire(host, options, *, apply=False):
    """Move the settings the removed registration named aside, never delete them."""
    # A registration's third argument is whatever was typed there. It is retired only when the file
    # it names actually reads as this hook's settings, because a canonical-looking command naming an
    # unrelated existing file would otherwise have that file moved aside.
    fixed = str(Path(host["codexHome"]) / completion.CONFIG_NAME)
    named = [entry.get("settings") for entry in host["hook"]["entries"] if entry.get("settings")]
    # The registered document is archived LAST. Every archive lands under one stem and the recovery
    # takes the newest, so ordering decides which document a rerun rebuilds from: archived first, an
    # unrelated file at the fixed path became the newest archive and silently supplied the marker
    # root, the database and the journal after an interruption.
    paths, watched = [], []
    for candidate in [host["settings"]["path"], fixed] + named:
        if not candidate or candidate in paths or candidate in watched:
            continue
        if not Path(candidate).exists():
            # Absent now, and still named by a registration or by the fixed path. Kept rather
            # than dropped: a supported installer creating it between this filter and the
            # standdown leaves settings nothing here looked at, its store and journal abandoned
            # while the run reports success. It is locked with the rest and looked at again
            # inside that lock.
            watched.append(candidate)
            continue
        if candidate != fixed:
            document, outcome, _detail, _found = completion.read_configuration(Path(candidate))
            if document is None:
                continue
        paths.append(candidate)
    # De-duplicated keeping the LAST occurrence, so a path that is both the fixed one and the one
    # the registration names is archived in the registered position rather than the earlier one.
    paths = [path for index, path in enumerate(paths) if path not in paths[index + 1:]]
    if not paths and not (apply and watched):
        # Nothing here to archive and nothing worth holding a lock over. With watched paths and
        # an apply, the run continues into the lock below: the interrupted state, where every
        # candidate is absent, is exactly when a supported installer is most likely to write one
        # back, and this step answering already_done sends the standdown over a registration
        # whose settings arrived a moment later.
        return _answer("settings retire", ALREADY, "no settings file is there to retire")
    # Asked before the dry run answers too, so an operator learns the layout is unsupported from
    # the run that changes nothing rather than from the one that was going to change everything.
    linked = [complaint for complaint in (symlink_complaint(candidate) for candidate in paths)
              if complaint]
    if linked:
        return _answer("settings retire", REFUSED, "; ".join(linked))
    known = ((host.get("registered") or {}).get("conflict") or {}).get("documents") or {}
    owners = []
    for candidate in paths:
        document = known.get(candidate)
        if document is None:
            document, _outcome, _detail, _found = completion.read_configuration(Path(candidate))
        owners.append(completion.owner_of(document) if document else None)
    if owners and all(owner == completion.OWNER_PLUGIN for owner in owners):
        # Every document that would be retired already names the plugin. Deciding this from the
        # fixed path alone left a user-owned document a registration still reads unarchived.
        return _answer("settings retire", ALREADY,
                       "every settings document here already names the plugin as the owner")
    if not apply:
        if not paths:
            return _answer("settings retire", ALREADY, "no settings file is there to retire")
        return _answer("settings retire", WOULD, "would retire " + ", ".join(paths), paths=paths)
    home = Path(host["codexHome"])
    known = ((host.get("registered") or {}).get("conflict") or {}).get("documents") or {}
    if host["settings"].get("document") is not None:
        known.setdefault(str(host["settings"]["path"]), host["settings"]["document"])
    moved = []
    # Every lock first, then every proof, and only then the moves. Validating and moving in one
    # pass meant a later file failing its check left the earlier ones already archived, with the
    # standdown never reached: those registrations stay installed and release in silence, which is
    # a partial retirement reported as a refusal.
    with contextlib.ExitStack() as locks:
        for candidate in paths + watched:
            locks.enter_context(hostrecord.Locked(Path(candidate)))
        for candidate in watched:
            if Path(candidate).exists():
                return _answer("settings retire", REFUSED,
                               str(candidate) + " was absent when this host was read and is there"
                               " now, so a registration's settings appeared while this ran and"
                               " nothing here proved them. Nothing was archived; rerun to decide"
                               " against the settings as they now stand", retired=[])
        if not paths:
            # Nothing to archive after all, answered from inside the lock so the check above is
            # the one that decided it rather than a reading taken before anything was held.
            return _answer("settings retire", ALREADY, "no settings file is there to retire")
        for candidate in paths:
            # The snapshot proved what this file said; the lock only serialises the rename. A
            # writer that finished in between has settings this command never read, and archiving
            # them takes a live installation's configuration away.
            now, outcome, detail, _found = completion.read_configuration(Path(candidate))
            was = known.get(candidate)
            if was is None:
                # No prior reading means this file was not one of the documents the snapshot
                # proved. Absent then and valid now is a supported installer's settings written
                # while this ran, and treating a missing prior value as permission archived it
                # unread -- then replaced a live installation's marker root, store and journal
                # with the ones from the stale snapshot.
                return _answer("settings retire", REFUSED,
                               str(candidate) + " was not one of the documents this host was read"
                               " with, so it appeared after the reading and nothing here proved"
                               " it. Nothing was archived; rerun to decide against the settings"
                               " as they now stand", retired=[])
            if now != was:
                return _answer("settings retire", REFUSED,
                               str(candidate) + " changed after it was read (" + str(outcome or "")
                               + str(detail or "") + "), so nothing was archived; rerun to decide"
                               " against the settings as they now stand",
                               retired=[])
        for candidate in paths:
            try:
                moved.append({"from": candidate,
                              "to": retire(candidate, into=home, stem=completion.CONFIG_NAME)})
            except OSError as error:
                # A failure here leaves the earlier documents already archived, and this answer
                # is not SETTLED, so the rollback at the end of the run -- which acts on a
                # settled retire -- would never learn of them. They go back from here, under the
                # locks this block already holds.
                restored, kept = _restore_moved(moved, locked=True)
                return _answer("settings retire", REFUSED,
                               "archiving " + str(candidate) + " failed ("
                               + type(error).__name__ + ": " + str(error) + "), and the documents"
                               " already archived were put back"
                               + ("" if not kept else
                                  " except " + ", ".join(kept) + ", which are still archived and"
                                  " whose registrations have no settings to read until they are"
                                  " restored by hand")
                               + ".", retired=[], settingsRestored=restored,
                               settingsLeftArchived=kept)
    return _answer("settings retire", SETTLED, "retired " + ", ".join(p["from"] for p in moved),
                   applied=True, wrote=True, retired=moved, watched=watched)


def _newest_retired(host):
    """The most recently retired settings document, and where it came from.

    Retiring is what disable does, so this is the path back: the locations an operator is still
    using are in that file, and reading them is the difference between re-enabling an installation
    and quietly pointing it at a fresh empty store.
    """
    document, name = inventory.newest_retired(host["codexHome"])
    return document, ("the retired document " + name) if name else None

def settings_install(host, options, *, apply=False, previous=None):
    """Write the plugin-owned settings, carrying the operational locations forward.

    The marker root, the database and the journal come from the document being replaced. A
    transition that quietly relocated them would look like a success and lose the evidence.

    Two things this has to handle that the first draft did not, both found by running the
    contrasts rather than by reading. A dry run reaches here with the old settings still in place,
    so deciding against the file on disk would report DIFFERS and refuse a sequence that would
    have worked; the dry run projects the write that follows the retire step instead, and says so.
    And after a disable there is no live document at all, while the locations it carried are in the
    file that disable retired, so the newest retired document is read rather than refusing or,
    worse, silently relocating an operational database.
    """
    interpreter, adapter = adapter_paths(host)
    source = previous if previous is not None else host["settings"]["document"]
    # The label travels with the document rather than being guessed at here, so a receipt names
    # which file the operational locations actually came from.
    carried = (host.get("registered") or {}).get("from") or "the settings being replaced"
    if source is None:
        source, carried = _newest_retired(host)
    if source is None:
        return _answer("settings install", REFUSED,
                       "the settings being replaced were not read, so their marker root,"
                       " database and journal could not be carried forward, and no retired"
                       " document was found to read them from either")
    policy = source.get("journalPolicy") or completion.EVERY_INVOCATION
    if policy != completion.EVERY_INVOCATION:
        # configuration() writes every_invocation and takes no policy argument, and that function
        # belongs to another change. Carrying the value is not available, so the alternative to
        # refusing is silently turning a host's chosen journalling back on, which is a change
        # nobody asked for made invisibly.
        return _answer("settings install", REFUSED,
                       "the settings being replaced record journalPolicy " + str(policy)
                       + ", and the document this command builds always records "
                       + completion.EVERY_INVOCATION + ". Transitioning would change what this"
                       " host records without being asked, so it stops here")
    timeout = source.get("timeoutSeconds") or completion.DEFAULT_TIMEOUT_SECONDS
    if timeout > MAX_GUARD_SECONDS:
        return _answer("settings install", REFUSED,
                       "the previous guard budget is " + str(timeout) + "s, and a plugin-owned"
                       " document has to stay at or under " + str(MAX_GUARD_SECONDS) + "s: the"
                       " packaged launcher waits the budget plus "
                       + str(LAUNCHER_MARGIN_SECONDS) + "s capped at "
                       + str(completion.LAUNCHER_CEILING_SECONDS) + "s, so anything above that"
                       " leaves it no margin to outlast the adapter it runs")
    try:
        wanted = completion.configuration(
            destination=host["destination"], marker_root=source.get("markerRoot"),
            database=source.get("dbPath"), mode=source.get("mode") or completion.OBSERVE,
            timeout=timeout, journal_root=source.get("journalRoot"),
            codex_home=host["codexHome"], issue=source.get("installedBy"),
            isolation=source.get("isolationAssertedBy"), owner=completion.OWNER_PLUGIN,
            adapter_interpreter=interpreter, adapter_entry_point=adapter)
    except ValueError as error:
        return _answer("settings install", REFUSED, str(error))
    # The packaged launcher reads one path and ignores the settings override, deliberately, so the
    # plugin-owned document goes to that path and nowhere else. Honouring an override here would
    # write a document no launcher will ever open, and a Stop that cannot find its settings releases
    # in silence.
    path = Path(host["codexHome"]) / completion.CONFIG_NAME
    override = completion.override_complaints(completion.OWNER_PLUGIN)
    if override:
        return _answer("settings install", REFUSED, "; ".join(override))
    live = host["settings"]["document"]
    if not apply and (live is None or completion.owner_of(live) == completion.OWNER_USER):
        # Projected past the retire step deliberately, and ONLY past that step. Deciding against a
        # user-owned file still on disk would answer DIFFERS and refuse a sequence that settles once
        # step 2 has run; hiding a plugin-owned document that says something else would promise a
        # write that will not happen.
        return _answer("settings install", WOULD,
                       "would write these settings after the retire step; nothing was written",
                       configuration={"configuration": str(path), "wanted": wanted},
                       adapterEntryPoint=adapter, adapterInterpreter=interpreter,
                       carriedFrom=carried, projected=True)
    written = completion.write_configuration(path, wanted, apply=apply)
    outcome = written["outcome"]
    settled = SETTLED if outcome in (completion.CONFIG_CREATED,) else (
        ALREADY if outcome == completion.CONFIG_UNCHANGED else (
            WOULD if outcome == completion.CONFIG_WOULD_CREATE else REFUSED))
    return _answer("settings install", settled, written.get("detail"),
                   configuration=written, adapterEntryPoint=adapter,
                   adapterInterpreter=interpreter, carriedFrom=carried,
                   wrote=written.get("wrote"),
                   applied=written.get("applied"))


def mcp_record_retire(host, options, *, apply=False):
    """Retire the user-owned record, because owner is part of what makes a record the same one."""
    record = host["mcp"].get("record")
    path = Path(host["mcp"]["recordPath"])
    if record is None:
        return _answer("mcp record retire", ALREADY, "no bridge record is there")
    if bridgerecord.owner_of(record) == bridgerecord.OWNER_PLUGIN:
        return _answer("mcp record retire", ALREADY, "the record already names the plugin")
    linked = symlink_complaint(path)
    if linked:
        return _answer("mcp record retire", REFUSED, linked)
    if not apply:
        return _answer("mcp record retire", WOULD, "would retire " + str(path))
    moved = retire(path)
    return _answer("mcp record retire", SETTLED, "retired " + str(path), applied=True,
                   wrote=True, retired=[{"from": str(path), "to": moved}])


def mcp_table_standdown(host, options, *, apply=False):
    """Remove the table this repository rendered, and prove every other byte survived."""
    mcp = host["mcp"]
    if mcp["table"] != reading.PRESENT:
        return _answer("mcp table standdown", ALREADY, "no " + inventory.SERVER_NAME + " table")
    if not mcp["tableProven"]:
        return _answer("mcp table standdown", REFUSED,
                       "the table is not the block this repository renders, so it is left alone")
    path = Path(mcp["configPath"])
    block = (mcp.get("tableSpan") or mcp["renderedTable"]).strip()
    if not apply:
        return _answer("mcp table standdown", WOULD, "would remove the " + inventory.SERVER_NAME
                       + " table from " + str(path))
    with hostrecord.Locked(path):
        # The span is re-derived inside the lock rather than carried from the snapshot. Authorship
        # is proved by equality and equality is a property of the WHOLE table: a field appended to
        # it after the snapshot leaves the old span a substring of the file, so the containment
        # test below still says ours, and removing the old span deletes the header and leaves the
        # appended field attached to whatever table precedes it.
        again = inventory.read_mcp(host["codexHome"])
        if again["table"] != reading.PRESENT:
            return _answer("mcp table standdown", REFUSED,
                           "the " + inventory.SERVER_NAME + " table reads " + str(again["table"])
                           + " now, so the configuration changed after it was read and nothing"
                           " was removed")
        if not again["tableProven"]:
            return _answer("mcp table standdown", REFUSED,
                           "the table changed after it was read and is no longer the block this"
                           " repository renders, so it is left alone"
                           + (": " + str(again["detail"]) if again.get("detail") else ""))
        block = (again.get("tableSpan") or again["renderedTable"]).strip()
        text = reading.read_text(path, "the Codex configuration")
        if not text.usable or block not in text.value:
            return _answer("mcp table standdown", REFUSED,
                           "the configuration changed after it was read, so nothing was removed")
        before = text.value
        stripped = before.replace(block + "\n", "", 1)
        if stripped == before:
            stripped = before.replace(block, "", 1)
        # Everything outside the removed block, byte for byte. Checked rather than asserted.
        if stripped.replace("\n", "") != before.replace(block, "", 1).replace("\n", ""):
            return _answer("mcp table standdown", REFUSED,
                           "removing the block would have changed bytes outside it")
        hostrecord.atomic_write(path, stripped)
        back = reading.read_text(path, "the Codex configuration")
    view = codexconfig.scan(back.value) if back.usable else None
    present = view and codexconfig.registration_of(view, inventory.SERVER_NAME)[0]
    if present:
        return _answer("mcp table standdown", REFUSED, "the table is still registered after the"
                       " write", applied=True, wrote=True)
    return _answer("mcp table standdown", SETTLED, "removed the table and left every other byte",
                   applied=True, wrote=True,
                   otherTablesPreserved=bool(view and view.readable))


def mcp_record_install(host, options, *, apply=False):
    command = bridge_command(host)
    if not command:
        return _answer("mcp record install", REFUSED, "no bridge executable could be named")
    registration = host["mcp"].get("registration") or {}
    retired = _retired_record(host)
    # After the table is removed the registration is gone, so an interrupted run would rebuild the
    # record with no arguments. The retired record is where they survive.
    # "args" absent and "args" empty are different answers: falling back on an empty live list
    # restored historical arguments the current registration had deliberately dropped.
    if registration:
        arguments = list(registration.get("args") or [])
    elif retired is not None:
        arguments = list(retired.get("args") or [])
    else:
        arguments = []
    try:
        wanted = bridgerecord.document(command=command, arguments=arguments,
                                       name=inventory.SERVER_NAME, issue="CRW-115",
                                       owner=bridgerecord.OWNER_PLUGIN)
    except ValueError as error:
        return _answer("mcp record install", REFUSED, str(error))
    if not apply and host["mcp"]["recordOwner"] in (None, bridgerecord.OWNER_USER):
        # Projected past the retire step for the same reason the settings step is, and with the same
        # limit: a plugin-owned record that says something else is a refusal, not a projection.
        return _answer("mcp record install", WOULD,
                       "would write this record after the retire step; nothing was written",
                       record={"record": host["mcp"]["recordPath"], "wanted": wanted},
                       projected=True)
    written = bridgerecord.write(Path(host["mcp"]["recordPath"]), wanted, apply=apply)
    outcome = written["outcome"]
    settled = SETTLED if outcome == bridgerecord.CREATED else (
        ALREADY if outcome == bridgerecord.UNCHANGED else (
            WOULD if outcome == bridgerecord.WOULD_CREATE else REFUSED))
    return _answer("mcp record install", settled, written.get("detail"), record=written,
                   applied=written.get("applied"), wrote=written.get("wrote"))


def plugin_refusals(host):
    """Whether the plugin can still serve what is about to be removed.

    preflight reads this once, and the steps that remove things run afterwards. A plugin disabled
    or removed in between leaves a host whose manual surfaces are being taken away and whose
    replacement is no longer there, so the question is asked again before the last removal.
    """
    plugin = inventory.read_plugin(host["codexHome"])
    found = []
    if plugin["configEntry"] != reading.PRESENT:
        found.append("the plugin is no longer registered in "
                     + str(inventory.config_path(host["codexHome"])) + " ("
                     + str(plugin["configEntry"]) + ")")
    if plugin.get("enabled") is not True:
        found.append(
            "the plugin entry " + str(plugin.get("entryKey")) + " is disabled"
            if plugin.get("enabled") is False else
            "the plugin entry " + str(plugin.get("entryKey")) + " does not record enabled = true"
            " (" + repr(plugin.get("enabled")) + ")")
    # The runtime the settings about to be written will name. preflight probes it once and every
    # step here runs afterwards, so a version replacement that took the adapter away between them
    # would be recorded as if it were still there.
    found.extend(runtime_complaints(host))
    if not plugin.get("cacheVersion"):
        found.append("no single installed plugin version could be named"
                     + (": " + str(plugin["detail"]) if plugin.get("detail") else ""))
    else:
        # The same contract preflight applies, not a lighter census of it. The directories being
        # present says nothing about whether the package still declares the hook and the server:
        # a wholesale cache replacement landing mid-run leaves one version that may declare
        # neither, and every destructive step after it would pass a check that only counted
        # directories.
        found.extend(payload_complaints(host["repoRoot"], plugin["cacheVersion"])[0])
    if len(plugin.get("entryKeys") or []) > 1:
        # The same cardinality preflight refuses on. Asked again here because an entry installed
        # after the snapshot leaves the first one present and enabled, so every other check in
        # this function passes while two declarations load: the manual surfaces would be removed
        # into exactly the duplicate hook and duplicate server this transition exists to end.
        found.append("the plugin is now registered from more than one marketplace ("
                     + ", ".join(plugin["entryKeys"]) + "), and every one of them loads")
    linked = {Path(item["path"]).name for item in host["skills"]["crwOwned"]}
    missing = sorted(linked - set(plugin.get("skills") or []))
    if missing:
        found.append("the installed package no longer carries " + ", ".join(missing))
    return found


def _still_ours(item):
    """Whether this path is, right now, the CRW-owned link ownership was established on.

    One reader for two callers inside the step: the proof pass that decides whether anything may
    be removed at all, and the last look each path gets immediately before its own unlink. Both
    ask the same question of the same three things -- it is a symlink, it resolves into a CRW
    checkout carrying a SKILL.md, and it still resolves to the target ownership was decided on.
    """
    path = Path(item["path"])
    if not path.is_symlink():
        return False
    if inventory.checkout_of(path) is None:
        return False
    try:
        settled = path.resolve()
    except (OSError, RuntimeError):
        return False
    return (settled / "SKILL.md").is_file() and str(settled) == item.get("target")


def skill_unlink(host, options, *, apply=False):
    owned = host["skills"]["crwOwned"]
    if not apply:
        if not owned:
            return _answer("skill unlink", ALREADY, "no CRW-owned links are there")
        return _answer("skill unlink", WOULD, "would remove "
                       + ", ".join(item["path"] for item in owned),
                       paths=[item["path"] for item in owned])
    changed = plugin_refusals(host)
    if changed:
        return _answer("skill unlink", REFUSED, "; ".join(changed))
    # Both sets, because they answer different questions. The snapshot's links have to be VISITED
    # even when they stopped being ours, or a link replaced since then is passed over in silence
    # instead of reported. The directory is also re-inventoried, because it has no lock the way
    # the bridge record has one and had no recheck the way the hook file has one: a manual install
    # that landed a link after the snapshot would otherwise survive a run that reported success,
    # still exposing the installation this was removing.
    fresh = inventory.read_skill_links(host["codexHome"], host["repoRoot"])
    if fresh["unreadable"]:
        # An empty crwOwned means "none there" or "not read", and those are different hosts. Read
        # as the first, a timed-out installer check answers already_done while the links are still
        # in place and the run reports a manual install removed that is still offered.
        return _answer("skill unlink", REFUSED,
                       "the skill links could not be inventoried, so whether any CRW-owned link"
                       " is still there was not established and nothing was removed: "
                       + str(fresh["unreadable"]))
    known = {item["path"] for item in owned}
    candidates = list(owned) + [item for item in fresh["crwOwned"]
                                if item["path"] not in known]
    if not candidates:
        return _answer("skill unlink", ALREADY, "no CRW-owned links are there")
    # The replacement requirement, applied to the set actually being removed. plugin_refusals
    # derives it from the snapshot, so a link that arrived after it would be removed without the
    # installed package ever being asked whether it carries that skill -- and the link can be the
    # only copy of it.
    plugin = inventory.read_plugin(host["codexHome"])
    absent = sorted({Path(item["path"]).name for item in candidates}
                    - set(plugin.get("skills") or []))
    if absent:
        return _answer("skill unlink", REFUSED,
                       "the installed package does not carry " + ", ".join(absent)
                       + ", which the links being removed provide, so nothing was removed: taking"
                       " them away would leave this host with no copy of those skills",
                       paths=[item["path"] for item in candidates])
    removed, left = [], []
    for item in candidates:
        path = Path(item["path"])
        # Re-established here rather than trusted from the inventory: a link replaced since then is
        # somebody else's, and "is a symlink" is not the question ownership was decided on.
        if not _still_ours(item):
            left.append(str(path))
    if left:
        return _answer("skill unlink", REFUSED,
                       "these links are no longer the ones ownership was established on, so they"
                       " were left: " + ", ".join(left),
                       removed=removed, applied=False, wrote=False)
    # Every proof first, then every removal, the way the settings retire does it. Proving and
    # unlinking in one pass left the links before the mismatch already gone and the ones after it
    # in place, so a refusal reported a manual installation that was in fact half dismantled.
    for item in candidates:
        # Proved once more immediately before its own unlink. Nothing locks this directory, so a
        # writer can replace a path between the proof pass and this one, and unlinking then would
        # delete somebody else's registration at a name we had proved was ours. The re-check does
        # not close that window -- only a lock both writers take could -- but it narrows it to the
        # gap between this read and the call below, and it stops rather than carrying on.
        if not _still_ours(item):
            return _answer("skill unlink", REFUSED,
                           str(item["path"]) + " was replaced while these links were being"
                           " removed, so it was left where it is and the removals stopped there."
                           " Rerun this transition to decide against the directory as it stands",
                           removed=removed, applied=bool(removed), wrote=bool(removed),
                           paths=[item["path"]])
        Path(item["path"]).unlink()
        removed.append(item["path"])
    # Read once more, for the same reason the hook file is: what is in reach is not preventing a
    # link that arrives during the removals, but refusing to report success over one.
    back = inventory.read_skill_links(host["codexHome"], host["repoRoot"])
    if back["unreadable"]:
        return _answer("skill unlink", REFUSED,
                       "the links were removed and the directory could not be inventoried again,"
                       " so whether one arrived meanwhile was not established: "
                       + str(back["unreadable"]),
                       removed=removed, applied=bool(removed), wrote=bool(removed))
    again = back["crwOwned"]
    if again:
        return _answer("skill unlink", REFUSED,
                       "a CRW-owned link is in the destination again ("
                       + ", ".join(item["path"] for item in again) + "). Another install added it"
                       " while these were being removed, so the manual installation is still"
                       " exposed through it. Rerun this transition to remove it",
                       removed=removed, applied=bool(removed), wrote=bool(removed),
                       paths=[item["path"] for item in again])
    return _answer("skill unlink", SETTLED, "removed " + ", ".join(removed), applied=True,
                   wrote=True, removed=removed,
                   foreignLeft=[item["path"] for item in fresh["foreign"]])


# Retire before standdown. The custom settings path is recorded only in the hook command, so
# removing the command first and stopping there leaves a file the next run cannot rediscover and a
# host with no completion hook. Retiring first costs a window in which the old registration runs
# against absent settings -- it releases in silence and records nothing -- and no window in which
# two adapters run, because the plugin-owned settings are still not installed.
ORDER = (("settings retire", settings_retire), ("hook standdown", hook_standdown),
         ("settings install", settings_install), ("mcp record retire", mcp_record_retire),
         ("mcp table standdown", mcp_table_standdown),
         ("mcp record install", mcp_record_install), ("skill unlink", skill_unlink))

# The MCP surface is decided and written under ONE lock, the same one register-mcp takes, because a
# concurrent user-owned registration landing between the retire and the table removal would put the
# record back, refuse the plugin record, and leave the host with no bridge.
MCP_STEPS = ("mcp record retire", "mcp table standdown", "mcp record install")

# The steps that take something away. preflight reads the replacement once and these run afterwards,
# so the plugin's readiness is asked again before each of them: a plugin disabled or removed while
# this was running leaves a host losing its manual surfaces with nothing to serve them.
DESTRUCTIVE = ("settings retire", "hook standdown", "mcp record retire", "mcp table standdown",
               "skill unlink")


def hook_recheck(host):
    """Whether a registration of this adapter is in the hook file after the sequence ran.

    Hook ownership spans two artifacts. The installer decides it from the settings and then locks
    hooks.json separately, so a concurrent user-owned install that made its decision before these
    settings became plugin-owned can still append after the standdown. Both registrations would
    then run on every Stop.

    Closing that race needs the other side to decide ownership under the lock it writes in, and
    that side is not this command's to change. What is in reach is refusing to report success over
    it: the file is read again at the end, and a registration that reappeared is named.
    """
    again = inventory.read_hook(host["codexHome"], host["hook"]["event"],
                               destination=host.get("destination"), repo_root=host["repoRoot"])
    if again["reading"] is not None:
        return _answer("hook recheck", REFUSED,
                       "the hook file could not be read back, so whether a registration reappeared"
                       " was not established", reading=again["reading"])
    if again["entries"]:
        return _answer("hook recheck", REFUSED,
                       "a registration of this adapter is in the hook file again ("
                       + ", ".join(item["identity"] for item in again["entries"])
                       + "). Another install appended after the standdown, and both it and the"
                       " plugin declaration would run on every " + str(host["hook"]["event"])
                       + ". Rerun this transition to remove it",
                       identities=[item["identity"] for item in again["entries"]])
    if again["unrecognised"]:
        # The other half of the same question. preflight refuses on an entry naming the packaged
        # adapter or the destination, and one appended while this ran passed unseen: this read
        # looked only at the registrations this repository writes, so the run reported success
        # with that entry firing beside the plugin's declaration on every Stop.
        return _answer("hook recheck", REFUSED,
                       "a registration naming the packaged adapter or the destination is in the"
                       " hook file ("
                       + ", ".join(item["identity"] for item in again["unrecognised"])
                       + "), and this transition did not write it. It and the plugin declaration"
                       " would both run on every " + str(host["hook"]["event"])
                       + ". Decide what happens to it by hand",
                       identities=[item["identity"] for item in again["unrecognised"]])
    return _answer("hook recheck", SETTLED,
                   "no registration of this adapter is in the hook file")


def _restore_moved(moved, *, locked=False):
    """Put archived documents back at the paths they came from.

    Never over a file that appeared at the original path meanwhile: that one belongs to whoever
    wrote it, and the archive stays where the recovery can still find it.

    The locked flag is for the caller that already holds these paths' locks -- the retire undoing
    its own half-finished loop -- because taking them again in the same process would wait out
    the timeout and answer Busy about a lock this very run is holding.
    """
    restored, kept = [], []
    for item in moved:
        origin, archive = Path(item["from"]), Path(item["to"])
        try:
            # The same lock the writer of that path takes, held across the question and the
            # answer. Asking whether the path is free and then writing it without the lock is a
            # race with a supported installer, and losing it means replacing that installation's
            # configuration with a document from before it existed.
            with contextlib.nullcontext() if locked else hostrecord.Locked(origin):
                # lexists, not exists: exists() follows the link, so a dangling symlink placed at
                # this path reads as nothing being there and os.replace would delete it. Anything
                # at all at the path belongs to whoever put it there.
                if os.path.lexists(str(origin)) or not archive.is_file():
                    kept.append(str(archive))
                    continue
                try:
                    os.replace(str(archive), str(origin))
                except OSError as error:
                    if error.errno != errno.EXDEV:
                        raise
                    # The retire crosses filesystems by copying, and so does the way back. Without
                    # this the original path stays absent on exactly the layout retire was taught
                    # to handle, and the registration still installed reads nothing.
                    #
                    # Copied beside the path and moved onto it, never written into it directly: a
                    # copy that fails halfway -- the destination filling up is the ordinary way --
                    # would otherwise leave a partial document at the live path, which every later
                    # attempt then reads as occupied and skips while the hook reads it as its
                    # settings.
                    handle, temporary = tempfile.mkstemp(dir=str(origin.parent),
                                                         prefix=".crw-restore-")
                    os.close(handle)
                    try:
                        shutil.copy2(str(archive), temporary)
                        os.replace(temporary, str(origin))
                    except BaseException:
                        Path(temporary).unlink(missing_ok=True)
                        raise
                    os.unlink(str(archive))
                restored.append(str(origin))
        except (OSError, hostrecord.Busy) as error:
            kept.append(str(archive) + " (" + type(error).__name__ + ": " + str(error) + ")")
    return restored, kept


def _restore_retired(results):
    """Put the settings back when the standdown they were retired for refused.

    The retire goes first on purpose: a custom settings path lives only in the hook command, so
    removing the command first leaves a file the next run cannot rediscover. The cost is this
    case -- a hook file that changed in between makes the standdown ask a consent question again
    and refuse, with the settings already archived, so the still-registered adapter releases in
    silence and the next run needs a flag the operator has not agreed to yet.
    """
    retire = next((item for item in results if item["step"] == "settings retire"), None)
    return _restore_moved((retire or {}).get("retired") or [])


def _recreated_settings(results):
    """Paths this run archived that something has written to again, with the owner found there.

    The retire releases each path's lock before the standdown takes the hook file's, and an
    identical registration lets a supported runtime_install.py hook --apply recreate the settings
    without touching hooks.json at all. Removing the registration after that leaves a user-owned
    document the plugin install refuses to overwrite and a packaged launcher that stands down
    because the owner is not the plugin: no completion hook at all, out of a run that refused.
    Presence is enough to stop, whoever owns it, because the path is not this run's any more.
    """
    retire = next((item for item in results if item["step"] == "settings retire"), None)
    found = []
    paths = [moved["from"] for moved in (retire or {}).get("retired") or []]
    # The watched paths too: a registration names them, they were absent when this run read the
    # host, and one appearing now is the same event as one coming back -- a document the install
    # will refuse to overwrite, behind a registration this step is about to remove.
    paths += [path for path in (retire or {}).get("watched") or [] if path not in paths]
    for candidate in paths:
        origin = Path(candidate)
        if not os.path.lexists(str(origin)):
            continue
        document, _outcome, _detail, _found = completion.read_configuration(origin)
        found.append(str(origin) + " (owner "
                     + str(completion.owner_of(document) if document else None) + ")")
    return found


def _rollback_if_unfinished(results):
    """Undo the retire when the standdown it was made for did not happen.

    One place rather than one per exit. The settings are retired FOR the standdown, so every way
    a run can end between them leaves the identical host: a registration still installed whose
    settings are archived, releasing every Stop in silence. The standdown refusing is only the
    first of those ways -- the plugin recheck in front of it can refuse, and the hook file's lock
    can be held by another run -- and a rollback written at one exit is a rollback missing from
    the others.
    """
    done = {item["step"]: item for item in results}
    retire = done.get("settings retire")
    if not retire or retire["outcome"] != SETTLED:
        return
    standdown = done.get("hook standdown")
    if standdown is not None and standdown["outcome"] in DONE:
        return
    if standdown is not None and standdown.get("wrote"):
        # The write landed and only the read-back failed, so the registration may already be gone.
        # Restoring a user-owned document then leaves the packaged launcher standing down on an
        # owner that is not the plugin while there is no manual registration left to serve the
        # Stop either: both hooks off, from a rollback meant to keep one. The archive stays where
        # the recovery looks for it and the receipt says why.
        standdown["settingsRestored"] = []
        standdown["settingsLeftArchived"] = [moved["to"] for moved in
                                             (retire.get("retired") or [])]
        standdown["detail"] = (str(standdown["detail"]) + ". The settings stay archived because"
                               " this step had already written the hook file: putting them back"
                               " while the registration may be gone would leave no completion"
                               " hook at all. Rerun to converge from where this stopped")
        return
    restored, kept = _restore_retired(results)
    # Reported on the answer that ended the run, which is the one an operator reads first.
    stopper = next((item for item in reversed(results)
                    if item["outcome"] not in (NOT_REACHED,)), retire)
    stopper["settingsRestored"] = restored
    stopper["settingsLeftArchived"] = kept
    if restored:
        stopper["detail"] = (str(stopper["detail"]) + ". The settings this run had already"
                             " archived were put back at " + ", ".join(restored) + ", so the"
                             " registration still there keeps working")


def transition(host, options, *, apply=False):
    """Run the steps in order, stopping at the first refusal."""
    results = [preflight(host, options)]
    if results[0]["outcome"] == REFUSED:
        results += [_answer(name, NOT_REACHED, "preflight refused") for name, _ in ORDER]
        return results
    previous, _carried = registered_settings(host)
    lock = None
    active = None
    try:
        for name, step in ORDER:
            active = name
            if name == MCP_STEPS[0] and apply:
                # Assigned only after it is held. Assigning first meant a Busy raised inside
                # __enter__ reached the finally below with an object whose __exit__ unlinks the
                # lock file by name -- another run's lock, removed by the run that failed to take
                # it, letting a third in while the first was still writing.
                taking = hostrecord.Locked(bridgerecord.ownership_lock_path(host["codexHome"]))
                taking.__enter__()
                lock = taking
                # Re-read INSIDE the lock, because the snapshot was taken before it. Two concurrent
                # runs would otherwise both hold the user-owned record in memory, and the second
                # would retire the plugin record the first had just written, leaving new sessions
                # with no bridge. The decision is made about the state that will be written.
                host = {**host, "mcp": inventory.read_mcp(host["codexHome"])}
                changed = mcp_refusals(host["mcp"])
                # The executable probe preflight makes, against the refreshed reading. mcp_refusals
                # judges shape, ownership and agreement; register-mcp writes whatever
                # --bridge-command it was given without requiring it to exist, so a supported
                # registration landing here can name an absolute path that is not there, and this
                # would retire the live surfaces and install a record that cannot start a bridge.
                command = bridge_command(host)
                if command and not _executable(command):
                    changed.append("the bridge at " + str(command) + " is not an executable file,"
                                   " so the record this would install names something that cannot"
                                   " run")
                if changed:
                    # The refreshed reading has to pass the same checks preflight applied, or a
                    # surface that changed while this ran would reach the steps unvalidated: an
                    # alias registered in the meantime would survive the table removal and start a
                    # second bridge.
                    results.append(_answer("mcp record retire", REFUSED, "; ".join(changed)))
                    for remaining in MCP_STEPS[1:] + ("skill unlink",):
                        results.append(_answer(remaining, NOT_REACHED,
                                               "the bridge surface changed while this ran"))
                    break
            if apply and name in DESTRUCTIVE:
                changed = plugin_refusals(host)
                if changed:
                    results.append(_answer(name, REFUSED, "; ".join(changed)))
                    results += [_answer(other, NOT_REACHED,
                                        "the plugin stopped being able to serve what this removes")
                                for other, _step in ORDER[[n for n, _s in ORDER].index(name) + 1:]]
                    break
            if apply and name == "hook standdown":
                # The check and the removal under the SAME held locks. Asking whether the archived
                # settings came back and then removing the registration in a separate breath left
                # the window the question was asked about: the supported writer takes these locks,
                # so holding them across both is what actually keeps it out rather than merely
                # noticing afterwards that it got in.
                with contextlib.ExitStack() as guard:
                    retired = next((item for item in results
                                    if item["step"] == "settings retire"), None)
                    held = [moved["from"] for moved in (retired or {}).get("retired") or []]
                    held += [path for path in (retired or {}).get("watched") or []
                             if path not in held]
                    for path in held:
                        guard.enter_context(hostrecord.Locked(Path(path)))
                    back = _recreated_settings(results)
                    if back:
                        results.append(_answer(name, REFUSED,
                                               "settings were written again at " + "; ".join(back)
                                               + " after this run archived them, so the"
                                               " registration is left installed and reading what"
                                               " is there now. Removing it would leave a document"
                                               " the plugin settings cannot replace and no"
                                               " completion hook at all. Rerun to decide against"
                                               " the host as it stands", paths=back))
                        results += [_answer(other, NOT_REACHED,
                                            "the settings this run archived came back while it"
                                            " ran")
                                    for other, _step
                                    in ORDER[[n for n, _s in ORDER].index(name) + 1:]]
                        break
                    answer = step(host, options, apply=apply)
            else:
                answer = step(host, options, apply=apply) if name != "settings install" \
                    else step(host, options, apply=apply, previous=previous)
            results.append(answer)
            if name == MCP_STEPS[-1] and lock is not None:
                lock.__exit__(None, None, None)
                lock = None
            if answer["outcome"] == REFUSED:
                remaining = [n for n, _ in ORDER][
                    [n for n, _ in ORDER].index(name) + 1:]
                results += [_answer(n, NOT_REACHED, "an earlier step refused") for n in remaining]
                break
    except hostrecord.Busy as error:
        # Named for the step that was running, not for the last lock this function happens to
        # mention. Every step here takes a lock of its own, and a receipt that answers "mcp
        # ownership lock" for a contended settings file sends the operator to the wrong resource.
        results.append(_answer(active or "preflight", BUSY, str(error)))
        order = [n for n, _ in ORDER]
        remaining = order[order.index(active) + 1:] if active in order else order
        results += [_answer(other, NOT_REACHED,
                            "a lock an earlier step needs is held by another run")
                    for other in remaining]
    finally:
        if lock is not None:
            lock.__exit__(None, None, None)
        # In the finally, because an OSError on the way through does not resume after it. The
        # settings are retired for a standdown, and a run that dies between them leaves the same
        # host whether it died of a refusal, a held lock or a full disk. The original failure
        # keeps propagating; what changes is that the host is put back first.
        if apply:
            _rollback_if_unfinished(results)
    if apply and all(item["outcome"] in DONE for item in results):
        results.append(hook_recheck(host))
    return results


def _settings_owner(path):
    """The owner named by the document at this exact path, read now rather than from a snapshot."""
    document, _outcome, _detail, _found = completion.read_configuration(Path(path))
    return completion.owner_of(document) if document else None


def disable(host, options, *, apply=False):
    """Stop new calls by retiring the two records the packaged launchers read.

    Only the records this repository wrote FOR THE PLUGIN. Run before a transition, these paths
    hold the user-owned manual settings and bridge record, and retiring those would stop the
    manual installation while claiming to have disabled the plugin -- a different operation than
    the one asked for, performed on somebody else's registration.

    Each owner is read again inside the lock that serialises the move, because an owner read
    before the lock is the owner of a file that may have been replaced since. For the bridge
    record that lock is the shared ownership lock register-mcp takes, not the record file's own:
    the two owners write different files, so a user-owned record arriving through the
    configuration side is not serialised by locking this one, and a cached owner would have this
    command archive a registration that became somebody else's while it ran.
    """
    home = Path(host["codexHome"])
    results = []

    def decide(step, path, owner):
        """Every answer that needs no write. None means the move is this command's to make."""
        if not Path(path).exists():
            return _answer(step, ALREADY, str(path) + " is not there")
        linked = symlink_complaint(path)
        if linked:
            return _answer(step, REFUSED, linked)
        if owner != completion.OWNER_PLUGIN:
            return _answer(step, REFUSED,
                           str(path) + " does not name " + completion.OWNER_PLUGIN
                           + " as the owner of that registration (" + str(owner)
                           + "), so it is not this command's to retire. A malformed or"
                           " unreadable record owns nothing and is left where it is;"
                           " a user-owned one belongs to the manual install")
        if not apply:
            return _answer(step, WOULD, "would retire " + str(path))
        return None

    # Read from the paths being retired, not from the reading that honours the settings override:
    # with the override set, the owner of some other document would decide the fate of this one,
    # and a refusal on that basis leaves the bridge record retired and the hook settings in place.
    #
    # The fixed path, for the same reason the install writes it: the packaged launcher reads that
    # one file and ignores the settings override, so retiring whatever an override happens to name
    # would leave the document the launcher actually reads in place and stop nothing.
    settings = home / completion.CONFIG_NAME
    try:
        answer = decide("hook settings", settings, _settings_owner(settings))
        if answer is None:
            with hostrecord.Locked(settings):
                answer = decide("hook settings", settings, _settings_owner(settings))
                if answer is None:
                    answer = _answer("hook settings", SETTLED, "retired " + str(settings),
                                     applied=True, wrote=True, retired=retire(settings))
        results.append(answer)
    except hostrecord.Busy as error:
        results.append(_answer("hook settings", BUSY, str(error)))

    if not apply:
        record = Path(host["mcp"]["recordPath"])
        results.append(decide("bridge record", record, host["mcp"]["recordOwner"]))
        return results
    try:
        with hostrecord.Locked(bridgerecord.ownership_lock_path(home)):
            # Re-read inside the lock, the way register-mcp decides its own write: the owner that
            # authorises this move has to be the owner of the record the move will take.
            mcp = inventory.read_mcp(home)
            record = Path(mcp["recordPath"])
            answer = decide("bridge record", record, mcp["recordOwner"])
            if answer is None:
                answer = _answer("bridge record", SETTLED, "retired " + str(record), applied=True,
                                 wrote=True, retired=retire(record))
            results.append(answer)
    except hostrecord.Busy as error:
        results.append(_answer("bridge record", BUSY, str(error)))
    return results


def preserved_paths(host):
    """What disable and remove do not touch, named so the output can say it rather than imply it."""
    document = (host.get("registered") or {}).get("document") \
        or host["settings"]["document"] or {}
    return {k: v for k, v in {
        "relayStore": document.get("dbPath"),
        "markerRoot": document.get("markerRoot"),
        "hookJournal": document.get("journalRoot"),
        "runtimeInstallation": host.get("destination"),
        "pluginCache": host["plugin"].get("cacheVersion"),
    }.items() if v}


# Each stop claim names the step that has to have settled for it to be true. Emitting them as a
# fixed list said "new adapter invocations are stopped" on a dry run that wrote nothing and on a
# run whose retire refused, so the receipt claimed an effect the host did not have and no reader
# could tell an intended effect from an applied one.
STOP_CLAIMS = (
    ("hook settings", "new adapter invocations, because the packaged launcher finds no settings"
                      " and returns without running anything"),
    ("bridge record", "new bridge starts, because the packaged launcher has no record to read"),
)


def stop_claims(results):
    """The stop claims split by what this run actually did to each surface.

    settled and already_done are both true of the host now: one because this run moved the record,
    the other because there was none there to move. would_change is what an --apply would do and
    nothing more. Every other outcome leaves the surface live, and the reason travels with it
    rather than being left for a reader to infer from the step list.
    """
    answers = {item["step"]: item for item in results}
    stopped, projected, live = [], [], []
    for step, claim in STOP_CLAIMS:
        item = answers.get(step)
        if item is None:
            live.append(claim + " -- NOT stopped: " + step + " did not run")
        elif item["outcome"] in (SETTLED, ALREADY):
            stopped.append(claim)
        elif item["outcome"] == WOULD:
            projected.append(claim)
        else:
            live.append(claim + " -- NOT stopped: " + str(item.get("detail")))
    return {"stopped": stopped, "wouldStop": projected, "stillLive": live}


def remove(host, options, *, apply=False):
    """Retire the records, then the links, and only in that order.

    The links go last and only when the records were settled. Unlinking after a refused disable
    would take the skills away from an installation this command just declined to touch, which is
    the manual install losing its skills because the plugin's records were not ours to retire.
    """
    results = disable(host, options, apply=apply)
    if any(item["outcome"] in (REFUSED, BUSY) for item in results):
        results.append(_answer("skill unlink", NOT_REACHED,
                               "the records were not retired, so the links are left where they"
                               " are: removing them now would take the skills from an install"
                               " this command did not disable"))
        return results
    results.append(skill_unlink(host, options, apply=apply))
    return results


def swap_state(host, options):
    """What a version replacement left, with the pointer and the record reported apart."""
    point = host["pointer"]
    recorded = None
    detail = None
    path = None
    try:
        # The host record follows XDG rather than the Codex home, so the path it resolves to is
        # reported: an answer about a record is useless without saying which record was read.
        path = hostrecord.record_path()
        loaded = hostrecord.load(path, 1)
        # load() answers with a Reading, and a Reading is an object: truthy for an absent record,
        # an unreadable one and a malformed one alike. Reporting presence from bool() told an
        # operator a host record was there after exactly the failure that would remove it.
        state = getattr(loaded, "state", None)
        usable = getattr(loaded, "usable", None)
        recorded = bool(state == reading.PRESENT) if state is not None else None
        detail = getattr(loaded, "detail", None) if not usable else None
        if state is not None and state != reading.PRESENT:
            detail = detail or ("the host record reading answered " + str(state))
    except Exception as error:  # noqa: BLE001 - a private receipt that cannot be read is a reading
        detail = type(error).__name__ + ": " + str(error)
    return {
        "command": "swap-state",
        "pointerState": point.get("state"),
        "pointerTarget": point.get("target"),
        "pointerResolves": bool(point.get("targetDirectory")),
        "recordedHostRecord": recorded,
        "hostRecordPath": str(path) if path else None,
        "recordedDetail": detail,
        "agrees": None,
        "residualFromRun": None,
        "note": ("residualPaths, residualOwnership and recoveryRequires exist only in the failed"
                 " run's own result and cannot be recovered from any later reading, so they are"
                 " reported as absent rather than invented. The retry is the owner's command:"
                 " runtime_install.py install --apply. Nothing here moves a pointer, writes a"
                 " host record or touches the store."),
        "preserved": preserved_paths(host),
    }
