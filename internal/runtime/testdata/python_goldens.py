#!/usr/bin/env python3
"""Capture scripts/crw_runtime's answers for internal/runtime's Go tests (dev-only).

Run from the repository root: python3 internal/runtime/testdata/python_goldens.py > internal/runtime/testdata/goldens.json

Every case is deterministic: no clock, pid, hostname or filesystem outside a temporary
directory reaches an answer, so the committed goldens are the Python implementation's answers
and the parity-tagged Go test regenerates them to prove they still are.
"""

import hashlib
import itertools
import json
import os
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import hostrecord, ownership, reading, scope, staging, swapgate  # noqa: E402

FIXTURE = ROOT / "internal" / "runtime" / "record" / "testdata" / "host-record-v1.json"


def dumps_indent():
    texts = [
        '{"b": [], "a": {}, "c": [1, {"z": null, "y": true}], "d": "\\u00e9\\ud83d\\ude00\\u007f\\u0001"}',
        '[1.0, 1e16, 1e-5, 0.1, 123456789012345678901234567890, -0, -0.0, 2.5e-7]',
        '{"k": NaN, "l": Infinity, "m": -Infinity}',
        '{"dup": 1, "dup": 2, "x": "\\ud800"}',
        '"plain"',
        '{"nested": {"deeper": {"deepest": [[], [[]], {}]}}}',
    ]
    return [{"input": text, "output": json.dumps(json.loads(text), indent=2, sort_keys=True) + "\n"}
            for text in texts]


GO_RELAY = {
    "binaryDigest": "a" * 64, "digestMatchesDefinition": True,
    "entryPoint": "/home/user/.local/share/crw-runtime/bin-0.3.0-aaaaaaaaaaaa/bin/codex-session-relay",
    "environment": "/home/user/.local/share/crw-runtime/bin-0.3.0-aaaaaaaaaaaa",
    "integrity": "a" * 64, "location": "/home/user/.local/share/crw-runtime/bin-0.3.0-aaaaaaaaaaaa/bin",
    "reachedVia": "crw install", "target": "linux/amd64",
    "source": {"repositoryCommit": "1" * 40, "repositoryTree": "2" * 40, "subdirectoryTree": "2" * 40,
               "workingTreeClean": True},
}
GO_BRIDGE = dict(GO_RELAY, entryPoint=GO_RELAY["entryPoint"].replace("codex-session-relay", "codex-thread-bridge"))
GO_POINT = {"exercised": True, "install": GO_RELAY["location"], "installDigest": "a" * 64,
            "codexCli": "codex-cli 0.154.0", "host": "host", "date": "2026-09-29T00:00:00Z",
            "digestMatchesDefinition": True, "method": "crw doctor", "measuredBy": "CRW-157"}
FIXTURE_POINTER = "/home/user/.local/share/crw-runtime/current"

UPDATES = {
    "promote": {"installs": [["codex-session-relay", GO_RELAY], ["codex-thread-bridge", GO_BRIDGE]],
                "points": [["codex-session-relay", GO_POINT]],
                "select": {"codex-session-relay": GO_RELAY["location"], "codex-thread-bridge": GO_BRIDGE["location"]},
                "pointer": {"path": FIXTURE_POINTER, "recordedAt": "2026-09-29T00:00:00Z", "recordedBy": "CRW-157"}},
    "reinstall-same-location": {"installs": [["codex-session-relay", dict(GO_RELAY, reachedVia="again")],
                                             ["codex-session-relay", dict(GO_RELAY, reachedVia="third")]]},
    "deselect-and-restore": {"deselect": {"codex-session-relay": "/home/user/.local/share/crw-runtime/env-1-0be23c258476/lib/python3.13/site-packages/codex_session_relay",
                                          "codex-thread-bridge": "/somewhere/else"},
                             "restore_pointer": {"wrote": FIXTURE_POINTER, "found": {"path": FIXTURE_POINTER}}},
    "drop-environment": {"drop_environment": "/home/user/.local/share/crw-runtime/env-1-4d296fa3657c"},
    "drop-pointer": {"drop_pointer": FIXTURE_POINTER},
    "drop-pointer-elsewhere": {"drop_pointer": "/elsewhere/current"},
    "nothing": {},
}


