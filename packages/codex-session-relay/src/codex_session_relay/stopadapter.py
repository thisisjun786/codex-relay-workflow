"""The Stop hook adapter, carried by the installed runtime instead of by a checkout.

The CRW plugin package declares a Stop hook and ships a launcher for it, but a package cannot
carry a runtime: the version cache is replaced wholesale on every install, so an executable a
running session depends on must live outside it. Until now the launcher pointed at
<checkout>/scripts/completion_hook.py, which made a plugin installation depend on somebody's
working copy. This module is where that adapter lives once the runtime is installed, reached as
the console script crw-completion-hook.

Standard library only, and no import from the rest of this package. Both halves of that are
deliberate. This runs on a Stop with the host's payload, and an ImportError here is a hook that
failed rather than a hook that released. Keeping it import-free is also what lets the agreement
test in scripts/ci/tests/test_adapter_agreement.py load this file directly and drive it against
the checkout adapter in the offline CI job, where this package is not installed.

That agreement test is the reason this file is allowed to exist at all. It is a second copy of
the runtime path scripts/crw_runtime/completion.py owns, and two copies of a rule do not stay in
agreement on their own. So the test compares behaviour rather than shape -- what the adapter
returns, what it writes and where it writes it -- and it carries its own mutation case that
proves it would notice a divergence. The duplicate is temporary by intent; removing the checkout
copy is a tracked obligation in the operations project that nobody has scheduled yet.

The three refusals are inherited unchanged, because a hook that breaks them costs turns rather
than reporting a fault.

It never decides a turn itself. Only a verdict the guard produced can reach stdout.

It never exits 2. The host reads exit 2 as the blocking code and takes stderr as the continuation
prompt, and argparse exits 2 on any usage error, so nothing here parses arguments and nothing
here writes to stderr.

It answers each Stop event once. A turn can end several times, and each end is its own event;
two registrations answering one end, or one end delivered twice, is one event handled twice. The
first invocation to create the event's accepted record asks the guard, and every other one leaves
a row saying it was a duplicate. An invocation that cannot tell which event it is answers as it
always did and says so. See event_identity() and docs/runtime-install.md.

It never answers one question with another question's reading. "the guard released" and "the
guard could not be asked" are different values, and so are "the runtime refused the request" and
"the runtime rejected the call before it ran".

Rules: skills/crw-run/references/hook-contract.md.
"""

import errno
import hashlib
import json
import os
import re
import stat as stat_module
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

# ----------------------------------------------------------------- the mirrored contract
#
# Every name below is mirrored from scripts/crw_runtime/completion.py, which this file cannot
# import. The agreement test asserts the two still behave alike; it does not assert they look
# alike, because matching names would let the behaviour diverge while the test stayed green.

EVENT = "Stop"

CONFIG_NAME = "crw-completion-hook.json"
CONFIG_VERSION = 1

GUARD_COMMAND = "guard-evaluate"

OBSERVE = "observe"
HOLD = "hold"
MODES = (OBSERVE, HOLD)

OWNER_USER = "user"
OWNER_PLUGIN = "plugin"
OWNERS = (OWNER_USER, OWNER_PLUGIN)

EVERY_INVOCATION = "every_invocation"
FAULTS_ONLY = "faults_only"
NO_JOURNAL = "no_journal"
JOURNAL_POLICIES = (EVERY_INVOCATION, FAULTS_ONLY, NO_JOURNAL)

DEFAULT_TIMEOUT_SECONDS = 5
MAX_TIMEOUT_SECONDS = 86400
LAUNCHER_CEILING_SECONDS = 9

NOT_STARTED = "not_started"
EXITED = "exited"
SIGNALLED = "signalled"
TIMED_OUT = "timed_out"

SAID_NOTHING = "said_nothing"
SAID_A_VERDICT = "said_a_verdict"
SAID_AN_ERROR_RECORD = "said_an_error_record"
SAID_SOMETHING_UNREADABLE = "said_something_unreadable"

GUARD_EXIT_OK = 0
GUARD_EXIT_REFUSED = 2
GUARD_EXIT_HOST = 3
GUARD_EXIT_USAGE = 4

STDIN_UNREADABLE = "stdin_unreadable"
STDIN_NOT_JSON = "stdin_not_json"
STDIN_NOT_OBJECT = "stdin_not_object"
CONFIG_ABSENT = "config_absent"
CONFIG_UNREADABLE = "config_unreadable"
CONFIG_UNREACHABLE = "config_unreachable"
CONFIG_MALFORMED = "config_malformed"
GUARD_UNREACHABLE = "guard_unreachable"
GUARD_TIMED_OUT = "guard_timed_out"
GUARD_SIGNALLED = "guard_signalled"
GUARD_REJECTED_THE_CALL = "guard_rejected_the_call"
GUARD_REFUSED = "guard_refused"
GUARD_HOST_ERROR = "guard_host_error"
GUARD_USAGE_ERROR = "guard_usage_error"
GUARD_ENDED_UNEXPECTEDLY = "guard_ended_unexpectedly"
GUARD_SAID_NOTHING = "guard_said_nothing"
GUARD_OUTPUT_UNREADABLE = "guard_output_unreadable"
GUARD_VERDICT_INCOMPLETE = "guard_verdict_incomplete"
GUARD_ANSWERED = "guard_answered"
ADAPTER_FAULTED = "adapter_faulted"

ANSWERED = (GUARD_ANSWERED,)

BLOCK = "block"
RELEASE = "release"
DECISIONS = (BLOCK, RELEASE)

JOURNAL_NAME = re.compile(r"^[0-9a-f]{32}\.json$")

# How the checkout's reading module classifies a failure, mirrored rather than approximated: ANY
# OSError established nothing and is an access error, while a decode or shape failure read
# something and could not make sense of it. Getting this wrong sends an operator to the wrong
# repair, and getting the pre-read part wrong is worse than that -- see read_settings.
DECODE_FAILURES = (UnicodeDecodeError, ValueError)

# ----------------------------------------------------------------- which Stop event this is
#
# CRW-212. A Stop EVENT is one end of one sampling sequence, and a turn can have several: every
# continuation a hook asks for, and every waiting message the host appends, makes the turn end
# again. Two registrations answering one Stop, or one Stop delivered twice, is one event handled
# twice. The payload cannot tell those apart -- an isolated Codex 0.154.0 run produced two Stops of
# one turn with byte-identical payloads -- so the identity is read from what the host recorded in
# the transcript before it ran the hook: the answer item that ended this sampling.

