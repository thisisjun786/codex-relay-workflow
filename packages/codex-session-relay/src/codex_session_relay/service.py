"""Who owns the daemon, and which process may be told to stop.

Two questions the state directory cannot answer on its own.

A flock on <state>/daemon.lock proves one daemon per state directory. It says nothing about a
second installation that chose a DIFFERENT state directory for the same App Server, and those
two would each hold their own lock and each believe they were alone. Single ownership
therefore lives in a scope registry keyed by the operating scope - the socket - in a location
neither installation chose.

And a pid is not a process. Verifying /proc/<pid> and then calling os.kill leaves a window in
which that process exits and its number is reused, so the signal lands somewhere else. Every
signal here goes through a pidfd opened BEFORE the identity check and kept open until the
process is gone, so the thing verified and the thing signalled are the same thing.
"""

import errno
import fcntl
import hashlib
import json
import math
import os
import pwd
import signal
import time
import uuid
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path

SCOPE_ENV = "CODEX_SESSION_RELAY_SCOPE_DIR"
# Carries the id of the launch whose execution policy is already settled. Set by a service on
# the daemon it launches and read only by that daemon, which is why it is an environment key
# rather than an option: the command surface is where a caller composes a command from, and
# nothing about this is a caller's to state.
LAUNCH_SETTLED_ENV = "CODEX_SESSION_RELAY_LAUNCH_POLICY_SETTLED"
PRODUCTION, ISOLATED = "production", "isolated"

DAEMON_LOCK = "daemon.lock"
DAEMON_RECORD = "daemon.json"
WORKER_POLICY = "worker-policy.json"
WORKER_POLICY_LIMIT = 65536
SERVICE_INTENT = "service.json"
LAUNCH_POLICY = "launch-policy.json"
DAEMON_LOG = "daemon.log"
STOP_REQUEST = "stop.request"

# A worker that reached its own clock AFTER the instant its supervisor gave it. It took no
# tick and served nothing, so the supervisor must read it as neither a clean segment nor a
# crash: the first would report healthy work that never happened, the second would back off
# from a process that did not fail. It is defined here rather than beside the CLI's other exit
# codes because the supervisor in this module is the only thing that interprets it.
EXIT_BOUND_SPENT = 5

OURS, FOREIGN, UNVERIFIABLE, NONE = "ours", "foreign", "unverifiable", "none"


def _now() -> str:
    from .store import _now_iso

    return _now_iso()


def boot_id():
    """Without it a pid recorded before a reboot can match a live unrelated pid today."""
    try:
        return Path("/proc/sys/kernel/random/boot_id").read_text(encoding="utf-8").strip()
    except OSError:
        return None


def _stat_fields(pid: int):
    """The fields of /proc/<pid>/stat after the comm.

    The comm can itself contain spaces and brackets, so the split starts after the LAST
    closing bracket rather than at the second whitespace-separated field.
    """
    try:
        raw = Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
        return raw[raw.rindex(")") + 2:].split()
    except (OSError, ValueError):
        return None


def start_ticks(pid: int):
    """Field 22. Pids repeat; a pid together with its start time does not."""
    fields = _stat_fields(pid)
    try:
        return int(fields[19])
    except (TypeError, ValueError, IndexError):
        return None


def process_state(pid: int):
    """Field 3: R, S, D, Z, T and friends."""
    fields = _stat_fields(pid)
    return fields[0] if fields else None


def production_scope_root() -> Path:
    """The one ownership authority a real launch may use.

    Read from the passwd database rather than $HOME, so it does not move with the launch
    environment - which is the whole point, since the environment is what distinguishes two
    installations. Deliberately not under ~/.codex, which this package never writes.
    """
    return Path(pwd.getpwuid(os.geteuid()).pw_dir) / ".codex-session-relay" / "scopes"


def resolve_scope_root():
    """(root, authority). An override never silently becomes the authority.

    An environment override alone would put the hole back one level down: two real launches
    could set it to different directories, or one could set it and the other not, and both
    would claim the same socket while passing every ownership check. So an override marks the
    registry ISOLATED, and the commands that start a daemon refuse an isolated authority
    unless the caller passes --allow-isolated-scope and means it.
    """
    override = os.environ.get(SCOPE_ENV)
    if override:
        return Path(override).expanduser().absolute(), ISOLATED
    return production_scope_root(), PRODUCTION


class ScopeUnavailable(Exception):
    """The ownership record cannot be reached, so ownership cannot be established."""


