"""MANIFEST-CANON-01: the deliverable digest, and how it is verified.

The absolute declared path is inside the digest, so identical bytes at a different
location hash differently. That is intentional: a manifest is evidence of a specific
delivery at a specific place, not a portable content address. A receipt that must survive
relocation carries a frozen copy, and the frozen copy keeps the ORIGINAL declared paths,
so freezing never changes the digest.
"""

import errno
import hashlib
import json
import os
import re
from dataclasses import dataclass
from pathlib import Path

from .errors import RefusalReason, ScopeError
from .scope import (
    AuthorizedFile,
    PathBindingMode,
    at_least,
    normalize_declared_path,
    open_authorized,
)

CHUNK = 1 << 20
DIGEST_RE = re.compile(r"^[0-9a-f]{64}$")
SERIALIZATION = (
    "MANIFEST-CANON-01: absolute normalized POSIX paths, sorted byte-wise, "
    "'<absolutePath>:<lowercase hex sha256>' per entry, joined with a single LF and no "
    "trailing newline, sha256 of that UTF-8 string"
)


@dataclass(frozen=True)
class Entry:
    path: str
    sha256: str
    bytes: int | None = None

    def to_record(self) -> dict:
        record = {"path": self.path, "sha256": self.sha256}
        if self.bytes is not None:
            record["bytes"] = self.bytes
        return record

    @staticmethod
    def from_record(record: dict) -> "Entry":
        return Entry(path=record["path"], sha256=record["sha256"], bytes=record.get("bytes"))


def canonical_payload(entries) -> str:
    lines = []
    for entry in sorted(entries, key=lambda e: e.path.encode("utf-8")):
        path = normalize_declared_path(entry.path)
        digest = entry.sha256
        if not isinstance(digest, str) or not DIGEST_RE.match(digest):
            raise ScopeError(
                RefusalReason.MANIFEST_UNVERIFIED,
                f"entry {path!r} has a digest that is not 64 lowercase hex characters: "
                f"{digest!r}",
            )
        lines.append(f"{path}:{digest}")
    return "\n".join(lines)


def revision_hash(entries) -> str:
    """Digest of the canonical payload.

    An empty manifest hashes to the sha256 of the empty string, which is NOT the
    no-deliverable sentinel. Callers must reject an empty manifest on a reviewable receipt
    rather than letting it produce a legitimate-looking digest.
    """
    return hashlib.sha256(canonical_payload(entries).encode("utf-8")).hexdigest()


def _hash_descriptor(fd: int) -> tuple[str, int]:
    digest = hashlib.sha256()
    offset = 0
    while True:
        block = os.pread(fd, CHUNK, offset)
        if not block:
            break
        digest.update(block)
        offset += len(block)
    return digest.hexdigest(), offset


def hash_authorized(handle: AuthorizedFile, *, between_passes=None) -> tuple[str, int]:
    """Hash twice from the same descriptor and require the passes to agree.

    Two passes cost one extra read of a file that is being hashed anyway, and they catch a
    writer whose changes do not move the timestamps the stat comparison watches. This is
    detection, not a guarantee: a writer that produces the identical byte stream on both
    passes is not caught. On a lease-enforced binding the question does not arise.
    """
    first, size = _hash_descriptor(handle.fd)
    if between_passes is not None:
        between_passes()
    second, size_again = _hash_descriptor(handle.fd)
    if (first, size) != (second, size_again):
        raise ScopeError(
            RefusalReason.ARTIFACT_MUTATED_DURING_READ,
            f"{handle.declared!r} produced different bytes on two consecutive reads",
        )
    handle.verify_stable()
    return first, size


def hash_path(declared: str, roots, *, allow_lease: bool = False, between_passes=None):
    with open_authorized(declared, roots, allow_lease=allow_lease) as handle:
        digest, size = hash_authorized(handle, between_passes=between_passes)
        return digest, size, handle.binding


def build(paths, roots, *, allow_lease: bool = False) -> tuple[list, dict]:
    """Build manifest entries by reading each authorized artifact."""
    entries, bindings = [], {}
    for path in paths:
        declared = normalize_declared_path(path)
        digest, size, binding = hash_path(declared, roots, allow_lease=allow_lease)
        entries.append(Entry(path=declared, sha256=digest, bytes=size))
        bindings[declared] = binding
    return entries, bindings


def weakest_binding(bindings) -> PathBindingMode:
    mode = PathBindingMode.LEASE_ENFORCED
    for binding in bindings.values():
        if not at_least(binding.mode, mode):
            mode = binding.mode
    return mode


# The errnos scope._walk_error gives a named meaning to. Everything else falls through to a
# generic SCOPE_ESCAPE, which reads like a policy decision and is not one: it is an open that did
# not work and nobody interpreted. That fallthrough is how a permission error came to be reported
# as a deliverable that changed.
_INTERPRETED_ERRNOS = frozenset({errno.ELOOP, errno.ENOTDIR, errno.ENOENT, errno.ESTALE})


