"""Installing a Linear hook the way OPS-6.3 requires: by appending, and nothing else.

Codex records a trusted hash against a hook's identity, and that identity is positional:
<source>:<event>:<matcher-index>:<hook-index>. So inserting a hook renumbers every later
hook in the same file and detaches the trusted hash recorded against the old identity.
Removing one does the same. That is why installation only ever appends a new matcher group
at the end of its event, why removal is refused rather than performed, and why nothing here
rewrites the content of an existing hook.

Installed, enabled and observed to have fired are three different claims and this module
reports them as three. It activates nothing.
"""

import hashlib
import json
from pathlib import Path

from . import hostrecord, reading

SOURCE = "user"

# Installation appends a group carrying no matcher, so that is the matcher a duplicate has to
# share to be the same registration.
INSTALLED_MATCHER = None


def identity(source, event, matcher_index, hook_index):
    return ":".join((source, event, str(matcher_index), str(hook_index)))


def _hash(value):
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _refused(failed):
    """A reading that failed, reported as itself.

    The outcome IS the reading's state, so an unreadable shape and an unreachable file stay
    two answers. Both are refusals to the caller; only one of them is about the content.
    """
    return {"outcome": failed.state, "detail": failed.detail, "applied": False,
            "wrote": False, "reading": failed.refusal()}


def shape(document):
    """Reject containers no consumer here can walk, with a message naming the one that failed."""
    if not isinstance(document, dict):
        raise TypeError("a hook file is an object, found " + type(document).__name__)
    events = document.get("hooks")
    if events is not None and not isinstance(events, dict):
        raise TypeError("hooks is an object, found " + type(events).__name__)
    for name, groups in (events or {}).items():
        if groups is not None and not isinstance(groups, list):
            raise TypeError("hooks." + str(name) + " is a list, found " + type(groups).__name__)
        for group in groups or []:
            if not isinstance(group, dict):
                raise TypeError("every group in hooks." + str(name) + " is an object, found "
                                + type(group).__name__)
            entries = group.get("hooks")
            if entries is not None and not isinstance(entries, list):
                raise TypeError("a group's hooks is a list, found " + type(entries).__name__)
            for entry in entries or []:
                if not isinstance(entry, dict):
                    raise TypeError("every hook in hooks." + str(name)
                                    + " is an object, found " + type(entry).__name__)
    return document


def read(path):
    """Read the hook file as a reading, so absent, unreadable and unreachable stay apart."""
    return reading.read_json(path, "the hook file", absent=lambda: {"hooks": {}}, shape=shape)


def inventory(document, event=None):
    """Every hook currently present, with the identity Codex knows it by.

    Pass an event to restrict the inventory to it. Duplicate matching has to be
    event-scoped: an identical command installed under another event is a different
    registration, and treating it as the same one would report the requested event as
    hooked while leaving it without a hook.
    """
    found = []
    for name, groups in (document.get("hooks") or {}).items():
        if event is not None and name != event:
            continue
        for matcher_index, group in enumerate(groups or []):
            for hook_index, hook in enumerate((group or {}).get("hooks") or []):
                found.append({
                    "identity": identity(SOURCE, name, matcher_index, hook_index),
                    "event": name,
                    # Part of the registration, not decoration: the same command under a
                    # different matcher fires on different turns, so treating it as already
                    # installed reports the requested matcher as hooked while leaving it bare.
                    "matcher": (group or {}).get("matcher"),
                    "trustedHash": _hash(json.dumps(hook, sort_keys=True)),
                })
    return found


def plan(document, event, hook):
    """Where this hook would go, without writing anything."""
    groups = (document.get("hooks") or {}).get(event) or []
    return {
        "identity": identity(SOURCE, event, len(groups), 0),
        "matcherIndex": len(groups),
        "hookIndex": 0,
        "trustedHash": _hash(json.dumps(hook, sort_keys=True)),
        "appendOnly": True,
        "existingIdentitiesPreserved": [entry["identity"] for entry in inventory(document)],
    }


