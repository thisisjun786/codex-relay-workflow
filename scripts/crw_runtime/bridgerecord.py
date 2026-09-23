"""The record a plugin-declared MCP server reads, because the package cannot resolve a host.

The Codex configuration is where an MCP server is registered, and that registration names a
command. A plugin declares its server in its own manifest instead, and that declaration ships
inside a package that knows nothing about the machine it lands on: it cannot name the pointer
the runtime installer owns, and the process it starts inherits neither CODEX_HOME nor the
plugin root.

So the two halves are split. The package supplies the launcher and the declaration; this file
supplies the one fact the launcher cannot derive, written by the command that would otherwise
have written the configuration entry.

One owner registers this server. The configuration entry and this record are the two owners,
they are refused against each other, and the record carries the owner so the launcher can
stand down at run time as well.

A plugin-owned record may also name the host's execution policy. The launcher Codex spawns
inherits the App Server's bare environment, so without this the bridge it starts can read no
policy at all and checks no role. The record names the FILE and the digest register-mcp read it
under, and never what the file says: the bridge still parses the policy itself, and the digest is
what lets the launcher and the bridge refuse a file that changed after it was registered.
"""

import hashlib
import json
import os
import re
import stat as stat_module
from pathlib import Path

from . import hostrecord, reading

# This record's own file, beside the completion hook's and never inside it. Sharing one would
# make two owners of one document, and an operator changing the MCP pointer would be editing
# the settings a Stop hook reads.
RECORD_NAME = "crw-bridge-mcp.json"
RECORD_VERSION = 1
# A record that names an execution policy is its own version rather than version 1 with one more
# key. A launcher that implements only version 1 refuses a version it does not read; handed an
# extra key under the old number it would start the bridge without the policy and say nothing,
# which is the silent presence-only start this field exists to end.
POLICY_RECORD_VERSION = 2
RECORD_VERSIONS = (RECORD_VERSION, POLICY_RECORD_VERSION)
POLICY_FIELD = "executionPolicy"
POLICY_KEYS = ("digest", "path")
_DIGEST = re.compile(r"[0-9a-f]{64}")

OWNER_USER = "user"
OWNER_PLUGIN = "plugin"
OWNERS = (OWNER_USER, OWNER_PLUGIN)

ABSENT = "record_absent"
MALFORMED = "record_malformed"
UNCHANGED = "record_unchanged"
DIFFERS = "record_differs"
WOULD_CREATE = "record_would_create"
CREATED = "record_created"
CHANGED_UNDERNEATH = "record_changed_underneath"
APPLIED_UNVERIFIED = "record_applied_unverified"
# The record is, or would be, the one wanted, and the policy file it names no longer hashes to
# the digest it records. Not settled: the launcher refuses such a record at every start.
POLICY_CHANGED = "record_policy_changed"
# The outcomes that mean the record now says what this run asked it to, or would with --apply.
SETTLED = (UNCHANGED, CREATED, WOULD_CREATE)

# The lock both owners take around the ownership decision and the write that follows it.
# Its own target, because the two owners write different files: the user path locks the Codex
# configuration and the plugin path locks this record, so neither of those serializes the
# decision they share. Without this, two runs both read an empty host and both write.
OWNERSHIP_LOCK = "crw-mcp-ownership"


def ownership_lock_path(codex_home=None, environ=None):
    return record_path(codex_home, environ).with_name(OWNERSHIP_LOCK)


def record_path(codex_home=None, environ=None):
    environ = os.environ if environ is None else environ
    home = codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")
    return Path(home).expanduser() / RECORD_NAME


def policy_path_complaints(path):
    """Why a string cannot name the policy file the bridge will open, or an empty list.

    Padding is refused rather than trimmed. The bridge strips the variable it reads, so a path
    recorded with a trailing space would be checked here as one file and opened there as another.
    A control character has no business in a path and cannot cross an environment at all.
    """
    if not isinstance(path, str) or not path:
        return ["the execution policy path must be a non-empty string"]
    wrong = []
    if path != path.strip():
        wrong.append("the execution policy path " + repr(path) + " has leading or trailing"
                     " whitespace, which the bridge would strip and so open a different file")
    if any(ord(character) < 32 or ord(character) == 127 for character in path):
        wrong.append("the execution policy path " + repr(path) + " contains a control character")
    if not os.path.isabs(path):
        wrong.append("the execution policy path " + repr(path) + " must be absolute, because the"
                     " packaged launcher runs from the installed package directory")
    try:
        os.fsencode(path)
    except UnicodeError:
        wrong.append("the execution policy path " + repr(path) + " cannot be encoded as a file"
                     " name on this system")
    return wrong


