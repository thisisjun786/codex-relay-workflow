"""Installing a Linear hook the way OPS-6.3 requires: by appending, and nothing else.

Codex records a trusted hash against a hook's identity, and that identity is positional:
<source>:<event>:<matcher-index>:<hook-index>. So inserting a hook renumbers every later
hook in the same file and detaches the trusted hash recorded against the old identity.
Removing one does the same. That is why installation only ever appends a new matcher group
at the end of its event, why a hook is disabled rather than deleted, and why nothing here
rewrites the content of an existing hook.

Installed, enabled and observed to have fired are three different claims and this module
reports them as three. It activates nothing.
"""

import hashlib
import json
from pathlib import Path

SOURCE = "user"


def identity(source, event, matcher_index, hook_index):
    return ":".join((source, event, str(matcher_index), str(hook_index)))


def _hash(value):
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def read(path):
    path = Path(path)
    if not path.is_file():
        return {"hooks": {}}, None
    try:
        return json.loads(path.read_text(encoding="utf-8")), None
    except (OSError, ValueError) as error:
        return None, type(error).__name__ + ": " + error.__str__()


def inventory(document):
    """Every hook currently present, with the identity Codex knows it by."""
    found = []
    for event, groups in (document.get("hooks") or {}).items():
        for matcher_index, group in enumerate(groups or []):
            for hook_index, hook in enumerate((group or {}).get("hooks") or []):
                found.append({
                    "identity": identity(SOURCE, event, matcher_index, hook_index),
                    "event": event,
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
    before_text = path.read_text(encoding="utf-8") if path.is_file() else ""
    document, error = read(path)
    if document is None:
        return {"outcome": "UNREADABLE", "detail": error}

    existing = {entry["identity"]: entry["trustedHash"] for entry in inventory(document)}
    proposal = plan(document, event, hook)
    if proposal["trustedHash"] in existing.values():
        return {"outcome": "LINKED", "detail": "an identical hook is already installed",
                "identity": proposal["identity"], "applied": False,
                "installed": True, "enabled": "unknown", "observedFired": "unknown"}

    if not apply:
        return {"outcome": "MISSING", "detail": "would append; nothing was written",
                "plan": proposal, "applied": False,
                "installed": False, "enabled": "unknown", "observedFired": "unknown"}

    document.setdefault("hooks", {}).setdefault(event, []).append({"hooks": [hook]})
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")

    # Read the registration back rather than trusting the write.
    written, error = read(path)
    if written is None:
        return {"outcome": "UNREADABLE", "detail": "wrote, but could not read back: " + str(error)}
    after = {entry["identity"]: entry["trustedHash"] for entry in inventory(written)}
    preserved = all(after.get(key) == value for key, value in existing.items())
    return {
        "outcome": "CREATED",
        "identity": proposal["identity"],
        "trustedHash": proposal["trustedHash"],
        "hookFileSha256": _hash(path.read_text(encoding="utf-8")),
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
    """Disabling is the supported removal. Deleting renumbers, so it is refused."""
    return {
        "outcome": "REFUSED",
        "detail": (
            "removing a hook renumbers every later hook in the same file and detaches the"
            " trusted hash recorded against their identities, so a hook is disabled rather"
            " than deleted (OPS-6.3)"
        ),
        "identity": identity(SOURCE, event, matcher_index, hook_index),
    }

