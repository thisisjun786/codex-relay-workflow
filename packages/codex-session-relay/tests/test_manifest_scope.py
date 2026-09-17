"""MANIFEST-CANON-01, path containment, and the artifact-read stability boundary."""

import fcntl
import hashlib
import mmap
import os
import shutil
import unittest

from codex_session_relay import manifest
from codex_session_relay.errors import RefusalReason, ScopeError
from codex_session_relay.manifest import Entry
from codex_session_relay.scope import (
    PathBindingMode,
    at_least,
    is_within,
    normalize_declared_path,
    open_authorized,
)

from .support import RelayTestCase


class Canonicalization(unittest.TestCase):
    def test_serialization_matches_the_contract_exactly(self):
        entries = [Entry("/b/two", "b" * 64), Entry("/a/one", "a" * 64)]
        payload = manifest.canonical_payload(entries)
        self.assertEqual(payload, f"/a/one:{'a' * 64}\n/b/two:{'b' * 64}")
        self.assertFalse(payload.endswith("\n"))
        self.assertEqual(
            manifest.revision_hash(entries),
            hashlib.sha256(payload.encode("utf-8")).hexdigest(),
        )

    def test_single_entry_has_no_separator(self):
        self.assertEqual(manifest.canonical_payload([Entry("/a", "a" * 64)]), f"/a:{'a' * 64}")

    def test_empty_manifest_is_the_empty_string_not_the_sentinel(self):
        self.assertEqual(manifest.canonical_payload([]), "")
        self.assertEqual(
            manifest.revision_hash([]), hashlib.sha256(b"").hexdigest()
        )
        self.assertNotEqual(manifest.revision_hash([]), "0" * 64)

    def test_sort_is_byte_wise_on_the_path(self):
        entries = [Entry("/a/b", "1" * 64), Entry("/a/B", "2" * 64), Entry("/a/-", "3" * 64)]
        order = [line.split(":")[0] for line in manifest.canonical_payload(entries).split("\n")]
        self.assertEqual(order, sorted(order, key=str.encode))

    def test_relative_and_unnormalized_paths_are_refused(self):
        for bad in ("relative/path", "/a/../b", "/a/b/", "/a/./b", "~/a"):
            with self.assertRaises(ScopeError, msg=bad):
                normalize_declared_path(bad)


class Containment(unittest.TestCase):
    def test_component_containment_not_string_prefix(self):
        self.assertTrue(is_within("/a/b", "/a/b"))
        self.assertTrue(is_within("/a/b", "/a/b/c"))
        self.assertFalse(is_within("/a/b", "/a/bc"))
        self.assertFalse(is_within("/a/b", "/a/bc/d"))
        self.assertFalse(is_within("/a/b", "/a"))


class PinnedTraversal(RelayTestCase):
    def test_symlink_leaf_is_refused(self):
        real = self.artifact("real.txt", "payload")
        link = os.path.join(self.root, "link.txt")
        os.symlink(real, link)
        self.assertRefused(
            RefusalReason.SYMLINK_COMPONENT, manifest.hash_path, link, [self.root]
        )

    def test_symlink_intermediate_component_is_refused(self):
        os.makedirs(os.path.join(self.root, "real"))
        self.artifact("real/file.txt", "payload")
        os.symlink(os.path.join(self.root, "real"), os.path.join(self.root, "alias"))
        self.assertRefused(
            RefusalReason.SYMLINK_COMPONENT,
            manifest.hash_path, os.path.join(self.root, "alias", "file.txt"), [self.root],
        )

    def test_symlinked_root_itself_is_refused(self):
        # The walk starts at "/" so even the root's own components are pinned.
        real_root = os.path.join(self.tmp, "realroot")
        os.makedirs(real_root)
        with open(os.path.join(real_root, "f.txt"), "w") as handle:
            handle.write("payload")
        alias_root = os.path.join(self.tmp, "aliasroot")
        os.symlink(real_root, alias_root)
        self.assertRefused(
            RefusalReason.SYMLINK_COMPONENT,
            manifest.hash_path, os.path.join(alias_root, "f.txt"), [alias_root],
        )

    def test_absolute_symlink_target_outside_the_roots_is_refused(self):
        outside = os.path.join(self.tmp, "outside.txt")
        with open(outside, "w") as handle:
            handle.write("secret")
        link = os.path.join(self.root, "inside.txt")
        os.symlink(outside, link)
        self.assertRefused(
            RefusalReason.SYMLINK_COMPONENT, manifest.hash_path, link, [self.root]
        )

    def test_path_outside_the_roots_is_refused_before_any_read(self):
        outside = os.path.join(self.tmp, "elsewhere.txt")
        with open(outside, "w") as handle:
            handle.write("secret")
        self.assertRefused(
            RefusalReason.SCOPE_ESCAPE, manifest.hash_path, outside, [self.root]
        )

    def test_missing_component_reports_path_changed(self):
        self.assertRefused(
            RefusalReason.PATH_CHANGED,
            manifest.hash_path, os.path.join(self.root, "gone", "f.txt"), [self.root],
        )

    def test_directory_is_not_an_artifact(self):
        self.assertRefused(
            RefusalReason.NOT_A_REGULAR_FILE, manifest.hash_path, self.root, [self.root]
        )


