"""The operating scope, read from the relay rather than rediscovered.

OPS-3.1 puts one relay service and one durable store behind an entire operating scope: one
host, one OS user, one App Server. A second repository or project installs into that same
scope and reuses the same service and store. Nothing here creates a daemon or a store per
project, per repository or per parent.

The relay already owns store discovery, sibling detection and reachability, and its doctor
constructs no Store while answering. So this module invokes it and reports what it says. It
does not re-derive a socket hash, choose between stores, or interpret a database.
"""

import json
import os
import subprocess
from pathlib import Path

from . import reading

STATE_ENV = "CODEX_SESSION_RELAY_STATE"

RUNNING = "RUNNING"
STOPPED = "STOPPED"


def service_state(envelope):
    """Classify a service status reading. First match wins, and the invocation wins first.

    A failed invocation returns ok false with no payload, which satisfies both 'the command
    failed' and 'the payload is missing'. Order matters because those are different answers:
    a command that did not run says nothing about the daemon, and reading it as stopped would
    report an activation state nobody observed. Being unable to ask is never being told no.
    """
    if not isinstance(envelope, dict) or not envelope.get("ok"):
        detail = (envelope or {}).get("unreadable") or (envelope or {}).get("stderr")
        return {"state": reading.ACCESS_ERROR, "running": None,
                "detail": "the service could not be asked: " + str(detail or "the command failed")}
    payload = envelope.get("payload")
    if not isinstance(payload, dict):
        return {"state": reading.UNREADABLE, "running": None,
                "detail": "the service answered with no readable status object"}
    held = payload.get("running")
    if not isinstance(held, bool):
        return {"state": reading.UNREADABLE, "running": None,
                "detail": "the status carries no boolean 'running', found "
                          + type(held).__name__}
    return {"state": RUNNING if held else STOPPED, "running": held,
            "detail": "the service answered and reports itself "
                      + ("running" if held else "not running")}


def relay(command, *, executable, socket=None, state=None, env=None, discovery=False, timeout=60):
    """Run one relay command and return its parsed JSON with the invocation recorded.

    When discovery is asked for, CODEX_SESSION_RELAY_STATE is removed from the environment
    and no --state is passed. resolve_state_dir returns immediately on that override with
    source 'env', which suppresses sibling discovery exactly as --state does, so inheriting
    it would quietly produce an empty conflict inventory.
    """
    environment = dict(os.environ if env is None else env)
    argv = [str(executable)]
    if socket:
        argv += ["--socket", str(socket)]
    if discovery:
        environment.pop(STATE_ENV, None)
    elif state:
        argv += ["--state", str(state)]
        # Both selectors together (OPS-3.3): the flag alone moves the store while leaving
        # the adapter's operations ledger behind.
        environment[STATE_ENV] = str(state)
    argv += list(command)
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, env=environment)
    except (OSError, subprocess.SubprocessError) as error:
        return {"ok": False, "command": argv, "unreadable": type(error).__name__ + ": " + error.__str__()}
    try:
        payload = json.loads(done.stdout) if done.stdout.strip() else None
    except ValueError:
        payload = None
    return {
        "ok": done.returncode == 0,
        "exitCode": done.returncode,
        "command": argv,
        "payload": payload,
        "stderr": done.stderr.strip()[:2000] or None,
        "unreadable": None if payload is not None else "the relay did not return JSON",
    }


def default_state_root(env=None):
    env = os.environ if env is None else env
    base = env.get("XDG_STATE_HOME")
    root = Path(base).expanduser() if base else Path(env.get("HOME", "~")).expanduser() / ".local/state"
    return root / "codex-session-relay"


def survey(*, executable, socket, state=None, env=None):
    """Three doctor readings, because one call cannot answer all three questions.

    discovery  the only call whose siblingStores is populated
    selected   the store actually in use
    root       a relay.sqlite3 sitting at the root of the state home, which discovery never
               sees because it enumerates child directories only
    """
    root = default_state_root(env)
    readings = {
        "discovery": relay(["doctor"], executable=executable, socket=socket, env=env, discovery=True),
        "selected": relay(["doctor"], executable=executable, socket=socket, state=state, env=env)
        if state else {"ok": False, "skipped": "no explicit state directory was selected"},
    }
    if (root / "relay.sqlite3").is_file():
        readings["rootCandidate"] = relay(
            ["doctor"], executable=executable, socket=socket, state=root, env=env,
        )
    else:
        readings["rootCandidate"] = {"skipped": "no relay.sqlite3 at " + str(root)}
    return readings