def policy_complaints(reference):
    """What is wrong with an executionPolicy reference, field by field."""
    if not isinstance(reference, dict):
        return [POLICY_FIELD + " must be an object naming path and digest"]
    if sorted(reference) != sorted(POLICY_KEYS):
        return [POLICY_FIELD + " must have exactly the keys " + ", ".join(POLICY_KEYS)
                + ", found " + ", ".join(sorted(map(str, reference)))]
    wrong = policy_path_complaints(reference.get("path"))
    digest = reference.get("digest")
    if not isinstance(digest, str) or not _DIGEST.fullmatch(digest):
        wrong.append(POLICY_FIELD + " digest must be 64 lowercase hexadecimal characters")
    return wrong


def policy_file_complaints(reference):
    """Why the packaged launcher would refuse this reference as its file now stands, or [].

    The launcher's own two questions, asked before a record naming the reference is written or
    confirmed: the path opens as a regular file, and its bytes hash to the recorded digest. A
    record that fails either starts no bridge on any new thread, so writing one and reporting it
    settled is an outage reported as success. Opened without blocking and judged on that
    descriptor, as the launcher does. The contents are not parsed again: bytes that hash to the
    recorded digest are the bytes register-mcp parsed when it recorded it, and the installed
    bridge parses them again at every start.
    """
    wrong = policy_complaints(reference)
    if wrong:
        return wrong
    path, recorded = reference["path"], reference["digest"]
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0))
    except OSError as error:
        return ["the execution policy " + path + " could not be opened ("
                + type(error).__name__ + ": " + str(error) + ")"]
    try:
        if not stat_module.S_ISREG(os.fstat(descriptor).st_mode):
            return ["the execution policy " + path + " is not a regular file"]
        digest = hashlib.sha256()
        while True:
            chunk = os.read(descriptor, 1 << 16)
            if not chunk:
                break
            digest.update(chunk)
    except OSError as error:
        return ["the execution policy " + path + " could not be read ("
                + type(error).__name__ + ": " + str(error) + ")"]
    finally:
        os.close(descriptor)
    if digest.hexdigest() != recorded:
        return ["the execution policy " + path + " now hashes to " + digest.hexdigest()
                + ", not the recorded " + recorded]
    return []


def document(*, command, arguments=None, name=None, issue=None, owner=OWNER_PLUGIN,
             execution_policy=None):
    """The record, built once so the writer and the launcher cannot disagree about its shape."""
    if owner not in OWNERS:
        raise ValueError("owner must be one of " + ", ".join(OWNERS) + ", not " + repr(owner))
    if not command or not str(command).strip():
        raise ValueError("a bridge executable is required")
    if owner == OWNER_PLUGIN and not os.path.isabs(str(command)):
        # The packaged launcher runs with the installed package as its directory, so a relative
        # command resolves inside the version cache -- the one place a runtime must never be.
        # A user-owned record carries whatever the configuration registered instead, because
        # nothing executes it: the launcher reads the owner and stands down before it ever
        # looks at this field, and requiring more here would refuse a registration this command
        # has always accepted.
        raise ValueError("the bridge executable must be an absolute path when owner is "
                         + OWNER_PLUGIN + ", because the packaged launcher runs from the"
                         " installed package directory")
    if execution_policy is not None:
        if owner != OWNER_PLUGIN:
            # The launcher stands down for a user-owned record before it reads anything else, so
            # a policy written there would be read by nothing while looking like it applied.
            raise ValueError("an execution policy is carried only by a " + OWNER_PLUGIN
                             + "-owned record; a " + owner + "-owned registration is started"
                             " by its Codex configuration entry, which never reads this record")
        wrong = policy_complaints(execution_policy)
        if wrong:
            raise ValueError("; ".join(wrong))
    record = {
        "recordVersion": RECORD_VERSION,
        "owner": owner,
        "serverName": name,
        "bridgeExecutable": str(command),
        "args": [str(word) for word in (arguments or [])],
        "installedBy": issue,
    }
    if execution_policy is not None:
        record["recordVersion"] = POLICY_RECORD_VERSION
        record[POLICY_FIELD] = {key: execution_policy[key] for key in POLICY_KEYS}
    return record


