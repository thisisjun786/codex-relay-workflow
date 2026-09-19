"""The ordered transition, and the refusals that protect a working host from it.

One rule underneath all of it: nothing is removed that this repository cannot prove runs its own
code, and nothing is removed before the replacement is proven able to serve it. Every step decides
from what is on disk, so an interrupted run converges on the next one.
"""

import errno
import json
import os
import shutil
import subprocess
import sys
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
    if mcp.get("recordOwner") == bridgerecord.OWNER_USER and registration:
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
    for entry in host["hook"]["entries"]:
        named = entry.get("settings")
        if not named or not entry.get("proven"):
            continue
        document, _outcome, _detail, _found = completion.read_configuration(Path(named))
        if document is not None:
            return document, "the settings the registration names, " + str(named)
    document = host["settings"].get("document")
    if document is not None:
        return document, "the settings at " + str(host["settings"]["path"])
    return None, None


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
    if plugin.get("enabled") is False:
        refusals.append("the plugin entry " + str(plugin.get("entryKey")) + " is disabled, so its"
                        " skills, hook and server would not load")
    if not plugin.get("cacheVersion"):
        # The reader's own reason travels with the refusal. Without it, a host carrying two cached
        # versions is reported as a host carrying none, which sends the operator to the wrong repair.
        refusals.append("no single installed plugin version could be named under "
                        + str(Path(host["codexHome"]) / "plugins" / "cache")
                        + (": " + str(plugin["detail"]) if plugin.get("detail") else ""))
    else:
        # The repository already owns a payload contract. A file census is not it: an empty hook
        # document and an empty mcp.json satisfy existence and leave no hook and no bridge.
        argv = [sys.executable, str(Path(host["repoRoot"]) / "scripts" / "ci" / "plugin.py"),
                "--payload", plugin["cacheVersion"]]
        try:
            done = subprocess.run(argv, capture_output=True, text=True, timeout=300)
            notes.append({"payloadCheck": argv, "exitCode": done.returncode})
            if done.returncode != 0:
                refusals.append("the installed payload at " + plugin["cacheVersion"]
                                + " did not pass scripts/ci/plugin.py --payload, so what is"
                                  " installed is not a package this transition can rely on: "
                                + (done.stdout + done.stderr).strip()[:400])
        except (OSError, subprocess.SubprocessError) as error:
            refusals.append("the installed payload could not be validated: "
                            + type(error).__name__ + ": " + str(error))
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

    if not host.get("destination"):
        refusals.append("no install destination was named or derivable from the recorded"
                        " relayExecutable, so no adapter path could be recorded")
    else:
        point = host["pointer"]
        if point.get("state") != "LINK" or not point.get("targetDirectory"):
            refusals.append("the pointer at " + str(point.get("pointer")) + " is "
                            + str(point.get("state")) + " (" + str(point.get("detail")) + ")."
                            " A recorded adapter path has to resolve, and a dangling pointer"
                            " still reads as a link")
        else:
            interpreter, adapter = adapter_paths(host)
            for label, path in (("the adapter", adapter), ("its interpreter", interpreter),
                                ("the bridge", bridge_command(host))):
                if not _executable(path):
                    refusals.append(label + " at " + str(path) + " is not an executable file, so"
                                    " recording it would name something that cannot run")

    # Checked here, before the standdown, because the packaged launcher reads one fixed path and
    # ignores this override: with it set, the plugin-owned document would be written where no
    # launcher looks. Discovering that at the write means discovering it after the working
    # registration has been removed and both settings files moved aside, which leaves the host with
    # no completion hook and an explanation. A populated override also passes every other reading,
    # so nothing else here catches it.
    refusals.extend(completion.override_complaints(completion.OWNER_PLUGIN))

    # The document registered_settings() will carry, because that is the one whose budget and
    # journal policy have to be expressible as a plugin-owned document. Validating the fixed file
    # while carrying the custom one let a faults_only policy or a 9-second budget through preflight
    # and refuse at the write, with the registration already removed.
    document, _source = registered_settings(host)
    budget = (document or {}).get("timeoutSeconds")
    if isinstance(budget, (int, float)) and not isinstance(budget, bool) \
            and budget >= completion.LAUNCHER_CEILING_SECONDS:
        # Checked here rather than at the write. The packaged launcher caps its own deadline at
        # that ceiling and has to outlast the adapter it runs, so these settings cannot become
        # plugin-owned -- and finding that out after the registration has been removed would leave
        # the host with no completion hook and a refusal.
        refusals.append("the settings record a guard budget of " + str(budget) + "s, and a"
                        " plugin-owned document has to stay under "
                        + str(completion.LAUNCHER_CEILING_SECONDS) + "s so the packaged launcher"
                        " outlasts the adapter it runs. Lower it before transitioning")
    policy = (document or {}).get("journalPolicy")
    if policy and policy != completion.EVERY_INVOCATION:
        refusals.append("the settings record journalPolicy " + str(policy) + ", and the document"
                        " this command builds always records " + completion.EVERY_INVOCATION
                        + ", so transitioning would change what this host records without being"
                          " asked")
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
        # The consent question is asked again here, against the file being written. The list the
        # snapshot carried was computed before anything was locked, so a foreign hook that landed
        # in the same group since then would have its index moved, and its recorded trust detached,
        # without anyone having agreed to it.
        shifted_now = inventory.shifted_identities(hooks.inventory(again.value), current)
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
    paths = []
    for candidate in [host["settings"]["path"], fixed] + named:
        if not candidate or candidate in paths or not Path(candidate).exists():
            continue
        if candidate != fixed:
            document, outcome, _detail, _found = completion.read_configuration(Path(candidate))
            if document is None:
                continue
        paths.append(candidate)
    # De-duplicated keeping the LAST occurrence, so a path that is both the fixed one and the one
    # the registration names is archived in the registered position rather than the earlier one.
    paths = [path for index, path in enumerate(paths) if path not in paths[index + 1:]]
    if not paths:
        return _answer("settings retire", ALREADY, "no settings file is there to retire")
    document = host["settings"]["document"]
    if document and completion.owner_of(document) == completion.OWNER_PLUGIN:
        return _answer("settings retire", ALREADY,
                       "the settings already name the plugin as the owner")
    if not apply:
        return _answer("settings retire", WOULD, "would retire " + ", ".join(paths), paths=paths)
    home = Path(host["codexHome"])
    moved = []
    for candidate in paths:
        with hostrecord.Locked(Path(candidate)):
            moved.append({"from": candidate,
                          "to": retire(candidate, into=home, stem=completion.CONFIG_NAME)})
    return _answer("settings retire", SETTLED, "retired " + ", ".join(p["from"] for p in moved),
                   applied=True, wrote=True, retired=moved)


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
    carried = "the settings being replaced"
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
    if timeout >= completion.LAUNCHER_CEILING_SECONDS:
        return _answer("settings install", REFUSED,
                       "the previous guard budget is " + str(timeout) + "s, and a plugin-owned"
                       " document has to stay under " + str(completion.LAUNCHER_CEILING_SECONDS)
                       + "s so the packaged launcher outlasts the adapter it runs")
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
    arguments = registration.get("args") or (retired or {}).get("args") or []
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


