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

# Derived from the exception lattice rather than enumerated, because enumerating it is what
# let two paths escape: ValueError covers UnicodeDecodeError, json.JSONDecodeError and the
# ValueError a NUL-bearing string raises from Path.resolve; LookupError covers KeyError and
# IndexError; OSError covers permission, loop and I/O failures.
SHAPE_FAILURES = (TypeError, AttributeError, LookupError, ValueError)
READ_FAILURES = SHAPE_FAILURES + (OSError,)


class Reading:
    """A value and the state of the attempt that produced it."""

    def __init__(self, value=None, state=PRESENT, *, exception=None, source=None,
                 at=None, detail=None, field=None):
        self.value = value
        self.state = state
        self.exception = exception
        self.source = None if source is None else str(source)
        self.at = at
        self.detail = detail
        self.field = field

    @property
    def ok(self):
        """It was read. An existing record with nothing in it is still read."""
        return self.state == PRESENT

    @property
    def usable(self):
        """Read, or established to be absent. Absence is a usable answer; failure is not."""
        return self.state in (PRESENT, ABSENT)

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
            value = json.loads(Path(str(path)).read_text(encoding="utf-8"))
            if shape is not None:
                shape(value)
    except Refused as refused:
        return refused.reading
    return Reading(value=value, state=PRESENT, source=path)


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
