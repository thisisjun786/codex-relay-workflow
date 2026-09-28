"""The retained Python owner's bounded guard-evaluate control socket."""

import json
import os
import selectors
import socket
import stat
import struct
import threading
from datetime import datetime, timezone
from pathlib import Path

from . import guard


def peer_uid(connection):
    """The kernel-observed Unix peer uid; no request bytes are trusted first."""
    if not hasattr(socket, "SO_PEERCRED"):
        raise OSError("guard peer credentials unavailable")
    raw = connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i"))
    _pid, uid, _gid = struct.unpack("3i", raw)
    return uid


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
        with selectors.DefaultSelector() as selector:
            selector.register(self.listener, selectors.EVENT_READ)
            selector.register(self.wake, selectors.EVENT_READ)
            while True:
                ready = selector.select()
                if any(key.fileobj is self.wake for key, _ in ready):
                    return
                connection, _ = self.listener.accept()
                with connection:
                    connection.settimeout(5)
                    if peer_uid(connection) != os.getuid():
                        continue
                    try:
                        self._answer(connection)
                    except (OSError, ValueError, TypeError, KeyError) as error:
                        answer = {"error": "host", "detail": f"{type(error).__name__}: {error}"}
                        try:
                            connection.sendall(json.dumps(answer).encode() + b"\n")
                        except OSError:
                            # The peer has already gone. No durable operation is lost:
                            # guard-evaluate only reads relay receipts.
                            continue

    def _answer(self, connection):
        with connection.makefile("rb") as stream:
            raw = stream.readline(16777217)
        if len(raw) > 16777216:
            raise ValueError("guard request exceeds 16 MiB")
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
        deadline = datetime.fromisoformat(params["deadline"].replace("Z", "+00:00"))
        remaining = (deadline - datetime.now(timezone.utc)).total_seconds()
        if remaining <= 0:
            raise TimeoutError("guard request deadline expired")
        connection.settimeout(min(5, remaining))
        answer = guard.evaluate(params["markerRoot"], stop,
                                now=params.get("now") or datetime.now(timezone.utc).isoformat(),
                                mode=params.get("mode") or guard.OBSERVE,
                                db_path=params.get("dbPath"),
                                default_db_path=str(self.path.parent / "relay.sqlite3"),
                                record=not params.get("noRecord", False))
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
