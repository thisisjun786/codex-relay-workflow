"""The Stop hook adapter: the host's event in, the relay guard's answer out.

The decision this hook delivers is not made here. skills/crw-run/references/hook-contract.md
fixes the rules and codex_session_relay.guard implements them, down to the exact Stop JSON a
hook should print. What was missing was the piece between the host and that guard, and this is
it: read the payload the host delivers, ask the configured runtime, print what it answered.

Three things it will not do, each of them a way a hook turns into an outage.

It never decides a turn itself. Only a verdict the guard produced can reach stdout, so a defect
here degrades into no hook rather than into a stuck session.

It never exits 2. The host reads exit 2 as the blocking code and takes stderr as the
continuation prompt, and argparse exits 2 on any usage error, so nothing here parses arguments,
nothing here writes to stderr, and the subprocess's streams are captured rather than inherited.
A stale flag in somebody's hook file must not become a hold on every ordinary turn.

It never answers one question with another question's reading. "the guard released" and "the
guard could not be asked" are different values, and so are "the runtime refused the request" and
"the runtime rejected the call before it ran": those are different repairs, and a single
failure value would send the operator to the wrong one.

What it does not observe, it does not infer. The Stop path reads the marker and a read-only
database, so a stopped daemon is not visible from here and is never guessed at. Registration,
the runtime actually offering the subcommand, and this hook having fired are likewise three
separate questions, answered separately by status().
"""

import errno
import json
import os
import re
import shlex
import subprocess
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

from . import hooks, hostrecord, pointer, reading

# The event whose contract this adapter implements. Stop is the host's response-turn boundary,
# and it is the only event whose output schema carries a blocking decision at all.
EVENT = "Stop"

# This hook's own configuration, owned by this hook and read by nothing else. Deliberately a
# separate file from any other hook's: sharing one would make two owners of one record, and an
# operator turning this hook off would be editing somebody else's settings.
CONFIG_NAME = "crw-completion-hook.json"
CONFIG_ENV = "CRW_COMPLETION_HOOK_CONFIG"
CONFIG_VERSION = 1

# The registered command's file name. Named here because two readers need it: the installer
# derives the command from it, and status() recognises this adapter's own registrations by it.
ENTRY_POINT_NAME = "completion_hook.py"

# The relay subcommand that owns the decision.
GUARD_COMMAND = "guard-evaluate"

# The two modes the guard offers. Observe classifies and records and never holds, and it is the
# default here for the same reason it is the default there: holding depends on per-session write
# isolation the caller has to have granted, and a hook that cannot establish that grant and
# holds anyway is outside the contract.
OBSERVE = "observe"
HOLD = "hold"
MODES = (OBSERVE, HOLD)

# How much this hook records about itself. Every invocation is the default because the guard
# records only when it selected an assignment: on a host with no managed session it writes
# nothing at all, and then "no firing evidence" would be indistinguishable from "this hook never
# runs". The policy is reported by status() so a small count is never read as a small number of
# invocations.
EVERY_INVOCATION = "every_invocation"
FAULTS_ONLY = "faults_only"
NO_JOURNAL = "no_journal"
JOURNAL_POLICIES = (EVERY_INVOCATION, FAULTS_ONLY, NO_JOURNAL)

# The self-imposed wall clock, well under the timeout the hook is registered with, so that a
# database somebody is holding cannot spend the host's whole budget.
DEFAULT_TIMEOUT_SECONDS = 5
REGISTERED_TIMEOUT_SECONDS = 10


def budget_complaints(guard_timeout, registered_timeout):
    """Whether the adapter's own budget can do what it is for, against the host's.

    The two numbers only mean something together. A budget the host's timeout does not exceed
    lets the host kill the adapter mid-call, and the evidence that would have explained the
    timeout is the record the killed process was about to write. Stated as its own check because
    the settings hold one number and the hook registration holds the other, so neither reader
    can see the relationship on its own.
    """
    found = []
    if isinstance(guard_timeout, bool) or not isinstance(guard_timeout, (int, float)) \
            or guard_timeout <= 0:
        found.append("the guard budget must be a positive number of seconds")
    elif (isinstance(registered_timeout, (int, float))
            and not isinstance(registered_timeout, bool)
            and guard_timeout >= registered_timeout):
        found.append("the guard budget must be under the registered hook timeout of "
                     + str(registered_timeout) + "s, or the host can kill this adapter before"
                     " it records why it did not answer")
    return found


