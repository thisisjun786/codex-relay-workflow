"""The owned pointer: one stable path that names whichever runtime is selected.

An update always builds a new environment, because the directory name comes from the source
digests. Without an indirection the Codex registration would have to be rewritten every time,
and this repository will not rewrite a configuration it did not author: the registration is
append-only and proves it preserved everything by requiring the prior content to be an exact
prefix of the new file, which an in-place edit cannot satisfy. So the registration names this
pointer instead, once, and an update moves the pointer.

The pointer is a way to REACH a runtime and never an identity. A console script keeps the
absolute shebang pip wrote, so a process started through the pointer reports the concrete
environment as its own prefix and executable, and a bridge Codex already spawned goes on
running the environment it started in after the pointer moves. That is what keeps a live
process alive across an update, and it holds only because no predecessor is ever removed.

Reading it needs its own partition. The record reader follows a link and then refuses anything
that is not a regular file, so a working directory symlink would be reported as an unreadable
record -- an answer about the wrong thing entirely.
"""

import os
import tempfile
from pathlib import Path

POINTER_NAME = "current"

# What is at the pointer path. Four answers, decided by an ordered observation.
NO_POINTER = "NO_POINTER"
LINK = "LINK"
NOT_A_LINK = "NOT_A_LINK"
UNREACHABLE = "UNREACHABLE"
POINTER_STATES = (NO_POINTER, LINK, NOT_A_LINK, UNREACHABLE)

# The states a caller may act on. NOT_A_LINK is somebody's real directory and UNREACHABLE
# established nothing, so neither is a state this command places a pointer over.
POINTER_USABLE = (LINK, NO_POINTER)


def usable(state):
    """Whether a pointer in this state may be placed over. Asked of this module rather than
    tested against one member, because two of the four states mean 'do not touch it' for
    entirely different reasons."""
    return state in POINTER_USABLE


def pointer_path(destination):
    return Path(destination) / POINTER_NAME


def read(path):
    """What is at this path, without following the link to decide.

    lstat first, because following it answers about the target and the question here is about
    the pointer. A link whose target is missing is still a link, and reporting it as absent
    would invite placing a second one over it.
    """
    path = Path(path)
    try:
        found = os.lstat(str(path))
    except FileNotFoundError:
        return {"state": NO_POINTER, "target": None,
                "detail": "nothing exists at " + str(path)}
    except OSError as error:
        return {"state": UNREACHABLE, "target": None,
                "detail": ("whether anything exists at " + str(path) + " could not be"
                           " established: " + type(error).__name__ + ": " + str(error))}
    except ValueError as error:
        return {"state": UNREACHABLE, "target": None,
                "detail": "this path cannot name a file: " + type(error).__name__ + ": "
                          + str(error)}
    import stat as stat_module
    if not stat_module.S_ISLNK(found.st_mode):
        return {"state": NOT_A_LINK, "target": None,
                "detail": ("this path is a real file or directory, not a pointer this command"
                           " placed, so it is left exactly as it is")}
    try:
        target = os.readlink(str(path))
    except OSError as error:
        return {"state": UNREACHABLE, "target": None,
                "detail": ("the pointer is a link whose target could not be read: "
                           + type(error).__name__ + ": " + str(error))}
    return {"state": LINK, "target": str(target),
            "detail": "the pointer names " + str(target)}


def place(path, target):
    """Point this pointer at target, atomically, replacing only a link.

    A temporary link beside it and then a rename: the rename is atomic, so a reader either sees
    the old target or the new one and never a missing pointer. Renaming over a real directory
    fails rather than succeeding, which is the safe direction; renaming over an existing LINK
    succeeds whoever made it, so whether this command owns that link is established by the
    caller before this is called.

    This is the only thing in this module that writes.
    """
    path = Path(path)
    target = Path(target)
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(dir=str(path.parent), prefix=".crw-pointer-")
    os.close(handle)
    try:
        os.unlink(temporary)
        os.symlink(str(target), temporary)
        os.replace(temporary, str(path))
    except BaseException:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise
    return {"pointer": str(path), "target": str(target)}


def names(path, environment):
    """Whether the pointer names this environment. True, False, or None for could not tell.

    NO_POINTER and NOT_A_LINK are established answers: nothing there and a real directory there
    both mean no pointer names this environment. Only a reading that failed is None, because a
    pointer nobody could read says nothing about what it names, and a caller deciding whether
    to delete a runtime must be able to tell those apart.
    """
    answer = read(path)
    if answer["state"] == UNREACHABLE:
        return None
    if answer["state"] != LINK or answer["target"] is None:
        return False
    try:
        pointed = Path(answer["target"])
        if not pointed.is_absolute():
            pointed = Path(path).parent / pointed
        return pointed.resolve() == Path(environment).resolve()
    except (OSError, ValueError):
        return None
