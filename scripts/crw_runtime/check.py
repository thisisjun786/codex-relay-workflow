"""OPS-6.1 results in the OPS-6.2 shape, kept apart on purpose.

Six of these are the contract's fields. Two more, imported and settingsPreserved, sit beside
them because importing and preserving a user's settings are separately falsifiable and the
issue asks for them by name. None of the eight is ever derived from another: a package that
imports proves nothing about a tool being exposed, and a tool being exposed proves nothing
about a socket accepting a connection.
"""

VALUES = ("verified", "not_verified", "unknown", "not_applicable")

FIELDS = (
    "installed", "mcpExposed", "connected", "deliveryAccepted",
    "verificationComplete", "alwaysActive", "imported", "settingsPreserved",
)


def field(value, evidence, command=None, acting_process=None, measured_at=None):
    """One result. measured_at is 'unknown' unless a real observation supplied a time.

    Never supply a plausible time to satisfy the shape and never copy another field's time:
    both produce a record that reads as a host measurement nobody performed, which is worse
    than an admitted gap because a later reader cannot tell the difference.
    """
    if value not in VALUES:
        raise ValueError("unsupported result value: " + repr(value))
    return {
        "value": value,
        "evidence": evidence,
        "command": command,
        "actingProcess": acting_process,
        "measuredAt": measured_at or "unknown",
    }


def unknown(reason):
    return field("unknown", reason)


def not_applicable(reason):
    return field("not_applicable", reason)


def record(fields, *, destination, destination_kind, scope=None):
    """Assemble the result, naming the destination it measured.

    destination_kind is 'temporary' or 'host'. It is recorded so a temporary-destination
    proof can never be read later as a claim about somebody's real Codex home.
    """
    if destination_kind not in ("temporary", "host"):
        raise ValueError("destination_kind must be temporary or host")
    missing = [name for name in FIELDS if name not in fields]
    if missing:
        raise ValueError("every result must be stated, missing: " + ", ".join(missing))
    return {
        "recordVersion": 1,
        "destination": str(destination),
        "destinationKind": destination_kind,
        "scope": scope,
        "results": {name: fields[name] for name in FIELDS},
        "note": (
            "These results never imply one another. installed, mcpExposed, connected,"
            " deliveryAccepted, verificationComplete and alwaysActive are the six OPS-6.1"
            " fields; imported and settingsPreserved are two further observations beside"
            " them. A result measured against a temporary destination is evidence about"
            " that destination only."
        ),
    }