def skill_unlink(host, options, *, apply=False):
    owned = host["skills"]["crwOwned"]
    if not owned:
        return _answer("skill unlink", ALREADY, "no CRW-owned links are there")
    if not apply:
        return _answer("skill unlink", WOULD, "would remove "
                       + ", ".join(item["path"] for item in owned),
                       paths=[item["path"] for item in owned])
    removed = []
    for item in owned:
        path = Path(item["path"])
        if path.is_symlink():
            path.unlink()
            removed.append(str(path))
    return _answer("skill unlink", SETTLED, "removed " + ", ".join(removed), applied=True,
                   wrote=True, removed=removed,
                   foreignLeft=[item["path"] for item in host["skills"]["foreign"]])


ORDER = (("hook standdown", hook_standdown), ("settings retire", settings_retire),
         ("settings install", settings_install), ("mcp record retire", mcp_record_retire),
         ("mcp table standdown", mcp_table_standdown),
         ("mcp record install", mcp_record_install), ("skill unlink", skill_unlink))

# The MCP surface is decided and written under ONE lock, the same one register-mcp takes, because a
# concurrent user-owned registration landing between the retire and the table removal would put the
# record back, refuse the plugin record, and leave the host with no bridge.
MCP_STEPS = ("mcp record retire", "mcp table standdown", "mcp record install")


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
                               repo_root=host["repoRoot"])
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
    return _answer("hook recheck", SETTLED,
                   "no registration of this adapter is in the hook file")

