"""What a run left behind on an install destination, asked of the decision that owns it.

`residualPaths` existed only on a failed install's own JSON result, so an operator who wanted
the cleanup warning after a failed update had to have kept that run's stdout. Diagnosis is
where that question belongs, and this answers it.

It writes nothing, and it removes nothing. It does not decide either: staging.decide already
says what may be done with an environment directory that exists, and staging.REMOVES already
declares which of its decisions authorises removal. Writing a second set of rules here would be
a second opinion about the same directory kept equal to the first by hand -- and the first
draft of this module proved that is not theoretical, because its own rules had no answer for an
unreadable host record or a pointer nobody could resolve, both of which decide() has and keeps.

So an entry is residue exactly when the installer would reclaim it, and every guard that
decision carries is inherited rather than restated: a claim this command wrote, in STAGING, an
owner established gone, a selection that positively does not name it, and a protection reading
that is conservative in the safe direction. A selected staging is RESUME -- built, and possibly
in use -- and a finished environment is KEEP however its selection moved, because a process may
still be running out of it.

Two readings are excluded from the scan itself rather than judged. Symbolic links are skipped,
so the owned pointer is never followed into the environment it names and reported as a staging
sitting there; and the pointer's own path is excluded by name for the same reason.

What this answer is NOT: the install failure's residualPaths is what THAT RUN left, read from
the run itself. This is what is on the destination now. Neither is a superset of the other, and
an empty answer here never means a failed run left nothing behind.
"""

import os
import stat
from pathlib import Path

from . import pointer, staging

# Why a path is named. The decision itself comes from staging and is reported verbatim; this
# only names the two questions this module adds around it.
DANGLING_POINTER = "dangling_pointer"
FOREIGN_POINTER = "foreign_pointer"
UNREADABLE_POINTER_TARGET = "unreadable_pointer_target"
POINTER_OUTSIDE_DESTINATION = "pointer_outside_destination"
NOT_SCANNED = "not_scanned"
POINTER_FINDINGS = (DANGLING_POINTER, FOREIGN_POINTER, UNREADABLE_POINTER_TARGET,
                    POINTER_OUTSIDE_DESTINATION, NOT_SCANNED)

# What a listed child is. None is a fourth answer and it is the one that matters: scandir can
# succeed while a child's own metadata lookup fails, and Path.is_dir() reports that failure as
# "not a directory", which reads exactly like a regular file.
DIRECTORY = "directory"
LINK = "link"
OTHER = "other"
KINDS = (DIRECTORY, LINK, OTHER)


def _kind_of(path):
    """(kind, detail), or (None, why) when the child could not be read at all."""
    try:
        found = os.lstat(str(path))
    except OSError as error:
        return None, ("this entry could not be inspected: " + type(error).__name__ + ": "
                      + str(error))
    if stat.S_ISLNK(found.st_mode):
        return LINK, "a symbolic link"
    if stat.S_ISDIR(found.st_mode):
        return DIRECTORY, "a directory"
    return OTHER, "not a directory"

NOTE = ("residue is exactly what the installer's own decision would reclaim (staging.REMOVES),"
        " so what is reported here as clearable and what a later install would take are the"
        " same set rather than two opinions. A failed install's residualPaths is a different"
        " reading: it is what THAT RUN left, and this is what is on the destination now."
        " It is also not a snapshot: the claim, the lock, the contents, the host record and"
        " the pointer are read at different moments, so a host changing underneath this"
        " command is described in pieces. Every decision is conservative in the same"
        " direction, so that costs a path being kept rather than a live one being named.")


def _entry(path, **fields):
    found = {"path": str(path), "decision": None, "reason": None, "residual": False}
    found.update(fields)
    return found


def _same_directory(one, other):
    """Whether two spellings name the same directory, answered conservatively.

    Two spellings of one directory must compare equal -- Path.absolute() keeps '..', so
    /tmp/detour/../dest and /tmp/dest were two destinations and an owned dangling pointer in
    the surveyed one was disowned. But lexical cancellation is not sound either: the kernel
    follows a symlink before applying '..', so /srv/link/../dest is /var/runtime/dest when
    /srv/link points into /var/runtime, while normpath says /srv/dest.

    So BOTH have to agree. The lexical form catches the spelling difference and the resolved
    form catches the symlink traversal, and a pair that disagrees is treated as different --
    which keeps an unverified pointer out of the cleanup list, the direction this whole module
    fails in on purpose.
    """
    one, other = str(one), str(other)
    try:
        if os.path.normpath(one) != os.path.normpath(other):
            return False
        # realpath answers best-effort and does not raise on a loop, but a NUL-bearing
        # string cannot name a path at all and raises ValueError from either of these.
        return os.path.realpath(one) == os.path.realpath(other)
    except ValueError:
        # Nothing was established, and this comparison's conservative answer is
        # "different", which keeps the pointer out of the cleanup list.
        return False