def _is_access_failure(error: ScopeError) -> bool:
    """Was this refusal really an OSError nobody could interpret?

    Read from the exception chain rather than from the message text, because scope raises its
    refusals with the original OSError attached. A binding that could not be established counts
    too: not verifying a path is not the same as deciding against it.
    """
    if getattr(error, "reason", None) is RefusalReason.UNVERIFIABLE_PATH_BINDING:
        return True
    cause = getattr(error, "__cause__", None)
    if not isinstance(cause, OSError):
        return False
    return cause.errno not in _INTERPRETED_ERRNOS


def verify_against_disk_detailed(entries, roots, *, allow_lease: bool = False):
    """verify_against_disk, and which of the problems were failures to READ.

    Added beside verify_against_disk for the same reason verify_frozen_detailed was: the receipt
    intake path depends on the two-value answer and its behaviour must not move. problems is built
    exactly as it was in every branch, and unreadable is the subset naming the entries this process
    could not open at all.

    The distinction is invisible to an exception boundary, because the errors are caught here and
    returned as text. A caller deciding whether a deliverable CHANGED has to ask for it by name.
    """
    problems, bindings, unreadable = [], {}, []
    for entry in entries:
        try:
            digest, size, binding = hash_path(entry.path, roots, allow_lease=allow_lease)
        except ScopeError as error:
            message = f"{entry.path}: {error.reason.value}: {error.detail}"
            problems.append(message)
            if _is_access_failure(error):
                unreadable.append(message)
            continue
        except FileNotFoundError:
            problems.append(f"{entry.path}: missing")
            continue
        bindings[entry.path] = binding
        if digest != entry.sha256:
            problems.append(
                f"{entry.path}: bytes hash to {digest} but the manifest claims {entry.sha256}"
            )
        elif entry.bytes is not None and entry.bytes != size:
            problems.append(f"{entry.path}: size {size} but the manifest claims {entry.bytes}")
    return problems, bindings, unreadable


def verify_against_disk(entries, roots, *, allow_lease: bool = False):
    """Re-hash every declared path and report what disagrees.

    A truncated or partially written artifact hashes differently, a vanished one reports
    missing, and a path that escapes the authorized roots reports its refusal reason. The
    caller never learns "verified" from anything but a full match.

    The two-value form the intake path has always called: the detailed one with the access
    breakdown dropped, so every branch answers exactly what it answered before.
    """
    problems, bindings, _unreadable = verify_against_disk_detailed(
        entries, roots, allow_lease=allow_lease
    )
    return problems, bindings


def freeze(entries, destination) -> str:
    """Copy the bytes somewhere durable while keeping the original declared paths.

    The digest is path-dependent, so the frozen copy records the paths the receipt was
    hashed over and stores the bytes by content. Verifying later reproduces the same digest
    from the same declared paths even though the live files have moved on.
    """
    destination = Path(destination)
    (destination / "files").mkdir(mode=0o700, parents=True, exist_ok=True)
    for entry in entries:
        # The digest names a file on disk, so it is validated BEFORE it is ever joined to a
        # path. An absolute or traversing value would otherwise escape the snapshot root.
        if not DIGEST_RE.match(entry.sha256 or ""):
            raise ScopeError(
                RefusalReason.MANIFEST_UNVERIFIED,
                f"refusing to store bytes under a non-digest name {entry.sha256!r}",
            )
        blob = destination / "files" / entry.sha256
        if not blob.exists():
            with open_authorized(entry.path, ["/"]) as handle:
                with open(blob, "wb") as out:
                    offset = 0
                    while True:
                        block = os.pread(handle.fd, CHUNK, offset)
                        if not block:
                            break
                        out.write(block)
                        offset += len(block)
                handle.verify_stable()
            os.chmod(blob, 0o600)
        # Never publish a manifest over bytes that were not re-read and confirmed.
        copied, size = _read_frozen_blob(destination, entry.sha256)
        if copied != entry.sha256:
            raise ScopeError(
                RefusalReason.MANIFEST_UNVERIFIED,
                f"frozen copy of {entry.path!r} hashes to {copied}, not {entry.sha256}",
            )
        if entry.bytes is not None and entry.bytes != size:
            raise ScopeError(
                RefusalReason.MANIFEST_UNVERIFIED,
                f"frozen copy of {entry.path!r} is {size} bytes, not {entry.bytes}",
            )
    payload = {
        "serialization": SERIALIZATION,
        "revisionHash": revision_hash(entries),
        "entries": [entry.to_record() for entry in entries],
    }
    (destination / "MANIFEST.json").write_text(json.dumps(payload, indent=2, sort_keys=True))
    os.chmod(destination / "MANIFEST.json", 0o600)
    return str(destination)