def install(path, event, hook, *, issue, apply=False):
    """Append one hook at the end of its event and read the registration back."""
    path = Path(path)
    before = reading.read_text(path, "the hook file")
    if not before.usable:
        return _refused(before)
    before_text = before.value
    first = read(path)
    if not first.usable:
        return _refused(first)
    document = first.value

    existing = {entry["identity"]: entry["trustedHash"] for entry in inventory(document)}
    in_event = {entry["identity"]: entry["trustedHash"]
                for entry in inventory(document, event)
                if entry["matcher"] == INSTALLED_MATCHER}
    proposal = plan(document, event, hook)
    already = [key for key, value in in_event.items() if value == proposal["trustedHash"]]
    if already:
        # The identity of the hook that is already there, not the next free slot: reporting
        # the slot this run would have used would name a hook that does not exist.
        return {"outcome": "LINKED", "detail": "an identical hook is already installed",
                "identity": already[0], "applied": False,
                "installed": True, "enabled": "unknown", "observedFired": "unknown"}

    if not apply:
        return {"outcome": "MISSING", "detail": "would append; nothing was written",
                "plan": proposal, "applied": False,
                "installed": False, "enabled": "unknown", "observedFired": "unknown"}

    # The whole read-modify-write is held under one lock, and the write itself is a temp
    # file and replace, so an interrupted run cannot truncate a file holding other hooks.
    # The lock coordinates runs of this command; it cannot coordinate with an editor that
    # does not take it, and that limit is stated rather than assumed away.
    with hostrecord.Locked(path):
        again = read(path)
        if not again.usable:
            return _refused(again)
        if json.dumps(again.value, sort_keys=True) != json.dumps(document, sort_keys=True):
            return {"outcome": "CHANGED", "detail": (
                "the hook file changed after it was read, so nothing was appended;"
                " rerun to plan against the file as it now stands")}
        document.setdefault("hooks", {}).setdefault(event, []).append({"hooks": [hook]})
        # The bytes this run writes, kept so the receipt can name them without reading the
        # file a second time outside a guarded region.
        payload = json.dumps(document, indent=2) + "\n"
        hostrecord.atomic_write(path, payload)

        # Read the registration back rather than trusting the write.
        back = read(path)
    if not back.usable:
        # The append landed. Reporting this as a refusal that wrote nothing would be the
        # worst answer available: the caller would retry and append a second copy.
        return {"outcome": "APPLIED_UNVERIFIED", "applied": True, "wrote": True,
                "readBack": False, "installed": True, "enabled": "unknown",
                "observedFired": "unknown", "identity": proposal["identity"],
                "reading": back.refusal(),
                "detail": "the hook was appended and the file could not be read back: "
                          + str(back.detail)}
    written = back.value
    after = {entry["identity"]: entry["trustedHash"] for entry in inventory(written)}
    preserved = all(after.get(key) == value for key, value in existing.items())
    return {
        "outcome": "CREATED",
        "identity": proposal["identity"],
        "trustedHash": proposal["trustedHash"],
        "hookFileSha256": _hash(payload),
        "hookFileSha256Before": _hash(before_text),
        "installedBy": issue,
        "applied": True,
        "readBack": proposal["identity"] in after,
        "existingIdentitiesPreserved": preserved,
        # Three separate claims. Installation is not activation.
        "installed": True,
        "enabled": "unknown",
        "enabledDetail": "whether Codex loads and trusts this identity is the host's answer, not this command's",
        "observedFired": "unknown",
        "observedFiredDetail": "no hook execution was observed by this command",
    }


def disable(document, event, matcher_index, hook_index):
    """Removal is refused, and nothing here performs one.

    Not "disabled rather than deleted": that would name an operation this module does not
    provide and no command exposes. Removing a hook renumbers every later hook in the same
    file and detaches the trusted hash Codex recorded against their identities, so the honest
    answer is that this command appends and does not remove. Whoever needs a hook gone edits
    the file and accepts the renumbering, knowingly.
    """
    return {
        "outcome": "REFUSED",
        "detail": (
            "removing a hook renumbers every later hook in the same file and detaches the"
            " trusted hash recorded against their identities, so a hook is disabled rather"
            " than deleted (OPS-6.3)"
        ),
        "identity": identity(SOURCE, event, matcher_index, hook_index),
    }
