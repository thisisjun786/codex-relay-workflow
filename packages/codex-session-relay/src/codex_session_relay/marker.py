"""The managed marker: create-once facts on a filesystem the relay database never sees.

A hook that only inspects registered relationships cannot see a missing registration. The marker
therefore exists BEFORE the relationship and is checkable without asking the relay anything, which
is the whole reason it is not a table: a question about a task nobody registered has to stay
answerable when there is no row to read.

Every fact is published once and never updated in place. Publication writes a sibling temp file,
fsyncs it, links it onto the target and unlinks the temp, so first-publication-wins is an operating
system fact rather than a convention a writer might forget. A writer that dies mid-write leaves an
orphan temp and no target, so readers see the fact as absent; two writers racing produce exactly one
EEXIST, so the loser learns synchronously that it lost instead of silently discarding the winner.

Identity here is deliberately strict. Missing, empty, blank and non-string values all name nothing,
and two records that name nothing are never a match. Stated that way because that is the form the
rule gets violated in: None == None is not evidence.

The layout and every rule in this module are fixed by skills/crw-run/references/hook-contract.md.
"""

import hashlib
import json
import os
import re
import uuid
from dataclasses import dataclass
from pathlib import Path

MARKER_ENV = "CODEX_SESSION_RELAY_MARKER_ROOT"
PRECEDENCE = ("flag", "env", "xdg", "home")
DIRECTORY_NAME = "codex-session-marker"

PUBLISHED = "published"
EXISTS = "exists"

# The three single facts, and the three directories of numbered facts. Claims and dispositions are
# keyed by the writer's own session instead, because a writer publishes inside a directory it owns.
SINGLE_FACTS = {"intent": "intent.json", "bound": "bound.json", "relationship": "relationship.json"}
NUMBERED_FACTS = ("attempts", "conflicts", "resolutions")

CLAIM_FILE = "claim.json"

# An assignment id is the hex sha256 of a dispatch request id and nothing else. Validated rather
# than trusted, because the value reaches this module from a command line: a mistyped id would
# otherwise publish facts into a directory no reader can ever select, and a crafted one carrying
# path separators or a parent reference would publish them outside the workspace it names.
ASSIGNMENT_RE = re.compile(r"^[0-9a-f]{64}$")


def valid_assignment(assignment) -> bool:
    return bool(ASSIGNMENT_RE.match(str(assignment or "")))


@dataclass(frozen=True)
class MarkerSelection:
    """Which rule chose the marker root, and the exact value that won.

    Carried the way store.StateSelection carries its own reason: a participant that cannot say
    which rule applied to it cannot be compared with another participant.
    """

    path: Path
    source: str
    detail: str

    def to_record(self) -> dict:
        return {
            "path": str(self.path),
            "source": self.source,
            "detail": self.detail,
            "precedence": list(PRECEDENCE),
        }


def resolve_marker_root(explicit=None) -> MarkerSelection:
    """Highest precedence first: an explicit root, the environment, XDG, then the home default.

    Deliberately a DIFFERENT directory from the relay state directory. The contract gives the relay
    daemon no access to the marker at all, and a root nested inside the store's directory would make
    that separation a matter of good behaviour rather than of layout.
    """
    if explicit:
        return MarkerSelection(
            Path(explicit).expanduser().absolute(), "flag", "--marker-root " + str(explicit)
        )
    override = os.environ.get(MARKER_ENV)
    if override:
        return MarkerSelection(
            Path(override).expanduser().absolute(), "env", MARKER_ENV + "=" + override
        )
    xdg = os.environ.get("XDG_STATE_HOME")
    if xdg:
        base, source, detail = Path(xdg).expanduser(), "xdg", "XDG_STATE_HOME=" + xdg
    else:
        base, source = Path.home() / ".local" / "state", "home"
        detail = "default under " + str(Path.home() / ".local" / "state")
    return MarkerSelection((base / DIRECTORY_NAME).absolute(), source, detail)


def named(value) -> bool:
    """Whether a record actually names an identity.

    Missing, empty, blank and non-string values all name nothing, and collapsing them to one answer
    is the point: two records that name nothing must never compare equal, which is exactly what
    None == None quietly does.
    """
    return isinstance(value, str) and bool(value.strip())


def same_identity(left, right) -> bool:
    """Do two records name the same identity? Unnamed on either side is never a match."""
    return named(left) and named(right) and left == right


def workspace_key(workspace) -> str:
    """One directory per workspace, keyed on the resolved path.

    realpath rather than the spelling handed to the hook, so a symlinked or relative cwd reaches the
    same assignment the coordinator declared against rather than a fresh empty one.
    """
    return hashlib.sha256(str(Path(workspace).expanduser().resolve()).encode("utf-8")).hexdigest()


def assignment_id(dispatch_request_id: str) -> str:
    """The assignment directory name: the hash of the dispatch request id, never the id itself.

    Storing it in the clear would make correlation meaningless, because any session able to read the
    directory could then present it. The child holds the preimage from its own dispatch.
    """
    return hashlib.sha256(str(dispatch_request_id).encode("utf-8")).hexdigest()


def workspace_dir(root, workspace) -> Path:
    return Path(root) / workspace_key(workspace)


def assignment_dir(root, workspace, assignment) -> Path:
    """A workspace path outlives the assignment that used it, so the assignment owns a level.

    Named by the workspace alone, a reused worktree would resolve to the previous assignment: every
    fact there is create-once, so the new coordinator's writes would all fail EEXIST while the new
    child read the previous session's bind and released. The undeclared turn held on a fresh
    workspace would go unheld on a reused one.
    """
    return workspace_dir(root, workspace) / _checked_assignment(assignment)


