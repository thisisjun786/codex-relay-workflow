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
import os
import pwd
import signal
import time
import uuid
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path

SCOPE_ENV = "CODEX_SESSION_RELAY_SCOPE_DIR"
PRODUCTION, ISOLATED = "production", "isolated"

DAEMON_LOCK = "daemon.lock"
DAEMON_RECORD = "daemon.json"
SERVICE_INTENT = "service.json"
DAEMON_LOG = "daemon.log"
STOP_REQUEST = "stop.request"

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
        canonical = str(Path(socket_path).expanduser().absolute())
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
        self.record_path(socket_path).write_text(json.dumps(payload, indent=2), encoding="utf-8")
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
        self.path.write_text(json.dumps(payload, indent=2), encoding="utf-8")
        return dict(payload, configured=True)


class RelayService:
    """Start, stop and describe the daemon that belongs to THIS installation."""

    def __init__(self, selection, *, socket_path=None, scope=None, store_id=None):
        self.selection = selection
        self.socket_path = socket_path
        self.store_id = store_id
        if scope is None:
            root, authority = resolve_scope_root()
            scope = ScopeRegistry(root, authority)
        self.scope = scope
        self.intent = ServiceIntent(selection.path / SERVICE_INTENT)
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
        self.record_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")
        return payload

    def new_record(self, *, pid, token=None, worker_pid=None) -> dict:
        return {
            "pid": pid, "startTicks": start_ticks(pid), "bootId": boot_id(),
            "workerPid": worker_pid, "token": token,
            "storeId": self.store_id, "installationId": self.installation_id,
            "stateDir": str(self.selection.path), "socketPath": self.socket_path,
            "scopeAuthority": self.scope.authority, "scopeRoot": str(self.scope.root),
            "startedAt": _now(), "restarts": 0, "consecutiveFailures": 0, "lastExit": None,
            "nextRestartAt": None,
        }

    def ownership(self, record=None):
        """ours | foreign | unverifiable | none, decided through a real process handle."""
        record = self.record() if record is None else record
        if not record or not record.get("pid"):
            return NONE, None, "no daemon record"
        handle = ProcessHandle(record["pid"])
        if handle.already_gone:
            return NONE, handle, "the recorded process is gone"
        if not handle.usable:
            return UNVERIFIABLE, handle, handle.detail
        mismatches = []
        if record.get("bootId") not in (None, boot_id()):
            mismatches.append("recorded before a different boot")
        ticks = start_ticks(record["pid"])
        if record.get("startTicks") is not None and ticks != record["startTicks"]:
            mismatches.append("the pid was reused by a different process")
        if record.get("installationId") != self.installation_id:
            mismatches.append("another installation owns it")
        if self.store_id is not None and record.get("storeId") not in (None, self.store_id):
            mismatches.append("it is using a different store")
        if mismatches:
            return FOREIGN, handle, "; ".join(mismatches)
        return OURS, handle, None

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

    def enable(self, *, actor: str = "cli") -> dict:
        return {"ok": True, "reason": None,
                "intent": self.intent.write(enabled=True, actor=actor)}

    def disable(self, *, actor: str = "cli", timeout: float = 10.0) -> dict:
        written = self.intent.write(enabled=False, actor=actor)
        stopped = self.stop(actor=actor, timeout=timeout)
        # A stop that had nothing to stop is not a failure; the intent is what disable owns.
        failed = not stopped["ok"] and stopped["reason"] != "not_running"
        return {"ok": not failed, "reason": stopped["reason"] if failed else None,
                "intent": written, "stop": stopped}

    def status(self) -> dict:
        record = self.record()
        owner, handle, detail = self.ownership(record)
        if handle is not None:
            handle.close()
        intent = self.intent.read()
        held = self.lock_is_held()
        return {
            "enabled": intent["enabled"], "intentConfigured": intent["configured"],
            "intentChangedAt": intent["changedAt"], "intentChangedBy": intent["changedBy"],
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
        }

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
        # Recorded before any signal, so a supervisor that is about to die knows this was a
        # graceful stop and does not launch a replacement on its way out.
        self.request_stop()
        owner, handle, detail = self.ownership(record)
        try:
            if owner == FOREIGN:
                return {"ok": False, "reason": "not_ours", "detail": detail,
                        "supervisor": "untouched", "worker": "untouched"}
            if owner == UNVERIFIABLE:
                # Refusing is the point. Signalling on a pid match alone is how an unrelated
                # process gets killed after its number is reused.
                return {"ok": False, "reason": "ownership_unverifiable", "detail": detail,
                        "supervisor": "untouched", "worker": "untouched"}
            # NONE still falls through to the worker: a supervisor can die leaving its worker
            # alive, and that orphan is exactly what a stop has to reach.
            outcome = ("gone" if owner == NONE
                       else self._terminate(handle, timeout=timeout, grace=grace))
        finally:
            if handle is not None:
                handle.close()
        worker = self._stop_worker(record, timeout=timeout, grace=grace)
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
            self.write_record(cleared)
        if owner == NONE and worker == "gone":
            return {"ok": False, "reason": "not_running", "detail": detail,
                    "supervisor": "gone", "worker": "gone"}
        ok = supervisor_done and worker_done
        return {"ok": ok, "reason": None if ok else "did_not_exit",
                "detail": None, "supervisor": outcome, "worker": worker}

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
            if ticks is not None and start_ticks(pid) != ticks:
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
        stopped = self.stop(**{k: v for k, v in kw.items() if k in ("actor", "timeout")})
        if not stopped["ok"] and stopped["reason"] not in ("not_running",):
            return {"ok": False, "reason": stopped["reason"], "stop": stopped}
        # Re-read AFTER stopping: an owner who disabled the service while it was being
        # stopped must not get a replacement launched from the value we cached above.
        if not self.intent.read()["enabled"]:
            return {"ok": False, "reason": "service_disabled", "stop": stopped,
                    "intent": self.intent.read(),
                    "detail": "intent changed to disabled while the service was stopping"}
        started = self.start(allow_isolated=allow_isolated, launcher=launcher, **kw)
        return {"ok": started["ok"], "reason": started["reason"], "stop": stopped,
                "start": started}

    def start(self, *, allow_isolated: bool = False, launcher=None, actor: str = "cli",
              max_ticks=None, deadline=None, segment_seconds=None, max_segments=None,
              timeout: float = 20.0, poll: float = 0.05) -> dict:
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
        if self.lock_is_held():
            return {"ok": False, "reason": "already_running", "status": self.status()}
        live = [conflict for conflict in self.conflicts() if conflict["live"]]
        if live:
            return {"ok": False, "reason": "scope_owned_by_other_store", "conflicts": live}

        before = (self.record() or {}).get("pid")
        child = (launcher or self.default_launcher)(
            self, allow_isolated=allow_isolated, max_ticks=max_ticks, deadline=deadline,
            segment_seconds=segment_seconds, max_segments=max_segments,
        )
        deadline_at = time.monotonic() + timeout
        while time.monotonic() < deadline_at:
            record = self.record() or {}
            if record.get("pid") and record.get("pid") != before and self.lock_is_held():
                return {"ok": True, "reason": None, "pid": record["pid"],
                        "scopeAuthority": self.scope.authority,
                        "scopeRoot": str(self.scope.root), "status": self.status()}
            if getattr(child, "poll", lambda: None)() is not None:
                return {"ok": False, "reason": "child_exited",
                        "exitCode": child.returncode, "log": self._log_tail()}
            time.sleep(poll)
        return {"ok": False, "reason": "did_not_report", "log": self._log_tail()}

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
            argv += ["--deadline", str(deadline)]
        environment = dict(os.environ)
        if self.scope.authority == ISOLATED:
            # The ALREADY RESOLVED absolute root, so a relative override cannot resolve
            # differently in the child, and the child cannot land in another registry.
            environment[SCOPE_ENV] = str(self.scope.root)
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

    def spawn_worker(self, *, lock_fd, scope_fd, token, segment_seconds, allow_isolated):
        """One bounded worker, sharing the descriptors this supervisor already holds."""
        import subprocess
        import sys

        argv = [sys.executable, "-m", "codex_session_relay.cli",
                "--state", str(self.selection.path)]
        if self.socket_path:
            argv += ["--socket", str(self.socket_path)]
        argv += ["daemon", "--deadline", str(segment_seconds),
                 "--supervised-token", token,
                 "--supervised-lock-fd", str(lock_fd),
                 "--supervised-scope-fd", str(scope_fd if scope_fd is not None else -1)]
        if allow_isolated:
            argv.append("--allow-isolated-scope")
        environment = dict(os.environ)
        if self.scope.authority == ISOLATED:
            environment[SCOPE_ENV] = str(self.scope.root)
        pass_fds = tuple(fd for fd in (lock_fd, scope_fd) if fd is not None)
        with open(self.selection.path / DAEMON_LOG, "a", encoding="utf-8") as log:
            return subprocess.Popen(
                argv, stdout=log, stderr=log, stdin=subprocess.DEVNULL,
                pass_fds=pass_fds, env=environment,
            )

    def supervise(self, *, allow_isolated=False, segment_seconds=None, max_segments=None,
                  deadline=None, spawn=None, policy=None, sleeper=None, on_start=None) -> dict:
        """Replace bounded workers for as long as the owner wants this service running.

        RelayDaemon.run stays bounded by construction; continuation is a supervisor OVER
        successive bounded workers, not a longer loop inside one. The locks are acquired once
        here and inherited by every worker, so the store, the generations and the scope claim
        are untouched across a worker boundary - which is what carries an assignment past any
        single process lifetime without asking the parent model anything.
        """
        from .daemon import SingleInstance
        from .policy import RetryPolicy

        policy = policy or RetryPolicy()
        sleeper = sleeper or time.sleep
        spawn = spawn or self.spawn_worker
        segment_seconds = segment_seconds or policy.segment_seconds

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
        with SingleInstance(self.selection.path, shared=True) as lock:
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
                if on_start is not None:
                    on_start()
                started = time.monotonic()
                while True:
                    if max_segments is not None and len(segments) >= max_segments:
                        break
                    if deadline is not None and time.monotonic() - started >= deadline:
                        break
                    if self.stop_requested():
                        break
                    # Re-read every cycle: an owner who disables the service while a worker
                    # was running gets no replacement, and the supervisor never writes intent.
                    if not self.intent.read()["enabled"]:
                        break
                    child = spawn(
                        lock_fd=lock.fileno(), scope_fd=scope_fd, token=token,
                        segment_seconds=segment_seconds, allow_isolated=allow_isolated,
                    )
                    self._note(workerPid=child.pid, workerStartTicks=start_ticks(child.pid))
                    code = child.wait()
                    self._note(workerPid=None, workerStartTicks=None, lastExit=code,
                               restarts=len(segments) + 1)
                    segments.append(code)
                    failures = failures + 1 if code != 0 else 0
                    if failures >= policy.repeat_failure_threshold:
                        degraded = (f"{failures} consecutive worker failures, last exit {code}")
                        self.store_journal_note(degraded)
                    self._note(consecutiveFailures=failures, degraded=degraded)
                    if self.stop_requested() or not self.intent.read()["enabled"]:
                        break
                    wait = policy.restart_delay_for(failures)
                    self._note(nextRestartAt=time.time() + wait)
                    sleeper(wait)
            finally:
                if self.socket_path:
                    self.scope.release(self.socket_path)
                current = self.record() or {}
                self.write_record(dict(current, pid=None, workerPid=None, stoppedAt=_now()))
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