def record_updates():
    out = {}
    fixture = FIXTURE.read_bytes()
    with tempfile.TemporaryDirectory() as temporary:
        for name, delta in UPDATES.items():
            path = Path(temporary) / name / "host-record.json"
            path.parent.mkdir()
            path.write_bytes(fixture)
            kwargs = dict(delta)
            for key in ("installs", "points"):
                if key in kwargs:
                    kwargs[key] = [tuple(pair) for pair in kwargs[key]]
            found = hostrecord.update(path, 1, **kwargs)
            written = path.read_bytes()
            out[name] = {"delta": delta, "state": found.state, "sha256": hashlib.sha256(written).hexdigest(),
                         "length": len(written), "lockLeft": (path.parent / "host-record.json.crw-lock").exists()}
            if name == "promote":
                # The one update written out whole, so a reviewer can read what a Go install adds.
                out[name]["bytes"] = written.decode("utf-8")
        # A record absent: update starts from empty() and writes it, which names this host.
        # Only the shape is compared, so the generator records the key set.
        absent = Path(temporary) / "absent" / "host-record.json"
        hostrecord.update(absent, 1, select={"codex-session-relay": "/x"})
        written = json.loads(absent.read_text(encoding="utf-8"))
        out["absent"] = {"keys": sorted(written), "selected": written["selected"], "recordVersion": written["recordVersion"]}
        # An unreadable record is never replaced.
        broken = Path(temporary) / "broken" / "host-record.json"
        broken.parent.mkdir()
        broken.write_text("{ not json", encoding="utf-8")
        found = hostrecord.update(broken, 1, select={"codex-session-relay": "/x"})
        out["unreadable"] = {"state": found.state, "exception": found.exception, "bytes": broken.read_text(encoding="utf-8")}
    return out


def shapes():
    cases = {
        "list": [], "components-null": {"components": None}, "component-list": {"components": {"x": []}},
        "installs-null": {"components": {"x": {"installs": None}}}, "point-str": {"components": {"x": {"measuredPoints": ["p"]}}},
        "selected-list": {"selected": []}, "pointer-str": {"pointer": "p"}, "pointer-path-list": {"pointer": {"path": []}},
        "fine": {"components": {"x": {"installs": [{}]}}, "selected": None, "pointer": {"path": "/p"}},
    }
    out = {}
    for name, value in cases.items():
        try:
            hostrecord.shape(value)
            refusal = None
        except (TypeError, ValueError) as error:
            refusal = type(error).__name__ + ": " + str(error)
        out[name] = {"input": value, "refusal": refusal}
    return out


# The OPS-1.2 walk over a small tree, written the same way by the Go test.
DIGEST_TREE = {"a.py": b"a", "sub/b.py": b"b", "sub/__pycache__/junk.pyc": b"junk", "z/\u00e9.txt": b"\x00\xff"}


def ops12():
    from crw_runtime import definition
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary) / "pkg"
        for name, data in DIGEST_TREE.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        return {"tree": {k: v.decode("latin-1") for k, v in DIGEST_TREE.items()}, "digest": definition.ops12_digest(root)}


class Claim:
    def __init__(self, state, value=None, detail=None):
        self.state = state
        self.value = value
        self.detail = detail

    @property
    def usable(self):
        return self.state in reading.USABLE


def staging_decisions():
    claims = {
        "ABSENT": Claim(reading.ABSENT), "UNREADABLE": Claim(reading.UNREADABLE, detail="the claim detail"),
        "ACCESS_ERROR": Claim(reading.ACCESS_ERROR, detail="the claim detail"),
        "STAGING": Claim(reading.PRESENT, {"state": "STAGING"}), "COMPLETE": Claim(reading.PRESENT, {"state": "COMPLETE"}),
    }
    out = []
    for (claim, liveness, occupied, protected, selected) in itertools.product(
            claims, staging.LIVENESS, (None, True, False), (True, False), (True, False)):
        decision, reason = staging.decide(claims[claim], liveness, occupied=occupied,
                                          protected=protected, selected=selected)
        out.append({"claim": claim, "liveness": liveness, "occupied": occupied, "protected": protected,
                    "selected": selected, "decision": decision, "reason": reason})
    return out