def registration_complaints(event):
    """Whether a registration this command is about to make is one this adapter can act on.

    The third member of a class the first two arrived in separately: settings this command can
    write but its own reader rejects, a budget the host's timeout does not exceed, and now an
    event whose contract this adapter does not implement. Each one installs successfully and
    leaves a hook that is registered, inert, and silent about it, so the check belongs to the
    registration rather than to any one flag.

    Stop is the only event this adapter has a decision for. The guard judges a turn ending, and
    on every other event the payload means something else and the output schema has no
    top-level decision to carry an answer, so a hook registered elsewhere could never see the
    turn it was installed to watch.
    """
    if event is not None and event != EVENT:
        return ["this adapter implements the " + EVENT + " contract and has no decision for "
                + str(event) + "; register an explicit --hook-command for another event"]
    return []

# How the guard process ended. Four answers and none of them is inferred from the output: a
# process that never started and one that started and said nothing are the same silence on
# stdout and completely different repairs.
NOT_STARTED = "not_started"
EXITED = "exited"
SIGNALLED = "signalled"
TIMED_OUT = "timed_out"
PROCESS_ENDINGS = (NOT_STARTED, EXITED, SIGNALLED, TIMED_OUT)

# What its stdout said, read as its own question. Kept apart from the ending above because the
# pairs carry the information: an exit of 2 with an error record is the relay refusing a request
# it understood, and an exit of 2 with nothing is its argument parser rejecting the call.
SAID_NOTHING = "said_nothing"
SAID_A_VERDICT = "said_a_verdict"
SAID_AN_ERROR_RECORD = "said_an_error_record"
SAID_SOMETHING_UNREADABLE = "said_something_unreadable"
STDOUT_READINGS = (SAID_NOTHING, SAID_A_VERDICT, SAID_AN_ERROR_RECORD,
                   SAID_SOMETHING_UNREADABLE)

# The relay's own exit codes, named so a reader does not have to recognise a number.
GUARD_EXIT_OK = 0
GUARD_EXIT_REFUSED = 2
GUARD_EXIT_HOST = 3
GUARD_EXIT_USAGE = 4

# Every answer this adapter gives about one Stop. Each one releases the turn: this adapter has
# no opinion of its own to enforce, and a fault in it must never cost a turn.
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

OUTCOMES = (STDIN_UNREADABLE, STDIN_NOT_JSON, STDIN_NOT_OBJECT, CONFIG_ABSENT,
            CONFIG_UNREADABLE, CONFIG_UNREACHABLE, CONFIG_MALFORMED, GUARD_UNREACHABLE,
            GUARD_TIMED_OUT, GUARD_SIGNALLED, GUARD_REJECTED_THE_CALL, GUARD_REFUSED,
            GUARD_HOST_ERROR, GUARD_USAGE_ERROR, GUARD_ENDED_UNEXPECTEDLY, GUARD_SAID_NOTHING,
            GUARD_OUTPUT_UNREADABLE, GUARD_VERDICT_INCOMPLETE, GUARD_ANSWERED, ADAPTER_FAULTED)

# The only outcome in which the guard actually reached a decision. Named as a set rather than
# tested against one member, because "did the guard answer" and "did it say block" are two
# questions and folding them would let a missing answer read as a release.
ANSWERED = (GUARD_ANSWERED,)

# The reading states this adapter reports its configuration with. Taken from the module that
# owns the partition instead of respelling three of its four members here, so a state added
# there cannot silently fall through to a default.
CONFIG_OUTCOMES = {
    reading.ABSENT: CONFIG_ABSENT,
    reading.UNREADABLE: CONFIG_UNREADABLE,
    reading.ACCESS_ERROR: CONFIG_UNREACHABLE,
}

# What the guard puts in hook_output when it holds. The only value the host accepts for a
# top-level Stop decision.
BLOCK = "block"

# The name this writer gives a journal record: a random hex name and nothing else. Matched
# rather than assumed when counting, so a file somebody else left under the journal root is
# never counted as an invocation this hook recorded.
JOURNAL_NAME = re.compile(r"^[0-9a-f]{32}\.json$")
JOURNAL_DAY = re.compile(r"^[0-9]{8}$")

# The answers a configuration write gives about the file it found. The same shape hooks.install
# uses, in this module's own words rather than its words, so an answer about these settings can
# never be read as an answer about the hook file. The reason for the shape is the same one: a
# write that cannot say "there was nothing there" cannot tell a first install from one that
# found somebody else's file.
CONFIG_CREATED = "config_created"
CONFIG_UNCHANGED = "config_unchanged"
CONFIG_WOULD_CREATE = "config_would_create"
CONFIG_DIFFERS = "config_differs"
CONFIG_CHANGED_UNDERNEATH = "config_changed_underneath"
# The writer refusing to write a document its own reader would reject. Without it an install
# reports success and every Stop afterwards reads the settings it just wrote as malformed,
# which is a hook that is registered, inert, and says so nowhere anybody looks.
CONFIG_WOULD_NOT_BE_READABLE = "config_would_not_be_readable"
CONFIG_WRITE_OUTCOMES = (CONFIG_CREATED, CONFIG_UNCHANGED, CONFIG_WOULD_CREATE, CONFIG_DIFFERS,
                         CONFIG_CHANGED_UNDERNEATH, CONFIG_WOULD_NOT_BE_READABLE)

