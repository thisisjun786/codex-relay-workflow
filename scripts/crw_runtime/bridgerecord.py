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

import json
import os
import re
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
    found = reading.read_json(path, "the bridge MCP record")
    if not found.usable:
        return None, found.state, found.detail
    if found.state == reading.ABSENT:
        return None, ABSENT, "no record at " + str(path)
    wrong = complaints(found.value)
    if wrong:
        return None, MALFORMED, "; ".join(wrong)
    return found.value, None, None


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
    """
    path = Path(path)
    unusable = complaints(wanted)
    if unusable:
        return {"record": str(path), "outcome": MALFORMED, "applied": False, "wrote": False,
                "detail": "; ".join(unusable), "complaints": unusable}
    found = reading.read_json(path, "the bridge MCP record")
    outcome = outcome_for(wanted, found)
    answer = {"record": str(path), "outcome": outcome, "applied": False, "wrote": False}
    if not found.usable:
        answer["reading"] = found.refusal()
        return answer
    if outcome == UNCHANGED:
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
            answer["repair"] = ("move " + str(path) + " aside by hand (or retire it with"
                                " plugin_transition.py disable, which also retires the Stop"
                                " settings), then run register-mcp again. Threads started in"
                                " between find no record and start no bridge; threads already"
                                " running keep the bridge they spawned")
        return answer
    if not apply:
        answer["detail"] = "would write this record; nothing was written"
        return answer
    with hostrecord.Locked(path):
        again = reading.read_json(path, "the bridge MCP record")
        if outcome_for(wanted, again) != outcome:
            answer["outcome"] = CHANGED_UNDERNEATH
            answer["detail"] = ("the record changed after it was read, so nothing was written;"
                                " rerun to decide against the file as it now stands")
            return answer
        hostrecord.atomic_write(path, json.dumps(wanted, indent=2, sort_keys=True) + "\n")
        back = reading.read_json(path, "the bridge MCP record")
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
