"""The claim a run leaves in the environment it is building, and who may remove it.

The environment name is derived from the definition version and the source digests, so it is
deterministic, and the directory is created with an exclusive mkdir. That exclusivity proves a
run owns the directory and may remove it when it fails, and the proof used to expire badly: a
run killed outright left a directory behind with nothing to say who made it, and every later
run refused that destination at the existence check, for ever.

So a run leaves two files, and they are two because they answer two questions.

The LOCK file answers "is anybody still building this". It is created once and never replaced,
because an advisory lock belongs to an inode and not to a name: replacing the file the lock was
taken on leaves the lock on an unlinked inode while the next reader opens the new one and finds
it free. That defect is not theoretical here -- it deleted a live build in testing -- so the
lock file is opened, locked, and never written through a rename again.

The CLAIM file answers "what did that run say it was doing". It is rewritten when the staging
settles, which is exactly why it cannot also be the lock.

Liveness is the lock and never a recorded process id. The relay reached the same conclusion
about its own supervisor: inside a container sharing a kernel the same pid under the same boot
id is a different process, so an identity that can lie is worse than no reading at all. The pid
and the host are recorded as provenance for a report and decide nothing.

Removing anything requires positive proof of ownership. A claim this command did not write is
not a licence to delete a directory, so the claim has to carry this command's own marker and a
state from the declared set; anything else is somebody else's file that happens to sit at that
path.
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
# Separate from the claim, and never replaced. See the module docstring: a lock follows the
# inode, so locking a file that is later rewritten by rename unlocks it silently.
LOCK_NAME = ".crw-staging-lock"

# The marker that makes a claim THIS command's. Without it any readable JSON at that path would
# authorise deleting the directory it sits in.
WRITTEN_BY = "runtime_install.py"
CLAIM_VERSION = 1

# What a claim says about the run that wrote it.
STAGING = "STAGING"
COMPLETE = "COMPLETE"
CLAIM_STATES = (STAGING, COMPLETE)

# Whether anybody still holds the lock. A partition and not a boolean, because "nobody could
# tell" is a third answer and it is the one that must never be read as "nobody is there".
LIVE = "LIVE"
DEAD = "DEAD"
UNKNOWN = "UNKNOWN"
LIVENESS = (LIVE, DEAD, UNKNOWN)

# What a reading of an existing environment directory decides.
RECLAIM = "RECLAIM"
ADOPT = "ADOPT"
RESUME = "RESUME"
OCCUPIED = "OCCUPIED"
FOREIGN = "FOREIGN"
KEEP = "KEEP"
SETTLED = "SETTLED"
DECISIONS = (RECLAIM, ADOPT, RESUME, OCCUPIED, FOREIGN, KEEP, SETTLED)

# The only decision that deletes anything. Declared, so a reader can see the whole of what this
# module authorises removal for rather than having to find every branch.
REMOVES = (RECLAIM,)

# The errnos flock raises for a lock somebody else holds. Anything else failed to ask, and
# failing to ask is UNKNOWN rather than free.
CONTENTION = (errno.EACCES, errno.EAGAIN, errno.EWOULDBLOCK)

# Each input decide() reads, and the reading that answers it. The decision fills no cell from a
# neighbour's value, so the inventory is the argument set and every argument names its reader.
# 'protected' is the caller's reading, passed as a keyword the way the conflict readings are,
# because this module has neither the host record nor the pointer in scope.
DECISION_READINGS = {
    "claim": ("staging", "read_claim"),
    "liveness": ("staging", "owner_liveness"),
    "occupied": ("staging", "directory_occupied"),
    "protected": ("runtime_install", "protected_environment"),
}


def claim_path(environment):
    return Path(environment) / CLAIM_NAME


def lock_path(environment):
    return Path(environment) / LOCK_NAME


def claim_payload(state, *, issue=None, run=None):
    """What a claim records. Everything except 'state' is provenance and decides nothing."""
    return {
        "claimVersion": CLAIM_VERSION,
        "state": state,
        "writtenBy": WRITTEN_BY,
        "issue": issue,
        "run": run,
        "pid": os.getpid(),
        "host": socket.gethostname(),
        "writtenAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "livenessNote": (
            "the pid and host are provenance for a report, not a liveness test. Whether a run"
            " is still building is decided by the advisory lock on " + LOCK_NAME + " alone."
        ),
    }


def shape(claim):
    """Reject anything that is not a claim this command wrote.

    Ownership is what this establishes, and it is the precondition for deleting a directory.
    A bare object, an object with no state, or an object written by something else is not a
    claim: it is a file at a path, and reading it as permission to remove the directory around
    it is how somebody else's work gets deleted.
    """
    if not isinstance(claim, dict):
        raise TypeError("a staging claim is an object, found " + type(claim).__name__)
    if claim.get("writtenBy") != WRITTEN_BY:
        raise ValueError("this claim was not written by " + WRITTEN_BY + ", it names "
                         + repr(claim.get("writtenBy")))
    if claim.get("claimVersion") != CLAIM_VERSION:
        raise ValueError("a claim declares claimVersion " + str(CLAIM_VERSION) + ", found "
                         + repr(claim.get("claimVersion")))
    if claim.get("state") not in CLAIM_STATES:
        raise ValueError("a staging claim's state is one of " + ", ".join(CLAIM_STATES)
                         + ", found " + repr(claim.get("state")))
    return claim


def read_claim(environment):
    """Read the claim as a reading, so absent, present, unreadable and unreachable stay four
    answers. An absent claim carries None rather than an empty claim: a directory with nothing
    in it is not a directory this command said it owns."""
    return reading.read_json(claim_path(environment), "the staging claim", absent=None,
                             shape=shape)


def write_claim(environment, state, *, issue=None, run=None):
    """Write the claim's bytes. This replaces the claim file, which is why it is not the file
    the advisory lock is taken on."""
    target = claim_path(environment)
    payload = claim_payload(state, issue=issue, run=run)
    with hostrecord.Locked(target):
        hostrecord.atomic_write(target, json.dumps(payload, indent=2, sort_keys=True) + "\n")
    return payload


class Held:
    """The advisory lock a run holds while it is building, on a file nothing rewrites.

    Taking it fails rather than waiting: a directory somebody else is building is not this
    run's to take, and blocking on it would turn a clean refusal into a stall of unknown length.
    """

    def __init__(self, environment):
        self.path = lock_path(environment)
        self.handle = None
        self.available = fcntl is not None

    def take(self):
        """Acquire the lock and keep holding it, without a scope to leave.

        An install runs for minutes across many steps, so the lock's lifetime is the RUN's and
        not a block's. Held this way the operating system releases it when this process ends,
        however it ends, which is exactly the question a later run asks: is anybody still
        building this. A context manager would release it at the end of a block that is not the
        end of the work.
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
    """Whether anybody still holds this staging. Returns (state, detail).

    Taking the lock and releasing it immediately is the whole test. DEAD is answered only when
    the lock was actually free; UNKNOWN whenever the question could not be put -- no advisory
    locking on this platform, a file that could not be opened, or any error other than
    contention. An owner nobody could establish is never reported as an owner that is gone,
    because deleting a live run's environment is the accident this exists to prevent.
    """
    if fcntl is None:
        return UNKNOWN, ("this platform provides no advisory locking, so whether a run still"
                         " holds this directory could not be established")
    path = lock_path(environment)
    try:
        handle = os.open(str(path), os.O_RDWR)
    except FileNotFoundError:
        return DEAD, ("no run has taken the lock on this staging; there is nothing holding it")
    except OSError as error:
        return UNKNOWN, ("the staging lock could not be opened to test it: "
                         + type(error).__name__ + ": " + str(error))
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as error:
        if error.errno in CONTENTION:
            return LIVE, "another run holds the advisory lock on this staging"
        return UNKNOWN, ("the advisory lock could not be tested: " + type(error).__name__
                         + ": " + str(error))
    else:
        fcntl.flock(handle, fcntl.LOCK_UN)
        return DEAD, "nothing holds the advisory lock on this staging"
    finally:
        os.close(handle)