# The outcomes that mean these settings now say what this install asked them to, or would with
# --apply. Everything else, including every unusable reading, is a refusal, and the set is named
# here so an exit status is derived from it rather than from a list kept equal by hand.
CONFIG_SETTLED = (CONFIG_CREATED, CONFIG_UNCHANGED, CONFIG_WOULD_CREATE)

# What status() answers with when it did not ask. Distinct from an absence, which is an answer.
NOT_READ = "not_read"


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


# ------------------------------------------------------------------ the delivered payload


def stop_input(payload):
    """Read the Stop payload. Three distinct failures, and no field gate beyond being an object.

    Deliberately no check that the nine documented fields are present. The guard already
    classifies a payload with no cwd and one whose identity cannot be a directory name, and it
    records what it found. A gate here would re-decide the host's contract from one host and one
    version, and on a host that delivers a different field set it would switch detection off
    with nothing written down.
    """
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


# ------------------------------------------------------------------ this hook's own settings


def configuration_path(codex_home=None, environ=None):
    """Where this hook's settings live. Its own file, never another hook's."""
    environ = os.environ if environ is None else environ
    override = environ.get(CONFIG_ENV)
    if override:
        return Path(override).expanduser()
    home = codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")
    return Path(home).expanduser() / CONFIG_NAME


def complaints(document):
    """What is wrong with a settings document, named field by field.

    Returned as a list rather than raised, so a malformed configuration stays its own answer
    instead of arriving at the caller as an unreadable file. The file was perfectly readable;
    what it says cannot be acted on, and those are different repairs.
    """
    found = []
    if not isinstance(document, dict):
        return ["the configuration is a " + type(document).__name__ + ", not an object"]
    if document.get("configVersion") != CONFIG_VERSION:
        # Checked rather than merely recorded. A version nothing reads is not a compatibility
        # boundary, and a later document shape would otherwise be acted on by this reader as
        # though it said what this one means.
        found.append("configVersion must be " + str(CONFIG_VERSION) + ", found "
                     + repr(document.get("configVersion")))
    for field in ("relayExecutable", "markerRoot"):
        value = document.get(field)
        if not isinstance(value, str) or not value.strip():
            found.append(field + " must be a non-empty string")
        elif not os.path.isabs(value):
            # This hook runs with the session's own workspace as its directory, so a relative
            # path resolves somewhere the install never named. A bare name is worse still: it
            # falls back to PATH, which is the resolution this adapter exists not to do.
            found.append(field + " must be an absolute path, because this hook runs in the"
                                 " session's workspace and a relative path resolves there")
    database = document.get("dbPath")
    if database is not None and (not isinstance(database, str) or not database.strip()):
        found.append("dbPath must be a non-empty string when it is present at all")
    elif isinstance(database, str) and database.strip() and not os.path.isabs(database):
        found.append("dbPath must be an absolute path")
    if document.get("mode") not in MODES:
        found.append("mode must be one of " + ", ".join(MODES))
    policy = document.get("journalPolicy")
    if policy is not None and policy not in JOURNAL_POLICIES:
        found.append("journalPolicy must be one of " + ", ".join(JOURNAL_POLICIES))
    budget = document.get("timeoutSeconds")
    if budget is not None and (isinstance(budget, bool) or not isinstance(budget, (int, float))
                               or budget <= 0):
        found.append("timeoutSeconds must be a positive number")
    root = document.get("journalRoot")
    if root is not None and (not isinstance(root, str) or not root.strip()):
        found.append("journalRoot must be a non-empty string when it is present at all")
    elif isinstance(root, str) and root.strip() and not os.path.isabs(root):
        found.append("journalRoot must be an absolute path")
    return found


def read_configuration(path):
    """Read the settings as a reading, so absent, unreadable and unreachable stay three answers.

    The shape is checked outside the reading region on purpose. Handing the shape check to
    read_json would report a readable file that says the wrong thing as an unreadable one, and
    an operator would go looking for a permission problem that is not there.
    """
    found = reading.read_json(path, "the completion hook configuration")
    if not found.usable:
        return None, CONFIG_OUTCOMES[found.state], found.detail, found
    if found.state == reading.ABSENT:
        return None, CONFIG_ABSENT, "no configuration at " + str(path), found
    wrong = complaints(found.value)
    if wrong:
        return None, CONFIG_MALFORMED, "; ".join(wrong), found
    return found.value, None, None, found