def claim_payload():
    keys = sorted(staging.claim_payload(staging.STAGING))
    payload = staging.claim_payload(staging.STAGING, issue="CRW-157", run="42")
    payload.update({"pid": 4242, "host": "host", "writtenAt": "2026-09-29T00:00:00Z"})
    return {"keys": keys, "bytes": json.dumps(payload, indent=2, sort_keys=True) + "\n"}


def ownership_classes():
    out = []
    for recorded, digest, point, conflict, unreadable in itertools.product(
            (True, False), (None, True, False), (True, False), (None, "registration", "pointer"), (False, True)):
        signals = ownership.Signals(
            entry_point_recorded=recorded, digest_matches=digest, has_point=point,
            registration_conflict="registered elsewhere" if conflict == "registration" else None,
            pointer_conflict="the pointer names another runtime" if conflict == "pointer" else None,
            unreadable=["the host record"] if unreadable else None)
        answer, reasons = ownership.classify(signals)
        out.append({"recorded": recorded, "digest": digest, "point": point, "conflict": conflict,
                    "unreadable": unreadable, "class": answer, "reasons": reasons})
    return out


ENVELOPES = {
    "running": {"ok": True, "payload": {"running": True}, "command": ["relay", "service", "status"]},
    "stopped": {"ok": True, "payload": {"running": False}, "command": ["relay", "service", "status"]},
    "failed": {"ok": False, "unreadable": "no binary", "command": ["relay"]},
    "stderr": {"ok": False, "stderr": "boom", "command": ["relay"]},
    "bare-failure": {"ok": False},
    "no-payload": {"ok": True, "payload": None, "command": ["relay"]},
    "list-payload": {"ok": True, "payload": [1], "command": ["relay"]},
    "string-running": {"ok": True, "payload": {"running": "yes"}, "command": ["relay"]},
    "open-0": {"ok": True, "payload": {"contents": {"available": True, "openAttempts": 0}}, "command": ["relay", "doctor"]},
    "open-2": {"ok": True, "payload": {"contents": {"available": True, "openAttempts": 2}}, "command": ["relay", "doctor"]},
    "open-bool": {"ok": True, "payload": {"contents": {"available": True, "openAttempts": True}}, "command": ["relay", "doctor"]},
    "open-str": {"ok": True, "payload": {"contents": {"available": True, "openAttempts": "2"}}, "command": ["relay", "doctor"]},
    "unavailable": {"ok": True, "payload": {"contents": {"available": False, "detail": "not readable"}}, "command": ["relay", "doctor"]},
    "no-contents": {"ok": True, "payload": {}, "command": ["relay", "doctor"]},
}
PRESENCES = {
    "none": None,
    "absent": {"readable": True, "present": False, "dbPath": "/s/relay.sqlite3", "command": None},
    "present": {"readable": True, "present": True, "dbPath": "/s/relay.sqlite3", "command": None},
    "unreadable": {"readable": False, "present": None, "dbPath": "/s/relay.sqlite3", "detail": "PermissionError: denied", "command": None},
}
SAME = {"table a": "CREATE TABLE a (x TEXT)", "table b": "CREATE TABLE b (y TEXT)"}
SCHEMAS = {
    "same": {"readable": True, "present": True, "objects": SAME, "dbPath": "/d"},
    "narrow": {"readable": True, "present": True, "objects": {"table a": "CREATE TABLE a (x TEXT)"}, "dbPath": "/d"},
    "wide": {"readable": True, "present": True, "objects": dict(SAME, **{"index c": "CREATE INDEX c ON a (x)"}), "dbPath": "/d"},
    "differs": {"readable": True, "present": True, "objects": {"table a": "CREATE TABLE a (x TEXT, y INT)", "table b": "CREATE TABLE b (y TEXT)"}, "dbPath": "/d"},
    "spaced": {"readable": True, "present": True, "objects": {"table a": "CREATE  TABLE   a (x TEXT)", "table b": "CREATE TABLE b\n (y TEXT)"}, "dbPath": "/d"},
    "absent": {"readable": True, "present": False, "objects": None, "dbPath": "/d"},
    "unreadable": {"readable": False, "detail": "denied"},
    "names-only": {"readable": True, "present": True, "objects": ["table a"], "dbPath": "/d"},
    "no-detail": {"readable": False},
}
STATEMENTS = [
    "CREATE TABLE a (x TEXT DEFAULT 'A  B')", "CREATE   TABLE a\n\t(x TEXT)", "  leading and trailing  ",
    'CREATE TABLE "a  b" (x)', "CREATE TABLE [a  b] (x)", "CREATE TABLE `a  b` (x)", "x  y", "x\x1cy",
    "unterminated 'quote  here", "", None,
]


