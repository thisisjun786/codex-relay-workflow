"""Capture, once, the reason Python gives for every worker-policy fixture the Go tests build.

Not run by any test: its output, worker_reasons.json beside it, is the oracle the Go tests read,
so default CI never starts Python for these properties. Regenerate after a change to
service.read_worker_policy or rolepolicy.worker_readiness with

    cd packages/codex-session-relay && PYTHONDONTWRITEBYTECODE=1 \
        ../../.venv/bin/python ../../internal/relay/service/testdata/worker_reasons_capture.py

"observations" are live fixtures: the WorkerPolicyEvidence harness starts a real worker that
holds the daemon lock and its scope claim and publishes its receipt; each case changes one
thing, reads service.read_worker_policy(), and restores the fixture. "readiness" cases are the
pure rolepolicy.worker_readiness over a valid observation, a requirement list and the caller's
policy. Case names are the Go tables' names; the Go side builds the same change in its own tree.
"""
import copy
import json
import os
import signal
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.getcwd())

from codex_session_relay import rolepolicy  # noqa: E402
from codex_session_relay.service import DAEMON_LOCK, WORKER_POLICY  # noqa: E402

from tests.test_rolepolicy import CHILD_EFFORT, CHILD_MODEL, PARENT_EFFORT, PARENT_MODEL, POLICY  # noqa: E402
from tests.test_worker_policy import WorkerPolicyEvidence  # noqa: E402

OUT = Path(__file__).with_name("worker_reasons.json")
CAPTURED = {"observations": {}, "readiness": {}}


