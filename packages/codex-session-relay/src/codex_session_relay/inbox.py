"""Decision 25: immutable receipt/ACK ingress, shared with the Go owner."""

import argparse
import hashlib
import json
import os
import re
import uuid
from pathlib import Path

from .errors import RefusalReason, RelayError

# Only receipt ingestion and acknowledgments, not verification claims/verdicts,
# transport operations, publication completion, or operator state changes.
COMMAND_KEYS = {"emit": None, "ack": "event", "fault-notification-ack": "notification"}
# Previously queued host-verified readbacks must still be decoded and retired.
REPLAY_KEYS = {**COMMAND_KEYS, "supervisor-read": "message"}


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def subcommand(parser, command):
    return next(action for action in parser._actions
                if isinstance(action, argparse._SubParsersAction)).choices[command]


def envelope(parser, args):
    command = args.command
    actions = subcommand(parser, command)._actions
    arguments = {action.dest: getattr(args, action.dest) for action in actions
                 if action.dest != "help" and hasattr(args, action.dest)
                 and getattr(args, action.dest) != action.default}
    digest = "sha256:" + hashlib.sha256(canonical(arguments)).hexdigest()
    key = arguments[REPLAY_KEYS[command]] if REPLAY_KEYS[command] else digest[7:39]
    raw = (command + "." + str(key)).encode("utf-8")
    identifier = "".join(chr(byte) if (65 <= byte <= 90 or 97 <= byte <= 122 or
                                     48 <= byte <= 57 or byte in b"._-")
                         else f"%{byte:02X}" for byte in raw)
    if len(identifier) > 200:
        raise ValueError("inbox operation ID exceeds 200 encoded characters")
    return {"inboxVersion": 1, "operationId": identifier, "command": command,
            "arguments": arguments, "payloadDigest": digest}


def sync_directory(directory):
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_request(handle, raw):
    """One injectable write boundary; tests inject ENOSPC here."""
    handle.write(raw)
    handle.flush()
    os.fsync(handle.fileno())


def enqueue(state, request):
    directory = Path(state) / "takeover-inbox"
    directory.mkdir(mode=0o700, exist_ok=True)
    sync_directory(directory.parent)
    raw = canonical(request)
    identifier = request["operationId"]
    final = directory / identifier
    temporary = directory / f".tmp-{identifier}-{os.getpid()}-{uuid.uuid4().hex}"
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as handle:
            write_request(handle, raw)
        try:
            os.link(temporary, final)
        except FileExistsError:
            if final.read_bytes() != raw:
                raise RelayError(RefusalReason.INBOX_CONFLICT,
                                 "the operation ID already names different inbox bytes") from None
        sync_directory(directory)
    finally:
        temporary.unlink(missing_ok=True)
    return {"status": "durably_queued", "operationId": identifier,
            "payloadDigest": request["payloadDigest"], "detail": "durably queued; not yet applied"}


def replay(services, parser):
    """Apply once in the same transaction as the retained result marker.

    A host/storage exception keeps the entry for retry. A domain refusal is a
    durable result, with its handler writes rolled back to the savepoint.
    """
    from .cli import EXIT_USAGE, PayloadExit, SystemExit2

    directory = services.selection.path / "takeover-inbox"
    if not directory.exists():
        return
    for path in sorted(directory.iterdir()):
        if path.name.startswith("."):
            continue
        try:
            raw = path.read_bytes()
        except FileNotFoundError:
            # Another admitted owner process committed and retired this entry.
            continue
        request = json.loads(raw)
        if (not isinstance(request, dict) or request.get("inboxVersion") != 1
                or request.get("command") not in REPLAY_KEYS
                or len(path.name) > 200
                or not re.fullmatch(r"(?:[A-Za-z0-9._-]|%[0-9A-F]{2})+", path.name)
                or request.get("operationId") != path.name or canonical(request) != raw):
            raise ValueError(f"invalid takeover inbox entry: {path.name}")
        action_parser = subcommand(parser, request["command"])
        actions = {action.dest: action for action in action_parser._actions if action.dest != "help"}
        arguments = request.get("arguments")
        if not isinstance(arguments, dict) or not set(arguments) <= set(actions):
            raise ValueError("invalid inbox arguments")
        args = argparse.Namespace(command=request["command"], **action_parser._defaults)
        for dest, action in actions.items():
            value = arguments.get(dest, action.default)
            if action.required and dest not in arguments:
                raise ValueError(f"missing inbox argument: {dest}")
            if dest in arguments:
                if isinstance(action, argparse._AppendAction):
                    valid = isinstance(value, list) and all(isinstance(item, str) for item in value)
                elif isinstance(action, (argparse._StoreTrueAction, argparse._StoreFalseAction)):
                    valid = type(value) is bool
                elif action.type is int:
                    valid = type(value) is int
                else:
                    valid = isinstance(value, str)
                if not valid or action.choices is not None and value not in action.choices:
                    raise ValueError(f"invalid inbox argument: {dest}")
            setattr(args, dest, value)
        if envelope(parser, args) != request:
            raise ValueError("inbox payload digest or identifier disagrees")
        key = "inbox:" + path.name
        with services.store.composing() as db:
            previous = db.execute("SELECT value FROM schema_meta WHERE key=?", (key,)).fetchone()
            if previous:
                if json.loads(previous[0])["payloadDigest"] != request["payloadDigest"]:
                    raise RelayError(RefusalReason.INBOX_CONFLICT, "committed inbox payload differs")
            else:
                db.execute("SAVEPOINT inbox_handler")
                try:
                    answer, code = args.handler(services, args), 0
                except RelayError as error:
                    db.execute("ROLLBACK TO inbox_handler")
                    answer = {"error": "refused", "reason": error.reason.value if error.reason else None,
                              "detail": error.detail}
                    code = 2
                except (SystemExit2, PayloadExit) as error:
                    if error.code not in (2, EXIT_USAGE):
                        raise
                    db.execute("ROLLBACK TO inbox_handler")
                    answer = (error.payload if isinstance(error, PayloadExit)
                              else {"error": "usage", "detail": str(error)})
                    code = error.code
                finally:
                    db.execute("RELEASE inbox_handler")
                marker = {"payloadDigest": request["payloadDigest"], "exit": code, "answer": answer}
                db.execute("INSERT INTO schema_meta VALUES (?,?)", (key, canonical(marker).decode("utf-8")))
        # The commit above precedes both unlink and directory sync. A crash here
        # leaves a file whose next replay reads the marker and calls no handler.
        path.unlink(missing_ok=True)
        sync_directory(directory)
