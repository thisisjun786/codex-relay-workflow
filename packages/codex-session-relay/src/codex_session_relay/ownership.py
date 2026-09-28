"""Connection-lifetime flock admission for the retained Python fence runtime."""

import fcntl
import hashlib
import json
import os
import shutil
import sqlite3
import tempfile
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from .errors import RefusalReason, RelayError

BUILD = "codex-session-relay/0.2.0"
KEYS = ("writer_protocol", "owner", "owner_epoch", "takeover_id",
        "rollback_allowed", "python_compatibility_build")


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


def check_start(path, *, candidate=None):
    """Read-only preflight; the actual writable opener checks again under its gate."""
    record = mirror(path)
    meta = metadata(path)
    if not any(key in meta for key in KEYS) and record is None:
        if candidate is not None:
            raise OwnershipRefused("candidate requires an existing fenced store")
        return
    validate(path, meta, record, candidate=candidate)


def report(path):
    try:
        meta, record = metadata(path), mirror(path)
        return {**{key: meta.get(key) for key in KEYS},
                "phase": record.get("phase") if record else None,
                "runtime_build": BUILD, "detail": None}
    except (OSError, sqlite3.Error, OwnershipRefused) as error:
        return {**{key: None for key in KEYS}, "phase": None,
                "runtime_build": BUILD, "detail": str(error)}


class Admission:
    def __init__(self, path, *, initialize=False, candidate=None):
        self.path = Path(path).resolve()
        self.candidate = candidate
        self.fd = None
        self.initializing = False
        self.epoch = None
        if initialize:
            self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        flags = os.O_RDWR | getattr(os, "O_NOFOLLOW", 0)
        try:
            self.fd = os.open(self.path.parent / "write-gate.lock", flags)
        except FileNotFoundError:
            if not initialize:
                raise OwnershipRefused("the store has no write admission gate") from None
            # A foreign or partially fenced store must not even gain a lock file.
            meta, record = metadata(self.path), mirror(self.path)
            if any(key in meta for key in KEYS) or record is not None:
                raise OwnershipRefused("the fenced store has no write admission gate")
            # Pre-fence stores are initialized only under an exclusive maintenance gate.
            self.fd = os.open(self.path.parent / "write-gate.lock", flags | os.O_CREAT, 0o600)
        try:
            fcntl.flock(self.fd, fcntl.LOCK_SH)
            meta, record = metadata(self.path), mirror(self.path)
            legacy = not any(key in meta for key in KEYS) and record is None
            if legacy and initialize:
                fcntl.flock(self.fd, fcntl.LOCK_UN)
                fcntl.flock(self.fd, fcntl.LOCK_EX)
                meta, record = metadata(self.path), mirror(self.path)
                legacy = not any(key in meta for key in KEYS) and record is None
                if legacy:
                    self.initializing = True
                    return
                fcntl.flock(self.fd, fcntl.LOCK_SH)
            self.epoch = validate(self.path, meta, record, candidate=self.candidate)
        except BaseException:
            self.close()
            raise

    def initialize(self, db, socket_path):
        if not self.initializing:
            return
        socket_path = (str(Path(socket_path).expanduser().resolve()) if socket_path else
                       dict(db.execute("SELECT key,value FROM schema_meta")).get("socket_path"))
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
                  "scopeKey": hashlib.sha256(socket_path.encode()).hexdigest()[:16] if socket_path else None,
                  "epoch": 1, "owner": "python", "phase": "active", "transition": None,
                  "holder": None, "controller": None, "rollbackAllowed": True,
                  "pythonCompatibilityBuild": BUILD, "relayRPCSocket": str(self.path.parent / "control.sock"),
                  "updatedAt": datetime.now(timezone.utc).isoformat()}
        fd, temporary = tempfile.mkstemp(prefix=".takeover-", dir=self.path.parent)
        try:
            with os.fdopen(fd, "w") as handle:
                json.dump(record, handle, sort_keys=True, separators=(",", ":"))
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, self.path.parent / "takeover.json")
            directory = os.open(self.path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
        self.initializing = False
        self.epoch = "1"
        assert self.fd is not None
        fcntl.flock(self.fd, fcntl.LOCK_SH)

    def revalidate(self, db):
        meta = dict(db.execute("SELECT key,value FROM schema_meta WHERE key IN "
                               "('writer_protocol','owner','owner_epoch','takeover_id',"
                               "'rollback_allowed','python_compatibility_build','store_id')"))
        validate(self.path, meta, mirror(self.path), admitted_epoch=self.epoch,
                 candidate=self.candidate)

    def close(self):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None