# ------------------------------------------------------------------ asking the guard


def guard_argv(config):
    """The call, built from the settings and from nothing else.

    --marker-root is always passed. The relay resolves it from a flag, then the environment,
    then XDG, then home, and the host process does not carry the coordinator's environment, so
    leaving it off would let this hook resolve a different root from the party that declared the
    intent and read every workspace as unmanaged.

    --db-path is passed only when it was configured, and for the opposite reason: the guard
    prefers the dbPath the coordinator recorded in its own intent, and passing a resolved
    default here would make that recorded value unreachable.

    --now is never passed. The time a decision is made is the guard's to observe.
    """
    argv = [str(config["relayExecutable"]), GUARD_COMMAND,
            "--marker-root", str(config["markerRoot"])]
    if config.get("dbPath"):
        argv += ["--db-path", str(config["dbPath"])]
    if config.get("mode") == HOLD:
        argv += ["--mode", HOLD]
    return argv


def invoke_guard(config, payload):
    """Run the guard and report how the process ended, without reading its output.

    Two questions, asked separately and answered separately. This one is only about the process.
    """
    argv = guard_argv(config)
    budget = config.get("timeoutSeconds") or DEFAULT_TIMEOUT_SECONDS
    started = time.monotonic()
    try:
        finished = subprocess.run(
            argv, input=payload if isinstance(payload, bytes) else str(payload).encode("utf-8"),
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=budget,
            start_new_session=True,
        )
    except subprocess.TimeoutExpired as expired:
        return {"ending": TIMED_OUT, "argv": argv, "code": None, "signal": None,
                "elapsedMs": round((time.monotonic() - started) * 1000),
                "stdout": _text(expired.stdout), "stderr": _text(expired.stderr),
                "detail": "the guard did not answer within " + str(budget) + "s and was killed"}
    except OSError as error:
        return {"ending": NOT_STARTED, "argv": argv, "code": None, "signal": None,
                "elapsedMs": round((time.monotonic() - started) * 1000),
                "stdout": "", "stderr": "",
                "errno": errno.errorcode.get(error.errno, error.errno),
                "detail": "the configured runtime could not be run: " + str(error)}
    code = finished.returncode
    return {
        "ending": SIGNALLED if code is not None and code < 0 else EXITED,
        "argv": argv,
        "code": None if code is None or code < 0 else code,
        "signal": None if code is None or code >= 0 else -code,
        "elapsedMs": round((time.monotonic() - started) * 1000),
        "stdout": _text(finished.stdout),
        "stderr": _text(finished.stderr),
        "detail": None,
    }


def _text(raw):
    if not raw:
        return ""
    if isinstance(raw, bytes):
        return raw.decode("utf-8", "replace")
    return str(raw)


def read_guard_stdout(text):
    """What the guard's stdout said, as its own question.

    A verdict and an error record are both JSON objects the relay prints deliberately; unreadable
    output and no output at all are two different silences. None of these is decided from the
    exit code, and the exit code is not decided from any of these.
    """
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

    The pairs are what carry the information. An exit of 2 carrying the relay's own error record
    is the relay refusing a request it understood; an exit of 2 carrying nothing is its argument
    parser rejecting the call before any command ran, which is what a runtime that does not offer
    this subcommand looks like. Reporting both as one failure would send an operator to the
    wrong repair, and this is the case that actually occurs: a host can have a relay on PATH
    whose build predates the guard entirely.
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
        # A verdict printed by a run the relay then failed. The verdict is not honoured,
        # because the process disagrees with it, and the disagreement is what gets reported.
        return GUARD_ENDED_UNEXPECTEDLY
    if verdict_complaints(value):
        return GUARD_VERDICT_INCOMPLETE
    return GUARD_ANSWERED


def verdict_complaints(verdict):
    """Whether a verdict agrees with itself, asked before any part of it is acted on.

    Both cells are read, because reading one to answer for the other is how an answer nobody
    gave gets delivered. A verdict whose own decision releases while its hook_output holds did
    not come from the guard, and rebuilding a block out of the nested half alone would let this
    adapter manufacture a hold from output it cannot account for.
    """
    if not isinstance(verdict, dict):
        return ["the guard's answer is not an object"]
    answer = verdict.get("hook_output")
    decision = verdict.get("decision")
    if not isinstance(answer, dict):
        return ["the verdict carries no hook_output object"]
    if not answer:
        return (["the verdict holds and carries nothing for the host to act on"]
                if decision == BLOCK else [])
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
    """The Stop JSON to print, rebuilt from validated fields rather than passed through.

    The guard already produces exactly this object. It is rebuilt anyway because this is the one
    place where something reaches the host, and a decision the host does not accept, or a block
    carrying no prompt, is reported by the host as a failed hook run that continues nothing.
    An empty answer is a release and prints nothing at all.
    """
    answer = (verdict or {}).get("hook_output")
    if verdict_complaints(verdict) or not answer:
        return None
    return json.dumps({"decision": BLOCK, "reason": answer["reason"], "continue": True})


