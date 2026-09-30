"""Connection-lifetime flock admission for the retained Python fence runtime."""

import fcntl
import hashlib
import json
import os
import shutil
import sqlite3
import stat
import tempfile
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from .errors import RefusalReason, RelayError

BUILD = "codex-session-relay/0.2.0"
KEYS = ("writer_protocol", "owner", "owner_epoch", "takeover_id",
        "rollback_allowed", "python_compatibility_build")
# The one bound on a fence lock wait that another writer can hold for an unbounded time: the
# socket-binding write-gate EX and the inbox .replay.lock (cutover.md Lock order). The store's
# SQLite busy timeout (store.py _open) and Go's default OpenOptions.BusyTimeout are the same.
LOCK_WAIT_SECONDS = 30.0
# How long a start preflight waits for a first opener still creating the store (refuse_partial;
# Go store.CreationWait, the same bound): the bound Go's admitted open waits for a creation
# within (awaitCreation, its default busy timeout) and LOCK_WAIT_SECONDS. Tests lower it.
CREATION_WAIT_SECONDS = LOCK_WAIT_SECONDS


class LockWaitExpired(Exception):
    """A bounded fence lock wait ran out: a retryable host error that changed nothing."""


def flock_within(fd, operation, what, seconds=None):
    """flock `operation` on `fd`, giving up after LOCK_WAIT_SECONDS."""
    bound = LOCK_WAIT_SECONDS if seconds is None else seconds
    deadline = time.monotonic() + bound
    while True:
        try:
            fcntl.flock(fd, operation | fcntl.LOCK_NB)
            return
        except BlockingIOError:
            if time.monotonic() >= deadline:
                raise LockWaitExpired(f"{what} was not acquired within {bound:g}s; retry") from None
            time.sleep(0.01)


@dataclass(frozen=True, slots=True)
class CandidatePermit:
    """Authorization received only by the controller's directly launched candidate."""

    transition_id: str
    epoch: int
    boot_id: str
    pid: int
    start_ticks: int

    def matches(self, meta, record) -> bool:
        return (self.transition_id == meta.get("takeover_id")
                and self.epoch == record.get("epoch")
                and (record.get("transition") or {}).get("id") == self.transition_id
                and record.get("controller") == {
                    "bootId": self.boot_id, "pid": self.pid, "startTicks": self.start_ticks})


class OwnershipRefused(RelayError):
    reason: RefusalReason | None = RefusalReason.STORE_OWNED_BY_OTHER

    def __init__(self, detail, *, queueable=False):
        super().__init__(detail=detail)
        self.queueable = queueable


def metadata(path):
    """Read a disposable SQLite snapshot, never making sidecars beside the source.

    The admission gate excludes ownership changes while this is called by a writer.
    Ordinary admitted writes may race the copy; a damaged/incomplete snapshot refuses,
    never repairs the source. WAL is copied too: immutable=1 alone ignores live WAL.
    """
    path = Path(path).resolve()
    if not path.exists():
        return {}
    with tempfile.TemporaryDirectory(prefix="crw-ownership-") as scratch:
        target = Path(scratch) / path.name
        shutil.copyfile(path, target)
        try:
            shutil.copyfile(str(path) + "-wal", str(target) + "-wal")
        except FileNotFoundError:
            pass
        db = sqlite3.connect(target.as_uri() + "?mode=ro", uri=True, timeout=0)
        try:
            if not db.execute("SELECT 1 FROM sqlite_master WHERE name='schema_meta'").fetchone():
                return {}
            return dict(db.execute("SELECT key,value FROM schema_meta"))
        finally:
            db.close()


# A write-ahead log no longer than its header holds no frame.
WAL_HEADER_SIZE = 32
WAL_WITHOUT_INDEX = ("the store's write-ahead log holds frames and its shared-memory index is"
                     " missing or unusable (an unclean shutdown), so its committed state cannot be"
                     " read without creating a SQLite sidecar")


