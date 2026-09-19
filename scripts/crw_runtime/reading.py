"""What a reading of a record observed, including what it could not observe.

A record that cannot be read is an answer, not a crash. The four states below are the four
answers, and they are deliberately not interchangeable: absent means a clean host, unreadable
means something is there whose shape cannot be read, an access error means the question could
not be asked at all, and present means it was read. Collapsing any pair of them turns a
permission problem into a malformed record or a misconfigured path into a clean host.

The boundary this module provides is narrow on purpose. Only acquiring, decoding and
shape-reading a record goes inside a region. Wrapping ordinary logic would convert a genuine
defect into a refusal, which hides the defect exactly as well as a swallowed exception does,
so a refusal always carries the exception type and the source location that produced it.
"""

import contextlib
import errno
import json
import os
import stat as stat_module
from pathlib import Path

ABSENT = "ABSENT"
PRESENT = "PRESENT"
UNREADABLE = "UNREADABLE"
ACCESS_ERROR = "ACCESS_ERROR"

STATES = (ABSENT, PRESENT, UNREADABLE, ACCESS_ERROR)

# The same four states, split the way consumers actually ask about them. A consumer that wants
# to know whether a record can be proceeded on asks this module for the partition instead of
# testing one member of it: "== UNREADABLE" answers for one member and silently says yes to the
# other, which is how a permission failure reached classification as a readable configuration.
USABLE = (PRESENT, ABSENT)
UNUSABLE = (UNREADABLE, ACCESS_ERROR)


def unusable(state):
    """Whether nothing can be concluded from a reading in this state."""
    return state in UNUSABLE


# Directory readers whose answer to a subtree they cannot read is to LEAVE IT OUT. A walk that
# skips is not a walk that failed: it returns a complete, well-formed value describing a
# DIFFERENT tree, so the boundary above it sees a value and whatever compares that value
# decides on it. That is how a package digest came back as exactly the digest of the smaller
# tree, and the component was reported a fork for a subdirectory nobody could read.
#
# Every use of one in these modules is declared below with what omission means where it is
# used, because "it does not matter here" is a judgement and an undeclared judgement is the
# shape this whole family of defects is made of.
# os.walk belongs here for the same reason and is easy to miss: its default onerror is None,
# which means it discards the error and goes on, so it omits exactly the way rglob does.
# os.scandir is deliberately absent: it raises, which is the behaviour this list exists to
# require rather than to forbid.
OMITTING_READERS = ("rglob", "glob", "iterdir", "walk")

OMISSION_DECLARED = {
    "store_places": "the root is listed first and unconditionally, so a listing that cannot be"
                    " read loses the scopes under it and never the root itself",
    "filesystem_candidates": "a pattern that cannot be listed drops that pattern's matches;"
                             " every entry says it was listed rather than identified, and the"
                             " relay owns the judgement about what a store is",
    "directory_occupied": "an unreadable listing answers None, which is stated to be different"
                          " from an empty directory and is never read as one",
}


# Derived from the exception lattice rather than enumerated, because enumerating it is what
# let two paths escape: ValueError covers UnicodeDecodeError, json.JSONDecodeError and the
# ValueError a NUL-bearing string raises from Path.resolve; LookupError covers KeyError and
# IndexError; OSError covers permission, loop and I/O failures.
SHAPE_FAILURES = (TypeError, AttributeError, LookupError, ValueError)
READ_FAILURES = SHAPE_FAILURES + (OSError,)

# Resolving a path is the one read whose failure type depends on the interpreter. Below 3.11
# pathlib raises RuntimeError("Symlink loop from ...") where 3.11 and later raise
# OSError(ELOOP), so a caller that catches only OSError answers with a reading on one
# interpreter and dies with a traceback on the other -- and this repository's floor is 3.10.
# Declared here so both resolve sites ask for the same set rather than each remembering the
# difference. RuntimeError is NOT in READ_FAILURES: swallowing it around ordinary logic would
# hide real defects, and it belongs only where a path is being resolved.
RESOLVE_FAILURES = READ_FAILURES + (RuntimeError,)


