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
import shutil
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

# Who owns the registration these settings belong to. The settings say which runtime answers a
# Stop; this says which registration is allowed to exist at all. Two owners of one event is not
# a preference: both copies run, both ask the guard, and the turn's one hold goes to whichever
# wins the reservation.
#
# "user" is this repository's own $CODEX_HOME/hooks.json registration and stays the default.
# It is deliberately NOT written into the document: a host that installed before this key
# existed holds a document without it, and adding the key to what this command generates would
# make an ordinary reinstall differ from the file on disk and be refused. Absent reads as user.
#
# "plugin" is a registration the CRW plugin package declares. The plugin cannot write this file,
# so the record is written here, by the command that would otherwise have registered the hook.
OWNER_USER = "user"
OWNER_PLUGIN = "plugin"
OWNERS = (OWNER_USER, OWNER_PLUGIN)

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

# The ceiling the packaged launcher puts on its own subprocess deadline. It sits between the
# host and the adapter, so its deadline has to be longer than the adapter's guard budget:
# a launcher that expires first kills the adapter mid-call and discards the very record that
# would have explained the timeout, and then releases the turn saying nothing. Mirrored in
# plugins/crw/wiring/crw_stop_hook.py, which cannot import this module.
LAUNCHER_CEILING_SECONDS = 9

# The largest budget any of these checks will entertain. Compared against rather than converted,
# because an arbitrary-precision integer cannot always become a float: math.isfinite raises
# OverflowError on one, and a guard against a bad value must never itself be the failure.
MAX_TIMEOUT_SECONDS = 86400


def usable_seconds(value):
    """Whether this can be a number of seconds at all.

    Written without converting, so an integer is judged as an integer and a float as a float.
    NaN fails every comparison it appears in, which is what excludes it here.
    """
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return 0 < value <= MAX_TIMEOUT_SECONDS

# The lowest Python this adapter is supported on, and the one the repository's checks run.
SUPPORTED_PYTHON = (3, 10)


def budget_complaints(guard_timeout, registered_timeout):
    """Whether the adapter's own budget can do what it is for, against the host's.

    The two numbers only mean something together. A budget the host's timeout does not exceed
    lets the host kill the adapter mid-call, and the evidence that would have explained the
    timeout is the record the killed process was about to write. Stated as its own check because
    the settings hold one number and the hook registration holds the other, so neither reader
    can see the relationship on its own.
    """
    found = []
    if not usable_seconds(guard_timeout):
        found.append("the guard budget must be a positive number of seconds, at most "
                     + str(MAX_TIMEOUT_SECONDS))
    elif (isinstance(registered_timeout, (int, float))
            and not isinstance(registered_timeout, bool)
            and guard_timeout >= registered_timeout):
        found.append("the guard budget must be under the registered hook timeout of "
                     + str(registered_timeout) + "s, or the host can kill this adapter before"
                     " it records why it did not answer")
    if (isinstance(registered_timeout, (int, float))
            and not isinstance(registered_timeout, bool)
            and registered_timeout > REGISTERED_TIMEOUT_SECONDS):
        # The host clamps an over-long timeout at discovery, so a large number here is not the
        # deadline it looks like: the effective one can land under the guard budget and kill
        # this adapter before it records anything. What the clamp is was not measured, so this
        # declines to exceed the only registered timeout this repository has evidence for
        # rather than guessing at a larger one that happens to survive.
        found.append("the registered timeout must not exceed " + str(REGISTERED_TIMEOUT_SECONDS)
                     + "s: the host clamps an over-long timeout at discovery, and the clamped"
                     " value is not measured here, so a larger number is not the deadline it"
                     " appears to be")
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
RELEASE = "release"
DECISIONS = (BLOCK, RELEASE)

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
# The write landed and could not be confirmed. Reported as applied, because saying nothing was
# written would invite a retry over a file that now exists, and reported as unsettled, because
# the settings a hook is about to be registered against have not been read back.
CONFIG_APPLIED_UNVERIFIED = "config_applied_unverified"
# The writer refusing to write a document its own reader would reject. Without it an install
# reports success and every Stop afterwards reads the settings it just wrote as malformed,
# which is a hook that is registered, inert, and says so nowhere anybody looks.
CONFIG_WOULD_NOT_BE_READABLE = "config_would_not_be_readable"
CONFIG_WRITE_OUTCOMES = (CONFIG_CREATED, CONFIG_UNCHANGED, CONFIG_WOULD_CREATE, CONFIG_DIFFERS,
                         CONFIG_CHANGED_UNDERNEATH, CONFIG_APPLIED_UNVERIFIED,
                         CONFIG_WOULD_NOT_BE_READABLE)

