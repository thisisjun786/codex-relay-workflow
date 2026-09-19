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

It never answers one question with another question's reading. "the guard released" and "the
guard could not be asked" are different values, and so are "the runtime refused the request" and
"the runtime rejected the call before it ran".

Rules: skills/crw-run/references/hook-contract.md.
"""

import errno
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
    """
    argv = [str(config["relayExecutable"]), GUARD_COMMAND,
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


# ----------------------------------------------------------------- this hook's own record


def journal(config, record):
    """Append one record of this invocation, under a name nothing else can take."""
    policy = config.get("journalPolicy") or EVERY_INVOCATION
    if policy == NO_JOURNAL:
        return None
    if policy == FAULTS_ONLY and record.get("adapterOutcome") in ANSWERED:
        return None
    root = config.get("journalRoot")
    if not root:
        return None
    directory = Path(root).expanduser() / datetime.now(timezone.utc).strftime("%Y%m%d")
    target = directory / (uuid.uuid4().hex + ".json")
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


def _release(config, record, outcome, detail, started):
    record["adapterOutcome"] = outcome
    record["detail"] = detail
    record["elapsedMs"] = round((time.monotonic() - started) * 1000)
    record["journalledAs"] = journal(config, record)
    return None


def run(payload, codex_home=None, environ=None, settings=None):
    """Decide one Stop and return the text to print, or None.

    Never raises and never holds on its own. Every path ends in a recorded outcome and a release,
    except the one where the guard itself decided to hold.
    """
    started = time.monotonic()
    record = {"recordVersion": 1, "event": EVENT, "at": now(),
              "adapterOutcome": None, "processEnding": None, "stdoutReading": None,
              "guardState": None, "guardDecision": None, "guardMode": None,
              "assignmentId": None, "guardRecordedAs": None, "held": False}
    config = {}
    try:
        # The settings are read FIRST, before the payload is looked at, because they are what
        # says where a record goes: reading them second means a payload this hook could not parse
        # is released with nothing written down anywhere.
        path = settings_path(settings, environ, codex_home)
        record["configuration"] = str(path)
        config, failed, detail = read_settings(path)
        if failed is not None:
            return _release(config or {}, record, failed, detail, started)
        stop, payload_failed, payload_detail = stop_input(payload)
        if payload_failed is not None:
            return _release(config, record, payload_failed, payload_detail, started)
        record["sessionId"] = stop.get("session_id")
        record["turnId"] = stop.get("turn_id")
        record["stopHookActive"] = stop.get("stop_hook_active")
        record["guardMode"] = config.get("mode")
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
        record["journalledAs"] = journal(config, record)
        return answer
    except BaseException as error:  # noqa: BLE001 - a detector that dies must still release
        record["adapterOutcome"] = ADAPTER_FAULTED
        record["fault"] = type(error).__name__ + ": " + str(error)
        record["elapsedMs"] = round((time.monotonic() - started) * 1000)
        try:
            journal(config or {}, record)
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
