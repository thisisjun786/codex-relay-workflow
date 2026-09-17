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

from . import reading

RECORD_NAME = "host-record.json"

# Dimensions a point must carry and a caller must supply. Optional dimensions may be absent
# on either side; these may not, because an absent one compared as "no constraint" is how a
# point recorded under an unknown Codex CLI came to match every Codex CLI.
MANDATORY_DIMENSIONS = ("codexCli", "host")


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


def shape(record):
    """Reject a record whose containers are not what every consumer here assumes.

    This is the explicit half of the guarantee: the boundary makes the worst case a refusal,
    and this makes the message say which container was wrong. It checks containers only,
    because a field whose absence has a defined meaning is interpreted rather than refused.
    """
    if not isinstance(record, dict):
        raise TypeError("a host record is an object, found " + type(record).__name__)
    components = record.get("components")
    if components is not None and not isinstance(components, dict):
        raise TypeError("components is an object, found " + type(components).__name__)
    for name, entry in (components or {}).items():
        if not isinstance(entry, dict):
            raise TypeError("component " + str(name) + " is an object, found "
                            + type(entry).__name__)
        for key in ("installs", "measuredPoints"):
            value = entry.get(key)
            if value is not None and not isinstance(value, list):
                raise TypeError(str(name) + "." + key + " is a list, found "
                                + type(value).__name__)
            for item in value or []:
                if not isinstance(item, dict):
                    raise TypeError("every entry in " + str(name) + "." + key
                                    + " is an object, found " + type(item).__name__)
    selected = record.get("selected")
    if selected is not None and not isinstance(selected, dict):
        raise TypeError("selected is an object, found " + type(selected).__name__)
    return record


def load(path, definition_version):
    """Read the record as a reading, never as a sentinel.

    An unreadable record is never replaced silently, because overwriting it would destroy the
    only evidence that anything was ever exercised on this host. Which kind of failure it was
    travels with the reading: a parse failure and a permission failure are different answers
    and a caller that cannot tell them apart cannot report either one honestly.
    """
    return reading.read_json(path, "the host record",
                             absent=lambda: empty(definition_version), shape=shape)


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
        # Bytes that disagree with the definition are the ones classification calls a fork,
        # so a point measured against them can never authorize reuse. Recorded as "not
        # false" rather than "true" so a point written before this field existed stays
        # readable as the non-qualifying evidence it already was.
        if point.get("digestMatchesDefinition") is False:
            continue
        # The rest of the combination counts too. A point recorded against another Codex
        # CLI, another App Server or another host describes a run that is not this one,
        # and reusing it would authorize an installation nobody exercised here (OPS-1.3).
        #
        # MANDATORY dimensions have to be readable on BOTH sides. A null on either side used
        # to mean "do not compare", so a point that recorded nothing about the Codex CLI
        # matched every CLI instead of none -- the widest possible answer from the least
        # possible evidence.
        for field in MANDATORY_DIMENSIONS:
            wanted = {"codexCli": codex_cli, "host": host}[field]
            if wanted is None or point.get(field) is None or point.get(field) != wanted:
                break
        else:
            if app_server is not None and point.get("appServer") != app_server:
                continue
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


# ------------------------------------------------------------------ the one way to write

def update(path, definition_version, *, installs=None, points=None, select=None,
           component_facts=None, outgoing=None, drop_environment=None):
    """Apply narrow deltas to state this helper loads itself, inside the lock, at write time.

    The helper never accepts a record, and that is the whole point. A caller that loads a
    record, spends minutes installing and exercising a runtime, and then hands the record back
    to be saved will overwrite whatever another run committed in between. A lock around that
    save does not help, because the staleness is already inside the value being written. So a
    caller says what it learned -- this install, these points, this selection -- and the merge
    happens here against the record as it stands.

    'drop_environment' is the recovery delta: it removes the install records this run created
    and leaves the selection exactly as found, because another run's successful promotion is
    not this run's to undo.

    Returns the Reading it loaded, so a caller can report an unreadable record rather than
    guess. Nothing is written when the record could not be read.
    """
    with Locked(path):
        current = load(path, definition_version)
        if not current.usable:
            return current
        record = current.value
        for name, install in (installs or []):
            put_install(record, name, install)
        for name, point in (points or []):
            add_point(record, name, point)
        for name, facts in (component_facts or {}).items():
            component(record, name).update(facts)
        if drop_environment is not None:
            for name in list(record.get("components") or {}):
                entry = record["components"][name]
                entry["installs"] = [i for i in entry.get("installs") or []
                                     if i.get("environment") != str(drop_environment)]
        if outgoing is not None:
            record["outgoing"] = outgoing
        if select:
            # Only the assignments this run made. A whole selection map would carry back
            # entries the caller read before its slow work and re-assert them as current.
            record.setdefault("selected", {}).update(select)
        save(path, record)
    return current


def _under(location, environment):
    """Whether a recorded location is that environment or lives inside it.

    Containment over resolved parts, not a string prefix: /opt/env-other shares the text of
    /opt/env and is a different place.
    """
    try:
        candidate, root = Path(str(location)).resolve(), Path(str(environment)).resolve()
    except (OSError, ValueError):
        return False
    return candidate == root or root in candidate.parents


def release_candidate(path, definition_version, environment):
    """Drop the install records for an environment, unless it is the selected one.

    Returns (reading, decision). The decision is READ from the record under the lock rather
    than remembered from a flag, and that distinction is the whole point: update() saves
    inside the lock and releasing the lock can still raise afterwards, so a run can have
    committed its promotion and raised anyway. A caller concluding "the call did not return,
    so nothing was promoted" would then delete a runtime that is now selected.

    Absence of evidence is not permission either. When the record cannot be read the
    candidate is kept, because an unreadable record says nothing about what is selected.
    """
    with Locked(path):
        current = load(path, definition_version)
        if not current.usable:
            return current, ("kept: the record could not be read, so nothing about the"
                             " selection could be established")
        record = current.value
        selected = record.get("selected") or {}
        if any(_under(location, environment) for location in selected.values() if location):
            return current, ("kept: this environment is the selected one, so the run that"
                             " promoted it committed before it failed")
        for name in list(record.get("components") or {}):
            entry = record["components"][name]
            entry["installs"] = [i for i in entry.get("installs") or []
                                 if i.get("environment") != str(environment)]
        save(path, record)
    return current, "dropped: the selection does not name this environment"