RECORD_VERSION = 2
LEDGER_DIRECTORY = "accepted"
LEDGER_NAME = re.compile(r"^[0-9a-f]{64}\.json$")
OUTCOME_NAME = re.compile(r"^[0-9a-f]{64}\.outcome\.json$")
OUTCOME_SUFFIX = ".outcome.json"
LEDGER_VERSION = 1
# Where the registrations of one host meet over a Stop event: a directory under the Codex home the
# Stop fired in. Each registration's settings name its own journalRoot, and two registrations
# reading different settings keep different roots; they share the host's Codex home.
HOST_LEDGER_PARTS = ("crw-completion-hook", "stop-events")
EVENT_KEY_TAG = "crw-stop-event/1"
# Bounded so the scan stays inside the margin the packaged launcher keeps over the guard budget:
# the launcher waits the budget plus two seconds, and this is at most three quarters of one. The
# byte bound covers the largest turn measured on the host this was built on (62 MB, 2026-09-23);
# reading that far back took 0.11-0.31 s.
SCAN_CHUNK_BYTES = 1 << 16
SCAN_MAX_BYTES = 64 << 20
SCAN_MAX_SECONDS = 0.75

ACCEPTED = "accepted"
DUPLICATE = "duplicate"
UNESTABLISHED = "unestablished"
UNCLAIMABLE = "unclaimable"
CLAIM_FAILED = "claim_failed"
# The host's file for the event could be neither made nor found, so no invocation can own the event
# and this one released it without asking the guard.
UNARBITRATED = "unarbitrated"
ACCEPTANCES = (ACCEPTED, DUPLICATE, UNESTABLISHED, UNCLAIMABLE, CLAIM_FAILED, UNARBITRATED)

DUPLICATE_INVOCATION = "duplicate_invocation"
ARBITRATION_FAILED = "arbitration_failed"
# What faults_only leaves out of the journal. Only journal() reads this: the guard fields and the
# hook output still follow ANSWERED, so a duplicate never carries a verdict nobody asked for.
QUIET = (GUARD_ANSWERED, DUPLICATE_INVOCATION)

IDENTITY_FIELDS_INCOMPLETE = "identity_fields_incomplete"
TRANSCRIPT_PATH_MISSING = "transcript_path_missing"
TRANSCRIPT_PATH_RELATIVE = "transcript_path_relative"
TRANSCRIPT_ABSENT = "transcript_absent"
TRANSCRIPT_UNREACHABLE = "transcript_unreachable"
TRANSCRIPT_NOT_REGULAR = "transcript_not_regular"
SCAN_BOUND_EXCEEDED = "scan_bound_exceeded"
SCAN_TIMED_OUT = "scan_timed_out"
TRANSCRIPT_TAIL_INCOMPLETE = "transcript_tail_incomplete"
TRANSCRIPT_LINE_UNREADABLE = "transcript_line_unreadable"
TURN_START_NOT_FOUND = "turn_start_not_found"
NO_ANSWER_ITEM_FOR_TURN = "no_answer_item_for_turn"
ANSWER_ITEM_UNIDENTIFIED = "answer_item_unidentified"
ANSWER_PRECEDES_LATEST_INPUT = "answer_precedes_latest_input"
ANSWER_TEXT_MISMATCH = "answer_text_mismatch"
ANSWER_TEXT_AMBIGUOUS = "answer_text_ambiguous"
SESSION_MISMATCH = "session_mismatch"

