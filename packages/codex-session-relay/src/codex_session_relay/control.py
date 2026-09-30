"""The retained Python owner's bounded guard-evaluate control socket."""

import json
import os
import select
import selectors
import socket
import stat
import threading
import time
import types
from datetime import datetime, timezone
from pathlib import Path

from . import guard, marker
from .stopadapter import peer_uid  # the Stop client's own peer-credential reader

# Decision 24: the socket frame's transport limit, the same for both runtimes.
MAX_FRAME_BYTES = 64 << 20

# The time a peer has to send its whole request line, from the moment it is served: a bound on
# the line, not on each read, so a peer that trickles its line is answered when it is spent, as
# the Go owner (hook.HandleControl) answers it.
READ_TIMEOUT = 5


def _read_request(connection, read_by):
    """The request line as readline(MAX_FRAME_BYTES + 1) returns it, all of it by read_by.

    Up to and including the first line end, or MAX_FRAME_BYTES + 1 bytes without one, or what
    arrived before the peer closed its side; each read is given only the time that remains.
    """
    raw = bytearray()
    while True:
        remaining = read_by - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("timed out")
        connection.settimeout(remaining)
        chunk = connection.recv(min(65536, MAX_FRAME_BYTES + 1 - len(raw)))
        if not chunk:
            return bytes(raw)
        end = chunk.find(b"\n")
        if end >= 0:
            return bytes(raw + chunk[:end + 1])
        raw += chunk
        if len(raw) > MAX_FRAME_BYTES:
            return bytes(raw)


def _names(requested, own) -> bool:
    """Whether a request's path names the owner's own: the same spelling, or the same file.

    Only an absolute path can name anything here: a relative one would resolve against this
    daemon's directory, not the caller's. Go's hook.namesOwnPath is the same rule.
    """
    if not isinstance(requested, str) or not os.path.isabs(requested):
        return False
    if os.path.normpath(requested) == os.path.normpath(str(own)):
        return True
    try:
        return os.path.samefile(requested, own)
    except (OSError, ValueError):
        return False


def owner_paths(state, params):
    """The marker root and receipt store this owner evaluates with, or the host detail refusing.

    A control request may not choose where the owner writes (PR #185 thread 4127894432). The
    owner evaluates only under its own marker root, resolved from its own configuration as the
    relay resolves one without --marker-root (the root the installer records as the hook's
    markerRoot), and reads only its own store: a request's dbPath, when present, must name
    state/relay.sqlite3. Either is then used as the owner's own path, never the request's
    spelling. journalRoot and every other hook setting are not request parameters at all; the
    adapter journals in its own process. Go's HandleControl applies the same rule.
    """
    own_root = marker.resolve_marker_root(None).path
    requested = params.get("markerRoot")
    if not _names(requested, own_root):
        return None, None, ("control.sock evaluates Stops only under this owner's marker root "
                            + str(own_root) + "; the request named "
                            + (requested if isinstance(requested, str) else "no path"))
    own_db = Path(state) / "relay.sqlite3"
    requested = params.get("dbPath")
    if requested is not None and not _names(requested, own_db):
        return None, None, ("control.sock reads only this owner's store " + str(own_db)
                            + "; the request named "
                            + (requested if isinstance(requested, str) else "no path"))
    return own_root, (own_db if requested is not None else None), None