class StabilityBoundary(RelayTestCase):
    """Three separate windows, plus the mapped-writer case the reviewer asked for."""

    def test_relocation_before_the_walk_is_refused_by_the_walk(self):
        os.makedirs(os.path.join(self.root, "sub"))
        path = self.artifact("sub/f.txt", "payload")
        os.rename(os.path.join(self.root, "sub"), os.path.join(self.tmp, "moved"))
        self.assertRefused(RefusalReason.PATH_CHANGED, manifest.hash_path, path, [self.root])

    def test_relocation_after_the_first_readlink_is_refused(self):
        os.makedirs(os.path.join(self.root, "sub"))
        path = self.artifact("sub/f.txt", "payload")
        with open_authorized(path, [self.root]) as handle:
            # The descriptor is already open and authorized. Move its ancestor now: the
            # descriptor keeps reading the same inode, but it is no longer at the path we
            # authorized, and the post-read check is what notices.
            os.rename(os.path.join(self.root, "sub"), os.path.join(self.tmp, "moved2"))
            with self.assertRaises(ScopeError) as caught:
                manifest.hash_authorized(handle)
            self.assertEqual(caught.exception.reason, RefusalReason.PATH_RELOCATED)

    def test_ordinary_write_between_passes_is_detected(self):
        path = self.artifact("f.txt", "original content")

        def mutate():
            with open(path, "w", encoding="utf-8") as handle:
                handle.write("different content entirely")

        with open_authorized(path, [self.root]) as handle:
            with self.assertRaises(ScopeError) as caught:
                manifest.hash_authorized(handle, between_passes=mutate)
            self.assertEqual(caught.exception.reason, RefusalReason.ARTIFACT_MUTATED_DURING_READ)

    def test_mapped_writer_between_passes_is_detected(self):
        """A MAP_SHARED writer can keep the size identical and defer timestamp updates.

        This is the case the stat comparison cannot be relied on to catch, which is why the
        file is hashed twice from the same descriptor. Holding the file open read-write also
        means no lease can be taken, so this exercises the best-effort tier deliberately.
        """
        path = self.artifact("mapped.txt", "A" * 4096)
        writable = os.open(path, os.O_RDWR)
        self.addCleanup(os.close, writable)
        mapping = mmap.mmap(writable, 4096, access=mmap.ACCESS_WRITE)
        self.addCleanup(mapping.close)

        holder = {}

        def mutate():
            mapping[0:1] = b"B"
            mapping.flush()
            # Re-baseline the metadata snapshot to whatever the mutation left behind. That
            # is the case this test exists for: a mapped writer whose timestamps the stat
            # comparison cannot be relied on to catch. With the metadata check neutralised,
            # only the two-pass digest comparison can still detect the change.
            handle = holder["handle"]
            handle._snapshot = handle._stat_tuple(os.fstat(handle.fd))

        with open_authorized(path, [self.root]) as handle:
            holder["handle"] = handle
            self.assertEqual(handle.binding.mode, PathBindingMode.BEST_EFFORT_DETECTION)
            with self.assertRaises(ScopeError) as caught:
                manifest.hash_authorized(handle, between_passes=mutate)
            self.assertEqual(caught.exception.reason, RefusalReason.ARTIFACT_MUTATED_DURING_READ)
            # Prove the metadata detector really was neutralised, so the digest comparison
            # is what raised.
            self.assertEqual(handle._stat_tuple(os.fstat(handle.fd)), handle._snapshot)

    def test_a_quiet_read_succeeds_and_reports_its_tier(self):
        path = self.artifact("quiet.txt", "payload")
        digest, size, binding = manifest.hash_path(path, [self.root])
        self.assertEqual(digest, hashlib.sha256(b"payload").hexdigest())
        self.assertEqual(size, 7)
        self.assertIn(
            binding.mode,
            (PathBindingMode.LEASE_ENFORCED, PathBindingMode.BEST_EFFORT_DETECTION),
        )
        self.assertTrue(binding.lease_detail)