@dataclass
class ScopeRegistry:
    """One claim per operating scope, in a place neither state directory chose."""

    root: Path
    authority: str = PRODUCTION
    _handle: object = field(default=None, repr=False)

    def key(self, socket_path) -> str:
        # Resolved, not merely absolute. A socket reachable through a symlink or a relative
        # alias is the SAME operating scope, and hashing the supplied spelling would give the
        # two launches different keys - so both would take a lock and both would serve one
        # App Server, which is the exact thing this registry exists to prevent.
        canonical = str(Path(socket_path).expanduser().absolute().resolve())
        digest = hashlib.sha256(canonical.encode()).hexdigest()[:16]
        if self.authority == PRODUCTION:
            return digest
        # Namespaced so an isolated record can never be read as the production one. This is
        # NOT socket isolation: two deliberately isolated services still share the socket.
        salt = hashlib.sha256(str(self.root).encode()).hexdigest()[:8]
        return f"isolated-{salt}-{digest}"

    def prepare(self) -> Path:
        """Create and validate the directory, or refuse. There is no second location."""
        try:
            self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
            info = os.stat(self.root)
        except OSError as error:
            raise ScopeUnavailable(f"{self.root}: {type(error).__name__}: {error}") from error
        if info.st_uid != os.geteuid():
            raise ScopeUnavailable(f"{self.root} is owned by uid {info.st_uid}, not this user")
        if info.st_mode & 0o022:
            raise ScopeUnavailable(f"{self.root} is group or world writable")
        return self.root

    def record_path(self, socket_path) -> Path:
        return self.root / f"{self.key(socket_path)}.json"

    def read(self, socket_path):
        try:
            return json.loads(self.record_path(socket_path).read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return None

    def claim(self, socket_path, record: dict, *, shared: bool = False):
        """Take the scope lock and write the record, or report who already holds it.

        Liveness is the LOCK, never a recorded pid. A supervisor that dies leaving a worker
        alive still owns the scope through the descriptor the worker inherited, and a dead
        pid in the record is not permission to take it.
        """
        self.prepare()
        # A stopped registration naming a DIFFERENT store is still that store's registration.
        # Taking the free lock and overwriting the record would erase the only evidence that
        # two stores have served this socket, which is the duplicate this registry exists to
        # surface. Replacing it has to be deliberate.
        existing = self.read(socket_path)
        if (existing and record.get("storeId") and existing.get("storeId")
                and existing["storeId"] != record["storeId"]
                and not record.get("takeover")):
            return {"ok": False, "reason": "scope_registered_to_other_store",
                    "held_by": existing, "scopeKey": self.key(socket_path)}
        path = self.root / f"{self.key(socket_path)}.lock"
        handle = open(path, "a+")
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as error:
            handle.close()
            if error.errno not in (errno.EACCES, errno.EAGAIN):
                raise ScopeUnavailable(f"{path}: {type(error).__name__}: {error}") from error
            return {"ok": False, "reason": "scope_owned_by_other_store",
                    "held_by": self.read(socket_path), "scopeKey": self.key(socket_path)}
        self._handle = handle
        if shared:
            os.set_inheritable(handle.fileno(), True)
        payload = dict(record, scopeKey=self.key(socket_path), scopeAuthority=self.authority,
                       socketPath=str(socket_path), registeredAt=_now())
        try:
            self.record_path(socket_path).write_text(
                json.dumps(payload, indent=2), encoding="utf-8",
            )
        except OSError:
            # The lock is taken but the registration it stands for was never published. Left
            # open, this handle goes on refusing every later start in this process on behalf
            # of a registration that does not exist - and nothing releases it before the
            # object is collected.
            self._handle = None
            handle.close()
            raise
        return {"ok": True, "reason": None, "record": payload, "lockFd": handle.fileno()}

    def release(self, socket_path, *, shared: bool = False) -> None:
        """Keep the record, drop the claim.

        The record outlives the process on purpose: a STOPPED duplicate registration on a
        different store is still a duplicate, and live-only scanning could not see it.
        """
        existing = self.read(socket_path)
        if existing is not None:
            existing.update(pid=None, workerPid=None, startedAt=None, releasedAt=_now())
            try:
                self.record_path(socket_path).write_text(
                    json.dumps(existing, indent=2), encoding="utf-8")
            except OSError:
                pass
        handle, self._handle = self._handle, None
        if handle is not None:
            # Closing releases it when this is the last descriptor. Never LOCK_UN: unlocking
            # through any duplicate would release a lock a supervised worker still relies on.
            if not shared:
                try:
                    fcntl.flock(handle, fcntl.LOCK_UN)
                except OSError:
                    pass
            handle.close()

    def conflicts(self, socket_path, *, store_id=None, state_dir=None) -> list:
        """Every record for this scope that names a different store, alive or not."""
        out = []
        try:
            entries = sorted(self.root.glob("*.json"))
        except OSError:
            return out
        mine = self.key(socket_path)
        for entry in entries:
            try:
                found = json.loads(entry.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if found.get("scopeKey") != mine:
                continue
            same_store = store_id is not None and found.get("storeId") == store_id
            same_dir = state_dir is not None and found.get("stateDir") == str(state_dir)
            if same_store and same_dir:
                continue
            out.append({
                "stateDir": found.get("stateDir"), "storeId": found.get("storeId"),
                "installationId": found.get("installationId"), "pid": found.get("pid"),
                "live": _is_live(found), "reason": "same_scope_different_store",
            })
        return out


def _is_live(record) -> bool:
    pid, recorded_boot = record.get("pid"), record.get("bootId")
    if not pid:
        return False
    if recorded_boot is not None and recorded_boot != boot_id():
        # A pid from a previous boot cannot be this record's process.
        return False
    if process_state(int(pid)) in (None, "Z"):
        return False
    ticks = start_ticks(int(pid))
    if ticks is None:
        return False
    recorded = record.get("startTicks")
    return recorded is None or int(recorded) == ticks


def installation_id(state_dir) -> str:
    """This checkout plus this state directory. A second of either is a second install."""
    package = Path(__file__).resolve().parent
    material = f"{package}\0{Path(state_dir).expanduser().absolute().resolve()}"
    return hashlib.sha256(material.encode()).hexdigest()[:16]


PR_SET_PDEATHSIG = 1


def arm_parent_death_signal(expected_parent: int) -> dict:
    """Ask the kernel to signal this process when its parent dies, then check it is not late.

    PR_SET_PDEATHSIG is not delivered retrospectively, so a parent that died before the child
    armed it leaves an orphan. The getppid check immediately afterwards is what closes that
    window, and it can only be done here, in the child.

    Armed in the child's own bootstrap rather than through preexec_fn, which is unsafe once
    the supervisor has started the adapter's transport thread.
    """
    armed, detail = False, None
    try:
        import ctypes

        libc = ctypes.CDLL("libc.so.6", use_errno=True)
        armed = libc.prctl(PR_SET_PDEATHSIG, signal.SIGTERM, 0, 0, 0) == 0
        if not armed:
            detail = f"prctl failed: errno {ctypes.get_errno()}"
    except (OSError, AttributeError, ValueError) as error:
        detail = f"{type(error).__name__}: {error}"
    actual = os.getppid()
    orphaned = actual != expected_parent
    return {"armed": armed, "detail": detail, "parent": actual, "orphaned": orphaned}


class ProcessHandle:
    """A handle to a specific process, not to a number.

    Opened FIRST, verified second, signalled third, all through the same descriptor, and kept
    open until the process is gone - including across the grace period before SIGKILL. That
    ordering is the point: a pid verified and then signalled can belong to a different process
    by the time the signal lands. Holding the descriptor also reserves the pid, so /proc stays
    truthful for us while we wait.

    Where pidfd is unavailable this refuses rather than falling back to os.kill. A signal that
    cannot be aimed is worse than no signal.
    """

    def __init__(self, pid):
        self.pid = int(pid)
        self.fd = None
        self.detail = None
        self.already_gone = False
        opener = getattr(os, "pidfd_open", None)
        if opener is None or getattr(signal, "pidfd_send_signal", None) is None:
            self.detail = "this build has no pidfd support, so a signal cannot be aimed"
            return
        try:
            self.fd = opener(self.pid)
        except ProcessLookupError:
            self.already_gone = True
            self.detail = "the process is already gone"
        except OSError as error:
            self.detail = f"{type(error).__name__}: {error}"

    @property
    def usable(self) -> bool:
        return self.fd is not None

    def alive(self) -> bool:
        """A zombie is not alive.

        Its /proc entry survives until its parent reaps it, and holding a pidfd keeps the pid
        reserved, so a liveness check that only asks whether /proc/<pid> exists would wait out
        the whole grace period and then report a terminated process as still running.
        """
        state = process_state(self.pid)
        return state is not None and state != "Z"

    def send(self, sig) -> bool:
        if self.fd is None:
            return False
        try:
            signal.pidfd_send_signal(self.fd, sig)
        except ProcessLookupError:
            return True
        except OSError as error:
            self.detail = f"{type(error).__name__}: {error}"
            return False
        return True

    def close(self) -> None:
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None


@dataclass
class ServiceIntent:
    """What the owner asked for, independent of whether anything is running.

    Absent means never configured, which is NOT enabled. Nothing here writes enabled=True as a
    side effect of starting; only an explicit enable does.
    """

    path: Path

    def read(self) -> dict:
        try:
            data = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return {"enabled": False, "configured": False, "changedAt": None, "changedBy": None}
        return {"enabled": bool(data.get("enabled")), "configured": True,
                "changedAt": data.get("changedAt"), "changedBy": data.get("changedBy")}

    def write(self, *, enabled: bool, actor: str) -> dict:
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        payload = {"enabled": bool(enabled), "changedAt": _now(), "changedBy": actor}
        # Replaced atomically, for the same reason the daemon record is. read() treats an
        # unreadable document as not configured and therefore disabled, and the supervisor
        # re-reads intent at every worker boundary - so a reader landing in the truncated
        # middle of this write would stop a service its owner had left enabled.
        temporary = self.path.with_name(f".{self.path.name}.{os.getpid()}")
        try:
            temporary.write_text(json.dumps(payload, indent=2), encoding="utf-8")
            os.replace(temporary, self.path)
        except OSError:
            temporary.unlink(missing_ok=True)
            raise
        return dict(payload, configured=True)


@dataclass
class LaunchPolicy:
    """Which execution policy file this service launches its daemon with.

    The daemon reads its role policy from its OWN environment, once, at startup. Until this
    record existed the only thing that put the variable there was the shell that typed the
    command, so the policy a service ran on was a property of whoever last typed it: a restart
    from a shell without it left the worker able to read no policy at all and every role-bound
    delivery withheld, while the file, the installation and the owner's intent were all
    unchanged. The declaration belongs to the service, so a launch carries it.

    It names the FILE and never copies what is in it. Every launch still reads the policy from
    that path, so editing the policy takes effect at the next restart exactly as before and
    this is not a second place a policy can be written.

    Kept out of service.json for the reason worker-policy.json is kept out of daemon.json:
    enable and disable replace that document wholesale, and a disable followed by an enable
    must not silently drop the declaration every later launch depends on.
    """

    path: Path

    def read(self) -> dict:
        """The declaration, or its absence, or the fact that it cannot be read.

        Those are three answers rather than two. An unreadable record read as an absent one
        would send the next launch back to whatever the calling shell happened to carry, which
        is the failure this record exists to end - so corruption is reported and absence is
        not inferred from it.
        """
        try:
            raw = self.path.read_text(encoding="utf-8")
        except FileNotFoundError:
            return {"path": None, "declaredAt": None, "declaredBy": None, "unreadable": None}
        except OSError as error:
            return {"path": None, "declaredAt": None, "declaredBy": None,
                    "unreadable": f"the launch declaration could not be read: {error}"}
        try:
            data = json.loads(raw)
        except ValueError as error:
            return {"path": None, "declaredAt": None, "declaredBy": None,
                    "unreadable": f"the launch declaration is not readable JSON: {error}"}
        declared = data.get("path") if isinstance(data, dict) else None
        if not isinstance(declared, str) or not declared.strip():
            return {"path": None, "declaredAt": None, "declaredBy": None,
                    "unreadable": "the launch declaration names no execution policy file"}
        return {"path": declared, "declaredAt": data.get("declaredAt"),
                "declaredBy": data.get("declaredBy"), "unreadable": None}

    def write(self, *, path: str, actor: str) -> dict:
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        payload = {"schemaVersion": 1, "path": path, "declaredAt": _now(), "declaredBy": actor}
        # Replaced atomically, like every other record here. A launch that read the truncated
        # middle of this file would refuse to start a service whose declaration was fine.
        temporary = self.path.with_name(f".{self.path.name}.{os.getpid()}")
        try:
            temporary.write_text(json.dumps(payload, indent=2), encoding="utf-8")
            os.replace(temporary, self.path)
        except OSError:
            temporary.unlink(missing_ok=True)
            raise
        return dict(payload)

    def forget(self) -> dict:
        """Drop the declaration and report what it was, so a receipt can name it."""
        before = self.read()
        self.path.unlink(missing_ok=True)
        return before


def canonical_policy_path(value):
    """One spelling of a policy file, for comparing two of them.

    A declaration is stored as the operator wrote it, absolute and expanded but not resolved,
    because a symlink they chose is a path they chose. Comparison is a different question:
    './p', '~/p', a trailing slash and an alias of one file are one file, and reporting them
    as two would refuse a launch over a disagreement that does not exist.
    """
    try:
        return str(Path(value).expanduser().absolute().resolve())
    except (OSError, RuntimeError, ValueError):  # pragma: no cover - a path the OS refuses
        return str(value)


def launch_policy_refusal(resolution):
    """The refusal a launch owes when it cannot say which policy file it would read.

    Two states, one answer: two files named at once, and a record that cannot be read. Both
    are questions only the operator can settle, and both are decided before anything is
    stopped, so a disagreement is a diagnosis rather than an outage.
    """
    if resolution["source"] not in ("conflict", "unreadable_record"):
        return None
    return {"ok": False,
            "reason": ("launch_policy_conflict" if resolution["source"] == "conflict"
                       else "launch_policy_unreadable"),
            "detail": resolution["detail"], "launchPolicy": resolution}


class RelayService:
    """Start, stop and describe the daemon that belongs to THIS installation."""

    def __init__(self, selection, *, socket_path=None, scope=None, store_id=None,
                 store_unidentified=False):
        self.selection = selection
        self.socket_path = socket_path
        self.store_id = store_id
        # A store file is HERE and its identity could not be read. Different in kind from
        # store_id being None because there is no store yet, which is an ordinary pre-launch
        # state. Only a caller that measured the difference sets it; every other construction
        # keeps the behaviour it had.
        self.store_unidentified = store_unidentified
        self.launch_id = None
        # Set only by an explicit --takeover, and only ever read by ScopeRegistry.claim.
        self.takeover = False
        if scope is None:
            root, authority = resolve_scope_root()
            scope = ScopeRegistry(root, authority)
        self.scope = scope
        self.intent = ServiceIntent(selection.path / SERVICE_INTENT)
        self.launch = LaunchPolicy(selection.path / LAUNCH_POLICY)
        # The resolution the current launch was decided under, frozen by start() and applied
        # by the launcher. Resolving twice would let a declaration written in between produce
        # a conflict AFTER a restart had already stopped the service.
        self.launch_environment = None
        self.installation_id = installation_id(selection.path)

    @property
    def record_path(self) -> Path:
        return self.selection.path / DAEMON_RECORD

    def record(self):
        try:
            return json.loads(self.record_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return None

    def write_record(self, payload: dict) -> dict:
        self.selection.path.mkdir(mode=0o700, parents=True, exist_ok=True)
        # Replaced atomically. write_text truncates first, and _note rewrites this file on
        # every worker boundary, so a concurrent stop could read the empty or half-written
        # middle, call a running supervisor absent and return not_running without signalling
        # its worker - or miss the foreign markers and write a stop request for someone else.
        temporary = self.record_path.with_name(f".{self.record_path.name}.{os.getpid()}")
        try:
            temporary.write_text(json.dumps(payload, indent=2), encoding="utf-8")
            os.replace(temporary, self.record_path)
        except OSError:
            temporary.unlink(missing_ok=True)
            raise
        return payload

    def new_record(self, *, pid, token=None, worker_pid=None) -> dict:
        return {
            "pid": pid, "startTicks": start_ticks(pid), "bootId": boot_id(),
            "workerPid": worker_pid, "token": token,
            "launchId": self.launch_id,
            "takeover": self.takeover or None,
            # Written by supervise once on_start has returned. Absent means "not serving yet".
            "readyAt": None,
            "storeId": self.store_id, "installationId": self.installation_id,
            "stateDir": str(self.selection.path), "socketPath": self.socket_path,
            "scopeAuthority": self.scope.authority, "scopeRoot": str(self.scope.root),
            "startedAt": _now(), "restarts": 0, "consecutiveFailures": 0, "lastExit": None,
            "nextRestartAt": None,
        }

    def publish_worker_policy(self, policy: dict) -> dict:
        """Written by the bounded worker, never by a diagnostic caller or supervisor.

        daemon.json belongs to the supervisor. A separate atomic receipt avoids overwriting
        its workerPid while it registers a newly spawned worker. Readers refuse that short
        registration race until the two records actually agree.
        """
        record = self.record() or {}
        pid = os.getpid()
        if record.get("pid") not in (pid, os.getppid()) or not record.get("token"):
            raise ValueError("worker policy publication needs this process's service run")
        info = self.selection.db_path.stat()
        payload = {
            "schemaVersion": 1, "policy": policy, "observedAt": _now(),
            "worker": {"pid": pid, "startTicks": start_ticks(pid), "bootId": boot_id()},
            "service": {
                key: record.get(key) for key in (
                    "token", "pid", "startTicks", "installationId", "stateDir",
                    "socketPath", "storeId", "scopeRoot", "scopeAuthority",
                )
            },
        }
        payload["service"].update(dbDevice=info.st_dev, dbInode=info.st_ino)
        path = self.selection.path / WORKER_POLICY
        temporary = path.with_name(f".{path.name}.{pid}.{uuid.uuid4().hex}")
        try:
            with temporary.open("x", encoding="utf-8") as handle:
                json.dump(payload, handle)
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)
        return payload

    def read_worker_policy(self) -> dict:
        """Read the currently serving worker's snapshot without opening or repairing a Store.

        This is evidence between cooperating same-user processes, not authentication against
        a process permitted to forge their files. No lock is held across a later host call.
        """
        def absent(reason):
            return {"observed": False, "reason": reason, "policy": None}

        try:
            record = self.record()
            path = self.selection.path / WORKER_POLICY
            with path.open("rb") as handle:
                raw = handle.read(WORKER_POLICY_LIMIT + 1)
            if len(raw) > WORKER_POLICY_LIMIT:
                return absent("worker_policy_unreadable")
            receipt = json.loads(raw)
            if not isinstance(record, dict) or not isinstance(receipt, dict):
                return absent("worker_policy_unreadable")
            if type(receipt.get("schemaVersion")) is not int or receipt["schemaVersion"] != 1:
                return absent("worker_policy_version_unknown")
            worker, run = receipt.get("worker"), receipt.get("service")
            if not isinstance(worker, dict) or not isinstance(run, dict):
                return absent("worker_policy_unreadable")
            if not isinstance(receipt.get("policy"), dict):
                return absent("worker_policy_unreadable")
            if (type(record.get("pid")) is not int or record["pid"] <= 0
                    or type(record.get("startTicks")) is not int or record["startTicks"] < 0
                    or (record.get("workerPid") is not None and
                        (type(record["workerPid"]) is not int or record["workerPid"] <= 0))):
                return absent("worker_policy_process_mismatch")
            expected_pid = record.get("workerPid") or record.get("pid")
            expected_ticks = record.get("workerStartTicks") if record.get("workerPid") else record.get("startTicks")
            if (type(expected_pid) is not int or expected_pid <= 0
                    or type(expected_ticks) is not int or expected_ticks < 0
                    or type(worker.get("pid")) is not int
                    or type(worker.get("startTicks")) is not int
                    or worker.get("pid") != expected_pid
                    or worker.get("startTicks") != expected_ticks):
                return absent("worker_policy_process_mismatch")
            if (not record.get("bootId") or worker.get("bootId") != record["bootId"]
                    or record["bootId"] != boot_id()):
                return absent("worker_policy_boot_mismatch")
            identity = {key: record.get(key) for key in (
                "token", "pid", "startTicks", "installationId", "stateDir",
                "socketPath", "storeId", "scopeRoot", "scopeAuthority",
            )}
            info = self.selection.db_path.stat()
            identity.update(dbDevice=info.st_dev, dbInode=info.st_ino)
            if (run != identity or not record.get("token") or not self.store_id
                    or record.get("storeId") != self.store_id
                    or record.get("installationId") != self.installation_id
                    or record.get("stateDir") != str(self.selection.path)
                    or record.get("socketPath") != self.socket_path
                    or record.get("scopeRoot") != str(self.scope.root)
                    or record.get("scopeAuthority") != self.scope.authority):
                return absent("worker_policy_service_mismatch")
            process = ProcessHandle(expected_pid)
            try:
                if (not process.usable or not process.alive()
                        or process_state(expected_pid) in ("T", "t", "Z", "X", "x")
                        or start_ticks(expected_pid) != expected_ticks):
                    return absent("worker_policy_process_unavailable")
                locks = [self.selection.path / DAEMON_LOCK]
                scope_record = None
                if self.socket_path:
                    scope_record = self.scope.read(self.socket_path)
                    if (not isinstance(scope_record, dict)
                            or any(scope_record.get(key) != record.get(key) for key in (
                                "token", "pid", "startTicks", "bootId", "storeId",
                                "installationId", "stateDir", "socketPath",
                            ))):
                        return absent("worker_policy_scope_mismatch")
                    locks.append(self.scope.root / f"{self.scope.key(self.socket_path)}.lock")
                for lock in locks:
                    if not _existing_lock_held(lock):
                        return absent("worker_policy_lock_unheld")
                current = self.selection.db_path.stat()
                if (record != self.record()
                        or (info.st_dev, info.st_ino) != (current.st_dev, current.st_ino)
                        or (self.socket_path and scope_record != self.scope.read(self.socket_path))
                        or not process.alive() or start_ticks(expected_pid) != expected_ticks
                        or process_state(expected_pid) in ("T", "t", "Z", "X", "x")):
                    return absent("worker_policy_observation_changed")
            finally:
                process.close()
            return {"observed": True, "reason": None, "policy": receipt["policy"],
                    "worker": worker, "service": run, "observedAt": receipt.get("observedAt")}
        except (OSError, ValueError, TypeError):
            return absent("worker_policy_unreadable")

    def ownership(self, record=None):
        """ours | foreign | unverifiable | none, decided through a real process handle."""
        record = self.record() if record is None else record
        if not record or not record.get("pid"):
            if record and record.get("workerPid"):
                # A supervisor that died leaving its worker alive keeps the worker identity
                # on purpose, so this record still names something stop() will reach. It has
                # to be classified first: answering none here sent the orphan path straight
                # at another installation's worker.
                foreign = self._foreign_markers(record)
                if foreign:
                    return FOREIGN, None, "; ".join(foreign)
                unreadable = self._store_unreadable(record)
                if unreadable is not None:
                    return UNVERIFIABLE, None, unreadable
                if record.get("bootId") is None and boot_id() is not None:
                    # The same rule the live-supervisor path gets. _stop_worker checks only
                    # start ticks, and a reboot resets those along with the pid space, so an
                    # unrelated process holding the old worker number would be signalled.
                    return UNVERIFIABLE, None, (
                        "no boot id is recorded, so this worker pid cannot be"
                        " distinguished from one reused after a reboot"
                    )
            return NONE, None, "no daemon record"
        # Read from the record itself, so they survive the supervisor's death. A foreign
        # installation whose supervisor is gone can still have a live worker recorded, and
        # answering none there sent stop() down the orphan path and straight at it.
        foreign = self._foreign_markers(record)
        handle = ProcessHandle(record["pid"])
        if handle.already_gone:
            if foreign:
                return FOREIGN, handle, "; ".join(foreign)
            unreadable = self._store_unreadable(record)
            if unreadable is not None:
                return UNVERIFIABLE, handle, unreadable
            if (record.get("workerPid") and record.get("bootId") is None
                    and boot_id() is not None):
                # The same rule the cleared-pid path above already applies, and this path is
                # just as stale: the supervisor is gone but its worker number is still in the
                # record, so stop() falls through to _stop_worker - which validates start
                # ticks only. A reboot resets those along with the pid space, so an unrelated
                # process holding the old worker number would be signalled.
                return UNVERIFIABLE, handle, (
                    "no boot id is recorded, so this worker pid cannot be distinguished from"
                    " one reused after a reboot"
                )
            return NONE, handle, "the recorded process is gone"
        if not handle.usable:
            if foreign:
                return FOREIGN, handle, "; ".join(foreign)
            return UNVERIFIABLE, handle, handle.detail
        mismatches = list(foreign)
        ticks = start_ticks(record["pid"])
        if mismatches:
            # Decided BEFORE the start-time question. Installation, store and boot each prove
            # the record is foreign on their own, and answering unverifiable would discard a
            # definite answer in favour of an uncertain one.
            return FOREIGN, handle, "; ".join(mismatches)
        # After the definite answers and before ours, which is the precedence this function
        # already keeps: what is proven first, then what could not be established.
        unreadable = self._store_unreadable(record)
        if unreadable is not None:
            return UNVERIFIABLE, handle, unreadable
        if record.get("bootId") is None and boot_id() is not None:
            # A reboot resets both the pid space and the start-tick counter, so a record with
            # no boot written on a host that HAS one cannot be ruled out as pre-reboot: an
            # unrelated process can hold the old number with a matching tick count.
            # Conditional on the host, because where no boot id is available at all every
            # record lacks one, and refusing them all would leave a service unstoppable by
            # its own owner - trading a narrow risk for a certain failure.
            return UNVERIFIABLE, handle, (
                "no boot id is recorded, so a pid from before a reboot cannot be ruled out"
            )
        if record.get("startTicks") is None or ticks is None:
            # Nothing proves it foreign, and without a start time a pid is just a number that
            # anything could be holding now. Unverifiable is the honest answer; stop refuses.
            return UNVERIFIABLE, handle, (
                "no start time is available for this pid, so identity cannot be established"
            )
        if ticks != record["startTicks"]:
            mismatches.append("the pid was reused by a different process")
        if mismatches:
            return FOREIGN, handle, "; ".join(mismatches)
        return OURS, handle, None

    def _foreign_markers(self, record) -> list:
        """What the record alone proves about who owns it, with no live process required."""
        markers = []
        if record.get("bootId") not in (None, boot_id()):
            markers.append("recorded before a different boot")
        if record.get("installationId") != self.installation_id:
            markers.append("another installation owns it")
        if self.store_id is not None and record.get("storeId") not in (None, self.store_id):
            markers.append("it is using a different store")
        return markers

    def _store_unreadable(self, record):
        """Why this record's store cannot be compared against ours, or None while it can.

        Deliberately NOT a foreign marker. Every marker above PROVES the record foreign on its
        own, and an identity we could not read proves nothing about who owns it - only that the
        one question separating two stores of one installation has no answer here.

        It exists because refusing to read is not the same as having nothing to compare. A
        diagnostic read that cannot bind itself to this store returns no identity at all, and
        reading that silence as "no expectation" would skip the comparison in exactly the case
        it matters: the store being moved under the command. Absence is never agreement, so
        this answers unverifiable and the lifecycle commands refuse instead of signalling.

        A record naming no store is untouched, because there is genuinely nothing to compare -
        which is what keeps a first launch working, since it adopts the child's store id after
        the child reports.
        """
        # Holding an identity settles it: the ordinary store comparison in _foreign_markers
        # applies and there is nothing unreadable left to report. Asked here rather than
        # cleared at each place an identity arrives, so the flag cannot go stale - _await_launch
        # adopts the child's store id after a first launch, and a flag left set there would
        # have reported our own new service as unverifiable.
        if self.store_id is not None or not (self.store_unidentified and record.get("storeId")):
            return None
        return (
            "a store is present here and its identity could not be read, so the store this"
            f" record names ({record.get('storeId')}) cannot be compared against it"
        )

    def _worker_identified(self, record) -> bool:
        """Whether the recorded worker pid provably names OUR worker, still running.

        The same three questions _stop_worker asks before it signals - the process exists, it
        is readable, and its start time matches what we recorded - plus the boot question,
        which ownership() does not always reach. When the recorded SUPERVISOR pid is gone it
        answers none at the already-gone branch, before its own missing-boot check runs, so a
        record written before a reboot arrives here naming a worker number that an unrelated
        process may now hold.
        """
        pid = (record or {}).get("workerPid")
        if not pid:
            return False
        if record.get("bootId") is None and boot_id() is not None:
            # Conditional on the host for the reason ownership() gives: where no boot id is
            # available at all, every record lacks one, and refusing them all would leave a
            # service unusable by its own owner.
            return False
        handle = ProcessHandle(pid)
        try:
            if handle.already_gone or not handle.usable:
                return False
            ticks = record.get("workerStartTicks")
            current = start_ticks(pid)
            return ticks is not None and current is not None and current == ticks
        finally:
            handle.close()

    def _holder_is_ours(self, record, owner, markers) -> bool:
        """Whether whatever is holding this state directory can be attributed to us.

        Not the same question as "is the owner ours". A supervisor that died leaving our own
        worker alive answers none, and that worker still holds the daemon lock through the
        descriptor it inherited - refusing there would stop an owner from disabling their own
        orphan, which stop() deliberately still reaches.

        Everything else that answers none while the lock is held is a holder we cannot name:
        no record at all, a record whose process is gone, or a supervisor that has taken the
        lock and not yet published. Writing the shared intent on any of those is how a refused
        command still shuts down another installation.
        """
        if markers:
            return False
        if owner == OURS:
            return True
        return owner == NONE and self._worker_identified(record)

    def lock_is_held(self) -> bool:
        """Probed by trying to take it: the lock is the only honest liveness signal."""
        path = self.selection.path / DAEMON_LOCK
        if not path.exists():
            return False
        try:
            handle = open(path, "a+")
        except OSError:
            return False
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return True
        else:
            fcntl.flock(handle, fcntl.LOCK_UN)
            return False
        finally:
            handle.close()

    @contextmanager
    def daemon_lock_if_free(self):
        """Take the daemon lock and KEEP it for the duration of a decision.

        lock_is_held probes and releases, which is enough to describe a state but not to act
        on one: between the probe and whatever the caller does next, a supervisor can acquire
        the lock. Holding it across the decision closes that gap - a supervisor cannot start
        while we hold it - and failing to take it is itself the answer that someone is there.

        Opened with 'a+' so the probe is atomic even on a state directory that has never run
        a daemon: a supervisor starting there has to create and lock this same file.

        Only CONTENTION yields None. Mapping every OSError to "someone holds it" made an
        unreadable directory or an exhausted descriptor table indistinguishable from a running
        supervisor, and callers act on that answer - stop reports a replacement, disable
        refuses an owner. An operational failure is not a statement about who owns this
        directory, so it is raised and reported as itself. Which means opening the file has to
        sit OUTSIDE the contention handler: open() raises EACCES too, for a lock file that has
        become unreadable, and reading that as contention is the same mistake one level down.
        """
        path = self.selection.path / DAEMON_LOCK
        self.selection.path.mkdir(mode=0o700, parents=True, exist_ok=True)
        handle = open(path, "a+")
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as error:
            handle.close()
            handle = None
            # Only flock's contention errnos. Anything else from flock is operational too.
            if error.errno not in (errno.EACCES, errno.EAGAIN):
                raise
        try:
            yield handle
        finally:
            if handle is not None:
                fcntl.flock(handle, fcntl.LOCK_UN)
                handle.close()

    # -------------------------------------------------------------- operations

    def authority_check(self, *, allow_isolated: bool):
        if self.scope.authority == ISOLATED and not allow_isolated:
            return {
                "ok": False, "reason": "isolated_scope_not_allowed",
                "detail": f"{SCOPE_ENV} is set, which moves the ownership record away from the"
                          " one authority every launch shares. Pass --allow-isolated-scope if"
                          " that is deliberate; single ownership is not enforced across"
                          " differently isolated roots.",
            }
        return {"ok": True, "reason": None}

    def _holder_refusal(self):
        """None when nothing foreign holds this state directory, or the refusal that says so.

        Called with the daemon lock already refused to this process, by every writer of a
        record this state directory SHARES. One spelling of that question on purpose: two
        copies is how the classification one caller validates against drifts away from the one
        every other caller performs.
        """
        record = self.record()
        owner, handle, detail = self.ownership(record)
        if handle is not None:
            handle.close()
        # Read from the record as well as from the classification. A supervisor on its way
        # out clears its pids before releasing the lock, and ownership answers none once both
        # are absent - exactly the interval in which a foreign shutdown could be reversed.
        markers = self._foreign_markers(record) if record else []
        if self._holder_is_ours(record, owner, markers):
            return None
        return {"ok": False,
                "reason": ("not_ours" if (owner == FOREIGN or markers)
                           else "ownership_unverifiable"),
                "detail": detail or "; ".join(markers) or (
                    "the daemon lock is held but nothing here identifies its owner")}

    def enable(self, *, actor: str = "cli") -> dict:
        # The same ownership question disable asks, and decided and written under the SAME
        # lock for the same reason. service.json is shared by everything pointed at this
        # state directory, and classifying then writing are two operations: a foreign
        # supervisor can pass its own enabled-intent check and take the lock in between, and
        # this call would then write enabled=true over a disable that happened during the
        # handoff - leaving that supervisor running and eligible to restart. Holding the lock
        # across both means no supervisor can start while we decide, and failing to take it
        # is itself the answer that one is already there to be classified.
        with self.daemon_lock_if_free() as held:
            if held is None:
                refusal = self._holder_refusal()
                if refusal is not None:
                    return dict(refusal, intent=self.intent.read(),
                                note="intent is shared with the owner of this state directory"
                                     " and was left unchanged")
            # Either nothing is running here - a stopped registration belonging to someone
            # else is not a reason to refuse an owner configuring their own installation, and
            # refusing then would make a state directory unusable forever - or what is
            # running is ours.
            return {"ok": True, "reason": None,
                    "intent": self.intent.write(enabled=True, actor=actor)}

    def disable(self, *, actor: str = "cli", timeout: float = 10.0) -> dict:
        # Ownership BEFORE the intent write. service.json is shared by everything pointed at
        # this state directory, and a foreign supervisor re-reads it at every worker
        # boundary: writing enabled=false and only then discovering the supervisor is not
        # ours refused the stop while still shutting that supervisor down at its next
        # boundary. A refusal that still has an effect is not a refusal.
        #
        # Decided and written UNDER the daemon lock. Checking ownership and then writing are
        # two operations, and a foreign supervisor can start between them: it reads
        # enabled=true, takes the lock, and then sees the intent we changed afterwards and
        # exits at its next boundary. Holding the lock means no supervisor can start while we
        # decide, and failing to take it means one is already there to be classified.
        refusal = None
        with self.daemon_lock_if_free() as held:
            if held is None:
                # Someone holds it. Re-read: a supervisor that started between our read and
                # now has published its record, or is about to.
                held_by_other = self._holder_refusal()
                if held_by_other is not None:
                    refusal = dict(
                        held_by_other,
                        intent=self.intent.read(),
                        stop={"ok": False, "reason": "refused",
                              "supervisor": "untouched", "worker": "untouched"},
                        note="intent is shared with the owner of this state directory and"
                             " was left unchanged",
                    )
            if refusal is None:
                written = self.intent.write(enabled=False, actor=actor)
        if refusal is not None:
            return refusal
        # Outside the lock deliberately: stop() takes it for its own decision, and holding it
        # here would make stop misread a free lock as ours.
        stopped = self.stop(actor=actor, timeout=timeout)
        # A stop that had nothing to stop is not a failure; the intent is what disable owns.
        failed = not stopped["ok"] and stopped["reason"] != "not_running"
        return {"ok": not failed, "reason": stopped["reason"] if failed else None,
                "intent": written, "stop": stopped}

    def resolve_launch_policy(self, environ=None) -> dict:
        """Which execution policy a daemon launched from here reads, and where it came from.

        An INPUT to the next launch and never a reading of a running one. What the serving
        worker resolved is its own published receipt, and the two disagree from the moment a
        declaration changes until the service is restarted, which is why status reports them
        side by side rather than as one answer.

        The declaration wins where there is one. A process that exports a different file is
        not overruled quietly: that is refused, because choosing between two policy files by
        preference is how the policy a service runs on became a property of whoever typed the
        command. With no declaration the environment still launches the service, exactly as it
        did before this record existed, and says that nothing recorded it.
        """
        from . import rolepolicy

        environ = os.environ if environ is None else environ
        stated = (environ.get(rolepolicy.ENVIRONMENT_VARIABLE) or "").strip()
        record = self.launch.read()
        declared = record["path"]
        answer = {"variable": rolepolicy.ENVIRONMENT_VARIABLE, "path": None, "source": None,
                  "record": declared, "environment": stated or None,
                  "declaredAt": record["declaredAt"], "declaredBy": record["declaredBy"],
                  "state": None, "digest": None, "persisted": None, "detail": None,
                  "hint": None}
        if record["unreadable"]:
            answer["source"] = "unreadable_record"
            answer["detail"] = record["unreadable"]
            answer["hint"] = (
                "declare the file again with service declare --execution-policy, or drop the"
                " record with service declare --forget-execution-policy. A launch does not"
                " fall back to this process's environment to cover an unreadable record"
            )
            return answer
        if declared and stated and (
                canonical_policy_path(stated) != canonical_policy_path(declared)):
            answer["source"] = "conflict"
            answer["detail"] = (
                f"this service declares {declared!r} and {rolepolicy.ENVIRONMENT_VARIABLE} in"
                f" this process names {stated!r}"
            )
            answer["hint"] = (
                "two files are not a preference: unset the variable to launch on the"
                " declaration, or declare that other file"
            )
            return answer
        if declared:
            answer.update(path=declared, source="record", persisted=True)
        elif stated:
            # Machine-readable, because the difference between these two sources is the whole
            # subject: one survives a restart typed anywhere, the other is this shell's.
            answer.update(path=stated, source="environment", persisted=False)
            answer["hint"] = (
                "this launch takes the policy from this process's environment and nothing"
                " records it, so a restart typed anywhere else loses it. Record it with"
                " service declare --execution-policy"
            )
        else:
            answer["detail"] = (
                f"no execution policy is declared for this service and"
                f" {rolepolicy.ENVIRONMENT_VARIABLE} is not set in this process, so a daemon"
                " launched from here can read none and withholds every role-bound delivery"
            )
            return answer
        reading = self._policy_reading(answer["path"])
        answer["state"], answer["digest"] = reading["state"], reading["digest"]
        if reading["detail"]:
            answer["detail"] = reading["detail"]
        return answer

    def _policy_reading(self, path) -> dict:
        """Whether the file a launch would name still resolves, and under which digest.

        A declaration checked when it was written is not a declaration still valid now: the
        file can be edited, moved or removed afterwards. Read through rolepolicy's explicit
        environment entry, which neither reads nor writes this process's own snapshot -
        reporting on a file is not the same act as adopting it.

        Reported rather than refused. The daemon already has the honest answer for a policy it
        cannot read: it withholds role-bound deliveries and says so. Refusing to start would
        turn a service that still does everything else into no service at all, over a file
        this command is only describing.
        """
        from . import rolepolicy

        resolved = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: path})
        if resolved:
            return {"state": "declared", "digest": resolved.digest, "detail": None}
        return {"state": "unreadable", "digest": None, "detail": resolved.detail}

    def declare_launch_policy(self, path, *, actor: str = "cli") -> dict:
        """Record the execution policy file this service launches its daemon with.

        Proven to resolve before it is recorded. A declaration nobody checked would be
        discovered at the next restart by a worker that then withholds every role-bound
        delivery, which is the failure this record exists to end rather than to relocate.

        Deliberately not written as a side effect of starting. The sibling rule is the one
        ServiceIntent states: nothing writes the owner's intent because a start happened, and
        a launch that quietly recorded whatever environment it was typed from would pin one
        shell's export as this service's policy for every restart after it.
        """
        from . import rolepolicy

        candidate = str(Path(path).expanduser().absolute())
        resolved = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: candidate})
        if not resolved:
            return {"ok": False, "reason": "execution_policy_unreadable", "path": candidate,
                    "detail": resolved.detail,
                    "launchPolicy": self.resolve_launch_policy()}
        with self.daemon_lock_if_free() as held:
            if held is None:
                refusal = self._holder_refusal()
                if refusal is not None:
                    return dict(refusal, launchPolicy=self.resolve_launch_policy(),
                                note="the launch declaration is shared with the owner of this"
                                     " state directory and was left unchanged")
            written = self.launch.write(path=candidate, actor=actor)
        return {"ok": True, "reason": None, "declared": written,
                "launchPolicy": self.resolve_launch_policy(),
                "note": "a running daemon keeps the policy it was launched with until it is"
                        " restarted"}

    def forget_launch_policy(self, *, actor: str = "cli") -> dict:
        """Drop the declaration, so the next launch falls back to its own environment.

        This can only ever make a launch resolve LESS. With nothing declared and nothing set,
        the daemon reads no policy and withholds every role-bound delivery, which is the
        refusal this product already treats as the honest answer.
        """
        with self.daemon_lock_if_free() as held:
            if held is None:
                refusal = self._holder_refusal()
                if refusal is not None:
                    return dict(refusal, launchPolicy=self.resolve_launch_policy(),
                                note="the launch declaration is shared with the owner of this"
                                     " state directory and was left unchanged")
            before = self.launch.forget()
        return {"ok": True, "reason": None, "forgot": before["path"], "actor": actor,
                "launchPolicy": self.resolve_launch_policy(),
                "note": "a running daemon keeps the policy it was launched with until it is"
                        " restarted"}

    def status(self) -> dict:
        record = self.record()
        owner, handle, detail = self.ownership(record)
        if handle is not None:
            handle.close()
        intent = self.intent.read()
        held = self.lock_is_held()
        launch = self.resolve_launch_policy()
        # A declaration is what the NEXT launch reads. What the serving worker resolved is its
        # own receipt, and the two disagree from the moment a declaration changes until the
        # service is restarted - so both are here, compared, rather than one of them being
        # read as the other.
        observed = self.read_worker_policy()
        running = ((observed.get("policy") or {}).get("digest")
                   if observed.get("observed") else None)
        launch["appliesTo"] = "the next daemon launched from this state directory"
        launch["runningDigest"] = running
        launch["matchesRunning"] = (
            "same" if running and launch["digest"] and running == launch["digest"]
            else "different" if running and launch["digest"] else "unknown"
        )
        return {
            "enabled": intent["enabled"], "intentConfigured": intent["configured"],
            "intentChangedAt": intent["changedAt"], "intentChangedBy": intent["changedBy"],
            # What the next launch would read, never a claim about the running one.
            "launchPolicy": launch,
            # Liveness is the lock, not the recorded pid: a supervisor that died leaving a
            # worker alive still holds it through the descriptor the worker inherited.
            "running": held,
            "ownership": owner, "ownershipDetail": detail,
            "pid": (record or {}).get("pid"), "workerPid": (record or {}).get("workerPid"),
            "startedAt": (record or {}).get("startedAt"),
            "storeId": (record or {}).get("storeId"), "socketPath": self.socket_path,
            "installationId": self.installation_id,
            "stateDirectory": str(self.selection.path),
            "scopeAuthority": self.scope.authority, "scopeRoot": str(self.scope.root),
            "lock": "held" if held else "free",
            "staleRecord": bool(record and record.get("pid") and not held),
            "restarts": (record or {}).get("restarts"),
            "consecutiveFailures": (record or {}).get("consecutiveFailures"),
            "lastExit": (record or {}).get("lastExit"),
            "nextRestartAt": (record or {}).get("nextRestartAt"),
            "conflicts": self.conflicts(),
            "projects": self.projects(),
        }

    def projects(self) -> dict:
        """Which projects this one service is carrying, read without opening a Store.

        Grouping only. The cross-delivery refusal is decided per assignment from its own
        endpoints; this exists so an operator can see that a single supervisor is serving
        several repositories rather than having to infer it.
        """
        from .registry import project_key
        from .store import read_only_rows

        answer = read_only_rows(
            self.selection,
            "SELECT relationship_id, issue_key, status, parent_task_id, parent_host_id,"
            "       parent_cwd"
            "  FROM relationships"
            " WHERE superseded_by IS NULL",
        )
        if not answer["readable"]:
            return {"available": False, "detail": answer["detail"], "projects": []}
        if answer["detail"]:
            # The file opened and the query did not. An inventory we could not read is not an
            # empty inventory, and reporting available with no projects says it is.
            return {"available": False, "detail": answer["detail"], "projects": []}
        grouped = {}
        for row in answer["rows"]:
            key = project_key({"parent": {"cwd": row["parent_cwd"],
                                          "hostId": row["parent_host_id"]}})
            entry = grouped.setdefault(
                key, {"project": key, "assignments": 0, "active": 0, "parents": set(),
                      "issues": set()},
            )
            entry["assignments"] += 1
            entry["active"] += 1 if row["status"] == "active" else 0
            entry["parents"].add(row["parent_task_id"])
            entry["issues"].add(row["issue_key"])
        projects = [
            {"project": e["project"], "assignments": e["assignments"], "active": e["active"],
             "parents": sorted(e["parents"]), "issues": sorted(e["issues"])}
            for e in sorted(grouped.values(), key=lambda e: e["project"])
        ]
        return {"available": True, "detail": answer["detail"], "projects": projects}

    def conflicts(self) -> list:
        if not self.socket_path:
            return []
        try:
            return self.scope.conflicts(
                self.socket_path, store_id=self.store_id, state_dir=self.selection.path,
            )
        except ScopeUnavailable:
            return []

    def stop(self, *, actor: str = "cli", timeout: float = 10.0, grace: float = 0.1) -> dict:
        """Signal only a process this installation owns, through a handle to that process."""
        record = self.record()
        owner, handle, detail = self.ownership(record)
        if owner == NONE and not (record or {}).get("workerPid"):
            # Nothing verifiable is running. Deciding that and writing a stop request are two
            # operations, and supervise() clears pending requests BEFORE it acquires the lock,
            # so a supervisor that starts between them consumes the request we leave and exits
            # - while this call reports not_running. Settle it while holding the lock instead.
            settled, record, owner, handle, detail = self._settle_absent_owner(record, detail)
            if settled is not None:
                if handle is not None:
                    handle.close()
                return settled
        try:
            if owner == FOREIGN:
                return {"ok": False, "reason": "not_ours", "detail": detail,
                        "supervisor": "untouched", "worker": "untouched"}
            if owner == UNVERIFIABLE:
                # Refusing is the point. Signalling on a pid match alone is how an unrelated
                # process gets killed after its number is reused.
                return {"ok": False, "reason": "ownership_unverifiable", "detail": detail,
                        "supervisor": "untouched", "worker": "untouched"}
            # Only now. Writing the stop request before validating ownership left a refused
            # stop able to halt another installation's supervisor at its next boundary, which
            # is a refusal that still had an effect.
            self.request_stop()
            # NONE still falls through to the worker: a supervisor can die leaving its worker
            # alive, and that orphan is exactly what a stop has to reach.
            outcome = ("gone" if owner == NONE
                       else self._terminate(handle, timeout=timeout, grace=grace))
        finally:
            if handle is not None:
                handle.close()
        # Re-read AFTER the supervisor is gone. A supervisor replacing a worker between our
        # first read and now means the pid we started with names a worker that has already
        # exited, and stopping that one would report success while the replacement, which
        # holds the inherited locks, is still delivering.
        #
        # But only when the re-read still describes the SAME launch. A start that acquires
        # the lock after the old supervisor exits publishes its own record here, and acting
        # on that would stop the new launch's worker and clear the new supervisor's pid using
        # the old one's outcome - reporting success while the replacement is still alive.
        fresh = self.record()
        # Identity, not just the launch id. A direct 'service run' carries no launch id at
        # all, so comparing that field alone made two anonymous launches look like one and
        # handed the replacement's worker and record straight back to this stop. The
        # supervisor's own pid and start time distinguish them whether or not a launch id
        # was ever assigned.
        superseded_by_a_new_launch = (
            fresh is not None and record is not None
            and self._launch_identity(fresh) != self._launch_identity(record)
        )
        if fresh is not None and not superseded_by_a_new_launch:
            record = fresh
        worker = self._stop_worker(record, timeout=timeout, grace=grace)
        if superseded_by_a_new_launch:
            # A replacement is running. Whatever we did to the launch we started from, the
            # SERVICE is not stopped, and reporting success from the old launch's outcome
            # would tell a caller the relay is down while it is still delivering.
            return {"ok": False, "reason": "replaced_by_new_launch",
                    "detail": "a new launch acquired the daemon lock during this stop; it was"
                              " left untouched and is still running",
                    "supervisor": outcome, "worker": "untouched"}
        supervisor_done = outcome in ("exited", "gone")
        worker_done = worker in ("exited", "gone")
        if record is not None:
            # Identity is kept until termination is CONFIRMED. Clearing a pid we have not
            # seen exit would lose the only handle a later stop has to reach it.
            cleared = dict(record, stoppedBy=actor)
            if supervisor_done:
                cleared["pid"] = None
            if worker_done:
                cleared["workerPid"] = None
                cleared["workerStartTicks"] = None
            if supervisor_done and worker_done:
                cleared["stoppedAt"] = _now()
            # Written UNDER the lock, or not at all. The re-read above is a snapshot, and the
            # daemon lock is free the moment the old supervisor and worker are gone - so a
            # replacement can acquire it after that read and publish its own record, and this
            # write would erase the identity of a launch that is running, leaving every later
            # status and stop with no handle on it. Holding the lock means no replacement can
            # start while we finalise; failing to take it means one already did.
            # One rule: this record is written only while we HOLD the lock. Three narrower
            # versions of this guard each left a window - a replacement that had published, one
            # that had not, an absent record - because each asked what the world looked like at
            # a moment rather than excluding change for the duration. Holding the lock is the
            # only thing that actually excludes a replacement, so that is the condition.
            with self.daemon_lock_if_free() as held:
                if held is None:
                    if supervisor_done and worker_done:
                        # Both processes this stop acted on are confirmed gone, so whatever
                        # holds the lock is neither of them.
                        return {"ok": False, "reason": "replaced_by_new_launch",
                                "detail": "the daemon lock was taken during this stop; that"
                                          " launch was left untouched and is still running",
                                "supervisor": outcome, "worker": worker}
                    # Otherwise the worker we could not confirm is the likeliest holder,
                    # through the descriptor it inherited. Nothing is written, and nothing
                    # needs to be: the record still names the processes a later stop must
                    # reach, which is exactly what this write would have preserved.
                elif (latest := self.record()) is not None and (
                    self._launch_identity(latest) != self._launch_identity(record)
                ):
                    return {"ok": False, "reason": "replaced_by_new_launch",
                            "detail": "a new launch published its record during this stop; it"
                                      " was left untouched and is still running",
                            "supervisor": outcome, "worker": worker}
                else:
                    self.write_record(cleared)
        if owner == NONE and worker == "gone":
            return {"ok": False, "reason": "not_running", "detail": detail,
                    "supervisor": "gone", "worker": "gone"}
        ok = supervisor_done and worker_done
        return {"ok": ok, "reason": None if ok else "did_not_exit",
                "detail": None, "supervisor": outcome, "worker": worker}

    @staticmethod
    def _launch_identity(record):
        """What distinguishes one supervisor's run from the next one's.

        The launch id alone is not enough: a direct 'service run' never has one, so two
        anonymous launches compare equal. The supervisor's pid and the moment it started
        differ across a replacement whether or not a launch id was assigned.

        Taken in order of stability rather than all at once. A supervisor finishing normally
        clears its own pid during cleanup while keeping the launch id and start time, so a
        tuple including the pid turned an ordinary exit into a phantom replacement and made
        a successful stop report failure.
        """
        record = record or {}
        if record.get("launchId") is not None:
            return ("launchId", record["launchId"])
        if record.get("startedAt") is not None:
            # With the start ticks, because _now() records whole seconds: two anonymous
            # launches inside the same second share a startedAt, and a replacement acquiring
            # the lock in that second would compare equal to the launch being stopped. The
            # kernel's start-time counter for that pid does not collide.
            return ("startedAt", record["startedAt"], record.get("startTicks"))
        return ("pid", record.get("pid"))

    def _settle_absent_owner(self, record, detail):
        """Decide 'nothing is running' while HOLDING the lock that would prove otherwise.

        Returns (response_or_None, record, owner, handle, detail). A response ends the stop;
        None means the caller should continue with the re-read record and classification.

        A record naming a WORKER never reaches here: that none is our own supervisor gone
        with its orphan holding the inherited lock, and reaching that orphan is what stop is
        for.
        """
        with self.daemon_lock_if_free() as held:
            if held is not None:
                # We hold it, so no supervisor is running and none can start while we decide.
                # Returning without a request is the whole point: a request left behind here
                # would be consumed by the next supervisor to start.
                return (
                    {"ok": False, "reason": "not_running", "detail": detail,
                     "supervisor": "gone", "worker": "gone"},
                    record, NONE, None, detail,
                )
        # Not free: something took it. Re-read - it may have published its identity by now.
        again = self.record()
        owner, handle, fresh = self.ownership(again)
        if owner == NONE and not (again or {}).get("workerPid"):
            if handle is not None:
                handle.close()
            return (
                {"ok": False, "reason": "ownership_unverifiable",
                 "detail": "the daemon lock is held by a process that has not yet"
                           " recorded its identity",
                 "supervisor": "untouched", "worker": "untouched"},
                again, owner, None, fresh,
            )
        return (None, again, owner, handle, fresh)

    def _terminate(self, handle, *, timeout, grace) -> str:
        if not handle.send(signal.SIGTERM):
            return "signal_refused"
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not handle.alive():
                return "exited"
            time.sleep(grace)
        handle.send(signal.SIGKILL)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not handle.alive():
                return "exited"
            time.sleep(grace)
        return "still_running"

    def _stop_worker(self, record, *, timeout, grace) -> str:
        """Reach the worker directly when the supervisor is already gone."""
        pid = (record or {}).get("workerPid")
        if not pid:
            return "gone"
        handle = ProcessHandle(pid)
        try:
            if handle.already_gone or not handle.usable:
                return "gone" if handle.already_gone else "unverifiable"
            ticks = (record or {}).get("workerStartTicks")
            current = start_ticks(pid)
            if ticks is None or current is None:
                # The same rule the supervisor gets. Without a recorded start time the worker
                # pid is just a number, and a crashed worker whose number was reused would
                # put SIGTERM into an unrelated process.
                return "unverifiable"
            if current != ticks:
                return "gone"
            return self._terminate(handle, timeout=timeout, grace=grace)
        finally:
            handle.close()

    def restart(self, *, allow_isolated: bool = False, launcher=None, **kw) -> dict:
        """Preserves the recorded intent. A disabled service is refused, never switched on."""
        intent = self.intent.read()
        if not intent["enabled"]:
            return {"ok": False, "reason": "service_disabled", "intent": intent,
                    "detail": "restart never enables a service the owner turned off"}
        # Decided BEFORE anything is stopped, and carried into the start below. Resolving it
        # again afterwards would let a declaration written in between refuse a launch whose
        # service this call had already taken down, which turns a question about two files
        # into an outage.
        launch_policy = self.resolve_launch_policy()
        refusal = launch_policy_refusal(launch_policy)
        if refusal is not None:
            return dict(refusal, intent=intent,
                        stop={"ok": False, "reason": "refused", "supervisor": "untouched",
                              "worker": "untouched"})
        stopped = self.stop(**{k: v for k, v in kw.items() if k in ("actor", "timeout")})
        if not stopped["ok"] and stopped["reason"] not in ("not_running",):
            return {"ok": False, "reason": stopped["reason"], "stop": stopped}
        # Re-read AFTER stopping: an owner who disabled the service while it was being
        # stopped must not get a replacement launched from the value we cached above.
        if not self.intent.read()["enabled"]:
            return {"ok": False, "reason": "service_disabled", "stop": stopped,
                    "intent": self.intent.read(),
                    "detail": "intent changed to disabled while the service was stopping"}
        started = self.start(allow_isolated=allow_isolated, launcher=launcher,
                             launch_policy=launch_policy, **kw)
        return {"ok": started["ok"], "reason": started["reason"], "stop": stopped,
                "start": started}

    def start(self, *, allow_isolated: bool = False, launcher=None, actor: str = "cli",
              max_ticks=None, deadline=None, segment_seconds=None, max_segments=None,
              timeout: float = 20.0, poll: float = 0.05, takeover: bool = False,
              launch_policy=None) -> dict:
        """Preconditions here; ownership in the child.

        The daemon lock and the scope claim are taken by the process that will HOLD them, not
        by this one: a parent that claimed and then exited would hand the child an unowned
        scope. So start checks what it can cheaply, launches, and then reports what the child
        actually managed to do.
        """
        gate = self.authority_check(allow_isolated=allow_isolated)
        if not gate["ok"]:
            return gate
        intent = self.intent.read()
        if not intent["enabled"]:
            return {"ok": False, "reason": "service_disabled", "intent": intent,
                    "detail": "start never enables a service; enable it explicitly first"}
        # One reading for the whole launch: this call's own when it was not handed one by a
        # restart that already decided.
        launch_policy = (self.resolve_launch_policy() if launch_policy is None
                         else launch_policy)
        refusal = launch_policy_refusal(launch_policy)
        if refusal is not None:
            return refusal
        if self.lock_is_held():
            return {"ok": False, "reason": "already_running", "status": self.status()}
        live = [conflict for conflict in self.conflicts() if conflict["live"]]
        if live:
            return {"ok": False, "reason": "scope_owned_by_other_store", "conflicts": live}
        # A registration naming a store that no longer exists - a database deleted, lost or
        # deliberately replaced - would otherwise block this socket forever, with recovery
        # only through finding and removing a registry file by hand. Taking it over is
        # explicit, and only ever from a registration nothing is running behind: the live
        # check above has already refused if anything holds the scope.
        self.takeover = bool(takeover)

        launch = uuid.uuid4().hex
        self.launch_id = launch
        self.launch_environment = launch_policy
        child = (launcher or self.default_launcher)(
            self, allow_isolated=allow_isolated, max_ticks=max_ticks, deadline=deadline,
            segment_seconds=segment_seconds, max_segments=max_segments,
        )
        deadline_at = time.monotonic() + timeout
        try:
            result = self._await_launch(child, launch, deadline_at, timeout=timeout, poll=poll)
        except BaseException:
            # Anything that leaves this call without a confirmed result - an exception, a
            # KeyboardInterrupt while recovery is still initialising - leaves a child the
            # default launcher put in its own session, so no terminal signal reaches it. It
            # would finish starting up, take both locks and begin serving a launch the caller
            # was never told had succeeded.
            self._abandon(child, timeout=timeout)
            raise
        # What this launch was decided under, reported whether it reported itself ready or
        # not: a launch that failed for another reason still says which policy it carried.
        result["launchPolicy"] = launch_policy
        return result

    def _await_launch(self, child, launch, deadline_at, *, timeout, poll):
        """Wait for the child to publish a record this call can recognise as its own launch."""
        while time.monotonic() < deadline_at:
            record = self.record() or {}
            # Matched on the launch id, not on the pid changing. After a crash the OS can
            # hand the replacement the very pid the stale record already names, and waiting
            # for a different number would time out on a service that is running fine.
            # The pid must still be there: supervision clears it on the way out while the
            # daemon lock is not yet released, so a launch that already finished spends a
            # moment matching the id and holding the lock with nothing left running.
            # readyAt is written only after on_start returns, so recovery that is still
            # running - or about to fail on an App Server connection - is not success either.
            if (record.get("pid") and record.get("readyAt")
                    and record.get("launchId") == launch and self.lock_is_held()):
                # The child minted or opened the store; this process only probed a path that
                # may not have existed yet. Without adopting its identity, conflicts() cannot
                # recognise the child's own scope registration as this store and reports the
                # service we just started as same_scope_different_store.
                if self.store_id is None and record.get("storeId"):
                    self.store_id = record["storeId"]
                return {"ok": True, "reason": None, "pid": record["pid"],
                        "scopeAuthority": self.scope.authority,
                        "scopeRoot": str(self.scope.root), "status": self.status()}
            if getattr(child, "poll", lambda: None)() is not None:
                return {"ok": False, "reason": "child_exited",
                        "exitCode": child.returncode, "log": self._log_tail()}
            time.sleep(poll)
        # A launch that never reported is not a launch that can be left alone. It may still
        # be initialising and would come up AFTER the caller was told it failed, holding the
        # locks against the retry the caller is about to make.
        return {"ok": False, "reason": "did_not_report", "log": self._log_tail(),
                "child": self._abandon(child, timeout=timeout)}

    def _abandon(self, child, *, timeout: float) -> str:
        """Stop a child this call started and could not confirm, and reap it."""
        if getattr(child, "poll", lambda: None)() is not None:
            return "exited"
        for step in (getattr(child, "terminate", None), getattr(child, "kill", None)):
            if step is None:
                continue
            try:
                step()
                child.wait(timeout=max(1.0, timeout / 2))
                return "terminated"
            except Exception:  # noqa: BLE001 - a child we cannot reach is reported, not raised
                continue
        return "still_running"

    def _log_tail(self, lines: int = 20) -> str:
        try:
            return "".join(
                (self.selection.path / DAEMON_LOG).read_text(encoding="utf-8").splitlines(True)
                [-lines:]
            )
        except OSError:
            return ""

    def default_launcher(self, service, *, allow_isolated, max_ticks=None, deadline=None,
                         segment_seconds=None, max_segments=None):
        import subprocess
        import sys

        argv = [sys.executable, "-m", "codex_session_relay.cli",
                "--state", str(self.selection.path)]
        if self.socket_path:
            argv += ["--socket", str(self.socket_path)]
        argv += ["service", "run"]
        if allow_isolated:
            argv.append("--allow-isolated-scope")
        if segment_seconds is not None:
            argv += ["--segment-seconds", str(segment_seconds)]
        if max_segments is not None:
            argv += ["--max-segments", str(max_segments)]
        if deadline is not None:
            # Handed down as the INSTANT this launch must stop at, not as the duration this
            # caller was given. The supervisor being launched reads its own clock only after
            # fork, interpreter start, imports and its own gate checks, so a duration would
            # begin counting there and the bound would end that much later than asked for.
            # Same host, same boot, and the child is exec'd now, so CLOCK_MONOTONIC is one
            # clock at both ends; repr keeps the float exact across the argument.
            argv += ["--deadline-monotonic", repr(time.monotonic() + deadline)]
        environment = dict(os.environ)
        if self.scope.authority == ISOLATED:
            # The ALREADY RESOLVED absolute root, so a relative override cannot resolve
            # differently in the child, and the child cannot land in another registry.
            environment[SCOPE_ENV] = str(self.scope.root)
        # The transport ledger resolves from the environment, not from --state, so a managed
        # launch forwarding only the flag would inherit the store/ledger split.
        environment["CODEX_SESSION_RELAY_STATE"] = str(self.selection.path)
        # The execution policy this SERVICE is declared with, applied rather than inherited.
        # The daemon reads it once from its own environment at startup, so a launch that only
        # passed on whatever the calling shell carried made the policy a property of whoever
        # typed the command: the same restart, typed in two terminals, produced a service
        # enforcing roles and a service withholding every role-bound delivery. start() froze
        # this reading before anything was stopped and it is applied here unchanged.
        policy = self.launch_environment or self.resolve_launch_policy()
        if launch_policy_refusal(policy) is not None:
            # Unreachable through start(), which refuses first and reports it as a payload. A
            # caller reaching the launcher directly gets the same answer rather than a guess
            # between two policy files.
            raise ValueError(policy["detail"])
        if policy["path"]:
            environment[policy["variable"]] = policy["path"]
        if self.launch_id:
            argv += ["--launch-id", self.launch_id]
            # And this launch's decision is settled: the daemon adopts the environment it was
            # given instead of reading the declaration again on the way up. Without it a
            # declaration written between this launch and that read turns an environment this
            # service already approved into a conflict, refused by the child after the restart
            # had stopped the old daemon - the outage the frozen resolution exists to prevent,
            # one process boundary further out.
            #
            # Bound to THIS launch's id, so it says nothing about any other one, and it is not
            # a defence against the user who owns both processes: it cannot be, since that user
            # can rewrite the declaration itself. It is the launch saying what it decided.
            environment[LAUNCH_SETTLED_ENV] = self.launch_id
        if self.takeover:
            # The supervisor is the process that CLAIMS the scope, so the flag has to reach
            # it. Set only on this object, it was dropped at the process boundary and the
            # takeover silently did nothing.
            argv.append("--takeover-scope")
        self.selection.path.mkdir(mode=0o700, parents=True, exist_ok=True)
        with open(self.selection.path / DAEMON_LOG, "a", encoding="utf-8") as log:
            return subprocess.Popen(
                argv, stdout=log, stderr=log, stdin=subprocess.DEVNULL,
                start_new_session=True, env=environment,
            )

    # ------------------------------------------------------------- supervision

    @property
    def stop_request_path(self) -> Path:
        return self.selection.path / STOP_REQUEST

    def request_stop(self) -> None:
        """Recorded BEFORE any signal, so a graceful stop is never read as a crash."""
        self.selection.path.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.stop_request_path.write_text(_now(), encoding="utf-8")

    def clear_stop_request(self) -> None:
        try:
            self.stop_request_path.unlink()
        except OSError:
            pass

    def stop_requested(self) -> bool:
        return self.stop_request_path.exists()

    def spawn_worker(self, *, lock_fd, scope_fd, token, segment_seconds, allow_isolated,
                     deadline_monotonic=None):
        """One bounded worker, sharing the descriptors this supervisor already holds.

        The worker's bound goes down in whichever form is true of it. With a supervisor bound
        to protect, `deadline_monotonic` is the INSTANT this worker must stop at, so what it
        spends starting up comes out of the segment instead of landing after it. With no
        supervisor bound there is nothing to protect and `segment_seconds` is a segment
        LENGTH, which is what a duration means.
        """
        import subprocess
        import sys

        argv = [sys.executable, "-m", "codex_session_relay.cli",
                "--state", str(self.selection.path)]
        if self.socket_path:
            argv += ["--socket", str(self.socket_path)]
        argv += ["daemon"]
        if deadline_monotonic is None:
            argv += ["--deadline", str(segment_seconds)]
        else:
            argv += ["--deadline-monotonic", repr(deadline_monotonic)]
        argv += ["--supervised-token", token,
                 "--supervised-lock-fd", str(lock_fd),
                 "--supervised-scope-fd", str(scope_fd if scope_fd is not None else -1)]
        if allow_isolated:
            argv.append("--allow-isolated-scope")
        environment = dict(os.environ)
        if self.scope.authority == ISOLATED:
            environment[SCOPE_ENV] = str(self.scope.root)
        environment["CODEX_SESSION_RELAY_STATE"] = str(self.selection.path)
        pass_fds = tuple(fd for fd in (lock_fd, scope_fd) if fd is not None)
        with open(self.selection.path / DAEMON_LOG, "a", encoding="utf-8") as log:
            return subprocess.Popen(
                argv, stdout=log, stderr=log, stdin=subprocess.DEVNULL,
                pass_fds=pass_fds, env=environment,
            )

    def supervise(self, *, allow_isolated=False, segment_seconds=None, max_segments=None,
                  deadline=None, deadline_monotonic=None, spawn=None, policy=None,
                  sleeper=None, on_start=None, monotonic=None) -> dict:
        """Replace bounded workers for as long as the owner wants this service running.

        RelayDaemon.run stays bounded by construction; continuation is a supervisor OVER
        successive bounded workers, not a longer loop inside one. The locks are acquired once
        here and inherited by every worker, so the store, the generations and the scope claim
        are untouched across a worker boundary - which is what carries an assignment past any
        single process lifetime without asking the parent model anything.

        `monotonic` is the clock this loop measures its bound with, defaulting to the real
        one. It is a parameter because the behaviour that matters here only appears after
        hours of it: an assignment outliving the host's own limit crosses many worker
        boundaries, and waiting for that in real time is not a test anyone runs. A scripted
        clock advanced by the injected `sleeper` and by the workers themselves reproduces the
        crossing deterministically. What that proves is the loop's own arithmetic and its
        intent re-reads; what it does not prove is anything about hours of real elapsed time.
        The distinction is written out in tests/test_service.py rather than implied.
        An injected clock has to be paired with an injected `spawn`: the instant this loop
        hands a worker is expressed in whatever clock it was given, and a real worker reads it
        against the real one.

        The bound arrives in one of two forms and they are not interchangeable. `deadline` is
        a DURATION, and it starts counting at this process. `deadline_monotonic` is the
        INSTANT a launching process already decided, read from the same CLOCK_MONOTONIC on the
        same host and boot; converting it here spends this process's own startup out of the
        bound instead of adding it on top. Passing both is a caller bug, not a preference.
        """
        from .daemon import SingleInstance
        from .policy import RetryPolicy

        if deadline is not None and deadline_monotonic is not None:
            raise ValueError(
                "deadline and deadline_monotonic are two different bounds; pass one. A "
                "duration starts at this process's clock; an instant was decided before it "
                "existed, and silently preferring either would end the run at a time the "
                "caller did not ask for"
            )
        for name, value in (("deadline", deadline), ("deadline_monotonic", deadline_monotonic)):
            # Checked here and not only at the CLI, because every comparison against nan is
            # False: a supervisor given one would never reach its bound, which is the
            # unbounded run this loop is built not to have. inf says the same thing plainly.
            if value is not None and not math.isfinite(value):
                raise ValueError(f"{name} must be a finite number of seconds")
            # Neither form can be negative: a duration is a length rather than a direction, and
            # CLOCK_MONOTONIC counts from a point at or before this boot, so an instant on this
            # host is never below zero.
            if value is not None and value < 0:
                raise ValueError(f"{name} cannot be negative")
        policy = policy or RetryPolicy()
        sleeper = sleeper or time.sleep
        monotonic = monotonic or time.monotonic
        spawn = spawn or self.spawn_worker
        # The default belongs to an absent value, not to a falsy one: zero is a segment length
        # somebody asked for, and silently turning it into an hour answers a different request.
        segment_seconds = policy.segment_seconds if segment_seconds is None else segment_seconds
        if not math.isfinite(segment_seconds) or segment_seconds <= 0:
            # This becomes every worker's OWN bound. A worker handed one no comparison can pass
            # is refused on arrival, and a supervisor reads that refusal as an ordinary worker
            # failure - so it would replace the worker, and replace the replacement, for as
            # long as the owner left the service running, alive and serving nothing. Refused
            # here, before any lock is taken or any worker is spawned.
            raise ValueError(
                "segment_seconds must be a finite number greater than zero; it is the bound "
                "every worker is given, and a worker cannot be given a length it must refuse"
            )

        gate = self.authority_check(allow_isolated=allow_isolated)
        if not gate["ok"]:
            raise ServiceRefused(gate["reason"], gate["detail"])
        if not self.intent.read()["enabled"]:
            raise ServiceRefused(
                "service_disabled",
                "this service is not enabled; supervising it would ignore the owner's intent",
            )

        self.clear_stop_request()
        token = uuid.uuid4().hex
        segments, failures, degraded = [], 0, None
        outstanding = None
        with SingleInstance(self.selection.path, shared=True) as lock:
            # Re-read UNDER the lock. The check above ran while nothing was held, so a disable
            # landing between them is missed: this supervisor starts, and the disabling caller
            # - which classified the PREVIOUS holder and wrote outside the lock - has written
            # enabled=false at a supervisor it never examined, which this one then obeys at its
            # first worker boundary. Whoever holds this lock is the one whose intent decides.
            if not self.intent.read()["enabled"]:
                raise ServiceRefused(
                    "service_disabled",
                    "this service was disabled while this supervisor was starting",
                )
            scope_fd = None
            if self.socket_path:
                claim = self.scope.claim(
                    self.socket_path, self.new_record(pid=os.getpid(), token=token),
                    shared=True,
                )
                if not claim["ok"]:
                    raise ServiceRefused(claim["reason"], json.dumps(claim.get("held_by")))
                scope_fd = claim["lockFd"]
            self.write_record(self.new_record(pid=os.getpid(), token=token))
            try:
                # Started BEFORE on_start, so the bound covers the whole supervisor run.
                # Recovery can make real App Server calls for unresolved attempts, and timing
                # only the loop let --deadline N spend an arbitrary startup interval first and
                # then run for another N - even spawning a worker past the requested bound.
                started = monotonic()
                if deadline_monotonic is not None:
                    # The last moment before the loop begins, so everything this process spent
                    # reaching it - fork, interpreter start, imports, the gate checks and the
                    # claim above - comes OUT of the bound rather than being added to it. The
                    # instant means this only for a child its launcher just exec'd on the same
                    # host and boot: CLOCK_MONOTONIC restarts near zero, so a value carried
                    # into a later boot sits in that boot's FUTURE and would name a bound much
                    # later than anyone asked for. Nothing stores it, which is why no boot
                    # identity is checked here.
                    deadline = max(0.0, deadline_monotonic - started)
                # This supervisor's own end, in its own clock. Every worker's instant is capped
                # at it below, so the 0.1 floor on a segment cannot hand a worker a bound that
                # outlives the one being enforced.
                ends_at = None if deadline is None else started + deadline
                # Checked BEFORE recovery, not only after it. on_start makes real App Server
                # calls for every unresolved attempt, and a bound that was already gone when
                # this process first read a clock buys that work no segment to be useful in.
                # The check after on_start stays as well: recovery can spend a bound that was
                # there when it began.
                spent = deadline is not None and deadline <= 0
                if on_start is not None and not spent:
                    on_start()
                # Only now is this service serving. Recovery runs before any worker can send,
                # and a caller told "started" while it was still in flight would go on to use
                # a service that might yet fail to initialise at all.
                #
                # And not at all if the bound was spent getting ready: the loop below exits
                # immediately in that case, so publishing readiness here would let a waiting
                # start() report a service that is already on its way out.
                expired = deadline is not None and monotonic() - started >= deadline
                if not expired:
                    self._note(readyAt=_now())
                while True:
                    if max_segments is not None and len(segments) >= max_segments:
                        break
                    if deadline is not None and monotonic() - started >= deadline:
                        break
                    if self.stop_requested():
                        break
                    # Re-read every cycle: an owner who disables the service while a worker
                    # was running gets no replacement, and the supervisor never writes intent.
                    if not self.intent.read()["enabled"]:
                        break
                    # ONE reading of the clock, used for both the length and the instant. Two
                    # readings are two different nows: the second is later by however long the
                    # arithmetic and the attribute lookups between them took, so the instant
                    # would express a segment measured from a moment that had already passed.
                    # The gap is small and the cap below hides it at the end of a bound, which
                    # is exactly why it would have gone unnoticed - and it is the same mistake
                    # this whole change is about, one scale down.
                    now = monotonic()
                    granted = (
                        segment_seconds if deadline is None else
                        max(0.1, min(segment_seconds, deadline - (now - started)))
                    )
                    child = spawn(
                        lock_fd=lock.fileno(), scope_fd=scope_fd, token=token,
                        # Clamped to what remains of the supervisor's own bound, or a worker
                        # started just before the deadline outlives it by a whole segment.
                        segment_seconds=granted,
                        # The same clamp as an INSTANT, so the startup the worker has not
                        # spent yet comes out of this segment instead of landing after it -
                        # and capped at this supervisor's own end, because the floor above
                        # rounds a remainder under a tenth of a second back up to one.
                        deadline_monotonic=(
                            None if ends_at is None
                            else min(now + granted, ends_at)
                        ),
                        allow_isolated=allow_isolated,
                    )
                    # Marked outstanding BEFORE the bookkeeping that can fail. A _note that
                    # raises after a successful spawn left a running worker the cleanup then
                    # treated as never started, clearing the identity a stop needs while the
                    # worker still holds the inherited locks.
                    outstanding = child
                    self._note(workerPid=child.pid, workerStartTicks=start_ticks(child.pid))
                    code = child.wait()
                    outstanding = None
                    self._note(workerPid=None, workerStartTicks=None, lastExit=code,
                               restarts=len(segments) + 1)
                    segments.append(code)
                    if code == EXIT_BOUND_SPENT:
                        # This worker reached its own clock after the instant it was given, so
                        # it took no tick and served nothing. At the end of a bounded run that
                        # is the ordinary tail and the checks below would end the loop anyway.
                        # Anywhere else it means a worker costs more to start than the segment
                        # it was granted, and replacing it would churn processes that never
                        # serve while reporting clean segments - so it is named and this
                        # supervisor stops instead of spinning.
                        if not (deadline is not None and monotonic() - started >= deadline):
                            degraded = ("a worker costs more to start than the segment it was"
                                        f" granted ({granted:.3f}s); it served nothing")
                            self.store_journal_note(degraded)
                            self._note(consecutiveFailures=failures, degraded=degraded)
                        break
                    failures = failures + 1 if code != 0 else 0
                    if failures >= policy.repeat_failure_threshold:
                        degraded = (f"{failures} consecutive worker failures, last exit {code}")
                        self.store_journal_note(degraded)
                    self._note(consecutiveFailures=failures, degraded=degraded)
                    if self.stop_requested() or not self.intent.read()["enabled"]:
                        break
                    if max_segments is not None and len(segments) >= max_segments:
                        # The top of the loop stops for this too, but only AFTER the restart
                        # delay below - so a finite run outran its own bound by up to the
                        # backoff cap, which after repeated failures is five minutes, before
                        # returning. There is nothing left to wait for.
                        break
                    if deadline is not None and monotonic() - started >= deadline:
                        break
                    wait = policy.restart_delay_for(failures)
                    if deadline is not None:
                        # Clamped the same way a worker segment is. An unclamped delay - up to
                        # five minutes after repeated failures - outlives the supervisor's own
                        # bound, so a short deadline took minutes to return.
                        wait = max(0.0, min(wait, deadline - (monotonic() - started)))
                    self._note(nextRestartAt=time.time() + wait)
                    sleeper(wait)
            finally:
                if self.socket_path:
                    # shared=True: this descriptor is the one every worker inherited, and
                    # LOCK_UN through it would release the scope for all of them. After an
                    # exception a worker may still be alive, and unlocking would let another
                    # state directory claim this socket beside the orphan.
                    self.scope.release(self.socket_path, shared=True)
                current = self.record() or {}
                # nextRestartAt with it: it names a restart this supervisor is no longer going
                # to make, and leaving it behind let a stopped service report a pending one.
                cleared = dict(current, pid=None, stoppedAt=_now(), nextRestartAt=None)
                if outstanding is None:
                    cleared["workerPid"] = None
                    cleared["workerStartTicks"] = None
                # Otherwise the worker was never waited on - an exception between spawn and
                # wait - and it still holds the inherited locks. Erasing its pid and start
                # time would leave a stop with nothing to aim at, so the orphan would keep
                # delivering for the rest of its segment while status reported not_running.
                self.write_record(cleared)
        return {"ok": True, "reason": None, "segments": segments,
                "consecutiveFailures": failures, "degraded": degraded}

    def _note(self, **fields) -> None:
        record = self.record()
        if record is not None:
            self.write_record(dict(record, **fields))

    def store_journal_note(self, detail: str) -> None:
        """Repeated failure is reported, not silently retried forever."""
        path = self.selection.path / DAEMON_LOG
        try:
            with open(path, "a", encoding="utf-8") as log:
                log.write(f"{_now()} service_degraded: {detail}\n")
        except OSError:
            pass