def stop_metadata(path):
    """schema_meta as the read-only Stop path reads it: no copy, no sidecar, no lock file.

    metadata() copies the store and its WAL into a temporary directory, which on a Stop is a
    write outside the hook's own root and a copy of the whole live store per turn end. A Stop
    evaluation writes nothing to the store (cutover.md Lock order: the read-only Stop path takes
    none of the fence locks), so it reads the durable half in place:

    - D-wal present beside a regular D-shm: a WAL connection (live or crashed) left SQLite's own
      coordination files, and a mode=ro open sees committed WAL frames while creating nothing.
    - no D-wal, or one holding no frame (empty, or only its 32-byte header): every committed
      transaction is in D (SQLite unlinks -shm before -wal at a checkpointed close, and a
      starting writer creates -wal before it can commit), so D is read immutable=1, which never
      creates -wal or -shm. mode=ro here would create both.
    - a D-wal holding frames beside no usable D-shm (an unclean shutdown), or one that cannot be
      examined, has no such read: immutable=1 would miss commits only its frames hold (an owner
      change among them), and mode=ro would create the index. sqlite3.OperationalError is
      raised, so no Stop is judged from D's stale owner.

    Go's native Stop owner read (hook ownsGuard, store.OpenStopRead, store.InPlaceRead) applies
    the same rule.
    """
    path = Path(path).resolve()
    if not path.exists():
        return {}
    from .intent import SQLITE_TIMEOUT

    try:
        wal = os.stat(str(path) + "-wal")
    except FileNotFoundError:
        wal = None
    except OSError as error:
        raise sqlite3.OperationalError(
            f"the store's write-ahead log could not be examined: {error}") from error
    try:
        shm = os.stat(str(path) + "-shm")
    except OSError:
        shm = None
    live = wal is not None and shm is not None and stat.S_ISREG(shm.st_mode)
    if not live and wal is not None and (not stat.S_ISREG(wal.st_mode)
                                         or wal.st_size > WAL_HEADER_SIZE):
        raise sqlite3.OperationalError(WAL_WITHOUT_INDEX)
    db = sqlite3.connect(path.as_uri() + ("?mode=ro" if live else "?mode=ro&immutable=1"),
                         uri=True, timeout=SQLITE_TIMEOUT)
    try:
        if not db.execute("SELECT 1 FROM sqlite_master WHERE name='schema_meta'").fetchone():
            return {}
        return dict(db.execute("SELECT key,value FROM schema_meta"))
    finally:
        db.close()


def mirror(path):
    try:
        value = json.loads((Path(path).resolve().parent / "takeover.json").read_bytes())
    except FileNotFoundError:
        return None
    except (OSError, ValueError) as error:
        raise OwnershipRefused(f"takeover record unreadable: {type(error).__name__}: {error}") from error
    if not isinstance(value, dict):
        raise OwnershipRefused("takeover record is not an object")
    return value


def physical(path):
    path = Path(path).resolve()
    info, directory = path.stat(), path.parent.stat()
    return {"realPath": str(path), "device": info.st_dev, "inode": info.st_ino,
            "walDirectoryDevice": directory.st_dev, "walDirectoryInode": directory.st_ino,
            "walBasename": path.name}


