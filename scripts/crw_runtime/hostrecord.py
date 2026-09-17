"""The private half of the definition: what is installed here, and what was exercised here.

OPS-3.2 makes a real record a private receipt, so this file never lives in the repository.
It holds the host facts the committed definition deliberately omits, and the measured points
without which OPS-2.2 can never reach 'own'.

A point is bound evidence, not an expectation: it names the install location it covers, the
combination it ran under, and the source digest it was measured against. That binding is what
keeps this file from becoming a second compatibility definition that could drift from the
committed one.
"""

import json
import os
import socket
import tempfile
import time
from pathlib import Path

RECORD_NAME = "host-record.json"


def state_home(env=None):
    env = os.environ if env is None else env
    base = env.get("XDG_STATE_HOME")
    return Path(base).expanduser() if base else Path(env.get("HOME", "~")).expanduser() / ".local/state"


def record_path(env=None):
    return state_home(env) / "codex-relay-workflow" / RECORD_NAME


def empty(definition_version):
    return {
        "recordVersion": 1,
        "definitionVersion": definition_version,
        "host": socket.gethostname(),
        "user": os.environ.get("USER") or "",
        "components": {},
    }


def load(path, definition_version):
    path = Path(path)
    if not path.is_file():
        return empty(definition_version)
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        # An unreadable record is reported by the caller as an unread signal. It is never
        # replaced silently, because overwriting it would destroy the only evidence that
        # anything was ever exercised on this host.
        return None


def save(path, record):
    """Atomic, so an interrupted write cannot leave a truncated record behind."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(dir=str(path.parent), prefix=".host-record-")
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as stream:
            json.dump(record, stream, indent=2, sort_keys=True)
            stream.write("\n")
        os.replace(temporary, str(path))
    except BaseException:
        Path(temporary).unlink(missing_ok=True)
        raise


def component(record, name):
    return record.setdefault("components", {}).setdefault(
        name, {"installs": [], "measuredPoints": []}
    )


def put_install(record, name, install):
    """Replace the entry for this location, or add it. One location, one current reading."""
    entry = component(record, name)
    entry["installs"] = [i for i in entry["installs"] if i.get("location") != install["location"]]
    entry["installs"].append(install)
    return install


def add_point(record, name, point):
    """Append. A point is never replaced, because a later reading does not unmake an
    earlier run; it only adds another combination that was observed."""
    component(record, name)["measuredPoints"].append(point)
    return point


def points_for(record, name, *, location, interpreter, install_digest,
               codex_cli=None, app_server=None, host=None):
    """Points that actually cover this install, this interpreter and these bytes.

    All three have to match. A point recorded for another interpreter is a different
    combination (OPS-1.3), and a point recorded against different bytes says nothing about
    the ones installed now.
    """
    found = []
    for point in component(record, name).get("measuredPoints", []):
        if not point.get("exercised"):
            continue
        if point.get("install") != location:
            continue
        if point.get("interpreter") != interpreter:
            continue
        # The digest of the bytes that were actually exercised, measured at that time.
        # A point written before this existed carries none and can never qualify, because
        # nothing in it says which bytes the run covered.
        if not point.get("installDigest") or point.get("installDigest") != install_digest:
            continue
        # The rest of the combination counts too. A point recorded against another Codex
        # CLI, another App Server or another host describes a run that is not this one,
        # and reusing it would authorize an installation nobody exercised here (OPS-1.3).
        for field, wanted in (("codexCli", codex_cli), ("appServer", app_server), ("host", host)):
            if wanted is not None and point.get(field) != wanted:
                break
        else:
            found.append(point)
    return found

# --------------------------------------------------------------- shared safe writing

LOCK_SUFFIX = ".crw-lock"
STALE_LOCK_SECONDS = 300


def atomic_write(path, text):
    """Write by temp file and replace, so an interrupted write cannot truncate the target.

    os.replace replaces a symlink rather than following it, which plain write_text does not.
    That difference is deliberate here: the file this command owns is the path it was given.
    """
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(dir=str(path.parent), prefix=".crw-write-")
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as stream:
            stream.write(text)
        os.replace(temporary, str(path))
    except BaseException:
        Path(temporary).unlink(missing_ok=True)
        raise


class Locked:
    """An exclusive lock file around a read-modify-write.

    What this covers: two runs of these commands cannot interleave their own
    read-modify-write, and the write itself cannot leave a truncated file.

    What it does not cover: an editor that does not take this lock. A concurrent writer that
    ignores it can still land between the last read and the replace. That limit is stated
    rather than papered over, because calling this compare-and-swap would claim a guarantee
    it does not have.
    """

    def __init__(self, target, timeout=10.0):
        self.path = Path(str(target) + LOCK_SUFFIX)
        self.timeout = timeout
        self.handle = None

    def __enter__(self):
        deadline = time.time() + self.timeout
        while True:
            try:
                self.path.parent.mkdir(parents=True, exist_ok=True)
                self.handle = os.open(str(self.path), os.O_CREAT | os.O_EXCL | os.O_WRONLY)
                os.write(self.handle, str(os.getpid()).encode())
                return self
            except FileExistsError:
                # A lock left by a process that died would otherwise block for ever.
                try:
                    age = time.time() - self.path.stat().st_mtime
                except OSError:
                    age = 0
                if age > STALE_LOCK_SECONDS:
                    self.path.unlink(missing_ok=True)
                    continue
                if time.time() > deadline:
                    raise TimeoutError("another run holds " + str(self.path))
                time.sleep(0.05)

    def __exit__(self, *exc):
        # Released on every path, including an exception, so a failure cannot strand the lock.
        if self.handle is not None:
            os.close(self.handle)
            self.handle = None
        self.path.unlink(missing_ok=True)
        return False