def stores_seen(readings, env=None):
    """Every store this survey saw, with how it was found. None is adopted."""
    root = default_state_root(env)
    seen = []
    def usable(name):
        answer = readings.get(name) or {}
        return (answer.get("payload") or {}) if answer.get("ok") else {}

    discovery = usable("discovery")
    siblings = discovery.get("siblingStores") or {}
    selected = usable("selected")

    for payload, how in ((discovery, "discovery"), (selected, "explicit --state")):
        path = state_directory(payload)
        if path:
            seen.append({"path": path, "foundBy": how})
    for path in siblings.get("withoutProvenance") or []:
        seen.append({"path": path, "foundBy": "discovery: records no socket"})
    for path in siblings.get("claimingThisSocket") or []:
        seen.append({"path": path, "foundBy": "discovery: claims this socket"})
    if usable("rootCandidate"):
        seen.append({"path": str(root),
                     "foundBy": "targeted: a database at the root of the state home,"
                                " which discovery never enumerates"})

    for candidate in filesystem_candidates(env):
        seen.append({"path": candidate["path"], "database": candidate["database"],
                     "kind": candidate["kind"], "foundBy": candidate["foundBy"]})

    unique = []
    for entry in seen:
        match = next((u for u in unique if u["path"] == entry["path"]
                      and u.get("database") == entry.get("database")), None)
        if match is None:
            unique.append(entry)
        else:
            # Keep both provenances rather than letting one reading mask the other.
            if entry["foundBy"] not in match["foundBy"]:
                match["foundBy"] = match["foundBy"] + "; " + entry["foundBy"]
            for key in ("database", "kind"):
                match.setdefault(key, entry.get(key))
    return unique


def summarise(readings, *, issue=None, env=None, service=None):
    """What the relay reported, kept honest about what was not checked."""
    def usable(name):
        answer = readings.get(name) or {}
        # A structured refusal still parses as JSON. Preferring it would let a refusal hide
        # a discovery reading that actually answered.
        return (answer.get("payload") or {}) if answer.get("ok") else {}

    discovery = usable("discovery")
    selected = usable("selected")
    primary = selected or discovery
    siblings = discovery.get("siblingStores") or {}
    reachability = primary.get("actorReachability") or {}
    contents = primary.get("contents") or {}
    store = primary.get("store") or {}

    return {
        "scopeId": scope_id(primary),
        "scopeMeaning": "one host, one OS user, one App Server (OPS-3.1). Parents in different"
                        " repositories and different Linear projects share this one scope.",
        "stateDirectory": state_directory(primary),
        "databasePath": store.get("dbPath") or store.get("realPath"),
        "storeId": store.get("storeId"),
        "ledger": primary.get("ledger"),
        "socketConnect": reachability.get("socketConnect"),
        "stateDirectoryWritable": reachability.get("stateDirectoryWritable"),
        "serviceOwner": service,
        "storesSeen": stores_seen(readings, env),
        "siblingDiscovery": sibling_reading(discovery),
        "ambiguous": siblings.get("ambiguous"),
        "relationships": contents.get("relationships", primary.get("relationships")),
        "relationshipsMeaning": (
            "a count only. OPS-3.4 proof is assignment-find --issue returning the expected"
            " relationship from each participating process: a nonzero count from a different"
            " populated database would satisfy a count check while proving nothing"
        ),
        # Filled in by the caller when it actually ran one. Never claims a lookup happened.
        "assignmentFind": {"ran": False, "reason": "not run by this reading"},
        "perProjectDaemon": (
            "none created. One service and one store serve the whole operating scope (OPS-3.1),"
            " so a second repository or project reuses them rather than starting its own"
        ),
    }


def state_directory(payload):
    """Where a doctor payload says its store is, across the shapes different builds emit."""
    payload = payload or {}
    selection = payload.get("stateSelection") or {}
    return payload.get("stateDirectory") or selection.get("path")


def scope_id(payload):
    selection = (payload or {}).get("stateSelection") or {}
    return selection.get("socketScope")


def sibling_reading(payload):
    """Distinguish 'this build does not report siblings' from 'none were found'.

    A signal that cannot be read is not a signal that agrees (OPS-2.1). An installed relay
    older than the revision that added sibling reporting emits no siblingStores at all, and
    rendering that as an empty inventory would hide exactly the conflict this exists to find.
    """
    payload = payload or {}
    if "siblingStores" not in payload or payload.get("siblingStores") is None:
        return "not reported: this relay build does not emit siblingStores, so the conflict" \
               " inventory could not be read from it"
    siblings = payload["siblingStores"]
    if siblings.get("checked") is False:
        return "not checked: " + str(siblings.get("reason"))
    return "checked"


def filesystem_candidates(env=None):
    """Every relay database and operations ledger visible on disk, listed and not interpreted.

    This exists because the relay cannot always answer. An installed build older than the
    revision that added sibling reporting returns no siblingStores at all, and a host in that
    state would otherwise get an inventory that silently omits a real store. Listing files is
    not rediscovering anything: nothing here opens a database, chooses between candidates or
    decides which one serves a socket. That judgement stays with the relay, which is why each
    entry says it was listed rather than identified.
    """
    root = default_state_root(env)
    found = []
    if not root.is_dir():
        return found
    database = root / "relay.sqlite3"
    if database.is_file():
        found.append({"path": str(root), "database": str(database), "kind": "relay store",
                      "foundBy": "listed on disk at the root of the state home"})
    try:
        children = sorted(p for p in root.iterdir() if p.is_dir())
    except OSError:
        return found
    for child in children:
        if (child / "relay.sqlite3").is_file():
            found.append({"path": str(child), "database": str(child / "relay.sqlite3"),
                          "kind": "relay store", "foundBy": "listed on disk in a scope directory"})
        for ledger in sorted(child.glob("operations-*.sqlite3")):
            found.append({"path": str(child), "database": str(ledger),
                          "kind": "adapter operations ledger, selected differently from the store"
                                  " (OPS-3.3)",
                          "foundBy": "listed on disk in a scope directory"})
    return found