class LeaseTier(RelayTestCase):
    def test_a_pre_existing_writable_open_degrades_to_best_effort(self):
        path = self.artifact("held.txt", "payload")
        writable = os.open(path, os.O_RDWR)
        self.addCleanup(os.close, writable)
        with open_authorized(path, [self.root], allow_lease=True) as handle:
            self.assertEqual(handle.binding.mode, PathBindingMode.BEST_EFFORT_DETECTION)
            self.assertIn("writable open", handle.binding.lease_detail)

    def test_lease_is_not_attempted_by_default(self):
        # Tier 1 is opt-in: a held read lease blocks any writer for the kernel lease-break
        # timeout, so hashing a file must not stall unrelated processes.
        path = self.artifact("nolease.txt", "payload")
        with open_authorized(path, [self.root], allow_lease=False) as handle:
            self.assertEqual(handle.binding.mode, PathBindingMode.BEST_EFFORT_DETECTION)
            self.assertEqual(handle.binding.lease_detail, "lease not attempted")

    def test_a_broken_lease_refuses_the_read(self):
        """The break flag is set directly rather than by racing a real writer.

        A second opener would block on the kernel's lease-break timeout, and doing it from
        this process would wait on itself, so the detection path is exercised at its seam.
        """
        path = self.artifact("lease.txt", "payload")
        with open_authorized(path, [self.root], allow_lease=True) as handle:
            if handle.binding.mode is not PathBindingMode.LEASE_ENFORCED:
                self.skipTest("this filesystem does not grant read leases")
            # Remove the lease behind the guard's back, which is what a broken lease looks
            # like to F_GETLEASE. The SIGIO flag is left alone, so this exercises the kernel
            # query rather than our own bookkeeping.
            fcntl.fcntl(handle.fd, fcntl.F_SETLEASE, fcntl.F_UNLCK)
            self.assertFalse(handle._lease.broken)
            self.assertFalse(handle._lease.still_held())
            with self.assertRaises(ScopeError) as caught:
                handle.verify_stable()
            self.assertEqual(caught.exception.reason, RefusalReason.ARTIFACT_LEASE_BROKEN)

    def test_the_sigio_flag_alone_also_refuses(self):
        path = self.artifact("lease2.txt", "payload")
        with open_authorized(path, [self.root], allow_lease=True) as handle:
            if handle.binding.mode is not PathBindingMode.LEASE_ENFORCED:
                self.skipTest("this filesystem does not grant read leases")
            handle._lease.broken = True
            with self.assertRaises(ScopeError) as caught:
                handle.verify_stable()
            self.assertEqual(caught.exception.reason, RefusalReason.ARTIFACT_LEASE_BROKEN)

    def test_lease_is_granted_when_asked_for_on_a_supporting_filesystem(self):
        path = self.artifact("wantlease.txt", "payload")
        with open_authorized(path, [self.root], allow_lease=True) as handle:
            if handle.binding.mode is not PathBindingMode.LEASE_ENFORCED:
                self.skipTest("this filesystem does not grant read leases")
            self.assertIn("lease held", handle.binding.lease_detail)
            digest, _size = manifest.hash_authorized(handle)
            self.assertEqual(digest, hashlib.sha256(b"payload").hexdigest())

    def test_tier_ordering(self):
        self.assertTrue(at_least(PathBindingMode.LEASE_ENFORCED, PathBindingMode.BEST_EFFORT_DETECTION))
        self.assertFalse(at_least(PathBindingMode.BEST_EFFORT_DETECTION, PathBindingMode.LEASE_ENFORCED))


