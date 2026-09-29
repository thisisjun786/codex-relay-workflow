"""Decision 25: immutable receipt/ACK ingress, shared with the Go owner."""

import argparse
import errno
import fcntl
import hashlib
import json
import os
import re
import stat
import uuid
from pathlib import Path

from .errors import RefusalReason, RelayError

# Only receipt ingestion and acknowledgments, not verification claims/verdicts,
# transport operations, publication completion, or operator state changes.
COMMAND_KEYS = {"emit": None, "ack": "event", "fault-notification-ack": "notification"}
# Previously queued host-verified readbacks must still be decoded and retired.
REPLAY_KEYS = {**COMMAND_KEYS, "supervisor-read": "message"}
# An inbox entry name (decision 25). Other names, like '.'-names, are not entries.
ENTRY_NAME = re.compile(r"(?:[A-Za-z0-9._-]|%[0-9A-F]{2})+")
# Serializes replayers from reading an entry until after its unlink and directory sync.
REPLAY_LOCK = ".replay.lock"


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


def is_entry_name(name):
    return not name.startswith(".") and len(name) <= 200 and ENTRY_NAME.fullmatch(name) is not None


def read_entry(path):
    """The entry's bytes and inode, never following a link; None when already retired."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    except OSError as error:
        if error.errno == errno.ELOOP:
            raise ValueError(f"invalid takeover inbox entry: {path.name}: symbolic link") from None
        raise
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise ValueError(f"invalid takeover inbox entry: {path.name}: not a regular file")
        chunks = []
        while chunk := os.read(fd, 1 << 20):
            chunks.append(chunk)
        return b"".join(chunks), (info.st_dev, info.st_ino)
    finally:
        os.close(fd)


def retained(error):
    """Ownership or host-unconfirmed failures keep the entry for a later replay."""
    from .errors import ReceiptRefused
    from .hostadapter import HostUnavailable
    from .ownership import OwnershipRefused

    return (isinstance(error, OwnershipRefused)
            or isinstance(error, ReceiptRefused) and isinstance(error.__cause__, HostUnavailable))


def replay(services, parser):
    """Apply once in the same transaction as the retained result marker.

    A host/storage exception keeps the entry for retry. A domain refusal is a
    durable result, with its handler writes rolled back to the savepoint.
    """
    directory = services.selection.path / "takeover-inbox"
    if not directory.exists():
        return
    from .ownership import flock_within

    lock = os.open(directory / REPLAY_LOCK, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        # Bounded: another replayer holds this across its handlers' host calls. Expiry is a
        # retryable host error (exit 3) before this command changes anything (cutover Lock order).
        flock_within(lock, fcntl.LOCK_EX, "the takeover inbox replay lock")
        for path in sorted(directory.iterdir()):
            if is_entry_name(path.name):
                _replay_entry(services, parser, directory, path)
    finally:
        os.close(lock)


def _replay_entry(services, parser, directory, path):
    from .cli import EXIT_USAGE, PayloadExit, SystemExit2

    entry = read_entry(path)
    if entry is None:
        # Another admitted owner process committed and retired this entry.
        return
    raw, inode = entry
    request = json.loads(raw)
    if (not isinstance(request, dict) or type(request.get("inboxVersion")) is not int
            or request["inboxVersion"] != 1 or request.get("command") not in REPLAY_KEYS
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
    if canonical(envelope(parser, args)) != raw:
        raise ValueError("inbox payload digest or identifier disagrees")
    key = "inbox:" + path.name
    digest = request["payloadDigest"]
    kept = False
    with services.store.composing() as db:
        previous = db.execute("SELECT value FROM schema_meta WHERE key=?", (key,)).fetchone()
        marker = json.loads(previous[0]) if previous else None
        if marker is not None and marker["payloadDigest"] != digest:
            # A retired ID reused with different bytes: a terminal conflict, never applied,
            # never blocking the entries after it.
            conflict = {"payloadDigest": digest, "exit": 2, "answer": {
                "error": "refused", "reason": RefusalReason.INBOX_CONFLICT.value,
                "detail": "committed inbox payload differs"}}
            db.execute("INSERT OR IGNORE INTO schema_meta VALUES (?,?)",
                       ("inbox-conflict:" + path.name + ":" + digest[7:23],
                        canonical(conflict).decode("utf-8")))
        elif marker is None or type(marker["exit"]) is not int or marker["exit"] != 0:
            # Only a successful application deduplicates; a refused one is judged again.
            db.execute("SAVEPOINT inbox_handler")
            try:
                answer, code = args.handler(services, args), 0
            except RelayError as error:
                db.execute("ROLLBACK TO inbox_handler")
                kept = retained(error)
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
            if not kept:
                db.execute("INSERT OR REPLACE INTO schema_meta VALUES (?,?)", (key, canonical(
                    {"payloadDigest": digest, "exit": code, "answer": answer}).decode("utf-8")))
    if kept:
        return
    # The commit above precedes both unlink and directory sync. A crash here
    # leaves a file whose next replay reads the marker and calls no handler.
    # Only the bytes just applied are retired, never a newer entry at the same name.
    try:
        current = os.lstat(path)
    except FileNotFoundError:
        return
    if (current.st_dev, current.st_ino) == inode:
        path.unlink(missing_ok=True)
        sync_directory(directory)