def complaints(found):
    """What is wrong with a record, named field by field, as a list rather than an exception."""
    if not isinstance(found, dict):
        return ["the record is a " + type(found).__name__ + ", not an object"]
    wrong = []
    version = found.get("recordVersion")
    if version not in RECORD_VERSIONS:
        wrong.append("recordVersion must be one of " + ", ".join(map(str, RECORD_VERSIONS))
                     + ", found " + repr(version))
    if found.get("owner") not in OWNERS:
        wrong.append("owner must be one of " + ", ".join(OWNERS) + ", found "
                     + repr(found.get("owner")))
    executable = found.get("bridgeExecutable")
    if not isinstance(executable, str) or not executable.strip():
        wrong.append("bridgeExecutable must be a non-empty string")
    elif found.get("owner") == OWNER_PLUGIN and not os.path.isabs(executable):
        # Only the owner whose launcher executes it. Reading a user-owned record more strictly
        # than it is written would report a perfectly good record as unusable, and an unusable
        # record refuses both owners.
        wrong.append("bridgeExecutable must be an absolute path when owner is " + OWNER_PLUGIN)
    arguments = found.get("args")
    if arguments is not None and (not isinstance(arguments, list)
                                  or not all(isinstance(word, str) for word in arguments)):
        wrong.append("args must be a list of strings when it is present at all")
    if version == RECORD_VERSION and POLICY_FIELD in found:
        # Refused rather than ignored, for the reason the version exists: the launcher of this
        # version would start the bridge without it.
        wrong.append("a version " + str(RECORD_VERSION) + " record names no " + POLICY_FIELD
                     + "; a record that names one is version " + str(POLICY_RECORD_VERSION))
    if version == POLICY_RECORD_VERSION:
        if found.get("owner") != OWNER_PLUGIN:
            wrong.append("only a " + OWNER_PLUGIN + "-owned record names an execution policy")
        wrong.extend(policy_complaints(found.get(POLICY_FIELD)))
    return wrong


def read(path):
    """Absent, unreadable and unusable stay three answers, because they are three repairs."""
    found = read_json_without_blocking(path, "the bridge MCP record")
    if not found.usable:
        return None, found.state, found.detail
    if found.state == reading.ABSENT:
        return None, ABSENT, "no record at " + str(path)
    wrong = complaints(found.value)
    if wrong:
        return None, MALFORMED, "; ".join(wrong)
    return found.value, None, None


def read_json_without_blocking(path, what, *, follow=True):
    """reading.read_json, through a descriptor opened without blocking.

    read_json looks at the path and then opens it, and a FIFO put there in between blocks that
    open until something writes to it -- inside a lock, for the writers of this record. Opened
    here first, with O_NONBLOCK, the descriptor is what read_json judges and reads, so there is
    no interval left. An open that fails is classified by looking at the path, never by opening
    it again: absence, a dangling link and an access error each keep their own answer. follow
    False refuses a symbolic link outright, for an archive that has to be the file itself.
    """
    flags = os.O_RDONLY | getattr(os, "O_NONBLOCK", 0)
    if not follow:
        flags |= getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(str(path), flags)
    except (OSError, ValueError) as error:
        # Classified without opening anything again. Handing the path back to read_json would
        # open it a second time, blocking, and a FIFO put there in between would hold the caller
        # -- the interval this function exists to remove.
        if not follow and os.path.islink(str(path)):
            return reading.Reading(state=reading.UNREADABLE, source=path,
                                   detail="a symbolic link, where the file itself is required")
        settled = reading.observe(path, what)
        if settled is not None:
            return settled
        # observe found a regular file where the open had just failed: the path changed between
        # the two looks. Named rather than read, so a rerun decides against it as it then stands.
        return reading.failure(error, source=path, what=what,
                               detail="the file changed while it was being read")
    try:
        return reading.read_json(path, what, descriptor=descriptor)
    finally:
        os.close(descriptor)


def owner_of(found):
    """Who owns this server. A record that cannot be read owns nothing and says so as None."""
    if not isinstance(found, dict):
        return None
    owner = found.get("owner")
    return owner if owner in OWNERS else None


# What makes two records the same registration: who owns it, what it is called, and what it
# starts. installedBy is evidence about who wrote the record and changes nothing about what
# Codex spawns, so a rerun that only carries a different issue is the same registration and
# has to stay idempotent rather than refuse.
#
# The execution policy is part of what it starts: the same executable under another policy file,
# or under the same file with other contents, is a bridge that checks something else. So a
# changed policy is a different registration, refused like a changed executable.
IDENTITY = ("owner", "serverName", "bridgeExecutable", "args", POLICY_FIELD)


