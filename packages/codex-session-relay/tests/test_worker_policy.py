"""Read the serving process's policy, not the shell that asks about it."""

import json
import errno
import os
import select
import signal
import subprocess
import sys
from pathlib import Path
from unittest import mock

from codex_session_relay import rolepolicy
from codex_session_relay import service as service_module
from codex_session_relay.service import WORKER_POLICY, owned_service, start_ticks

from .test_rolepolicy import PARENT_EFFORT, PARENT_MODEL, POLICY, SUPERSEDED_PARENT, write_policy
from .test_service import HOLDER, REPO, ServiceTestCase


class WorkerPolicyEvidence(ServiceTestCase):
    def setUp(self):
        super().setUp()
        self.addCleanup(rolepolicy.reset)
        self.policy_path = write_policy(Path(self.tmp))
        self.caller = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: self.policy_path})
        self.requirements = [
            {"role": "parent", "model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT}
        ]

    def worker(self, service, *, configured=True):
        program = HOLDER.format(
            src=os.path.join(REPO, "src"), state=str(service.selection.path),
            socket=service.socket_path, scopes=str(service.scope.root),
            store_id=service.store_id, launch="worker-policy-test",
        ).replace(
            "    while True:",
            "    from codex_session_relay import rolepolicy\n"
            "    service.publish_worker_policy(rolepolicy.snapshot_record())\n"
            "    print('published', flush=True)\n    while True:",
        )
        env = dict(os.environ)
        env.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        if configured:
            env[rolepolicy.ENVIRONMENT_VARIABLE] = self.policy_path
        child = subprocess.Popen(
            [sys.executable, "-c", program], env=env, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True,
        )
        self.children.append(child)
        self.addCleanup(child.stdout.close)
        self.addCleanup(child.stderr.close)
        readable, _, _ = select.select([child.stdout], [], [], 10)
        self.assertTrue(readable, "worker did not publish a policy receipt")
        line = child.stdout.readline()
        if line != "published\n":
            child.wait(timeout=10)
            self.fail(child.stderr.read())
        return child

    def readiness(self, service, requirements=None):
        return rolepolicy.worker_readiness(
            service.read_worker_policy(), self.requirements if requirements is None else requirements,
            policy=self.caller,
        )

    def test_configured_caller_cannot_supply_an_unconfigured_workers_policy(self):
        service = self.service()
        child = self.worker(service, configured=False)
        observation = service.read_worker_policy()
        self.assertTrue(observation["observed"], observation)
        self.assertEqual(observation["policy"]["state"], "unresolved")
        answer = self.readiness(service)
        self.assertFalse(answer["ready"])
        self.assertEqual(answer["reason"], "worker_policy_unconfigured")
        self.assertIsNone(child.poll(), "diagnosis must not stop the worker")

    def test_matching_live_worker_is_ready_and_reads_do_not_republish(self):
        service = self.service()
        self.worker(service)
        receipt = service.selection.path / WORKER_POLICY
        before = receipt.read_bytes(), receipt.stat().st_mtime_ns
        answer = self.readiness(service)
        self.assertTrue(answer["ready"], answer)
        self.assertEqual(answer["digest"], self.caller.digest)
        self.assertEqual(before, (receipt.read_bytes(), receipt.stat().st_mtime_ns))

    def test_supervised_receipt_names_worker_not_the_parent_process(self):
        service = self.service()
        with owned_service(service, allow_isolated=True, require_intent=False):
            code = f"""
from pathlib import Path
import sys
from codex_session_relay.service import RelayService, ScopeRegistry
from codex_session_relay.store import resolve_state_dir
from codex_session_relay import rolepolicy
service = RelayService(resolve_state_dir({str(service.selection.path)!r}),
    socket_path={service.socket_path!r}, scope=ScopeRegistry(Path({str(service.scope.root)!r}), 'isolated'),
    store_id={service.store_id!r})
service.publish_worker_policy(rolepolicy.snapshot_record())
print('published', flush=True)
sys.stdin.read()
"""
            env = dict(os.environ, **{rolepolicy.ENVIRONMENT_VARIABLE: self.policy_path})
            child = subprocess.Popen([sys.executable, "-c", code], env=env,
                                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                     text=True)
            self.children.append(child)
            for handle in (child.stdin, child.stdout, child.stderr):
                self.addCleanup(handle.close)
            readable, _, _ = select.select([child.stdout], [], [], 10)
            self.assertTrue(readable)
            self.assertEqual(child.stdout.readline(), "published\n")
            # Publication can beat the supervisor recording workerPid. The gap is
            # unobserved, never attributed to the still-live supervisor instead.
            self.assertFalse(service.read_worker_policy()["observed"])
            service._note(workerPid=child.pid, workerStartTicks=start_ticks(child.pid))
            observed = service.read_worker_policy()
            self.assertTrue(self.readiness(service)["ready"], observed)
            self.assertEqual(observed["worker"]["pid"], child.pid)
            self.assertEqual(observed["service"]["pid"], os.getpid())
            child.stdin.close()
            child.wait(timeout=10)
            self.assertFalse(service.read_worker_policy()["observed"])

    def test_new_caller_policy_does_not_change_the_workers_snapshot(self):
        service = self.service()
        self.worker(service)
        changed = json.loads(json.dumps(POLICY))
        # The requested parent pair still matches. Only the policy revision changed,
        # so the pair guard cannot accidentally stand in for the digest guard.
        changed["roles"]["child"]["model"] = "different-model"
        Path(self.policy_path).write_text(json.dumps(changed))
        self.caller = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: self.policy_path})
        answer = self.readiness(service)
        self.assertFalse(answer["ready"])
        self.assertEqual(answer["reason"], "worker_policy_digest_mismatch")

    def test_configured_worker_does_not_authorize_an_unconfigured_caller(self):
        service = self.service()
        self.worker(service)
        self.caller = rolepolicy.declared({})
        answer = self.readiness(service)
        self.assertFalse(answer["ready"])
        self.assertEqual(answer["reason"], "caller_policy_unconfigured")

    def test_role_pair_cannot_override_the_same_policys_allowlist(self):
        policy = dict(POLICY, allowed=[{"model": "different-model", "efforts": ["max"]}])
        Path(self.policy_path).write_text(json.dumps(policy))
        self.caller = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: self.policy_path})
        service = self.service()
        self.worker(service)
        answer = self.readiness(service)
        self.assertFalse(answer["ready"], answer)
        # The shared parser already rejects contradictions between roles and
        # the allowlist. Readiness must preserve that refusal, not add a parser.
        self.assertEqual(answer["reason"], "worker_policy_unconfigured")

    def test_public_unresolved_snapshot_does_not_publish_private_paths(self):
        for contents in ("{not-json", None):
            with self.subTest(contents=contents):
                path = Path(self.policy_path)
                if contents is None:
                    path.unlink()
                else:
                    path.write_text(contents)
                service = self.service("missing" if contents is None else "malformed",
                                       socket=str(path.parent / str(contents)))
                self.worker(service)
                public = service.read_worker_policy()["policy"]
                self.assertEqual(public["state"], "unresolved")
                self.assertNotIn(str(path), json.dumps(public))
                self.assertTrue(public["detail"])

    def test_role_pair_mismatch_and_unsupported_requirements_are_not_ready(self):
        service = self.service()
        self.worker(service)
        # Each of the first four differs from the ready requirement in exactly one field, so that
        # field is the only thing that can refuse it.
        ready = self.requirements[0]
        for request in (
            [dict(ready, model=SUPERSEDED_PARENT[0])],
            [dict(ready, reasoningEffort=SUPERSEDED_PARENT[1])],
            [dict(ready, role="supervisor")],
            [dict(ready, exception="x")],
            [], {"role": "parent"}, [None], [dict(ready, model=False)],
        ):
            with self.subTest(request=request):
                self.assertFalse(self.readiness(service, request)["ready"])

    def test_absent_service_read_creates_nothing(self):
        service = self.service()
        before = set(service.selection.path.iterdir())
        self.assertFalse(service.read_worker_policy()["observed"])
        self.assertEqual(before, set(service.selection.path.iterdir()))

    def test_dead_or_stopped_worker_cannot_qualify_by_retained_receipt(self):
        service = self.service()
        child = self.worker(service)
        child.send_signal(signal.SIGSTOP)
        os.waitpid(child.pid, os.WUNTRACED)
        self.assertFalse(service.read_worker_policy()["observed"])
        child.send_signal(signal.SIGCONT)
        child.terminate()
        child.wait(timeout=10)
        self.assertFalse(service.read_worker_policy()["observed"])

    def test_receipt_identity_must_match_daemon_worker_pid_and_start_ticks(self):
        service = self.service()
        self.worker(service)
        path = service.selection.path / WORKER_POLICY
        original = json.loads(path.read_text())
        for field, wrong in (("pid", os.getpid()), ("startTicks", 1), ("bootId", "older-boot")):
            with self.subTest(field=field):
                changed = json.loads(json.dumps(original))
                changed["worker"][field] = wrong
                path.write_text(json.dumps(changed))
                self.assertFalse(service.read_worker_policy()["observed"])
        path.write_text(json.dumps(original))
        self.assertTrue(service.read_worker_policy()["observed"])
        service.write_record(dict(service.record(), workerPid=os.getpid(), workerStartTicks=1))
        self.assertFalse(service.read_worker_policy()["observed"])

    def test_store_replacement_same_store_id_still_invalidates_receipt(self):
        service = self.service()
        self.worker(service)
        original = service.selection.db_path
        replacement = original.with_suffix('.copy')
        replacement.write_bytes(original.read_bytes())
        os.replace(replacement, original)
        self.assertFalse(service.read_worker_policy()["observed"])

    def test_non_pid_scalars_cannot_select_the_foreground_fallback(self):
        service = self.service()
        self.worker(service)
        record = service.record()
        for pid in (False, 0, "", "123"):
            with self.subTest(pid=pid):
                service.write_record(dict(record, workerPid=pid))
                self.assertFalse(service.read_worker_policy()["observed"])
        service.write_record(record)
        self.assertTrue(service.read_worker_policy()["observed"])

    def test_malformed_or_foreign_receipts_never_pass(self):
        service = self.service()
        self.worker(service)
        path = service.selection.path / WORKER_POLICY
        original = path.read_text()
        bad = ["{", "[]", "null", '"not a record"', " " * 70000]
        for field in ("token", "socketPath", "storeId", "installationId", "stateDir"):
            changed = json.loads(original)
            changed["service"][field] = "wrong"
            bad.append(json.dumps(changed))
        bad.append(json.dumps(dict(json.loads(original), schemaVersion=True)))
        for raw in bad:
            with self.subTest(raw=raw[:80]):
                path.write_text(raw)
                self.assertFalse(service.read_worker_policy()["observed"])

    def test_record_changing_during_read_is_not_a_ready_snapshot(self):
        service = self.service()
        self.worker(service)
        record = service.record()
        with mock.patch.object(service, "record", side_effect=[record, dict(record, token="new-run")]):
            self.assertFalse(service.read_worker_policy()["observed"])

    def test_lock_io_failure_is_not_ownership_and_missing_lock_is_not_created(self):
        service = self.service()
        self.worker(service)
        with mock.patch.object(service_module.fcntl, "flock", side_effect=OSError(errno.EIO, "unreadable")):
            answer = service.read_worker_policy()
        self.assertFalse(answer["observed"])
        lock = service.selection.path / service_module.DAEMON_LOCK
        lock.unlink()
        self.assertFalse(service.read_worker_policy()["observed"])
        self.assertFalse(lock.exists())

    def test_same_digest_with_changed_role_payload_is_not_agreement(self):
        service = self.service()
        self.worker(service)
        path = service.selection.path / WORKER_POLICY
        receipt = json.loads(path.read_text())
        receipt["policy"]["roles"]["parent"]["model"] = "not-the-declared-model"
        path.write_text(json.dumps(receipt))
        answer = self.readiness(service)
        self.assertEqual(answer["reason"], "worker_policy_summary_mismatch")
        self.assertFalse(answer["ready"])

    def test_replaced_worker_requires_its_own_publication(self):
        service = self.service()
        child = self.worker(service)
        old = (service.selection.path / WORKER_POLICY).read_bytes()
        child.terminate()
        child.wait(timeout=10)
        replacement = self.worker(service)
        self.assertTrue(self.readiness(service)["ready"])
        path = service.selection.path / WORKER_POLICY
        path.write_bytes(old)
        self.assertFalse(self.readiness(service)["ready"])
        self.assertIsNone(replacement.poll())
