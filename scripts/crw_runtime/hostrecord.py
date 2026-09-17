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


def points_for(record, name, *, location, interpreter, source_digest,
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
        if point.get("definitionDigest") != source_digest:
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