def identity(found):
    return {field: (found or {}).get(field) for field in IDENTITY}


def same_registration(found, wanted):
    """Whether these two records describe the same registration."""
    return isinstance(found, dict) and identity(found) == identity(wanted)


# The repair for a record whose policy reference no longer describes the file: there is no
# command that rewrites a record, so it goes aside and is registered again.
_POLICY_REPAIR = ("move {path} aside by hand (or retire it with plugin_transition.py disable,"
                  " which also retires the Stop settings), then run register-mcp again. Threads"
                  " started in between find no record and start no bridge; threads already"
                  " running keep the bridge they spawned")


def _policy_now(wanted):
    """policy_file_complaints for the policy a record names, or [] when it names none."""
    reference = wanted.get(POLICY_FIELD) if isinstance(wanted, dict) else None
    return [] if reference is None else policy_file_complaints(reference)


def _file_identity(path):
    """(identity, bytes) of the regular file at path, from one descriptor, or None.

    Opened without blocking or following a link, and judged on that descriptor, so the inode and
    the bytes are one file's even if the path moves while this reads it.
    """
    try:
        descriptor = os.open(str(path), os.O_RDONLY | getattr(os, "O_NONBLOCK", 0)
                             | getattr(os, "O_NOFOLLOW", 0))
    except OSError:
        return None
    try:
        found = os.fstat(descriptor)
        if not stat_module.S_ISREG(found.st_mode):
            return None
        chunks = []
        while True:
            chunk = os.read(descriptor, 1 << 16)
            if not chunk:
                return (found.st_dev, found.st_ino), b"".join(chunks)
            chunks.append(chunk)
    except OSError:
        return None
    finally:
        os.close(descriptor)


def _remove_if_written_here(path, written):
    """Remove the record at path only while it is the file this run wrote: (removed, why).

    Compare-and-remove, because the lock around the write does not exclude a writer that ignores
    it or one that reclaimed it as stale, and removing whatever is at the path by then deletes
    that writer's record. The file is renamed aside, which is atomic, and judged there: the same
    inode holding the same bytes is this run's and is deleted. Anything else is linked back,
    which fails rather than replace a record that arrived since; it then stays aside under the
    name the answer gives. Either way nothing another writer put there is deleted.
    """
    aside = path.with_name(path.name + ".policy-changed-" + str(os.getpid()) + "-"
                           + os.urandom(4).hex())
    try:
        os.rename(str(path), str(aside))
    except OSError as error:
        return False, ("it could not be moved aside to be judged (" + type(error).__name__ + ": "
                       + str(error) + ")")
    if written is not None and _file_identity(aside) == written:
        try:
            os.unlink(str(aside))
        except OSError as error:
            return False, ("it was moved aside to " + str(aside) + " and could not be deleted ("
                           + type(error).__name__ + ": " + str(error) + ")")
        return True, None
    try:
        os.link(str(aside), str(path))
    except OSError as error:
        return False, ("the file there was no longer the one this run wrote; it was moved aside to "
                       + str(aside) + " and could not be put back (" + type(error).__name__ + ": "
                       + str(error) + "), so it is kept there and nothing was deleted")
    try:
        os.unlink(str(aside))
    except OSError:
        return False, ("the file there was no longer the one this run wrote, so it was left in"
                       " place; a second name for it remains at " + str(aside))
    return False, "the file there was no longer the one this run wrote, so it was left in place"


def outcome_for(wanted, found):
    if not found.usable:
        return found.state
    if found.state == reading.ABSENT:
        return WOULD_CREATE
    # Compared on identity, and on identity alone, so this and the ownership check that runs
    # before it cannot disagree about what counts as the same record. An existing installedBy
    # is left where it is rather than rewritten, which is what keeps the rerun idempotent.
    #
    # Identity alone is not enough to call a record installed, though. A file that gained an
    # unsupported version while keeping its identity is one the launcher refuses to act on, so
    # reporting it unchanged would leave a run exiting 0 over a record that cannot start the
    # bridge. Readable first, then the same registration.
    if complaints(found.value):
        return DIFFERS
    return UNCHANGED if same_registration(found.value, wanted) else DIFFERS