# ------------------------------------------------------------------ the registered command


def command_for(interpreter, script):
    """The registered command, quoted so the host runs the two words this names.

    A hook file carries a command line, not an argv, so the two are joined with shell quoting.
    Concatenating them raw has two failure modes and they are not the same size: a path holding
    a space is delivered as more words than it is, and a path holding shell syntax is delivered
    as syntax and runs on every Stop with the user's own privileges. Ordinary paths come back
    from the quoting unchanged.
    """
    return shlex.join([str(interpreter), str(script)])


def registered_argv(command):
    """The words a registered command is made of, or None when it is not a command line.

    The inverse of the join above, and it has to be the inverse: a reader that splits on
    whitespace disagrees with the writer exactly where the quoting was needed.
    """
    try:
        return shlex.split(str(command or ""))
    except ValueError:
        return None


def names_this_adapter(command):
    """The word of a registered command that runs this adapter, or None.

    An identity decision, taken on a complete argument's own last component. A neighbouring
    program called not_completion_hook.py contains this name inside its own, and answering that
    a registration is this adapter's because the text contains the name would report a hook
    nobody installed, then check a target belonging to somebody else.
    """
    for word in registered_argv(command) or []:
        if Path(word).name == ENTRY_POINT_NAME:
            return word
    return None


# ------------------------------------------------------------------ this hook's own record


def journal(config, record):
    """Append one record of this invocation, under a name nothing else can take.

    Create-once with a random name rather than one built from the delivered identity: a session
    or turn id arrives as host input and would become a path component, and a record that cannot
    be written is worse than one that cannot be indexed.

    The limit worth stating: a journal write that fails cannot be recorded, because this is the
    place the recording would go and the hook may not speak on stderr. status() answers whether
    the journal is readable, which is where that failure becomes visible.
    """
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
            os.write(handle, (json.dumps(record, sort_keys=True, default=str) + "\n").encode("utf-8"))
        finally:
            os.close(handle)
    except (OSError, ValueError):
        return None
    return str(target)


def run(payload, codex_home=None, environ=None):
    """Decide one Stop and return the text to print, or None.

    Never raises and never holds on its own. Every path ends in a recorded outcome and a
    release, except the one where the guard itself decided to hold.
    """
    started = time.monotonic()
    record = {"recordVersion": 1, "event": EVENT, "at": now(),
              "adapterOutcome": None, "processEnding": None, "stdoutReading": None,
              "guardState": None, "guardDecision": None, "guardMode": None,
              "assignmentId": None, "guardRecordedAs": None, "held": False}
    config = {}
    try:
        stop, failed, detail = stop_input(payload)
        if failed is not None:
            return _release(config, record, failed, detail, started)
        record["sessionId"] = stop.get("session_id")
        record["turnId"] = stop.get("turn_id")
        record["stopHookActive"] = stop.get("stop_hook_active")
        path = configuration_path(codex_home, environ)
        record["configuration"] = str(path)
        config, failed, detail, _found = read_configuration(path)
        if failed is not None:
            return _release(config or {}, record, failed, detail, started)
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


def _release(config, record, outcome, detail, started):
    record["adapterOutcome"] = outcome
    record["detail"] = detail
    record["elapsedMs"] = round((time.monotonic() - started) * 1000)
    record["journalledAs"] = journal(config, record)
    return None


# ------------------------------------------------------------------ installing the settings

# Where the marker root defaults to when nothing names one. Duplicated from the relay because no
# relay command prints its resolved root, and stated here rather than guessed at a call site so
# status() can show the operator the value this install actually wrote.
MARKER_DIRECTORY_NAME = "codex-session-marker"
JOURNAL_DIRECTORY_NAME = "crw-completion-hook"


def default_marker_root(environ=None):
    environ = os.environ if environ is None else environ
    state = environ.get("XDG_STATE_HOME")
    base = Path(state).expanduser() if state else Path.home() / ".local" / "state"
    return _settled(base / MARKER_DIRECTORY_NAME)