class VerifyAgainstDisk(RelayTestCase):
    def test_truncated_artifact_is_reported(self):
        path = self.artifact("a.txt", "the full contents")
        entries, _ = manifest.build([path], [self.root])
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("trunc")
        problems, _ = manifest.verify_against_disk(entries, [self.root])
        self.assertTrue(problems)
        self.assertIn("hash to", problems[0])

    def test_missing_artifact_is_reported(self):
        path = self.artifact("b.txt", "payload")
        entries, _ = manifest.build([path], [self.root])
        os.remove(path)
        problems, _ = manifest.verify_against_disk(entries, [self.root])
        self.assertTrue(problems)
        self.assertIn("path_changed", problems[0])

    def test_unchanged_artifacts_verify_clean(self):
        paths = [self.artifact("c.txt", "one"), self.artifact("d.txt", "two")]
        entries, _ = manifest.build(paths, [self.root])
        problems, bindings = manifest.verify_against_disk(entries, [self.root])
        self.assertEqual(problems, [])
        self.assertEqual(len(bindings), 2)


class FrozenCopy(RelayTestCase):
    def test_relocation_is_verifiable_through_the_frozen_copy(self):
        path = self.artifact("deliver.txt", "the delivered bytes")
        entries, _ = manifest.build([path], [self.root])
        digest = manifest.revision_hash(entries)
        reference = os.path.join(self.tmp, "frozen")
        manifest.freeze(entries, reference)

        # The live tree moves on: the deliverable is replaced by a later revision.
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("a later revision")
        problems, _ = manifest.verify_against_disk(entries, [self.root])
        self.assertTrue(problems, "the live files no longer match, which is the point")

        frozen_digest, frozen_problems = manifest.verify_frozen(reference, entries)
        self.assertEqual(frozen_problems, [])
        self.assertEqual(frozen_digest, digest, "freezing must not change the digest")

    def test_frozen_copy_keeps_the_original_declared_paths(self):
        path = self.artifact("keep.txt", "bytes")
        entries, _ = manifest.build([path], [self.root])
        reference = os.path.join(self.tmp, "frozen2")
        manifest.freeze(entries, reference)
        shutil.rmtree(self.root)
        frozen_digest, problems = manifest.verify_frozen(reference)
        self.assertEqual(problems, [])
        self.assertEqual(frozen_digest, manifest.revision_hash(entries))

    def test_tampered_frozen_bytes_are_reported(self):
        path = self.artifact("tamper.txt", "bytes")
        entries, _ = manifest.build([path], [self.root])
        reference = os.path.join(self.tmp, "frozen3")
        manifest.freeze(entries, reference)
        blob = os.path.join(reference, "files", entries[0].sha256)
        os.chmod(blob, 0o600)
        with open(blob, "wb") as handle:
            handle.write(b"tampered")
        _digest, problems = manifest.verify_frozen(reference, entries)
        self.assertTrue(problems)


