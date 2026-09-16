"""The durable store: a failed transition is never a success."""

import os
import unittest

import shutil
import tempfile
from pathlib import Path
from unittest import mock

from codex_session_relay.store import (
    SCHEMA_VERSION, Store, compare_store, nonce_lookup, probe, resolve_state_dir, state_dir,
)

from .support import RelayTestCase

EXPECTED_TABLES = {
    "acks", "attempts", "deliveries", "events", "generations", "journal", "observations",
    "recipient_lifecycle", "recipient_rate", "refusals", "relationships", "schema_meta",
    "verdicts", "verification_claims",
}


class Schema(RelayTestCase):
    def test_schema_v1_carries_every_table_the_later_phases_need(self):
        names = {r[0] for r in self.store.all("SELECT name FROM sqlite_master WHERE type='table'")}
        self.assertTrue(EXPECTED_TABLES <= names, EXPECTED_TABLES - names)
        self.assertEqual(
            self.store.one("SELECT value FROM schema_meta WHERE key='version'")["value"],
            str(SCHEMA_VERSION),
        )

    def test_columns_the_delivery_and_ack_layers_need_exist_now(self):
        def columns(table):
            return {r["name"] for r in self.store.all(f"PRAGMA table_info({table})")}

        self.assertTrue(
            {"kind", "recipient_task_id", "recipient_thread_id", "lease_owner", "lease_until",
             "hold_reason", "dispatch_evidence", "provenance"} <= columns("deliveries")
        )
        self.assertTrue(
            {"internal_state", "sealed", "operation_observation", "recipient_scan",
             "affirmative_evidence"} <= columns("attempts")
        )
        self.assertIn("verified", columns("acks"))
        self.assertTrue(
            {"archived", "goal_status", "can_accept_input", "deliverable", "withhold_reason"}
            <= columns("recipient_lifecycle")
        )

    def test_the_database_file_is_private(self):
        self.assertEqual(os.stat(self.store.path).st_mode & 0o777, 0o600)


class Atomicity(RelayTestCase):
    def test_an_exception_inside_a_transaction_leaves_no_partial_row(self):
        try:
            with self.store.transaction() as db:
                db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t','k','s','d')")
                raise RuntimeError("interrupted mid-write")
        except RuntimeError:
            pass
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM journal")["c"], 0)

    def test_a_fault_after_the_body_still_rolls_back(self):
        def fault():
            raise RuntimeError("storage failed at commit time")

        self.store.fault_hook = fault
        try:
            with self.store.transaction() as db:
                db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t','k','s','d')")
        except RuntimeError:
            pass
        finally:
            self.store.fault_hook = None
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM journal")["c"], 0)

    def test_a_failed_registration_is_not_a_registration(self):
        self.store.fault_hook = lambda: (_ for _ in ()).throw(RuntimeError("disk full"))
        with self.assertRaises(RuntimeError):
            self.register()
        self.store.fault_hook = None
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM relationships")["c"], 0)
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM generations")["c"], 0)

    def test_state_survives_reopening_the_database(self):
        relationship = self.register()
        self.store.db.close()
        reopened = Store(self.store.path)
        self.addCleanup(reopened.close)
        row = reopened.one(
            "SELECT * FROM relationships WHERE relationship_id = ?",
            (relationship["relationshipId"],),
        )
        self.assertIsNotNone(row)


class StateDirectory(unittest.TestCase):
    def test_explicit_override_wins(self):
        os.environ["CODEX_SESSION_RELAY_STATE"] = "/tmp/relay-state-test"
        try:
            self.assertEqual(str(state_dir()), "/tmp/relay-state-test")
        finally:
            del os.environ["CODEX_SESSION_RELAY_STATE"]

    def test_a_socket_gets_its_own_endpoint_directory(self):
        os.environ.pop("CODEX_SESSION_RELAY_STATE", None)
        first = state_dir("/run/one.sock")
        second = state_dir("/run/two.sock")
        self.assertNotEqual(first, second)
        self.assertIn("codex-session-relay", str(first))