def _settled(path):
    """An absolute path, with any link along it left alone.

    Absolute because this hook runs with the session's own workspace as its directory, so a
    relative path recorded at install time would resolve somewhere the install never named, and
    a bare name would be looked up on PATH. Not resolved, though: the runtime is named through a
    pointer on purpose, and following it here would record today's target and leave the next
    update moving a link nothing reads.
    """
    return Path(os.path.abspath(str(Path(path).expanduser())))


def relay_through_pointer(destination):
    """The relay named through the installer's own pointer, never through PATH.

    PATH answers about whatever is installed on this machine, which is a different question from
    the one a hook has to ask, and a checkout path answers about source that may never have been
    installed anywhere. The pointer is the indirection the installer already owns, so an update
    moves it and this configuration keeps naming the right runtime.
    """
    return _settled(Path(destination) / pointer.POINTER_NAME / "bin" / "codex-session-relay")


def configuration(*, destination=None, relay=None, marker_root=None, database=None,
                  mode=OBSERVE, timeout=DEFAULT_TIMEOUT_SECONDS, journal_root=None,
                  codex_home=None, environ=None, issue=None):
    """The settings document, built once so install and diagnosis cannot disagree about it."""
    environ = os.environ if environ is None else environ
    home = Path(codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")).expanduser()
    if relay:
        executable = _settled(relay)
    elif destination:
        executable = relay_through_pointer(destination)
    else:
        raise ValueError("a relay executable or an install destination is required: this"
                         " configuration never resolves the runtime from PATH")
    return {
        "configVersion": CONFIG_VERSION,
        "event": EVENT,
        "relayExecutable": str(executable),
        "markerRoot": str(_settled(marker_root) if marker_root
                          else default_marker_root(environ)),
        "dbPath": str(_settled(database)) if database else None,
        "mode": mode,
        "timeoutSeconds": timeout,
        "journalRoot": str(_settled(journal_root) if journal_root
                           else _settled(home / JOURNAL_DIRECTORY_NAME / "journal")),
        "journalPolicy": EVERY_INVOCATION,
        "installedBy": issue,
    }


def no_configuration():
    """The answer for settings that were not there at all: this write creates them.

    A named operation rather than a branch, because the class this belongs to is exactly the one
    an answer set keeps failing to express. A writer that can only say linked or differs has no
    value for "there was nothing there", and then a first install is indistinguishable from one
    that found somebody else's file.
    """
    return CONFIG_CREATED


def config_outcome(wanted, previous):
    """What a write would do to the settings that were found, including finding none.

    previous is the reading of the file as it stood. An unusable reading is never treated as an
    absent file: overwriting settings we could not read would be the one mistake with no undo.
    """
    if not previous.usable:
        return CONFIG_OUTCOMES[previous.state]
    if previous.state == reading.ABSENT:
        return no_configuration()
    if previous.value == wanted:
        return CONFIG_UNCHANGED
    return CONFIG_DIFFERS


def write_configuration(path, wanted, *, apply=False):
    """Write this hook's settings, and never over settings that say something else.

    Refusing to overwrite is not caution for its own sake. These settings carry the mode, and a
    silent rewrite from hold to observe, or the reverse, changes whether turns can be held at
    all without anyone being told.

    The decision is taken twice and only the second one is acted on. The first reading answers
    the caller; the second happens after the lock is held, and a file that moved in between is
    refused rather than written over, because a judgment made on a state that no longer exists
    is a judgment about nothing.
    """
    path = Path(path)
    unreadable = complaints(wanted)
    if unreadable:
        # Checked against the reader rather than against a list of flags, so a document this
        # command can generate but its own reader cannot act on is refused wherever it came
        # from, not only where today's caller happened to build it.
        return {"configuration": str(path), "outcome": CONFIG_WOULD_NOT_BE_READABLE,
                "applied": False, "wrote": False, "detail": "; ".join(unreadable),
                "complaints": unreadable}
    found = reading.read_json(path, "the completion hook configuration")
    outcome = config_outcome(wanted, found)
    answer = {"configuration": str(path), "outcome": outcome, "applied": False, "wrote": False}
    if not found.usable:
        answer["reading"] = found.refusal()
        return answer
    if outcome == CONFIG_UNCHANGED:
        answer["detail"] = "these settings are already installed"
        return answer
    if outcome == CONFIG_DIFFERS:
        answer["detail"] = ("settings are already installed and say something else; this command"
                            " does not overwrite them")
        answer["differingFields"] = sorted(
            field for field in set(wanted) | set(found.value or {})
            if (found.value or {}).get(field) != wanted.get(field)
        )
        return answer
    if not apply:
        answer["outcome"] = CONFIG_WOULD_CREATE
        answer["detail"] = "would write these settings; nothing was written"
        return answer
    with hostrecord.Locked(path):
        again = reading.read_json(path, "the completion hook configuration")
        settled = config_outcome(wanted, again)
        if settled != outcome:
            answer["outcome"] = CONFIG_CHANGED_UNDERNEATH
            answer["detail"] = ("the settings changed after they were read, so nothing was"
                                " written; rerun to decide against the file as it now stands")
            return answer
        hostrecord.atomic_write(path, json.dumps(wanted, indent=2, sort_keys=True) + "\n")
        back = reading.read_json(path, "the completion hook configuration")
    answer["applied"] = True
    answer["wrote"] = True
    answer["readBack"] = bool(back.usable and back.value == wanted)
    if not back.usable:
        answer["reading"] = back.refusal()
    return answer


# ------------------------------------------------------------------ registration and firing


def _cell(value, evidence, **extra):
    answer = {"value": value, "evidence": evidence}
    answer.update(extra)
    return answer


def _registration(codex_home, event, command_fragment):
    path = Path(codex_home) / "hooks.json"
    found = hooks.read(path)
    if not found.usable:
        return _cell(found.state, "the hook file could not be read", hookFile=str(path),
                     reading=found.refusal()), None
    entries = hooks.inventory(found.value, event)
    ours = []
    for entry in _commands(found.value, event):
        target = names_this_adapter(entry["command"])
        if target is not None:
            ours.append({**entry, "target": target})
    return _cell(str(len(entries)), "hooks registered for " + event + " in the user hook file",
                 hookFile=str(path), identities=[entry["identity"] for entry in entries],
                 thisAdapter=ours), ours


def _commands(document, event):
    found = []
    for matcher_index, group in enumerate((document.get("hooks") or {}).get(event) or []):
        for hook_index, entry in enumerate((group or {}).get("hooks") or []):
            found.append({"identity": hooks.identity(hooks.SOURCE, event, matcher_index,
                                                     hook_index),
                          "command": str((entry or {}).get("command") or ""),
                          "timeout": (entry or {}).get("timeout")})
    return found


def _offers_guard(executable, timeout):
    """Whether the configured runtime actually offers the subcommand this hook calls.

    Its own cell, separate from whether the file exists. A host can carry a relay built before
    the guard existed: the executable is there, it runs, and it rejects this call. Merging the
    two questions would report that hook as working.
    """
    try:
        finished = subprocess.run([str(executable), GUARD_COMMAND, "--help"],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                  timeout=timeout)
    except subprocess.TimeoutExpired:
        return _cell(NOT_READ, "the runtime did not answer --help within " + str(timeout) + "s")
    except OSError as error:
        return _cell(NOT_STARTED, "the runtime could not be run: " + str(error),
                     errno=errno.errorcode.get(error.errno, error.errno))
    if finished.returncode == GUARD_EXIT_OK:
        return _cell(GUARD_COMMAND, "the configured runtime offers " + GUARD_COMMAND)
    return _cell(GUARD_REJECTED_THE_CALL,
                 "the configured runtime does not offer " + GUARD_COMMAND,
                 exitCode=finished.returncode,
                 stderr=_text(finished.stderr)[:400])


def _budget_cell(config, ours):
    """The adapter's own wall clock against the timeout it is registered with.

    Reported rather than asserted. Installation refuses a budget the registered timeout does not
    exceed, but how much room is left over is a question about this host: process start, reading
    the marker and writing the record all cost time nobody here can measure at install time. So
    both numbers are shown with the margin between them, and every invocation journals its own
    elapsed milliseconds, which is the distribution an operator would actually tune against.
    """
    budget = (config or {}).get("timeoutSeconds")
    registered = sorted({entry.get("timeout") for entry in (ours or [])
                         if isinstance(entry.get("timeout"), (int, float))})
    if budget is None or not registered:
        return _cell(NOT_READ, "both numbers are needed and one of them was not read",
                     guardBudgetSeconds=budget, registeredTimeoutSeconds=registered or None)
    return _cell(str(min(registered) - budget),
                 "seconds between this adapter's own budget and the timeout the host registered"
                 " it with; measured cost per invocation is journalled as elapsedMs",
                 guardBudgetSeconds=budget, registeredTimeoutSeconds=registered)


def _journal_cell(config):
    """What this hook recorded about its own invocations."""
    root = (config or {}).get("journalRoot")
    policy = (config or {}).get("journalPolicy") or EVERY_INVOCATION
    if not root:
        return _cell(NO_JOURNAL, "no journal is configured, so this hook records nothing about"
                                 " its own invocations", journalPolicy=policy)
    directory = Path(root).expanduser()
    try:
        days = sorted(entry.name for entry in os.scandir(str(directory)) if entry.is_dir())
    except FileNotFoundError:
        return _cell(reading.ABSENT, "the journal directory does not exist, so this hook has"
                                     " recorded no invocation into it",
                     journalRoot=str(directory), journalPolicy=policy)
    except OSError as error:
        return _cell(reading.ACCESS_ERROR, "the journal could not be listed: " + str(error),
                     journalRoot=str(directory), journalPolicy=policy)
    days = [day for day in days if JOURNAL_DAY.match(day)]
    counted = 0
    for day in days:
        try:
            counted += sum(1 for entry in os.scandir(str(directory / day))
                           if entry.is_file() and JOURNAL_NAME.match(entry.name))
        except OSError:
            return _cell(reading.ACCESS_ERROR, "a journal day could not be listed",
                         journalRoot=str(directory), journalPolicy=policy, days=days)
    return _cell(str(counted), "invocations this hook recorded for itself",
                 journalRoot=str(directory), journalPolicy=policy, days=days)


def status(codex_home=None, environ=None, event=EVENT):
    """Registration and firing, answered as separate cells that are never merged.

    Installed, enabled and observed to have fired are three claims, and this reports them as
    more than three, because the middle one splits: a registered command whose target is gone
    and a present runtime that does not offer the subcommand both look like a working hook from
    the hook file alone. not_read is used where a question was not asked, and it is never
    written as an absence, because "nobody looked" and "there was nothing there" are the pair
    this whole design exists to keep apart.
    """
    environ = os.environ if environ is None else environ
    home = Path(codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")).expanduser()
    path = configuration_path(home, environ)
    config, failed, detail, found = read_configuration(path)
    registration, ours = _registration(home, event, ENTRY_POINT_NAME)

    target = _cell(NOT_READ, "no registration for this adapter was found to check")
    if ours:
        missing = [entry for entry in ours if not Path(entry["target"]).is_file()]
        target = _cell(
            reading.ABSENT if missing else reading.PRESENT,
            "the registered command names a script that is not there" if missing
            else "every registered command names a script that exists",
            commands=[entry["command"] for entry in ours])

    if failed is not None:
        settings = _cell(failed, detail or "", configuration=str(path))
        relay = _cell(NOT_READ, "no usable configuration names a runtime")
        offers = _cell(NOT_READ, "no usable configuration names a runtime")
        marker = _cell(NOT_READ, "no usable configuration names a marker root")
        # Asked separately from the cell below, because settings that could not be read and
        # settings that deliberately configure no journal are different answers. Reporting the
        # first as an empty journal would say this hook has recorded nothing, when what
        # happened is that nobody could tell where it would record.
        firing = _cell(NOT_READ, "no usable configuration names a journal to read")
    else:
        settings = _cell(found.state, "settings read", configuration=str(path),
                         mode=config.get("mode"), dbPath=config.get("dbPath"))
        executable = Path(config["relayExecutable"])
        relay = _cell(reading.PRESENT if executable.is_file() else reading.ABSENT,
                      "the configured runtime", relayExecutable=str(executable))
        offers = (_offers_guard(executable, config.get("timeoutSeconds")
                                or DEFAULT_TIMEOUT_SECONDS)
                  if executable.is_file()
                  else _cell(NOT_READ, "the configured runtime is not there to ask"))
        root = Path(config["markerRoot"])
        marker = _cell(reading.PRESENT if root.is_dir() else reading.ABSENT,
                       "the configured marker root", markerRoot=str(root))
        firing = _journal_cell(config)

    return {
        "command": "hook-status",
        "codexHome": str(home),
        "event": event,
        "registration": registration,
        "registeredCommandTarget": target,
        "hostTrust": _cell(NOT_READ, "whether the host loads and trusts these identities is"
                                     " recorded in its own state and is not read here"),
        "configuration": settings,
        "relayExecutable": relay,
        "guardEvaluateOffered": offers,
        "markerRoot": marker,
        "firingJournal": firing,
        "budget": _budget_cell(config, ours),
        "guardRecords": _cell(NOT_READ, "the guard's own per-observation records live in the"
                                        " marker and belong to the relay, not to this command"),
        "daemon": _cell(NOT_READ, "the Stop path reads the marker and a read-only database and"
                                  " never reaches the daemon, so this command does not infer"
                                  " one from a hook result"),
        "note": ("Registered, offered and observed to have fired are separate claims. A"
                 " registration says a line is in the hook file; it does not say the host ran"
                 " it, that the runtime it names can answer, or that any turn was judged."),
    }