def transition(host, options, *, apply=False):
    """Run the steps in order, stopping at the first refusal."""
    results = [preflight(host, options)]
    if results[0]["outcome"] == REFUSED:
        results += [_answer(name, NOT_REACHED, "preflight refused") for name, _ in ORDER]
        return results
    previous, _carried = registered_settings(host)
    lock = None
    try:
        for name, step in ORDER:
            if name == MCP_STEPS[0] and apply:
                lock = hostrecord.Locked(bridgerecord.ownership_lock_path(host["codexHome"]))
                lock.__enter__()
                # Re-read INSIDE the lock, because the snapshot was taken before it. Two concurrent
                # runs would otherwise both hold the user-owned record in memory, and the second
                # would retire the plugin record the first had just written, leaving new sessions
                # with no bridge. The decision is made about the state that will be written.
                host = {**host, "mcp": inventory.read_mcp(host["codexHome"])}
                changed = mcp_refusals(host["mcp"])
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
        results.append(_answer("mcp ownership lock", BUSY, str(error)))
    finally:
        if lock is not None:
            lock.__exit__(None, None, None)
    if apply and all(item["outcome"] in DONE for item in results):
        results.append(hook_recheck(host))
    return results


def disable(host, options, *, apply=False):
    """Stop new calls by retiring the two records the packaged launchers read.

    Only the records this repository wrote FOR THE PLUGIN. Run before a transition, these paths
    hold the user-owned manual settings and bridge record, and retiring those would stop the
    manual installation while claiming to have disabled the plugin -- a different operation than
    the one asked for, performed on somebody else's registration.
    """
    results = []
    # Read from the paths being retired, not from the reading that honours the settings override:
    # with the override set, the owner of some other document would decide the fate of this one,
    # and a refusal on that basis leaves the bridge record retired and the hook settings in place.
    settings_here, _outcome, _detail, _found = completion.read_configuration(
        Path(host["codexHome"]) / completion.CONFIG_NAME)
    owners = {"hook settings": completion.owner_of(settings_here) if settings_here else None,
              "bridge record": host["mcp"]["recordOwner"]}
    # The fixed path, for the same reason the install writes it: the packaged launcher reads that
    # one file and ignores the settings override, so retiring whatever an override happens to name
    # would leave the document the launcher actually reads in place and stop nothing.
    for step, path in (("hook settings", Path(host["codexHome"]) / completion.CONFIG_NAME),
                       ("bridge record", Path(host["mcp"]["recordPath"]))):
        if not Path(path).exists():
            results.append(_answer(step, ALREADY, str(path) + " is not there"))
            continue
        if owners.get(step) != completion.OWNER_PLUGIN:
            results.append(_answer(step, REFUSED,
                                   str(path) + " does not name " + completion.OWNER_PLUGIN
                                   + " as the owner of that registration (" + str(owners.get(step))
                                   + "), so it is not this command's to retire. A malformed or"
                                   " unreadable record owns nothing and is left where it is;"
                                   " a user-owned one belongs to the manual install"))
            continue
        if not apply:
            results.append(_answer(step, WOULD, "would retire " + str(path)))
            continue
        with hostrecord.Locked(Path(path)):
            results.append(_answer(step, SETTLED, "retired " + str(path), applied=True,
                                   wrote=True, retired=retire(path)))
    return results


def preserved_paths(host):
    """What disable and remove do not touch, named so the output can say it rather than imply it."""
    document = host["settings"]["document"] or {}
    return {k: v for k, v in {
        "relayStore": document.get("dbPath"),
        "markerRoot": document.get("markerRoot"),
        "hookJournal": document.get("journalRoot"),
        "runtimeInstallation": host.get("destination"),
        "pluginCache": host["plugin"].get("cacheVersion"),
    }.items() if v}


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
    try:
        path = hostrecord.record_path()
        loaded = hostrecord.load(path, 1)
        recorded = loaded
    except Exception as error:  # noqa: BLE001 - a private receipt that cannot be read is a reading
        detail = type(error).__name__ + ": " + str(error)
    return {
        "command": "swap-state",
        "pointerState": point.get("state"),
        "pointerTarget": point.get("target"),
        "pointerResolves": bool(point.get("targetDirectory")),
        "recordedHostRecord": bool(recorded),
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
