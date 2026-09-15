"""The few value types that cross module boundaries."""

from dataclasses import dataclass


@dataclass(frozen=True)
class Endpoint:
    task_id: str
    host_id: str
    cwd: str | None = None
    cxc_session: str | None = None

    def to_record(self) -> dict:
        return {"taskId": self.task_id, "hostId": self.host_id, "cwd": self.cwd}


@dataclass(frozen=True)
class TurnRef:
    thread_id: str
    turn_id: str
    turn_status: str

    def to_record(self) -> dict:
        return {
            "threadId": self.thread_id,
            "turnId": self.turn_id,
            "turnStatus": self.turn_status,
        }

    @staticmethod
    def from_record(record: dict) -> "TurnRef":
        return TurnRef(record["threadId"], record["turnId"], record["turnStatus"])