def same_directory(one, other):
    """Whether two spellings name the same directory, established by asking the filesystem.

    os.path.samefile stats both and compares the device and inode the kernel reports, so it
    answers the question the kernel would answer: an alias of this destination is this
    destination, and 'link/..' lands where the link actually pointed rather than cancelling.
    It also raises when either path is not traversable, which is the half that pure string
    work cannot do.

    Four attempts reached this, and the three that failed are worth keeping written down
    because each was wrong in a way the next one reintroduced:

      - raw strings made ONE directory into two whenever the spelling differed, and an owned
        dangling pointer in the surveyed destination was disowned;
      - lexically normalised strings made TWO directories into one, because normpath cancels
        'X/..' without knowing X is a symlink;
      - requiring both forms to agree brought the first failure back for any path combining an
        alias with '..';
      - resolution alone fixed that and still collapsed 'missing/..', because realpath is
        best-effort: it equated a destination the kernel answers ENOENT for with a real one,
        so a pointer could be claimed for a destination that was never scanned.

    Every one of those was a STRING answering a question about the filesystem. This asks the
    filesystem. A failure establishes nothing and is answered "different", which keeps an
    unverified pointer out of a cleanup list -- the direction it fails in on purpose.

    Ask this only where BOTH spellings are being read together. Where one side was read
    EARLIER, ask path_identity instead: re-statting a stored spelling asks about whatever it
    names now, which is not the question the earlier reading answered.
    """
    try:
        return os.path.samefile(str(one), str(other))
    except (OSError, ValueError):
        return False


def path_identity(path):
    """The identity the kernel gives a path NOW, as a value a later comparison can be pinned to.

    same_directory asks whether two spellings name one thing at the moment it is called. That is
    the wrong question when one side was read earlier: a symlink retargeted in between matches
    its NEW target, and a caller comparing against the stored spelling would then reuse a
    reading taken from the old one -- a wrong reading rather than a missing one, which is the
    failure this module exists to avoid.

    It lives here beside same_directory because both are the same subject: what the kernel, and
    not a string, says two paths are. A destination a survey describes and a journal a
    registration records through are both named by spellings that can be aliases.

    None where the kernel could not answer, and a None must never compare equal to anything: an
    identity that was not established may not collapse two readings into one.
    """
    try:
        found = os.stat(str(path))
    except (OSError, ValueError):
        return None
    return (found.st_dev, found.st_ino)


def _descriptor_identity(opened):
    """The identity of the object a descriptor is open on, which nothing can retarget.

    path_identity answers for a SPELLING at the moment it is asked, which is all a lookup can
    claim and is enough to ASK whether a reading already taken covers this spelling. It is not
    enough to PUBLISH one under, and that asymmetry is the whole point of having both.
    """
    try:
        found = os.fstat(opened.fileno())
    except (OSError, ValueError):
        return None
    return (found.st_dev, found.st_ino)


class Reading:
    """A value and the state of the attempt that produced it."""

    def __init__(self, value=None, state=PRESENT, *, exception=None, source=None,
                 at=None, detail=None, field=None, identity=None):
        self.value = value
        self.state = state
        self.exception = exception
        self.source = None if source is None else str(source)
        self.at = at
        self.detail = detail
        self.field = field
        # What the kernel called the object these BYTES came from, taken from the descriptor
        # they were read through rather than from a second lookup of the path. A caller that
        # wants to know whether two spellings named one file cannot ask a path for that: a link
        # retargeted between the lookup and the open files the bytes under an identity they
        # never came from, which is a wrong reading rather than a missing one. None wherever it
        # was not established, and a None must never compare equal to anything.
        self.identity = identity

    @property
    def ok(self):
        """It was read. An existing record with nothing in it is still read."""
        return self.state == PRESENT

    @property
    def usable(self):
        """Read, or established to be absent. Absence is a usable answer; failure is not."""
        return self.state in USABLE

    def refusal(self):
        """What a refusal reports. Never the record's contents, only where and what failed."""
        return {"state": self.state, "source": self.source, "exception": self.exception,
                "raisedAt": self.at, "field": self.field, "detail": self.detail}

    def raise_if_unusable(self):
        if not self.usable:
            raise Refused(self)
        return self.value


class Refused(Exception):
    """Raised out of a reading region, carrying the reading that failed."""

    def __init__(self, reading):
        super().__init__(reading.detail or reading.state)
        self.reading = reading


def where(error):
    """The frame that actually raised, so a code defect stays locatable in the refusal.

    Without this a genuine defect reaching a boundary is reported as a data problem and the
    place it came from is gone. The refusal says which file and line raised it.
    """
    frame = error.__traceback__
    if frame is None:
        return None
    while frame.tb_next is not None:
        frame = frame.tb_next
    return os.path.basename(frame.tb_frame.f_code.co_filename) + ":" + str(frame.tb_lineno)