# The items that start a sampling. Met before any answer when reading newest-first, one of these
# means the transcript does not yet show this Stop's answer.
HOOK_PROMPT = "HookPrompt"
USER_MESSAGE = "UserMessage"
INPUT_ITEMS = (HOOK_PROMPT, USER_MESSAGE)


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def usable_seconds(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return 0 < value <= MAX_TIMEOUT_SECONDS


# ----------------------------------------------------------------- the delivered payload


def stop_input(payload):
    """Read the Stop payload. Three distinct failures, and no field gate beyond being an object."""
    if payload is None:
        return None, STDIN_UNREADABLE, "the Stop payload could not be read from stdin"
    try:
        text = payload.decode("utf-8") if isinstance(payload, bytes) else str(payload)
    except ValueError as error:
        return None, STDIN_UNREADABLE, "the Stop payload is not UTF-8: " + str(error)
    try:
        value = json.loads(text)
    except ValueError as error:
        return None, STDIN_NOT_JSON, "the Stop payload is not JSON: " + str(error)
    if not isinstance(value, dict):
        return None, STDIN_NOT_OBJECT, ("the Stop payload is a " + type(value).__name__
                                        + ", not an object")
    return value, None, None


# ----------------------------------------------------------------- this hook's own settings


def settings_path(named=None, environ=None, codex_home=None):
    """Where the settings are, with the same precedence the packaged launcher uses.

    The launcher passes the path it read as the first argument, so this process does not resolve
    it again in a different directory. What is deliberately NOT honoured is the settings override
    the rest of the repository accepts: a plugin declaration carries no settings argument, so a
    session inheriting that variable from a shell profile would be sent somewhere no install ever
    wrote, and a Stop that cannot find its settings releases in silence.
    """
    if named:
        return Path(named).expanduser()
    environ = os.environ if environ is None else environ
    home = codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")
    return Path(home).expanduser() / CONFIG_NAME


def complaints(document):
    """What is wrong with a settings document, named field by field.

    A readable file that says something this adapter cannot act on is not an unreadable file, and
    the two are different repairs.
    """
    found = []
    if not isinstance(document, dict):
        return ["the configuration is a " + type(document).__name__ + ", not an object"]
    if document.get("configVersion") != CONFIG_VERSION:
        found.append("configVersion must be " + str(CONFIG_VERSION) + ", found "
                     + repr(document.get("configVersion")))
    for field in ("relayExecutable", "markerRoot"):
        value = document.get(field)
        if not isinstance(value, str) or not value.strip():
            found.append(field + " must be a non-empty string")
        elif not os.path.isabs(value):
            found.append(field + " must be an absolute path, because this hook runs in the"
                                 " session's workspace and a relative path resolves there")
    database = document.get("dbPath")
    if database is not None and (not isinstance(database, str) or not database.strip()):
        found.append("dbPath must be a non-empty string when it is present at all")
    elif isinstance(database, str) and database.strip() and not os.path.isabs(database):
        found.append("dbPath must be an absolute path")
    served = document.get("socketPath")
    if served is not None and (not isinstance(served, str) or not served.strip()):
        found.append("socketPath must be a non-empty string when it is present at all")
    elif isinstance(served, str) and served.strip() and not os.path.isabs(served):
        # Absolute for the same reason as the others, and for one more: the relay canonicalises a
        # socket path before comparing it with the one a store recorded, so a relative spelling
        # would be resolved against the session's workspace and compared as a different socket.
        found.append("socketPath must be an absolute path")
    if document.get("mode") not in MODES:
        found.append("mode must be one of " + ", ".join(MODES))
    if document.get("mode") == HOLD:
        asserted = document.get("isolationAssertedBy")
        if not isinstance(asserted, str) or not asserted.strip():
            found.append("holding requires isolationAssertedBy to name who established that a"
                         " held child cannot write the facts the decision reads; observing"
                         " requires nothing, which is why it is the default")
    policy = document.get("journalPolicy")
    if policy is not None and policy not in JOURNAL_POLICIES:
        found.append("journalPolicy must be one of " + ", ".join(JOURNAL_POLICIES))
    owner = document.get("owner")
    if owner is not None and owner not in OWNERS:
        found.append("owner must be one of " + ", ".join(OWNERS)
                     + " when it is present at all, found " + repr(owner))
    for field in ("adapterInterpreter", "adapterEntryPoint"):
        value = document.get(field)
        if value is None:
            continue
        if not isinstance(value, str) or not value.strip():
            found.append(field + " must be a non-empty string when it is present at all")
        elif not os.path.isabs(value):
            found.append(field + " must be an absolute path")
    if owner == OWNER_PLUGIN:
        for field in ("adapterInterpreter", "adapterEntryPoint"):
            if not document.get(field):
                found.append(field + " is required when owner is " + OWNER_PLUGIN
                             + ": a registration declared by the plugin package cannot resolve"
                               " this repository's adapter, so the install records it here")
        budget = document.get("timeoutSeconds")
        if isinstance(budget, (int, float)) and not isinstance(budget, bool) \
                and budget >= LAUNCHER_CEILING_SECONDS:
            found.append("timeoutSeconds must be under " + str(LAUNCHER_CEILING_SECONDS)
                         + " when owner is " + OWNER_PLUGIN + ", because the packaged launcher"
                         " caps its own deadline there and has to outlast the adapter it runs")
    budget = document.get("timeoutSeconds")
    if budget is not None and not usable_seconds(budget):
        found.append("timeoutSeconds must be a positive number of seconds, at most "
                     + str(MAX_TIMEOUT_SECONDS))
    root = document.get("journalRoot")
    if root is not None and (not isinstance(root, str) or not root.strip()):
        found.append("journalRoot must be a non-empty string when it is present at all")
    elif isinstance(root, str) and root.strip() and not os.path.isabs(root):
        found.append("journalRoot must be an absolute path")
    return found


def _kind(mode):
    for predicate, name in ((stat_module.S_ISDIR, "directory"), (stat_module.S_ISSOCK, "socket"),
                            (stat_module.S_ISFIFO, "named pipe"),
                            (stat_module.S_ISBLK, "block device"),
                            (stat_module.S_ISCHR, "character device")):
        if predicate(mode):
            return name
    return "not a regular file"


def observe(path):
    """What is at this path, settled before anything is opened, or None for a regular file.

    Mirrors the checkout reader's first two steps. ABSENT is only for established absence: a check
    that could not be made is an access error, because "the check failed" and "there is nothing
    there" are different answers and they are different repairs.
    """
    try:
        found = os.lstat(str(path))
    except FileNotFoundError:
        return None, CONFIG_ABSENT, "nothing exists at " + str(path)
    except OSError as error:
        return None, CONFIG_UNREACHABLE, ("whether anything exists at " + str(path)
                                          + " could not be established: " + str(error))
    except ValueError as error:
        # A NUL-bearing string cannot name a path, so nothing was established either.
        return None, CONFIG_UNREACHABLE, str(path) + " cannot name a file: " + str(error)
    if stat_module.S_ISLNK(found.st_mode):
        try:
            found = os.stat(str(path))
        except FileNotFoundError:
            return None, CONFIG_UNREADABLE, ("the configuration at " + str(path)
                                             + " is a symbolic link whose target does not exist")
        except OSError as error:
            if error.errno == errno.ELOOP:
                return None, CONFIG_UNREADABLE, ("the configuration at " + str(path)
                                                 + " is a symbolic link that loops")
            return None, CONFIG_UNREACHABLE, ("the configuration at " + str(path)
                                              + " is a symbolic link whose target could not be"
                                                " resolved: " + str(error))
    if not stat_module.S_ISREG(found.st_mode):
        return None, CONFIG_UNREADABLE, ("the configuration at " + str(path) + " is a "
                                         + _kind(found.st_mode) + ", not a regular file")
    return None

def read_settings(path):
    """Absent, unreachable, unreadable and malformed stay four answers, because they are four repairs.

    This is the one place the checkout adapter reaches its reading module and this file cannot, so
    the module's ordered partition is mirrored here, INCLUDING the part that happens before any
    read. What is at the path is established with lstat first, and anything that is not a regular
    file is refused unopened.

    That order is not tidiness. A hook whose settings path is a named pipe would block this process
    on open until the host killed it, and a Stop that is killed mid-adapter releases with nothing
    recorded -- the one failure this adapter exists to avoid. The checkout reader refuses a FIFO,
    a socket, a device and a directory without opening any of them, and so does this.
    """
    settled = observe(path)
    if settled is not None:
        return settled
    try:
        raw = Path(path).read_bytes()
    except OSError as error:
        # Every OSError from here is an access error, because it established nothing about the
        # record. That is the module's rule, not a simplification of it.
        return None, CONFIG_UNREACHABLE, ("the configuration at " + str(path)
                                          + " could not be read: " + str(error))
    try:
        value = json.loads(raw.decode("utf-8"))
    except DECODE_FAILURES as error:
        return None, CONFIG_UNREADABLE, ("the configuration at " + str(path)
                                        + " could not be decoded: " + str(error))
    wrong = complaints(value)
    if wrong:
        return None, CONFIG_MALFORMED, "; ".join(wrong)
    return value, None, None


# ----------------------------------------------------------------- asking the guard


def guard_argv(config):
    """The call, built from the settings and from nothing else.

    --marker-root is always passed: the host process does not carry the coordinator's
    environment, so letting the relay resolve its own root would read every workspace as
    unmanaged. --db-path is passed only when configured, for the opposite reason: the guard
    prefers the path the coordinator recorded in its own intent. --now is never passed, because
    the time a decision is made is the guard's to observe.

    --socket is passed only when configured, and it is what lets the guard tell that the store it
    is about to read belongs to another App Server. A store records the socket it serves, so the
    comparison needs the socket this installation expects; with none configured there is nothing to
    compare, and an inherited state directory pointing at another installation's store is read as
    though it were this one's. It goes before the subcommand because it is a global option.
    """
    argv = [str(config["relayExecutable"])]
    if config.get("socketPath"):
        argv += ["--socket", str(config["socketPath"])]
    argv += [GUARD_COMMAND,
            "--marker-root", str(config["markerRoot"])]
    if config.get("dbPath"):
        argv += ["--db-path", str(config["dbPath"])]
    if config.get("mode") == HOLD:
        argv += ["--mode", HOLD]
    return argv


def _end_group(opened, group):
    """End the session this call started, then the process itself as a fallback."""
    for ending in (os.killpg, None):
        try:
            if ending is None:
                opened.kill()
            else:
                ending(group, 9)
            return
        except (OSError, AttributeError, ProcessLookupError):
            continue


def _text(raw):
    if not raw:
        return ""
    if isinstance(raw, bytes):
        return raw.decode("utf-8", "replace")
    return str(raw)


def invoke_guard(config, payload):
    """Run the guard and report how the process ended, without reading its output."""
    argv = guard_argv(config)
    budget = config.get("timeoutSeconds") or DEFAULT_TIMEOUT_SECONDS
    started = time.monotonic()
    try:
        opened = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            start_new_session=True,
        )
    except OSError as error:
        return {"ending": NOT_STARTED, "argv": argv, "code": None, "signal": None,
                "elapsedMs": round((time.monotonic() - started) * 1000),
                "stdout": "", "stderr": "",
                "errno": errno.errorcode.get(error.errno, error.errno),
                "detail": "the configured runtime could not be run: " + str(error)}
    # Saved while the leader is certainly alive and certainly leads the group: start_new_session
    # made its pid the group's id, and looking it up at kill time asks a process that may be gone.
    group = opened.pid
    sent = payload if isinstance(payload, bytes) else str(payload).encode("utf-8")
    try:
        out, err = opened.communicate(input=sent, timeout=budget)
    except subprocess.TimeoutExpired:
        _end_group(opened, group)
        # Draining is bounded by what is LEFT of the budget, never by the budget again: a second
        # full wait would take this to nearly twice the budget, which is the window where the
        # host kills the adapter and the timeout goes unrecorded.
        remaining = budget - (time.monotonic() - started)
        out, err = b"", b""
        if remaining > 0:
            try:
                out, err = opened.communicate(timeout=remaining)
            except subprocess.TimeoutExpired:
                out, err = b"", b""
        # Closed by hand, because the drain above is bounded and may have given up: a guard that
        # outlives its budget leaves this process holding its pipe ends open, and a Stop happens
        # on every turn. The group has already been ended; what is left is this side's handles.
        for stream in (opened.stdin, opened.stdout, opened.stderr):
            try:
                if stream is not None:
                    stream.close()
            except OSError:
                pass
        opened.poll()
        return {"ending": TIMED_OUT, "argv": argv, "code": None, "signal": None,
                "elapsedMs": round((time.monotonic() - started) * 1000),
                "stdout": _text(out), "stderr": _text(err),
                "detail": "the guard did not answer within " + str(budget)
                          + "s and its process group was ended"}
    code = opened.returncode
    return {
        "ending": SIGNALLED if code is not None and code < 0 else EXITED,
        "argv": argv,
        "code": None if code is None or code < 0 else code,
        "signal": None if code is None or code >= 0 else -code,
        "elapsedMs": round((time.monotonic() - started) * 1000),
        "stdout": _text(out),
        "stderr": _text(err),
        "detail": None,
    }