# The outcomes that mean these settings were READ BACK saying what this install asked them to,
# or would with --apply. Membership follows the read-back rather than the write's intention,
# because a hook is about to be registered against whatever is in that file now, not against
# what this command meant to put there. Everything else, including every unusable reading, is a
# refusal, and the set is named here so an exit status is derived from it rather than from a
# list kept equal by hand.
CONFIG_SETTLED = (CONFIG_CREATED, CONFIG_UNCHANGED, CONFIG_WOULD_CREATE)

# What status() answers with when it did not ask. Distinct from an absence, which is an answer.
NOT_READ = "not_read"

# A registration this command can see and cannot act on: it names its settings with a relative
# path, which the hook resolves against each session's workspace. There is no single file to
# inspect, and inspecting the one this process would resolve would report on a file the hook
# never opens.
REGISTRATION_RELATIVE = "registration_names_a_relative_settings_path"

# More than one registration naming more than one settings file. Every one of them runs, so
# reporting the first would describe one hook and leave the others unmentioned.
REGISTRATION_AMBIGUOUS = "registrations_name_different_settings"

# A flag only the real subcommand's help carries. Checked alongside the subcommand's own name
# because a program that echoes its arguments prints that name back without offering anything.
GUARD_HELP_MARKER = "--marker-root"

# A registration whose adapter target is relative. The hook resolves it against each session's
# workspace, so no single file answers for it and the one this process would resolve is not it.
REGISTRATION_RELATIVE_TARGET = "registration_names_a_relative_adapter"

# A spelling this command cannot judge from here: relative, with a separator, so it names one
# program from the hook's workspace and another from wherever a diagnosis happens to run.
WORKSPACE_DEPENDENT = "workspace_dependent_spelling"


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


def configuration_path(codex_home=None, environ=None, settings=None):
    """Where this hook's settings live. Its own file, never another hook's.

    Precedence: a path this caller was handed, then the environment override, then the Codex
    home. The first exists because the registered command carries the path the install resolved,
    and a value the install already decided must not be decided again somewhere else, in another
    directory, under another CODEX_HOME.
    """
    if settings:
        return _settled(settings)
    environ = os.environ if environ is None else environ
    override = environ.get(CONFIG_ENV)
    if override:
        # Settled for the same reason every other path here is: the installer and the hook run
        # from different directories, so a relative override would name one file at install
        # time and a different one, or none, at every Stop.
        return _settled(override)
    home = codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")
    return _settled(Path(home) / CONFIG_NAME)


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
        # Checked rather than ignored. An owner this reader does not know is not a document it
        # can act on: the value decides which registration may exist, so reading past it would
        # let a second copy be installed beside one this command cannot see.
        found.append("owner must be one of " + ", ".join(OWNERS)
                     + " when it is present at all, found " + repr(owner))
    for field in ("adapterInterpreter", "adapterEntryPoint"):
        value = document.get(field)
        if value is None:
            continue
        if not isinstance(value, str) or not value.strip():
            found.append(field + " must be a non-empty string when it is present at all")
        elif not os.path.isabs(value):
            # Same reason relayExecutable is absolute: whatever starts this adapter runs from
            # the session's own workspace, and a relative path names a file the install never
            # resolved. A bare name is worse, because it falls back to PATH.
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
            # The packaged launcher sits between the host and the adapter and caps its own
            # deadline here, so a guard budget at or above that ceiling lets the launcher kill
            # the adapter first and release the turn without the record that explains it.
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


def owner_of(document):
    """Who owns the registration these settings belong to.

    Absent is not unknown. A document written before this key existed was written by the
    command that also appends to the hook file, so absence means exactly one thing and is
    answered as that one thing rather than as a third state nobody can act on.
    """
    owner = (document or {}).get("owner")
    return owner if owner in OWNERS else OWNER_USER


