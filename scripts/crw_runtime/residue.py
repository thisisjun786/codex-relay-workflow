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

from . import hostrecord, pointer, reading, staging

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

NOTE = ("residue is what the installer's own decision would reclaim (staging.REMOVES), so this"
        " is that decision rather than a second opinion about the same directory. It is not"
        " guaranteed to be the same SET as a later install's, and the difference is stated rather"
        " than implied: this command asks about the pointer the host RECORD names, and cmd_install"
        " asks about the destination it was invoked with. Those are the same directory on an"
        " ordinary host and not on one whose recorded pointer lies elsewhere, where this command"
        " is the conservative of the two -- it protects an environment the recorded pointer"
        " still reaches. Which of the two questions the installer should ask is a decision about"
        " the installer, and it is not this issue's to make. A failed install's residualPaths is a different"
        " reading: it is what THAT RUN left, and this is what is on the destination now."
        " It is also not a snapshot: the claim, the lock, the contents, the host record and"
        " the pointer are read at different moments, so a host changing underneath this"
        " command is described in pieces. Every decision is conservative in the same"
        " direction, so an error costs a path being kept rather than one being missed."
        " What it does NOT promise is that a listed path is still residue when this payload"
        " is read: this survey takes no lock, so an install can reclaim and rebuild a path"
        " between the reading and the reading being acted on. Entries can go stale, and a"
        " path is only safely clearable under the installer lock -- which is why the recovery"
        " text names the install rather than a removal.")


def _entry(path, **fields):
    found = {"path": str(path), "decision": None, "reason": None, "residual": False}
    found.update(fields)
    return found


# Moved to reading.same_directory once a second reader needed it: the journals a hook records
# through can be named through an alias too, and two copies of this predicate kept equal by
# hand is the shape this repository keeps removing.
_same_directory = reading.same_directory


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


def survey(destination, *, pointer_path=None, pointer_ownership=None, protection=None,
           unreadable=None):
    """Every directory under this destination, classified by staging.decide.

    'protection' is the caller's ownership reading, called with an environment path and
    answering (protected, selected). A caller that passes none made no such reading, and then
    nothing is reported as residue at all: no residue found and nobody looked are different
    answers, and the second one must never be printed as an empty cleanup list.

    'pointer_ownership' is the host record's whole pointer entry and not the path out of it.
    That object answers two questions -- which path this host's pointer IS, and whether a link
    this command PLACED is at it -- and a rollback takes the second away while keeping the
    first. Handed only the path, this survey could not tell those apart and read a preserved
    location as placement evidence.

    'unreadable' is a reading the CALLER already failed to take about this destination, carried
    into the same list as the ones taken here. A destination spelling the caller could not even
    resolve is not the same answer as no destination having been named, and reporting it as the
    second would have described a scan nobody asked for.
    """
    answer = {"destination": None if destination is None else str(destination), "read": False,
              "entries": [], "pointer": None, "residualPaths": [], "recoveryRequires": [],
              "unreadable": ([unreadable] if unreadable else []),
              "ownershipRead": protection is not None, "note": NOTE}
    if destination is None:
        # Only where the caller has not already said why there is nothing to scan. A spelling
        # it could not resolve and an omitted argument are different answers, and printing
        # both left one list contradicting itself.
        if not answer["unreadable"]:
            answer["unreadable"].append("no destination was named, so nothing was scanned")
        return answer
    root = Path(destination)
    # Built from the SURVEYED root's own spelling, because that is the spelling scandir will
    # return children in. Holding the pointer's own spelling instead missed it whenever --dest
    # was an alias of the directory the pointer sits in, and a real directory standing where
    # the pointer belongs -- carrying an abandoned claim -- was then classified RECLAIM while
    # the pointer cell beside it read NOT_A_LINK and promised it was left exactly as it is.
    # Only when the pointer is actually IN this destination. Excluding by basename alone
    # suppressed a local child that merely shared the name of a pointer recorded
    # elsewhere, and an abandoned staging sitting at that child vanished from cleanup.
    excluded = ({str(root / Path(pointer_path).name)}
                if pointer_path and _same_directory(Path(pointer_path).parent, root)
                else set())
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
        return _with_pointer(answer, pointer_path, pointer_ownership, root)
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

    return _with_pointer(answer, pointer_path, pointer_ownership, root)


def _with_pointer(answer, pointer_path, pointer_ownership, destination):
    """Read the pointer and publish it. One place, because the two exits used to differ and the
    difference was a dropped repair rather than a difference anybody intended."""
    answer["pointer"] = _pointer_finding(pointer_path, pointer_ownership, destination)
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


def _pointer_finding(pointer_path, pointer_ownership, destination):
    """Whether the pointer is a residue, which needs OWNERSHIP and not only shape.

    pointer.read establishes what is at the path; it does not establish whose it is. A link this
    command never placed is somebody else's, and naming it in a cleanup list is how another
    tool's link gets removed -- which is exactly why pointer.remove refuses one. So a dangling
    link reaches residualPaths only when the host record positively records that path as the
    pointer this command owns. A dangling link the record does not claim is reported under its
    own name and left alone.

    Ownership here is the PLACEMENT half of the record's pointer entry, asked of hostrecord
    rather than tested against a key here. The entry keeps 'path' across a failed promotion on
    purpose -- a retry has to derive the same pointer -- while a rollback that established the
    link is gone withdraws 'recordedAt' and 'recordedBy'. Reading the surviving path as
    placement evidence let a foreign dangling link appearing at that location afterwards be
    published as this command's own residue, which is the one direction this may not fail in.

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
    claimed = (hostrecord.placement_recorded(pointer_ownership)
               and str(Path(pointer_ownership["path"])) == path)
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
           else ". The host record does not record a link THIS COMMAND PLACED at this path --"
                " either it names another path, or a rollback established the link it placed"
                " is gone and withdrew the placement while keeping the path it is known by --"
                " so whose link this is was not established and it is reported rather than"
                " listed for removal"))
    return found