def read_guard_stdout(text):
    """What the guard's stdout said, as its own question."""
    if not (text or "").strip():
        return SAID_NOTHING, None
    try:
        value = json.loads(text)
    except ValueError:
        return SAID_SOMETHING_UNREADABLE, None
    if not isinstance(value, dict):
        return SAID_SOMETHING_UNREADABLE, None
    if "error" in value:
        return SAID_AN_ERROR_RECORD, value
    if "decision" in value:
        return SAID_A_VERDICT, value
    return SAID_SOMETHING_UNREADABLE, value


def outcome_of(ending, said, value):
    """One answer, derived from both cells and from neither cell alone.

    An exit of 2 carrying the relay's own error record is the relay refusing a request it
    understood; an exit of 2 carrying nothing is its argument parser rejecting the call before any
    command ran, which is what a runtime that does not offer this subcommand looks like.
    """
    how = ending.get("ending")
    if how == NOT_STARTED:
        return GUARD_UNREACHABLE
    if how == TIMED_OUT:
        return GUARD_TIMED_OUT
    if how == SIGNALLED:
        return GUARD_SIGNALLED
    code = ending.get("code")
    if said == SAID_SOMETHING_UNREADABLE:
        return GUARD_OUTPUT_UNREADABLE
    if said == SAID_AN_ERROR_RECORD:
        if code == GUARD_EXIT_REFUSED:
            return GUARD_REFUSED
        if code == GUARD_EXIT_HOST:
            return GUARD_HOST_ERROR
        if code == GUARD_EXIT_USAGE:
            return GUARD_USAGE_ERROR
        return GUARD_ENDED_UNEXPECTEDLY
    if said == SAID_NOTHING:
        if code == GUARD_EXIT_REFUSED:
            return GUARD_REJECTED_THE_CALL
        if code == GUARD_EXIT_OK:
            return GUARD_SAID_NOTHING
        return GUARD_ENDED_UNEXPECTEDLY
    if code != GUARD_EXIT_OK:
        return GUARD_ENDED_UNEXPECTEDLY
    if verdict_complaints(value):
        return GUARD_VERDICT_INCOMPLETE
    return GUARD_ANSWERED