def ownership_complaints(path, owner, *, registered):
    """Why this owner may not take the registration, given what is already on this host.

    Two owners of one Stop is the failure this exists to prevent, and it is prevented in both
    directions because either one can be installed first. The user path is refused by settings
    that already name the plugin; the plugin path is refused by a registration already sitting
    in the hook file. Neither reads the other's artifact as a hint: settings are read as
    settings and the hook file as the hook file, and a reading that did not happen refuses
    rather than defaults.

    registered is the list adapter_entries() returned for the event, or None when the hook file
    could not be read. None refuses: whether this adapter is already registered was not
    established, and installing on an unanswered question is how the second copy arrives.
    """
    if owner not in OWNERS:
        return ["owner must be one of " + ", ".join(OWNERS) + ", found " + repr(owner)]
    document, outcome, detail, _ = read_configuration(path)
    if owner == OWNER_USER:
        if document is not None and owner_of(document) == OWNER_PLUGIN:
            return ["the settings at " + str(path) + " record the " + OWNER_PLUGIN
                    + " as the owner of this " + EVENT + " registration, so the plugin package"
                      " already declares it; appending a hook file registration would run two"
                      " copies on every " + EVENT + ". Install with --owner " + OWNER_PLUGIN
                    + ", or remove the plugin declaration and these settings first"]
        return []
    if outcome == CONFIG_MALFORMED:
        # A readable file saying something this reader cannot act on is not an absent one. The
        # write below would refuse it anyway; refusing here says why in the operator's terms.
        return ["the settings at " + str(path) + " could not be acted on (" + str(detail)
                + "), so who owns this " + EVENT + " registration was not established"]
    if registered is None:
        return ["the hook file could not be read, so whether this adapter is already registered"
                " for " + EVENT + " was not established; nothing was written"]
    if registered:
        names = ", ".join(entry["identity"] for entry in registered)
        return ["this adapter is already registered for " + EVENT + " in the hook file as "
                + names + ", which is the " + OWNER_USER + "-owned registration; a plugin"
                  " declaration beside it would run two copies on every " + EVENT
                + ". Remove that registration first, by hand, because removal renumbers later"
                  " identities and this command does not perform one"]
    return []


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
    # Saved now, while the leader is certainly alive and certainly leads it: start_new_session
    # made its pid the group's id. Looking the group up at kill time asks a process that may
    # already be gone, and a recycled pid would name somebody else's group.
    group = opened.pid
    sent = payload if isinstance(payload, bytes) else str(payload).encode("utf-8")
    try:
        out, err = opened.communicate(input=sent, timeout=budget)
    except subprocess.TimeoutExpired:
        # The whole group, not just the process this started. A relay that forks keeps the
        # inherited pipes open, and waiting on those is what would carry this past its own
        # budget and let the host kill the adapter before it records why it did not answer.
        _end_group(opened, group)
        # Draining is bounded by what is LEFT of the budget, never by the budget again. A
        # descendant that escaped the group by calling setsid still holds these pipes, and a
        # second full wait would take the whole thing to nearly twice the budget - which is the
        # window in which the host kills this process and the timeout goes unrecorded. The
        # record is worth more here than the output it could not collect.
        remaining = budget - (time.monotonic() - started)
        out, err = b"", b""
        if remaining > 0:
            try:
                out, err = opened.communicate(timeout=remaining)
            except subprocess.TimeoutExpired:
                out, err = b"", b""
        # Reaped without waiting. The status is collected when it is already there, and when it
        # is not this returns anyway: the entry point exits moments later and the child becomes
        # init's to reap, which is a bounded cost, while blocking here is not.
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
        if decision == BLOCK:
            return ["the verdict holds and carries nothing for the host to act on"]
        if decision not in DECISIONS:
            # An unknown decision is not a release. Reading it as one would record an
            # incompatible runtime as having answered, which is the single reading that hides
            # the incompatibility instead of reporting it.
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


def command_for(interpreter, script, settings=None):
    """The registered command, quoted so the host runs the two words this names.

    A hook file carries a command line, not an argv, so the two are joined with shell quoting.
    Concatenating them raw has two failure modes and they are not the same size: a path holding
    a space is delivered as more words than it is, and a path holding shell syntax is delivered
    as syntax and runs on every Stop with the user's own privileges. Ordinary paths come back
    from the quoting unchanged.

    The settings path is carried here rather than left to be resolved again at every Stop.
    Resolving it twice means resolving it in two different directories and against two different
    values of CODEX_HOME: the install decided which file it wrote, so the install is what should
    say which file to read.
    """
    words = [str(interpreter), str(script)]
    if settings:
        words.append(str(settings))
    return shlex.join(words)


def interpreter_for(python, *, run=True):
    """The interpreter this command will register, settled here rather than at every Stop.

    A bare name is looked up now, on the machine doing the install, because that is the only
    moment a lookup means anything: the hook runs later, from each session's own workspace, and
    a name resolved then could find a different interpreter or nothing at all. The same reason
    the runtime is named through the pointer instead of through PATH.

    run=False resolves without executing the candidate, for a caller that is going to write
    nothing. A plan should not run a program somebody named on the command line, and what it
    did not check it does not claim.
    """
    if not python:
        raise ValueError("an interpreter is required")
    found = shutil.which(str(python))
    if found:
        settled = _settled(found)
        if run:
            _require_python(settled)
        return settled
    settled = _settled(python)
    probe = presence(settled, "an interpreter")
    if probe["value"] == reading.PRESENT and os.access(str(settled), os.X_OK):
        if run:
            _require_python(settled)
        return settled
    if probe["value"] == reading.PRESENT:
        raise ValueError(str(settled) + " is not executable; every Stop would fail before the"
                                        " adapter starts, evaluating and journalling nothing")
    raise ValueError("no interpreter was found at " + str(python)
                     + " (" + probe["evidence"] + ")"
                     + "; the hook runs from each session's workspace, so this has to name one"
                       " that can be found from anywhere")


