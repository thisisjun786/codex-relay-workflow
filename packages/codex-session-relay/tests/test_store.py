"""The durable store: a failed transition is never a success."""

import os
import unittest

import shutil
import tempfile
from pathlib import Path
from unittest import mock

from codex_session_relay.store import (
    SCHEMA_VERSION, Store, compare_store, nonce_lookup, probe, read_only_rows,
    resolve_state_dir, state_dir,
)

from .support import RelayTestCase, WorkerKilled, killed_before_commit

EXPECTED_TABLES = {
    "acks", "attempts", "deliveries", "events", "generations", "journal", "observations",
    "recipient_lifecycle", "recipient_rate", "refusals", "relationships", "schema_meta",
    "verdicts", "verification_claims",
    # The fault ledger: the merged tables and the corrected contract's, all created on open.
    "fault_ledger", "fault_occurrences", "fault_timeline", "fault_remediations",
    "fault_publications", "fault_targets", "fault_cursors", "fault_target_projects",
    "fault_publication_payloads", "fault_links", "fault_adoptions", "fault_aliases",
    "fault_publication_attempts", "fault_budget_uses", "fault_limits", "fault_notifications",
    "fault_policies",
}


def log_location(measured, *, inode=None):
    """What a participant sends as `--expect-log`, from what it measured about itself."""
    return "%s:%s:%s" % (
        measured["logDevice"],
        measured["logInode"] if inode is None else inode,
        measured["logName"],
    )


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