def _target_exists(path):
    """Whether the pointer's target is there. Three answers, because Path.exists() gives two.

    exists() returns False for a filesystem failure just as it does for a file that is not
    there, so a target behind an unreadable directory, a symlink loop or a transient I/O error
    read as established absence -- and a pointer whose target may be perfectly fine was named in
    a list telling an operator to remove it. Only FileNotFoundError establishes absence here.
    """
    try:
        os.stat(str(path))
    except FileNotFoundError:
        return False, "the target does not exist"
    except (OSError, ValueError) as error:
        return None, ("whether the target exists could not be established: "
                      + type(error).__name__ + ": " + str(error))
    return True, "the target exists"


def survey(destination, *, pointer_path=None, recorded_pointer=None, protection=None):
    """Every directory under this destination, classified by staging.decide.

    'protection' is the caller's ownership reading, called with an environment path and
    answering (protected, selected). A caller that passes none made no such reading, and then
    nothing is reported as residue at all: no residue found and nobody looked are different
    answers, and the second one must never be printed as an empty cleanup list.
    """
    answer = {"destination": None if destination is None else str(destination), "read": False,
              "entries": [], "pointer": None, "residualPaths": [], "recoveryRequires": [],
              "unreadable": [], "ownershipRead": protection is not None, "note": NOTE}
    if destination is None:
        answer["unreadable"].append("no destination was named, so nothing was scanned")
        return answer
    root = Path(destination)
    excluded = {str(Path(pointer_path))} if pointer_path else set()
    try:
        # Not a glob: a listing that cannot be made must raise here rather than come back as a
        # complete description of a smaller tree (reading.OMITTING_READERS).
        found = sorted(entry.path for entry in os.scandir(str(root)))
    except (OSError, ValueError) as error:
        # ValueError as well as OSError: a recorded pointer path may carry a NUL, which cannot
        # name a file at all, and scandir raises ValueError for it. A host record that is
        # otherwise readable must still produce a reading here rather than an internal error.
        answer["unreadable"].append("the destination could not be listed: "
                                    + type(error).__name__ + ": " + str(error))
        # The pointer is read and PUBLISHED on this path too. Returning before the common
        # assembly left a cell saying residual=true above an empty residualPaths, so a listing
        # failure in the destination silently dropped a pointer repair that had been
        # established independently of it.
        return _with_pointer(answer, pointer_path, recorded_pointer, root)
    answer["read"] = True
    for path in found:
        entry = Path(path)
        if str(entry) in excluded:
            answer["entries"].append(_entry(entry, decision=NOT_SCANNED,
                                            reason="this is the owned pointer, not an"
                                                   " environment under this destination"))
            continue
        kind, kind_detail = _kind_of(entry)
        if kind is None:
            # scandir succeeded and this child's own metadata did not. is_dir() answers False
            # for that exactly as it does for a regular file, so the survey used to record
            # "not a directory" and leave read=True with nothing unreadable -- an incomplete
            # scan presented as a complete one.
            answer["entries"].append(_entry(entry, decision=NOT_SCANNED, reason=kind_detail))
            answer["unreadable"].append(str(entry) + ": " + kind_detail)
            continue
        if kind == LINK:
            # Never followed. is_dir() answers about the target, so a link to an environment
            # would be scanned as though the link itself were that environment, and the claim
            # read under it would be the target's.
            answer["entries"].append(_entry(entry, decision=NOT_SCANNED,
                                            reason="a symbolic link is not an environment this"
                                                   " command built, and it is not followed"))
            continue
        if kind != DIRECTORY:
            answer["entries"].append(_entry(entry, decision=NOT_SCANNED,
                                            reason=kind_detail))
            continue
        claim = staging.read_claim(entry)
        liveness, liveness_detail = staging.owner_liveness(entry)
        occupied, occupied_detail = staging.directory_occupied(entry)
        if protection is None:
            answer["entries"].append(_entry(
                entry, decision=NOT_SCANNED, claimState=claim.state, liveness=liveness,
                reason="no ownership reading was supplied, so whether anything selects or"
                       " reaches this environment was not established and nothing about it"
                       " is reported as clearable"))
            continue
        protected, selected = protection(entry)
        decision, reason = staging.decide(claim, liveness, occupied=occupied,
                                          protected=protected, selected=selected)
        residual = decision in staging.REMOVES
        answer["entries"].append(_entry(
            entry, decision=decision, reason=reason, residual=residual,
            claimState=claim.state, claimReading=None if claim.usable else claim.refusal(),
            liveness=liveness, livenessDetail=liveness_detail,
            occupied=occupied, occupiedDetail=occupied_detail,
            recordSelectsIt=selected, protected=protected))
        if residual:
            answer["residualPaths"].append(str(entry))
            answer["recoveryRequires"].append(
                "let the next install of this same combination reclaim " + str(entry)
                + ", which takes the lock this reading did not: " + reason
                + ". Removing it by hand means re-reading it first, because this survey holds"
                  " no lock and an install may have started building there since it looked")
        elif not claim.usable:
            answer["unreadable"].append(str(entry) + ": " + str(claim.detail))
        elif liveness == staging.UNKNOWN:
            answer["unreadable"].append(str(entry) + ": " + liveness_detail)

    return _with_pointer(answer, pointer_path, recorded_pointer, root)