def _require_python(candidate):
    """Ask the candidate to be a Python before registering it as one.

    Executable is not the question. /bin/true is executable, exits 0, and would be registered
    happily; every Stop would then succeed at running it and never reach the adapter, so there
    would be no guard decision and no journal entry, and the install would have reported success.

    Nor is being a Python the whole question. A Python too old to run this adapter fails the same
    way and looks the same from the hook file, so the version is read rather than the bare fact
    that something answered.
    """
    try:
        finished = subprocess.run(
            [str(candidate), "-c",
             "import sys; print('%d.%d' % (sys.version_info[0], sys.version_info[1]))"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
    except (OSError, subprocess.SubprocessError) as error:
        raise ValueError(str(candidate) + " could not be run as an interpreter: "
                         + str(error)) from error
    said = (finished.stdout or b"").decode("utf-8", "replace").strip()
    parts = said.split(".")
    if finished.returncode != 0 or len(parts) != 2 or not all(p.isdigit() for p in parts):
        raise ValueError(str(candidate) + " is executable but does not run Python; every Stop"
                                          " would succeed at running it and never reach the"
                                          " adapter")
    if (int(parts[0]), int(parts[1])) < SUPPORTED_PYTHON:
        raise ValueError(str(candidate) + " runs Python " + said + ", below the supported "
                         + ".".join(str(part) for part in SUPPORTED_PYTHON)
                         + "; the adapter would fail on every Stop before evaluating or"
                           " journalling anything")


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


def adapter_entries(document, event):
    """Every registration in this hook file that runs this adapter, with its parsed target."""
    found = []
    for matcher_index, group in enumerate((document.get("hooks") or {}).get(event) or []):
        for hook_index, entry in enumerate((group or {}).get("hooks") or []):
            command = str((entry or {}).get("command") or "")
            target = names_this_adapter(command)
            if target is not None:
                words = registered_argv(command) or []
                after = words.index(target) + 1
                found.append({
                    "identity": hooks.identity(hooks.SOURCE, event, matcher_index, hook_index),
                    "command": command, "timeout": (entry or {}).get("timeout"),
                    "target": target,
                    # Part of the registration, not decoration: the same command under a
                    # different matcher fires on different turns, and hooks.install treats only
                    # the unconditional group as already installed, so an identical command
                    # under a matcher would be appended again and both would run.
                    "matcher": (group or {}).get("matcher"),
                    # The settings this registration actually reads, taken from the command
                    # rather than recomputed. A reader that resolves its own path answers about
                    # a file the hook may never open.
                    "settings": words[after] if after < len(words) else None})
    return found


def duplicate_complaints(document, event, command, timeout):
    """Whether appending would leave two of this adapter registered on one event.

    Installation appends and never removes, because removing renumbers every later hook and
    detaches the trusted hash Codex recorded against it. So a second registration that differs
    only in its timeout is not a correction, it is a second copy: both run on every Stop, both
    ask the guard, and the turn's one hold goes to whichever wins the reservation. Refused here
    rather than appended, and the existing identity is named so the operator can edit it.
    """
    already = adapter_entries(document, event)
    if not already:
        return []
    names = ", ".join(entry["identity"] for entry in already)
    if len(already) > 1:
        # Two already there is the state this check exists to reject, and an identical one among
        # them does not make it acceptable: every copy asks the guard and journals every Stop.
        return ["this adapter is registered more than once for " + event + " as " + names
                + "; every copy asks the guard on every " + event + ", and removal renumbers"
                  " later identities so this command does not perform one. Reduce it to one"
                  " registration first"]
    entry = already[0]
    if (entry["command"] == command and entry["timeout"] == timeout
            and entry["matcher"] == hooks.INSTALLED_MATCHER):
        return []
    if entry["matcher"] != hooks.INSTALLED_MATCHER:
        return ["this adapter is already registered for " + event + " as " + names
                + " under a matcher, and installation only ever appends an unconditional group:"
                  " appending would add a second registration beside it and both would run on a"
                  " matching " + event + ". Edit or remove that registration first"]
    return ["this adapter is already registered for " + event + " as " + names
            + " with different settings; appending would run two copies on every " + event
            + ", and removal renumbers later identities so this command does not perform one."
            " Edit or remove that registration first"]


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
            # Written to completion, and removed if it cannot be. os.write may write fewer bytes
            # than it was given, and a truncated record is worse than none: it survives under a
            # name nothing will reuse and is counted as an invocation whose contents no longer
            # read back.
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


def run(payload, codex_home=None, environ=None, settings=None):
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
        # The settings are read FIRST, before the payload is looked at. They are what says where
        # a record goes, so reading them second meant a payload this hook could not parse was
        # released with nothing written down anywhere - the one class of invocation that most
        # needs a record, silently absent from the firing evidence.
        path = configuration_path(codex_home, environ, settings)
        record["configuration"] = str(path)
        config, failed, detail, _found = read_configuration(path)
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
# The relay's own override, spelled the same. Read at install time so the recorded root is the
# one the coordinator is actually publishing under.
MARKER_ENV = "CODEX_SESSION_RELAY_MARKER_ROOT"
JOURNAL_DIRECTORY_NAME = "crw-completion-hook"


def default_marker_root(environ=None):
    """The root the relay would resolve, by the relay's own precedence minus the flag.

    The environment override is read here because the relay reads it, and a default that skips
    it is not a default but a disagreement: a coordinator that sets the variable publishes its
    intents under one tree while this hook would look under another, and every managed turn
    would read as unmanaged with nothing recorded. It is settled now, at install time, rather
    than left to be re-resolved from whatever environment the host hands a hook.
    """
    environ = os.environ if environ is None else environ
    override = environ.get(MARKER_ENV)
    if override:
        return _settled(override)
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
                  codex_home=None, environ=None, issue=None, isolation=None,
                  owner=OWNER_USER, adapter_interpreter=None, adapter_entry_point=None):
    """The settings document, built once so install and diagnosis cannot disagree about it."""
    environ = os.environ if environ is None else environ
    home = Path(codex_home or environ.get("CODEX_HOME") or (Path.home() / ".codex")).expanduser()
    if owner not in OWNERS:
        raise ValueError("owner must be one of " + ", ".join(OWNERS) + ", not " + repr(owner))
    if relay:
        executable = _settled(relay)
    elif destination:
        executable = relay_through_pointer(destination)
    else:
        raise ValueError("a relay executable or an install destination is required: this"
                         " configuration never resolves the runtime from PATH")
    document = {
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
        # Who asserted that a held child cannot forge the facts the decision reads. The contract
        # makes that grant a prerequisite for holding and not for observing, so it is recorded
        # where a later reader can see whose assertion it was, rather than being inferred from
        # the mode having been set.
        "isolationAssertedBy": isolation,
    }
    if owner != OWNER_USER:
        # Written only when it is not the default, so a host that installed before this key
        # existed keeps a byte-identical document and an ordinary reinstall is still UNCHANGED
        # rather than refused as settings that say something else.
        document["owner"] = owner
        # The user-owned registration carries these two in the command line it writes into the
        # hook file. A plugin-declared registration has no such moment: its command ships inside
        # a package that knows nothing about this host, so the two values the launcher needs are
        # resolved here, once, by the command that resolved them for the other owner too.
        document["adapterInterpreter"] = str(_settled(adapter_interpreter)) \
            if adapter_interpreter else None
        document["adapterEntryPoint"] = str(_settled(adapter_entry_point)) \
            if adapter_entry_point else None
    return document


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
        if isinstance(found.value, dict):
            answer["differingFields"] = sorted(
                field for field in set(wanted) | set(found.value)
                if found.value.get(field) != wanted.get(field))
        else:
            # A file holding valid JSON that is not an object has no fields to compare, and
            # asking it for some is a traceback where a modelled refusal was promised. The
            # refusal stands; what it cannot carry is a field list.
            answer["differingFields"] = None
            answer["detail"] = ("settings are already installed and hold a "
                                + type(found.value).__name__ + " rather than an object; this"
                                  " command does not overwrite them")
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
    if not answer["readBack"]:
        # The write landed; what is in the file now was not confirmed to be it. Left settled,
        # this is the same hole the write-before-register order exists to close, one step later:
        # a hook registered against settings nobody read back.
        answer["outcome"] = CONFIG_APPLIED_UNVERIFIED
        answer["detail"] = ("the settings were written and could not be read back as written;"
                            " no hook should be registered against them until they can be")
    return answer


# ------------------------------------------------------------------ registration and firing


def _cell(value, evidence, **extra):
    answer = {"value": value, "evidence": evidence}
    answer.update(extra)
    return answer


def presence(path, what, *, directory=False):
    """Whether something is at this path, keeping "could not look" apart from "not there".

    Path.is_file answers false for both, which turns a permission problem or an unreachable
    mount into a clean report that the runtime is simply missing, and sends the repair to the
    wrong place. The four states come from the module that owns them.
    """
    try:
        found = os.lstat(str(path))
    except FileNotFoundError:
        return _cell(reading.ABSENT, "nothing exists at " + str(path), path=str(path))
    except (OSError, ValueError) as error:
        return _cell(reading.ACCESS_ERROR,
                     "whether anything exists at this path could not be established: "
                     + type(error).__name__ + ": " + str(error), path=str(path))
    import stat as stat_module
    if stat_module.S_ISLNK(found.st_mode):
        # A link IS something at this path. Reporting a dangling one as absent loses the only
        # fact that would repair it, and says the component was never installed when what
        # actually happened is that its target went away.
        try:
            found = os.stat(str(path))
        except FileNotFoundError:
            return _cell(reading.UNREADABLE,
                         "a symbolic link whose target does not exist", path=str(path))
        except OSError as error:
            if error.errno == errno.ELOOP:
                return _cell(reading.UNREADABLE, "a symbolic link that loops", path=str(path))
            return _cell(reading.ACCESS_ERROR,
                         "a symbolic link whose target could not be resolved: " + str(error),
                         path=str(path))
    right = stat_module.S_ISDIR(found.st_mode) if directory else stat_module.S_ISREG(found.st_mode)
    if not right:
        return _cell(reading.UNREADABLE,
                     "something is at this path and it is not the " + what + " expected here",
                     path=str(path))
    return _cell(reading.PRESENT, what, path=str(path))


def _registration(codex_home, event):
    path = Path(codex_home) / "hooks.json"
    found = hooks.read(path)
    if not found.usable:
        return _cell(found.state, "the hook file could not be read", hookFile=str(path),
                     reading=found.refusal()), None
    entries = hooks.inventory(found.value, event)
    # The same reader the installer's duplicate check uses. Two loops over the same hook file
    # asking the same question is how the settings word came to exist in one answer and not in
    # the other, so there is one.
    ours = adapter_entries(found.value, event)
    return _cell(str(len(entries)), "hooks registered for " + event + " in the user hook file",
                 hookFile=str(path), identities=[entry["identity"] for entry in entries],
                 thisAdapter=ours), ours


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
        said = _text(finished.stdout) + _text(finished.stderr)
        if GUARD_COMMAND not in said or GUARD_HELP_MARKER not in said:
            # Exit 0 alone does not answer this question. A program that ignores its arguments
            # and succeeds - /bin/true is the whole family - would otherwise be reported as
            # offering a subcommand it has never heard of, and every Stop would then fail
            # somewhere this cell said it would not. Nor is the subcommand's own name enough:
            # a program that echoes its arguments prints that name back while offering nothing,
            # so a flag the real help carries is required beside it.
            return _cell(GUARD_REJECTED_THE_CALL,
                         "the configured runtime exited 0 without describing " + GUARD_COMMAND
                         + ", so it was not asked anything it recognised",
                         exitCode=finished.returncode, said=said[:200])
        return _cell(GUARD_COMMAND, "the configured runtime describes " + GUARD_COMMAND
                                    + " when asked for its help")
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


def _interpreter_cell(ours):
    """The program that has to run the adapter, probed as its own question.

    The first word of a registered command is the thing the host executes, and it is not always
    a path: a bare name is resolved on PATH, which is what the host does too, so reporting it
    absent because no file sits at that spelling would fail a working hook in diagnosis.

    What this does not follow is a wrapper. A command whose first word runs something else - an
    env, a shell, a launcher script - is reported on the word that was checked, and the evidence
    says so, because following it would mean guessing at an argument convention this command did
    not write.
    """
    checked = []
    for entry in ours:
        first = (registered_argv(entry["command"]) or [None])[0]
        if not first:
            continue
        if os.sep in first or (os.altsep and os.altsep in first):
            if not os.path.isabs(first):
                # which() resolves a spelling carrying a separator against the CALLER's working
                # directory, so probing it here answers about a program under whatever checkout
                # this diagnosis was run from. The host starts the hook in each session's
                # workspace, where that spelling names something else or nothing.
                probe = _cell(WORKSPACE_DEPENDENT,
                              "the registration names its interpreter with the relative path "
                              + first + ", which resolves differently in every workspace; it"
                              " was not probed here", path=first)
                checked.append({"word": first, "resolved": first, "probe": probe})
                continue
            resolved = first
        else:
            # A bare name is looked up on PATH, which is what the host does with it too.
            resolved = shutil.which(first) or first
        probe = presence(resolved, "the registered interpreter")
        if probe["value"] == reading.PRESENT and not os.access(str(resolved), os.X_OK):
            probe = _cell(reading.UNREADABLE, "the registered interpreter is not executable",
                          path=str(resolved))
        checked.append({"word": first, "resolved": str(resolved), "probe": probe})
    if not checked:
        return _cell(NOT_READ, "no registration named a program to run the adapter")
    unusable = next((one for one in checked if one["probe"]["value"] != reading.PRESENT), None)
    chosen = unusable or checked[0]
    return _cell(chosen["probe"]["value"],
                 chosen["probe"]["evidence"]
                 + "; this is the first word of the registered command, and a wrapper's own"
                   " target is not followed",
                 interpreters=[one["resolved"] for one in checked])


def _recorded_program_cell(named, label):
    """A program these settings name, probed the way a registered one is.

    A plugin-owned hook has no entry in the hook file, so the cell above receives nothing and
    answers that no registration named a program. That is true about the hook file and useless
    about this host: the launcher starts the two programs recorded here, and if either is gone
    every Stop is released without a word. Same probe, different source.
    """
    if not named:
        return None
    if not os.path.isabs(str(named)):
        return _cell(WORKSPACE_DEPENDENT, "these settings name " + label + " with the relative"
                     " path " + str(named) + ", which resolves differently in every workspace;"
                     " it was not probed here", path=str(named))
    probe = presence(named, label)
    if probe["value"] == reading.PRESENT and not os.access(str(named), os.X_OK):
        return _cell(reading.UNREADABLE, label + " is not executable", path=str(named))
    return probe


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
    registration, ours = _registration(home, event)
    # The file the registered hook actually reads, taken from the registration when there is
    # one. Recomputing it here would answer about a file the hook may never open: an install
    # that used an override embedded the resolved path in its command, and this command has no
    # reason to be running under the same environment.
    carried = [entry["settings"] for entry in (ours or []) if entry.get("settings")]
    # Judged with the expansion the hook itself applies, because a ~ path is absolute once the
    # hook opens it. Calling it relative here would hide a working configuration and every cell
    # downstream of it.
    relative = [named for named in carried
                if not os.path.isabs(os.path.expanduser(named))]
    # Each relative spelling counts as its own unresolved source. Excluding them made two
    # registrations naming different relative files, or one relative beside one absolute, look
    # like a single source, and the reader then described one hook while suppressing another
    # that may carry a different mode or relay.
    distinct = sorted({str(_settled(named)) for named in carried if named not in relative}
                      | set(relative))
    # A registration with no settings argument resolves its own path, which is not necessarily
    # the one its neighbour names. Counted as a separate answer for that reason: "one path and
    # one silence" is two different files just as surely as two paths are.
    silent = len(ours or []) - len(carried)
    if len(distinct) + (1 if silent else 0) > 1:
        # Every registration runs, so naming one of them would describe one hook while
        # reporting the others' state as if it were that one's.
        path, source, config = Path(distinct[0]), "the registered commands", None
        failed, detail, found = REGISTRATION_AMBIGUOUS, (
            "registrations name different settings files (" + ", ".join(distinct)
            + ") and every one of them runs, so none of them answers for the others"), None
    elif relative:
        # Not settled here. A relative path in a registration is resolved by the hook against
        # each session's workspace, so there is no one file to inspect, and inspecting the one
        # THIS process would resolve would report an unrelated file as the hook's own.
        path, source, config = Path(relative[0]), "the registered command", None
        failed, detail, found = REGISTRATION_RELATIVE, (
            "the registration names its settings with the relative path " + relative[0]
            + ", which the hook resolves against each session's workspace; no single file"
              " answers for it and none was read"), None
    else:
        path = _settled(carried[0]) if carried else configuration_path(home, environ)
        source = ("the registered command" if carried
                  else "this command's own resolution; no registration named one")
        config, failed, detail, found = read_configuration(path)

    target = _cell(NOT_READ, "no registration for this adapter was found to check")
    interpreter = _cell(NOT_READ, "no registration for this adapter was found to check")
    if ours:
        # No expansion here, unlike the settings path. The settings path is expanded by this
        # adapter before it opens it; the adapter's own path is handed to the interpreter
        # literally, and nothing expands a tilde on the way. So a ~ target is relative in
        # effect, and judging it with expanduser would report a file the host never runs.
        loose = [entry["target"] for entry in ours if not os.path.isabs(entry["target"])]
        firm = [entry for entry in ours if os.path.isabs(entry["target"])]
        probes = [presence(entry["target"], "the adapter script") for entry in firm]
        worst = next((probe for probe in probes if probe["value"] != reading.PRESENT), None)
        if worst is not None:
            # An absolute target that is missing is reported even when another registration
            # names a relative one: skipping every probe because one entry is unjudgeable
            # hides the broken copies beside it.
            target = _cell(worst["value"], worst["evidence"],
                           commands=[entry["command"] for entry in ours], probes=probes,
                           relativeTargets=loose or None)
        elif loose:
            # Not resolved here, for the same reason a relative settings path is not: the hook
            # resolves it against each session's workspace, so the file this process would find
            # is not the one the host runs, and reporting on it answers about the wrong program.
            target = _cell(REGISTRATION_RELATIVE_TARGET,
                           "the registration names the adapter with the relative path "
                           + loose[0] + ", which the hook resolves against each session's"
                           " workspace; no single file answers for it and none was read",
                           commands=[entry["command"] for entry in ours])
        else:
            target = _cell(probes[0]["value"], probes[0]["evidence"],
                           commands=[entry["command"] for entry in ours],
                           probes=probes)
        # Its own cell, because the script being there says nothing about the program that has
        # to run it. A virtual environment that moved after installation leaves the script in
        # place and the interpreter gone, and then the host cannot start the adapter at all: no
        # decision, no journal entry, and a registration that still looks correct.
        interpreter = _interpreter_cell(ours)

    if failed is not None:
        settings = _cell(failed, detail or "", configuration=str(path),
                         configurationSource=source)
        relay = _cell(NOT_READ, "no usable configuration names a runtime")
        offers = _cell(NOT_READ, "no usable configuration names a runtime")
        marker = _cell(NOT_READ, "no usable configuration names a marker root")
        adapter = _cell(NOT_READ, "no usable configuration names an adapter")
        adapter_interpreter = _cell(NOT_READ, "no usable configuration names an adapter"
                                              " interpreter")
        # Asked separately from the cell below, because settings that could not be read and
        # settings that deliberately configure no journal are different answers. Reporting the
        # first as an empty journal would say this hook has recorded nothing, when what
        # happened is that nobody could tell where it would record.
        firing = _cell(NOT_READ, "no usable configuration names a journal to read")
    else:
        settings = _cell(found.state, "settings read", configuration=str(path),
                         configurationSource=source, mode=config.get("mode"),
                         dbPath=config.get("dbPath"),
                         isolationAssertedBy=config.get("isolationAssertedBy"))
        executable = Path(config["relayExecutable"])
        relay = presence(executable, "the configured runtime")
        # The launcher a plugin-owned registration runs has to release the turn in silence
        # when this file is gone, because a hook that reports its own faults to the host costs
        # turns. Silence is right there and wrong here, so the question is asked once, out of
        # band, where an operator can see it: the entry point lives in a checkout this command
        # does not own, and a checkout that moved or was deleted leaves a registration that
        # still looks correct and a Stop that is never judged.
        adapter = (presence(config["adapterEntryPoint"], "the recorded adapter entry point")
                   if config.get("adapterEntryPoint")
                   else _cell(NOT_READ, "these settings record no adapter entry point, which is"
                                        " the " + OWNER_USER + " owner's shape: its registered"
                                        " command line carries the adapter instead"))
        # The launcher starts two programs and either one can go missing on its own. A virtual
        # environment removed after installation leaves the script in place and the interpreter
        # gone, and then nothing runs at all: the same outage, a different repair.
        adapter_interpreter = (
            _recorded_program_cell(config.get("adapterInterpreter"),
                                   "the recorded adapter interpreter")
            or _cell(NOT_READ, "these settings record no adapter interpreter, which is the "
                     + OWNER_USER + " owner's shape: its registered command line carries the"
                     " interpreter instead"))
        offers = (_offers_guard(executable, config.get("timeoutSeconds")
                                or DEFAULT_TIMEOUT_SECONDS)
                  if relay["value"] == reading.PRESENT
                  else _cell(NOT_READ, "the configured runtime could not be asked: "
                                       + relay["evidence"]))
        marker = presence(config["markerRoot"], "the configured marker root", directory=True)
        firing = _journal_cell(config)

    return {
        "command": "hook-status",
        "codexHome": str(home),
        "event": event,
        # Read before the registration cell, because the registration cell only ever looks in
        # the hook file. A plugin-owned hook is registered in the package's manifest, so that
        # cell says nothing is registered and is right about the file and wrong about the host.
        # This names the owner the settings recorded, so the two readings stay distinguishable.
        # The owner these settings record, and nothing more. Writing the settings is not
        # registering anything -- the plugin owner deliberately registers nothing here -- so
        # naming the package manifest as the place it is registered would report an
        # installation this command never looked for. Where a plugin-owned registration lives,
        # and whether it exists at all, is a separate reading nothing here makes.
        "registrationOwner": (
            _cell(owner_of(config), "the owner recorded in these settings",
                  registeredWhere=(str(home / "hooks.json")
                                   if owner_of(config) == OWNER_USER else None),
                  note=("these settings name the " + OWNER_PLUGIN + " as the owner, so the"
                        " registration is the package's to declare and no installed package"
                        " was read here" if owner_of(config) == OWNER_PLUGIN else None))
            if config else
            _cell(NOT_READ, "the settings were not read, so the owner was not established")),
        "registration": registration,
        "adapterEntryPoint": adapter,
        "adapterInterpreter": adapter_interpreter,
        "registeredCommandTarget": target,
        "registeredInterpreter": interpreter,
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
                 " it, that the runtime it names can answer, or that any turn was judged."
                 " This command writes nothing of its own, but it is not inert: answering"
                 " whether the runtime offers " + GUARD_COMMAND + " means running that runtime"
                 " with --help, and what that runtime does is outside this command's control."),
    }