def _checked_assignment(assignment) -> str:
    if not valid_assignment(assignment):
        raise ValueError(
            "an assignment id is the hex sha256 of a dispatch request id, not " + repr(assignment)
        )
    return str(assignment)


def fact_digest(payload: dict) -> str:
    """SHA-256 over the fact's JSON with factId removed, sorted keys, no whitespace, ASCII escaped.

    The exact spelling matters because an independent writer must reproduce it byte for byte.
    Pinned by the contract's reproducible vector, which the tests assert.
    """
    body = {key: value for key, value in payload.items() if key != "factId"}
    return hashlib.sha256(_canonical(body).encode("utf-8")).hexdigest()


def _canonical(payload) -> str:
    return json.dumps(payload, sort_keys=True, separators=(",", ":"))


def _fsync_directory(directory) -> None:
    """A linked name is not durable until its directory is.

    Best effort on purpose: some filesystems refuse to open a directory for fsync, and failing the
    publication over that would turn a durability improvement into an availability regression. The
    link itself already decided who won.
    """
    try:
        descriptor = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    except OSError:
        pass
    finally:
        os.close(descriptor)


def publish(target, payload: dict) -> str:
    """Create-once publication. Returns 'published' when this writer won, 'exists' when it lost.

    The temp sibling is what forces the writable unit to be the DIRECTORY rather than the single
    name, which is why a claim lives at claims/<session>/claim.json and not claims/<session>.json.
    Its name is dot-prefixed so a reader walking a fact directory never mistakes a half-written temp
    for a fact.

    Losing is returned rather than raised because it is an ordinary outcome: a replayed bind of the
    same identity is a no-op, and only the caller knows whether the value it lost to agrees.
    """
    target = Path(target)
    directory = target.parent
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    temp = directory / (
        "." + target.name + ".tmp." + str(os.getpid()) + "." + uuid.uuid4().hex[:12]
    )
    descriptor = os.open(temp, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    try:
        os.write(descriptor, _canonical(payload).encode("utf-8"))
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    try:
        os.link(temp, target)
        outcome = PUBLISHED
    except FileExistsError:
        outcome = EXISTS
    finally:
        try:
            os.unlink(temp)
        except OSError:
            pass
    _fsync_directory(directory)
    return outcome


def _read_fact(path):
    """Read one fact. Returns (value, readable). Unreadable is never reported as absent."""
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError:
        return None, False
    try:
        return json.loads(text), True
    except ValueError:
        # Parsed nothing, so nothing is known about this fact. That is "I could not read it",
        # which must not be reported as "it is not there".
        return None, False


def _identified(value, fact_id):
    """Attach the factId the READER walked to.

    Never copied out of a body: a factId a writer chose would prove nothing, and every coverage
    decision in the contract is keyed on this value.
    """
    if isinstance(value, dict):
        record = dict(value)
        record["factId"] = fact_id
        return record
    # Left exactly as it is. A fact that is not a record has to reach the malformed check as the
    # wrong shape; coercing it here would hide the one thing the reader must report.
    return value


def read_assignment(directory):
    """Every published fact in one assignment, plus the labels of anything that could not be read.

    A reader arriving mid-race sees a subset of files, which is always a valid earlier state rather
    than a corrupt one, so absence is never an error here. Unreadability is, and it is returned
    separately because "I could not look" and "there is nothing there" are different answers.
    """
    directory = Path(directory)
    marker, unreadable = {}, []

    for key, name in SINGLE_FACTS.items():
        path = directory / name
        if not path.exists():
            continue
        value, readable = _read_fact(path)
        if not readable:
            unreadable.append(key)
            continue
        marker[key] = _identified(value, key)

    for key in NUMBERED_FACTS:
        sub = directory / key
        if not sub.is_dir():
            continue
        items = []
        for entry in sorted(sub.glob("*.json")):
            if entry.name.startswith("."):
                continue
            value, readable = _read_fact(entry)
            if not readable:
                unreadable.append(key + "/" + entry.stem)
                continue
            items.append(_identified(value, key + "/" + entry.stem))
        marker[key] = items

    claims_dir = directory / "claims"
    if claims_dir.is_dir():
        claims = []
        for session_dir in sorted(p for p in claims_dir.iterdir() if p.is_dir()):
            path = session_dir / CLAIM_FILE
            if not path.exists():
                continue
            value, readable = _read_fact(path)
            fact_id = "claims/" + session_dir.name + "/" + CLAIM_FILE
            if not readable:
                unreadable.append(fact_id)
                continue
            claims.append(_identified(value, fact_id))
        marker["claims"] = claims

    return marker, unreadable


def read_disposition(directory, session_id, turn_id):
    """The disposition this session recorded for this turn, if it published one.

    Read at the path the reader's own Stop identity derives rather than by searching for a body that
    matches, so a disposition published for another turn can never answer for this one.
    """
    if not (named(session_id) and named(turn_id)):
        return None, True
    path = Path(directory) / "dispositions" / session_id / (turn_id + ".json")
    if not path.exists():
        return None, True
    value, readable = _read_fact(path)
    if not readable:
        return None, False
    return _identified(value, "dispositions/" + session_id + "/" + turn_id), True


def list_assignments(root, workspace):
    """Every assignment directory declared for this workspace, oldest name first.

    Sorted by name so that every reader of the same listing walks it in the same order, which is
    what makes the contract's tie-break on assignment id reproducible.
    """
    directory = workspace_dir(root, workspace)
    try:
        return sorted(p for p in directory.iterdir() if p.is_dir())
    except OSError:
        return []