class Composing(RelayTestCase):
    """Several existing writers, one transaction, and only where somebody asked for it."""

    def journal(self, db, kind):
        db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t',?,'s','d')", (kind,))

    def kinds(self):
        return [row["kind"] for row in self.store.all("SELECT kind FROM journal")]

    def durable_kinds(self):
        """What a SECOND connection can see, which is the only reading of "committed".

        Asked through its own read-only connection because store.db sees its own uncommitted
        writes: reading the claim back through the connection that wrote it would pass whether
        or not anything was committed, which is the opposite of what these cases assert. WAL
        is on (Store.__init__), so this reader is not blocked by the open write transaction.
        """
        import sqlite3

        reader = sqlite3.connect(f"{Path(self.store.path).as_uri()}?mode=ro", uri=True, timeout=5)
        try:
            return [row[0] for row in reader.execute("SELECT kind FROM journal")]
        finally:
            reader.close()

    def test_a_joined_scope_commits_nothing_of_its_own(self):
        """The inner writer thinks it committed; nothing is durable until the opener says so."""
        with self.store.composing():
            with self.store.transaction() as db:
                self.journal(db, "inner")
            # Still inside the composed transaction. The inner scope's exit committed nothing,
            # which is the whole point, and a reader outside this connection proves it.
            self.assertTrue(self.store.in_transaction)
            self.assertEqual(
                self.durable_kinds(), [],
                "the joined scope committed on its own, so composing() is not composing",
            )
        self.assertEqual(self.kinds(), ["inner"])
        self.assertEqual(self.durable_kinds(), ["inner"])

    def test_a_raise_inside_a_joined_scope_rolls_back_what_the_opener_wrote(self):
        with self.assertRaises(RuntimeError):
            with self.store.composing() as outer:
                self.journal(outer, "opener")
                with self.store.transaction() as db:
                    self.journal(db, "inner")
                    raise RuntimeError("interrupted mid-write")
        self.assertEqual(self.kinds(), [])
        self.assertEqual(self.durable_kinds(), [])
        self.assertFalse(self.store.in_transaction)

    def test_nesting_without_composing_is_still_the_error_it_always_was(self):
        """Joining is a deliberate act. Accidental nesting must not quietly become one.

        This is what keeps D1's cost bounded: every existing caller that opens a transaction
        inside a transaction still fails loudly rather than silently handing its writes to a
        scope that may roll them back.
        """
        import sqlite3

        with self.assertRaises(sqlite3.OperationalError):
            with self.store.transaction():
                with self.store.transaction():
                    pass
        self.assertFalse(self.store.in_transaction)
        # And the store is still usable afterwards, so the loud failure is a refusal rather
        # than a connection left in a state nothing can write through.
        with self.store.transaction() as db:
            self.journal(db, "after")
        self.assertEqual(self.durable_kinds(), ["after"])

    def test_the_composing_counter_is_released_when_the_body_raises(self):
        """A failed composition must not leave the store willing to join forever after."""
        import sqlite3

        with self.assertRaises(RuntimeError):
            with self.store.composing():
                raise RuntimeError("interrupted mid-write")
        with self.assertRaises(sqlite3.OperationalError):
            with self.store.transaction():
                with self.store.transaction():
                    pass


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
        """Killed inside the registration, not merely inside whichever write came first.

        register() runs three transactions and this name held only because the relationship
        write happens to be in the first one. Naming the table is what makes that a property
        of the case rather than of the order.
        """
        with self.assertRaises(WorkerKilled):
            with killed_before_commit(self.store, writing="relationships") as kill:
                self.register()
        self.assertIn(
            "generations", kill.killed,
            "the relationship and its first generation are no longer written in one"
            f" transaction, so this case no longer measures what its name says: {kill.killed}",
        )
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
    def setUp(self):
        """Pin HOME. Resolving a socket's directory reads the siblings next to it, and with
        the real home that means opening the databases this host actually runs on - a test
        has no business touching those, even to read them."""
        self.tmp = tempfile.mkdtemp(prefix="relay-statedir-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        home = os.path.join(self.tmp, "home")
        os.makedirs(home)
        patch = mock.patch.dict(os.environ, {"HOME": home}, clear=False)
        patch.start()
        self.addCleanup(patch.stop)
        for name in ("CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"):
            os.environ.pop(name, None)

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

    def test_a_store_is_found_by_the_socket_it_recorded_not_by_its_hash(self):
        """The legacy comparison only helps when THIS invocation uses the old spelling.

        A first post-upgrade command that happens to use the absolute path has
        legacy == scope, so the comparison never looks at the store the relative spelling
        created - and creating a canonical database then hides it for good, because
        afterwards even the old spelling finds the new one.
        """
        from codex_session_relay.store import legacy_socket_scope, socket_scope

        os.makedirs(os.path.join(self.tmp, "run"), exist_ok=True)
        absolute = os.path.join(self.tmp, "run", "app-server.sock")
        open(absolute, "w").close()
        here = os.getcwd()
        os.chdir(self.tmp)
        try:
            relative = os.path.join("run", "app-server.sock")
            # The store the pre-upgrade installation left behind, under the RELATIVE hash.
            previous = os.path.join(
                self.home, ".local", "state", "codex-session-relay",
                legacy_socket_scope(relative),
            )
            os.makedirs(previous)
            Store(Path(previous) / "relay.sqlite3", socket_path=relative).close()

            # The first post-upgrade command uses the ABSOLUTE spelling, so the legacy
            # comparison is a no-op: legacy == scope.
            self.assertEqual(
                legacy_socket_scope(absolute), socket_scope(absolute),
                "the fixture needs the case the hash comparison cannot see",
            )
            chosen = resolve_state_dir(None, absolute)
        finally:
            os.chdir(here)

        self.assertEqual(str(chosen.path), previous,
                         "the store recorded for this socket must be adopted, not hidden")
        self.assertIn("adopted", chosen.detail)

    def test_two_stores_claiming_one_socket_are_not_silently_chosen_between(self):
        """Picking whichever sorts first operates on one set of assignments today and the
        other after a rename. Adopting nothing is wrong too, but it is visible."""
        from codex_session_relay.store import (
            discover_store_for_socket, stores_claiming_socket,
        )

        root = os.path.join(self.home, ".local", "state", "codex-session-relay")
        socket = "/run/contested.sock"
        both = []
        for name in ("aaaa000000000000", "bbbb000000000000"):
            directory = os.path.join(root, name)
            os.makedirs(directory)
            Store(Path(directory) / "relay.sqlite3", socket_path=socket).close()
            both.append(directory)

        self.assertIsNone(
            discover_store_for_socket(Path(root), socket),
            "an ambiguity is not a choice to make silently",
        )
        self.assertEqual(sorted(stores_claiming_socket(Path(root), socket)), sorted(both))

    def test_one_store_claiming_a_socket_is_still_adopted(self):
        """The refusal must not swallow the case discovery exists for."""
        from codex_session_relay.store import discover_store_for_socket

        root = os.path.join(self.home, ".local", "state", "codex-session-relay")
        socket = "/run/sole.sock"
        directory = os.path.join(root, "cccc000000000000")
        os.makedirs(directory)
        Store(Path(directory) / "relay.sqlite3", socket_path=socket).close()

        self.assertEqual(
            str(discover_store_for_socket(Path(root), socket)), directory,
        )

    def test_two_stores_claiming_one_socket_do_not_produce_a_third(self):
        """Adopting neither is not the visible failure it was argued to be.

        The caller lands on the canonical directory, the first write there creates a THIRD
        empty database, and from that moment the canonical-exists branch wins every later
        resolution - so both real stores are hidden for good. The selection has to carry the
        conflict instead, which is what lets the command line refuse.
        """
        root = os.path.join(self.home, ".local", "state", "codex-session-relay")
        socket = "/run/contested-selection.sock"
        both = []
        for name in ("aaaa333333333333", "bbbb333333333333"):
            directory = os.path.join(root, name)
            os.makedirs(directory)
            Store(Path(directory) / "relay.sqlite3", socket_path=socket).close()
            both.append(directory)

        chosen = resolve_state_dir(None, socket)

        self.assertEqual(sorted(chosen.ambiguous), sorted(both))
        self.assertEqual(sorted(chosen.to_record()["ambiguous"]), sorted(both))
        self.assertNotIn(str(chosen.path), both, "neither is adopted on a guess")

    def test_an_ordinary_selection_carries_no_ambiguity(self):
        """The field is a conflict report, not a list of neighbours."""
        self.assertEqual(resolve_state_dir(None, "/run/quiet.sock").ambiguous, ())
        self.assertEqual(resolve_state_dir(os.path.join(self.tmp, "flag")).ambiguous, ())

    def test_a_store_with_no_recorded_socket_is_never_adopted_on_a_guess(self):
        """Provenance or nothing. Adopting an unlabelled store is how the wrong one is served."""
        from codex_session_relay.store import socket_scope, stores_without_provenance

        root = os.path.join(self.home, ".local", "state", "codex-session-relay")
        stranger = os.path.join(root, "0123456789abcdef")
        os.makedirs(stranger)
        Store(Path(stranger) / "relay.sqlite3").close()

        chosen = resolve_state_dir(None, "/run/brand-new.sock")

        self.assertEqual(chosen.socket_scope, socket_scope("/run/brand-new.sock"))
        self.assertNotEqual(str(chosen.path), stranger)
        # But it IS visible, so an operator is not left guessing why a store looks empty.
        self.assertIn(stranger, stores_without_provenance(root, skip=chosen.path.name))

    def test_a_store_recording_no_socket_blocks_creating_one_beside_it(self):
        """Its directory hash cannot be inverted, so it cannot be ruled out as this socket's.

        Creating a canonical database next to it hides it exactly the way a third store hides
        two contested ones, and reporting it through doctor alone did not help: ordinary
        commands do not run doctor.
        """
        root = os.path.join(self.home, ".local", "state", "codex-session-relay")
        stranger = os.path.join(root, "fedcba9876543210")
        os.makedirs(stranger)
        Store(Path(stranger) / "relay.sqlite3").close()

        chosen = resolve_state_dir(None, "/run/first-after-upgrade.sock")

        self.assertEqual(list(chosen.unidentified), [stranger])
        self.assertEqual(chosen.ambiguous, ())
        self.assertEqual(list(chosen.to_record()["unidentified"]), [stranger])

    def test_an_existing_canonical_store_settles_the_question_already(self):
        """The refusal is about CREATING one. A store that is already here has answered."""
        from codex_session_relay.store import socket_scope

        root = os.path.join(self.home, ".local", "state", "codex-session-relay")
        stranger = os.path.join(root, "fedcba9876543211")
        os.makedirs(stranger)
        Store(Path(stranger) / "relay.sqlite3").close()
        socket = "/run/already-here.sock"
        mine = os.path.join(root, socket_scope(socket))
        os.makedirs(mine)
        Store(Path(mine) / "relay.sqlite3", socket_path=socket).close()

        chosen = resolve_state_dir(None, socket)

        self.assertEqual(str(chosen.path), mine)
        self.assertEqual(chosen.unidentified, ())

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
        # Proof takes both: the nonce says a write of the peer's reached this file, and the
        # physical identity says it is still the same file. The ordering this fixture relies
        # on - challenge written after the copy - is what nothing in the protocol enforces,
        # which is why the nonce alone is not proof. See
        # test_a_nonce_copied_with_the_bytes_is_not_proof_of_a_shared_store.
        mine = self.store.locate()
        self.assertEqual(
            compare_store(
                mine, expect_inode=f"{mine['device']}:{mine['inode']}",
                expect_log=log_location(mine), nonce=here,
            )["sameStore"],
            "proven",
        )
        self.assertEqual(compare_store(mine, nonce=here)["sameStore"], "unproven")
        self.assertEqual(
            compare_store(probe(resolve_state_dir(b))["store"], nonce=there)["sameStore"],
            "mismatch",
        )

    def test_a_nonce_copied_with_the_bytes_is_not_proof_of_a_shared_store(self):
        """The order the protocol cannot enforce, and why a nonce alone is not proof.

        The fixture above writes the challenge AFTER the copy, so the copy cannot contain it -
        and that ordering is what made this look settled. Nothing in `store-challenge --write`
        followed by `doctor --expect-nonce` establishes it. A copy taken after the write
        carries the nonce, the store id and the bytes, and it stays stable for the whole
        invocation, so every check for a replacement or a second name sees nothing wrong.

        What a found nonce says is narrower than proof: the file I read contains a write that
        was made to the writer's file at some earlier moment. Whether it is STILL one file is
        what the physical identity says, which is why proof takes both.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        mine = probe(resolve_state_dir(self.a))["store"]

        copied = self.copy_store(os.path.join(self.tmp, "copied-after"))
        theirs = probe(resolve_state_dir(copied))["store"]
        here = nonce_lookup(resolve_state_dir(copied), written["nonce"])

        graded = compare_store(theirs, nonce=here)
        self.assertNotEqual(
            graded["sameStore"], "proven",
            "a nonce that travelled with a copy of the bytes proved a shared store",
        )
        self.assertIn("copy", graded["detail"], graded)

        # The case is the one described: the copy really carries both the nonce and the id.
        self.assertTrue(here["found"], here)
        self.assertEqual(theirs["storeId"], mine["storeId"])
        self.assertNotEqual(theirs["inode"], mine["inode"])

        # And the evidence that still separates them is the physical identity.
        self.assertEqual(
            compare_store(
                theirs, expect_inode=f"{mine['device']}:{mine['inode']}", nonce=here,
            )["sameStore"],
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
        # One pathname, reached by two spellings: resolve() follows the symlink, so both
        # participants open the same name and share one write-ahead log.
        self.assertEqual(through_alias["realPath"], mine["realPath"])
        self.assertEqual(through_alias["links"], 1, through_alias)

        # Not a mismatch, and not proof either. This side holds its own path and the peer's
        # device and inode, and nothing in that says the peer resolved to the same pathname.
        self.assertEqual(
            compare_store(
                through_alias, expect_store=mine["storeId"],
                expect_inode=f"{mine['device']}:{mine['inode']}",
            )["sameStore"],
            "unproven",
        )
        # What settles it is the nonce, which is an observation rather than an inference.
        written = self.store.write_challenge(actor="parent")
        found = nonce_lookup(resolve_state_dir(alias), written["nonce"])
        self.assertTrue(found["found"], "the alias reaches the same file")
        # Both spellings resolve to one pathname, so both connections write one log - which is
        # what makes this the positive case for the log location rather than a second name.
        self.assertEqual(log_location(through_alias), log_location(mine))
        self.assertEqual(
            compare_store(
                through_alias, expect_inode=f"{mine['device']}:{mine['inode']}",
                expect_log=log_location(mine), nonce=found,
            )["sameStore"],
            "proven",
        )

    def test_an_agreeing_device_and_inode_is_never_proof_on_its_own(self):
        """One inode can be reached at more than one pathname, and the log follows the name.

        `st_nlink` counts hardlink names, and the name count is not the whole question: a file
        bind mount attaches one inode at a second pathname without changing it. So a count of
        one is not evidence that both participants opened the same name, and grading an
        agreeing pair as proof would be sound only for cases this side can tell apart - it
        holds its own path and the peer's device and inode, and nothing that says which name
        the peer opened.

        The bind mount is not made here: this host refuses an unprivileged mount namespace
        (`unshare -rm` fails writing `/proc/self/uid_map`) and a test suite may not take a
        privileged one. It HAS since been measured, in a private mount namespace on
        2026-09-22, and it behaved as this docstring predicted - one name, one pair, a
        checkpointed nonce readable through both, and `proven` at exit 0 while the two
        directories each grew their own `-wal`. That is what the log location now refuses; see
        the two tests below it. What is asserted here is still the narrower rule this test was
        written for, on an ordinary single-named store: an agreeing pair alone is not proof.
        """
        mine = self.store.locate()
        self.assertEqual(mine["links"], 1, "this case is about a single-named inode")

        graded = compare_store(
            probe(resolve_state_dir(self.a))["store"], expect_store=mine["storeId"],
            expect_inode=f"{mine['device']}:{mine['inode']}",
        )

        self.assertEqual(
            graded["sameStore"], "unproven",
            "an agreeing device and inode was graded as proof of one live store",
        )
        self.assertIn("pathname", graded["detail"], graded)

        # Conclusive in the direction where it is conclusive: a different pair is a mismatch
        # rather than merely unproven, and demoting agreement must not cost that.
        elsewhere = self.copy_store(os.path.join(self.tmp, "elsewhere"))
        self.assertEqual(
            compare_store(
                probe(resolve_state_dir(elsewhere))["store"],
                expect_inode=f"{mine['device']}:{mine['inode']}",
            )["sameStore"],
            "mismatch",
        )

    def test_a_second_name_for_one_inode_is_not_proof_of_a_shared_store(self):
        """A hardlink agrees on every identity this compared, and is still not one store.

        Measured here on 2026-09-17 rather than reasoned about. With a store open on
        `a/relay.sqlite3` and a hardlink at `b/relay.sqlite3`: `st_nlink` is 2 and the
        device, inode and store id all agree - the first two are properties of the one inode
        and the third is a row inside it, minted once.
        A read through the second name while the first connection's write-ahead log was live
        failed with `OperationalError: disk I/O error` and `probe` returned `storeId: None`;
        after the first connection closed and checkpointed, the same read succeeded and the
        second directory had grown its own `relay.sqlite3-wal` and `-shm`. Two names are two
        write-ahead logs, so agreeing on the inode does not say the two participants are
        writing and reading one live store.

        This is why the pair is graded the way it is: it is decisive about a DIFFERENT file
        and not sufficient for the same one. A nonce does not close this either - once the
        first connection checkpoints, a nonce written through one name is readable through
        the other - so the name count is measured rather than inferred from either.
        """
        mine = self.store.locate()
        # Closed first so the committed content is in the main file: a probe through the
        # second name during a live write-ahead log cannot read the store id at all, and
        # then this case would pass because the identity was MISSING rather than because a
        # shared inode was refused as proof.
        self.store.close()
        other = os.path.join(self.tmp, "hardlink")
        os.makedirs(other)
        os.link(os.path.join(self.a, "relay.sqlite3"), os.path.join(other, "relay.sqlite3"))

        theirs = probe(resolve_state_dir(other))["store"]

        graded = compare_store(
            theirs, expect_store=mine["storeId"],
            expect_inode=f"{mine['device']}:{mine['inode']}",
        )

        # The defect first, so a failure says what it is rather than naming a missing field.
        self.assertEqual(
            graded["sameStore"], "unproven",
            "a second name for one inode was graded as proof of a shared live store",
        )
        self.assertIn("names", graded["detail"], graded)

        # And the case really is the one described: every identity agrees, only the number
        # of names for the inode does not.
        self.assertEqual(theirs["storeId"], mine["storeId"], "the identities must agree")
        self.assertEqual(
            (theirs["device"], theirs["inode"]), (mine["device"], mine["inode"]),
            "one inode, or this case is not the one being tested",
        )
        self.assertNotEqual(theirs["realPath"], mine["realPath"])
        self.assertEqual(theirs["links"], 2, theirs)

    def test_a_nonce_does_not_talk_the_second_name_up_into_proof(self):
        """The nonce cannot see this, so it must not outvote it.

        A nonce written through one name and read through the other is found once the
        writer has checkpointed, which says they shared the file at that moment and says
        nothing about the separate write-ahead logs they keep writing into. Absence is never
        agreement here for the same reason a missing identity is not.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        other = os.path.join(self.tmp, "hardlink")
        os.makedirs(other)
        os.link(os.path.join(self.a, "relay.sqlite3"), os.path.join(other, "relay.sqlite3"))

        found = nonce_lookup(resolve_state_dir(other), written["nonce"])
        self.assertTrue(found["found"], "the nonce is readable through the second name")

        graded = compare_store(probe(resolve_state_dir(other))["store"], nonce=found)
        self.assertEqual(graded["sameStore"], "unproven", graded)
        self.assertIn("names", graded["detail"], graded)

    def test_a_second_pathname_no_name_count_can_see_is_still_refused(self):
        """The bind mount, in the shape it was measured in.

        A file bind mount reaches one inode at a second pathname without changing `st_nlink`,
        so every check this side can make alone agrees: one name, one device and inode, and a
        checkpointed nonce readable through both. Measured on this host on 2026-09-22 inside a
        private mount namespace, exactly that reached `proven` at exit 0 - and each directory
        grew its own `-wal` and `-shm`, with a frame written live through the first name
        unreadable through the second. Two logs, graded as proof.

        The mount is not made here, for the reason the test above gives. What is reproduced is
        the measured SHAPE: everything agrees except the directory entry the log goes under.
        The recorded run and its raw captures live outside this repository.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        measured = probe(resolve_state_dir(self.a))["store"]
        found = nonce_lookup(resolve_state_dir(self.a), written["nonce"])
        self.assertEqual(measured["links"], 1, "the bind mount leaves ONE name, or this is not it")
        self.assertTrue(found["found"], found)

        # The peer opened the other pathname, so its log goes under another directory entry.
        peer = log_location(measured, inode=measured["logInode"] + 1)
        graded = compare_store(
            measured, expect_store=measured["storeId"],
            expect_inode=f"{measured['device']}:{measured['inode']}",
            expect_log=peer, nonce=found,
        )

        self.assertEqual(
            graded["sameStore"], "unproven",
            "a second pathname no count can see was graded as proof of a shared live store",
        )
        # Both locations, so an operator can see which two directories are in play rather
        # than only that something disagreed.
        self.assertIn(peer, graded["detail"], graded)
        self.assertIn(str(measured["logInode"]), graded["detail"], graded)

    def test_two_pathnames_that_share_one_log_are_still_one_store(self):
        """The case that decides why this is a directory entry and not a pathname.

        A whole-directory bind mount also gives one inode two pathname strings - and there the
        two participants DO share a log. Measured the same day: a frame written live through
        one name was readable through the other, and both resolved into one directory object.
        A container reaching the host's state directory at another path is that shape. So the
        comparison must refuse the test above and accept this one, which a pathname string
        cannot do: it differs in both.

        `compare_store` is therefore asserted to not consult `realPath` at all.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        measured = probe(resolve_state_dir(self.a))["store"]
        found = nonce_lookup(resolve_state_dir(self.a), written["nonce"])
        expectations = {
            "expect_inode": f"{measured['device']}:{measured['inode']}",
            "expect_log": log_location(measured),
            "nonce": found,
        }

        elsewhere = dict(measured, realPath="/mounted/somewhere/else/relay.sqlite3")
        self.assertNotEqual(elsewhere["realPath"], measured["realPath"])
        self.assertEqual(
            compare_store(elsewhere, **expectations)["sameStore"], "proven",
            "two pathnames for one shared log were refused, which would refuse a container",
        )

        # And the agreeing branch is really reached rather than skipped: move the location
        # and the same inputs refuse.
        moved = dict(expectations, expect_log=log_location(measured, inode=measured["logInode"] + 1))
        self.assertEqual(compare_store(elsewhere, **moved)["sameStore"], "unproven")

    def test_a_nonce_is_not_proof_until_the_log_location_has_been_compared(self):
        """Absence of the comparison is not agreement, and the refusal says what to send.

        A caller holding only the older expectations is not wrong about them - it simply has
        not asked the question that separates one store from one inode at two pathnames, and a
        verdict that cannot tell those apart must not be `proven`.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        measured = probe(resolve_state_dir(self.a))["store"]
        found = nonce_lookup(resolve_state_dir(self.a), written["nonce"])
        pair = f"{measured['device']}:{measured['inode']}"

        graded = compare_store(measured, expect_inode=pair, nonce=found)
        self.assertEqual(graded["sameStore"], "unproven", graded)
        self.assertIn("--expect-log", graded["detail"], graded)
        # The one that WAS supplied is not asked for again.
        self.assertNotIn("--expect-inode", graded["detail"], graded)

        # Neither supplied: both are named, and the older message still reads as it did.
        neither = compare_store(measured, nonce=found)
        self.assertEqual(neither["sameStore"], "unproven", neither)
        self.assertIn("--expect-inode", neither["detail"], neither)
        self.assertIn("--expect-log", neither["detail"], neither)

    def test_a_log_location_that_cannot_be_used_is_not_comparable_rather_than_different(self):
        """Could not compare is not compared and disagreed, the rule the pair already follows.

        A malformed value and an unmeasurable one are the same answer: this side cannot say.
        Grading either as a difference would report a second pathname that nobody observed.
        """
        measured = probe(resolve_state_dir(self.a))["store"]
        for unusable in ("", "1:2", "nonsense"):
            graded = compare_store(measured, expect_log=unusable)
            self.assertEqual(graded["sameStore"], "unproven", (unusable, graded))
            self.assertIn("not comparable here", graded["detail"], (unusable, graded))

        unmeasured = dict(measured, logDevice=None, logInode=None, logName=None)
        graded = compare_store(unmeasured, expect_log=log_location(measured))
        self.assertEqual(graded["sameStore"], "unproven", graded)
        self.assertIn("not comparable here", graded["detail"], graded)

    def test_a_nonce_read_through_another_log_is_not_attributed_to_this_one(self):
        """The attribution the device and inode already get, one level along.

        `probe` and `nonce_lookup` are two independent opens of the pathname, so an answer can
        carry this store's device and inode and still have been read through a name whose log
        is a different file - a `--state` pointing at a symlink or mount point that is
        re-pointed between the two calls reaches it. Without this the log arm would compare
        only the probe's location with the peer's, and proof would rest on a nonce read
        somewhere else.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        measured = probe(resolve_state_dir(self.a))["store"]
        found = nonce_lookup(resolve_state_dir(self.a), written["nonce"])
        read_elsewhere = dict(found, logInode=found["logInode"] + 1)

        graded = compare_store(
            measured, expect_inode=f"{measured['device']}:{measured['inode']}",
            expect_log=log_location(measured), nonce=read_elsewhere,
        )
        self.assertEqual(graded["sameStore"], "unproven", graded)
        self.assertIn("read through a pathname", graded["detail"], graded)

        # And a location that could not be measured at all is refused the same way rather
        # than read as agreement: absence is never agreement here either, which is the rule
        # the device and inode attribution beside it already follows.
        unmeasured = dict(found, logDevice=None, logInode=None, logName=None)
        graded = compare_store(
            measured, expect_inode=f"{measured['device']}:{measured['inode']}",
            expect_log=log_location(measured), nonce=unmeasured,
        )
        self.assertEqual(graded["sameStore"], "unproven", graded)
        self.assertIn("could not be measured", graded["detail"], graded)

    def test_a_nonce_read_from_a_replacement_does_not_prove_the_measured_store(self):
        """The only evidence graded as proof, bound to the file it was read from.

        `nonce_lookup` opens the path itself, after whatever stat'd it for the receipt, so a
        replacement between the two hands a comparison whose identity came from A an answer
        that came from B. A copy carries the challenge row with the bytes - `copy_store` is
        the same copy the identifier tests use - so the replacement does not even have to be
        crafted to contain the nonce.

        It matters more than it did: this class also stopped grading an agreeing device and
        inode as proof, which leaves the nonce as the only proving mechanism. Unbound, it is
        the whole grading rather than one voice in it.
        """
        mine = self.store.locate()
        written = self.store.write_challenge(actor="parent")
        # Closed first so the committed challenge row is in the main file and the copy below
        # carries it; a live write-ahead log would leave the replacement without the nonce and
        # this would pass because the answer was MISSING rather than because it was refused.
        self.store.close()

        replacement = self.copy_store(os.path.join(self.tmp, "replacement"))
        os.replace(
            os.path.join(replacement, "relay.sqlite3"),
            os.path.join(self.a, "relay.sqlite3"),
        )

        answer = nonce_lookup(resolve_state_dir(self.a), written["nonce"])

        # The defect first, so a failure says what it is: the identity under this comparison
        # is the one measured from A, and the answer came out of B.
        graded = compare_store(mine, nonce=answer)
        self.assertNotEqual(
            graded["sameStore"], "proven",
            "a nonce read from a database that replaced the measured one proved it",
        )
        self.assertIn("read from", graded["detail"], graded)

        # And the case is the one described rather than a read that simply failed.
        self.assertTrue(answer["found"], answer)
        self.assertNotEqual(
            (answer["device"], answer["inode"]), (mine["device"], mine["inode"]),
        )

    def test_a_name_added_after_the_probe_still_vetoes_a_found_nonce(self):
        """The hazard is the same whether the second name was there at the probe or arrived.

        The nonce answer carries the name count observed at ITS read, and grading only the
        probe's count took one half of the fresher measurement and left the other: both
        physical identifiers are unchanged by a hardlink created in between, the nonce is
        found through the original name, and a stale count of one cannot veto it.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        measured = probe(resolve_state_dir(self.a))["store"]
        self.assertEqual(measured["links"], 1, "the probe has to see one name first")

        other = os.path.join(self.tmp, "late-hardlink")
        os.makedirs(other)
        os.link(os.path.join(self.a, "relay.sqlite3"), os.path.join(other, "relay.sqlite3"))

        answer = nonce_lookup(resolve_state_dir(self.a), written["nonce"])

        graded = compare_store(measured, nonce=answer)
        self.assertEqual(
            graded["sameStore"], "unproven",
            "a second name created after the probe did not veto the nonce",
        )
        self.assertIn("names", graded["detail"], graded)

        # The case is the one described: the read saw the second name, the probe did not.
        self.assertTrue(answer["found"], answer)
        self.assertEqual(answer["links"], 2, answer)
        self.assertEqual(
            (answer["device"], answer["inode"]), (measured["device"], measured["inode"]),
        )

    def test_a_name_that_goes_away_during_the_read_is_still_counted(self):
        """Both of the read's own observations, not just the one it finished with.

        `nonce_lookup` identifies the file it holds open before it reads and again after it
        closes. A second name present at the open and unlinked before the close leaves both the
        caller's count and the closing count at one, while the opening count saw two - and a
        peer that already opened the removed alias can hold that connection and keep writing
        through its own write-ahead log. Carrying only the closing count kept half of what the
        function measured.

        The unlink is injected at the seam rather than raced: both counts are real counts of
        the real file, and the wrapper only decides WHEN the alias goes away, because the
        window is inside one call.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        measured = probe(resolve_state_dir(self.a))["store"]
        self.assertEqual(measured["links"], 1, "the caller has to see one name")

        alias = os.path.join(self.tmp, "vanishing.sqlite3")
        os.link(os.path.join(self.a, "relay.sqlite3"), alias)

        from codex_session_relay import store as store_module

        real = store_module._held_identity
        seen = []

        def observe(fd):
            answer = real(fd)
            seen.append(answer)
            if len(seen) == 1:
                os.unlink(alias)
            return answer

        store_module._held_identity = observe
        try:
            answer = nonce_lookup(resolve_state_dir(self.a), written["nonce"])
        finally:
            store_module._held_identity = real

        graded = compare_store(measured, nonce=answer)
        self.assertEqual(
            graded["sameStore"], "unproven",
            "a name seen only at the open did not veto the nonce",
        )
        self.assertIn("names", graded["detail"], graded)

        # The case is the one described: two real observations, two then one.
        self.assertEqual([count["links"] for count in seen], [2, 1], seen)
        self.assertTrue(answer["found"], answer)
        self.assertEqual(answer["links"], 2, answer)

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


class DescriptorIdentity(unittest.TestCase):
    """The read legs name the file they held open, or they refuse.

    Every case here lives in one window: between the moment something measures this store and
    the moment rows come back from it. Measuring at the pathname could not see into that window,
    because a replacement reverted inside it leaves both observations reporting the original
    inode while the rows came out of another file entirely.

    What is closed is every relocation that has already happened when the read asks. SQLite
    resolves the descriptor and opens the name it finds, so a rename timed inside that call is
    not closed and is recorded as a limit rather than asserted away here.
    """

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-descriptor-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.a = os.path.join(self.tmp, "a")
        os.makedirs(self.a)
        self.path = Path(self.a) / "relay.sqlite3"
        store = Store(self.path)
        self.written = store.write_challenge(actor="parent")
        self.mine = store.locate()
        store.close()

        # A whole second store, with its own identity and its own challenge row. It stands in
        # for the database an actor swaps onto this pathname, and it is a real store rather
        # than a broken file so that reading it would SUCCEED and be believed.
        self.b = os.path.join(self.tmp, "b")
        os.makedirs(self.b)
        self.interloper_path = os.path.join(self.b, "relay.sqlite3")
        other = Store(Path(self.interloper_path))
        self.interloper_nonce = other.write_challenge(actor="interloper")
        self.theirs = other.locate()
        other.close()
        self.assertNotEqual(self.theirs["inode"], self.mine["inode"])
        # Closing checkpoints and removes the sidecars, so moving the one file below moves the
        # whole store. Asserted rather than assumed: a left-behind log would strand the
        # challenge row and the swap would be believed for the wrong reason.
        for directory in (self.a, self.b):
            self.assertEqual(
                [name for name in os.listdir(directory) if name != "relay.sqlite3"], [],
                "a closed store left a sidecar behind, so moving the main file loses writes",
            )

    def swap_around_the_connect(self):
        """Stand the interloper at this pathname for the duration of the real sqlite3.connect.

        The wrapper decides WHEN, never WHAT: the real connect runs, on whatever the code under
        test asked it to open. On the first connect only:

          1. our database is renamed away, so the pathname is free;
          2. the interloper is renamed onto the pathname;
          3. the real connect runs;
          4. the interloper is moved off and ours is renamed back.

        Anything measuring at the pathname sees our store before and our store after, and both
        observations agree. What the connect opened was the interloper. That is the whole
        defect, and it is why two observations of a name cannot stand in for the file itself.
        """
        from codex_session_relay import store as store_module

        real = store_module.sqlite3.connect
        saved = os.path.join(self.tmp, "saved.sqlite3")
        parked = os.path.join(self.tmp, "parked.sqlite3")
        done = []

        def wrapper(*args, **kwargs):
            if done:
                return real(*args, **kwargs)
            done.append(True)
            os.rename(self.path, saved)
            os.rename(self.interloper_path, self.path)
            try:
                return real(*args, **kwargs)
            finally:
                os.rename(self.path, parked)
                os.rename(saved, self.path)

        patcher = mock.patch.object(store_module.sqlite3, "connect", wrapper)
        patcher.start()
        self.addCleanup(patcher.stop)
        return done

    def move_the_file_while_it_is_held(self, move):
        """Move our database after the descriptor is open and before anything is read.

        Injected inside os.open, which is the one moment the code under test holds the file and
        has not yet asked whether it is still this store.

        Restricted to the database's own path. probe opens a temporary file in the directory
        first, to measure writability by writing, and a seam that fired on that one moved the
        database before probe had even stat'd it - so the test passed on a missing-file path
        with the refusal it meant to exercise never reached.
        """
        from codex_session_relay import store as store_module

        real = store_module.os.open
        done = []

        def wrapper(path, flags, *args, **kwargs):
            fd = real(path, flags, *args, **kwargs)
            if not done and os.fspath(path) == os.fspath(self.path):
                done.append(True)
                move()
            return fd

        patcher = mock.patch.object(store_module.os, "open", wrapper)
        patcher.start()
        self.addCleanup(patcher.stop)
        return done

    def assertRefusedTheMove(self, detail):
        """The answer was withdrawn BECAUSE the store moved, whichever check noticed first.

        Two of them can: the closing `_relocation`, and the question put to the connection
        about which file it opened. Which one fires depends on whether SQLite needs the moved
        name before it is asked, so pinning the wording to one of them would make this test
        depend on that ordering rather than on the contract. What the contract says is that a
        move is named as the reason - never passed through as a bare I/O error on a database
        reported readable.
        """
        self.assertTrue(detail, "a withdrawn answer has to say why")
        self.assertTrue(
            "no longer the file at" in detail or "rather than the database at" in detail,
            f"the refusal did not name the move: {detail!r}",
        )

    def test_rows_read_through_a_swap_are_never_attributed_to_this_store(self):
        """The defect this class exists for, asserted on both halves of the answer.

        Against the pathname mechanism this fails: the connect opened the interloper, so the
        rows are the interloper's, while both observations of the name reported ours - an
        answer pairing one store's rows with another store's device and inode. Holding the file
        makes the rows ours, because the descriptor cannot become another file.
        """
        swapped = self.swap_around_the_connect()
        answer = read_only_rows(
            resolve_state_dir(self.a), "SELECT written_by FROM store_challenge ORDER BY nonce",
        )
        self.assertTrue(swapped, "the seam never fired, so this asserts nothing")

        writers = [row["written_by"] for row in answer["rows"]]
        self.assertNotIn(
            "interloper", writers,
            "rows out of the database that stood at this pathname were reported as this store's",
        )
        # Measured outcome, and the second of the two that are correct. SQLite resolves the
        # descriptor to the name the held inode had AT THE CONNECT - the one the swap moved it
        # to - and reads through its own descriptor from there. The seam then renames that name
        # away again, so the log the read still needs is gone and it fails attributably: no
        # rows, no identity, and a detail every caller already treats as "we could not look".
        # What it must never do is hand back the interloper's rows under our device and inode,
        # which is exactly what two observations of the pathname did here.
        self.assertEqual(answer["rows"], [], answer)
        self.assertIsNone(answer["device"], "a failed read must not carry an identity")
        self.assertTrue(answer["detail"], answer)

    def test_a_nonce_read_through_a_swap_cannot_prove_the_measured_store(self):
        """The same window, aimed at the only evidence compare_store grades as proof.

        The interloper's own nonce is looked up. Against the pathname mechanism it is FOUND -
        in the interloper - and comes back wearing our device and inode, which compare_store
        then grades proven: a database nobody measured proving the one that was.
        """
        swapped = self.swap_around_the_connect()
        answer = nonce_lookup(resolve_state_dir(self.a), self.interloper_nonce["nonce"])
        self.assertTrue(swapped, "the seam never fired, so this asserts nothing")

        self.assertFalse(
            answer["found"],
            "a nonce written into the database that stood at this pathname was reported as"
            " readable in this store",
        )
        graded = compare_store(
            self.mine, expect_inode=f"{self.mine['device']}:{self.mine['inode']}", nonce=answer,
        )
        self.assertNotEqual(graded["sameStore"], "proven", graded)

    def test_a_database_that_lost_its_name_while_held_is_refused(self):
        """Fail closed, and say which of the two things happened.

        Replacing the file at the pathname leaves the held inode with no name at all. SQLite
        answers that the same way it answers a store that was never there, so the refusal is
        named here instead of passed through as a message about something missing.
        """
        fired = self.move_the_file_while_it_is_held(
            lambda: os.replace(self.interloper_path, self.path))
        answer = read_only_rows(resolve_state_dir(self.a), "SELECT 1 AS one")
        self.assertTrue(fired, "the seam never fired, so this asserts nothing")

        self.assertFalse(answer["readable"], answer)
        self.assertEqual(answer["rows"], [])
        self.assertIsNone(answer["device"], "a refused read must not carry an identity")
        self.assertIn("no longer the file at", answer["detail"], answer)

    def test_a_relocated_database_is_refused_before_it_can_leave_a_log_behind(self):
        """The case that makes the descriptor insufficient on its own.

        SQLite resolves the descriptor to whatever name the held inode has NOW and opens that
        name, which is what puts the write-ahead log beside the real file in the ordinary case.
        Renamed, that name is the wrong one: measured on this host on 2026-09-22, a read
        through a relocated name raised a disk I/O error against a live log AND created a stray
        log beside the new name - a write from a command that promises none.

        So the descriptor is required to still name this store, asked before any connection is
        opened. Asserted on the side effect as well as the answer, because a refusal that still
        wrote something is not a refusal.
        """
        moved = os.path.join(self.tmp, "moved.sqlite3")
        fired = self.move_the_file_while_it_is_held(lambda: os.rename(self.path, moved))
        answer = read_only_rows(resolve_state_dir(self.a), "SELECT 1 AS one")
        self.assertTrue(fired, "the seam never fired, so this asserts nothing")

        self.assertFalse(answer["readable"], answer)
        self.assertIn("no longer the file at", answer["detail"], answer)
        self.assertEqual(
            [name for name in os.listdir(self.tmp) if name.startswith("moved.sqlite3-")], [],
            "the refused read still opened the relocated name and left a log beside it",
        )

    def test_the_probe_re_asks_between_its_read_and_its_write(self):
        """One check at the open is not enough, because the probe opens twice.

        The read connection and the write probe go through one descriptor, so a rename between
        them puts the write probe on a relocated name - the same failed read and the same stray
        log as above. That second question is also the CLOSING one for the read that just
        happened: a store that moved during it leaves behind an identity a caller reads as "the
        store at this path", so what the read published is withdrawn rather than reported.
        """
        from codex_session_relay import store as store_module

        moved = os.path.join(self.tmp, "moved.sqlite3")
        real = store_module.sqlite3.connect
        done = []

        def wrapper(*args, **kwargs):
            connection = real(*args, **kwargs)
            if not done:
                done.append(True)
                os.rename(self.path, moved)
            return connection

        with mock.patch.object(store_module.sqlite3, "connect", wrapper):
            report = probe(resolve_state_dir(self.a))
        self.addCleanup(os.rename, moved, self.path)

        self.assertTrue(done, "the seam never fired, so this asserts nothing")
        self.assertFalse(report["access"]["dbWritable"], report)
        self.assertFalse(report["access"]["dbReadable"], report)
        self.assertIsNone(report["store"]["storeId"], report)
        self.assertIsNone(report["store"]["inode"], report)
        self.assertIn("moved while it was being read", report["access"]["detail"], report)
        self.assertEqual(
            [name for name in os.listdir(self.tmp) if name.startswith("moved.sqlite3-")], [],
            "the refused write probe still opened the relocated name",
        )

    def test_the_ordinary_probe_describes_the_file_it_held(self):
        """The normal case, including where SQLite puts the log it needs.

        realPath is the descriptor's own name rather than a second resolution of the path, so a
        reported path and a reported inode are two facts about one file. The write probe runs
        through the same descriptor and leaves nothing behind.
        """
        report = probe(resolve_state_dir(self.a))
        store = report["store"]
        self.assertTrue(report["access"]["dbReadable"], report)
        self.assertTrue(report["access"]["dbWritable"], report)
        self.assertIsNone(report["access"]["detail"], report)
        self.assertEqual(store["realPath"], str(self.path.resolve()))
        self.assertEqual(
            (store["device"], store["inode"]), (self.mine["device"], self.mine["inode"]))
        self.assertEqual(store["storeId"], self.mine["storeId"])
        self.assertEqual(
            [name for name in os.listdir(self.a) if name != "relay.sqlite3"], [],
            "diagnosis left a write-ahead log behind",
        )

    def test_a_store_reached_through_a_symlinked_directory_still_reads(self):
        """The descriptor resolves to the real name, which is the name the store lives at."""
        alias = os.path.join(self.tmp, "alias")
        os.symlink(self.a, alias)
        answer = nonce_lookup(resolve_state_dir(alias), self.written["nonce"])
        self.assertTrue(answer["found"], answer)
        self.assertEqual(
            (answer["device"], answer["inode"]), (self.mine["device"], self.mine["inode"]))

    def test_the_descriptor_is_released_on_every_path(self):
        """Held files are closed whether the read answers, fails its query, or is refused.

        Counted rather than reasoned about: a leak here would exhaust a long-lived daemon
        slowly, far from the call that caused it.
        """
        def held():
            return len(os.listdir("/proc/self/fd"))

        from codex_session_relay import store as store_module

        before = held()
        for _ in range(25):
            read_only_rows(resolve_state_dir(self.a), "SELECT 1 AS one")
            read_only_rows(resolve_state_dir(self.a), "SELECT * FROM no_such_table")
            nonce_lookup(resolve_state_dir(self.a), "absent")
            read_only_rows(
                resolve_state_dir(os.path.join(self.tmp, "nothing-here")), "SELECT 1 AS one")
            with mock.patch.object(store_module, "_relocation", lambda fd, expected: "moved"):
                # The refusal path closes the descriptor _hold_database took, which is the one
                # branch that returns without ever reaching a connection.
                read_only_rows(resolve_state_dir(self.a), "SELECT 1 AS one")
                nonce_lookup(resolve_state_dir(self.a), "absent")
        self.assertLessEqual(held(), before + 1, "a descriptor was not released")

    def test_a_missing_store_is_answered_rather_than_raised(self):
        """Every failure is a field, and the refusal names what could not be done."""
        answer = read_only_rows(
            resolve_state_dir(os.path.join(self.tmp, "nothing-here")), "SELECT 1 AS one")
        self.assertFalse(answer["readable"])
        self.assertEqual(answer["rows"], [])
        self.assertIsNone(answer["device"])
        self.assertTrue(answer["detail"])

    def test_a_held_database_that_cannot_be_identified_is_not_described(self):
        """Fail closed rather than pair one file's inode with another file's identity.

        The stat that decides a file is here measures whatever the PATH reached. If the
        descriptor then cannot be identified, carrying that earlier inode while reading the
        store id through the descriptor would report exactly the hybrid this whole change
        exists to remove - and a stat/open race can produce it.
        """
        from codex_session_relay import store as store_module

        with mock.patch.object(store_module, "_held_identity", lambda fd: None):
            report = probe(resolve_state_dir(self.a))

        store = report["store"]
        self.assertTrue(store["exists"], "the file is still here and that is still reportable")
        self.assertIsNone(store["device"])
        self.assertIsNone(store["inode"])
        self.assertIsNone(store["links"])
        self.assertIsNone(store["storeId"], "an identity was read without a file to attach it to")
        self.assertFalse(report["access"]["dbReadable"], report)
        self.assertFalse(report["access"]["dbWritable"], report)
        self.assertIn("could not be identified", report["access"]["detail"], report)

    def test_a_refused_hold_states_no_identity_at_all(self):
        """A read that could not be bound must not be graded as a definite mismatch.

        The stat that decides a file is here measures whatever the PATH reached. Reporting that
        as the physical identity while refusing the read hands compare_store a device and inode
        it grades as conclusive WHEN THEY DIFFER, so "this could not be bound to a file" came
        back as "this is definitely a different store". Unproven is the honest verdict, and it
        is the one absence has always been given here.
        """
        from codex_session_relay import store as store_module

        moved = os.path.join(self.tmp, "moved.sqlite3")
        with mock.patch.object(
            store_module, "_hold_database", lambda path: (None, None, "refused for the test")
        ):
            report = probe(resolve_state_dir(self.a))

        store = report["store"]
        self.assertTrue(store["exists"], "the file is here and that stays reportable")
        self.assertIsNone(store["device"])
        self.assertIsNone(store["inode"])
        self.assertIsNone(store["links"])
        self.assertFalse(report["access"]["dbReadable"], report)
        self.assertEqual(
            compare_store(
                store, expect_inode=f"{self.mine['device']}:{self.mine['inode']}",
            )["sameStore"],
            "unproven",
            "a refused read was graded as a definite mismatch on a pathname observation",
        )
        self.assertFalse(os.path.exists(moved))

    def test_a_relocated_store_is_unproven_rather_than_a_different_store(self):
        """The same rule through the real refusal, not an injected one."""
        moved = os.path.join(self.tmp, "moved.sqlite3")
        fired = self.move_the_file_while_it_is_held(lambda: os.rename(self.path, moved))
        report = probe(resolve_state_dir(self.a))
        self.addCleanup(os.rename, moved, self.path)
        self.assertTrue(fired, "the seam never fired, so this asserts nothing")

        store = report["store"]
        self.assertTrue(
            store["exists"],
            "the database was moved before probe stat'd it, so this exercised a missing file"
            " rather than a refused hold",
        )
        self.assertIn("could not be held open", report["access"]["detail"], report)
        self.assertIsNone(store["device"], report)
        self.assertIsNone(store["inode"], report)
        self.assertIsNone(store["links"], report)
        self.assertFalse(report["access"]["dbReadable"], report)
        self.assertEqual(
            compare_store(
                store, expect_inode=f"{self.mine['device']}:{self.mine['inode']}",
            )["sameStore"],
            "unproven",
            report,
        )

    def test_a_store_that_moves_during_the_read_withdraws_the_answer(self):
        """The closing question, which is what makes the answer about a whole read.

        The pre-connect check says the file was this store when the read started. Without a
        closing one, a rename during the read still returns rows and an identity a caller reads
        as "the store at this path". Together the two say the file was the one at this pathname
        for the whole read, or there is no answer.
        """
        from codex_session_relay import store as store_module

        moved = os.path.join(self.tmp, "moved.sqlite3")
        real = store_module.sqlite3.connect
        done = []

        def wrapper(*args, **kwargs):
            connection = real(*args, **kwargs)
            if not done:
                done.append(True)
                os.rename(self.path, moved)
            return connection

        with mock.patch.object(store_module.sqlite3, "connect", wrapper):
            answer = read_only_rows(
                resolve_state_dir(self.a), "SELECT written_by FROM store_challenge")
        self.addCleanup(os.rename, moved, self.path)

        self.assertTrue(done, "the seam never fired, so this asserts nothing")
        self.assertFalse(answer["readable"], answer)
        self.assertEqual(answer["rows"], [])
        self.assertIsNone(answer["device"], "a withdrawn answer must not carry an identity")
        self.assertRefusedTheMove(answer["detail"])

    def test_a_nonce_read_while_the_store_moves_is_not_proof(self):
        """The same closing question on the only evidence compare_store grades as proof."""
        from codex_session_relay import store as store_module

        moved = os.path.join(self.tmp, "moved.sqlite3")
        real = store_module.sqlite3.connect
        done = []

        def wrapper(*args, **kwargs):
            connection = real(*args, **kwargs)
            if not done:
                done.append(True)
                os.rename(self.path, moved)
            return connection

        with mock.patch.object(store_module.sqlite3, "connect", wrapper):
            answer = nonce_lookup(resolve_state_dir(self.a), self.written["nonce"])
        self.addCleanup(os.rename, moved, self.path)

        self.assertTrue(done, "the seam never fired, so this asserts nothing")
        self.assertFalse(answer["readable"], answer)
        self.assertFalse(answer["found"], answer)
        self.assertNotEqual(
            compare_store(
                self.mine, expect_inode=f"{self.mine['device']}:{self.mine['inode']}",
                nonce=answer,
            )["sameStore"],
            "proven",
        )