def verdict_complaints(verdict):
    """Whether a verdict agrees with itself, asked before any part of it is acted on."""
    if not isinstance(verdict, dict):
        return ["the guard's answer is not an object"]
    answer = verdict.get("hook_output")
    decision = verdict.get("decision")
    if not isinstance(answer, dict):
        return ["the verdict carries no hook_output object"]
    if not answer:
        if decision == BLOCK:
            return ["the verdict holds and carries nothing for the host to act on"]
        if decision not in DECISIONS:
            return ["the verdict decides " + repr(decision) + ", which is neither answer this"
                    " contract has"]
        return []
    found = []
    if decision != BLOCK:
        found.append("the verdict decides " + repr(decision) + " while its hook_output holds")
    if answer.get("decision") != BLOCK:
        found.append("hook_output carries a decision this host does not accept: "
                     + repr(answer.get("decision")))
    if answer.get("continue") is not True:
        found.append("hook_output does not ask for a continuation")
    reason = answer.get("reason")
    if not isinstance(reason, str) or not reason.strip():
        found.append("a block carrying no prompt is reported by the host as a failed run")
    return found


def hook_output(verdict):
    """The Stop JSON to print, rebuilt from validated fields rather than passed through."""
    answer = (verdict or {}).get("hook_output")
    if verdict_complaints(verdict) or not answer:
        return None
    return json.dumps({"decision": BLOCK, "reason": answer["reason"], "continue": True})


# ----------------------------------------------------------------- which Stop event this is


def _transcript_refusal(path):
    """None for a regular file, else why the transcript will not be opened.

    Settled with lstat before any open, for the reason read_settings settles its own path first: a
    named pipe would block this process on open until the host killed it, and a Stop killed
    mid-adapter releases with nothing recorded.
    """
    try:
        found = os.lstat(path)
    except FileNotFoundError:
        return TRANSCRIPT_ABSENT
    except (OSError, ValueError):
        return TRANSCRIPT_UNREACHABLE
    if stat_module.S_ISLNK(found.st_mode):
        try:
            found = os.stat(path)
        except FileNotFoundError:
            return TRANSCRIPT_ABSENT
        except (OSError, ValueError):
            return TRANSCRIPT_UNREACHABLE
    if not stat_module.S_ISREG(found.st_mode):
        return TRANSCRIPT_NOT_REGULAR
    return None


def _answer_text(item):
    parts = item.get("content")
    if not isinstance(parts, list):
        return None
    return "".join(part["text"] for part in parts
                   if isinstance(part, dict) and part.get("type") == "Text"
                   and isinstance(part.get("text"), str))


def _classify(row, turn):
    """What one transcript record says about this turn: an answer, an input, the turn's start, or
    nothing (None)."""
    if not isinstance(row, dict) or row.get("type") != "event_msg":
        return None
    body = row.get("payload")
    if not isinstance(body, dict) or body.get("turn_id") != turn:
        return None
    if body.get("type") == "task_started":
        return ("start",)
    item = body.get("item")
    if body.get("type") != "item_completed" or not isinstance(item, dict):
        return None
    if item.get("type") in INPUT_ITEMS:
        return ("input", item.get("type"))
    if item.get("type") == "AgentMessage":
        return ("answer", item.get("id"), _answer_text(item), body.get("thread_id"))
    return None


def _turn_items(handle, turn, identity, started):
    """This turn's answers and inputs, newest first, read backwards to the turn's start, or a reason.

    Everything back to the start is needed, not just the newest answer: whether an earlier Stop of
    the same turn reported the same text decides whether this invocation can be told apart from a
    late delivery of that earlier Stop. Lines are assembled from pieces, so a line longer than a
    chunk costs its own length once rather than once per chunk.

    Nothing that could be one of this turn's items is read past. A last line without its newline is
    one the host is still writing, and any line naming this turn that does not parse could be an
    input; either leaves the identity unestablished rather than falling back to an older answer.
    Only lines carrying the turn id are parsed, and those are the host's small event records: on
    the host this was built on, every such line of a 62 MB turn parsed in about 10 ms.
    """
    token = turn.encode("utf-8", "surrogatepass")
    size = os.fstat(handle).st_size
    if size == 0:
        return None, NO_ANSWER_ITEM_FOR_TURN
    os.lseek(handle, size - 1, os.SEEK_SET)
    unfinished = os.read(handle, 1) != b"\n"
    items = []
    position = size
    pieces = []
    newest = True
    while position > 0:
        if identity["scannedBytes"] >= SCAN_MAX_BYTES:
            return None, SCAN_BOUND_EXCEEDED
        if time.monotonic() - started > SCAN_MAX_SECONDS:
            return None, SCAN_TIMED_OUT
        width = min(SCAN_CHUNK_BYTES, position)
        position -= width
        os.lseek(handle, position, os.SEEK_SET)
        chunk = b""
        while len(chunk) < width:
            piece = os.read(handle, width - len(chunk))
            if not piece:
                break
            chunk += piece
        identity["scannedBytes"] += len(chunk)
        segments = chunk.split(b"\n")
        if len(segments) == 1:
            # No line ends in this chunk: it is the front of the line the pieces carry.
            pieces.insert(0, chunk)
            if position > 0:
                continue
            lines, pieces = [b"".join(pieces)], []
        else:
            lines = [segments[-1] + b"".join(pieces)] + segments[-2:0:-1]
            if position == 0:
                lines.append(segments[0])
                pieces = []
            else:
                pieces = [segments[0]]
        for line in lines:
            identity["scannedLines"] += 1
            if newest:
                newest = False
                if unfinished:
                    return None, TRANSCRIPT_TAIL_INCOMPLETE
                continue
            if token not in line:
                continue
            try:
                found = _classify(json.loads(line.decode("utf-8")), turn)
            except ValueError:
                return None, TRANSCRIPT_LINE_UNREADABLE
            if found is None:
                continue
            if found[0] == "start":
                return items, None
            items.append(found)
    # The file began without this turn's start. Whatever came before it is not here, so an earlier
    # Stop of the turn may be missing too, and an unseen Stop is what a late delivery hides behind.
    return None, TURN_START_NOT_FOUND