class GuardServer:
    """Started only while the caller owns daemon, scope, and writer admission."""

    def __init__(self, state):
        self.path = Path(state) / "control.sock"
        if self.path.exists():
            if not stat.S_ISSOCK(self.path.lstat().st_mode):
                raise ValueError("control.sock is not a socket")
            self.path.unlink()
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.wake, self.stopper = socket.socketpair()
        try:
            # Linux can address a long canonical state directory through a held
            # directory descriptor without changing the public socket pathname.
            directory = os.open(self.path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                address = str(self.path)
                if len(os.fsencode(address)) >= 104 and Path("/proc/self/fd").is_dir():
                    address = f"/proc/self/fd/{directory}/control.sock"
                self.listener.bind(address)
            finally:
                os.close(directory)
            os.chmod(self.path, 0o600)
            self.listener.listen(16)
            self.thread = threading.Thread(target=self._serve, name="relay-control", daemon=True)
            self.thread.start()
        except BaseException:
            self.listener.close()
            self.wake.close()
            self.stopper.close()
            raise

    def _serve(self):
        """The only serving thread: nothing a peer or the kernel does may end it."""
        with selectors.DefaultSelector() as selector:
            selector.register(self.listener, selectors.EVENT_READ)
            selector.register(self.wake, selectors.EVENT_READ)
            while True:
                ready = selector.select()
                if any(key.fileobj is self.wake for key, _ in ready):
                    return
                try:
                    connection, _ = self.listener.accept()
                except (BlockingIOError, InterruptedError, ConnectionAbortedError):
                    continue
                except OSError:
                    # EMFILE, ENFILE, ENOBUFS, ENOMEM: back off briefly, then accept again.
                    if select.select([self.wake], [], [], 0.05)[0]:
                        return
                    continue
                with connection:
                    try:
                        connection.settimeout(READ_TIMEOUT)
                        trusted = peer_uid(connection) == os.getuid()
                    except Exception:  # noqa: BLE001 - an unauthenticated peer is only closed
                        continue
                    if not trusted:
                        continue
                    try:
                        self._answer(connection)
                    except Exception as error:  # noqa: BLE001 - answered, never fatal to the loop
                        answer = {"error": "host", "detail": f"{type(error).__name__}: {error}"}
                        try:
                            connection.sendall(json.dumps(answer).encode() + b"\n")
                        except OSError:
                            # The peer has already gone. No durable operation is lost:
                            # guard-evaluate only reads relay receipts.
                            continue

    def _fallback(self, socket_path, program):
        """The owner's store under the CLI's selection refusals (Go hook ownerFallback).

        Resolved in this daemon's environment; a selection other than this state directory is
        replaced by this directory as an explicit --state, whose override refusal still applies.
        """
        state = self.path.parent

        def resolve():
            from . import cli
            from .store import resolve_state_dir

            selection = resolve_state_dir(None, socket_path)
            if Path(selection.path) != state:
                selection = resolve_state_dir(str(state), socket_path)
            services = types.SimpleNamespace(selection=selection, socket_path=socket_path)
            token = cli.PROGRAM.set(program or "codex-session-relay")
            try:
                return cli._guard_fallback(services, None)()
            finally:
                cli.PROGRAM.reset(token)

        return resolve

    def _answer(self, connection):
        raw = _read_request(connection, time.monotonic() + READ_TIMEOUT)
        if len(raw) > MAX_FRAME_BYTES:
            raise ValueError("guard request exceeds 64 MiB")
        request = json.loads(raw)
        if not isinstance(request, dict):
            raise TypeError("guard request must be an object")
        if request.get("protocol") != 1 or request.get("method") != "guard-evaluate":
            connection.sendall(b'{"protocol":1,"requestRejected":true}\n')
            return
        params = request["params"]
        if not isinstance(params, dict):
            raise TypeError("guard params must be an object")
        stop = params["stopInput"]
        if not isinstance(stop, dict):
            raise TypeError("stop input must be an object")
        if not isinstance(params.get("deadline"), str):
            raise TypeError("guard deadline must be a string")
        deadline = datetime.fromisoformat(params["deadline"].replace("Z", "+00:00"))
        remaining = (deadline - datetime.now(timezone.utc)).total_seconds()
        if remaining <= 0:
            raise TimeoutError("guard request deadline expired")
        connection.settimeout(min(5, remaining))
        socket_path, program = params.get("socketPath"), params.get("program")
        if socket_path is not None and not isinstance(socket_path, str):
            raise TypeError("guard socketPath must be a string")
        if program is not None and not isinstance(program, str):
            raise TypeError("guard program must be a string")
        # mode and now reach the verdict and the observation it records as they are sent, so
        # only a string names either; null, or an empty string, asks for the default.
        for key in ("mode", "now"):
            if params.get(key) is not None and not isinstance(params.get(key), str):
                raise TypeError("guard " + key + " must be a string")
        root, db_path, refused = owner_paths(self.path.parent, params)
        if refused is not None:
            # Answered before anything is read or recorded, as a host error: the request asked
            # this owner to evaluate somewhere it does not, which is no Stop refusal.
            connection.sendall(json.dumps({"error": "host", "detail": refused}).encode() + b"\n")
            return
        try:
            answer = guard.evaluate(root, stop,
                                    now=params.get("now") or datetime.now(timezone.utc).isoformat(),
                                    mode=params.get("mode") or guard.OBSERVE,
                                    db_path=None if db_path is None else str(db_path),
                                    default_db_path=self._fallback(socket_path, program),
                                    record=not params.get("noRecord", False))
        except guard.StoreNotSelected as refused:
            # The CLI's exit-2 refusal payload, shared rather than bypassed (decision 24).
            answer = refused.detail
        connection.sendall(json.dumps(answer, default=str).encode() + b"\n")

    def close(self):
        self.stopper.sendall(b"stop")
        self.thread.join(timeout=6)
        self.listener.close()
        self.wake.close()
        self.stopper.close()
        if self.thread.is_alive():
            raise RuntimeError("guard control listener did not stop")
        self.path.unlink()