def directory_occupied(environment):
    """Whether this directory holds anything besides this command's own two files.

    Returns (occupied, detail). None means the listing could not be made, which is not the same
    as an empty directory and is never read as one.
    """
    ours = (CLAIM_NAME, LOCK_NAME)
    try:
        entries = [entry.name for entry in Path(environment).iterdir()]
    except OSError as error:
        return None, ("the directory could not be listed: " + type(error).__name__ + ": "
                      + str(error))
    other = [name for name in entries if name not in ours]
    return bool(other), ("it holds " + str(len(other)) + " entries besides this command's own"
                         if other else "it holds nothing besides this command's own files")


def decide(claim, liveness, *, occupied, protected):
    """What may be done with an environment directory that already exists.

    Returns (decision, reason). Each argument is one reading's answer and none of them is
    derived from another here; a caller that could not make a reading passes what that reading
    actually returned, including None, rather than a stand-in.

    RECLAIM is the only decision that deletes, and it is reached only for a directory carrying
    a claim this command wrote, in STAGING, whose owner is established gone, that nothing is
    using. A finished environment is never deleted here however its selection has moved: it is
    a runtime that was promoted once, and a process may still be running out of it.
    """
    if claim.state == reading.ABSENT:
        if occupied is None:
            return KEEP, ("there is no claim here and the directory could not be listed, so"
                          " whether it holds anything could not be established")
        if occupied:
            return FOREIGN, ("this directory holds files and carries no claim from this"
                             " command, so it belongs to somebody else and is left alone")
        if protected:
            return KEEP, ("this environment is in use and carries no claim, so it is left"
                          " exactly as it is")
        # Nothing is in it, so nothing is deleted: it is taken over as it stands. This is the
        # window between the exclusive mkdir and the claim being written, and without this
        # answer a run killed inside that window would refuse its own destination for ever.
        return ADOPT, ("this directory is empty and carries no claim, so it is taken over as"
                       " it stands; nothing is removed")

    if not claim.usable:
        return KEEP, ("no claim of this command's could be read here, so who owns this"
                      " directory could not be established: " + str(claim.detail))

    state = (claim.value or {}).get("state")
    if liveness == LIVE:
        return OCCUPIED, "another run holds this staging and is still building it"
    if liveness == UNKNOWN:
        return KEEP, ("whether a run still holds this staging could not be established, and an"
                      " owner nobody could establish is not an owner that is gone")

    if state == COMPLETE:
        if protected:
            return SETTLED, ("this combination is already installed here and in use, so there"
                             " is nothing to build")
        return KEEP, ("this environment was promoted once and finished. Nothing selects it now,"
                      " but a process started from it may still be running out of it, so it is"
                      " reported rather than removed")

    if protected:
        # The previous run committed the selection and did not live to move the pointer. The
        # runtime is built and selected; what is missing is the half that was never written.
        return RESUME, ("a previous run committed this environment as selected and did not"
                        " finish. It is built and in use, so the pointer is brought into"
                        " agreement with the selection rather than anything being rebuilt")
    return RECLAIM, ("this staging was abandoned by a run that no longer holds it and nothing"
                     " selects it or points at it, so it is removed and created again")


def settled(claim):
    """Whether a readable claim says its run finished. Never inferred from the directory."""
    return claim.ok and (claim.value or {}).get("state") == COMPLETE