class FrozenAccessIsNotFrozenDisagreement(RelayTestCase):
    """verify_frozen catches its access errors and returns them as problem strings.

    That is the right contract for the intake, which only asks whether the receipt verified. It is
    not enough for a caller deciding between "the deliverable changed" and "I could not compare
    it", because those have different repairs. The detailed form answers that question without
    moving what verify_frozen says, and these tests hold both halves of that claim.
    """

    def frozen(self, name, text="the delivered bytes"):
        path = self.artifact(name + ".txt", text)
        entries, _ = manifest.build([path], [self.root])
        reference = os.path.join(self.tmp, name)
        manifest.freeze(entries, reference)
        return path, entries, reference

    def unreachable_blobs(self, reference):
        """Make the blob directory unopenable.

        It has to be a real access failure. A regular file where the directory belongs raises
        ENOTDIR, which scope interprets as a structural answer about the snapshot rather than as a
        failure to look, and that is the distinction this class exists to hold.
        """
        files = os.path.join(reference, "files")
        self.addCleanup(os.chmod, files, 0o700)
        os.chmod(files, 0o000)

    @unittest.skipIf(os.geteuid() == 0, "root bypasses the permission this depends on")
    def test_an_unreachable_frozen_blob_is_named_as_an_access_failure(self):
        _path, entries, reference = self.frozen("frozen-access")
        self.unreachable_blobs(reference)
        _digest, problems, unreadable = manifest.verify_frozen_detailed(reference, entries)
        self.assertTrue(problems)
        self.assertEqual(unreadable, problems)

    def test_a_deleted_frozen_blob_is_a_broken_snapshot_not_an_access_failure(self):
        """Absence is as definitive here as a vanished live artifact, and it answers the same way."""
        _path, entries, reference = self.frozen("frozen-deleted-blob")
        os.remove(os.path.join(reference, "files", entries[0].sha256))
        _digest, problems, unreadable = manifest.verify_frozen_detailed(reference, entries)
        self.assertTrue(problems)
        self.assertEqual(unreadable, [])

    def test_a_blob_directory_replaced_by_a_file_is_a_broken_snapshot(self):
        """ENOTDIR is interpreted by scope, so it is an answer about the snapshot, not a failure."""
        _path, entries, reference = self.frozen("frozen-not-a-directory")
        shutil.rmtree(os.path.join(reference, "files"))
        with open(os.path.join(reference, "files"), "w", encoding="utf-8") as handle:
            handle.write("not a directory")
        _digest, problems, unreadable = manifest.verify_frozen_detailed(reference, entries)
        self.assertTrue(problems)
        self.assertEqual(unreadable, [])

    def test_a_disagreeing_frozen_copy_is_not_an_access_failure(self):
        _path, entries, reference = self.frozen("frozen-disagree")
        blob = os.path.join(reference, "files", entries[0].sha256)
        os.chmod(blob, 0o600)
        with open(blob, "wb") as handle:
            handle.write(b"tampered")
        _digest, problems, unreadable = manifest.verify_frozen_detailed(reference, entries)
        self.assertTrue(problems)
        self.assertEqual(unreadable, [], "tampered bytes were read; they simply disagree")

    def test_an_absent_frozen_copy_is_not_an_access_failure(self):
        _digest, problems, unreadable = manifest.verify_frozen_detailed(
            os.path.join(self.tmp, "never-frozen")
        )
        self.assertTrue(problems)
        self.assertEqual(unreadable, [])

    @unittest.skipIf(os.geteuid() == 0, "root bypasses the permission this depends on")
    def test_an_unreachable_frozen_directory_raises_rather_than_answering_absent(self):
        """Path.is_file() does not swallow this one, and that is worth pinning down.

        On this interpreter it raises PermissionError instead of returning False, so the
        three-state probe beside it never runs for a blocked parent. The error does not disappear:
        it leaves as an exception, which the guard's classification boundary turns into an
        unverifiable deliverable. The failure is reported either way; which mechanism reports it
        depends on the interpreter, so it is asserted rather than assumed.
        """
        holder = os.path.join(self.tmp, "blocked-holder")
        os.makedirs(os.path.join(holder, "frozen"))
        self.addCleanup(os.chmod, holder, 0o700)
        os.chmod(holder, 0o000)
        with self.assertRaises(OSError):
            manifest.verify_frozen_detailed(os.path.join(holder, "frozen"))

    def test_a_frozen_reference_that_is_not_a_directory_is_not_an_access_failure(self):
        """ENOTDIR is an errno scope interprets, so it answers about the reference itself."""
        reference = os.path.join(self.tmp, "frozen-dir-is-a-file")
        with open(reference, "w", encoding="utf-8") as handle:
            handle.write("a file where the frozen directory belongs")
        _digest, problems, unreadable = manifest.verify_frozen_detailed(reference)
        self.assertTrue(problems)
        self.assertEqual(unreadable, [])

    def test_the_two_value_form_answers_exactly_what_it_always_did(self):
        """The invariance the intake depends on, held across every branch that produces it."""
        cases = {}
        _p, entries, good = self.frozen("equiv-good")
        cases["a frozen copy that verifies"] = (good, entries)
        _p2, entries2, broken = self.frozen("equiv-unreachable")
        os.remove(os.path.join(broken, "files", entries2[0].sha256))
        cases["a missing blob"] = (broken, entries2)
        _p3, entries3, tampered = self.frozen("equiv-tampered")
        blob = os.path.join(tampered, "files", entries3[0].sha256)
        os.chmod(blob, 0o600)
        with open(blob, "wb") as handle:
            handle.write(b"tampered")
        cases["tampered frozen bytes"] = (tampered, entries3)
        cases["no frozen copy at all"] = (os.path.join(self.tmp, "equiv-absent"), None)
        _p4, _e4, other = self.frozen("equiv-other", text="different bytes")
        cases["a frozen copy of something else"] = (other, entries)

        for label, (reference, entries_for) in cases.items():
            with self.subTest(label):
                two = manifest.verify_frozen(reference, entries_for)
                detailed = manifest.verify_frozen_detailed(reference, entries_for)
                self.assertEqual(two, detailed[:2])