def raising(function, *args):
    """The answer, or the exception class Python raised instead of answering."""
    try:
        return function(*args)
    except Exception as error:  # noqa: BLE001 - recorded, and Go's answer is checked separately
        return {"raises": type(error).__name__}


def swap_gate():
    out = {"daemon": {}, "inflight": {}, "schema": {}, "normalised": [], "decide": []}
    for name, envelope in ENVELOPES.items():
        out["daemon"][name] = swapgate.daemon_cell(envelope)
        out["inflight"][name] = {p: raising(swapgate.inflight_cell, envelope, presence) for p, presence in PRESENCES.items()}
    out["serviceState"] = {name: scope.service_state(envelope) for name, envelope in ENVELOPES.items()}
    for store in SCHEMAS:
        for candidate in SCHEMAS:
            out["schema"][store + "|" + candidate] = raising(swapgate.schema_cell, SCHEMAS[store], SCHEMAS[candidate])
    out["normalised"] = [{"input": s, "output": swapgate._normalised(s)} for s in STATEMENTS]
    choices = {
        "daemon": ["running", "stopped", "failed", "missing"],
        "inFlight": ["open-0", "open-2", "unavailable", "missing"],
        "storeSchema": ["same|same", "narrow|same", "unreadable|same", "absent|same", "missing"],
    }
    for daemon, inflight, schema in itertools.product(*choices.values()):
        cells = {}
        if daemon != "missing":
            cells["daemon"] = out["daemon"][daemon]
        if inflight != "missing":
            cells["inFlight"] = out["inflight"][inflight]["none"]
        if schema != "missing":
            cells["storeSchema"] = out["schema"][schema]
        out["decide"].append({"choice": [daemon, inflight, schema], "answer": swapgate.decide(cells)})
    return out


def scope_readings():
    env = {"XDG_STATE_HOME": "/nonexistent-crw-test-root", "HOME": "/nonexistent-crw-test-home"}
    readings = {
        "discovery": {"ok": True, "command": ["relay", "doctor"], "payload": {
            "stateSelection": {"path": "/s/scope", "socketScope": "scope-1"},
            "siblingStores": {"checked": True, "withoutProvenance": ["/s/default"], "claimingThisSocket": [], "ambiguous": False},
            "store": {"dbPath": "/s/scope/relay.sqlite3", "storeId": "id-1"},
            "actorReachability": {"socketConnect": "ok", "stateDirectoryWritable": True},
            "contents": {"available": True, "relationships": 3}, "ledger": {"path": "/l"}}},
        "selected": {"ok": False, "skipped": "no store is explicitly selected"},
        "selectedVia": None,
        "rootCandidate": {"skipped": "no relay.sqlite3"},
    }
    explicit = dict(readings, selected={"ok": True, "command": ["relay", "--state", "/s/other", "doctor"],
                                        "payload": {"stateDirectory": "/s/other"}}, selectedVia="flag")
    failed = dict(readings, selected={"ok": False, "command": ["relay"], "unreadable": "boom"}, selectedVia="flag")
    return {
        "readings": {"discovery-only": readings, "explicit": explicit, "failed-selection": failed},
        "summaries": {name: scope.summarise(value, env=env, service={"state": "x"})
                      for name, value in (("discovery-only", readings), ("explicit", explicit), ("failed-selection", failed))},
        "siblings": [scope.sibling_reading(p) for p in ({"stateDirectory": "/s"}, {"siblingStores": None},
                                                         {"siblingStores": {"checked": False, "reason": "chosen"}},
                                                         {"siblingStores": {"checked": True}})],
        "env": env,
    }


def main():
    json.dump({
        "dumpsIndent": dumps_indent(),
        "recordUpdates": record_updates(),
        "shapes": shapes(),
        "ops12": ops12(),
        "stagingDecisions": staging_decisions(),
        "claimPayload": claim_payload(),
        "ownership": ownership_classes(),
        "swapGate": swap_gate(),
        "scope": scope_readings(),
    }, sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