def failure(error, *, source, what, field=None, detail=None):
    """Classify a raised exception. An OSError could not establish anything; the rest read
    something and could not make sense of it."""
    state = ACCESS_ERROR if isinstance(error, OSError) else UNREADABLE
    said = type(error).__name__ + ": " + str(error)
    return Reading(state=state, exception=type(error).__name__, source=source,
                   at=where(error), field=field,
                   detail=(detail or ("could not read " + what)) + " (" + said + ")")


@contextlib.contextmanager
def region(source, what, field=None):
    """The boundary: acquiring, decoding and shape-reading a record, and nothing else.

    Everything inside is a read. Classification, mutation, writing, subprocess execution and
    result assembly stay outside, so a ValueError or LookupError from ordinary logic keeps
    raising instead of being reported as an unreadable record.
    """
    try:
        yield
    except Refused:
        raise
    except READ_FAILURES as error:
        raise Refused(failure(error, source=source, what=what, field=field)) from error


def _kind(mode):
    for predicate, name in ((stat_module.S_ISDIR, "directory"), (stat_module.S_ISSOCK, "socket"),
                            (stat_module.S_ISFIFO, "named pipe"), (stat_module.S_ISBLK, "block device"),
                            (stat_module.S_ISCHR, "character device")):
        if predicate(mode):
            return name
    return "not a regular file"


def observe(path, what):
    """Steps 1 and 2 of the ordered partition: what is at this path.

    Returns a settled Reading, or None when a regular file is there to be read. ABSENT is
    only returned for established absence: a failure to establish existence is an access
    error, because 'the check failed' and 'there is nothing there' are different answers.
    """
    try:
        found = os.lstat(str(path))
    except FileNotFoundError:
        return Reading(state=ABSENT, source=path, detail="nothing exists at " + str(path))
    except OSError as error:
        return failure(error, source=path, what=what,
                       detail="whether anything exists at this path could not be established")
    except ValueError as error:
        # A NUL-bearing string cannot name a path at all, so nothing was established either.
        return failure(error, source=path, what=what, detail="this path cannot name a file")

    if stat_module.S_ISLNK(found.st_mode):
        try:
            found = os.stat(str(path))
        except FileNotFoundError:
            return Reading(state=UNREADABLE, exception="FileNotFoundError", source=path,
                           detail="a symbolic link whose target does not exist")
        except OSError as error:
            if error.errno == errno.ELOOP:
                return Reading(state=UNREADABLE, exception=type(error).__name__, source=path,
                               detail="a symbolic link that loops")
            # Neither a dangling link nor a loop: the target could not be resolved, which
            # establishes nothing about it.
            return failure(error, source=path, what=what,
                           detail="a symbolic link whose target could not be resolved")

    if not stat_module.S_ISREG(found.st_mode):
        return Reading(state=UNREADABLE, source=path,
                       detail="this path is a " + _kind(found.st_mode) + ", not a regular file")
    return None


def read_json(path, what, *, absent=None, shape=None):
    """Read one JSON record, returning a Reading rather than a sentinel.

    'absent' is the value an established absence carries, so a caller can start from an empty
    record without that being mistaken for one it read. 'shape' is called with the parsed
    value and may raise to reject a shape the caller cannot use.
    """
    settled = observe(path, what)
    if settled is not None:
        if settled.state == ABSENT:
            settled.value = absent() if callable(absent) else absent
        return settled
    try:
        with region(path, what):
            # Opened ONCE, and the identity taken from that descriptor. Reading the bytes and
            # then asking the path what it is are two lookups, and a link retargeted between
            # them answers for a file these bytes did not come from -- so a caller merging two
            # spellings on that identity would reuse a reading taken from somewhere else.
            # A descriptor cannot be retargeted, so there is no interval left to race.
            with open(str(path), "rb") as opened:
                identity = _descriptor_identity(opened)
                value = json.loads(opened.read().decode("utf-8"))
            if shape is not None:
                shape(value)
    except Refused as refused:
        return refused.reading
    return Reading(value=value, state=PRESENT, source=path, identity=identity)


def read_text(path, what, *, absent=""):
    """Read one text file through the same partition, for the config reader."""
    settled = observe(path, what)
    if settled is not None:
        if settled.state == ABSENT:
            settled.value = absent
        return settled
    try:
        with region(path, what):
            value = Path(str(path)).read_text(encoding="utf-8")
    except Refused as refused:
        return refused.reading
    return Reading(value=value, state=PRESENT, source=path)