class IntakeBehaviourIsUnchangedByTheAccessSplit(RelayTestCase):
    """The intake asks verify_frozen one question and must keep getting the same answer.

    verify_frozen_detailed was added beside it rather than inside it precisely so this holds. The
    tests above prove the two forms agree; these prove the agreement reaches the intake, which is
    the caller that actually decides whether a receipt is admitted.
    """

    def moved_on(self, name):
        """A receipt whose live bytes have moved on, with a frozen copy behind it."""
        relationship = self.register()
        path = self.artifact(name + ".txt", "the delivered bytes")
        payload = self.ready_payload(relationship, [path])
        entries = [manifest.Entry.from_record(record) for record in payload["manifest"]]
        reference = os.path.join(self.tmp, name)
        manifest.freeze(entries, reference)
        payload["manifestRef"] = reference
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("a later revision")
        return payload, reference

    def test_a_good_frozen_copy_is_still_admitted_at_the_best_effort_tier(self):
        payload, _reference = self.moved_on("intake-frozen-good")
        self.accept(payload)
        row = self.store.one(
            "SELECT path_binding_mode FROM events WHERE event_id = ?", (payload["eventId"],)
        )
        self.assertEqual(row["path_binding_mode"], PathBindingMode.BEST_EFFORT_DETECTION.value)

    def test_an_unreachable_frozen_copy_is_still_refused_as_unverified(self):
        payload, reference = self.moved_on("intake-frozen-unreachable")
        shutil.rmtree(os.path.join(reference, "files"))
        with open(os.path.join(reference, "files"), "w", encoding="utf-8") as handle:
            handle.write("not a directory")
        self.assertRefused(RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload)
        self.assertIsNone(
            self.store.one("SELECT 1 AS hit FROM events WHERE event_id = ?", (payload["eventId"],))
        )

    def test_a_tampered_frozen_copy_is_still_refused_as_unverified(self):
        payload, reference = self.moved_on("intake-frozen-tampered")
        entries = [manifest.Entry.from_record(record) for record in payload["manifest"]]
        blob = os.path.join(reference, "files", entries[0].sha256)
        os.chmod(blob, 0o600)
        with open(blob, "wb") as handle:
            handle.write(b"tampered")
        self.assertRefused(RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload)

    def test_an_absent_frozen_copy_is_still_refused_as_unverified(self):
        payload, reference = self.moved_on("intake-frozen-absent")
        shutil.rmtree(reference)
        self.assertRefused(RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload)


