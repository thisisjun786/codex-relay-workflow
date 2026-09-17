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
import json
import os
import shutil
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
    try:
        done = subprocess.run([str(python), "-c", code], capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return None, type(error).__name__ + ": " + error.__str__()
    if done.returncode != 0:
        return None, (done.stderr.strip().splitlines() or ["import failed"])[-1]
    return done.stdout.strip(), None


def classify_component(component, *, record, entry_override=None, registration=None):
    """Gather the four OPS-2.1 signals and classify."""
    unreadable = []
    entry = resolve_entry_point(component["consoleScript"], entry_override)
    resolved = entry.resolve() if entry and entry.exists() else None

    recorded_roots = [ROOT.resolve()]
    entry_recorded = bool(resolved and any(
        str(resolved).startswith(str(r)) for r in recorded_roots
    ))
    shebang = interpreter_of(resolved) if resolved else None
    if resolved and not entry_recorded and shebang:
        entry_recorded = any(str(Path(shebang).resolve()).startswith(str(r)) for r in recorded_roots)

    python = shebang or (sys.executable if resolved is None else shebang)
    version = interpreter_version(python) if python else None
    location, import_error = (None, "no interpreter to ask")
    if python:
        location, import_error = module_location(python, component["module"])

    digest_matches = None
    if location and Path(location).is_dir():
        digest_matches = definition.ops12_digest(location) == component["sourceDigest"]
    elif resolved is not None:
        unreadable.append("the installed package location for " + component["component"])

    commit_matches = tree_matches = None
    if entry_recorded:
        commit_matches = definition.git(["rev-parse", "HEAD:" + component["subdirectory"]], ROOT) \
            == component["subdirectoryTree"]
        tree_matches = commit_matches
    clean = definition.working_tree_clean(ROOT) if entry_recorded else None
    if entry_recorded and clean is None:
        unreadable.append("the working tree cleanliness of " + str(ROOT))

    points = []
    if record is not None and location and version:
        points = hostrecord.points_for(
            record, component["component"], location=location,
            interpreter=version, source_digest=component["sourceDigest"],
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
        "digestMatches": digest_matches,
        "measuredPoints": len(points),
        "reusable": ownership.reusable(classification),
    }


# ------------------------------------------------------------------------- diagnose

def read_config(codex_home):
    path = Path(codex_home) / "config.toml"
    return path, (path.read_text(encoding="utf-8") if path.is_file() else "")


def registration_state(codex_home, command, args):
    path, text = read_config(codex_home)
    new_text, outcome, detail = codexconfig.register(text, MCP_NAME, command, args)
    return {"path": str(path), "outcome": outcome, "detail": detail, "wouldWrite": new_text != text}


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

    installed_class = classes["codex-session-relay"]["class"]
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
            command="python -c 'import <module>; print(__file__)'",
            acting_process=acting_process(), measured_at=now(),
        ),
        "mcpExposed": _mcp_exposed(registration, args.observed_tool),
        "connected": check.field(
            "verified" if connect == "ok" else ("not_verified" if connect else "unknown"),
            "doctor actorReachability.socketConnect = " + repr(connect)
            + ". A socket file existing on disk does not establish this.",
            command="codex-session-relay --socket <sock> doctor",
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
    registered = registration["outcome"] == "LINKED"
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


def _trial(args, relay_executable):
    if not relay_executable:
        return check.field("not_verified", "trial requested but no relay executable was found",
                           acting_process=acting_process())
    return check.field(
        "not_verified",
        "trial mode requires an authorized recipient, a registered relationship and a bounded"
        " deliver for one event; emit alone only stores the receipt and enqueues it, so an"
        " emitted receipt's own turn id never satisfies this field. Supply --issue,"
        " --recipient and a reachable App Server.",
        command="codex-session-relay ... register, emit, then deliver --event <id>",
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
    environment = destination / ("env-" + str(data["definitionVersion"]) + "-"
                                 + data["components"][0]["sourceDigest"][:12])
    steps = [
        {"step": "verify-definition", "outcome": "passed"},
        {"step": "resolve interpreter", "outcome": str(interpreter),
         "version": interpreter_version(interpreter)},
        {"step": "create environment", "target": str(environment),
         "outcome": "exists already, refused" if environment.exists() else "would create a new directory"},
        {"step": "install packages", "from": [c["subdirectory"] for c in data["components"]]},
        {"step": "read imported locations back from the interpreter"},
        {"step": "measure the candidate", "note": "a qualifying OPS-1.3 point is required"},
        {"step": "promote the recorded pointer",
         "note": "only after the candidate is exercised; a candidate that imports but fails its"
                 " exercise stays unselected and the previous runtime remains selected"},
    ]
    if environment.exists():
        emit({"command": "install", "refused": "the environment directory already exists",
              "environment": str(environment), "plan": steps,
              "note": "an existing environment is never overwritten"})
        return EXIT_REFUSED
    if not args.apply:
        emit({"command": "install", "applied": False, "plan": steps, "environment": str(environment),
              "note": "nothing was written. Rerun with --apply to stage the installation."})
        return EXIT_OK

    emit({"command": "install", "applied": False, "plan": steps, "environment": str(environment),
          "refused": "applying an installation needs network access to resolve the bridge's"
                     " own dependencies, and this run is not authorized to change an installed"
                     " runtime. The staged plan above is what it would do.",
          "note": "store is never removed, moved or recreated by an install or a failed one"})
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

def cmd_measure(args):
    data = definition.load()
    relay_component = next(c for c in data["components"] if c["component"] == "codex-session-relay")
    executable = args.relay_command or shutil.which(relay_component["consoleScript"])
    operations = []

    if not executable:
        operations.append({"component": "codex-session-relay", "exercised": False,
                           "detail": "no relay executable was found"})
    else:
        reading = scope.relay(["doctor"], executable=executable, socket=args.socket, state=args.state)
        payload = reading.get("payload") or {}
        connect = ((payload.get("actorReachability") or {}).get("socketConnect"))
        operations.append({
            "component": "codex-session-relay", "command": reading.get("command"),
            "exercised": connect == "ok",
            "detail": "actorReachability.socketConnect = " + repr(connect)
                      + "; a real connect is what makes this an exercise rather than a file read",
        })

    bridge_component = next(
        c for c in data["components"] if c["component"] == "codex-thread-bridge"
    )
    script = ROOT / bridge_component["exerciseScript"]
    operations.append({
        "component": "codex-thread-bridge", "command": [str(args.python or sys.executable), str(script),
                                                        "--socket", str(args.socket or "")],
        "exercised": False,
        "detail": "the package's own read-only smoke check starts the MCP server, lists its tools"
                  " and calls get_capabilities, an App Server round trip. It was not run here"
                  " because this command is not authorized to exercise the installed runtime.",
    })

    qualifying = all(op["exercised"] for op in operations)
    emit({
        "command": "measure",
        "operations": operations,
        "qualifyingPoint": qualifying,
        "note": (
            "A point requires the combination to be EXERCISED (OPS-1.3). Starting a process is"
            " not enough: the bridge's entry point starts a stdio server and never contacts the"
            " App Server. A connection, protocol or tool-call failure records no point."
        ),
    })
    return EXIT_OK if qualifying else EXIT_REFUSED


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
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(new_text, encoding="utf-8")
        wrote = True

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
    diagnose.add_argument("--temporary", action="store_true",
                          help="record that this destination is temporary, not a host")
    diagnose.set_defaults(handler=cmd_diagnose)

    install = sub.add_parser("install")
    install.add_argument("--dest", required=True)
    install.add_argument("--python")
    install.add_argument("--apply", action="store_true")
    install.set_defaults(handler=cmd_install)

    measure = sub.add_parser("measure")
    measure.add_argument("--socket")
    measure.add_argument("--state")
    measure.add_argument("--relay-command")
    measure.add_argument("--python")
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