def event_identity(stop, started=None):
    """Which Stop event this invocation answers, as (key, identity).

    The key is None when the identity could not be established, and identity["reason"] says why.
    An established event is (session_id, turn_id, stop_hook_active, answer item id), where the
    answer is the one Stop of this turn the transcript shows reporting the payload's text under the
    payload's stop_hook_active: the newest answer for the live Stop, an earlier Stop's own answer
    for a late delivery of it. It is established only when the latest sampling's answer is
    recorded, exactly one Stop matches, and that answer's recorded thread is the session. The key
    is a SHA-256 over the four values, so no host value becomes a path and nothing is minted here.
    """
    started = time.monotonic() if started is None else started
    identity = {"established": False, "reason": None, "answerItem": None,
                "transcriptPath": None, "scannedBytes": 0, "scannedLines": 0}
    session, turn = stop.get("session_id"), stop.get("turn_id")
    active, said = stop.get("stop_hook_active"), stop.get("last_assistant_message")
    if not (isinstance(session, str) and session and isinstance(turn, str) and turn
            and isinstance(active, bool) and isinstance(said, str)):
        identity["reason"] = IDENTITY_FIELDS_INCOMPLETE
        return None, identity
    path = stop.get("transcript_path")
    if not isinstance(path, str) or not path:
        identity["reason"] = TRANSCRIPT_PATH_MISSING
        return None, identity
    identity["transcriptPath"] = path
    if not os.path.isabs(path):
        identity["reason"] = TRANSCRIPT_PATH_RELATIVE
        return None, identity
    refused = _transcript_refusal(path)
    if refused is not None:
        identity["reason"] = refused
        return None, identity
    try:
        handle = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0)
                         | getattr(os, "O_NOCTTY", 0))
    except FileNotFoundError:
        identity["reason"] = TRANSCRIPT_ABSENT
        return None, identity
    except (OSError, ValueError):
        identity["reason"] = TRANSCRIPT_UNREACHABLE
        return None, identity
    try:
        if not stat_module.S_ISREG(os.fstat(handle).st_mode):
            items, reason = None, TRANSCRIPT_NOT_REGULAR
        else:
            items, reason = _turn_items(handle, turn, identity, started)
    except OSError:
        items, reason = None, TRANSCRIPT_UNREACHABLE
    finally:
        os.close(handle)
    if items is None:
        identity["reason"] = reason
        return None, identity
    if not items or not any(entry[0] == "answer" for entry in items):
        identity["reason"] = NO_ANSWER_ITEM_FOR_TURN
        return None, identity
    if items[0][0] != "answer":
        # The latest sampling's answer is not recorded. This may be its own Stop, whose answer the
        # transcript does not show yet, or a late delivery of an earlier one; nothing tells them
        # apart, so nothing is claimed.
        identity["reason"] = ANSWER_PRECEDES_LATEST_INPUT
        return None, identity
    # The Stops of this turn, as the transcript shows them: the last answer before each later input,
    # and the newest answer, which is the latest sampling's. A Stop's stop_hook_active is true once
    # a hook continuation has happened in the turn, so each one's flag is whether a continuation
    # prompt precedes it. The live Stop reports the newest answer; a late delivery reports its own.
    # The event is the one Stop whose text and flag are what this payload reports. Two such Stops
    # cannot be told apart, and none means the payload is not about anything recorded here.
    chronological = list(reversed(items))
    matching = []
    for index, entry in enumerate(chronological):
        if entry[0] != "answer":
            continue
        if index + 1 < len(chronological) and chronological[index + 1][0] != "input":
            continue
        flag = any(prior[0] == "input" and prior[1] == HOOK_PROMPT
                   for prior in chronological[:index])
        if entry[2] == said and flag == active:
            matching.append(entry)
    if not matching:
        identity["reason"] = ANSWER_TEXT_MISMATCH
        return None, identity
    if len(matching) > 1:
        identity["reason"] = ANSWER_TEXT_AMBIGUOUS
        return None, identity
    _kind, item_id, _text, thread = matching[0]
    if not isinstance(item_id, str) or not item_id:
        identity["reason"] = ANSWER_ITEM_UNIDENTIFIED
        return None, identity
    identity["answerItem"] = item_id
    if thread != session:
        identity["reason"] = SESSION_MISMATCH
        return None, identity
    identity["established"] = True
    return event_key(session, turn, active, item_id), identity


def event_key(session, turn, active, item):
    """The key of one Stop event: a SHA-256 over the four values that identify it.

    One function for the adapter that claims and the reader that checks, so a record whose own
    session, turn, stop_hook_active and answer item do not hash to the key it is filed under is
    noticed rather than read as a record of that event.
    """
    return hashlib.sha256(json.dumps([EVENT_KEY_TAG, session, turn, active, item],
                                     separators=(",", ":")).encode("ascii")).hexdigest()


def new_slot():
    """Where this invocation's row will go, chosen before anything names it."""
    return datetime.now(timezone.utc).strftime("%Y%m%d"), uuid.uuid4().hex


def slot_name(slot):
    return slot[0] + "/" + slot[1] + ".json"


def _write_whole(handle, document):
    payload = (json.dumps(document, sort_keys=True, default=str) + "\n").encode("utf-8")
    written = 0
    while written < len(payload):
        written += os.write(handle, payload[written:])


def host_ledger(codex_home=None, environ=None):
    """Where this host's registrations arbitrate a Stop event, whatever journal root each keeps.

    The Codex home of the process the Stop fired in, resolved the way the settings are when no
    path is named: every registration the host starts for one Stop inherits that environment,
    while the settings each one reads, and so its journal root, may differ.
    """
    environ = os.environ if environ is None else environ
    home = codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")
    # Absolute, because the claim names it and a reading may run from any directory.
    return Path(os.path.abspath(str(Path(home).expanduser()))).joinpath(*HOST_LEDGER_PARTS)