def validate(path, meta, record, *, admitted_epoch=None, candidate=None):
    if meta.get("writer_protocol") != "1" or not record or type(record.get("protocol")) is not int or record["protocol"] != 1:
        raise OwnershipRefused("missing or unsupported writer protocol")
    required = {"protocol", "storeId", "database", "appServerSocket", "scopeKey", "epoch",
                "owner", "phase", "transition", "holder", "controller", "rollbackAllowed",
                "pythonCompatibilityBuild", "relayRPCSocket", "updatedAt"}
    if not required <= record.keys() or not all(key in meta for key in KEYS):
        raise OwnershipRefused("incomplete ownership record")
    if type(record["epoch"]) is not int or type(record["rollbackAllowed"]) is not bool:
        raise OwnershipRefused("mistyped ownership record")
    # The scope identity both halves record (Go record.go Validate): the mirror's socket is the
    # store's schema_meta socket_path, or null exactly when the store records none, and its
    # scopeKey is the key this process's own scope-registry authority (its
    # CODEX_SESSION_RELAY_SCOPE_DIR, or the production root) gives that socket. A process under
    # another authority would lock another scope for the same App Server, so it is refused, and
    # so is one whose override names a home that cannot be found (pathlib's RuntimeError): it
    # has no authority to judge the key by. Both are refusals, so a read-only form still reads.
    socket_path = record["appServerSocket"]
    if socket_path is None:
        if record["scopeKey"] is not None or meta.get("socket_path"):
            raise OwnershipRefused("scope without socket")
    elif (not isinstance(socket_path, str) or not os.path.isabs(socket_path)
            or os.path.normpath(socket_path) != socket_path
            or not isinstance(record["scopeKey"], str) or not record["scopeKey"]
            or socket_path != meta.get("socket_path")):
        raise OwnershipRefused("invalid socket/scope identity")
    elif record["scopeKey"] != _authority_key(socket_path):
        raise OwnershipRefused("scope key disagrees with lock authority")
    if record["phase"] not in ("active", "draining", "starting"):
        raise OwnershipRefused("invalid takeover phase")
    if record["relayRPCSocket"] != str(Path(path).resolve().parent / "control.sock"):
        raise OwnershipRefused("the control socket belongs to another state directory")
    transition = record["transition"]
    if transition is not None and (not isinstance(transition, dict)
                                   or transition.get("id") != meta["takeover_id"]
                                   and record["phase"] != "draining"):
        raise OwnershipRefused("ownership transition disagrees")
    epoch = meta.get("owner_epoch")
    if not epoch or not epoch.isdecimal() or int(epoch) < 1:
        raise OwnershipRefused("invalid ownership epoch")
    if meta.get("owner") not in ("python", "go") or meta.get("rollback_allowed") not in ("0", "1"):
        raise OwnershipRefused("invalid durable ownership record")
    expected = {"owner": meta["owner"], "epoch": int(epoch), "storeId": meta.get("store_id"),
                "database": physical(path), "pythonCompatibilityBuild": meta.get("python_compatibility_build"),
                "rollbackAllowed": meta.get("rollback_allowed") == "1"}
    if any(record.get(key) != value for key, value in expected.items()):
        raise OwnershipRefused("ownership record disagrees with the durable store")
    if meta.get("python_compatibility_build") != BUILD:
        raise OwnershipRefused("unsupported Python compatibility build")
    if meta["owner"] != "python":
        raise OwnershipRefused("the relay store belongs to another runtime", queueable=True)
    if admitted_epoch is not None and epoch != admitted_epoch:
        raise OwnershipRefused("the admitted ownership epoch changed")
    if candidate is not None and (candidate.epoch != int(epoch)
                                  or candidate.transition_id != meta["takeover_id"]):
        raise OwnershipRefused("the candidate ownership permit changed")
    phase = record.get("phase")
    if phase == "starting":
        if candidate is None or not candidate.matches(meta, record):
            raise OwnershipRefused("only the designated candidate may enter starting", queueable=True)
        return epoch
    if phase == "draining" and admitted_epoch is None:
        raise OwnershipRefused("the relay store is draining", queueable=True)
    if phase not in (("active", "draining") if admitted_epoch else ("active",)):
        raise OwnershipRefused("the relay store is not active")
    return epoch


def unbound(meta, record, socket, candidate=None):
    """The schema_meta a socket binding starts from, or None when this open binds nothing.

    A fenced store this runtime owns whose mirror names no App Server socket (it was created
    by a socketless writable opener) is bound to the first writable opener that passes one
    (decision 30, cutover.md Record): under write-gate EX, `socket_path` is committed, then the
    mirror's appServerSocket/scopeKey are published. A crash between the two leaves
    `socket_path` equal to that socket under a null mirror, which the next opener passing the
    same socket completes. `socket` is canonical; the candidate never binds.
    """
    if (socket is None or candidate is not None or not isinstance(record, dict)
            or "appServerSocket" not in record or record["appServerSocket"] is not None
            or record.get("scopeKey") is not None
            or meta.get("socket_path") not in (None, socket)
            or meta.get("owner") != "python" or record.get("owner") != "python"
            or record.get("phase") != "active" or record.get("transition") is not None):
        return None
    return {key: value for key, value in meta.items() if key != "socket_path"}


def check_start(path, *, candidate=None, socket=None):
    """Read-only preflight; the actual writable opener checks again under its gate.

    `socket` is the App Server socket this writable open passes, so a binding the opener
    would complete (see unbound) is not refused here first.
    """
    record = mirror(path)
    meta = metadata(path)
    if not any(key in meta for key in KEYS) and record is None:
        if candidate is not None:
            raise OwnershipRefused("candidate requires an existing fenced store")
        return
    if socket:
        from .store import canonical_socket

        meta = unbound(meta, record, canonical_socket(socket), candidate) or meta
    validate(path, meta, record, candidate=candidate)


def _absent(name):
    """Whether nothing at all is at `name`: only a name lstat certainly finds missing.

    A link naming no file is there, so a store holding one is never an absent store.
    """
    try:
        os.lstat(name)
    except FileNotFoundError:
        return True
    except OSError:
        return False
    return False