class LiveAccessIsNotLiveDisagreement(RelayTestCase):
    """The same split on the live path, where scope reports through refusals rather than raising.

    scope._walk_error names the four errnos it understands and falls through to a generic
    SCOPE_ESCAPE for the rest. That fallthrough is not a scope decision; it is an open that did not
    work. verify_against_disk then catches the refusal and returns it as text, so nothing ever
    raises and an exception boundary cannot tell the two apart.
    """

    def declared(self, name, text="the delivered bytes"):
        path = self.artifact(name, text)
        entries, _ = manifest.build([path], [self.root])
        return path, entries

    @unittest.skipIf(os.geteuid() == 0, "root bypasses the permission this depends on")
    def test_a_component_that_cannot_be_opened_is_an_access_failure(self):
        path, entries = self.declared("locked/deliver.txt")
        locked = os.path.dirname(path)
        self.addCleanup(os.chmod, locked, 0o700)
        os.chmod(locked, 0o000)
        problems, _bindings, unreadable = manifest.verify_against_disk_detailed(
            entries, [self.root]
        )
        self.assertTrue(problems)
        self.assertEqual(unreadable, problems)

    def test_a_vanished_artifact_is_not_an_access_failure(self):
        path, entries = self.declared("gone.txt")
        os.remove(path)
        problems, _bindings, unreadable = manifest.verify_against_disk_detailed(
            entries, [self.root]
        )
        self.assertTrue(problems)
        self.assertEqual(unreadable, [], "a file that is not there is a readable answer")

    def test_changed_bytes_are_not_an_access_failure(self):
        path, entries = self.declared("changed.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("a later revision")
        problems, _bindings, unreadable = manifest.verify_against_disk_detailed(
            entries, [self.root]
        )
        self.assertTrue(problems)
        self.assertEqual(unreadable, [])

    def test_a_path_outside_the_roots_is_not_an_access_failure(self):
        """A scope decision is a decision. It was reached, so it is not a failure to look."""
        path, entries = self.declared("outside.txt")
        problems, _bindings, unreadable = manifest.verify_against_disk_detailed(
            entries, [os.path.join(self.tmp, "elsewhere")]
        )
        self.assertTrue(problems, path)
        self.assertEqual(unreadable, [])

    def test_the_two_value_form_answers_exactly_what_it_always_did(self):
        """The invariance the intake depends on, across every branch that produces a problem."""
        cases = {}
        good_path, good = self.declared("equiv-live-good.txt")
        cases["a deliverable that verifies"] = (good, [self.root])
        _gone_path, gone = self.declared("equiv-live-gone.txt")
        os.remove(_gone_path)
        cases["a vanished artifact"] = (gone, [self.root])
        changed_path, changed = self.declared("equiv-live-changed.txt")
        with open(changed_path, "w", encoding="utf-8") as handle:
            handle.write("a later revision")
        cases["changed bytes"] = (changed, [self.root])
        cases["a path outside the roots"] = (good, [os.path.join(self.tmp, "elsewhere")])
        cases["nothing declared at all"] = ([], [self.root])

        for label, (entries, roots) in cases.items():
            with self.subTest(label):
                two = manifest.verify_against_disk(entries, roots)
                detailed = manifest.verify_against_disk_detailed(entries, roots)
                self.assertEqual(two, detailed[:2])


class LiveIntakeBehaviourIsUnchangedByTheAccessSplit(RelayTestCase):
    """The same invariance claim as the frozen split, held at the intake boundary."""

    def test_a_verifying_deliverable_is_still_admitted(self):
        relationship = self.register()
        payload = self.ready_payload(relationship, [self.artifact("live-good.txt", "bytes")])
        self.accept(payload)
        self.assertIsNotNone(
            self.store.one("SELECT 1 AS hit FROM events WHERE event_id = ?", (payload["eventId"],))
        )

    def test_a_changed_deliverable_is_still_refused_as_unverified(self):
        relationship = self.register()
        path = self.artifact("live-changed.txt", "bytes")
        payload = self.ready_payload(relationship, [path])
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("a later revision")
        self.assertRefused(RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload)

    @unittest.skipIf(os.geteuid() == 0, "root bypasses the permission this depends on")
    def test_an_unreadable_deliverable_is_still_refused_as_unverified(self):
        relationship = self.register()
        path = self.artifact("locked-intake/live.txt", "bytes")
        payload = self.ready_payload(relationship, [path])
        locked = os.path.dirname(path)
        self.addCleanup(os.chmod, locked, 0o700)
        os.chmod(locked, 0o000)
        self.assertRefused(RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload)


if __name__ == "__main__":
    unittest.main()