def _with_pointer(answer, pointer_path, recorded_pointer, destination):
    """Read the pointer and publish it. One place, because the two exits used to differ and the
    difference was a dropped repair rather than a difference anybody intended."""
    answer["pointer"] = _pointer_finding(pointer_path, recorded_pointer, destination)
    if answer["pointer"].get("residual"):
        answer["residualPaths"].append(answer["pointer"]["path"])
        answer["recoveryRequires"].append(
            "the pointer at " + answer["pointer"]["path"] + " names a target that is not there,"
            " and the host record records it as this command's own, so nothing reaches a"
            " runtime through it until an install repoints it. Repointing is the recovery, and"
            " an install does it under the lock this reading did not hold. This command does"
            " NOT recommend removing the link by hand: rereading it first does not close the"
            " gap, because a run can repoint it between the reread and the removal, and taking"
            " away a link that has become live breaks every registered command that goes"
            " through it. pointer.remove exists for exactly that reason -- it refuses a link"
            " that has stopped naming what its caller placed")
    return answer


def _pointer_finding(pointer_path, recorded_pointer, destination):
    """Whether the pointer is a residue, which needs OWNERSHIP and not only shape.

    pointer.read establishes what is at the path; it does not establish whose it is. A link this
    command never placed is somebody else's, and naming it in a cleanup list is how another
    tool's link gets removed -- which is exactly why pointer.remove refuses one. So a dangling
    link reaches residualPaths only when the host record positively records that path as the
    pointer this command owns. A dangling link the record does not claim is reported under its
    own name and left alone.

    It also has to be THIS destination's pointer. Diagnosis prefers the RECORDED pointer when
    classifying a runtime, and that pointer can sit under a different destination from the one
    --dest named; a survey rooted here would then have published a cleanup path belonging to
    another installation while claiming to describe this one.
    """
    if not pointer_path:
        return {"path": None, "finding": NOT_SCANNED, "residual": False,
                "detail": "no pointer was named to read"}
    path = str(Path(pointer_path))
    if destination is not None and not _same_directory(Path(path).parent, destination):
        return {"path": path, "finding": POINTER_OUTSIDE_DESTINATION, "residual": False,
                "destination": str(destination),
                "detail": ("this pointer sits under " + str(Path(path).parent) + " and this"
                           " survey describes " + str(destination) + ", so it belongs to"
                           " another installation and nothing about it is reported here")}
    read = pointer.read(path)
    claimed = recorded_pointer is not None and str(Path(recorded_pointer)) == path
    found = {"path": path, "state": read["state"], "target": read.get("target"),
             "detail": read["detail"], "recordClaimsIt": claimed, "residual": False,
             "finding": None}
    if read["state"] != pointer.LINK:
        return found
    target = Path(read["target"])
    if not target.is_absolute():
        target = Path(path).parent / target
    there, detail = _target_exists(target)
    if there:
        return found
    if there is None:
        found["finding"] = UNREADABLE_POINTER_TARGET
        found["detail"] = (detail + ", so whether this pointer still reaches a runtime was not"
                           " established and it is reported rather than listed for removal")
        return found
    found["finding"] = DANGLING_POINTER if claimed else FOREIGN_POINTER
    found["residual"] = claimed
    found["detail"] = (
        "the pointer names " + str(read["target"]) + ", which does not exist"
        + (", and the host record records this path as this command's own pointer" if claimed
           else ". The host record does not record this path as this command's pointer, so"
                " whose link it is was not established and it is reported rather than listed"
                " for removal"))
    return found