def _creating(gate):
    """Whether another opener holds `gate` EX: a first opener still creating the store.

    A creator holds the gate EX from before any other opener can find it (_create_gate, Go's
    placeGate) until the store it creates is whole. Probed once without waiting, a shared lock
    released at once (Go's gateHeld); a gate that cannot be opened or locked is nobody's
    creation, and so is one that is not a regular file, which no creator places. The open does
    not wait either (O_NONBLOCK): a FIFO opened read-only would wait for a writer.
    """
    try:
        fd = os.open(gate, os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_NOFOLLOW", 0))
    except OSError:
        return False
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            return False
        fcntl.flock(fd, fcntl.LOCK_SH | fcntl.LOCK_NB)
    except BlockingIOError:
        return True
    except OSError:
        return False
    finally:
        os.close(fd)
    return False


def _await_creator(gate, deadline):
    """Wait until no opener holds `gate` EX, polling every 10 ms without blocking (Go's
    awaitCreator); past `deadline` the command is refused as a creation still in progress."""
    while _creating(gate):
        if time.monotonic() >= deadline:
            raise OwnershipRefused("store creation in progress: write-gate.lock still held after "
                                   f"{CREATION_WAIT_SECONDS:g}s; retry")
        time.sleep(0.01)


def refuse_partial(path):
    """Refuse a partial store before a service command or the daemon touches S.

    A partial store has no D (certainly absent) but a gate or a mirror name. A writer's
    Admission meets it and refuses it, but the service and daemon commands act on S and the
    scope registry (daemon.lock, daemon.json, service.json, the scope claim) before any admitted
    open, and check_start passes a store whose mirror reads as no record as unfenced; cli.main
    runs this first for them, in Go's store.StartPreflight order. D, the mirror and the gate
    are named as every opener names them, beside Path.resolve()'s D (a dangling D link names an
    absent D). The mirror is read as check_start reads it: an unreadable one refuses here in
    check_start's words, and a record is left to check_start, whose validate refuses it next. A
    mirror link naming no file reads as no record but is there, so beside no D it is partial.

    A gate that another opener holds EX beside no D and no mirror is a first opener creating the
    store. It is neither partial nor a store to act on yet: `service enable`, `disable` and
    `declare` change S with no admitted open after this, so letting it through wrote their
    intent for a store the creator could stamp for the other runtime. This waits, polling
    without blocking, until no opener holds the gate EX (_await_creator), for at most
    CREATION_WAIT_SECONDS, then judges the store again from the start: the store the creator
    left is check_start's to judge, and it refuses the other runtime's; a gate let go with no D
    (a creator that gave up) is partial; a creator that still holds it at the bound is refused
    as a creation in progress. Go's StartPreflight waits and judges alike, with the same bound
    and words. The probe is the only lock this takes, and only for a moment each time. A
    creation can complete between the look and the probe, so a probe that finds the gate unheld
    is followed by a second look, in Go's order (creating, then partialStore's own lstat of D):
    a D that now exists, or a mirror that now holds a record, is left to check_start.
    """
    path = Path(path).resolve()
    gate = path.parent / "write-gate.lock"
    deadline = time.monotonic() + CREATION_WAIT_SECONDS
    while True:
        if not _absent(path) or (_absent(path.parent / "takeover.json") and _absent(gate)):
            return
        if mirror(path) is not None:
            return
        if _absent(gate) or not _creating(gate):
            break
        _await_creator(gate, deadline)
    if not _absent(path) or mirror(path) is not None:
        return
    raise OwnershipRefused("partial store: write-gate.lock without a database")


def check_stop(path):
    """check_start for a read-only Stop evaluation (guard-evaluate's local path).

    The same verdict as a candidate-less, socketless check_start: the mirror takeover.json and
    the durable stamp must agree for this runtime's active store, and an unfenced or absent
    store evaluates. Only the read differs: stop_metadata, never a copy. Writers keep
    check_start and Admission.
    """
    record = mirror(path)
    meta = stop_metadata(path)
    if not any(key in meta for key in KEYS) and record is None:
        return
    validate(path, meta, record)


def report(path):
    try:
        meta, record = metadata(path), mirror(path)
        # Stamped but unpublished (a torn step-0 or absent-store initialization) is not healthy.
        detail = ("takeover record missing"
                  if record is None and any(key in meta for key in KEYS) else None)
        return {**{key: meta.get(key) for key in KEYS},
                "phase": record.get("phase") if record else None,
                "runtime_build": BUILD, "detail": detail}
    except (OSError, sqlite3.Error, OwnershipRefused) as error:
        return {**{key: None for key in KEYS}, "phase": None,
                "runtime_build": BUILD, "detail": str(error)}


def scope_key(socket_path):
    """The scope-registry key of a canonical socket as the ownership record carries it
    (service.canonical_scope_key, Go ownership.ScopeKey), including the isolated-<salt>-
    namespace an overridden registry root adds.

    Every caller passes a canonical spelling (canonical_socket's at binding and initialization,
    the recorded appServerSocket in validate), hashed as given: never resolved again, so a socket
    directory that becomes a symlink after binding leaves the record valid in both runtimes. The
    production authority's key has no salt, so its root (the passwd entry) is not looked up.
    """
    from .service import SCOPE_ENV, canonical_scope_key, resolve_scope_root

    if not os.environ.get(SCOPE_ENV):
        return canonical_scope_key(socket_path)
    root, _authority = resolve_scope_root()
    return canonical_scope_key(socket_path, root)


def _authority_key(socket_path):
    """scope_key for validate: an authority that gives no key refuses rather than raises.

    Go's record.go Validate refuses the same state in the same words (NoAuthorityDetail). A
    binding or an initialization, which records a new key, still raises the RuntimeError.
    """
    try:
        return scope_key(socket_path)
    except RuntimeError as error:
        raise OwnershipRefused(f"scope key cannot be judged: {error}") from error


def _legacy(meta, record):
    return not any(key in meta for key in KEYS) and record is None


def _create_gate(directory, flags):
    """Create write-gate.lock already held EX, or open the one another opener placed.

    The gate is created under a temporary name, locked EX, then link(2)ed into place, so no
    other opener can ever find it unlocked before its creator has created the store: a
    concurrent first opener that finds the gate waits for that creation instead of seeing a
    gate without a database. Returns (fd, created).
    """
    gate = directory / "write-gate.lock"
    fd, temporary = tempfile.mkstemp(prefix=".write-gate-", dir=directory)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)  # uncontended: no other process knows this name
        try:
            os.link(temporary, gate)
        except FileExistsError:
            os.close(fd)
            fd = None
            return os.open(gate, flags), False
        return fd, True
    except BaseException:
        if fd is not None:
            os.close(fd)
        raise
    finally:
        os.unlink(temporary)


