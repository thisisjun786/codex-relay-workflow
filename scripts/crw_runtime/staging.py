"""The claim a run leaves in the environment it is building, and who may remove it.

The environment name is derived from the definition version and the source digests, so it is
deterministic, and the directory is created with an exclusive mkdir. That exclusivity is what
proves a run owns the directory and may therefore remove it when it fails. The proof used to
expire badly: a run killed outright left the directory behind with nothing to say who made it,
and every later run then refused that destination at the existence check, for ever.

So a run writes a claim inside the directory and holds an advisory lock on it for its lifetime.
A later run then has a question it can answer -- who owns this -- instead of a guess.

Liveness is the lock, never the recorded process id. The relay reached the same conclusion about
its own supervisor: inside a container sharing a kernel, the same pid under the same boot id is a
different process, so a process identity that can lie is worse than no reading at all. The pid,
the host and the time are recorded beside the lock as provenance, so a report can say what left a
directory behind, but nothing here decides on them.

Two locks sit on the claim file and they answer two different questions. The advisory lock says
whether anybody is still building this directory. The exclusive lock file beside it serialises
the read-modify-write of the claim's own bytes. Collapsing them would mean a run that wanted to
know who was building could only find out by taking the writer's lock.
"""

import errno
import json
import os
import socket
import time
from pathlib import Path

from . import hostrecord, reading

try:
    import fcntl
except ImportError:                                              # pragma: no cover - not POSIX
    fcntl = None

CLAIM_NAME = ".crw-staging-claim.json"

# What a claim says about the run that wrote it.
STAGING = "STAGING"
COMPLETE = "COMPLETE"
CLAIM_STATES = (STAGING, COMPLETE)

# Whether anybody still holds the claim. A partition and not a boolean, because "nobody could
# tell" is a third answer and it is the one that must never be read as "nobody is there".
LIVE = "LIVE"
DEAD = "DEAD"
UNKNOWN = "UNKNOWN"
LIVENESS = (LIVE, DEAD, UNKNOWN)

# What a reading of an existing environment directory decides.
RECLAIM = "RECLAIM"
OCCUPIED = "OCCUPIED"
FOREIGN = "FOREIGN"
KEEP = "KEEP"
SETTLED = "SETTLED"
DECISIONS = (RECLAIM, OCCUPIED, FOREIGN, KEEP, SETTLED)

# Only this decision authorises removing a directory a previous run left behind.
REMOVES = (RECLAIM,)

# Each input decide() reads, and the reading that answers it. The decision fills no cell from a
# neighbour's value, so the inventory is the argument set and every argument names its reader.
# A protected environment is the caller's reading, passed as a keyword the way the conflict
# readings are, because this module has no record and no pointer in scope.
# The errnos flock raises for a lock somebody else holds. Anything else failed to ask,
# and failing to ask is UNKNOWN rather than free.
CONTENTION = (errno.EACCES, errno.EAGAIN, errno.EWOULDBLOCK)

DECISION_READINGS = {
    "claim": ("staging", "read_claim"),
    "liveness": ("staging", "owner_liveness"),
    "occupied": ("staging", "directory_occupied"),
    "protected": ("runtime_install", "protected_environment"),
}


def claim_path(environment):
    return Path(environment) / CLAIM_NAME


def claim_payload(state, *, issue=None, run=None):
    """What a claim records. Everything except 'state' is provenance and decides nothing."""
    return {
        "claimVersion": 1,
        "state": state,
        "writtenBy": "runtime_install.py",
        "issue": issue,
        "run": run,
        "pid": os.getpid(),
        "host": socket.gethostname(),
        "writtenAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "livenessNote": (
            "the pid and host are provenance for a report, not a liveness test. Whether this"
            " run is still building is decided by the advisory lock on this file alone."
        ),
    }


def shape(claim):
    """Reject a claim whose containers no consumer here can use."""
    if not isinstance(claim, dict):
        raise TypeError("a staging claim is an object, found " + type(claim).__name__)
    state = claim.get("state")
    if state is not None and state not in CLAIM_STATES:
        raise ValueError("a staging claim's state is one of " + ", ".join(CLAIM_STATES)
                         + ", found " + repr(state))
    return claim


def read_claim(environment):
    """Read the claim as a reading, so absent, present, unreadable and unreachable stay four
    answers. An absent claim carries None rather than an empty claim: a directory with nothing
    in it is not a directory this command said it owns."""
    return reading.read_json(claim_path(environment), "the staging claim", absent=None,
                             shape=shape)


def write_claim(environment, state, *, issue=None, run=None):
    """Write the claim's bytes. Held under the writer's lock, which is not the advisory lock
    that answers the liveness question."""
    target = claim_path(environment)
    payload = claim_payload(state, issue=issue, run=run)
    with hostrecord.Locked(target):
        hostrecord.atomic_write(target, json.dumps(payload, indent=2, sort_keys=True) + "\n")
    return payload


