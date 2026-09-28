"""Private controller channel for reverse activation of the retained Python build."""

import json
import os
import socket
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Final

from . import ownership
from .service import boot_id, start_ticks

CHANNEL_ENV: Final = "CRW_TAKEOVER_CHANNEL_FD"
CHANNEL_TIMEOUT: Final = 20.0
MAX_CANDIDATE_BYTES: Final = 64 << 20


def identity(pid: int, *, build: str | None = None):
    """Use the service's boot/start identity, never just a reusable process number."""
    boot, ticks = boot_id(), start_ticks(pid)
    if not boot or ticks is None:
        raise ownership.OwnershipRefused("process identity is unavailable")
    value = {"bootId": boot, "pid": pid, "startTicks": ticks}
    if build is not None:
        value["build"] = build
    return value


class CandidateChannel:
    """Own one inherited socket until activation, with no discoverable endpoint."""

    def __init__(self, connection: socket.socket, path: Path):
        self.connection = connection
        self.path = path
        self.deadline = time.monotonic() + CHANNEL_TIMEOUT
        message = self.receive()
        record = message.get("record") if message else None
        if (not message or message.get("kind") != "start" or not isinstance(record, dict)
                or record.get("phase") != "starting" or record.get("owner") != "python"
                or not isinstance(record.get("transition"), dict)
                or not isinstance(record.get("controller"), dict)):
            raise ownership.OwnershipRefused("invalid candidate authorization")
        controller = record["controller"]
        if controller != identity(os.getppid()):
            raise ownership.OwnershipRefused("controller identity disagrees")
        transition_id = record["transition"].get("id")
        if not isinstance(transition_id, str) or not transition_id or type(record.get("epoch")) is not int:
            raise ownership.OwnershipRefused("invalid candidate permit")
        self.permit = ownership.CandidatePermit(
            transition_id, record["epoch"], controller["bootId"],
            controller["pid"], controller["startTicks"],
        )
        ownership.validate(path, ownership.metadata(path), record, candidate=self.permit)
        ownership.check_start(path, candidate=self.permit)
        self.record = record

    def receive(self):
        """One JSON line under an absolute deadline, including partial/trickled reads."""
        line = bytearray()
        while True:
            remaining = self.deadline - time.monotonic()
            if remaining <= 0:
                raise ownership.OwnershipRefused("candidate channel deadline exceeded")
            self.connection.settimeout(remaining)
            byte = self.connection.recv(1)
            if not byte:
                if line:
                    raise ownership.OwnershipRefused("truncated candidate message")
                return None
            if byte == b"\n":
                value = json.loads(line)
                if not isinstance(value, dict):
                    raise ownership.OwnershipRefused("candidate message is not an object")
                return value
            line.extend(byte)
            if len(line) > MAX_CANDIDATE_BYTES:
                raise ownership.OwnershipRefused("candidate message exceeds the size limit")

    def send(self, value) -> None:
        encoded = json.dumps(value, separators=(",", ":")).encode() + b"\n"
        if len(encoded) > MAX_CANDIDATE_BYTES:
            raise ownership.OwnershipRefused("candidate message exceeds the size limit")
        self.connection.sendall(encoded)

    def ready(self) -> None:
        """Recovery finished; only the durable active holder may go on to serve."""
        own = identity(os.getpid(), build=ownership.BUILD)
        self.send({"kind": "ready", "identity": own, "storeId": self.record["storeId"],
                   "epoch": self.permit.epoch, "transitionId": self.permit.transition_id})
        answer = self.receive()
        current, meta = ownership.mirror(self.path), ownership.metadata(self.path)
        if current is None:
            raise ownership.OwnershipRefused("activation record is missing")
        ownership.validate(self.path, meta, current, candidate=self.permit)
        if (current["phase"] != "active" or current["holder"] != own
                or current["epoch"] != self.permit.epoch
                or meta["takeover_id"] != self.permit.transition_id
                or (current.get("transition") or {}).get("id") != self.permit.transition_id):
            raise ownership.OwnershipRefused("activation channel closed without matching active record")
        if answer is not None:
            if answer.get("kind") != "active":
                raise ownership.OwnershipRefused("invalid activation answer")
            self.send({"kind": "activated"})


@contextmanager
def receive_candidate(path: Path):
    """Consume designation before any writable open; never pass fd 3 to a worker."""
    if os.environ.pop(CHANNEL_ENV, None) != "3":
        raise ownership.OwnershipRefused("candidate needs inherited controller channel")
    with socket.socket(fileno=3) as connection:
        connection.set_inheritable(False)
        if connection.type != socket.SOCK_STREAM:
            raise ownership.OwnershipRefused("candidate channel must be a connected stream")
        connection.getpeername()
        yield CandidateChannel(connection, path)
