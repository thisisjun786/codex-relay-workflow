"""Authorizing an artifact read, and saying honestly how strongly it was authorized.

Two things are separate here and must stay separate.

Path binding is PROVEN. The traversal opens every component from "/" with O_NOFOLLOW, so
no symlink at any depth can redirect it, and the kernel is then asked where the resulting
descriptor actually is. The descriptor pins an inode, so a later rename cannot substitute
different bytes into a read already in progress.

Byte stability is a different question with two answers. On a filesystem where the caller
can take a read lease, it is ENFORCED: no other process holds the file writable, and any
attempt to open it for writing breaks the lease, which the post-read check sees. Everywhere
else it is DETECTED, not guaranteed, by several checks that each have a named evasion. The
tier that was actually achieved is recorded on the binding and travels with the receipt, and
the consumer enforces a minimum rather than assuming the strongest.

What is deliberately not claimed: exclusive ownership of the bytes. A hardlink or a bind
mount can expose the same inode under another name. The contract authorizes paths, not
inodes, so an alias is not a scope violation; it is simply outside what this proves.
"""

import errno
import fcntl
import os
import posixpath
import signal
import stat
import threading
from dataclasses import dataclass
from enum import Enum

from .errors import RefusalReason, ScopeError

PROC_FD = "/proc/self/fd"


class PathBindingMode(str, Enum):
    BEST_EFFORT_DETECTION = "best_effort_detection"
    LEASE_ENFORCED = "lease_enforced"


_STRENGTH = {
    PathBindingMode.BEST_EFFORT_DETECTION: 0,
    PathBindingMode.LEASE_ENFORCED: 1,
}


def at_least(actual: PathBindingMode, minimum: PathBindingMode) -> bool:
    return _STRENGTH[actual] >= _STRENGTH[minimum]


def normalize_declared_path(path: str) -> str:
    """MANIFEST-CANON-01 form: absolute, normalized POSIX, no '~', no trailing slash.

    Symlinks are deliberately NOT resolved. The declared string is what enters the digest,
    so resolving it here would change the identity of an identical set of bytes.
    """
    if not isinstance(path, str) or not path:
        raise ScopeError(RefusalReason.SCOPE_ESCAPE, "path must be a non-empty string")
    if "\x00" in path:
        raise ScopeError(RefusalReason.SCOPE_ESCAPE, "path must not contain NUL")
    if not path.startswith("/"):
        raise ScopeError(RefusalReason.SCOPE_ESCAPE, f"path must be absolute: {path!r}")
    if "~" in path:
        raise ScopeError(RefusalReason.SCOPE_ESCAPE, f"path must not contain '~': {path!r}")
    if path != posixpath.normpath(path):
        raise ScopeError(
            RefusalReason.SCOPE_ESCAPE,
            f"path must already be normalized; {path!r} normalizes to "
            f"{posixpath.normpath(path)!r}",
        )
    if path != "/" and path.endswith("/"):
        raise ScopeError(RefusalReason.SCOPE_ESCAPE, f"path must not end with '/': {path!r}")
    return path


def is_within(root: str, path: str) -> bool:
    """Containment by path component, so '/a/b' does not contain '/a/bc'."""
    root = posixpath.normpath(root)
    path = posixpath.normpath(path)
    if root == "/":
        return path.startswith("/")
    return path == root or path.startswith(root + "/")


def containing_root(path: str, roots) -> str | None:
    for root in roots:
        if is_within(root, path):
            return root
    return None


def assert_within(path: str, roots) -> str:
    root = containing_root(path, roots)
    if root is None:
        raise ScopeError(
            RefusalReason.SCOPE_ESCAPE,
            f"{path!r} lies outside every authorized root {list(roots)!r}",
        )
    return root


def check_recipient(task_id: str, allowed) -> None:
    if task_id not in set(allowed):
        raise ScopeError(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED,
            f"recipient {task_id!r} is not in the relationship's allowed recipients",
        )