def _arbitrate(host, key, identity, stop, slot, root):
    """The host-wide half of a claim: one create-once file per event under the Codex home.

    Returns (None, None) when this invocation is the first on this host to reach the event,
    (DUPLICATE, the file) when another already has, and (UNARBITRATED, None) when the file can be
    neither created nor found. A claim in a journal root alone lets two registrations with two
    roots each accept the same Stop; this file is the one they both meet, and only the invocation
    that made it may ask the guard. When nobody can make it, nobody asks. Like the claim, it is
    never rewritten and a short write leaves it in place.
    """
    directory = Path(host)
    try:
        directory.mkdir(parents=True, exist_ok=True)
    except (OSError, ValueError):
        return UNARBITRATED, None
    marker = directory / (key + ".json")
    try:
        handle = os.open(str(marker), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        # Named relative to the Codex home, as the accepted record is relative to its root.
        return DUPLICATE, "/".join(HOST_LEDGER_PARTS + (key + ".json",))
    except (OSError, ValueError):
        return UNARBITRATED, None
    try:
        _write_whole(handle, {"ledgerVersion": LEDGER_VERSION, "eventKey": key,
                              "sessionId": stop.get("session_id"),
                              "turnId": stop.get("turn_id"),
                              "stopHookActive": stop.get("stop_hook_active"),
                              "answerItem": identity.get("answerItem"), "claimedAt": now(),
                              "claimedBy": {"pid": os.getpid(),
                                            "journalRoot": str(root) if root else None,
                                            "attemptRow": slot_name(slot)}})
    except (OSError, ValueError):
        pass
    finally:
        os.close(handle)
    return None, None


def claim_event(config, key, identity, stop, slot, host=None):
    """Create this event's accepted record, or learn that another invocation already has.

    Returns (acceptance, acceptedAs). Creating a file that must not exist is the whole mechanism:
    two registrations firing in the same instant get one owner. The host's file (host, from
    host_ledger) is created first, whatever the settings say, so registrations whose settings name
    different journal roots, or none, still get one owner; the owner then creates the accepted
    record in its own root, where the reading of the journal finds it beside the rows, and names
    the host's ledger in it so the reading can find that too. Only the owner of the host's file
    goes on to ask the guard (UNCLAIMABLE and CLAIM_FAILED are owners whose root could not hold the
    record). Neither file is ever rewritten, and a short write leaves it where it is, because
    removing it would open the event to a second acceptance. Written under every journalPolicy:
    this is state, not a record of an invocation.
    """
    root = config.get("journalRoot")
    if host is not None:
        arbitrated, where = _arbitrate(host, key, identity, stop, slot, root)
        if arbitrated is not None:
            return arbitrated, where
    if not root:
        return UNCLAIMABLE, None
    directory = Path(root).expanduser() / LEDGER_DIRECTORY
    named = LEDGER_DIRECTORY + "/" + key + ".json"
    try:
        # Its own step, so a path that exists as something other than a directory reads as a
        # failed claim and never as the FileExistsError below, which means "already accepted".
        directory.mkdir(parents=True, exist_ok=True)
    except (OSError, ValueError):
        return CLAIM_FAILED, None
    try:
        handle = os.open(str(directory / (key + ".json")),
                         os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        return DUPLICATE, named
    except (OSError, ValueError):
        return CLAIM_FAILED, None
    try:
        _write_whole(handle, {"ledgerVersion": LEDGER_VERSION, "eventKey": key,
                              "sessionId": stop.get("session_id"),
                              "turnId": stop.get("turn_id"),
                              "stopHookActive": stop.get("stop_hook_active"),
                              "answerItem": identity.get("answerItem"), "claimedAt": now(),
                              "claimedBy": {"pid": os.getpid(), "attemptRow": slot_name(slot),
                                            "hostLedger": str(host) if host is not None
                                            else None}})
    except (OSError, ValueError):
        pass
    finally:
        os.close(handle)
    return ACCEPTED, named


def record_outcome(config, key, record, row):
    """What the accepted event was answered with, as a second create-once file beside its claim.

    Written after the row, naming it (row is its position under the journal root, or None when the
    policy wrote none). A claim with no outcome is an event whose owner died before answering, and
    a reading of the journal can say so instead of passing it. Unlike the claim, a short write is
    removed: its absence already says "outcome unrecorded", and a torn file would say less.
    """
    root = config.get("journalRoot")
    if not root or not key:
        return None
    target = Path(root).expanduser() / LEDGER_DIRECTORY / (key + OUTCOME_SUFFIX)
    try:
        handle = os.open(str(target), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except (OSError, ValueError):
        return None
    try:
        _write_whole(handle, {"ledgerVersion": LEDGER_VERSION, "eventKey": key,
                              "sessionId": record.get("sessionId"),
                              "turnId": record.get("turnId"),
                              "journalPolicy": config.get("journalPolicy") or EVERY_INVOCATION,
                              "adapterOutcome": record.get("adapterOutcome"),
                              "guardDecision": record.get("guardDecision"),
                              "guardState": record.get("guardState"),
                              "held": record.get("held"), "attemptRow": row, "at": now()})
    except (OSError, ValueError):
        os.close(handle)
        try:
            os.unlink(str(target))
        except OSError:
            pass
        return None
    os.close(handle)
    return LEDGER_DIRECTORY + "/" + key + OUTCOME_SUFFIX


# ----------------------------------------------------------------- this hook's own record


def journal(config, record, slot=None):
    """Append one record of this invocation, under a name nothing else can take.

    The name is the slot run() chose when it started, so an accepted record that names this row
    names the day it is actually under, even when the invocation crosses midnight.
    """
    policy = config.get("journalPolicy") or EVERY_INVOCATION
    if policy == NO_JOURNAL:
        return None
    if (policy == FAULTS_ONLY and record.get("adapterOutcome") in QUIET
            and record.get("acceptance") in (ACCEPTED, DUPLICATE)):
        # An answer to an event this hook could identify is not a fault. An answer given without
        # an identity is reported even here: it is the one invocation no accepted record covers.
        return None
    root = config.get("journalRoot")
    if not root:
        return None
    day, name = slot or new_slot()
    directory = Path(root).expanduser() / day
    target = directory / (name + ".json")
    try:
        directory.mkdir(parents=True, exist_ok=True)
        handle = os.open(str(target), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        try:
            # Written to completion, and removed if it cannot be: os.write may write fewer bytes
            # than it was given, and a truncated record survives under a name nothing will reuse
            # and is counted as an invocation whose contents no longer read back.
            payload = (json.dumps(record, sort_keys=True, default=str) + "\n").encode("utf-8")
            written = 0
            while written < len(payload):
                written += os.write(handle, payload[written:])
        finally:
            os.close(handle)
    except (OSError, ValueError):
        try:
            os.unlink(str(target))
        except OSError:
            pass
        return None
    return str(target)


def _release(config, record, outcome, detail, started, slot=None):
    record["adapterOutcome"] = outcome
    record["detail"] = detail
    record["elapsedMs"] = round((time.monotonic() - started) * 1000)
    record["journalledAs"] = journal(config, record, slot)
    return None


def run(payload, codex_home=None, environ=None, settings=None):
    """Decide one Stop and return the text to print, or None.

    Never raises and never holds on its own. Every path ends in a recorded outcome and a release,
    except the one where the guard itself decided to hold.
    """
    started = time.monotonic()
    slot = new_slot()
    record = {"recordVersion": RECORD_VERSION, "event": EVENT, "at": now(),
              "adapterOutcome": None, "processEnding": None, "stdoutReading": None,
              "guardState": None, "guardDecision": None, "guardMode": None,
              "assignmentId": None, "guardRecordedAs": None, "held": False,
              "eventKey": None, "eventIdentity": None, "identityScanMs": None,
              "acceptance": None, "acceptedAs": None, "guardInvoked": False}
    config = {}
    # The event whose accepted record this invocation created, if any. Whatever happens after
    # that, this invocation owes the event an outcome record.
    claimed = None
    try:
        # The settings are read FIRST, before the payload is looked at, because they are what
        # says where a record goes: reading them second means a payload this hook could not parse
        # is released with nothing written down anywhere.
        path = settings_path(settings, environ, codex_home)
        record["configuration"] = str(path)
        config, failed, detail = read_settings(path)
        if failed is not None:
            return _release(config or {}, record, failed, detail, started, slot)
        stop, payload_failed, payload_detail = stop_input(payload)
        if payload_failed is not None:
            return _release(config, record, payload_failed, payload_detail, started, slot)
        record["sessionId"] = stop.get("session_id")
        record["turnId"] = stop.get("turn_id")
        record["stopHookActive"] = stop.get("stop_hook_active")
        record["guardMode"] = config.get("mode")
        scanning = time.monotonic()
        key, identity = event_identity(stop, scanning)
        record["identityScanMs"] = round((time.monotonic() - scanning) * 1000)
        record["eventKey"] = key
        record["eventIdentity"] = identity
        if key is None:
            # Not knowing which event this is means not knowing it was answered, so it is asked
            # about exactly as before and never deduplicated.
            record["acceptance"] = UNESTABLISHED
        else:
            acceptance, accepted_as = claim_event(config, key, identity, stop, slot,
                                                  host_ledger(codex_home, environ))
            record["acceptance"] = acceptance
            record["acceptedAs"] = accepted_as
            if acceptance == DUPLICATE:
                return _release(config, record, DUPLICATE_INVOCATION,
                                "this Stop event already has its accepted record, so the guard"
                                " was not asked again", started, slot)
            if acceptance == UNARBITRATED:
                return _release(config, record, ARBITRATION_FAILED,
                                "the host's record of this Stop event could be neither made nor"
                                " found, so no invocation can own it and the guard was not asked",
                                started, slot)
            if acceptance == ACCEPTED:
                claimed = key
        record["guardInvoked"] = True
        ending = invoke_guard(config, payload)
        said, value = read_guard_stdout(ending.get("stdout"))
        record["processEnding"] = ending.get("ending")
        record["exitCode"] = ending.get("code")
        record["signal"] = ending.get("signal")
        record["errno"] = ending.get("errno")
        record["stdoutReading"] = said
        record["guardElapsedMs"] = ending.get("elapsedMs")
        record["guardStderr"] = (ending.get("stderr") or "")[:400]
        outcome = outcome_of(ending, said, value)
        if outcome in ANSWERED:
            record["guardState"] = value.get("state")
            record["guardDecision"] = value.get("decision")
            record["observation"] = value.get("observation")
            record["assignmentId"] = value.get("assignmentId")
            record["guardRecordedAs"] = value.get("recordedAs")
            record["counters"] = value.get("counters")
        answer = hook_output(value) if outcome in ANSWERED else None
        record["adapterOutcome"] = outcome
        record["detail"] = ending.get("detail")
        record["held"] = answer is not None
        record["elapsedMs"] = round((time.monotonic() - started) * 1000)
        record["journalledAs"] = journal(config, record, slot)
        if claimed is not None:
            record_outcome(config, claimed, record,
                           slot_name(slot) if record["journalledAs"] else None)
        return answer
    except BaseException as error:  # noqa: BLE001 - a detector that dies must still release
        record["adapterOutcome"] = ADAPTER_FAULTED
        record["fault"] = type(error).__name__ + ": " + str(error)
        record["elapsedMs"] = round((time.monotonic() - started) * 1000)
        written = None
        try:
            written = journal(config or {}, record, slot)
        except BaseException:
            pass
        if claimed is not None:
            try:
                record_outcome(config or {}, claimed, record,
                               slot_name(slot) if written else None)
            except BaseException:
                pass
        return None


def main():
    """The console script. Parses nothing, says nothing on stderr, and always exits 0.

    The settings path arrives positionally because the packaged launcher already resolved it, and
    an argument parser is exactly what must not appear here: argparse exits 2 on anything it does
    not recognise, and the host reads exit 2 as a request to hold the turn.
    """
    try:
        payload = sys.stdin.buffer.read()
    except BaseException:
        # The payload could not be read at all. run() is still called, with nothing, so the
        # invocation is classified and recorded rather than vanishing.
        payload = None
    named = sys.argv[1] if len(sys.argv) > 1 else None
    answer = run(payload, settings=named)
    if answer:
        sys.stdout.write(answer)


if __name__ == "__main__":
    try:
        main()
    except BaseException:
        # Deliberately bare and deliberately silent. Any escape here would be reported to the
        # host as a failed hook run at best, and as a blocking exit code at worst.
        pass
    raise SystemExit(0)