class Precedence(unittest.TestCase):
    """Four rules can choose the directory, and a participant has to be able to say which one."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-precedence-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        # Never read the real home or the real XDG state during a test.
        self.home = os.path.join(self.tmp, "home")
        os.makedirs(self.home)
        patch = mock.patch.dict(os.environ, {"HOME": self.home}, clear=False)
        patch.start()
        self.addCleanup(patch.stop)
        for name in ("CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"):
            os.environ.pop(name, None)

    def test_each_rule_wins_in_order_and_says_so(self):
        home = resolve_state_dir(None, "/run/x.sock")
        self.assertEqual(home.source, "home")
        self.assertTrue(str(home.path).startswith(self.home))

        os.environ["XDG_STATE_HOME"] = os.path.join(self.tmp, "xdg")
        xdg = resolve_state_dir(None, "/run/x.sock")
        self.assertEqual(xdg.source, "xdg")
        self.assertIn("XDG_STATE_HOME=", xdg.detail)
        self.assertEqual(xdg.socket_scope, home.socket_scope)

        os.environ["CODEX_SESSION_RELAY_STATE"] = os.path.join(self.tmp, "env")
        env = resolve_state_dir(None, "/run/x.sock")
        self.assertEqual(env.source, "env")
        self.assertIn("CODEX_SESSION_RELAY_STATE=", env.detail)

        flag = resolve_state_dir(os.path.join(self.tmp, "flag"), "/run/x.sock")
        self.assertEqual(flag.source, "flag")
        self.assertIn("--state ", flag.detail)
        self.assertEqual(flag.db_path.name, "relay.sqlite3")

    def test_a_relative_flag_resolves_to_the_same_store_as_its_absolute_form(self):
        target = os.path.join(self.tmp, "rel")
        os.makedirs(target)
        Store(Path(target) / "relay.sqlite3").close()
        here = os.getcwd()
        os.chdir(self.tmp)
        try:
            relative = probe(resolve_state_dir("rel"))
        finally:
            os.chdir(here)
        absolute = probe(resolve_state_dir(target))
        self.assertEqual(relative["store"]["storeId"], absolute["store"]["storeId"])
        self.assertEqual(relative["store"]["inode"], absolute["store"]["inode"])

    def test_a_store_already_in_use_keeps_its_directory_after_the_hash_changed(self):
        """Canonicalising the socket moved this hash, which is an upgrade hazard.

        A relative or symlinked socket that had been running would otherwise open a fresh
        empty database while its assignments, generations and pending deliveries sat in the
        old directory, invisible.
        """
        from codex_session_relay.store import legacy_socket_scope, socket_scope

        here = os.getcwd()
        os.chdir(self.tmp)
        try:
            relative = os.path.join("run", "app-server.sock")
            os.makedirs(os.path.join(self.tmp, "run"), exist_ok=True)
            open(os.path.join(self.tmp, "run", "app-server.sock"), "w").close()
            legacy = os.path.join(
                self.home, ".local", "state", "codex-session-relay",
                legacy_socket_scope(relative),
            )
            os.makedirs(legacy)
            Store(Path(legacy) / "relay.sqlite3").close()

            chosen = resolve_state_dir(None, relative)

            self.assertEqual(str(chosen.path), legacy,
                             "the store already in use keeps its directory")
            self.assertIn("already using", chosen.detail)

            # An empty canonical DIRECTORY is not a canonical store. Any command that writes
            # beside the database creates one - a stop request is enough - and testing for
            # the directory let that hide the legacy store behind an empty folder.
            os.makedirs(
                os.path.join(self.home, ".local", "state", "codex-session-relay",
                             socket_scope(relative)),
                exist_ok=True,
            )
            self.assertEqual(
                str(resolve_state_dir(None, relative).path), legacy,
                "an empty canonical directory must not hide a real store",
            )
        finally:
            os.chdir(here)

    def test_a_fresh_socket_uses_the_canonical_directory(self):
        """The fallback is for an existing store only; nothing new lands in the old name."""
        from codex_session_relay.store import socket_scope

        chosen = resolve_state_dir(None, "/run/brand-new.sock")

        self.assertEqual(chosen.socket_scope, socket_scope("/run/brand-new.sock"))
        self.assertTrue(str(chosen.path).endswith(socket_scope("/run/brand-new.sock")))

    def test_two_spellings_of_one_socket_choose_the_same_default_store(self):
        """The scope registry canonicalises the socket; this hash used to take it verbatim.

        With no --state and no environment override, an alias for the socket produced a
        different default state directory and therefore a different store. The registry then
        refused the second invocation as a foreign owner rather than letting it join the
        service already running on that socket.
        """
        real = os.path.join(self.tmp, "run", "app-server.sock")
        os.makedirs(os.path.dirname(real))
        open(real, "w").close()
        alias_dir = os.path.join(self.tmp, "alias")
        os.symlink(os.path.join(self.tmp, "run"), alias_dir)

        canonical = resolve_state_dir(None, real)
        through_symlink = resolve_state_dir(None, os.path.join(alias_dir, "app-server.sock"))

        here = os.getcwd()
        os.chdir(self.tmp)
        try:
            relative = resolve_state_dir(None, os.path.join("run", "app-server.sock"))
        finally:
            os.chdir(here)

        self.assertEqual(through_symlink.socket_scope, canonical.socket_scope)
        self.assertEqual(relative.socket_scope, canonical.socket_scope)
        self.assertEqual(through_symlink.path, canonical.path)
        self.assertEqual(relative.path, canonical.path)

class Identity(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-identity-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.a = os.path.join(self.tmp, "a")
        os.makedirs(self.a)
        self.store = Store(Path(self.a) / "relay.sqlite3")
        self.addCleanup(self.store.close)

    def copy_store(self, into):
        """Copy the store the way a person copying a state directory would.

        The sidecars matter: in WAL mode a recent write still lives in relay.sqlite3-wal, so
        copying the main file alone produces a store that has lost it. That would make this
        test pass for the wrong reason - an empty copy has no identifier to collide with.
        """
        os.makedirs(into, exist_ok=True)
        for suffix in ("", "-wal", "-shm"):
            source = os.path.join(self.a, f"relay.sqlite3{suffix}")
            if os.path.exists(source):
                shutil.copy(source, os.path.join(into, f"relay.sqlite3{suffix}"))
        return into

    def test_identity_is_minted_once_and_survives_reopening(self):
        first = self.store.locate()
        self.assertTrue(first["storeId"])
        self.store.close()
        again = Store(Path(self.a) / "relay.sqlite3")
        self.addCleanup(again.close)
        self.assertEqual(again.identity, first["storeId"])
        self.assertEqual(again.meta("store_created_at"), first["createdAt"])

    def test_a_copy_keeps_the_identifier_and_is_not_the_same_store(self):
        """The case a stored uuid gets wrong: copying the file copies the identifier."""
        mine = self.store.locate()
        b = self.copy_store(os.path.join(self.tmp, "b"))
        theirs = probe(resolve_state_dir(b))["store"]
        self.assertEqual(theirs["storeId"], mine["storeId"])
        self.assertNotEqual(theirs["inode"], mine["inode"])

        # The identifier alone is satisfied by the copy, so it is never proof on its own.
        self.assertEqual(
            compare_store(theirs, expect_store=mine["storeId"])["sameStore"], "unproven",
        )
        self.assertEqual(
            compare_store(
                theirs, expect_store=mine["storeId"],
                expect_inode=f"{mine['device']}:{mine['inode']}",
            )["sameStore"],
            "mismatch",
        )

    def test_a_nonce_written_after_the_copy_separates_them(self):
        b = self.copy_store(os.path.join(self.tmp, "b"))
        self.assertEqual(
            probe(resolve_state_dir(b))["store"]["storeId"], self.store.identity,
            "the copy must be a real copy, or the nonce result proves nothing",
        )
        # Written AFTER the copy on purpose: a nonce that already existed would have been
        # copied too, and finding it would prove nothing.
        written = self.store.write_challenge(actor="parent")
        here = nonce_lookup(resolve_state_dir(self.a), written["nonce"])
        there = nonce_lookup(resolve_state_dir(b), written["nonce"])
        self.assertTrue(here["found"])
        self.assertFalse(there["found"])
        self.assertEqual(compare_store(self.store.locate(), nonce=here)["sameStore"], "proven")
        self.assertEqual(
            compare_store(probe(resolve_state_dir(b))["store"], nonce=there)["sameStore"],
            "mismatch",
        )

    def test_a_nonce_query_that_fails_is_unreadable_rather_than_absent(self):
        """Could not look is not is not there, and compare_store grades the difference.

        A locked, malformed or momentarily unavailable database opens and then fails the
        query. Calling that readable turned it into a definite store mismatch, so
        doctor --expect-nonce would claim two participants use different stores on the
        strength of a transient read failure.
        """
        broken = os.path.join(self.tmp, "broken")
        os.makedirs(broken)
        with open(os.path.join(broken, "relay.sqlite3"), "wb"):
            pass

        answer = nonce_lookup(resolve_state_dir(broken), "any-nonce")

        self.assertFalse(answer["found"])
        self.assertFalse(answer["readable"], "the query failed; nothing was learned")
        self.assertIsNotNone(answer["detail"])
        self.assertNotEqual(
            compare_store(probe(resolve_state_dir(broken))["store"], nonce=answer)["sameStore"],
            "mismatch",
        )

    def test_a_symlinked_directory_is_the_same_store(self):
        alias = os.path.join(self.tmp, "alias")
        os.symlink(self.a, alias)
        mine = self.store.locate()
        through_alias = probe(resolve_state_dir(alias))["store"]
        self.assertNotEqual(through_alias["dbPath"], mine["dbPath"])
        self.assertEqual(
            compare_store(
                through_alias, expect_store=mine["storeId"],
                expect_inode=f"{mine['device']}:{mine['inode']}",
            )["sameStore"],
            "proven",
        )

    def test_a_store_with_no_identity_is_never_proven_equal(self):
        """Absence must not become agreement; an old store predates the identity rows."""
        self.assertEqual(
            compare_store({"storeId": None}, expect_store="whatever")["sameStore"], "unproven",
        )

    def test_an_unreadable_nonce_is_unproven_rather_than_a_mismatch(self):
        """Not being able to look is not the same as looking and finding nothing."""
        unreadable = {"nonce": "abc", "found": False, "readable": False,
                      "detail": "OperationalError: unable to open database file"}
        graded = compare_store({"storeId": "x"}, nonce=unreadable)
        self.assertEqual(graded["sameStore"], "unproven")
        self.assertIn("could not be read", graded["detail"])
        absent = {"nonce": "abc", "found": False, "readable": True, "detail": None}
        self.assertEqual(
            compare_store({"storeId": "x"}, nonce=absent)["sameStore"], "mismatch",
        )


class Probe(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-probe-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_a_missing_state_directory_is_an_answer_and_is_not_created(self):
        absent = os.path.join(self.tmp, "nothing-here")
        report = probe(resolve_state_dir(absent))
        self.assertFalse(report["access"]["directoryExists"])
        self.assertFalse(report["store"]["exists"])
        self.assertFalse(os.path.exists(absent), "probe must not create what it describes")

    def test_reported_writability_matches_what_this_process_can_really_do(self):
        """Asserted against a measured attempt, because a privileged runner ignores mode bits."""
        locked = os.path.join(self.tmp, "locked")
        os.makedirs(locked)
        Store(Path(locked) / "relay.sqlite3").close()
        os.chmod(locked, 0o500)
        self.addCleanup(os.chmod, locked, 0o700)
        try:
            probe_file = os.path.join(locked, ".really-writable")
            with open(probe_file, "w", encoding="utf-8"):
                pass
            os.unlink(probe_file)
            really_writable = True
        except OSError:
            really_writable = False
        report = probe(resolve_state_dir(locked))
        self.assertEqual(report["access"]["directoryWritable"], really_writable)
        self.assertTrue(report["access"]["dbReadable"])
        if not really_writable:
            self.assertIsNotNone(report["access"]["detail"])


if __name__ == "__main__":
    unittest.main()