def assert_assignment_delivery(relationship, *, kind, recipient_task_id,
                               recipient_thread_id=None, event_relationship_id=None,
                               manifest_paths=()):
    """A delivery belongs to ONE assignment, and goes to that assignment's own endpoint.

    Membership in allowed_recipients is not sufficient by itself. Two assignments on one host
    may legitimately authorize the same recipient, so a completion belonging to assignment A
    addressed to assignment B's parent passes a membership check and is still a cross
    delivery. The DIRECTION is what ties a message to its own assignment: a completion travels
    to this relationship's parent, a revision to this relationship's child, and nothing else
    is an authorized destination.
    """
    rid = relationship["relationshipId"]
    if event_relationship_id is not None and event_relationship_id != rid:
        raise ScopeError(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED,
            f"event belongs to relationship {event_relationship_id!r}, not {rid!r}",
        )
    expected = (
        relationship["child"]["taskId"] if kind == "revision_request"
        else relationship["parent"]["taskId"]
    )
    if recipient_task_id != expected:
        direction = "child" if kind == "revision_request" else "parent"
        raise ScopeError(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED,
            f"a {kind} for {rid!r} goes to its own {direction} {expected!r}, not to "
            f"{recipient_task_id!r}",
        )
    if recipient_thread_id is not None and recipient_thread_id != recipient_task_id:
        raise ScopeError(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED,
            f"the native thread {recipient_thread_id!r} is not the recipient task "
            f"{recipient_task_id!r}",
        )
    check_recipient(recipient_task_id, relationship["authorizedScope"]["allowedRecipients"])
    roots = relationship["authorizedScope"]["artifactRoots"]
    for path in manifest_paths:
        assert_within(path, roots)


@dataclass(frozen=True)
class PathBinding:
    declared: str
    root: str
    mode: PathBindingMode
    lease_detail: str

    def to_record(self) -> dict:
        return {"pathBindingMode": self.mode.value, "leaseDetail": self.lease_detail}


class _LeaseGuard:
    """Tier 1. A read lease excludes writers while it is held.

    Two practical constraints shape this. Acquisition fails with EAGAIN when a writable
    open already exists, which is exactly the pre-existing-writer check we want, and we
    degrade rather than pretend. And an unhandled SIGIO terminates the process, so a lease
    is only attempted from the main thread where a handler can be installed and restored.

    A third constraint decides the DEFAULT. While a read lease is held, a process that
    opens the file for writing BLOCKS until the lease is released or the kernel's
    lease-break timeout expires, which is 45 seconds by default. A library that can stall
    an unrelated writer for most of a minute simply by hashing a file is a bad neighbour,
    so tier 1 is opt-in. Ask for it where enforced byte stability is worth that cost, and
    pair it with minimum_path_binding=LEASE_ENFORCED so the guarantee is also required.
    """

    AVAILABLE = hasattr(fcntl, "F_SETLEASE") and hasattr(fcntl, "F_GETLEASE")

    def __init__(self, fd: int):
        self.fd = fd
        self.held = False
        self.broken = False
        self.detail = ""
        self._previous_handler = None

    def acquire(self) -> bool:
        if not self.AVAILABLE:
            self.detail = "F_SETLEASE unavailable on this platform"
            return False
        if threading.current_thread() is not threading.main_thread():
            self.detail = "not the main thread, so SIGIO cannot be handled safely"
            return False
        try:
            self._previous_handler = signal.signal(signal.SIGIO, self._on_break)
        except (ValueError, OSError) as error:
            self.detail = f"cannot install a SIGIO handler: {error}"
            return False
        try:
            fcntl.fcntl(self.fd, fcntl.F_SETLEASE, fcntl.F_RDLCK)
        except OSError as error:
            self._restore_handler()
            if error.errno == errno.EAGAIN:
                self.detail = "a writable open already exists, so no lease could be taken"
            else:
                self.detail = f"lease refused: {errno.errorcode.get(error.errno, error.errno)}"
            return False
        self.held = True
        self.detail = "read lease held for the whole read"
        return True

    def _on_break(self, _signum, _frame):
        self.broken = True

    def still_held(self) -> bool:
        if not self.held or self.broken:
            return False
        try:
            return fcntl.fcntl(self.fd, fcntl.F_GETLEASE) == fcntl.F_RDLCK
        except OSError:
            return False

    def release(self) -> None:
        if self.held:
            try:
                fcntl.fcntl(self.fd, fcntl.F_SETLEASE, fcntl.F_UNLCK)
            except OSError:
                pass
            self.held = False
        self._restore_handler()

    def _restore_handler(self) -> None:
        if self._previous_handler is not None:
            try:
                signal.signal(signal.SIGIO, self._previous_handler)
            except (ValueError, OSError):
                pass
            self._previous_handler = None


def _pinned_open(declared: str) -> int:
    """Open every component from '/' with O_NOFOLLOW so no symlink can redirect us."""
    components = [c for c in declared.split("/") if c]
    if not components:
        raise ScopeError(RefusalReason.NOT_A_REGULAR_FILE, "'/' is not an artifact")
    directory = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for component in components[:-1]:
            try:
                nxt = os.open(
                    component,
                    os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                    dir_fd=directory,
                )
            except OSError as error:
                raise _walk_error(error, component, declared) from error
            os.close(directory)
            directory = nxt
        try:
            # O_NONBLOCK so a FIFO cannot block the open before fstat can reject it as
            # a non-regular file. It is harmless for a regular file.
            return os.open(
                components[-1],
                os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                dir_fd=directory,
            )
        except OSError as error:
            raise _walk_error(error, components[-1], declared) from error
    finally:
        os.close(directory)