@contextmanager
def owned_service(service: RelayService, *, allow_isolated: bool = False, token=None,
                  require_intent: bool = True, adopt_lock_fd=None, adopt_scope_fd=None):
    """Hold the daemon lock and the scope claim for as long as this process serves.

    Both are released together on the way out, and the scope RECORD is deliberately kept: a
    stopped registration on a different store is still a duplicate registration, and a scan
    that only looked at live records could not see it.
    """
    from .daemon import SingleInstance

    gate = service.authority_check(allow_isolated=allow_isolated)
    if not gate["ok"]:
        raise ServiceRefused(gate["reason"], gate["detail"])
    if require_intent and not service.intent.read()["enabled"]:
        raise ServiceRefused(
            "service_disabled",
            "this service is not enabled; running it would ignore the owner's intent",
        )
    supervised = adopt_lock_fd is not None
    if not supervised and token is None:
        # Foreground runs need the same per-run identity as supervised workers.
        token = uuid.uuid4().hex
    with SingleInstance(service.selection.path, adopt_fd=adopt_lock_fd):
        claim = {"ok": True, "reason": None}
        if service.socket_path and not supervised:
            claim = service.scope.claim(service.socket_path, service.new_record(
                pid=os.getpid(), token=token,
            ))
            if not claim["ok"]:
                raise ServiceRefused(claim["reason"], json.dumps(claim.get("held_by")))
        if supervised:
            # The supervisor owns both the claim and the record. A worker that rewrote them
            # would erase the identity a stop needs to reach the supervisor.
            record = service.record() or service.new_record(pid=os.getpid(), token=token)
        else:
            record = service.write_record(service.new_record(pid=os.getpid(), token=token))
        try:
            yield record
        finally:
            if not supervised:
                if service.socket_path:
                    service.scope.release(service.socket_path)
                current = service.record() or record
                service.write_record(
                    dict(current, pid=None, workerPid=None, stoppedAt=_now()),
                )
            elif adopt_scope_fd is not None and adopt_scope_fd >= 0:
                # Close our copy only. The supervisor still holds the description, so the
                # scope stays claimed; unlocking would release it for both of us.
                try:
                    os.close(adopt_scope_fd)
                except OSError:
                    pass


class ServiceRefused(Exception):
    def __init__(self, reason, detail=None):
        super().__init__(detail or reason)
        self.reason = reason
        self.detail = detail


def _existing_lock_held(path) -> bool:
    """Only contention is evidence of ownership; do not create a missing lock file."""
    with open(path, "rb") as handle:
        before = os.fstat(handle.fileno())
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as error:
            if error.errno not in (errno.EACCES, errno.EAGAIN):
                raise
            after = os.stat(path)
            return (before.st_dev, before.st_ino) == (after.st_dev, after.st_ino)
        else:
            fcntl.flock(handle, fcntl.LOCK_UN)
            return False