def _publish(directory, record):
    """Atomically replace S/takeover.json and fsync its directory."""
    fd, temporary = tempfile.mkstemp(prefix=".takeover-", dir=directory)
    try:
        with os.fdopen(fd, "w") as handle:
            json.dump(record, handle, sort_keys=True, separators=(",", ":"))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, directory / "takeover.json")
        descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class Admission:
    def __init__(self, path, *, initialize=False, candidate=None, socket=None):
        """Hold write-gate SH for a connection's lifetime.

        `socket` is the canonical App Server socket a writable open may bind an unbound store
        to (see unbound); None never binds.
        """
        self.path = Path(path).resolve()
        self.candidate = candidate
        self.fd = None
        self.initializing = False
        self.binding = None
        self._bound_record = None
        self._bound_key = None
        self.epoch = None
        if initialize:
            self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        flags = os.O_RDWR | getattr(os, "O_NOFOLLOW", 0)
        created = False
        try:
            self.fd = os.open(self.path.parent / "write-gate.lock", flags)
        except FileNotFoundError:
            if not initialize:
                raise OwnershipRefused("the store has no write admission gate") from None
            # A foreign or partially fenced store must not even gain a lock file.
            meta, record = metadata(self.path), mirror(self.path)
            if any(key in meta for key in KEYS) or record is not None:
                raise OwnershipRefused("the fenced store has no write admission gate")
            if _absent(self.path) and not _absent(self.path.parent / "takeover.json"):
                # A mirror name that reads as no record (a link naming no file) beside no D is
                # not an absent store but a partial one, as every reader, refuse_partial and Go
                # judge it: refused in the gate's words and never initialized (decision 30).
                raise OwnershipRefused("partial store: write-gate.lock without a database")
            # Pre-fence stores are initialized only under an exclusive maintenance gate.
            self.fd, created = _create_gate(self.path.parent, flags)
        try:
            if created:
                # Held EX since before any other opener could find it.
                meta, record = metadata(self.path), mirror(self.path)
                if _legacy(meta, record):
                    self.initializing = True
                    return
                fcntl.flock(self.fd, fcntl.LOCK_SH)
            else:
                fcntl.flock(self.fd, fcntl.LOCK_SH)
                meta, record = metadata(self.path), mirror(self.path)
                legacy = initialize and _legacy(meta, record)
                if legacy or unbound(meta, record, socket, self.candidate) is not None:
                    fcntl.flock(self.fd, fcntl.LOCK_UN)
                    if legacy:
                        fcntl.flock(self.fd, fcntl.LOCK_EX)
                    else:
                        # Other admitted connections hold SH for their lifetime.
                        flock_within(self.fd, fcntl.LOCK_EX, "write-gate EX for the socket binding")
                    meta, record = metadata(self.path), mirror(self.path)
                    if initialize and _legacy(meta, record):
                        if not self.path.exists():
                            # A gate with no database that this opener found rather than
                            # created, while holding it EX: partial, refused and never
                            # repaired (decision 30, cutover.md Step 0 recovery).
                            raise OwnershipRefused("partial store: write-gate.lock without a database")
                        self.initializing = True
                        return
                    start = unbound(meta, record, socket, self.candidate)
                    if start is not None:
                        # Every other part of the record is validated before anything is written.
                        self.epoch = validate(self.path, start, record)
                        self._bound_key = scope_key(socket)
                        self._bound_record = record
                        self.binding = socket
                        return
                    fcntl.flock(self.fd, fcntl.LOCK_SH)
            self.epoch = validate(self.path, meta, record, candidate=self.candidate)
        except BaseException:
            self.close()
            raise

    def initialize(self, db, socket_path):
        """Complete what this admission started under EX, after Store._open ran."""
        if self.binding is not None:
            # _open committed socket_path (INSERT OR IGNORE; the DB half first). Only the socket
            # fields and updatedAt of the record validated under EX change.
            recorded = dict(db.execute("SELECT key,value FROM schema_meta")).get("socket_path")
            if recorded != self.binding:
                raise OwnershipRefused("requested socket disagrees with the recorded store socket")
            _publish(self.path.parent, {
                **self._bound_record, "appServerSocket": self.binding, "scopeKey": self._bound_key,
                "updatedAt": datetime.now(timezone.utc).isoformat()})
            self.binding = self._bound_record = self._bound_key = None
            assert self.fd is not None
            fcntl.flock(self.fd, fcntl.LOCK_SH)
            return
        if not self.initializing:
            return
        from .store import canonical_socket

        # The mirror records the store's own durable socket (the first recording wins), never a
        # requested spelling that disagrees with it.
        recorded = dict(db.execute("SELECT key,value FROM schema_meta")).get("socket_path")
        if socket_path and recorded != canonical_socket(socket_path):
            raise OwnershipRefused("requested socket disagrees with the recorded store socket")
        socket_path = recorded
        values = {"writer_protocol": "1", "owner": "python", "owner_epoch": "1",
                  "takeover_id": "", "rollback_allowed": "1", "python_compatibility_build": BUILD}
        db.execute("BEGIN IMMEDIATE")
        try:
            db.executemany("INSERT INTO schema_meta VALUES (?,?)", values.items())
            db.execute("COMMIT")
        except BaseException:
            db.execute("ROLLBACK")
            raise
        meta = dict(db.execute("SELECT key,value FROM schema_meta"))
        record = {"protocol": 1, "storeId": meta["store_id"], "database": physical(self.path),
                  "appServerSocket": socket_path,
                  "scopeKey": scope_key(socket_path) if socket_path else None,
                  "epoch": 1, "owner": "python", "phase": "active", "transition": None,
                  "holder": None, "controller": None, "rollbackAllowed": True,
                  "pythonCompatibilityBuild": BUILD, "relayRPCSocket": str(self.path.parent / "control.sock"),
                  "updatedAt": datetime.now(timezone.utc).isoformat()}
        _publish(self.path.parent, record)
        self.initializing = False
        self.epoch = "1"
        assert self.fd is not None
        fcntl.flock(self.fd, fcntl.LOCK_SH)

    def revalidate(self, db):
        meta = dict(db.execute("SELECT key,value FROM schema_meta WHERE key IN "
                               "('writer_protocol','owner','owner_epoch','takeover_id',"
                               "'rollback_allowed','python_compatibility_build','store_id',"
                               "'socket_path')"))
        validate(self.path, meta, mirror(self.path), admitted_epoch=self.epoch,
                 candidate=self.candidate)

    def close(self):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None
