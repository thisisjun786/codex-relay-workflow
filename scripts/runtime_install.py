#!/usr/bin/env python3
"""Install and diagnose the runtime the workflow depends on: the bridge and the relay.

This is the separate runtime entry point OPS-2.3 asks for. scripts/install.py stays what it
is - the standard-library-only, idempotent link step for skills - and runtime installation is
never folded into it. This command reads that installer rather than reimplementing it, and it
reuses its LINKED / MISSING / CONFLICT vocabulary so one word means one thing across both.

Standard library only, and importable on the Python this repository runs its own checks with.
The components it installs need 3.11 or newer; that interpreter is resolved, not assumed.
"""

import argparse
import hashlib
import json
import os
import shutil
import socket
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from crw_runtime import check, codexconfig, definition, hooks, hostrecord, ownership, scope

ROOT = Path(__file__).resolve().parents[1]
EXIT_OK, EXIT_REFUSED, EXIT_USAGE = 0, 1, 2
MCP_NAME = "codex-thread-bridge"


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def emit(payload):
    print(json.dumps(payload, indent=2, sort_keys=False, default=str))


def acting_process():
    return "runtime_install.py on " + sys.executable


# ----------------------------------------------------------------- verify-definition

def cmd_verify_definition(args):
    findings = definition.verify(ROOT)
    emit({
        "command": "verify-definition",
        "definition": str(definition.DEFINITION_PATH.relative_to(ROOT)),
        "findings": findings,
        "ok": not findings,
        "note": (
            "Re-derives every derivable field from this checkout. The upstream tree hash and"
            " the repository commit are not derivable here and are recorded or measured at run"
            " time instead; see the definition's own notes."
        ),
    })
    return EXIT_OK if not findings else EXIT_REFUSED


# ------------------------------------------------------------------------- skill links

def skill_links(codex_home):
    """Read the existing skill-link layer by running its own installer, never by copying it."""
    destination = Path(codex_home) / "skills"
    argv = [sys.executable, str(ROOT / "scripts" / "install.py"), "--check", "--dest", str(destination)]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return {"unreadable": type(error).__name__ + ": " + error.__str__(), "command": argv}
    lines = [line for line in (done.stdout + done.stderr).splitlines() if line.strip()]
    return {
        "command": argv,
        "exitCode": done.returncode,
        "linked": [l.split(" ", 1)[1] for l in lines if l.startswith("LINKED ")],
        "missing": [l.split(" ", 1)[1] for l in lines if l.startswith("MISSING ")],
        "conflict": [l.split(" ", 1)[1] for l in lines if l.startswith("CONFLICT ")],
        "legacy": [l.split(" ", 1)[1] for l in lines if l.startswith("LEGACY ")],
        "note": "scripts/install.py is unchanged and was run read-only with --check",
    }


# ------------------------------------------------------------------------- ownership

def resolve_entry_point(console_script, override=None):
    if override:
        return Path(override)
    found = shutil.which(console_script)
    return Path(found) if found else None


def interpreter_of(entry_point):
    """The interpreter a console script is bound to, read from its shebang."""
    try:
        first = Path(entry_point).read_text(encoding="utf-8", errors="replace").splitlines()[0]
    except (OSError, IndexError):
        return None
    return first[2:].strip() if first.startswith("#!") else None


