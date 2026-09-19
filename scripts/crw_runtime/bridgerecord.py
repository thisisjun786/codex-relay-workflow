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
"""

import json
import os
from pathlib import Path

from . import hostrecord, reading

# This record's own file, beside the completion hook's and never inside it. Sharing one would
# make two owners of one document, and an operator changing the MCP pointer would be editing
# the settings a Stop hook reads.
RECORD_NAME = "crw-bridge-mcp.json"
RECORD_VERSION = 1

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


def document(*, command, arguments=None, name=None, issue=None, owner=OWNER_PLUGIN):
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
    return {
        "recordVersion": RECORD_VERSION,
        "owner": owner,
        "serverName": name,
        "bridgeExecutable": str(command),
        "args": [str(word) for word in (arguments or [])],
        "installedBy": issue,
    }


def complaints(found):
    """What is wrong with a record, named field by field, as a list rather than an exception."""
    if not isinstance(found, dict):
        return ["the record is a " + type(found).__name__ + ", not an object"]
    wrong = []
    if found.get("recordVersion") != RECORD_VERSION:
        wrong.append("recordVersion must be " + str(RECORD_VERSION) + ", found "
                     + repr(found.get("recordVersion")))
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
IDENTITY = ("owner", "serverName", "bridgeExecutable", "args")


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