class Held:
    """The advisory lock a run holds on its claim for as long as it is building.

    Entering fails rather than waiting: a directory somebody else is building is not this run's
    to take, and blocking on it would turn a clean refusal into a stall of unknown length.
    """

    def __init__(self, environment):
        self.path = claim_path(environment)
        self.handle = None
        self.available = fcntl is not None

    def take(self):
        """Acquire the lock and keep holding it, without a scope to leave.

        An install runs for minutes across many steps, so the lock's lifetime is the RUN's and
        not a block's. Held this way the operating system releases it when this process ends,
        however it ends, which is exactly the question a later run asks: is anybody still
        building this. A context manager would release it at the end of a block that is not
        the end of the work.
        """
        if not self.available:
            return self
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = os.open(str(self.path), os.O_CREAT | os.O_RDWR, 0o644)
        try:
            fcntl.flock(self.handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BaseException:
            os.close(self.handle)
            self.handle = None
            raise
        return self

    def __enter__(self):
        return self.take()

    def __exit__(self, *exc):
        # Released on every path, including an exception, so a failure cannot strand the lock.
        if self.handle is not None:
            try:
                fcntl.flock(self.handle, fcntl.LOCK_UN)
            finally:
                os.close(self.handle)
                self.handle = None
        return False


def owner_liveness(environment):
    """Whether anybody still holds this claim. Returns (state, detail).

    Taking the lock and releasing it immediately is the whole test. It answers DEAD only when
    the lock was actually free, and UNKNOWN whenever the question could not be put -- no
    advisory locking on this platform, a file that could not be opened, or any error other
    than contention. An owner nobody could establish is never reported as an owner that is
    gone, because deleting a live run's environment is the accident this exists to prevent.
    """
    if fcntl is None:
        return UNKNOWN, ("this platform provides no advisory locking, so whether a run still"
                         " holds this directory could not be established")
    path = claim_path(environment)
    try:
        handle = os.open(str(path), os.O_RDWR)
    except OSError as error:
        return UNKNOWN, ("the claim could not be opened to test its lock: "
                         + type(error).__name__ + ": " + str(error))
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as error:
        if error.errno in CONTENTION:
            return LIVE, "another run holds the advisory lock on this claim"
        return UNKNOWN, ("the advisory lock could not be tested: " + type(error).__name__
                         + ": " + str(error))
    else:
        fcntl.flock(handle, fcntl.LOCK_UN)
        return DEAD, "nothing holds the advisory lock on this claim"
    finally:
        os.close(handle)



def directory_occupied(environment):
    """Whether this directory holds anything besides the claim. Returns (occupied, detail).

    None means the listing could not be made, which is not the same as an empty directory and
    is never read as one.
    """
    try:
        entries = [entry.name for entry in Path(environment).iterdir()]
    except OSError as error:
        return None, ("the directory could not be listed: " + type(error).__name__ + ": "
                      + str(error))
    other = [name for name in entries if name != CLAIM_NAME]
    return bool(other), ("it holds " + str(len(other)) + " entries besides the claim"
                         if other else "it holds nothing besides the claim")


def decide(claim, liveness, *, occupied, protected):
    """What may be done with an environment directory that already exists.

    Returns (decision, reason). Each argument is one reading's answer and none of them is
    derived from another here; a caller that could not make a reading passes what that reading
    actually returned, including None, rather than a stand-in.

    Only RECLAIM removes anything, and it is reached only for a directory carrying this
    command's own claim whose owner is established gone and which nothing is using.
    """
    if claim.state == reading.ABSENT:
        if protected:
            return KEEP, ("this environment is in use and carries no claim, so it is left"
                          " exactly as it is")
        if occupied is None:
            return KEEP, ("there is no claim here and the directory could not be listed, so"
                          " whether it holds anything could not be established")
        if occupied:
            return FOREIGN, ("this directory holds files and carries no claim from this"
                             " command, so it belongs to somebody else and is left alone")
        # An empty directory holds nothing to lose. This is the window between the exclusive
        # mkdir and the claim being written, and without this answer a run killed inside that
        # window would refuse its own destination for ever.
        return RECLAIM, ("an empty directory with no claim carries nothing, so it is removed"
                         " and created again")

    if not claim.usable:
        return KEEP, ("the claim could not be read, so who owns this directory could not be"
                      " established: " + str(claim.detail))

    state = (claim.value or {}).get("state")
    if liveness == LIVE:
        return OCCUPIED, "another run holds this directory and is still building it"
    if liveness == UNKNOWN:
        return KEEP, ("whether a run still holds this directory could not be established, and"
                      " an owner nobody could establish is not an owner that is gone")

    if state == COMPLETE and protected:
        return SETTLED, ("this combination is already installed here and in use, so there is"
                         " nothing to build")
    if protected:
        return KEEP, ("this environment is in use, so it is not this run's to remove however"
                      " its claim reads")
    if state == COMPLETE:
        # Finished, and nothing selects it or points at it. A gate that refused the swap
        # leaves exactly this, and refusing to rebuild it would refuse the retry too.
        return RECLAIM, ("this staging finished and nothing selects it or points at it, so it"
                         " is a candidate this command built and never promoted")
    return RECLAIM, ("this staging was abandoned by a run that no longer holds it, so it is"
                     " removed and created again")


def settled(claim):
    """Whether a readable claim says its run finished. Never inferred from the directory."""
    return claim.ok and (claim.value or {}).get("state") == COMPLETE