def interpreter_version(python):
    try:
        done = subprocess.run(
            [str(python), "-c", "import platform;print(platform.python_version())"],
            capture_output=True, text=True, timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() if done.returncode == 0 else None


def module_location(python, module):
    """Where the interpreter actually imports this module from. Never assumed.

    An editable install leaves nothing under site-packages and a copied one does, so the
    only honest answer comes from the interpreter itself (OPS-1.1).
    """
    code = "import " + module + " as m, os; print(os.path.dirname(m.__file__))"
    argv = [str(python), "-c", code]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return None, type(error).__name__ + ": " + error.__str__(), argv
    if done.returncode != 0:
        return None, (done.stderr.strip().splitlines() or ["import failed"])[-1], argv
    return done.stdout.strip(), None, argv


def recorded_roots(record, name):
    """Paths the definition or this host's record already accounts for.

    The source checkout is one of them, but an installation this command produced lives
    wherever its destination was, which is normally outside the checkout. Treating only
    the checkout as recorded would classify every runtime it installs as foreign for
    ever, and nothing would ever become reusable. Source-checkout identity and
    installed-runtime identity stay separate readings; this only decides which paths are
    accounted for.
    """
    roots = [ROOT.resolve()]
    for install in ((record or {}).get("components", {}).get(name) or {}).get("installs", []):
        for key in ("environment", "location"):
            value = install.get(key)
            if value:
                roots.append(Path(value).resolve())
    return roots


def classify_component(component, *, record, entry_override=None, registration=None):
    """Gather the four OPS-2.1 signals and classify."""
    unreadable = []
    entry = resolve_entry_point(component["consoleScript"], entry_override)
    resolved = entry.resolve() if entry and entry.exists() else None

    roots = recorded_roots(record, component["component"])
    entry_recorded = bool(resolved and any(
        str(resolved).startswith(str(r)) for r in roots
    ))
    shebang = interpreter_of(resolved) if resolved else None
    if resolved and not entry_recorded and shebang:
        entry_recorded = any(str(Path(shebang).resolve()).startswith(str(r)) for r in roots)

    python = shebang or (sys.executable if resolved is None else shebang)
    version = interpreter_version(python) if python else None
    location, import_error, import_command = (None, "no interpreter to ask", None)
    if python:
        location, import_error, import_command = module_location(python, component["module"])

    current_digest = None
    digest_matches = None
    if location and Path(location).is_dir():
        current_digest = definition.ops12_digest(location)
        digest_matches = current_digest == component["sourceDigest"]
    elif resolved is not None:
        unreadable.append("the installed package location for " + component["component"])

    # The component's subdirectory tree is the identity test, because packages share a
    # repository commit and a commit therefore cannot say whether this component changed
    # (OPS-1.5). The repository commit is read and reported beside it, but it is
    # informational: an unrelated commit must not turn an unchanged component into a fork.
    tree_matches = None
    if entry_recorded:
        tree_matches = definition.git(
            ["rev-parse", "HEAD:" + component["subdirectory"]], ROOT
        ) == component["subdirectoryTree"]
    repository_commit = definition.git(["rev-parse", "HEAD"], ROOT)
    recorded_commit = ((record or {}).get("components", {}).get(component["component"]) or {}) \
        .get("repositoryCommit")
    commit_matches = None
    clean = definition.working_tree_clean(ROOT) if entry_recorded else None
    if entry_recorded and clean is None:
        unreadable.append("the working tree cleanliness of " + str(ROOT))

    points = []
    if record is not None and location and version:
        points = hostrecord.points_for(
            record, component["component"], location=location,
            interpreter=version, install_digest=current_digest,
            codex_cli=codex_cli_version(), host=socket.gethostname(),
        )
    elif record is None:
        unreadable.append("the host record")

    conflict = None
    if registration and registration.get("outcome") == "CONFLICT":
        conflict = "the Codex configuration registers " + MCP_NAME + " differently: " + registration["detail"]
    if registration and registration.get("outcome") == "UNREADABLE":
        unreadable.append("the Codex configuration: " + str(registration.get("detail")))

    signals = ownership.Signals(
        entry_point_recorded=entry_recorded,
        commit_matches=commit_matches,
        tree_matches=tree_matches,
        working_tree_clean=clean,
        digest_matches=digest_matches,
        has_point=bool(points),
        registration_conflict=conflict,
        unreadable=unreadable,
    )
    classification, reasons = ownership.classify(signals)
    return {
        "component": component["component"],
        "class": classification,
        "reasons": reasons,
        "entryPoint": str(entry) if entry else None,
        "entryPointResolves": str(resolved) if resolved else None,
        "entryPointInRecordedPath": entry_recorded,
        "interpreter": version,
        "importedLocation": location,
        "importError": import_error,
        "importCommand": import_command,
        "digestMatches": digest_matches,
        "installedDigest": current_digest,
        "repositoryCommit": repository_commit,
        "repositoryCommitRecordedAtInstall": recorded_commit,
        "repositoryCommitDrift": (
            None if not recorded_commit else recorded_commit != repository_commit
        ),
        "repositoryCommitMeaning": (
            "reported, not used as the identity test. The component subdirectory tree decides"
            " whether this component changed (OPS-1.5); an unrelated commit does not."
        ),
        "measuredPoints": len(points),
        "reusable": ownership.reusable(classification),
    }


# ------------------------------------------------------------------------- diagnose

def read_config(codex_home):
    path = Path(codex_home) / "config.toml"
    return path, (path.read_text(encoding="utf-8") if path.is_file() else "")


def registration_state(codex_home, command, args, name=MCP_NAME):
    """What the configuration registers, and only compared when a command was supplied.

    Diagnosis with no expected command must not invent one. Comparing an existing, correct
    registration against an empty string reports CONFLICT for a host that is registered
    exactly right, and that false conflict then drags the component and the installed
    result down with it.
    """
    path, text = read_config(codex_home)
    view = codexconfig.scan(text)
    if not view.readable:
        return {"path": str(path), "outcome": "UNREADABLE",
                "detail": "; ".join(view.unreadable), "wouldWrite": False,
                "registered": None}
    registered = view.servers.get(name)
    if not command:
        return {
            "path": str(path),
            "outcome": "PRESENT" if registered else "ABSENT",
            "detail": ("the configuration registers " + repr((registered or {}).get("command"))
                       + "; no expected command was supplied, so nothing was compared")
                      if registered else "no registration for " + name,
            "wouldWrite": False,
            "registered": registered,
        }
    new_text, outcome, detail = codexconfig.register(text, name, command, args)
    return {"path": str(path), "outcome": outcome, "detail": detail,
            "wouldWrite": new_text != text, "registered": registered}


def cmd_diagnose(args):
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    data = definition.load()
    record_path = Path(args.record) if args.record else hostrecord.record_path()
    record = hostrecord.load(record_path, data["definitionVersion"])

    bridge = next(c for c in data["components"] if c["component"] == "codex-thread-bridge")
    relay_component = next(c for c in data["components"] if c["component"] == "codex-session-relay")

    registration = registration_state(codex_home, args.bridge_command or "", args.bridge_arg or [])
    classes = {
        c["component"]: classify_component(
            c, record=record,
            entry_override=args.relay_command if c["component"] == "codex-session-relay" else None,
            registration=registration if c["component"] == "codex-thread-bridge" else None,
        )
        for c in data["components"]
    }

    relay_executable = args.relay_command or shutil.which(relay_component["consoleScript"])
    survey = readings = None
    summary = {"skipped": "no relay executable was found, so no scope reading was made"}
    if relay_executable:
        readings = scope.survey(executable=relay_executable, socket=args.socket, state=args.state)
        status = scope.relay(["service", "status"], executable=relay_executable,
                             socket=args.socket, state=args.state)
        summary = scope.summarise(readings, issue=args.issue, service={
            "reading": status.get("payload"),
            "note": "who owns the service for this scope. A service is never started here, and"
                    " no parent may stop one another parent is using (OPS-4.1).",
        })

    # OPS-3.4's lookup constructs a store, and a diagnosis constructs none, so it is opt-in
    # and never part of the default path.
    assignment = {"ran": False, "reason": (
        "assignment-find constructs a writable store and this command constructs none."
        " Pass --assignment-lookup to run it, or use --trial, where it runs before anything"
        " is written."
    )}
    if args.assignment_lookup and relay_executable and args.issue:
        found = scope.relay(["assignment-find", "--issue", str(args.issue)],
                            executable=relay_executable, socket=args.socket, state=args.state)
        assignment = {"ran": True, "ok": found.get("ok"), "command": found.get("command"),
                      "payload": found.get("payload"),
                      "note": "OPS-3.4 also wants this reading from each participating"
                              " process; one command cannot produce that."}
    elif args.assignment_lookup:
        assignment = {"ran": False, "reason": "no --issue, or no relay executable was found"}
    if summary:
        summary["assignmentFind"] = assignment

    both_own = all(c["class"] == "own" for c in classes.values())
    imported_ok = all(c["importedLocation"] for c in classes.values())

    connect = (summary or {}).get("socketConnect")
    fields = {
        "installed": check.field(
            "verified" if both_own else "not_verified",
            "component classes: " + json.dumps({k: v["class"] for k, v in classes.items()})
            + ". Only 'own' is reusable (OPS-2.2).",
            command="runtime_install.py diagnose", acting_process=acting_process(), measured_at=now(),
        ),
        "imported": check.field(
            "verified" if imported_ok else "not_verified",
            "resolved locations: " + json.dumps({k: v["importedLocation"] for k, v in classes.items()})
            + ". Read from the interpreter, so an import satisfied by another copy is visible.",
            command=json.dumps({k: v.get("importCommand") for k, v in classes.items()}),
            acting_process=acting_process(), measured_at=now(),
        ),
        "mcpExposed": _mcp_exposed(registration, args.observed_tool),
        "connected": check.field(
            "verified" if connect == "ok" else ("not_verified" if connect else "unknown"),
            "doctor actorReachability.socketConnect = " + repr(connect)
            + ". A socket file existing on disk does not establish this.",
            command=json.dumps(((readings or {}).get("selected")
                                or (readings or {}).get("discovery") or {}).get("command")),
            acting_process=acting_process(), measured_at=now() if connect else None,
        ),
        "deliveryAccepted": (
            check.not_applicable(
                "no trial was requested. This field requires an attempt that recorded a returned"
                " turn id, which means creating work, so it is only measured under --trial."
            ) if not args.trial else _trial(args, relay_executable)
        ),
        "verificationComplete": check.not_applicable(
            "OPS-6.4 is a property of a verdict at a head, not of an installation. This command"
            " observes no verdict and never infers one from a completed turn or a green check."
        ),
        "alwaysActive": check.field(
            "not_verified",
            "no supervised runtime was enabled and no host restart was observed. Installation is"
            " not activation; this command enables no daemon.",
            acting_process=acting_process(),
        ),
        "settingsPreserved": check.field(
            "verified" if not registration["wouldWrite"] else "not_applicable",
            "diagnose writes nothing, so every table in config.toml and every hook entry is"
            " unchanged by it. Registration outcome would be " + registration["outcome"] + ".",
            acting_process=acting_process(), measured_at=now(),
        ),
    }

    emit({
        "command": "diagnose",
        "definitionVersion": data["definitionVersion"],
        "repositoryCommit": definition.git(["rev-parse", "HEAD"], ROOT),
        "codexHome": str(codex_home),
        "hostRecord": str(record_path),
        "hostRecordPresent": record is not None and Path(record_path).is_file(),
        "skillLinks": skill_links(codex_home),
        "components": classes,
        "mcpRegistration": registration,
        "scope": summary,
        "assignment": assignment,
        "scopeReadings": readings,
        "checks": check.record(
            fields, destination=codex_home,
            destination_kind="temporary" if args.temporary else "host",
            scope=(summary or {}).get("stateDirectory"),
        ),
    })
    return EXIT_OK


def _mcp_exposed(registration, observed):
    """Registration alone is never enough, and a tool list alone is not either.

    The bridge's own smoke check launches its own server, so its tool list says nothing about
    whether the REGISTERED command works. Both halves are required.
    """
    if registration["outcome"] == "CONFLICT":
        return check.field("not_verified", "the configuration registers a different command: "
                          + registration["detail"], acting_process=acting_process(), measured_at=now())
    registered = registration["outcome"] in ("LINKED", "PRESENT")
    identity_tool = _bridge_identity_tool()
    if observed and identity_tool not in observed:
        return check.field(
            "not_verified",
            "the observed tools " + ", ".join(observed) + " do not include " + identity_tool
            + ", which this bridge defines, so they do not establish that THIS server is the"
            " one exposed.",
            acting_process=acting_process(), measured_at=now(),
        )
    if not observed:
        return check.field(
            "not_verified",
            "registration outcome " + registration["outcome"] + ". No tool names were observed:"
            " only a live session can list them, and a configuration entry alone never establishes"
            " this field. Supply --observed-tool from a session that lists them.",
            acting_process=acting_process(), measured_at=now(),
        )
    if not registered:
        return check.field(
            "not_verified",
            "tools were observed (" + ", ".join(observed) + ") but the configuration does not"
            " register this exact command, so the observation does not cover the registration"
            " being diagnosed.",
            acting_process=acting_process(), measured_at=now(),
        )
    return check.field(
        "verified",
        "the configuration registers this exact command and these tools were listed in a live"
        " session: " + ", ".join(observed),
        acting_process=acting_process(), measured_at=now(),
    )


def _bridge_identity_tool():
    bridge = next(c for c in definition.load()["components"]
                  if c["component"] == "codex-thread-bridge")
    return bridge["identityTool"]


def codex_cli_version():
    try:
        done = subprocess.run(["codex", "--version"], capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() or None if done.returncode == 0 else None


TRIAL_RELATIONSHIP = "<relationship>"
TRIAL_GENERATION = "<generation>"
TRIAL_EVENT = "<event>"


def trial_request_id(issue):
    return "jun104-trial-" + str(issue)


def trial_steps(*, issue, parent_task, child_task, recipient, artifact_root,
                turn_thread, turn_id, host, artifacts=None, dispatch_turn_id=None,
                turn_status="completed", recipient_settings=None):
    """The exact relay invocations the trial makes, returned as data.

    Kept as data rather than built inline so the argv this command sends can be checked
    against the relay's own required arguments without performing a delivery. Identifiers
    that only exist once an earlier step has run appear as placeholders and are substituted
    at execution time.
    """
    request_id = trial_request_id(issue)
    steps = [
        # First, before anything is written. A lookup run after register could find the
        # relationship this trial just created, which says nothing about the store
        # (OPS-3.4). Run first, it describes the store as it was found.
        ["assignment-find", "--issue", str(issue)],
    ]
    if recipient_settings:
        # A send is withheld until the recipient's authorized settings are on record, because
        # preserving them is what the delivery has to check against.
        steps.append(["settings-record", "--task", str(recipient),
                      "--settings", str(recipient_settings)])
    return steps + [
        ["register", "--parent-task", str(parent_task), "--parent-host", str(host),
         "--child-task", str(child_task), "--child-host", str(host),
         "--issue", str(issue), "--artifact-root", str(artifact_root),
         "--allowed-recipient", str(recipient), "--dispatch-request-id", request_id],
        # The generation stays unbound until an exact dispatch turn id is supplied, and the
        # relay refuses to emit against an unbound generation.
        ["generation-open", "--relationship", TRIAL_RELATIONSHIP,
         "--dispatch-request-id", request_id]
        + (["--dispatch-turn-id", dispatch_turn_id] if dispatch_turn_id else []),
        # A reviewable receipt must carry a deliverable: the relay refuses one whose manifest
        # is empty, because that is the no-deliverable sentinel.
        # register opens the generation but leaves it unbound, and the relay refuses to emit
        # against a generation with no anchor. Binding is its own step, not a flag on the open.
        ["generation-bind", "--relationship", TRIAL_RELATIONSHIP,
         "--generation", TRIAL_GENERATION, "--dispatch-turn-id", dispatch_turn_id],
        # Only the anchor turn is admitted by default. A turn the child actually ran is a
        # continuation and needs an explicit admission naming the generation and an actor.
        ["admit-turn", "--relationship", TRIAL_RELATIONSHIP, "--generation", TRIAL_GENERATION,
         "--turn", str(turn_id), "--actor", str(child_task)],
        ["emit", "--relationship", TRIAL_RELATIONSHIP, "--generation", TRIAL_GENERATION,
         "--outcome", "ready_for_review", "--turn-thread", str(turn_thread),
         "--turn-id", str(turn_id), "--turn-status", str(turn_status)]
        + [token for artifact in (artifacts or []) for token in ("--artifact", str(artifact))],
        ["deliver", "--event", TRIAL_EVENT],
    ]


def _trial(args, relay_executable):
    """Register, open the generation, emit, then a bounded deliver. Only this path creates work.

    The field's evidence is the delivery attempt's returned turn id. emit stores the receipt
    and enqueues it; the attempt itself happens in deliver, so an emitted receipt's own turn
    id never satisfies deliveryAccepted (OPS-6.1).

    The invocations come from trial_steps so that what this sends is the same data a test can
    check against the relay's own required arguments.
    """
    if not relay_executable:
        return check.field("not_verified", "trial requested but no relay executable was found",
                           acting_process=acting_process())
    required = {"--issue": args.issue, "--parent-task": args.parent_task,
                "--child-task": args.child_task, "--recipient": args.recipient,
                "--artifact-root": args.artifact_root, "--turn-thread": args.turn_thread,
                "--turn-id": args.turn_id,
                # A reviewable receipt with an empty manifest is refused, and a generation
                # with no anchor cannot be emitted against. Both were discovered at the
                # relay, after the trial had already written rows.
                "--artifact": args.artifact,
                "--dispatch-turn-id": args.dispatch_turn_id}
    missing = sorted(name for name, value in required.items() if not value)
    if missing:
        return check.field(
            "not_verified",
            "trial requested but these inputs were not supplied: " + ", ".join(missing)
            + ". Everything the trial needs is checked here, before the first command, so an"
            " incomplete trial writes nothing.",
            acting_process=acting_process(), measured_at=now(),
        )
    if args.recipient != args.parent_task:
        return check.field(
            "not_verified",
            "the recipient " + str(args.recipient) + " is not the parent task "
            + str(args.parent_task) + ". A completion is queued to the relationship parent and"
            " that parent must be an allowed recipient, so this combination can only be"
            " refused after the store has been written to.",
            acting_process=acting_process(), measured_at=now(),
        )
    if not args.recipient_settings and not args.settings_already_recorded:
        return check.field(
            "not_verified",
            "a send is withheld until the recipient's authorized settings are on record."
            " Supply --recipient-settings, or --settings-already-recorded to proceed on the"
            " caller's own claim that they are already recorded for this recipient. There is"
            " no read-only way to check from here: every relay read constructs a store.",
            acting_process=acting_process(), measured_at=now(),
        )

    steps = trial_steps(
        issue=args.issue, parent_task=args.parent_task, child_task=args.child_task,
        recipient=args.recipient, artifact_root=args.artifact_root,
        turn_thread=args.turn_thread, turn_id=args.turn_id, host=socket.gethostname(),
        artifacts=args.artifact, dispatch_turn_id=args.dispatch_turn_id,
        turn_status=args.turn_status, recipient_settings=args.recipient_settings,
    )
    performed = []
    resolved = {}

    def run_step(argv):
        concrete = [resolved.get(token, token) for token in argv]
        reading = scope.relay(concrete, executable=relay_executable, socket=args.socket,
                              state=args.state)
        performed.append({"command": reading.get("command"), "ok": reading.get("ok"),
                          "exitCode": reading.get("exitCode")})
        return reading

    def refuse(step, reading):
        return check.field(
            "not_verified",
            step + " did not succeed, so nothing later could be established: "
            + str(reading.get("stderr") or reading.get("unreadable")
                  or json.dumps(reading.get("payload"))[:300])
            + ". Steps: " + json.dumps(performed),
            command=json.dumps(performed[-1]["command"]) if performed else None,
            acting_process=acting_process(), measured_at=now(),
        )

    by_name = {argv[0]: argv for argv in steps}

    # OPS-3.4: the lookup that distinguishes the expected store from a different populated
    # one. Run before anything is written, and compared against what the caller independently
    # expects. With no expectation supplied it stays an observation, not a proof.
    found = run_step(by_name["assignment-find"])
    assignment = {
        "ran": True,
        "ok": found.get("ok"),
        "payload": found.get("payload"),
        "expected": args.expect_relationship,
        "agrees": None,
        "meaning": (
            "OPS-3.4 also wants this reading from each participating process; one command"
            " cannot produce that, and this is the part it can."
        ),
    }
    if args.expect_relationship:
        seen = json.dumps(found.get("payload"))
        assignment["agrees"] = args.expect_relationship in seen
        if not assignment["agrees"]:
            return check.field(
                "not_verified",
                "the store does not hold the expected relationship "
                + str(args.expect_relationship) + " for issue " + str(args.issue)
                + ", so this process is pointed at a different store than the one the"
                " assignment lives in. Nothing was written. Lookup: " + seen[:300],
                command=json.dumps(performed[-1]["command"]),
                acting_process=acting_process(), measured_at=now(),
            )

    for argv in steps:
        if argv[0] == "settings-record":
            recorded = run_step(argv)
            if not recorded.get("ok"):
                return refuse("settings-record", recorded)

    registered = run_step(by_name["register"])
    payload = registered.get("payload") or {}
    relationship = payload.get("relationshipId") or (payload.get("relationship") or {}).get("id")
    if not registered.get("ok") or not relationship:
        return refuse("register", registered)
    resolved[TRIAL_RELATIONSHIP] = str(relationship)

    # generation-open declares --dispatch-request-id required, and replaying the SAME id the
    # registration used returns the generation it already opened rather than opening another.
    opened = run_step(by_name["generation-open"])
    payload = opened.get("payload") or {}
    generation = payload.get("executionGeneration")
    if generation is None:
        generation = (payload.get("generation") or {}).get("executionGeneration")
    if not opened.get("ok") or generation is None:
        return refuse("generation-open", opened)
    resolved[TRIAL_GENERATION] = str(generation)

    bound = run_step(by_name["generation-bind"])
    if not bound.get("ok"):
        return refuse("generation-bind", bound)

    admitted = run_step(by_name["admit-turn"])
    if not admitted.get("ok"):
        return refuse("admit-turn", admitted)

    emitted = run_step(by_name["emit"])
    payload = emitted.get("payload") or {}
    receipt = payload.get("receipt") or {}
    event = receipt.get("eventId") or payload.get("eventId") or receipt.get("id")
    if not emitted.get("ok") or not event:
        return refuse("emit", emitted)
    resolved[TRIAL_EVENT] = str(event)

    delivered = run_step(by_name["deliver"])
    payload = delivered.get("payload") or {}
    attempt = payload.get("attempt") or {}
    turn = attempt.get("turnId") or (attempt.get("turn") or {}).get("id")
    if delivered.get("ok") and turn:
        return check.field(
            "verified",
            "assignment lookup before any write: " + json.dumps(assignment)[:300]
            + ". The delivery attempt returned turn id " + str(turn) + " for event " + str(event)
            + ", relationship " + str(relationship) + ", generation " + str(generation)
            + ". Recipient " + str(args.recipient) + ". Steps: " + json.dumps(performed),
            command=json.dumps(performed[-1]["command"]),
            acting_process=acting_process(), measured_at=now(),
        )
    return check.field(
        "not_verified",
        "the delivery attempt recorded no returned turn id. A dispatch, a staged receipt or an"
        " absent error does not establish this field. Attempt: " + json.dumps(attempt)[:400]
        + ". Steps: " + json.dumps(performed),
        command=json.dumps(performed[-1]["command"]),
        acting_process=acting_process(), measured_at=now(),
    )


# ------------------------------------------------------------------------- hook

def cmd_hook(args):
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    path = codex_home / "hooks.json"
    hook = {"type": "command", "command": args.hook_command, "timeout": args.timeout}
    result = hooks.install(path, args.event, hook, issue=args.issue, apply=args.apply)
    emit({
        "command": "hook",
        "hookFile": str(path),
        "result": result,
        "note": (
            "Installed, enabled and observed to have fired are three separate claims. This"
            " command appends and reads back; it never enables a daemon and never reports"
            " activation. Removing a hook renumbers later identities, so removal is refused."
        ),
    })
    return EXIT_OK if result["outcome"] in ("LINKED", "CREATED", "MISSING") else EXIT_REFUSED


# ------------------------------------------------------------------------- install

def cmd_install(args):
    data = definition.load()
    findings = definition.verify(ROOT)
    if findings:
        emit({"command": "install", "refused": "the definition does not describe this checkout",
              "findings": findings})
        return EXIT_REFUSED

    interpreter = args.python or _find_interpreter(data)
    if not interpreter:
        emit({"command": "install", "refused": "no interpreter satisfying requires-python was found",
              "requiresPython": sorted({c["requiresPython"] for c in data["components"]}),
              "note": "the controller runs on " + ".".join(str(p) for p in sys.version_info[:3])
                      + " and never selects itself for a runtime that needs more"})
        return EXIT_REFUSED

    destination = Path(args.dest).expanduser().absolute()
    # Every component, not the first one. Derived from only the bridge, a relay-only change
    # produced the same directory name and the existence check then refused to install it.
    combined = hashlib.sha256(
        "".join(c["sourceDigest"] for c in data["components"]).encode()
    ).hexdigest()[:12]
    environment = destination / ("env-" + str(data["definitionVersion"]) + "-" + combined)
    record_path = Path(args.record) if args.record else hostrecord.record_path()
    record = hostrecord.load(record_path, data["definitionVersion"])
    if record is None:
        emit({"command": "install", "refused": "the host record exists but could not be read",
              "hostRecord": str(record_path),
              "note": "it is never replaced silently: it holds the only evidence of what was run here"})
        return EXIT_REFUSED

    plan = [
        {"step": "verify-definition", "outcome": "passed"},
        {"step": "resolve interpreter", "outcome": str(interpreter),
         "version": interpreter_version(interpreter)},
        {"step": "create environment", "target": str(environment)},
        {"step": "install packages", "from": [c["subdirectory"] for c in data["components"]]},
        {"step": "read imported locations back from the interpreter"},
        {"step": "measure the candidate", "note": "a qualifying OPS-1.3 point is required"},
        {"step": "promote the recorded pointer",
         "note": "only after the candidate is exercised; a candidate that imports but fails its"
                 " exercise stays unselected and the previous runtime remains selected"},
    ]
    if not args.apply:
        emit({"command": "install", "applied": False, "plan": plan, "environment": str(environment),
              "note": "nothing was written. Rerun with --apply to stage the installation."})
        return EXIT_OK

    previous = dict(record.get("selected") or {})
    performed = []

    def perform(name, argv, timeout=900):
        try:
            done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
        except (OSError, subprocess.SubprocessError) as error:
            performed.append({"step": name, "command": argv, "ok": False,
                              "detail": type(error).__name__ + ": " + error.__str__()})
            return False
        performed.append({"step": name, "command": argv, "ok": done.returncode == 0,
                          "exitCode": done.returncode,
                          "detail": (done.stderr or done.stdout).strip()[-600:] or None})
        return done.returncode == 0

    destination.mkdir(parents=True, exist_ok=True)
    # OPS-2.4's first measurement: what is selected now, before anything replaces it. Without
    # it, restoring the previous pointer after a failure reports a runtime as selected with no
    # current evidence about it.
    outgoing = _outgoing_runtime(record, data)
    record["outgoing"] = outgoing
    hostrecord.save(record_path, record)

    # Exclusive: this fails if the directory exists, which is what proves the run owns it and
    # may therefore remove it on failure. An exists() test before a separate create does not.
    try:
        environment.mkdir()
    except FileExistsError:
        emit({"command": "install", "refused": "the environment directory already exists",
              "environment": str(environment), "plan": plan,
              "note": "an existing environment is never overwritten, and a run only removes a"
                      " directory it created itself"})
        return EXIT_REFUSED
    owned = environment

    if not perform("create environment", [str(interpreter), "-m", "venv", str(environment)]):
        return _install_failed(record_path, record, previous, performed, environment, owned)

    python = environment / "bin" / "python"
    packages = [str(ROOT / c["subdirectory"]) for c in data["components"]]
    if not perform("install packages", [str(python), "-m", "pip", "install", "--quiet", *packages]):
        return _install_failed(record_path, record, previous, performed, environment, owned)

    version = interpreter_version(python)
    installs = {}
    for component in data["components"]:
        location, error, _argv = module_location(python, component["module"])
        if not location:
            performed.append({"step": "read imported location", "component": component["component"],
                              "ok": False, "detail": error})
            return _install_failed(record_path, record, previous, performed, environment, owned)
        digest = definition.ops12_digest(location)
        install = {
            "location": location,
            # Read back from the interpreter: an editable install leaves nothing under
            # site-packages and a copied one does, so the mode follows the location.
            "installMode": "copied" if str(environment) in location else "editable",
            "entryPoint": str(environment / "bin" / component["consoleScript"]),
            "environment": str(environment),
            "interpreter": version,
            "integrity": digest,
            "digestMatchesDefinition": digest == component["sourceDigest"],
            "reachedVia": "installed by runtime_install.py into " + str(destination),
        }
        entry = hostrecord.component(record, component["component"])
        entry["repositoryCommit"] = definition.git(["rev-parse", "HEAD"], ROOT)
        entry["repositoryTree"] = definition.git(["rev-parse", "HEAD^{tree}"], ROOT)
        entry["subdirectoryTree"] = definition.git(
            ["rev-parse", "HEAD:" + component["subdirectory"]], ROOT)
        entry["workingTreeClean"] = definition.working_tree_clean(ROOT)
        hostrecord.put_install(record, component["component"], install)
        installs[component["component"]] = install
        performed.append({"step": "read imported location", "component": component["component"],
                          "ok": True, "location": location,
                          "digestMatchesDefinition": install["digestMatchesDefinition"]})

    hostrecord.save(record_path, record)
    measurement = measure_candidate(data, record, python=python, environment=environment,
                                    socket_path=args.socket, state=args.state,
                                    relay_command=str(environment / "bin" / "codex-session-relay"),
                                    measured_by=args.issue)
    if not measurement["qualifyingPoint"]:
        # The candidate imports but does not work. Release the destination the same way any
        # other failure does, so a transient connection failure does not block every retry.
        performed.append({"step": "measure the candidate", "ok": False,
                          "detail": measurement.get("refused")
                          or "the candidate was not exercised successfully"})
        return _install_failed(record_path, record, previous, performed, environment, owned)

    record.setdefault("selected", {})
    for name, install in installs.items():
        record["selected"][name] = install["location"]
    hostrecord.save(record_path, record)

    emit({
        "command": "install", "applied": True, "environment": str(environment),
        "hostRecord": str(record_path), "steps": performed, "installs": installs,
        "measurement": measurement,
        "promoted": bool(measurement["qualifyingPoint"]),
        "selected": record.get("selected") or {},
        "previousSelection": previous,
        "note": (
            "the pointer moves only after a qualifying point exists for the candidate"
            " (OPS-2.4). A candidate that imports but fails its exercise stays unselected and"
            " the previous runtime remains selected. Nothing here removes, moves or recreates"
            " the store."
        ),
    })
    return EXIT_OK if measurement["qualifyingPoint"] else EXIT_REFUSED


def _outgoing_runtime(record, data):
    """What is selected right now, and whether its bytes are still what was recorded."""
    reading = {}
    selected = record.get("selected") or {}
    for component in data["components"]:
        location = selected.get(component["component"])
        if not location:
            reading[component["component"]] = {"selected": None}
            continue
        present = Path(location).is_dir()
        reading[component["component"]] = {
            "selected": location,
            "present": present,
            "digest": definition.ops12_digest(location) if present else None,
        }
    return reading


def _install_failed(record_path, record, previous, performed, environment, owned=None):
    """Restore the previous selection and release a destination this run created.

    Removing only a directory this run created is what makes the retry work without ever
    touching an environment somebody else owns; the exclusive mkdir above is the proof of
    that ownership. The store is never removed, moved or recreated: update failure and store
    loss are different accidents.
    """
    record["selected"] = previous
    removed = None
    if owned is not None and Path(owned).is_dir():
        shutil.rmtree(str(owned), ignore_errors=True)
        removed = str(owned)
    for component in list((record.get("components") or {})):
        entry = record["components"][component]
        entry["installs"] = [i for i in entry.get("installs", [])
                             if i.get("environment") != str(environment)]
    hostrecord.save(record_path, record)
    emit({"command": "install", "applied": False, "steps": performed,
          "environment": str(environment), "selected": previous,
          "removedCandidate": removed,
          "refused": "a step failed; the previously selected runtime remains selected",
          "note": "the candidate this run created was removed so the destination can be"
                  " retried, and the records for it were dropped. The store is untouched."})
    return EXIT_REFUSED


def _find_interpreter(data):
    minimum = (3, 11)
    for candidate in ("python3.13", "python3.12", "python3.11", "python3"):
        found = shutil.which(candidate)
        if not found:
            continue
        version = interpreter_version(found)
        if not version:
            continue
        parts = tuple(int(p) for p in version.split(".")[:2])
        if parts >= minimum:
            return found
    return None


# ------------------------------------------------------------------------- measure

def _interpreter_prefix(python):
    """The environment an interpreter reports for itself."""
    try:
        done = subprocess.run([str(python), "-c", "import sys;print(sys.prefix)"],
                              capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() or None if done.returncode == 0 else None


def _bind_installs(record, data, python, environment):
    """Match each module's imported location to the install recorded for this environment.

    Returns (bound, refusal). 'bound' maps a component name to its recorded install and the
    digest of the bytes as they are right now, which is what the point will record.
    """
    bound = {}
    for component in data["components"]:
        entry = (record.get("components", {}).get(component["component"]) or {})
        installs = [i for i in entry.get("installs", [])
                    if i.get("environment") == str(environment)]
        if not installs:
            return None, ("no install of " + component["component"] + " is recorded for "
                          + str(environment) + ", so a run there could not be attributed")
        install = installs[0]
        location, error, _argv = module_location(python, component["module"])
        if not location:
            return None, (component["component"] + " could not be imported by " + str(python)
                          + ": " + str(error))
        if Path(location).resolve() != Path(install["location"]).resolve():
            return None, (component["component"] + " imported from " + location
                          + " but the recorded install for this environment is "
                          + install["location"])
        digest = definition.ops12_digest(location)
        bound[component["component"]] = {"install": install, "digest": digest,
                                         "location": location}
    return bound, None


def measure_candidate(data, record, *, python, environment, socket_path, state,
                      relay_command, measured_by=None):
    """Exercise both components and record a point only if both actually ran.

    A point means the combination was exercised (OPS-1.3). Starting a process is not that:
    the bridge's entry point starts a stdio server and never contacts the App Server, so a
    startup-based recipe would record success against an unreachable host. The relay is
    exercised by a doctor whose socketConnect is a real connect, and the bridge by its own
    read-only smoke check, which starts the MCP server, lists its tools and calls
    get_capabilities. Any connection, protocol or tool-call failure records no point.
    """
    bridge = next(c for c in data["components"] if c["component"] == "codex-thread-bridge")
    relay_component = next(c for c in data["components"] if c["component"] == "codex-session-relay")
    operations = []

    # Bind the run to the environment before anything is exercised or recorded. Two checks,
    # because neither alone is enough. The interpreter reports its own prefix, which is the
    # only thing that identifies which environment is running: a virtual environment's
    # bin/python legitimately resolves to an interpreter outside it, so a path test would
    # reject valid environments. And each module's imported location must equal the location
    # recorded for that environment's install, which is what accommodates an editable
    # install whose location sits outside its environment by design (OPS-1.1). Without the
    # first, two environments sharing one editable source are indistinguishable.
    prefix = _interpreter_prefix(python)
    if prefix is None:
        return {"operations": [], "qualifyingPoint": False, "appServer": None, "toolsListed": [],
                "refused": "the interpreter did not report its prefix, so the environment it"
                           " runs cannot be identified"}
    if Path(prefix).resolve() != Path(environment).resolve():
        return {"operations": [], "qualifyingPoint": False, "appServer": None, "toolsListed": [],
                "refused": "the interpreter reports prefix " + str(prefix) + " but the selected"
                           " environment is " + str(environment) + ", so this run would exercise"
                           " one runtime and record the point against another"}

    bound, mismatch = _bind_installs(record, data, python, environment)
    if mismatch:
        return {"operations": [], "qualifyingPoint": False, "appServer": None, "toolsListed": [],
                "refused": mismatch}

    reading = scope.relay(["doctor"], executable=relay_command, socket=socket_path, state=state)
    connect = ((reading.get("payload") or {}).get("actorReachability") or {}).get("socketConnect")
    operations.append({
        "component": relay_component["component"], "command": reading.get("command"),
        "exercised": connect == "ok",
        "detail": "actorReachability.socketConnect = " + repr(connect)
                  + "; a real connect is what makes this an exercise rather than a file read",
    })

    script = ROOT / bridge["exerciseScript"]
    argv = [str(python), str(script)]
    if socket_path:
        argv += ["--socket", str(socket_path)]
    tools_listed, app_server = [], None
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=180)
        payload = json.loads(done.stdout) if done.stdout.strip() else {}
        tools_listed = payload.get("tools") or []
        app_server = json.dumps(payload.get("connection")) if payload.get("connection") else None
        exercised = done.returncode == 0 and bridge["identityTool"] in tools_listed
        detail = ("listed " + str(len(tools_listed)) + " tools and called "
                  + bridge["identityTool"]) if exercised else (
            (done.stderr or done.stdout).strip()[-500:] or "the smoke check did not succeed")
    except (OSError, subprocess.SubprocessError, ValueError) as error:
        exercised, detail = False, type(error).__name__ + ": " + error.__str__()
    operations.append({
        "component": bridge["component"], "command": argv, "exercised": exercised,
        "toolsListed": tools_listed, "detail": detail,
    })

    qualifying = all(op["exercised"] for op in operations)
    if qualifying:
        for component in data["components"]:
            install = bound[component["component"]]["install"]
            hostrecord.add_point(record, component["component"], {
                "interpreter": interpreter_version(python),
                "codexCli": codex_cli_version(),
                "appServer": app_server,
                "host": socket.gethostname(),
                "date": now(),
                "measuredBy": measured_by or "JUN-104",
                "method": "; ".join(
                    " ".join(str(part) for part in (op.get("command") or [])) for op in operations
                ),
                "exercised": True,
                "install": install.get("location"),
                # The digest of what was exercised, measured now, not the digest the
                # definition expects. A point has to describe the bytes that ran.
                "installDigest": bound[component["component"]]["digest"],
                "definitionDigest": component["sourceDigest"],
                "digestMatchesDefinition":
                    bound[component["component"]]["digest"] == component["sourceDigest"],
            })
    return {"operations": operations, "qualifyingPoint": qualifying,
            "appServer": app_server, "toolsListed": tools_listed}


def cmd_measure(args):
    data = definition.load()
    record_path = Path(args.record) if args.record else hostrecord.record_path()
    record = hostrecord.load(record_path, data["definitionVersion"])
    if record is None:
        emit({"command": "measure", "refused": "the host record exists but could not be read",
              "hostRecord": str(record_path)})
        return EXIT_REFUSED

    relay_component = next(c for c in data["components"] if c["component"] == "codex-session-relay")
    relay_command = args.relay_command or shutil.which(relay_component["consoleScript"])
    python = args.python or sys.executable
    # Derived from what the interpreter reports, not from its executable's parent: a virtual
    # environment's bin/python commonly resolves into the base installation, and that path
    # names the wrong environment.
    environment = args.environment or _interpreter_prefix(python)
    if not environment:
        emit({"command": "measure", "refused": "the interpreter did not report a prefix, so no"
              " environment could be selected; pass --environment"})
        return EXIT_REFUSED

    if not relay_command:
        emit({"command": "measure", "refused": "no relay executable was found"})
        return EXIT_REFUSED

    measurement = measure_candidate(data, record, python=python, environment=environment,
                                    socket_path=args.socket, state=args.state,
                                    relay_command=relay_command, measured_by=args.issue)
    if measurement["qualifyingPoint"]:
        hostrecord.save(record_path, record)
    emit({
        "command": "measure", "hostRecord": str(record_path),
        "recorded": bool(measurement["qualifyingPoint"]),
        **measurement,
        "note": (
            "A point requires the combination to be EXERCISED (OPS-1.3). A connection,"
            " protocol or tool-call failure records no point, and reading bytes never"
            " produces one."
        ),
    })
    return EXIT_OK if measurement["qualifyingPoint"] else EXIT_REFUSED


# ------------------------------------------------------------------------- register-mcp

def cmd_register_mcp(args):
    """Register the bridge through the supported Codex configuration path.

    Append-only and idempotent: an identical registration writes nothing, an absent one is
    appended at the end, and a different command or argument list is reported and refused.
    Every other table in the file is preserved, which is checked by comparing the bytes
    outside the appended block rather than asserted.
    """
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    path, before = read_config(codex_home)
    new_text, outcome, detail = codexconfig.register(
        before, args.name, args.bridge_command, args.bridge_arg or [],
    )
    wrote = False
    if args.apply and outcome == "CREATED":
        # Held under one lock for the whole read-modify-write, re-read immediately before
        # replacing, and written by temp file and replace. That coordinates runs of this
        # command with each other and removes truncation. It cannot coordinate with an editor
        # that does not take the same lock, and this is not called compare-and-swap for that
        # reason: a writer ignoring the lock can still land in the remaining window.
        try:
            with hostrecord.Locked(path):
                current = path.read_text(encoding="utf-8") if path.is_file() else ""
                if current != before:
                    emit({"command": "register-mcp", "path": str(path), "outcome": "CHANGED",
                          "detail": "config.toml changed after it was read, so nothing was"
                                    " written; rerun against the file as it now stands",
                          "applied": False, "otherTablesPreserved": True})
                    return EXIT_REFUSED
                fresh, outcome, detail = codexconfig.register(
                    current, args.name, args.bridge_command, args.bridge_arg or [],
                )
                if outcome == "CREATED":
                    hostrecord.atomic_write(path, fresh)
                    new_text, wrote = fresh, True
        except TimeoutError as error:
            emit({"command": "register-mcp", "path": str(path), "outcome": "BUSY",
                  "detail": str(error), "applied": False, "otherTablesPreserved": True})
            return EXIT_REFUSED

    after = path.read_text(encoding="utf-8") if path.is_file() else ""
    preserved = after.startswith(before) if wrote else after == before
    view = codexconfig.scan(after)
    emit({
        "command": "register-mcp",
        "path": str(path),
        "outcome": outcome,
        "detail": detail,
        "applied": wrote,
        "otherTablesPreserved": preserved,
        "preservedHow": (
            "the prior content is a byte-exact prefix of the new file, so nothing before the"
            " appended table was rewritten" if wrote else "nothing was written"
        ),
        "serversNow": sorted(view.servers) if view.readable else None,
        "unreadable": view.unreadable or None,
    })
    if outcome in ("CONFLICT", "UNREADABLE"):
        return EXIT_REFUSED
    return EXIT_OK


# ------------------------------------------------------------------------- wiring

def build_parser():
    parser = argparse.ArgumentParser(prog="runtime_install.py", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("verify-definition").set_defaults(handler=cmd_verify_definition)

    diagnose = sub.add_parser("diagnose")
    diagnose.add_argument("--codex-home")
    diagnose.add_argument("--record")
    diagnose.add_argument("--socket")
    diagnose.add_argument("--state")
    diagnose.add_argument("--issue")
    diagnose.add_argument("--relay-command")
    diagnose.add_argument("--bridge-command")
    diagnose.add_argument("--bridge-arg", action="append")
    diagnose.add_argument("--observed-tool", action="append",
                          help="a tool name actually listed in a live session")
    diagnose.add_argument("--trial", action="store_true",
                          help="the only mode that creates work; never implied by another flag")
    diagnose.add_argument("--parent-task", help="trial input: the registering parent task")
    diagnose.add_argument("--child-task", help="trial input: the child task")
    diagnose.add_argument("--recipient", help="trial input: the authorized recipient")
    diagnose.add_argument("--artifact-root", help="trial input: the artifact root")
    diagnose.add_argument("--turn-thread", help="trial input: the observed turn thread")
    diagnose.add_argument("--turn-id", help="trial input: the observed turn id")
    diagnose.add_argument("--artifact", action="append",
                          help="trial input: a deliverable for the reviewable receipt")
    diagnose.add_argument("--dispatch-turn-id",
                          help="trial input: the parent turn the generation binds to")
    diagnose.add_argument("--recipient-settings",
                          help="trial input: the recipient's authorized settings, JSON or @path")
    diagnose.add_argument("--settings-already-recorded", action="store_true",
                          help="trial input: the caller's own unverified claim that the"
                               " recipient's authorized settings are already recorded")
    diagnose.add_argument("--expect-relationship",
                          help="the relationship the caller expects the store to hold")
    diagnose.add_argument("--assignment-lookup", action="store_true",
                          help="run assignment-find and nothing else; it constructs a"
                               " store, which is why plain diagnose does not")
    diagnose.add_argument("--turn-status", default="completed",
                          help="trial input: the status the child turn was observed in")
    diagnose.add_argument("--temporary", action="store_true",
                          help="record that this destination is temporary, not a host")
    diagnose.set_defaults(handler=cmd_diagnose)

    install = sub.add_parser("install")
    install.add_argument("--dest", required=True)
    install.add_argument("--python")
    install.add_argument("--record")
    install.add_argument("--socket")
    install.add_argument("--state")
    install.add_argument("--issue", default="JUN-104")
    install.add_argument("--apply", action="store_true")
    install.set_defaults(handler=cmd_install)

    measure = sub.add_parser("measure")
    measure.add_argument("--socket")
    measure.add_argument("--state")
    measure.add_argument("--relay-command")
    measure.add_argument("--python")
    measure.add_argument("--environment")
    measure.add_argument("--record")
    measure.add_argument("--issue", default="JUN-104")
    measure.set_defaults(handler=cmd_measure)

    register = sub.add_parser("register-mcp")
    register.add_argument("--codex-home")
    register.add_argument("--name", default=MCP_NAME)
    register.add_argument("--bridge-command", required=True)
    register.add_argument("--bridge-arg", action="append")
    register.add_argument("--apply", action="store_true")
    register.set_defaults(handler=cmd_register_mcp)

    hook = sub.add_parser("hook")
    hook.add_argument("--codex-home")
    hook.add_argument("--event", default="SessionStart")
    hook.add_argument("--hook-command", required=True)
    hook.add_argument("--timeout", type=int, default=10)
    hook.add_argument("--issue", default="JUN-104")
    hook.add_argument("--apply", action="store_true")
    hook.set_defaults(handler=cmd_hook)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    return args.handler(args)


if __name__ == "__main__":
    raise SystemExit(main())