def _read_frozen_blob(reference: Path, digest: str) -> tuple[str, int]:
    """Read one snapshot blob through the pinned traversal a live artifact would use."""
    blob = (reference / "files" / digest).resolve()
    root = (reference / "files").resolve()
    with open_authorized(str(blob), [str(root)]) as handle:
        return hash_authorized(handle)


def _frozen_document_access(document, manifest_ref) -> list:
    """Why is_file() said no: absent, or out of reach? Returns the access failures, if any.

    is_file() answers False for both, and they are different facts. A frozen copy that does not
    exist is a receipt that never froze one; a frozen copy behind a permission or a vanished mount
    is one nobody was able to check. Only the second means the verification did not happen.
    """
    try:
        document.stat()
    except FileNotFoundError:
        return []
    except OSError as error:
        if error.errno in _INTERPRETED_ERRNOS:
            # The same rule the blob handler applies: an errno scope gives a name to is an answer
            # about the frozen copy, not a failure to reach it. A manifestRef traversing a regular
            # file is a malformed reference, and reporting it unreadable would release a receipt
            # whose snapshot is provably wrong.
            return []
        return [f"{manifest_ref}: the frozen manifest could not be reached: {error}"]
    # It is there and it is not a regular file. That is a malformed frozen copy, not a failure to
    # reach one, so it stays a content problem.
    return []


def verify_frozen_detailed(manifest_ref: str, entries=None) -> tuple[str, list, list]:
    """verify_frozen, and which of the problems were failures to READ rather than disagreements.

    Added alongside verify_frozen rather than folded into it. The receipt intake path depends on
    the two-value answer and its behaviour must not move, so problems is computed exactly as it was
    in every branch and unreadable is a subset of it naming the access failures. A caller that
    wants the distinction asks for it; one that does not gets what it always got.

    The distinction matters to a caller deciding whether a deliverable CHANGED or whether it could
    not be compared at all. Reporting an unreachable frozen copy as a disagreement claims a
    comparison against bytes nobody opened.
    """
    reference = Path(manifest_ref)
    document = reference / "MANIFEST.json"
    if not document.is_file():
        return (
            "",
            [f"{manifest_ref}: no MANIFEST.json in the frozen copy"],
            _frozen_document_access(document, manifest_ref),
        )
    # A document that is not valid UTF-8 or not valid JSON raises, as it always has. Those bytes
    # were readable and are not a manifest, which is a corrupt frozen copy rather than an access
    # failure, and the intake path has always seen it as an exception.
    payload = json.loads(document.read_text())
    frozen = [Entry.from_record(record) for record in payload["entries"]]
    problems, unreadable = [], []
    if entries is not None:
        claimed = {(e.path, e.sha256) for e in entries}
        stored = {(e.path, e.sha256) for e in frozen}
        if claimed != stored:
            problems.append(
                f"{manifest_ref}: the frozen manifest does not describe the same deliverables"
            )
        sizes = {e.path: e.bytes for e in frozen}
        for entry in entries:
            if entry.bytes is not None and entry.path in sizes:
                if sizes[entry.path] is not None and sizes[entry.path] != entry.bytes:
                    problems.append(
                        f"{entry.path}: caller claims {entry.bytes} bytes but the frozen copy "
                        f"records {sizes[entry.path]}"
                    )
    for entry in frozen:
        if not DIGEST_RE.match(entry.sha256 or ""):
            problems.append(f"{entry.path}: {entry.sha256!r} is not a digest")
            continue
        try:
            digest, size = _read_frozen_blob(reference, entry.sha256)
        except (ScopeError, OSError) as error:
            message = f"{entry.path}: frozen bytes unreadable for {entry.sha256}: {error}"
            problems.append(message)
            # The same discrimination the live path makes, applied here too. A blob that was
            # DELETED raises PATH_CHANGED, which is a definitive answer about a broken snapshot
            # rather than a failure to look, and calling it unreadable would release a turn whose
            # frozen copy is provably incomplete. Only an error nobody could interpret counts.
            if not isinstance(error, ScopeError) or _is_access_failure(error):
                unreadable.append(message)
            continue
        if digest != entry.sha256:
            problems.append(f"{entry.path}: frozen bytes do not match {entry.sha256}")
        elif entry.bytes is not None and entry.bytes != size:
            problems.append(f"{entry.path}: frozen bytes are {size}, not the claimed {entry.bytes}")
    return revision_hash(frozen), problems, unreadable


def verify_frozen(manifest_ref: str, entries=None) -> tuple[str, list]:
    """Verify a receipt against its frozen copy instead of files that may have moved.

    The two-value form the intake path has always called. It is the detailed one with the access
    breakdown dropped, so every branch answers exactly what it answered before.
    """
    digest, problems, _unreadable = verify_frozen_detailed(manifest_ref, entries)
    return digest, problems