class Capture(WorkerPolicyEvidence):
    def test_capture(self):
        service = self.service()
        child = self.worker(service)
        state = service.selection.path
        receipt_path, record_path = state / WORKER_POLICY, state / "daemon.json"
        scope_json = service.scope.root / f"{service.scope.key(service.socket_path)}.json"
        scope_lock = service.scope.root / f"{service.scope.key(service.socket_path)}.lock"
        originals = {path: path.read_bytes() for path in (receipt_path, record_path, scope_json)}
        receipt, record = json.loads(originals[receipt_path]), json.loads(originals[record_path])

        def restore():
            for path, raw in originals.items():
                path.write_bytes(raw)

        def observe(name, change):
            change()
            try:
                answer = service.read_worker_policy()
                CAPTURED["observations"][name] = answer["reason"] if not answer["observed"] else None
            finally:
                restore()

        def receipt_with(edit):
            def change():
                changed = copy.deepcopy(receipt)
                edit(changed)
                receipt_path.write_text(json.dumps(changed))
            return change

        def record_with(edit):
            def change():
                changed = copy.deepcopy(record)
                edit(changed)
                record_path.write_text(json.dumps(changed, indent=2))
            return change

        def scope_with(edit):
            def change():
                changed = json.loads(originals[scope_json])
                edit(changed)
                scope_json.write_text(json.dumps(changed))
            return change

        observe("valid", lambda: None)
        observe("receipt-absent", receipt_path.unlink)
        observe("receipt-not-json", lambda: receipt_path.write_text("{"))
        observe("receipt-list", lambda: receipt_path.write_text("[]"))
        observe("receipt-oversized", lambda: receipt_path.write_text(" " * 70000))
        observe("record-absent", record_path.unlink)
        observe("version-2", receipt_with(lambda r: r.update(schemaVersion=2)))
        observe("version-true", receipt_with(lambda r: r.update(schemaVersion=True)))
        observe("worker-not-object", receipt_with(lambda r: r.update(worker=[])))
        observe("policy-not-object", receipt_with(lambda r: r.update(policy=None)))
        observe("record-pid-zero", record_with(lambda r: r.update(pid=0)))
        observe("record-workerpid-string", record_with(lambda r: r.update(workerPid="123")))
        observe("worker-pid-other", receipt_with(lambda r: r["worker"].update(pid=1)))
        observe("worker-ticks", receipt_with(lambda r: r["worker"].update(startTicks=1)))
        observe("worker-bootid", receipt_with(lambda r: r["worker"].update(bootId="older-boot")))

        def older_boot():
            record_with(lambda r: r.update(bootId="older-boot"))()
            receipt_with(lambda r: r["worker"].update(bootId="older-boot"))()
        observe("record-bootid", older_boot)
        observe("service-token", receipt_with(lambda r: r["service"].update(token="wrong")))

        def elsewhere():
            record_with(lambda r: r.update(stateDir="/elsewhere"))()
            receipt_with(lambda r: r["service"].update(stateDir="/elsewhere"))()
        observe("record-statedir", elsewhere)

        observe("scope-absent", scope_json.unlink)
        observe("scope-token", scope_with(lambda r: r.update(token="other")))

        def without(path):
            def change():
                path.unlink()
            return change
        # An unlinked lock cannot be restored (its holder keeps the old inode), so these run
        # last, the scope lock first while the daemon lock, checked before it, is still held.
        observe("scope-lock-missing", without(scope_lock))
        observe("daemon-lock-missing", without(state / DAEMON_LOCK))
        child.terminate()
        child.wait(timeout=10)

        service = self.service("b", scopes=os.path.join(self.tmp, "scopes-b"))
        self.worker(service)
        record = service.record()
        with mock.patch.object(service, "record", side_effect=[record, dict(record, token="new-run")]):
            answer = service.read_worker_policy()
        CAPTURED["observations"]["record-changes"] = answer["reason"]
        # Replacing the database cannot be undone either: the receipt names the old inode.
        database = service.selection.db_path
        copy_path = database.with_suffix(".copy")
        copy_path.write_bytes(database.read_bytes())
        os.replace(copy_path, database)
        CAPTURED["observations"]["db-replaced"] = service.read_worker_policy()["reason"]

        service = self.service("c", scopes=os.path.join(self.tmp, "scopes-c"))
        child = self.worker(service)
        child.send_signal(signal.SIGSTOP)
        os.waitpid(child.pid, os.WUNTRACED)
        CAPTURED["observations"]["process-stopped"] = service.read_worker_policy()["reason"]
        child.send_signal(signal.SIGCONT)
        child.terminate()
        child.wait(timeout=10)
        CAPTURED["observations"]["process-dead"] = service.read_worker_policy()["reason"]

    def test_readiness(self):
        service = self.service()
        self.worker(service)
        valid = service.read_worker_policy()
        self.assertTrue(valid["observed"], valid)
        ready = {"role": "parent", "model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT}
        child = {"role": "child", "model": CHILD_MODEL, "reasoningEffort": CHILD_EFFORT}
        parent_only = {"roles": {"parent": POLICY["roles"]["parent"]}}
        other_path = Path(self.tmp) / "other-policy.json"
        other_path.write_text(json.dumps(parent_only))
        other = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: str(other_path)})

        def worker_policy(edit):
            changed = copy.deepcopy(valid)
            edit(changed["policy"])
            return changed

        cases = {
            "ready": (valid, [ready], self.caller),
            "ready-both": (valid, [ready, child], self.caller),
            "requirements-empty": (valid, [], self.caller),
            "requirements-object": (valid, {"role": "parent"}, self.caller),
            "requirements-null-item": (valid, [None], self.caller),
            "requirements-model-false": (valid, [dict(ready, model=False)], self.caller),
            "requirements-model-blank": (valid, [dict(ready, model="  ")], self.caller),
            "requirements-extra-key": (valid, [dict(ready, exception="x")], self.caller),
            "role-supervisor": (valid, [dict(ready, role="supervisor")], self.caller),
            "role-undeclared": (valid, [child], None),
            "unobserved": ({"observed": False, "reason": None, "policy": None}, [ready], self.caller),
            "unobserved-reason": ({"observed": False, "reason": "worker_policy_lock_unheld", "policy": None}, [ready], self.caller),
            "worker-unresolved": (worker_policy(lambda p: p.update(state="unresolved")), [ready], self.caller),
            "caller-unresolved": (valid, [ready], rolepolicy.declared({})),
            "digest-mismatch": (worker_policy(lambda p: p.update(digest="0" * 64)), [ready], self.caller),
            "summary-mismatch": (worker_policy(lambda p: p["roles"]["parent"].update(model="not-the-declared-model")), [ready], self.caller),
            "pair-mismatch-model": (valid, [dict(ready, model="other-model")], self.caller),
            "pair-mismatch-effort": (valid, [dict(ready, reasoningEffort="low")], self.caller),
        }
        for name, (observation, requirements, caller) in cases.items():
            if caller is None:
                # role-undeclared: the worker and the caller agree on a policy declaring no child.
                caller = other
                observation = dict(valid, policy=other.summary())
            CAPTURED["readiness"][name] = rolepolicy.worker_readiness(observation, requirements, policy=caller)["reason"]
        CAPTURED["policy"] = POLICY


if __name__ == "__main__":
    result = unittest.TextTestRunner(verbosity=2).run(unittest.TestSuite([Capture("test_capture"), Capture("test_readiness")]))
    if not result.wasSuccessful():
        sys.exit(1)
    OUT.write_text(json.dumps(CAPTURED, indent=2, sort_keys=True) + "\n")
    print(OUT)
