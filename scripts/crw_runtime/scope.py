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

STATE_ENV = "CODEX_SESSION_RELAY_STATE"


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
    discovery = (readings.get("discovery") or {}).get("payload") or {}
    siblings = discovery.get("siblingStores") or {}
    selected = (readings.get("selected") or {}).get("payload") or {}

    for payload, how in ((discovery, "discovery"), (selected, "explicit --state")):
        path = state_directory(payload)
        if path:
            seen.append({"path": path, "foundBy": how})
    for path in siblings.get("withoutProvenance") or []:
        seen.append({"path": path, "foundBy": "discovery: records no socket"})
    for path in siblings.get("claimingThisSocket") or []:
        seen.append({"path": path, "foundBy": "discovery: claims this socket"})
    if (readings.get("rootCandidate") or {}).get("payload"):
        seen.append({"path": str(root),
                     "foundBy": "targeted: a database at the root of the state home,"
                                " which discovery never enumerates"})

    unique = []
    for entry in seen:
        if entry["path"] not in [u["path"] for u in unique]:
            unique.append(entry)
    return unique


def summarise(readings, *, issue=None, env=None, service=None):
    """What the relay reported, kept honest about what was not checked."""
    discovery = (readings.get("discovery") or {}).get("payload") or {}
    selected = (readings.get("selected") or {}).get("payload") or {}
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
        "assignmentFind": (
            "not run: no --issue was supplied" if not issue else "run separately; see assignment"
        ),
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