def _walk_error(error: OSError, component: str, declared: str) -> ScopeError:
    # O_NOFOLLOW on a symlink raises ELOOP for the final component and ENOTDIR for an
    # intermediate one, because the kernel reports "not a directory" first.
    if error.errno in (errno.ELOOP, errno.ENOTDIR):
        return ScopeError(
            RefusalReason.SYMLINK_COMPONENT,
            f"component {component!r} of {declared!r} is a symlink or not a directory",
        )
    if error.errno in (errno.ENOENT, errno.ESTALE):
        return ScopeError(
            RefusalReason.PATH_CHANGED,
            f"component {component!r} of {declared!r} disappeared during resolution",
        )
    return ScopeError(
        RefusalReason.SCOPE_ESCAPE,
        f"cannot open component {component!r} of {declared!r}: "
        f"{errno.errorcode.get(error.errno, error.errno)}",
    )


def _fd_path(fd: int) -> str:
    try:
        return os.readlink(f"{PROC_FD}/{fd}")
    except OSError as error:
        raise ScopeError(
            RefusalReason.UNVERIFIABLE_PATH_BINDING,
            f"cannot read {PROC_FD}/{fd}; path binding cannot be verified: {error}",
        ) from error


class AuthorizedFile:
    """An open artifact whose path binding has been established, plus the tier achieved.

    Use as a context manager. Call verify_stable() after reading; hashing helpers do.
    """

    def __init__(self, declared: str, roots, *, allow_lease: bool = False):
        self.declared = normalize_declared_path(declared)
        self.root = assert_within(self.declared, roots)
        self._allow_lease = allow_lease
        self.fd = -1
        self._lease = None
        self._snapshot = None
        self.binding = None

    def __enter__(self) -> "AuthorizedFile":
        if not os.path.isdir(PROC_FD):
            raise ScopeError(
                RefusalReason.UNVERIFIABLE_PATH_BINDING,
                f"{PROC_FD} is unavailable, so an artifact read cannot be bound to its path",
            )
        self.fd = _pinned_open(self.declared)
        try:
            actual = _fd_path(self.fd)
            if actual != self.declared:
                raise ScopeError(
                    RefusalReason.PATH_RELOCATED,
                    f"descriptor for {self.declared!r} actually resolves to {actual!r}",
                )
            assert_within(actual, [self.root])
            info = os.fstat(self.fd)
            if not stat.S_ISREG(info.st_mode):
                raise ScopeError(
                    RefusalReason.NOT_A_REGULAR_FILE, f"{self.declared!r} is not a regular file"
                )
            self._snapshot = self._stat_tuple(info)
            self._lease = _LeaseGuard(self.fd)
            enforced = self._lease.acquire() if self._allow_lease else False
            if not self._allow_lease:
                self._lease.detail = "lease not attempted"
            self.binding = PathBinding(
                declared=self.declared,
                root=self.root,
                mode=(
                    PathBindingMode.LEASE_ENFORCED
                    if enforced
                    else PathBindingMode.BEST_EFFORT_DETECTION
                ),
                lease_detail=self._lease.detail,
            )
        except BaseException:
            self.close()
            raise
        return self

    def __exit__(self, *_exc) -> None:
        self.close()

    @staticmethod
    def _stat_tuple(info) -> tuple:
        return (
            info.st_dev,
            info.st_ino,
            info.st_size,
            info.st_mtime_ns,
            info.st_ctime_ns,
        )

    def verify_stable(self) -> None:
        """Re-check after reading. Each check has a named evasion; see the module docstring."""
        actual = _fd_path(self.fd)
        if actual != self.declared:
            raise ScopeError(
                RefusalReason.PATH_RELOCATED,
                f"{self.declared!r} moved to {actual!r} during the read",
            )
        if self._stat_tuple(os.fstat(self.fd)) != self._snapshot:
            raise ScopeError(
                RefusalReason.ARTIFACT_MUTATED_DURING_READ,
                f"{self.declared!r} changed size or timestamps during the read",
            )
        if self.binding.mode is PathBindingMode.LEASE_ENFORCED and not self._lease.still_held():
            raise ScopeError(
                RefusalReason.ARTIFACT_LEASE_BROKEN,
                f"the read lease on {self.declared!r} was broken during the read",
            )

    def close(self) -> None:
        if self._lease is not None:
            self._lease.release()
            self._lease = None
        if self.fd >= 0:
            try:
                os.close(self.fd)
            finally:
                self.fd = -1


def open_authorized(declared: str, roots, *, allow_lease: bool = False) -> AuthorizedFile:
    return AuthorizedFile(declared, roots, allow_lease=allow_lease)