def write(path, wanted, *, apply=False):
    """Write the record, and never over a record that says something else.

    Decided twice and acted on once. The first reading answers the caller; the second happens
    under the lock, and a file that moved in between is refused rather than written over.

    A record that names a policy is settled only against the policy as it stands when the
    answer is given. The digest was taken before this was called, and the policy file is not
    under this lock, so an edit in between would leave a record every new thread's launcher
    refuses. The file is asked again after the write, with the written record read back under
    the lock: a mismatch removes the record this run wrote, which leaves the path as it was
    found (absent, the only state a write starts from), and answers POLICY_CHANGED. An
    already-installed record whose policy no longer matches is not this run's, so it is left as
    it is and answered the same way. The removal is compare-and-remove (_remove_if_written_here):
    a record another writer put there after the write is never the one deleted.
    """
    path = Path(path)
    unusable = complaints(wanted)
    if unusable:
        return {"record": str(path), "outcome": MALFORMED, "applied": False, "wrote": False,
                "detail": "; ".join(unusable), "complaints": unusable}
    found = read_json_without_blocking(path, "the bridge MCP record")
    outcome = outcome_for(wanted, found)
    answer = {"record": str(path), "outcome": outcome, "applied": False, "wrote": False}
    if not found.usable:
        answer["reading"] = found.refusal()
        return answer
    if outcome == UNCHANGED:
        stale = _policy_now(wanted)
        if stale:
            answer["outcome"] = POLICY_CHANGED
            answer["detail"] = ("this record is already installed, and " + "; ".join(stale)
                                + ", so the launcher refuses to start the bridge from it. The"
                                " record was left as it was")
            answer["repair"] = _POLICY_REPAIR.format(path=path)
            return answer
        answer["detail"] = "this record is already installed"
        return answer
    if outcome == DIFFERS:
        answer["detail"] = ("a record is already installed and says something else; this"
                            " command does not overwrite it")
        answer["differingFields"] = sorted(
            field for field in set(wanted) | set(found.value)
            if found.value.get(field) != wanted.get(field)) \
            if isinstance(found.value, dict) else None
        if POLICY_FIELD in (answer["differingFields"] or []):
            # The one difference an operator produces in the ordinary course of things: the policy
            # file was edited, or a policy is being added to a record written before this field
            # existed. Named with its repair, because there is no command that rewrites it.
            answer["repair"] = _POLICY_REPAIR.format(path=path)
        return answer
    if not apply:
        answer["detail"] = "would write this record; nothing was written"
        return answer
    with hostrecord.Locked(path):
        again = read_json_without_blocking(path, "the bridge MCP record")
        if outcome_for(wanted, again) != outcome:
            answer["outcome"] = CHANGED_UNDERNEATH
            answer["detail"] = ("the record changed after it was read, so nothing was written;"
                                " rerun to decide against the file as it now stands")
            return answer
        text = json.dumps(wanted, indent=2, sort_keys=True) + "\n"
        hostrecord.atomic_write(path, text)
        # The inode and bytes this run put there, taken before anything is judged, so a removal
        # can later tell this run's file from one that replaced it.
        written = _file_identity(path)
        if written is not None and written[1] != text.encode("utf-8"):
            written = None
        back = read_json_without_blocking(path, "the bridge MCP record")
        stale = _policy_now(wanted) if back.usable and back.value == wanted else []
        if stale:
            removed, why = _remove_if_written_here(path, written)
    if stale:
        answer["outcome"] = POLICY_CHANGED
        answer["wrote"] = True
        answer["rolledBack"] = removed
        answer["detail"] = ("the execution policy changed while this record was being written: "
                            + "; ".join(stale) + ". The launcher would refuse to start the bridge"
                            " from it, so "
                            + ("the record this run wrote was removed and nothing is installed"
                               if removed else
                               "this run tried to remove the record it wrote and did not: " + why))
        answer["repair"] = ("run register-mcp again against the file as it now stands"
                            if removed else _POLICY_REPAIR.format(path=path))
        return answer
    answer["outcome"] = CREATED
    answer["applied"] = True
    answer["wrote"] = True
    answer["readBack"] = bool(back.usable and back.value == wanted)
    if not answer["readBack"]:
        # The write landed and what is in the file was not confirmed to be it. A launcher
        # pointed at a record nobody read back is the same silence this ordering exists to close.
        answer["outcome"] = APPLIED_UNVERIFIED
        answer["detail"] = ("the record was written and could not be read back as written; the"
                            " plugin should not be relied on to start the bridge until it can be")
    return answer
