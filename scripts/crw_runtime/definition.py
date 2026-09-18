"""The one compatibility definition, and the re-derivation that keeps it true.

OPS-1.1 allows exactly one definition, so this module is the only reader of
components.json and every other module asks it rather than parsing that file again.

Everything the checkout can prove about itself is re-derived here and compared, which is
what stops the committed values from drifting away from the source they describe. The two
things a checkout cannot prove are named rather than guessed: the upstream tree hash, which
the import did not bring, and the repository commit, which cannot be recorded inside itself.
"""

import hashlib
import json
import os
import re
import subprocess
from pathlib import Path

from .text import text_prefix

ROOT = Path(__file__).resolve().parents[2]
DEFINITION_PATH = Path(__file__).resolve().parent / "components.json"
PROVENANCE_PATH = "packages/README.md"

# The directory OPS-1.2 excludes from the walk. Named once so the pruning and the filter agree:
# pruning it and then still filtering on it is deliberate, because a path can carry the name
# without the walk having descended into a directory called that.
EXCLUDED_DIRECTORY = "__pycache__"


def files_under(root, skip=()):
    """Every file under a directory, with a subtree that cannot be read RAISING.

    rglob answers an unreadable subdirectory by leaving it out and raising nothing, so the
    digest built from it was a well-formed value describing a different tree -- byte for byte
    the value that tree really hashes to. The reading region around the caller saw a value, the
    comparison saw a mismatch, and the component was reported a fork. An incomplete reading is
    not a value (OPS-2.1), so the walk fails instead and the boundary reports UNREADABLE.

    The file set is the one rglob produced: recursive, not descending through a symbolic link
    to a directory, and counting a link to a file as the file it names.

    'skip' names directories to PRUNE rather than open. A directory whose contents cannot
    affect the answer must not be able to refuse it: a root-owned __pycache__ would otherwise
    turn a perfectly readable package into an unreadable one at every boundary that asks for
    its digest. Pruning is not omission -- nothing readable is dropped, and every subtree that
    can affect the answer still raises.
    """
    found, pending = [], [Path(root)]
    while pending:
        with os.scandir(str(pending.pop())) as entries:
            for entry in entries:
                if entry.is_dir(follow_symlinks=False):
                    if entry.name in skip:
                        continue
                    pending.append(Path(entry.path))
                elif entry.is_file():
                    found.append(Path(entry.path))
    return found


def ops12_digest(root):
    """SHA-256 over a package directory, exactly as OPS-1.2 defines it.

    Walk every file except anything under __pycache__, sort by POSIX-style relative path,
    and feed the hash the relative path, a zero byte, then the SHA-256 of the file's bytes.
    Defined so the standard library alone reproduces it for a source tree and for an
    installed copy of that tree.

    A subtree that cannot be read raises out of here rather than being skipped, so this never
    answers with the digest of a tree it could not see all of.
    """
    root = Path(root)
    files = []
    for path in files_under(root, skip=(EXCLUDED_DIRECTORY,)):
        if EXCLUDED_DIRECTORY in path.relative_to(root).parts:
            continue
        files.append(path)
    digest = hashlib.sha256()
    for path in sorted(files, key=lambda p: p.relative_to(root).as_posix()):
        digest.update(path.relative_to(root).as_posix().encode())
        digest.update(b"\x00")
        digest.update(hashlib.sha256(path.read_bytes()).digest())
    return digest.hexdigest()


def load(path=None):
    """Read the definition. Every caller goes through here; nobody re-parses the file."""
    return json.loads(Path(path or DEFINITION_PATH).read_text(encoding="utf-8"))


def git(args, root=None):
    """Run a read-only git command, returning None when the reading cannot be made.

    A signal that cannot be read is not a signal that agrees (OPS-2.1), so an unavailable
    git or a missing object returns None and the caller reports the failed reading rather
    than treating it as agreement.
    """
    try:
        done = subprocess.run(
            ["git", *args], cwd=str(root or ROOT), capture_output=True, text=True, timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() if done.returncode == 0 else None


def working_tree_clean(root=None):
    """Cleanliness of the WHOLE checkout, with no pathspec.

    An uncommitted change outside a package directory leaves that package's digest exactly
    equal to the recorded one while the checkout is no longer the revision the definition
    names, so narrowing this reading to the component's subdirectory would miss precisely
    the case OPS-2.1 requires it to catch.
    """
    status = git(["status", "--porcelain"], root)
    if status is None:
        return None
    return status == ""


_FIELD = re.compile(r'^\s*(version|requires-python)\s*=\s*"([^"]*)"\s*$')


def pyproject_fields(path):
    """Read version and requires-python from a pyproject's [project] table.

    Deliberately small, like this repository's other checked-in readers: it understands the
    single-line quoted form these two files actually use and nothing else. It is not a TOML
    parser, and tomllib is unavailable on the Python the repository's own checks run.
    Returns None for a field it did not find, which the caller reports as an unread signal.
    """
    found = {}
    section = None
    for line in Path(path).read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if text_prefix(stripped, "[") and text_prefix(stripped, "]", at="end"):
            section = stripped
            continue
        if section != "[project]":
            continue
        match = _FIELD.match(line)
        if match:
            found.setdefault(match[1], match[2])
    return found


def verify(root=None, definition=None):
    """Re-derive every derivable field and return one finding per disagreement."""
    root = Path(root or ROOT)
    data = definition if definition is not None else load()
    findings = []
    provenance = (root / PROVENANCE_PATH)
    provenance_text = provenance.read_text(encoding="utf-8") if provenance.is_file() else None
    if provenance_text is None:
        findings.append("cannot read " + PROVENANCE_PATH + ", so recorded provenance is unchecked")

    for component in data.get("components", []):
        name = component["component"]

        def mismatch(field, recorded, derived):
            if derived is None:
                findings.append(
                    name + ": " + field + " could not be read, so it is unchecked rather than agreed"
                )
            elif recorded != derived:
                findings.append(
                    name + ": " + field + " recorded " + repr(recorded) + " but derived " + repr(derived)
                )

        for field, subpath in (("subdirectoryTree", "subdirectory"),
                               ("packageTree", "packageLocation")):
            mismatch(field, component[field], git(["rev-parse", "HEAD:" + component[subpath]], root))

        package = root / component["packageLocation"]
        mismatch("sourceDigest", component["sourceDigest"],
                 ops12_digest(package) if package.is_dir() else None)

        pyproject = root / component["subdirectory"] / "pyproject.toml"
        fields = pyproject_fields(pyproject) if pyproject.is_file() else {}
        mismatch("version", component["version"], fields.get("version"))
        mismatch("requiresPython", component["requiresPython"], fields.get("requires-python"))

        licence = root / component["licencePath"]
        if not licence.is_file():
            findings.append(name + ": licence recorded at " + component["licencePath"] + " is absent")

        tool = component.get("identityTool")
        if tool:
            server = root / component["serverModule"]
            source = server.read_text(encoding="utf-8") if server.is_file() else None
            if source is None:
                findings.append(name + ": cannot read " + component["serverModule"])
            elif ("def " + tool) not in source:
                findings.append(
                    name + ": identityTool " + tool + " is not defined in "
                    + component["serverModule"]
                )

        revision = component["upstream"]["revision"]
        if provenance_text is not None and revision not in provenance_text:
            findings.append(
                name + ": upstream revision " + revision + " is not recorded in " + PROVENANCE_PATH
                + ", so the definition and the retained provenance disagree"
            )
    return findings
