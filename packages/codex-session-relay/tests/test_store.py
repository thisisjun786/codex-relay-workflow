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
                mine, expect_inode=f"{mine['device']}:{mine['inode']}", nonce=here,
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
        self.assertEqual(
            compare_store(
                through_alias, expect_inode=f"{mine['device']}:{mine['inode']}", nonce=found,
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

        The bind mount itself is not exercised here. This host refuses an unprivileged mount
        namespace - `unshare -rm` fails writing `/proc/self/uid_map` - so what is asserted is
        the grading rule, not the mount. The rule is written not to depend on telling the two
        cases apart, which is why it is asserted on an ordinary single-named store.
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

        `nonce_lookup` stats the path before it opens and after it closes. A second name
        present at the open and unlinked before the close leaves both the caller's count and
        the closing count at one, while the opening count saw two - and a peer that already
        opened the removed alias can hold that connection and keep writing through its own
        write-ahead log. Carrying only the closing count kept half of what the function
        measured.

        The unlink is injected at the seam rather than raced: both stats are real stats of the
        real file, and the wrapper only decides WHEN the alias goes away, because the window
        is inside one call.
        """
        written = self.store.write_challenge(actor="parent")
        self.store.close()
        measured = probe(resolve_state_dir(self.a))["store"]
        self.assertEqual(measured["links"], 1, "the caller has to see one name")

        alias = os.path.join(self.tmp, "vanishing.sqlite3")
        os.link(os.path.join(self.a, "relay.sqlite3"), alias)

        from codex_session_relay import store as store_module

        real = store_module._path_identity
        seen = []

        def observe(path):
            answer = real(path)
            seen.append(answer)
            if len(seen) == 1:
                os.unlink(alias)
            return answer

        store_module._path_identity = observe
        try:
            answer = nonce_lookup(resolve_state_dir(self.a), written["nonce"])
        finally:
            store_module._path_identity = real

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
