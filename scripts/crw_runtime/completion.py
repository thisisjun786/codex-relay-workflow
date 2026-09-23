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
import hashlib
import json
import os
import re
import shlex
import shutil
import signal
import stat
import subprocess
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

from . import firing, hooks, hostrecord, pointer, reading

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
# The margin that launcher keeps between its own deadline and the adapter's budget, mirrored
# from the same file. The two numbers only mean something together: the launcher waits
# min(timeoutSeconds + MARGIN, CEILING), so a budget above CEILING - MARGIN collapses the margin
# the launcher exists to keep, and the launcher's deadline then arrives while the adapter is
# still writing the record of its own timeout.
#
# This bound used to live only in scripts/crw_transition/steps.py, which meant the transition
# refused such a document and runtime_install.py hook --owner plugin accepted it. That was
# tolerable while the packaged launcher was reached only through the cache; it is not now that
# the same command installs the fallback every plugin host depends on, so the bound moved here,
# where both writers already validate.
LAUNCHER_MARGIN_SECONDS = 2
MAX_PLUGIN_GUARD_SECONDS = LAUNCHER_CEILING_SECONDS - LAUNCHER_MARGIN_SECONDS

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


def override_complaints(owner, environ=None):
    """Whether the settings path this owner would install can be found again at every Stop.

    The user owner writes the path it resolved into the command it registers, so a relative
    override is settled once, at install time, and never resolved again. A plugin-declared
    command carries no such argument at all, and that is the whole difference. The launcher
    has to rediscover the path at every Stop, from an environment this install cannot reach
    into: it reads the same variable and then falls back to the Codex home. An override is
    therefore refused whatever it is spelled like. A relative one names a different file in
    every workspace; an absolute one names the right file only in processes that happen to
    carry the same variable, and a Codex started from another shell reads the Codex home,
    where this install wrote nothing, and releases every Stop in silence.

    Refused at installation rather than papered over in the launcher, because this is the one
    moment where the install and the sessions that follow it are both in view. What it leaves
    behind is an invariant the launcher can rely on: plugin-owned settings are at the path it
    derives on its own.
    """
    if owner != OWNER_PLUGIN:
        return []
    environ = os.environ if environ is None else environ
    override = environ.get(CONFIG_ENV)
    if override:
        return [CONFIG_ENV + " is set to " + str(override) + ", and a " + OWNER_PLUGIN
                + "-owned registration carries no settings argument: the hook rediscovers the"
                " path at every Stop from the environment that session was started with, not"
                " from this one. A Codex started without this variable reads the Codex home,"
                " where this install would have written nothing, and releases every Stop"
                " saying nothing. Unset it so the settings land where the hook looks"]
    return []


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

# The copy of the packaged launcher this command installs beside these settings, and why it
# exists at all. A plugin-declared hook command is fixed when a turn starts, with the version
# cache path already resolved into it. Replacing the package removes that directory whole, so a
# turn still running holds an absolute path to a file that is gone -- and python3 exits 2 for a
# missing script, which is the number the hook protocol reads as "block this turn". One removed
# directory therefore became a termination loop rather than one silent miss.
#
# The declaration opens the cache copy first and this one only when the cache copy cannot be
# opened. That ordering matters: the cache copy is always the current version, so this copy can
# never outrank it, and the only moment it is reached is the moment the cache cannot answer.
LAUNCHER_NAME = "crw-stop-hook.py"
LAUNCHER_SOURCE = "plugins/crw/wiring/crw_stop_hook.py"
# Mirrored from that file, which cannot import this module. It marks the file as CRW's to
# replace and establishes nothing else: not who wrote it, and not that its bytes are whole. The
# digest reported beside it answers the second question; nothing answers the first.
LAUNCHER_MARKER = "crw-stop-hook/1"

LAUNCHER_PLACED = "launcher_placed"
LAUNCHER_UNCHANGED = "launcher_unchanged"
LAUNCHER_WOULD_PLACE = "launcher_would_place"
LAUNCHER_FOREIGN = "launcher_foreign"
LAUNCHER_NOT_A_FILE = "launcher_not_a_file"
LAUNCHER_UNREADABLE = "launcher_unreadable"
LAUNCHER_SOURCE_MISSING = "launcher_source_missing"
LAUNCHER_APPLIED_UNVERIFIED = "launcher_applied_unverified"
LAUNCHER_CHANGED_UNDERNEATH = "launcher_changed_underneath"
# The outcomes that leave a usable fallback, so a caller gates on one name rather than a list.
LAUNCHER_SETTLED = (LAUNCHER_PLACED, LAUNCHER_UNCHANGED, LAUNCHER_WOULD_PLACE)

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

# The cache key for a configuration that names no journal root at all. A sentinel rather than a
# normalised empty string, because that normalises to "/" -- a directory a configuration may
# legitimately name, which would then share this one's snapshot.
NO_ROOT = "<no journal root>"

# Refuse to open anything but a directory, where the platform can. Zero elsewhere, which only
# costs a later NotADirectoryError from scandir -- the same reading, taken one step later.
_DIRECTORY = getattr(os, "O_DIRECTORY", 0)


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
                and budget > MAX_PLUGIN_GUARD_SECONDS:
            # The packaged launcher waits min(budget + MARGIN, CEILING). Refusing only at the
            # ceiling let 8 through, and at 8 the launcher's own deadline is 9 while the adapter
            # is still allowed 8: the margin is gone, the launcher kills the adapter first, and
            # the turn is released without the record that explains it.
            found.append("timeoutSeconds must not exceed " + str(MAX_PLUGIN_GUARD_SECONDS)
                         + " when owner is " + OWNER_PLUGIN + ", because the packaged launcher"
                         " waits the budget plus " + str(LAUNCHER_MARGIN_SECONDS) + "s capped at "
                         + str(LAUNCHER_CEILING_SECONDS) + "s and has to outlast the adapter"
                         " it runs")
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


def read_configuration(path, hold=False, descriptor=None):
    """Read the settings as a reading, so absent, unreadable and unreachable stay three answers.

    The shape is checked outside the reading region on purpose. Handing the shape check to
    read_json would report a readable file that says the wrong thing as an unreadable one, and
    an operator would go looking for a permission problem that is not there.

    'hold' is passed through for a caller that will use the reading's identity as a cache key,
    and that caller releases it.
    """
    found = reading.read_json(path, "the completion hook configuration", hold=hold,
                              descriptor=descriptor)
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

    --socket is passed only when it was configured, and it is what lets the guard tell that the
    store it is about to read belongs to another App Server. A store records the socket it serves,
    so the comparison needs the socket this installation expects; with none configured there is
    nothing to compare and an inherited state directory pointing at another installation's store
    is read as though it were this one's. It goes before the subcommand because it is a global
    option, and the relay's parser takes it there.

    --now is never passed. The time a decision is made is the guard's to observe.
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
    version = _python_said(said)
    if finished.returncode != 0 or version is None:
        raise ValueError(str(candidate) + " is executable but does not run Python; every Stop"
                                          " would succeed at running it and never reach the"
                                          " adapter")
    if version < SUPPORTED_PYTHON:
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


# ------------------------------------------------------------------ which Stop event this is


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
    if stat.S_ISLNK(found.st_mode):
        try:
            found = os.stat(path)
        except FileNotFoundError:
            return TRANSCRIPT_ABSENT
        except (OSError, ValueError):
            return TRANSCRIPT_UNREACHABLE
    if not stat.S_ISREG(found.st_mode):
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
        if not stat.S_ISREG(os.fstat(handle).st_mode):
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


# ------------------------------------------------------------------ this hook's own record


def journal(config, record, slot=None):
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
    if (policy == FAULTS_ONLY and record.get("adapterOutcome") in QUIET
            and record.get("acceptance") in (ACCEPTED, DUPLICATE)):
        # An answer to an event this hook could identify is not a fault. An answer given without
        # an identity is reported even here: it is the one invocation no accepted record covers.
        return None
    root = config.get("journalRoot")
    if not root:
        return None
    # The slot run() chose when it started, so an accepted record naming this row names the day
    # the row is actually under, even when the invocation crosses midnight.
    day, name = slot or new_slot()
    directory = Path(root).expanduser() / day
    target = directory / (name + ".json")
    created = False
    try:
        directory.mkdir(parents=True, exist_ok=True)
        handle = os.open(str(target), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        created = True
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
        # Only a file this call created is its to remove. A name it could not create belongs to
        # whatever wrote it first -- this invocation's own finished row, when the fault path
        # retries the slot run() chose -- and removing it would erase that record.
        if created:
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
        # The settings are read FIRST, before the payload is looked at. They are what says where
        # a record goes, so reading them second meant a payload this hook could not parse was
        # released with nothing written down anywhere - the one class of invocation that most
        # needs a record, silently absent from the firing evidence.
        path = configuration_path(codex_home, environ, settings)
        record["configuration"] = str(path)
        config, failed, detail, _found = read_configuration(path)
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
        # This path prints nothing, so whatever was decided before the fault, nothing was held.
        record["held"] = False
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


def _release(config, record, outcome, detail, started, slot=None):
    record["adapterOutcome"] = outcome
    record["detail"] = detail
    record["elapsedMs"] = round((time.monotonic() - started) * 1000)
    record["journalledAs"] = journal(config, record, slot)
    return None


# ------------------------------------------------------------------ reading the journal per Stop event


PER_EVENT_PREDICATE = "one accepted record per Stop event (CRW-212)"
SUPERSEDED_PREDICATE = ("exactly one row per (session, turn); superseded by CRW-212 because a"
                        " continuation is a new Stop event in the same turn")
TRUE = "TRUE"
FALSE = "FALSE"
UNREADABLE_VERDICT = "UNREADABLE"


def _within(value, since, until):
    if not isinstance(value, str):
        return since is None and until is None
    return (since is None or value >= since) and (until is None or value < until)


def _read_json(path):
    try:
        return json.loads(Path(path).read_bytes().decode("utf-8")), True
    except (OSError, ValueError):
        return None, False


# The outcomes run() records before any guard is asked, and those that report asking one.
BEFORE_THE_GUARD = (STDIN_UNREADABLE, STDIN_NOT_JSON, STDIN_NOT_OBJECT, CONFIG_ABSENT,
                    CONFIG_UNREADABLE, CONFIG_UNREACHABLE, CONFIG_MALFORMED)
FROM_THE_GUARD = tuple(outcome for outcome in OUTCOMES
                       if outcome not in BEFORE_THE_GUARD and outcome != ADAPTER_FAULTED)
# The outcomes each acceptance ends in, besides a fault. An invocation that never reached an
# event (acceptance None) stopped at its input.
OUTCOMES_OF = {None: BEFORE_THE_GUARD, UNESTABLISHED: FROM_THE_GUARD, ACCEPTED: FROM_THE_GUARD,
               UNCLAIMABLE: FROM_THE_GUARD, CLAIM_FAILED: FROM_THE_GUARD,
               DUPLICATE: (DUPLICATE_INVOCATION,), UNARBITRATED: (ARBITRATION_FAILED,)}


# The fields run() puts in every record before anything can end it, and those it adds on the way.
ROW_FIELDS = ("recordVersion", "event", "at", "adapterOutcome", "processEnding", "stdoutReading",
              "guardState", "guardDecision", "guardMode", "assignmentId", "guardRecordedAs", "held",
              "eventKey", "eventIdentity", "identityScanMs", "acceptance", "acceptedAs",
              "guardInvoked", "configuration", "elapsedMs")
PAYLOAD_FIELDS = ("sessionId", "turnId", "stopHookActive")
GUARD_CALL_FIELDS = ("exitCode", "signal", "errno", "guardElapsedMs", "guardStderr")
ANSWER_FIELDS = ("observation", "counters")
IDENTITY_FIELDS = ("established", "reason", "answerItem", "transcriptPath", "scannedBytes",
                   "scannedLines")


def _row_fields_written(row):
    """Whether a row carries every field run() writes on the path its outcome names.

    Presence, not agreement: the values the adapter observed once -- timings, the transcript path,
    the guard's stderr and receipt fields -- appear in this record only, so nothing else can
    contradict them. Their absence is still a record run() did not write.
    """
    if any(field not in row for field in ROW_FIELDS) or row.get("event") != EVENT:
        return False
    outcome, acceptance = row.get("adapterOutcome"), row.get("acceptance")
    if outcome == ADAPTER_FAULTED:
        if not isinstance(row.get("fault"), str):
            return False
    elif "detail" not in row:
        return False
    if outcome in FROM_THE_GUARD:
        if any(field not in row for field in GUARD_CALL_FIELDS) or not _outcome_follows(row):
            return False
    elif outcome != ADAPTER_FAULTED and any(field in row for field in GUARD_CALL_FIELDS):
        # A guard call's fields on a row that asked nothing.
        return False
    if outcome == GUARD_ANSWERED:
        if any(field not in row for field in ANSWER_FIELDS):
            return False
    elif outcome != ADAPTER_FAULTED and any(field in row for field in ANSWER_FIELDS):
        return False
    if row.get("guardMode") not in MODES + (None,):
        return False
    if acceptance is not None:
        # The payload was read, and the transcript with it.
        identity = row.get("eventIdentity")
        if (any(field not in row for field in PAYLOAD_FIELDS) or not isinstance(identity, dict)
                or any(field not in identity for field in IDENTITY_FIELDS)
                or not _is_count(row.get("identityScanMs"), zero=True)
                or not _is_count(identity.get("scannedBytes"), zero=True)
                or not _is_count(identity.get("scannedLines"), zero=True)):
            return False
    return True


SAID = (SAID_NOTHING, SAID_A_VERDICT, SAID_AN_ERROR_RECORD, SAID_SOMETHING_UNREADABLE)
ENDINGS = (NOT_STARTED, EXITED, SIGNALLED, TIMED_OUT)


def _outcome_follows(row):
    """Whether a guard outcome is the one outcome_of() reaches from the call the row records.

    The process ending, the exit code and what stdout said are recorded; the verdict itself is not,
    so a verdict printed by a run that exited cleanly may have been answered or incomplete.
    """
    said, how = row.get("stdoutReading"), row.get("processEnding")
    if said not in SAID or how not in ENDINGS:
        return False
    code, signal, errno_name = row.get("exitCode"), row.get("signal"), row.get("errno")
    # What invoke_guard() records with each ending: an exit code only from a process that exited,
    # a signal only from one that was signalled, an errno only from one that never started.
    if how == EXITED:
        if not _is_count(code, zero=True) or signal is not None or errno_name is not None:
            return False
    elif how == SIGNALLED:
        if code is not None or not _is_count(signal) or errno_name is not None:
            return False
    elif how == NOT_STARTED:
        if code is not None or signal is not None or errno_name is None:
            return False
    elif code is not None or signal is not None or errno_name is not None:
        return False
    ending = {"ending": how, "code": code}
    if how == EXITED and said == SAID_A_VERDICT and row.get("exitCode") == GUARD_EXIT_OK:
        return row.get("adapterOutcome") in (GUARD_VERDICT_INCOMPLETE, GUARD_ANSWERED)
    return outcome_of(ending, said, None) == row.get("adapterOutcome")


def _guard_result_written(record):
    """Whether a record's guard result is one run() writes.

    Only an answer carries a decision, and it holds exactly when it blocks: verdict_complaints turns
    every other pairing into guard_verdict_incomplete. Every other outcome carries no decision, no
    state and no hold, except a fault, which may keep what it had reached and never holds, because
    the fault path prints nothing.
    """
    outcome, decision = record.get("adapterOutcome"), record.get("guardDecision")
    state, held = record.get("guardState"), record.get("held")
    if not isinstance(held, bool) or not (state is None or isinstance(state, str)):
        return False
    if outcome == GUARD_ANSWERED:
        return decision in DECISIONS and held == (decision == BLOCK)
    if held:
        return False
    if outcome == ADAPTER_FAULTED:
        return decision is None or decision in DECISIONS
    # No answer, so no decision, no state and none of the receipts an answer carries.
    return (decision is None and state is None and record.get("assignmentId") is None
            and record.get("guardRecordedAs") is None)


OUTCOME_FIELDS = ("ledgerVersion", "eventKey", "sessionId", "turnId", "journalPolicy",
                  "adapterOutcome", "guardDecision", "guardState", "held", "attemptRow", "at")


def _ledger_shape(body, key, outcome):
    """Whether an accepted record carries every field its kind is written with, and names its key."""
    if (not isinstance(body, dict) or body.get("eventKey") != key
            or body.get("ledgerVersion") != LEDGER_VERSION):
        return False
    for field in ("sessionId", "turnId"):
        if not isinstance(body.get(field), str) or not body.get(field):
            return False
    if outcome:
        # The owner's outcome: it got past its claim, so it asked the guard or faulted, and its
        # guard result is one run() writes.
        return (all(field in body for field in OUTCOME_FIELDS)
                and isinstance(body.get("at"), str)
                and (body.get("adapterOutcome") in FROM_THE_GUARD
                     or body.get("adapterOutcome") == ADAPTER_FAULTED)
                and body.get("journalPolicy") in JOURNAL_POLICIES
                and _guard_result_written(body)
                and (body.get("attemptRow") is None or isinstance(body.get("attemptRow"), str)))
    claimed_by = body.get("claimedBy")
    return (isinstance(body.get("claimedAt"), str) and isinstance(body.get("stopHookActive"), bool)
            and isinstance(body.get("answerItem"), str) and bool(body.get("answerItem"))
            and isinstance(claimed_by, dict) and isinstance(claimed_by.get("attemptRow"), str)
            and _is_count(claimed_by.get("pid"))
            and isinstance(claimed_by.get("hostLedger"), str) and bool(claimed_by.get("hostLedger"))
            # A claim is filed under the key of the event it names, recomputed here, so a claim
            # whose session, turn, flag or answer was not that event's is not read as its record.
            and key == event_key(body["sessionId"], body["turnId"], body["stopHookActive"],
                                 body["answerItem"]))


def _row_shape(row):
    """Whether a version-2 row is one run() writes.

    Checked before any window is applied. A row that names an event needs its session, turn and
    key; one that could not be identified needs its reason (its session and turn may be the very
    fields that were missing); one that never reached an event carries none of them. And every row
    carries what its acceptance is written with: the outcomes that acceptance ends in, whether the
    guard was asked, and a guard result only where one was reached (_guard_result_written).
    """
    if not isinstance(row.get("at"), str) or not row.get("at") or not _row_fields_written(row):
        return False
    acceptance, key, identity = row.get("acceptance"), row.get("eventKey"), row.get("eventIdentity")
    outcome, asked = row.get("adapterOutcome"), row.get("guardInvoked")
    if (acceptance not in OUTCOMES_OF or not isinstance(asked, bool)
            or not _guard_result_written(row)):
        return False
    if outcome == ADAPTER_FAULTED:
        # A fault may end any invocation that got past reading its input, except the two that
        # release before anything else can happen.
        if acceptance in (DUPLICATE, UNARBITRATED):
            return False
    elif outcome not in OUTCOMES_OF[acceptance]:
        return False
    elif outcome in FROM_THE_GUARD:
        if asked is not True or not isinstance(row.get("processEnding"), str):
            return False
    elif ((asked and acceptance != DUPLICATE) or row.get("processEnding") is not None
          or row.get("stdoutReading") is not None):
        # Nothing was asked. A duplicate that says it asked is left to the verdict, which reads
        # it FALSE rather than merely unreadable.
        return False
    keyed = isinstance(key, str) and LEDGER_NAME.match(key + ".json") is not None
    named = all(isinstance(row.get(field), str) and row.get(field)
                for field in ("sessionId", "turnId"))
    if acceptance in (ACCEPTED, DUPLICATE, UNCLAIMABLE, CLAIM_FAILED, UNARBITRATED):
        # The key is recomputed from the row's own session, turn, flag and answer, so a row about
        # another session or turn than its key's is not counted as a record of that event.
        if not (keyed and named and isinstance(identity, dict)
                and identity.get("established") is True and identity.get("reason") is None
                and isinstance(row.get("stopHookActive"), bool)
                and isinstance(identity.get("answerItem"), str) and bool(identity.get("answerItem"))
                and key == event_key(row["sessionId"], row["turnId"], row["stopHookActive"],
                                     identity["answerItem"])):
            return False
        where = row.get("acceptedAs")
        if acceptance == ACCEPTED:
            return where == LEDGER_DIRECTORY + "/" + key + ".json"
        if acceptance == DUPLICATE:
            return where in (LEDGER_DIRECTORY + "/" + key + ".json",
                             "/".join(HOST_LEDGER_PARTS + (key + ".json",)))
        return where is None
    if acceptance == UNESTABLISHED:
        return (key is None and isinstance(identity, dict)
                and identity.get("established") is False
                and isinstance(identity.get("reason"), str))
    return key is None and identity is None


def _host_shape(body, key):
    """Whether a host file carries every field it is written with, and is filed under its own key."""
    if (not isinstance(body, dict) or body.get("eventKey") != key
            or body.get("ledgerVersion") != LEDGER_VERSION):
        return False
    for field in ("sessionId", "turnId", "answerItem", "claimedAt"):
        if not isinstance(body.get(field), str) or not body.get(field):
            return False
    claimed_by = body.get("claimedBy")
    return (isinstance(body.get("stopHookActive"), bool) and isinstance(claimed_by, dict)
            and isinstance(claimed_by.get("attemptRow"), str) and _is_count(claimed_by.get("pid"))
            and "journalRoot" in claimed_by
            and (claimed_by.get("journalRoot") is None
                 or isinstance(claimed_by.get("journalRoot"), str))
            and key == event_key(body["sessionId"], body["turnId"], body["stopHookActive"],
                                 body["answerItem"]))


def _is_count(value, zero=False):
    return (isinstance(value, int) and not isinstance(value, bool)
            and (value >= 0 if zero else value > 0))


# The guard's result as the outcome and the accepted row both record it. One owner writes both in
# one run, from one record, so any difference is a record that is not what that run wrote.
GUARD_RESULT_FIELDS = ("adapterOutcome", "guardDecision", "guardState", "held")


def _identity_of(spelled):
    """The (device, inode) a path reaches, or None when it reaches nothing that can be stated."""
    try:
        found = os.stat(os.path.abspath(str(Path(spelled).expanduser())))
    except (OSError, ValueError):
        return None
    return found.st_dev, found.st_ino


def _read_row(root, named):
    """The row an outcome names, read only from the shape a row takes under its own root."""
    parts = named.split("/") if isinstance(named, str) else []
    if len(parts) != 2 or not JOURNAL_DAY.match(parts[0]) or not JOURNAL_NAME.match(parts[1]):
        return None
    body, readable = _read_json(Path(root) / parts[0] / parts[1])
    return body if readable and isinstance(body, dict) else None


def stop_events(roots, since=None, until=None, session=None, turn=None, hosts=None):
    """Whether every Stop event recorded under these journal roots was accepted exactly once.

    Read-only. It reads the journal roots, the host ledgers their claims name
    (<CODEX_HOME>/crw-completion-hook/stop-events), and the host ledgers of any Codex homes given
    in hosts. An event is judged as a unit: the window (since/until/session/turn) chooses the
    events it reaches -- every event one of whose records (host file, claim, outcome or row) falls
    in it -- and every record of a chosen event is then checked, whatever its own time. Rows that
    name no event are chosen one by one. One verdict, ordered FALSE > UNREADABLE > TRUE:

    FALSE when an event was accepted more than once -- claims for one key in two distinct roots,
    several accepted rows for one key, an accepted row whose claim is not in its root (acceptance
    that happened outside the ledger), or a duplicate row that asked the guard anyway.

    UNREADABLE when the reading cannot vouch for what it read -- a listing that failed (including
    an accepted/ or host ledger path that is not a directory); a row, claim, outcome or host file
    that does not parse or lacks what its kind is written with, including one whose own session,
    turn, stop_hook_active and answer item do not hash to its key; a row version this reader does
    not know; an entry in a ledger that is not one of its records; an outcome with no claim in its
    root, or naming another session or turn than its claim; a claim with no outcome in its root
    (its owner died before answering); an outcome whose accepted row is missing or is not that
    event's accepted row (including none under every_invocation); records of one event that
    disagree (recordsThatDisagree): the host file, the claim, the outcome and the accepted row are
    written by one owner in one run, so they name one slot and one process, the accepted row sits
    in that slot, the outcome and that row carry one guard result, and a duplicate that found the
    accepted record in its own root finds it there; a host file whose event has no
    claim in the root it names, or names a root this reading was not given (its owner died between
    the two files, its root could not hold the claim, or the root was left out); a claim whose host
    file is not in the ledger it names; a duplicate whose event has no claim in any root read; a
    ledger written under journalPolicy no_journal; any invocation it cannot judge -- identity not
    established, no owner, no root for the claim, no event reached, or a row from before event
    identity; or nothing to judge. An invocation it cannot judge was answered without
    deduplication, or not answered, so whether its Stop was answered once is not known;
    unjudgedInvocations and legacyRows count them by reason.

    The old per-(session, turn) reading is reported too, labelled as superseded. Roots and host
    ledgers are deduplicated by the (device, inode) they reach.
    """
    answer = {"predicate": PER_EVENT_PREDICATE, "verdict": None, "roots": [], "hostLedgers": [],
              "window": {"since": since, "until": until, "session": session, "turn": turn},
              "events": 0, "eventsWithMoreThanOneAcceptance": [], "duplicateInvocations": 0,
              "unjudgedInvocations": {}, "legacyRows": 0, "acceptedWithoutOutcome": [],
              "outcomesWithoutClaim": [], "acceptedRowsWithoutLedger": [],
              "guardAskedOnDuplicate": [], "ledgerUnreadable": [], "rowsUnreadable": [],
              "foreignLedgerEntries": [], "invocationsUnrecorded": [], "acceptedRowsMissing": [],
              "duplicatesWithoutClaim": [], "hostFilesWithoutClaim": [],
              "claimsWithoutHostFile": [], "recordsThatDisagree": [],
              "turnsWithMoreThanOneEvent": 0,
              "supersededPerTurn": {"predicate": SUPERSEDED_PREDICATE, "pairs": 0,
                                    "pairsWithMoreThanOneRow": 0}}
    try:
        return _read_stop_events(answer, roots, since, until, session, turn, hosts)
    except Exception as error:  # noqa: BLE001 - a reading that cannot finish cannot vouch
        answer["verdict"] = UNREADABLE_VERDICT
        answer["readerFault"] = type(error).__name__ + ": " + str(error)
        return answer


def _read_stop_events(answer, roots, since, until, session, turn, hosts):
    """The reading stop_events() answers with, filled into answer."""

    def in_window(stamp, owner, of_turn):
        return (_within(stamp, since, until)
                and (session is None or owner == session)
                and (turn is None or of_turn == turn))

    # Everything is read before any window is applied: a window chooses which events are judged,
    # never which of an event's records are looked at.
    listing_failed = False
    reached = {}
    read = []
    rows = []
    for spelled in roots:
        root = Path(os.path.abspath(str(Path(spelled).expanduser())))
        entry = {"root": str(root), "state": None}
        answer["roots"].append(entry)
        try:
            found = os.stat(str(root))
        except FileNotFoundError:
            entry["state"] = "absent"
            continue
        except (OSError, ValueError) as error:
            entry["state"], entry["detail"] = "unreadable", str(error)
            listing_failed = True
            continue
        identity = (found.st_dev, found.st_ino)
        if identity in reached:
            entry["state"] = "same_root_as_another_spelling"
            continue
        reached[identity] = root
        entry["state"] = "read"
        try:
            days = sorted(e.name for e in os.scandir(str(root))
                          if e.is_dir() and JOURNAL_DAY.match(e.name))
            if (root / LEDGER_DIRECTORY).is_dir():
                ledger = sorted(e.name for e in os.scandir(str(root / LEDGER_DIRECTORY)))
            elif os.path.lexists(str(root / LEDGER_DIRECTORY)):
                # Something is there that is not a directory: the accepted records cannot be
                # read, which is not the same answer as "none were written".
                raise NotADirectoryError(str(root / LEDGER_DIRECTORY) + " is not a directory")
            else:
                ledger = []
        except OSError as error:
            entry["state"], entry["detail"] = "unreadable", str(error)
            listing_failed = True
            continue
        claims, outcomes = {}, {}
        # Every claim file present, readable or not: a torn claim is still a claim, so an accepted
        # row naming it is unreadable evidence rather than acceptance outside the ledger.
        claim_files = {name[:-len(".json")] for name in ledger if LEDGER_NAME.match(name)}
        for name in ledger:
            path = root / LEDGER_DIRECTORY / name
            if OUTCOME_NAME.match(name):
                key, table = name[:-len(OUTCOME_SUFFIX)], outcomes
            elif LEDGER_NAME.match(name):
                key, table = name[:-len(".json")], claims
            else:
                answer["foreignLedgerEntries"].append(str(path))
                continue
            body, readable = _read_json(path)
            if not readable or not _ledger_shape(body, key, table is outcomes):
                answer["ledgerUnreadable"].append(str(path))
                continue
            table[key] = body
        for day in days:
            try:
                names = sorted(e.name for e in os.scandir(str(root / day))
                               if e.is_file() and JOURNAL_NAME.match(e.name))
            except OSError as error:
                entry["state"], entry["detail"] = "unreadable", str(error)
                listing_failed = True
                break
            for name in names:
                where = str(root / day / name)
                row, readable = _read_json(root / day / name)
                if (not readable or not isinstance(row, dict)
                        or row.get("recordVersion") not in (1, RECORD_VERSION)
                        or (row.get("recordVersion") == RECORD_VERSION
                            and not _row_shape(row))):
                    answer["rowsUnreadable"].append(where)
                    continue
                rows.append((str(root), where, row))
        read.append((str(root), claims, outcomes, claim_files))
    files_of = {root: files for root, _claims, _outcomes, files in read}
    every_claim = set()
    for files in files_of.values():
        every_claim |= files

    # The host ledgers: every one a claim names, and those of the Codex homes given.
    wanted = [Path(home).expanduser().joinpath(*HOST_LEDGER_PARTS) for home in (hosts or [])]
    for _root, claims, _outcomes, _files in read:
        wanted.extend(Path(body["claimedBy"]["hostLedger"]) for body in claims.values())
    host_files, ledger_identity, ledgers_reached = {}, {}, set()
    for spelled in wanted:
        ledger = os.path.abspath(str(spelled))
        if ledger in ledger_identity:
            continue
        ledger_identity[ledger] = None
        entry = {"ledger": ledger, "state": None}
        answer["hostLedgers"].append(entry)
        try:
            found = os.stat(ledger)
        except FileNotFoundError:
            entry["state"] = "absent"
            continue
        except (OSError, ValueError) as error:
            entry["state"], entry["detail"] = "unreadable", str(error)
            listing_failed = True
            continue
        identity = (found.st_dev, found.st_ino)
        ledger_identity[ledger] = identity
        if identity in ledgers_reached:
            entry["state"] = "same_ledger_as_another_spelling"
            continue
        try:
            if not stat.S_ISDIR(found.st_mode):
                raise NotADirectoryError(ledger + " is not a directory")
            names = sorted(e.name for e in os.scandir(ledger))
        except OSError as error:
            entry["state"], entry["detail"] = "unreadable", str(error)
            listing_failed = True
            continue
        ledgers_reached.add(identity)
        entry["state"] = "read"
        for name in names:
            path = os.path.join(ledger, name)
            if not LEDGER_NAME.match(name):
                answer["foreignLedgerEntries"].append(path)
                continue
            key = name[:-len(".json")]
            body, readable = _read_json(Path(path))
            if not readable or not _host_shape(body, key):
                answer["ledgerUnreadable"].append(path)
                continue
            host_files.setdefault(key, []).append((identity, path, body))

    # The events the window reaches: any of their records in it.
    selected = set()
    for _root, claims, outcomes, _files in read:
        for key, body in claims.items():
            if in_window(body["claimedAt"], body["sessionId"], body["turnId"]):
                selected.add(key)
        for key, body in outcomes.items():
            if in_window(body["at"], body["sessionId"], body["turnId"]):
                selected.add(key)
    for key, entries in host_files.items():
        for _identity, _path, body in entries:
            if in_window(body["claimedAt"], body["sessionId"], body["turnId"]):
                selected.add(key)
    for _root, _where, row in rows:
        if (row.get("recordVersion") == RECORD_VERSION and row.get("eventKey") is not None
                and in_window(row["at"], row["sessionId"], row["turnId"])):
            selected.add(row["eventKey"])

    counts = answer["unjudgedInvocations"]
    pairs, accepted_rows, duplicate_rows, accepted_at = {}, {}, [], {}
    for root, where, row in rows:
        chosen = in_window(row.get("at"), row.get("sessionId"), row.get("turnId"))
        if chosen and row.get("sessionId") and row.get("turnId"):
            pair = (row["sessionId"], row["turnId"])
            pairs[pair] = pairs.get(pair, 0) + 1
        if row.get("recordVersion") == 1:
            if chosen:
                answer["legacyRows"] += 1
            continue
        acceptance, key = row.get("acceptance"), row.get("eventKey")
        if key is None:
            if not chosen:
                continue
            if acceptance == UNESTABLISHED:
                label = UNESTABLISHED + ":" + row["eventIdentity"]["reason"]
            else:
                # The settings or the payload failed before any event was reached.
                label = "no_event:" + str(row.get("adapterOutcome"))
            counts[label] = counts.get(label, 0) + 1
            continue
        if key not in selected:
            continue
        if acceptance == ACCEPTED:
            accepted_rows.setdefault(key, []).append(where)
            accepted_at.setdefault((root, key), []).append(where)
            if key not in files_of[root]:
                answer["acceptedRowsWithoutLedger"].append(where)
        elif acceptance == DUPLICATE:
            answer["duplicateInvocations"] += 1
            duplicate_rows.append((key, where))
            if row["acceptedAs"] == LEDGER_DIRECTORY + "/" + key + ".json" \
                    and key not in files_of[root]:
                # It found the accepted record in its own root, which does not hold one.
                answer["recordsThatDisagree"].append(where)
            if row.get("guardInvoked") is not False:
                answer["guardAskedOnDuplicate"].append(where)
        else:
            counts[acceptance] = counts.get(acceptance, 0) + 1

    events, events_per_turn = set(), {}
    for key in sorted(selected):
        holding = [root for root, files in files_of.items() if key in files]
        if len(holding) > 1 or len(accepted_rows.get(key, [])) > 1:
            answer["eventsWithMoreThanOneAcceptance"].append(key)
        if holding:
            events.add(key)
        for root, claims, outcomes, files in read:
            claim, outcome = claims.get(key), outcomes.get(key)
            if outcome is not None and key not in files:
                answer["outcomesWithoutClaim"].append(key)
            if claim is None:
                continue
            events_per_turn.setdefault((claim["sessionId"], claim["turnId"]), set()).add(key)
            # One owner wrote the host file, this claim, the outcome and the accepted row in one
            # run: they name one slot and one process, and the accepted row sits in that slot.
            slot, pid = claim["claimedBy"]["attemptRow"], claim["claimedBy"]["pid"]
            claim_path = str(Path(root) / LEDGER_DIRECTORY / (key + ".json"))
            this_root = _identity_of(root)
            for _identity, path, body in host_files.get(key, []):
                owner = body["claimedBy"]["journalRoot"]
                if owner and _identity_of(owner) == this_root and (
                        body["claimedBy"]["attemptRow"] != slot or body["claimedBy"]["pid"] != pid):
                    answer["recordsThatDisagree"].append(path)
            for where in accepted_at.get((root, key), []):
                if where != str(Path(root) / slot):
                    answer["recordsThatDisagree"].append(where)
            named = claim["claimedBy"]["hostLedger"]
            identity = ledger_identity.get(os.path.abspath(named))
            if identity is None or not any(entry[0] == identity
                                           for entry in host_files.get(key, [])):
                answer["claimsWithoutHostFile"].append(
                    str(Path(root) / LEDGER_DIRECTORY / (key + ".json")))
            if outcome is None:
                answer["acceptedWithoutOutcome"].append(key)
                continue
            if (claim["sessionId"], claim["turnId"]) != (outcome["sessionId"], outcome["turnId"]):
                answer["ledgerUnreadable"].append(
                    str(Path(root) / LEDGER_DIRECTORY / (key + OUTCOME_SUFFIX)))
            if outcome["journalPolicy"] == NO_JOURNAL:
                answer["invocationsUnrecorded"].append(key)
            row, policy = outcome["attemptRow"], outcome["journalPolicy"]
            outcome_path = str(Path(root) / LEDGER_DIRECTORY / (key + OUTCOME_SUFFIX))
            # Whether the owner's policy wrote its row: every_invocation always, faults_only for
            # anything but a plain answer, no_journal never.
            kept = (policy == EVERY_INVOCATION
                    or (policy == FAULTS_ONLY and outcome["adapterOutcome"] not in QUIET))
            if row is None:
                if kept:
                    # The row was to be written, so the accepted one's is missing: its write
                    # failed, and what the event was answered with rests on the outcome alone.
                    answer["acceptedRowsMissing"].append(key)
                if accepted_at.get((root, key)):
                    # An accepted row the outcome says was never written.
                    answer["recordsThatDisagree"].append(outcome_path)
            else:
                if not kept or row != slot:
                    answer["recordsThatDisagree"].append(outcome_path)
                named_row = _read_row(root, row)
                if (named_row is None or named_row.get("recordVersion") != RECORD_VERSION
                        or named_row.get("acceptance") != ACCEPTED
                        or named_row.get("eventKey") != key):
                    answer["acceptedRowsMissing"].append(key)
                elif any(named_row.get(field) != outcome.get(field)
                         for field in GUARD_RESULT_FIELDS):
                    answer["recordsThatDisagree"].append(outcome_path)
        for _identity, path, body in host_files.get(key, []):
            owner = body["claimedBy"]["journalRoot"]
            owned_by = reached.get(_identity_of(owner)) if owner else None
            if owned_by is None or key not in files_of.get(str(owned_by), ()):
                answer["hostFilesWithoutClaim"].append(path)
    for key, where in duplicate_rows:
        if key not in every_claim:
            answer["duplicatesWithoutClaim"].append(where)
    answer["events"] = len(events)
    answer["turnsWithMoreThanOneEvent"] = sum(1 for keys in events_per_turn.values()
                                              if len(keys) > 1)
    answer["supersededPerTurn"]["pairs"] = len(pairs)
    answer["supersededPerTurn"]["pairsWithMoreThanOneRow"] = sum(1 for n in pairs.values()
                                                                if n > 1)
    for field in ("eventsWithMoreThanOneAcceptance", "acceptedWithoutOutcome",
                  "outcomesWithoutClaim", "invocationsUnrecorded", "acceptedRowsMissing",
                  "ledgerUnreadable", "duplicatesWithoutClaim", "foreignLedgerEntries",
                  "hostFilesWithoutClaim", "claimsWithoutHostFile", "recordsThatDisagree"):
        answer[field] = sorted(set(answer[field]))
    if (answer["eventsWithMoreThanOneAcceptance"] or answer["acceptedRowsWithoutLedger"]
            or answer["guardAskedOnDuplicate"]):
        answer["verdict"] = FALSE
    elif (listing_failed or answer["ledgerUnreadable"] or answer["rowsUnreadable"]
          or answer["outcomesWithoutClaim"] or answer["acceptedWithoutOutcome"]
          or answer["invocationsUnrecorded"] or answer["acceptedRowsMissing"]
          or answer["foreignLedgerEntries"] or answer["duplicatesWithoutClaim"]
          or answer["hostFilesWithoutClaim"] or answer["claimsWithoutHostFile"]
          or answer["recordsThatDisagree"]
          or answer["unjudgedInvocations"] or answer["legacyRows"] or not events):
        answer["verdict"] = UNREADABLE_VERDICT
    else:
        answer["verdict"] = TRUE
    return answer


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
                  owner=OWNER_USER, adapter_interpreter=None, adapter_entry_point=None,
                  socket=None):
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
    if socket:
        # Written only when there is one, for the same reason owner is: a host that installed
        # before this key existed keeps a byte-identical document, so an ordinary reinstall is
        # still UNCHANGED rather than settings that say something else. Absent means the guard is
        # asked without a socket and compares no provenance, which is what it did before.
        document["socketPath"] = str(_settled(socket))
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


def launcher_path(codex_home):
    """Beside the settings, for the same reason the settings live where they do.

    The packaged bootstrap derives this path from CODEX_HOME and nothing else, so both sides
    compute it identically instead of agreeing about a value.
    """
    return Path(codex_home) / LAUNCHER_NAME


def launcher_kind(path):
    """What is actually at that path, without following anything.

    lstat rather than exists(), because the symlink is the case that matters: replacing through
    one writes to wherever it points, which is a file this command was never given.
    """
    try:
        mode = os.lstat(str(path)).st_mode
    except FileNotFoundError:
        return "absent"
    except OSError as error:
        return "unreadable (" + type(error).__name__ + ")"
    if stat.S_ISLNK(mode):
        return "symlink"
    if stat.S_ISDIR(mode):
        return "directory"
    if stat.S_ISREG(mode):
        return "file"
    return "other"


def place_launcher(path, source, *, apply=False):
    """Install the fallback launcher, and never over something that is not ours.

    Ours means the file carries the marker. That is a claim of ownership, not of provenance: it
    says CRW put a launcher here, not who ran the command and not that the bytes are intact. So
    the answer carries both digests, and a caller comparing them learns what the marker cannot
    tell it.

    The decision is taken twice and only the second one is acted on, the way write_configuration
    does it: the first reading answers the caller, the second happens under the lock, and a file
    that moved in between is reported rather than written over.

    The lock covers this path only. There is deliberately no lock spanning this file and the
    settings. Widening one means reworking a write path that is already proven, and it is not
    needed: every state the two files can be left in is harmless. A launcher with no settings
    stands down, and settings with no launcher are what the host had before this file existed.
    What an interleaving can still do is make a receipt wrong, and the caller closes that by
    reading both paths back at the end rather than by holding a wider lock.
    """
    path, source = Path(path), Path(source)
    answer = {"launcher": str(path), "source": str(source), "outcome": None, "applied": False,
              "wrote": False, "kind": None, "digest": None, "sourceDigest": None}
    try:
        wanted = source.read_bytes()
    except OSError as error:
        answer["outcome"] = LAUNCHER_SOURCE_MISSING
        answer["detail"] = ("the packaged launcher could not be read at " + str(source) + " ("
                            + type(error).__name__ + ": " + str(error) + "), so nothing was"
                            " placed")
        return answer
    answer["sourceDigest"] = hashlib.sha256(wanted).hexdigest()

    def judge():
        kind = launcher_kind(path)
        if kind == "absent":
            return kind, None, None
        if kind != "file":
            return kind, None, LAUNCHER_NOT_A_FILE
        try:
            return kind, path.read_bytes(), None
        except OSError:
            return kind, None, LAUNCHER_UNREADABLE

    kind, found, refusal = judge()
    answer["kind"] = kind
    if refusal == LAUNCHER_NOT_A_FILE:
        answer["outcome"] = refusal
        answer["detail"] = ("the launcher path holds a " + kind + "; this command does not"
                            " follow it and does not replace it")
        return answer
    if refusal == LAUNCHER_UNREADABLE:
        answer["outcome"] = refusal
        answer["detail"] = "a file is there and its bytes could not be read, so it is left alone"
        return answer
    if found is not None:
        answer["digest"] = hashlib.sha256(found).hexdigest()
        if found == wanted:
            answer["outcome"] = LAUNCHER_UNCHANGED
            answer["detail"] = "the launcher this checkout ships is already installed"
            return answer
        if LAUNCHER_MARKER.encode("utf-8") not in found:
            answer["outcome"] = LAUNCHER_FOREIGN
            answer["detail"] = ("a file is already there and it does not carry "
                                + LAUNCHER_MARKER + ", so it is not this command's to replace")
            return answer
    if not apply:
        answer["outcome"] = LAUNCHER_WOULD_PLACE
        answer["detail"] = "would install the launcher; nothing was written"
        return answer
    with hostrecord.Locked(path):
        again_kind, again, again_refusal = judge()
        if again_kind != kind or again != found or again_refusal is not None:
            answer["kind"] = again_kind
            answer["outcome"] = LAUNCHER_CHANGED_UNDERNEATH
            answer["detail"] = ("the launcher path changed after it was read, so nothing was"
                                " written; rerun to decide against the file as it now stands")
            return answer
        hostrecord.atomic_write(path, wanted.decode("utf-8"))
        try:
            back = path.read_bytes()
        except OSError:
            back = None
    answer["applied"] = True
    answer["wrote"] = True
    answer["kind"] = launcher_kind(path)
    answer["digest"] = hashlib.sha256(back).hexdigest() if back is not None else None
    if back != wanted:
        answer["outcome"] = LAUNCHER_APPLIED_UNVERIFIED
        answer["detail"] = ("the launcher was written and could not be read back as written, so"
                            " the fallback cannot be claimed to be installed")
        return answer
    answer["outcome"] = LAUNCHER_PLACED
    answer["detail"] = "installed the launcher this checkout ships"
    return answer


def launcher_state(codex_home, source):
    """What is at the fallback path now, for a command that writes nothing.

    Reported beside the settings rather than merged into them. An installed fallback says a turn
    whose cache was replaced has something to run; it says nothing about whether the hook is
    registered, trusted, or has ever fired.
    """
    path = launcher_path(codex_home)
    state = {"launcher": str(path), "kind": launcher_kind(path), "digest": None,
             "sourceDigest": None, "matchesCheckout": None, "carriesMarker": None}
    try:
        state["sourceDigest"] = hashlib.sha256(Path(source).read_bytes()).hexdigest()
    except OSError:
        state["sourceDigest"] = None
    if state["kind"] != "file":
        return state
    try:
        found = path.read_bytes()
    except OSError:
        state["kind"] = "unreadable"
        return state
    state["digest"] = hashlib.sha256(found).hexdigest()
    state["carriesMarker"] = LAUNCHER_MARKER.encode("utf-8") in found
    state["matchesCheckout"] = (state["sourceDigest"] is not None
                                and state["digest"] == state["sourceDigest"])
    return state


def resource_key(path):
    """One key for one resource, for the spellings that are provably one resource.

    Caching a reading by the raw string read the same file twice when two registrations wrote
    it differently, and the host opens one file: two readings of it in one status call can
    disagree, and the payload then gives two registrations different answers about the same
    thing. So equivalent spellings share a key.

    But only the transformations that PRESERVE what the kernel does. normpath() does two very
    different jobs, and the second one is not sound here:

      - dropping '.' components and duplicate separators names the same file, always;
      - cancelling 'X/..' does not, because the kernel follows X first when X is a symlink, so
        /srv/link/../hook.py and /srv/hook.py are two different files that normpath calls one.
        Sharing a cached reading between them would report another resource's answer -- a wrong
        reading rather than a missing one, which is the failure this whole module exists to
        avoid.

    A trailing separator is left alone for the same reason: it requires a directory, so
    /tmp/hook.py/ and /tmp/hook.py are not interchangeable to lstat. A trailing '.' is the same
    demand written differently and is kept for the same reason.

    So a spelling carrying '..' keys only to itself, and resolve() is not used at all: it walks
    symlinks, which would make the key depend on what a link points at and fail on a loop. What
    this costs is a second reading of one file. What it refuses to cost is a reading of the
    wrong one.
    """
    expanded = os.path.expanduser(str(path))
    parts = expanded.split(os.sep)
    if os.pardir in parts:
        return expanded
    trailing = len(parts) > 1 and parts[-1] in ("", os.curdir)
    kept = [part for index, part in enumerate(parts)
            if part != os.curdir and (part != "" or index == 0)]
    key = os.sep.join(kept) or os.sep
    return key + os.sep if trailing else key


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
    # One probe per resolved interpreter, mapped back to every registration that names it.
    # Probing per registration attached time-separated results to commands that share one
    # executable, so an interpreter replaced or chmodded between probes gave them different
    # startability.
    probed = {}
    # One probe per SPELLING, and deliberately not one per file. That was the right unit while
    # this was a presence check -- a file either exists or does not, however it is named -- and
    # it is the wrong one now that the probe RUNS the program. The kernel hands the invoked
    # pathname to the program as argv[0], a script sees it as $0, and a dispatcher that branches
    # on its own name answers one spelling and refuses another. Sharing one answer between two
    # spellings of one inode published a working reading for a spelling that fails, and it also
    # let a link retargeted after the identity was taken lend its answer to a spelling that
    # still reaches the original. Two spellings are two invocations, so they are two questions.
    return _interpreter_probes(ours, checked, probed)


def _interpreter_probes(ours, checked, probed):
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
                checked.append({"registration": entry["identity"], "word": first,
                                "resolved": first, "probe": probe})
                continue
            resolved = first
        else:
            # A bare name is looked up on PATH, which is what the host does with it too.
            resolved = shutil.which(first) or first
        # The same program can be an interpreter for one registration and the adapter itself
        # for another, and the verdict differs, so the probe is shared only between the two
        # that ask the same question of it.
        interpreter_slot = first != entry["target"]
        # The spelling as the host will invoke it, because that is what the program is given.
        key = (str(resolved), interpreter_slot)
        if key in probed:
            probe = probed[key]
        else:
            probe = presence(resolved, "the registered interpreter")
            if probe["value"] == reading.PRESENT and not os.access(str(resolved), os.X_OK):
                probe = _cell(reading.UNREADABLE, "the registered interpreter is not executable",
                              path=str(resolved))
            elif probe["value"] == reading.PRESENT:
                probe = (_answers_as_an_interpreter(resolved, "the registered interpreter")
                         if interpreter_slot
                         else _unjudged_interpreter(resolved, "the registered interpreter"))
            probed[key] = probe
        checked.append({"registration": entry["identity"], "word": first,
                        "resolved": str(resolved), "probe": probe})
    if not checked:
        return _cell(NOT_READ, "no registration named a program to run the adapter")
    unusable = next((one for one in checked if one["probe"]["value"] != reading.PRESENT), None)
    chosen = unusable or checked[0]
    return _cell(chosen["probe"]["value"],
                 chosen["probe"]["evidence"]
                 + "; this is the first word of the registered command, and a wrapper's own"
                   " target is not followed",
                 interpreters=[one["resolved"] for one in checked],
                 # The worst probe is the cell's value, and every probe is carried beside it.
                 # A consumer asking whether THIS host can start the adapter at all has to be
                 # able to tell one broken registration from every registration being broken,
                 # and the worst-of value alone answers the second question for the first.
                 probes=checked)


# Bounded, because this runs a program on a real host. A slower answer is not a better one:
# past this the reading is unestablished and says so, rather than waiting for a definite
# answer it cannot have.
INTERPRETER_PROBE_SECONDS = 5

# What the probe's own answer can be, beside PRESENT. Declared so a consumer can be checked
# against the whole vocabulary rather than against the cases someone remembered.
INTERPRETER_UNESTABLISHED = (NOT_READ,)


def _startable_from(halves):
    """What a registration's two probe answers say about starting: cannot, could not judge, or
    nothing establishes that it cannot.

    Three answers, and the first of them is deliberately NOT "this registration starts". No
    reading here establishes that. The interpreter probe asks the registered first word to
    answer as a supported Python at the spelling the host resolves, and that is what it
    establishes -- not that the REGISTRATION would start, because the adapter path, its
    readability and the argument convention are separate facts and a wrapper's own target is
    deliberately not followed. Reading a present pair as startability would pick one cause
    among several by guess, which is the thing this answer set exists to stop.

    So True here means "nothing establishes that it cannot start", which is what the rules
    downstream actually need to scope a count, and the payload says as much rather than
    claiming the stronger thing. None stays "could not judge at all" -- a workspace-dependent
    spelling, one this command could not reach, one that did not answer in time -- because that
    is neither, and recording it as cannot-start dropped that registration's journal from every
    question while a blocked neighbour supplied a settled explanation for the whole host.

    The cannot-start side reads firing's DECLARED set rather than restating its members. It was
    written out as two of them, and when the probe learned to answer two more the new ones were
    dropped here while the rule that reads the same set acted on them -- one payload saying a
    registration cannot start and, beside it, counting its journal as a startable peer's.
    """
    if halves == {reading.PRESENT}:
        return True
    if halves & set(firing.CANNOT_START):
        return False
    return None


def _script_cell(named, label):
    """A script some program has to READ to run, probed for what that requires.

    Presence is not the question here either, one step down from the interpreter: the
    interpreter opens this file, and a regular file it cannot open is a file it cannot run. A
    target with no read permission answered PRESENT and the registration read as startable,
    while every Stop died before the adapter's first line.

    Readability is the whole of what is establishable about a script from here -- it is not a
    program this command can ask anything of -- so the cell claims that and no more.
    """
    probe = presence(named, label)
    if probe["value"] == reading.PRESENT and not os.access(str(named), os.R_OK):
        return _cell(reading.UNREADABLE, label + " cannot be read, so the interpreter"
                     " registered to run it cannot open it", path=str(named))
    return probe


def _python_said(said):
    """The version a Python reported, as a pair, or None when that is not what it said.

    One rule for the installer's refusal and the diagnosis probe, because they were two: the
    installer already knew that being executable is not the question and that being a Python is
    not the whole question, and refused an interpreter below the floor -- while diagnosis, on
    the same host, reported the registration startable. A predicate the writer enforces and the
    reader does not is the two of them disagreeing about one host.
    """
    parts = said.split(".")
    if len(parts) != 2 or not all(part.isdigit() for part in parts):
        return None
    return (int(parts[0]), int(parts[1]))


def _interpreter_question():
    """A question only a program that RAN the source can answer.

    A marker printed by the source and looked for in the output is answered by any program
    that repeats its arguments -- /bin/echo prints the source, marker and all -- so "the marker
    appeared" and "this ran Python" are different claims, which is the same mismatch this cell
    was fixed for one round ago. The source therefore asks for something COMPUTED: a nonce this
    call invents, written back reversed. Echoing the source shows the nonce forward, and the
    exact answer is compared rather than searched for.

    It asks for the VERSION in the same breath, because being a Python is not the whole
    question either: one too old to run this adapter fails every Stop and looks identical from
    the hook file. One execution answers both, so the stronger check costs no more.
    """
    nonce = os.urandom(12).hex()
    source = ("import sys;sys.stdout.write(''.join(reversed(" + repr(nonce) + "))"
              " + ' %d.%d' % (sys.version_info[0], sys.version_info[1]))")
    return source, nonce[::-1]


# What this probe accepts as an ANSWER, and what it does with everything else. Stated once,
# because every branch below and both call sites follow from it rather than each deciding for
# itself -- which is how one site came to discard an answer it had already received while
# another produced no verdict at all:
#
#   AN ANSWER      the exact nonce this call invented, computed and written back, beside a
#                  version. Only a program that ran the source can produce it, so it is
#                  evidence whatever the program does with its exit status afterwards. A
#                  version below the floor is still an answer, and its own repair.
#   REFUSED        it ran and would not take the question, which is what a wrapper around an
#                  interpreter does. Nothing about running Python was established.
#   NO ANSWER      it exited cleanly and did not answer. This is where the probe STOPS, and it
#                  is the whole reason it establishes nothing: a program that is not an
#                  interpreter and a wrapper that ignores an option meant for one are both
#                  exactly this, and no reading here separates them. Rounds of narrowing the
#                  non-answers taught that; the observation cannot distinguish its cases, so
#                  it does not answer as though it can, which is what this issue is about.
#   UNJUDGED       the command's shape does not put an interpreter in this word at all. There
#                  is no question to ask, so none is asked and nothing is run.
#
# Only an ANSWER establishes anything, because only a program that ran the source can produce
# it. Every non-answer is an unestablished reading that says which explanations remain open --
# which still closes the defect this probe was added for, since a presence check alone used to
# report "startable" and an unestablished reading does not. The last case never reaches this
# function: its caller answers it, because a gap that produces no verdict is read as a clean
# one.
def _said_invocation(resolved):
    """What this command actually ran, in the words an operator can repeat.

    A reading that runs host programs and does not say so is itself a missing piece of
    evidence, which is the failure this whole answer set exists to close. The receipt has to
    name the program and the invocation, not merely the conclusion drawn from it.

    The source is summarised rather than printed: it is a generated nonce, so the literal text
    differs on every probe and would be noise in a receipt meant to be compared.
    """
    return str(resolved) + " -c <version-and-nonce probe>"


def _end_the_session(started, session):
    """Kill everything the probe started, not only the process it made.

    A wrapper that forks and then hangs leaves its own children running when the direct child
    is killed, so each timeout left a descendant of this diagnosis behind. The probe opens its
    own session for exactly this, and the whole group goes.

    The group is the one taken when the process was STARTED. Asking for it now would ask about
    a leader that may already have exited while a child of its own holds the pipes open --
    which is precisely the shape that times out -- and the lookup fails just when it is needed.

    What this does NOT reach, stated rather than implied: a descendant that calls setsid() puts
    itself in a session no group named here contains, and nothing portable can name it
    afterwards. So the pipes are CLOSED rather than drained -- waiting on a descendant that
    escaped would hang this reading on the very process it failed to kill -- and such a
    descendant outlives the probe. It cannot write a journal record, which is the boundary this
    command is held to; it is a resource this command could not reclaim, and that is a limit
    and not a claim.
    """
    try:
        os.killpg(session, signal.SIGKILL)
    except (OSError, ValueError):
        try:
            started.kill()
        except OSError:
            pass
    for pipe in (started.stdout, started.stderr):
        try:
            if pipe is not None:
                pipe.close()
        except OSError:
            pass
    try:
        started.wait(timeout=INTERPRETER_PROBE_SECONDS)
    except (subprocess.TimeoutExpired, OSError, ValueError):
        pass


def _unjudged_interpreter(resolved, label):
    """The UNJUDGED outcome, reaching the caller as a reading rather than as nothing.

    A registration can execute the adapter directly through its shebang, so the first word is
    the script. Running it with an interpreter's own option would run the ADAPTER with an
    argument it never expected, and any verdict drawn from that would be about a question this
    command invented. So it is not run, and the answer says which question was not asked.
    """
    return _cell(NOT_READ, label + " is the adapter itself rather than a program registered to"
                 " run it, so no interpreter question applies to this word and none was asked",
                 path=str(resolved))


def _answers_as_an_interpreter(resolved, label):
    """Whether this program runs Python, asked by running it.

    A file being there and executable establishes that the path is not empty. It establishes
    nothing about what the host gets when it runs it, and reporting the registration startable
    from that is a capability claimed from a presence check. An interpreter replaced by any
    program that exits quietly -- /bin/true is the whole family, and it is the same family this
    module already refuses to accept for the relay's guard subcommand -- read as a working
    hook.

    Asked the way _offers_guard asks the runtime, and for the same reason exit 0 is not enough
    there: the answer has to come from the program's own output. And not merely CONTAIN the
    answer -- a program that repeats its arguments prints the source back, marker and all --
    so the source asks for something computed and the exact reply is compared.

    What this establishes is a MOMENT. It ran as a Python interpreter when this was asked, and
    the evidence says so, because the host runs it again on the next Stop and this command
    cannot speak for that one.

    Called only where the command's shape puts an interpreter in this word. Where it does not,
    the caller answers with _unjudged_interpreter and nothing is run.
    """
    source, expected = _interpreter_question()
    try:
        # Its OWN session, so a timeout can reap everything it started. subprocess kills the
        # process it created and leaves that process's own children running, so a wrapper that
        # forks left a descendant of this diagnosis behind on every timeout.
        started = subprocess.Popen([str(resolved), "-c", source],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   start_new_session=True)
    except OSError as error:
        return _cell(firing.COULD_NOT_BE_RUN, label + " could not be run: " + str(error),
                     path=str(resolved),
                     # Attempted, not run: the host refused to create the process, so no
                     # program of this host's executed and the receipt must not imply one did.
                     attempted=_said_invocation(resolved),
                     errno=errno.errorcode.get(error.errno, error.errno))
    # Taken NOW, while the leader is certainly alive and is certainly the group leader.
    try:
        session = os.getpgid(started.pid)
    except (OSError, ValueError):
        session = started.pid
    try:
        spoke, _complained = started.communicate(timeout=INTERPRETER_PROBE_SECONDS)
    except subprocess.TimeoutExpired:
        _end_the_session(started, session)
        return _cell(NOT_READ, label + " was RUN as " + _said_invocation(resolved)
                     + " and did not answer within " + str(INTERPRETER_PROBE_SECONDS)
                     + "s, so whether it runs Python was not established",
                     path=str(resolved), ran=_said_invocation(resolved))
    except OSError as error:
        # It STARTED, and reading its answer is what failed. Saying it could not be run would
        # be false twice: a program of this host's did execute, and COULD_NOT_BE_RUN is one of
        # firing.CANNOT_START, so the word would settle a startability question this reading
        # never reached. It reports what it ran and claims nothing further from it.
        _end_the_session(started, session)
        return _cell(NOT_READ, label + " was RUN as " + _said_invocation(resolved)
                     + " and its answer could not be read: " + str(error) + ", so whether it"
                     " runs Python was not established",
                     path=str(resolved), ran=_said_invocation(resolved),
                     errno=errno.errorcode.get(error.errno, error.errno))
    answered = _text(spoke).strip().split(" ")
    version = _python_said(answered[1]) if len(answered) == 2 else None
    # The ANSWER is read before the exit status, because only a program that ran the source can
    # produce this nonce and a wrapper is free to replace the status afterwards -- a launcher
    # ending in 'exit 1' would otherwise throw away the one piece of positive evidence there is.
    if answered[0] == expected and version is not None:
        if version < SUPPORTED_PYTHON:
            return _cell(firing.BELOW_SUPPORTED_PYTHON, label + " was RUN as "
                         + _said_invocation(resolved) + " and answered Python "
                         + ".".join(str(part) for part in version) + ", below the supported "
                         + ".".join(str(part) for part in SUPPORTED_PYTHON) + ", so the"
                         " adapter fails on every Stop before evaluating or journalling"
                         " anything", path=str(resolved), ran=_said_invocation(resolved))
        return _cell(reading.PRESENT, label + " was RUN as " + _said_invocation(resolved)
                     + " and answered as a supported Python interpreter. That is what this"
                     " establishes: the registered first word answers at the spelling the host"
                     " resolves. It does not establish that the registration starts -- the"
                     " command was never run as written and a wrapper's own target is not"
                     " followed -- and whether it answers on the next invocation is that"
                     " invocation's own fact",
                     path=str(resolved), ran=_said_invocation(resolved))
    if started.returncode != 0:
        # It REFUSED the question rather than answering it wrongly, and those are different
        # facts. A wrapper -- env, a shell, a launcher script -- rejects an option meant for an
        # interpreter and exits non-zero while starting the adapter perfectly well through the
        # words that follow it, which this command deliberately does not follow. Establishing
        # "not an interpreter" from that condemned a working registration.
        return _cell(NOT_READ, label + " was RUN as " + _said_invocation(resolved)
                     + " and did not accept the question, which is also what a wrapper around"
                     " an interpreter does, so whether it runs Python was not established"
                     " here", path=str(resolved), ran=_said_invocation(resolved))
    return _cell(NOT_READ, label + " was RUN as " + _said_invocation(resolved)
                 + " and did not answer as a Python interpreter. Two hosts look exactly like"
                 " this -- a program that is not an interpreter at all, and a wrapper that"
                 " ignores an option meant for the interpreter behind it and starts the adapter"
                 " perfectly well -- and nothing here separates them, so whether this can start"
                 " the adapter was not established",
                 path=str(resolved), ran=_said_invocation(resolved))


def _recorded_program_cell(named, label, asks=False):
    """A program these settings name, probed the way a registered one is.

    A plugin-owned hook has no entry in the hook file, so the cell above receives nothing and
    answers that no registration named a program. That is true about the hook file and useless
    about this host: the launcher starts the two programs recorded here, and if either is gone
    every Stop is released without a word. Same probe, different source.

    'asks' is for a program that can be RUN to answer for itself. The entry point is a script
    the interpreter runs, not a program this command can ask anything of, so presence is the
    whole of what is establishable about it here; the interpreter is asked. Said as a parameter
    rather than read off the label, because a label is prose and this is a decision.
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
    if asks and probe["value"] == reading.PRESENT:
        return _answers_as_an_interpreter(named, label)
    return probe


def _journal_cell(config, held=None):
    """What this hook recorded about its own invocations.

    'held' is a list a caller passes when it will use the reported journalIdentity as a KEY.
    The descriptor this listing was read through is appended to it and stays open, because an
    identity stops being a key the moment its object can be recycled: a journal directory
    deleted after it was listed hands its (device, inode) to whatever is created next, and a
    later registration's journal reaching that pair would be given this one's snapshot. The
    caller closes what it collects.
    """
    root = (config or {}).get("journalRoot")
    policy = (config or {}).get("journalPolicy") or EVERY_INVOCATION
    if not root:
        return _cell(NO_JOURNAL, "no journal is configured, so this hook records nothing about"
                                 " its own invocations", journalPolicy=policy)
    directory = Path(root).expanduser()
    try:
        # Opened ONCE, and every reading below is taken through this descriptor. Two separate
        # path lookups around a listing cannot promise they described the directory it read:
        # a link can point away and back between them, and the count is then filed under the
        # identity of a directory it never came from. A descriptor cannot be retargeted.
        handle = os.open(str(directory), os.O_RDONLY | _DIRECTORY)
    except FileNotFoundError:
        if os.path.islink(str(directory)):
            # A link whose target is gone is NOT an established absence. Nothing could be read
            # through it, and the hook cannot create its dated directory through it either, so
            # reporting "the directory does not exist" settled a count of zero for a host whose
            # journal path is broken -- an unreadable path answered as an empty journal, which
            # is the substitution this whole answer set exists to remove.
            return _cell(reading.ACCESS_ERROR,
                         "the journal path is a link whose target is not there, so nothing"
                         " could be read through it and the hook cannot create its own"
                         " directory through it: no count was established",
                         journalRoot=str(directory), journalPolicy=policy)
        return _cell(reading.ABSENT, "the journal directory does not exist, so this hook has"
                                     " recorded no invocation into it",
                     journalRoot=str(directory), journalPolicy=policy)
    except (OSError, ValueError) as error:
        # ValueError as well: complaints() accepts any absolute string, and one carrying a
        # NUL cannot name a path at all, so scandir raises it. A settings document that
        # reads back fine must still produce a journal reading rather than a traceback.
        return _cell(reading.ACCESS_ERROR, "the journal could not be opened: " + str(error),
                     # No identity. An open that yielded no descriptor established nothing
                     # about WHICH object refused it, and every way of naming it afterwards is
                     # another lookup of the same spelling: a link retargeted after the refusal
                     # publishes it under a readable directory's identity, and the next
                     # registration naming that directory inherits a failure belonging to
                     # something else. Opening the object merely to NAME it was tried and has
                     # the same window, which the case below caught.
                     #
                     # The cost is that two spellings reaching one unreadable object stay two
                     # journal roots, so recorded_on_another_path is NOT_RULED_OUT for them.
                     # That is an unestablished answer rather than a false one, and this is the
                     # direction this reading fails in on purpose. Closing it would need the
                     # listing re-opened through the descriptor that named the object -- on
                     # Linux, /proc/self/fd -- which is a larger change than this contract.
                     journalRoot=str(directory), journalPolicy=policy)
    try:
        # The identity of what was actually opened, reported beside the count so a caller can
        # tell two spellings apart by what they REACHED rather than by how they were written.
        taken = os.fstat(handle)
        identity = (taken.st_dev, taken.st_ino)
        try:
            days = sorted(entry.name for entry in os.scandir(handle) if entry.is_dir())
        except (OSError, ValueError) as error:
            return _cell(reading.ACCESS_ERROR, "the journal could not be listed: " + str(error),
                         journalRoot=str(directory), journalPolicy=policy,
                         journalIdentity=identity)
        days = [day for day in days if JOURNAL_DAY.match(day)]
        counted = 0
        for day in days:
            try:
                # Opened RELATIVE to the journal's own descriptor, so a day is read under the
                # directory that was listed rather than under whatever the spelling names now.
                inner = os.open(day, os.O_RDONLY | _DIRECTORY, dir_fd=handle)
            except OSError:
                return _cell(reading.ACCESS_ERROR, "a journal day could not be listed",
                             journalRoot=str(directory), journalPolicy=policy, days=days,
                             journalIdentity=identity)
            try:
                counted += sum(1 for entry in os.scandir(inner)
                               if entry.is_file() and JOURNAL_NAME.match(entry.name))
            except OSError:
                return _cell(reading.ACCESS_ERROR, "a journal day could not be listed",
                             journalRoot=str(directory), journalPolicy=policy, days=days,
                             journalIdentity=identity)
            finally:
                os.close(inner)
        return _cell(str(counted), "invocations this hook recorded for itself",
                     journalRoot=str(directory), journalPolicy=policy, days=days,
                     journalIdentity=identity)
    finally:
        if held is None:
            os.close(handle)
        else:
            held.append(handle)


def journals_named(registrations, already_read=None, pinned=None):
    """Each REGISTRATION's own settings file, and what the journal under it holds.

    This exists because "no record" had several causes and the command answered none of them.
    Every registration in the hook file runs, so when they name different settings files they
    record into different journals; reading one of those and reporting an absence says nothing
    about the others, and reading none of them -- which is what happened -- makes a hook that
    fired into one journal indistinguishable from a hook that never fired.

    Keyed by the registration and not by the path, because the journal and the program that
    writes into it are one registration's pair. Reduced to a bare set of paths, a journal
    could not be attributed to the command that fills it: an unstartable registration's empty
    journal then read as evidence that its startable neighbour had recorded somewhere else.

    It writes nothing and opens only files a registration already named.

    'already_read' carries readings this call's caller has already taken, keyed by path. A
    settings file read twice in one status call is a payload that can contradict itself: the
    configuration cell reported PRESENT from the first read while namedSettings reported ABSENT
    from the second, about one file, in one answer. A reading is taken once and used wherever
    it is needed.

    Each entry answers its own records question with one of three values, never with a
    stand-in. Settings that are absent, or that this hook's own reader rejects, keep no journal
    AT ALL: run() reads them before it looks at the payload, so every invocation releases
    without writing anywhere. That is an established "nothing is kept here". A settings file
    this process could not reach is different and stays unestablished, because a permission
    failure here says nothing about what the hook can open inside a session.
    """
    found = []
    # One snapshot per JOURNAL, not per registration. Two registrations can name different
    # settings files that configure the same journalRoot, and listing that one directory twice
    # let a Stop landing between the reads report two different counts for one directory --
    # enough to establish "recorded on another path" when there is only one path.
    scanned = {}
    # The kernel's identity for each snapshot, captured when it was taken, so a later
    # registration can ask whether its own spelling reaches that same directory. Kept beside
    # the snapshots rather than derived from their keys, because a key is lexical and this
    # question is not -- and stored as the ANSWER rather than as the spelling, so a link
    # retargeted afterwards cannot make a stale snapshot look current.
    aliases = {}
    # One reading per settings FILE, not per registration. Two registrations can name ONE file
    # through two absolute spellings -- a symlink alias is not a lexical difference, and
    # _settled deliberately does not resolve one, because resolving would make a key depend on
    # what a link points at. Read twice, an atomic rewrite landing between the reads reported
    # the old journalRoot in one entry and the new one in another, about one file, in one
    # answer; _recorded_on_another_path then read that as two journals disagreeing on a host
    # that has one. It is the same shape as the journal aliasing directly below, one level up.
    #
    # Keyed by the identity the READING carries, which read_json took from the descriptor the
    # bytes came through. A path lookup may ASK this cache, because it reports what a spelling
    # reached at the moment it was asked; what a reading is PUBLISHED under may not come from
    # a lookup, or a link retargeted between the lookup and the open files those bytes under an
    # identity they never came from.
    # An identity is only a key while this call HOLDS the object it names. An inode is recycled
    # the moment its last name and its last descriptor are gone, so a file deleted after it was
    # read can hand its (device, inode) to an unrelated file created afterwards -- and a cache
    # keyed on that pair would then serve one file's reading for another's. Holding a descriptor
    # open removes the interval: the kernel cannot reuse an inode something still has open.
    read_by = {}
    held = []
    try:
        # The caller's own reading goes through the same gate rather than straight in. The
        # descriptor it was read through was closed before this call began, so its identity is
        # exactly this recyclable too, and seeding it unheld left one entry skipping the rule
        # every other entry follows.
        for spelling, taken in (already_read or {}).items():
            bound = (pinned or {}).get(str(spelling))
            if bound is not None:
                # The CALLER pinned this one and holds it for longer than this call lives, so
                # its identity is a key already. Going through _keep instead asked for a holder
                # the caller never took -- it read through the pin rather than asking for one
                # -- so the seed was reachable by its exact spelling and by nothing else, and
                # an alias of that same file read it again. An in-place rewrite between the two
                # then gave one file two contents in one answer.
                read_by[bound[1]] = taken
            else:
                _keep(taken, read_by)
        _read_named(registrations, already_read, read_by, held, found, scanned, aliases,
                    pinned)
    finally:
        for descriptor in held:
            try:
                os.close(descriptor)
            except OSError:
                pass
    return found


def _stated_path_cell(named, what, read):
    """Read one launcher path a rejected document still states, or answer that it stated none.

    Only an absolute spelling, because a relative one is resolved by the host against something
    this command is not running under, and reading the path THIS process would resolve would
    report an unrelated file as the registration's own.

    Absolute LITERALLY, and not after expansion. The recorded launcher is handed to the
    interpreter as it is written -- unlike the settings path, which this adapter expands before
    it opens it -- so a tilde spelling is one the packaged launcher never resolves. Judging it
    on its expanded form while reading the literal one answered "startable" from a file the
    launcher would never reach.
    """
    if not isinstance(named, str) or not os.path.isabs(named):
        return NOT_READ
    found = read(named, what)
    return NOT_READ if found is None else found["value"]


def _pin_spellings(spellings, held):
    """Open and HOLD every spelling before any of them is read.

    A descriptor taken now fixes what each spelling named at ONE moment. Everything downstream
    -- the merge that decides whether two of them are one file, the reading this command
    settles on, and each registration's own reading -- then asks about the objects this set
    holds rather than about whatever the pathnames reach later. That is what makes their
    answers comparable: a replacement arriving at a pathname afterwards cannot move one
    consumer onto a different object from another.

    A spelling nobody could open or identify is not in the set, and its consumer falls back to
    reading the path. Its answer is then a moment later than the rest, which is the honest cost
    of a spelling the kernel would not hold still.
    """
    pinned = {}
    for spelling in spellings:
        spelling = str(spelling)
        if spelling in pinned:
            continue
        try:
            # NONBLOCK so a named pipe at a settings path cannot stall this command.
            descriptor = os.open(spelling, os.O_RDONLY | os.O_NONBLOCK)
        except (OSError, ValueError):
            continue
        mine = reading.descriptor_identity(descriptor)
        if mine is None:
            try:
                os.close(descriptor)
            except OSError:
                pass
            continue
        held.append(descriptor)
        pinned[spelling] = (descriptor, mine)
    return pinned


def _one_source_each(spellings, pinned=None):
    """Collapse the spellings the kernel says name one file, holding each while it is a key.

    Two registrations can name ONE settings file through two absolute spellings, and counting
    that as two sources reported the configuration unreadable as ambiguous while the cause
    partition beside it read the one file once and answered from it.

    Every identity compared here is taken from a descriptor this function HOLDS until it has
    finished comparing. An identity read and released is recyclable: a file deleted after it
    was identified hands its (device, inode) to the next file created, and a later, unrelated
    spelling would then be collapsed into it and never read at all -- one source reported where
    there are two, which is the opposite error and the worse one. Holding removes the interval.

    A spelling nobody could open or identify keeps its own place rather than being merged on a
    guess, because describing two files as one is what this check exists to prevent.

    Returns the spellings that keep a place, and the identity each identified spelling was
    judged under, so a caller that goes on to READ one can tell whether it read the object the
    judgement was made about.
    """
    seen, kept, held = {}, [], []
    judged = {}
    borrowed = pinned or {}
    try:
        for spelling in spellings:
            bound = borrowed.get(str(spelling))
            if bound is not None:
                # Already held by the caller, for longer than this call lives, so it is judged
                # against the same objects everything else here will read.
                if bound[1] not in seen:
                    seen[bound[1]] = spelling
                    kept.append(spelling)
                judged[spelling] = bound[1]
                continue
            try:
                # NONBLOCK so a named pipe at a settings path cannot stall this command.
                descriptor = os.open(spelling, os.O_RDONLY | os.O_NONBLOCK)
            except (OSError, ValueError):
                # ValueError is a spelling the kernel is never asked about at all -- an
                # embedded NUL, which a registration can carry. It is a path this command
                # cannot identify, which is a reading, and letting it out of here ended the
                # whole status payload in a traceback over one malformed registration.
                kept.append(spelling)
                continue
            mine = reading.descriptor_identity(descriptor)
            if mine is None or mine in seen:
                try:
                    os.close(descriptor)
                except OSError:
                    pass
                if mine is None:
                    kept.append(spelling)
                else:
                    judged[spelling] = mine
                continue
            seen[mine] = spelling
            judged[spelling] = mine
            held.append(descriptor)
            kept.append(spelling)
    finally:
        for descriptor in held:
            try:
                os.close(descriptor)
            except OSError:
                pass
    return kept, judged


def _journal_answer(cell):
    """What one journal cell says about records kept under it, as the rules read it.

    One function because there are two hosts and they must not drift: a registration in the
    hook file names its own settings, and a plugin-owned host names none there while this
    command still settles on one. The second used to carry no journal answer at all, so the
    policy causes had nothing to read on exactly the host whose repair they name.
    """
    value = cell["value"]
    policy = cell.get("journalPolicy")
    answer = {"journalRoot": cell.get("journalRoot"), "journalPolicy": policy,
              "faultsOnly": policy == FAULTS_ONLY,
              "records": None, "recordsAnswer": firing.UNESTABLISHED}
    # The POLICY is what decides whether a record is ever written, and journal() returns
    # without writing on no_journal whatever root is configured. Keying this on the cell's
    # NO_JOURNAL value alone read a host that keeps no journal by configuration as one whose
    # journal happens to be empty, which is the substitution this whole answer set exists to
    # remove.
    if value == NO_JOURNAL or policy == NO_JOURNAL:
        answer["recordsAnswer"] = firing.NO_RECORDS_KEPT
    elif value == reading.ABSENT:
        # The journal directory is established absent, so it holds nothing. That is a count
        # this command read, not one nobody could take.
        answer["records"], answer["recordsAnswer"] = 0, firing.COUNTED
    elif str(value).isdigit():
        answer["records"], answer["recordsAnswer"] = int(value), firing.COUNTED
    return answer


def _keep(taken, read_by, held=None):
    """Cache this reading only while a descriptor holds the object it was read THROUGH.

    The identity read_json reports is true of the bytes it returned. It stops being a usable
    KEY the moment that inode can be recycled: a settings file deleted after it was read hands
    its (device, inode) to whatever is created next, and a later spelling stat-ing to that pair
    would be served the deleted file's reading.

    Reopening the path to pin it is NOT enough, and that is the whole reason the reading
    carries its own descriptor. The reopen is a second lookup of the same spelling: replace the
    file between the read and the reopen with one that inherits the inode, and the identities
    compare equal while this pins the NEW object and caches the OLD one's bytes -- a wrong
    reading dressed as a verified one. The descriptor the bytes came through cannot be
    retargeted, so there is no interval left.

    'held' is the list of descriptors this call will close. A reading handed in by the caller
    is held by the CALLER for longer than this call lives, so it is cached without being
    adopted.
    """
    identity = getattr(taken[3], "identity", None)
    holder = getattr(taken[3], "holder", None)
    if identity is None or holder is None:
        return
    if held is not None:
        held.append(holder)
    read_by[identity] = taken


def _read_named(registrations, already_read, read_by, held, found, scanned, aliases,
                pinned=None):
    # Every spelling this call will read, opened and HELD before any of them is read.
    # Discovering aliases one spelling at a time left a window: an atomic rewrite landing
    # between the first snapshot and the next spelling's lookup made two registrations that now
    # name one file receive two different journalRoots, which recorded_on_another_path reads as
    # two sources disagreeing. Pinned together they are compared as they were at one moment,
    # and each reading is taken THROUGH the descriptor that pinned it, so no reading can come
    # from an object the comparison was not about.
    #
    # The caller may supply the set. status() does, because its OWN reading has to come from
    # the same objects: pinning here and reading there left a window between them, and the
    # carried reading then held one object while these pins held another.
    if pinned is None:
        pinned = _pin_spellings(
            [_settled(one["settings"]) for one in registrations if one.get("settings")], held)
    for registration in registrations:
        if not registration.get("settings"):
            # A relative spelling or no settings at all. There is no file here to open, and
            # record_path_unidentified is the cause that owns that state.
            continue
        path = _settled(registration["settings"])
        bound = pinned.get(str(path))
        taken = (already_read or {}).get(str(path))
        if taken is None:
            mine = bound[1] if bound is not None else reading.path_identity(path)
            taken = read_by.get(mine) if mine is not None else None
        if taken is None:
            if bound is not None:
                # The pin is the hold, so nothing further has to be kept open for it.
                taken = read_configuration(path, descriptor=bound[0])
                read_by[bound[1]] = taken
            else:
                taken = read_configuration(path, hold=True)
                _keep(taken, read_by, held)
        config, refused, detail, read_back = taken
        entry = {"registration": registration.get("registration"),
                 "startable": registration.get("startable"),
                 "settings": str(path), "settingsState": read_back.state,
                 "usable": config is not None, "refusedAs": refused, "detail": detail,
                 "journalRoot": None, "journalPolicy": None, "faultsOnly": False,
                 "records": None, "recordsAnswer": firing.UNESTABLISHED, "journal": None}
        if config is None:
            entry["recordsAnswer"] = (firing.UNESTABLISHED
                                      if read_back.state == reading.ACCESS_ERROR
                                      else firing.NO_RECORDS_KEPT)
            found.append(entry)
            continue
        # A missing root is not a path and must not be normalised into one: resource_key("")
        # answers "/", which is a real directory a configuration may legitimately name, and the
        # two would then share a snapshot. NO_ROOT is a sentinel no path can equal.
        configured = config.get("journalRoot")
        # A journal root is opened as a DIRECTORY and _journal_cell puts it through
        # Path, which drops a trailing separator, so /tmp/journal and /tmp/journal/
        # reach one scandir. The trailing separator resource_key keeps is a real
        # distinction for an executable and not for this, so it is dropped here rather
        # than weakened there.
        root = (resource_key(str(Path(configured))) if configured else NO_ROOT)
        # resource_key is lexical on purpose -- it refuses to resolve, because resolving would
        # make a cache key depend on what a link points at. That leaves one case it cannot see:
        # two registrations naming ONE directory, one through a real path and one through a
        # symlink alias. Listed twice, that directory produced two readings, and a Stop landing
        # between them showed one alias empty beside the other holding records -- which this
        # answer set reads as two journals disagreeing, on a host that has one journal.
        #
        # So identity is asked of the kernel, and only where the kernel can answer. Where it
        # cannot, the spellings keep their own keys rather than being merged on a guess: a
        # second reading of one directory is the cost, and a shared reading of two different
        # ones is what that refuses to cost.
        # This spelling's identity, to look up a snapshot already taken from that directory.
        # A lookup may ask a path: it reports what the spelling reached at the moment it was
        # asked, which is all any reading of a live filesystem claims. What may NOT come from
        # a path lookup is the identity a snapshot is PUBLISHED under, and that one is taken
        # from the descriptor the listing itself was read through.
        mine = reading.path_identity(configured) if configured else None
        if mine is not None and root not in scanned:
            alias = next((key for key, taken in aliases.items() if taken == mine), None)
            if alias is not None:
                root = alias
        if root in scanned:
            # The policy is this registration's own; the listing is the directory's, and the
            # directory is the same one.
            cell = dict(scanned[root], journalPolicy=config.get("journalPolicy")
                        or EVERY_INVOCATION)
        else:
            cell = _journal_cell(config, held=held)
            scanned[root] = cell
            # Published under the identity the READING reports, which it took from the
            # descriptor it listed. Bracketing the listing with two path lookups was not
            # enough: a link pointing away and back again agrees with itself across the
            # brackets while the listing in between came from somewhere else, and the count
            # was then filed under an identity it never came from. A descriptor cannot be
            # retargeted, so there is no interval left to race.
            taken = cell.get("journalIdentity")
            if taken is not None:
                aliases[root] = taken
        entry["journal"] = cell
        entry.update(_journal_answer(cell))
        found.append(entry)
    return found


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
    # Two spellings the kernel says are ONE file are one source, however they are spelled.
    # _settled removes lexical differences and deliberately does not resolve a symlink, so a
    # second registration naming this same file through an alias counted as a second source:
    # the configuration went unread as ambiguous and reported not_read, while the cause
    # partition -- which does ask the kernel -- read that one file once and answered
    # records_found from it. One payload, two answers about one file.
    #
    # Asked of the kernel here too, and only where the kernel can answer. Where it cannot, the
    # spellings keep their own places rather than being merged on a guess: describing two files
    # as one is the error this check exists to prevent, and a second entry is the cheaper cost.
    absolute = sorted({str(_settled(named)) for named in carried if named not in relative})
    # Pinned HERE, before the merge and before this command reads anything, and held until the
    # last consumer is done. The merge, the reading settled on below, and every registration's
    # own reading in journals_named then ask about one set of objects. Pinning inside that last
    # call left a window in front of it: a replacement arriving after this command had read and
    # revalidated its own snapshot, but before those pins were taken, left the carried reading
    # holding the old object while the pins held the new one -- and two registrations naming
    # ONE file received the old journalRoot and the new one, though there was no instant at
    # which they named different files. recorded_on_another_path establishes off exactly that
    # disagreement, so the payload named another path that was never another path.
    #
    # Detecting the disagreement instead was the other shape offered and it cannot produce a
    # coherent answer: the configuration cell is already built from the carried reading by the
    # time the pins exist, so a later consumer finding them different would have to report two
    # answers about one path, which is the contradiction the carried reading exists to prevent.
    # Removing the interval is the fix; there is then nothing to detect.
    held = []
    pinned = _pin_spellings(absolute + [configuration_path(home, environ)], held)
    named_once, judged = _one_source_each(absolute, pinned)
    collapsed = len(named_once) < len(absolute)
    distinct = sorted(set(named_once) | set(relative))
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
        # Held, because this reading's identity is about to be a cache key in journals_named:
        # one file read once has to answer both the configuration cell and the entry any
        # registration naming that same file gets. An identity released before it is used as a
        # key is recyclable, and a file deleted in between would hand it to whatever is created
        # next. Released immediately after that call, which is the only thing that uses it.
        settled_pin = pinned.get(str(path))
        config, failed, detail, found = (
            read_configuration(path, descriptor=settled_pin[0]) if settled_pin is not None
            else read_configuration(path, hold=True))
        # The merge above was judged on descriptors this call no longer holds, and this read is
        # a fresh lookup of the spelling. Where the two spellings were collapsed into one
        # source and the object read is NOT the one that judgement was made about -- a symlink
        # retargeted in between -- the single source was never established, and reporting the
        # reading as it would describe one file while two registrations run. Said as the
        # unsettled reading it is rather than presented as settled.
        # EVERY spelling the merge judged, not only the one that was read. A discarded alias
        # retargeted before this read is the same broken judgement seen from the other side:
        # the two registrations were reported as one source on a finding that no longer holds,
        # and checking only the retained spelling caught half of it.
        moved = [spelling for spelling, was in judged.items()
                 if reading.path_identity(spelling) != was]
        read_elsewhere = (found is not None and found.identity is not None
                          and judged.get(str(path)) is not None
                          and found.identity != judged[str(path)])
        if collapsed and (moved or read_elsewhere):
            reading.release(found)
            config, failed, found = None, REGISTRATION_AMBIGUOUS, None
            detail = ("the registrations' settings spellings were read as one file and the"
                      " file at " + ", ".join(sorted(moved) or [str(path)]) + " changed"
                      " before it could be read, so no single source was established and none"
                      " was read")

    target = _cell(NOT_READ, "no registration for this adapter was found to check")
    interpreter = _cell(NOT_READ, "no registration for this adapter was found to check")
    adapter_probes = {}
    if ours:
        # No expansion here, unlike the settings path. The settings path is expanded by this
        # adapter before it opens it; the adapter's own path is handed to the interpreter
        # literally, and nothing expands a tilde on the way. So a ~ target is relative in
        # effect, and judging it with expanduser would report a file the host never runs.
        loose = [entry["target"] for entry in ours if not os.path.isabs(entry["target"])]
        firm = [entry for entry in ours if os.path.isabs(entry["target"])]
        # Probed once, and kept against the registration each probe belongs to. Probing the
        # same file again for the cause meant two readings of one path in one call: an adapter
        # created or removed between them produced a payload whose target cell said PRESENT
        # while the cause established adapter_cannot_run about the same registration.
        # Keyed by the TARGET PATH and mapped back to identities. Two registrations can run one
        # adapter with different settings arguments, and probing that one file twice let the
        # payload give them different startability -- a file created or removed between the two
        # probes establishing adapter_cannot_run for one of two registrations that execute the
        # same program.
        by_target = {}
        for entry in firm:
            key = resource_key(entry["target"])
            if key not in by_target:
                by_target[key] = _script_cell(entry["target"], "the adapter script")
        adapter_probes = {entry["identity"]: by_target[resource_key(entry["target"])]
                          for entry in firm}
        probes = [adapter_probes[entry["identity"]] for entry in firm]
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

    # Startability PER REGISTRATION, paired by the registration's own identity. The two cells
    # above report the worst probe they took, which answers "is anything broken" and was read
    # as "is everything broken": one missing target among several registrations then claimed
    # the hook could not have run while its neighbour was running all along. And the two halves
    # have to stay paired, because a registration starts only when its adapter AND its
    # interpreter are both there; flattening them let a present interpreter under a missing
    # adapter read as something that could start.
    interpreter_probes = {one["registration"]: one["probe"]["value"]
                          for one in (interpreter.get("probes") or [])}
    start_probes = [
        {"registration": entry["identity"],
         "adapter": (adapter_probes[entry["identity"]]["value"]
                     if entry["identity"] in adapter_probes else REGISTRATION_RELATIVE_TARGET),
         "interpreter": interpreter_probes.get(entry["identity"], NOT_READ)}
        for entry in (ours or [])
    ]
    # Every registration's OWN settings file and journal, read, and carried with whether that
    # registration can be started at all. Reading one file is the right answer when one
    # registration names one file; when several name several, every one of them runs, and this
    # command used to answer by reading none of them. A hook that had fired into one journal
    # and a hook that had never fired at all then produced identical cells, which is the
    # distinction the operator procedure had to write down as missing. None is elected, because
    # electing one would make the answer depend on which path happened to sort first.
    # Three answers, not two. A probe this command did not judge -- a workspace-dependent
    # spelling, or one it could not reach -- is neither "starts" nor "cannot start", and
    # recording it as the latter dropped that registration's journal from every question while
    # a blocked neighbour supplied a settled explanation for the whole host.
    startable = {probe["registration"]:
                 _startable_from({probe["adapter"], probe["interpreter"]})
                 for probe in start_probes}
    named_journals = journals_named(
        [
        {"registration": entry["identity"],
         "startable": startable.get(entry["identity"], False),
         "settings": (entry["settings"]
                      if entry.get("settings") and entry["settings"] not in relative else None)}
            for entry in (ours or [])
        ],
        # The reading the configuration cell above is built from, so one file read once answers
        # both. 'found' is None in the branches where no file was read at all.
        already_read=({str(path): (config, failed, detail, found)} if found is not None else {}),
        # The same objects this command read its own settings through, so the carried reading
        # and every registration's reading provably came from one file rather than from one
        # pathname at two moments.
        pinned=pinned)
    # Every consumer of the pinned set is done, so the objects no longer have to be held.
    reading.release(found)
    for descriptor in held:
        try:
            os.close(descriptor)
        except OSError:
            pass
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
        journal_cell = _cell(NOT_READ, "no usable configuration names a journal to read")
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
        adapter = (_script_cell(config["adapterEntryPoint"], "the recorded adapter entry point")
                   if config.get("adapterEntryPoint")
                   else _cell(NOT_READ, "these settings record no adapter entry point, which is"
                                        " the " + OWNER_USER + " owner's shape: its registered"
                                        " command line carries the adapter instead"))
        # The launcher starts two programs and either one can go missing on its own. A virtual
        # environment removed after installation leaves the script in place and the interpreter
        # gone, and then nothing runs at all: the same outage, a different repair.
        adapter_interpreter = (
            _recorded_program_cell(config.get("adapterInterpreter"),
                                   "the recorded adapter interpreter", asks=True)
            or _cell(NOT_READ, "these settings record no adapter interpreter, which is the "
                     + OWNER_USER + " owner's shape: its registered command line carries the"
                     " interpreter instead"))
        offers = (_offers_guard(executable, config.get("timeoutSeconds")
                                or DEFAULT_TIMEOUT_SECONDS)
                  if relay["value"] == reading.PRESENT
                  else _cell(NOT_READ, "the configured runtime could not be asked: "
                                       + relay["evidence"]))
        marker = presence(config["markerRoot"], "the configured marker root", directory=True)
        # Taken from the registration that named this very file rather than read a
        # second time. Two reads of one journal in one status call opened a window: a
        # Stop landing between them produced a payload whose count said ABSENT while
        # the cause beside it said records_found, so the explanation contradicted the
        # cell it was explaining. One reading, two outputs.
        journal_cell = next((entry["journal"] for entry in named_journals
                             if entry["settings"] == str(path) and entry.get("journal")),
                            None)
        if journal_cell is None:
            journal_cell = _journal_cell(config)

    # Attached to the settings cell rather than replacing it: the cell above still answers
    # about the one file this command settled on, and this says what every registration named.
    settings["namedSettings"] = named_journals
    # Why there is no record, decided over the cells above and over no reading of its own.
    # 'ours' is None only when the hook file itself could not be read, which is why whether a
    # registration exists is passed as the readability of that file and not as a count of zero.
    # What the journal this command settled on holds where NO registration named one. It is the
    # same reading published as firingJournal below, passed rather than taken a second time, so
    # the cause and the count in one payload cannot disagree. Without it the absence cause was
    # decided from the hook file alone and claimed no record of an invocation could exist,
    # beside a count in the same answer saying one does.
    unregistered_records = (int(journal_cell["value"])
                            if not named_journals and str(journal_cell.get("value")).isdigit()
                            else None)
    # Whether the hook file is where this host's registration would BE. The plugin owner
    # registers through its package manifest, which this command does not read, so on a
    # correctly plugin-owned host an empty hook file is exactly what a working installation
    # looks like and establishes nothing at all about registration.
    #
    # Three states, not two. Where NO document was read -- undecodable bytes, a file this
    # process cannot open -- nothing establishes who owns the registration, so the hook file
    # establishes nothing either and the question stays open. Where a document WAS read, the
    # owner comes from the document rather than from the validated configuration: a file that
    # reads back fine and fails some other check still records who owns the registration, and
    # taking the default there established an absence for a plugin-owned host and suppressed
    # settings_unusable, the cause that would have named the actual repair. An ABSENT document
    # is neither, and it establishes nothing here: owner_of answers an absent KEY as the user
    # owner, but a document that is not there at all is a host this reading cannot identify.
    if found is None or not found.usable or found.state == reading.ABSENT:
        # Nothing read, or nothing there, and neither establishes an owner. An ABSENT document
        # used to answer "the hook file is where the registration lives", borrowing owner_of's
        # rule about an absent KEY in a document that exists. That rule does not reach a
        # document that does not: a plugin-owned installation whose settings file was deleted
        # while its package remains installed looks exactly like a host where nothing was ever
        # installed, and reading the absence as the user owner established not_registered for
        # it -- suppressing the plugin-side settings and launcher diagnoses and pointing
        # recovery at the wrong registration. One observation, two explanations, and no reading
        # here separates them, so the answer carries both rather than choosing.
        registration_read_here = False
    elif config is not None:
        registration_read_here = owner_of(config) != OWNER_PLUGIN
    elif not isinstance(found.value, dict):
        # Valid JSON that is not an object records no owner at all, and that is not the same
        # as a document written before the key existed.
        registration_read_here = False
    else:
        stated = found.value.get("owner")
        # An OMITTED owner is the legacy user document owner_of is written for. An owner this
        # reader does not know is the opposite: somebody wrote something here, and reading it
        # as the default would establish an absence from a hook file that may not be where
        # this host's registration lives.
        registration_read_here = (True if stated is None
                                  else (stated in OWNERS and stated != OWNER_PLUGIN))
    # Hoisted, because two observations below read it: whether the settings causes may answer
    # from the settled reading, and whether the launcher those settings record is the probe.
    registration_elsewhere = (found is not None and found.usable
                              and isinstance(found.value, dict)
                              and found.value.get("owner") == OWNER_PLUGIN)
    # The launcher those settings record, as a probe of what is there NOW. Both halves are
    # already read for the cells this payload publishes, so this carries a present reading
    # rather than re-deriving one -- and an old journal record can never stand in for it.
    recorded_launcher = (found.value if (found is not None and found.usable
                                         and isinstance(found.value, dict)) else {})
    probed, probed_interpreter = adapter["value"], adapter_interpreter["value"]
    if registration_elsewhere and NOT_READ in (probed, probed_interpreter):
        # The document was READ; some OTHER field failed validation -- a mode this reader does
        # not know, say. The launcher paths it records are present readings whatever that
        # field says, and taking the validated cells' NOT_READ as the answer dropped the probe
        # entirely: a deleted entry point then hid behind an unrelated complaint, on the one
        # host whose repair this probe exists to name. Read here only because the branch above
        # had no reading to carry, and only for paths the document states absolutely.
        #
        # Each half on its own. Requiring BOTH to be usable was a conjunctive guard over two
        # independent readings: an entry point this host cannot start stayed unreported because
        # the interpreter beside it was omitted or relative, which is the shape of hiding a
        # repair behind an unrelated fact. The cause reads the halves separately too -- one
        # half that cannot start establishes it whatever the other says.
        if probed == NOT_READ:
            probed = _stated_path_cell(recorded_launcher.get("adapterEntryPoint"),
                                       "the recorded adapter entry point", _script_cell)
        if probed_interpreter == NOT_READ:
            probed_interpreter = _stated_path_cell(
                recorded_launcher.get("adapterInterpreter"),
                "the recorded adapter interpreter",
                lambda path, label: _recorded_program_cell(path, label, asks=True))
    launcher_probe = ([{"registration": "the launcher these settings record",
                        "adapter": probed,
                        "interpreter": probed_interpreter}]
                      if registration_elsewhere
                      and not (probed == NOT_READ and probed_interpreter == NOT_READ)
                      else [])
    absence = firing.decide({
        "registrationReadable": ours is not None,
        "adapterRegistrations": len(ours or []),
        "registrationReadHere": registration_read_here,
        # Whether the registration is POSITIVELY established to live somewhere this command
        # does not read. Narrower than the negation above on purpose: that one is also false
        # when nobody could read who owns the registration, and the settings causes may only
        # answer from the settled reading where the owner was actually read as the plugin's.
        "registrationElsewhere": registration_elsewhere,
        # The settings this command SETTLED on, described the way a registration's own entry is
        # so the rules read one shape. Built from the reading already taken above rather than
        # from a second one, so this and the configuration cell cannot disagree. A plugin-owned
        # host names no settings in the hook file, and without this the settings causes had
        # nothing to read on exactly the host whose repair they exist to name.
        "settledSettings": {"settings": str(path),
                            "settingsState": None if found is None else found.state,
                            "usable": config is not None,
                            "refusedAs": failed,
                            "detail": detail,
                            # The journal half of the same host, derived from the very cell
                            # published as firingJournal below rather than from a second
                            # reading, so the cause and the count in one payload cannot
                            # disagree. Without it the POLICY causes had nothing to read here:
                            # a plugin-owned host whose settings say no_journal or faults_only
                            # carries that answer in a file this command did read, and the
                            # payload reported only that the registration could not be settled
                            # while showing the policy one cell over.
                            **_journal_answer(journal_cell)},
        "relativeSettings": bool(relative),
        "silentRegistrations": silent,
        "namedJournals": named_journals,
        # A plugin-owned host registers nothing in the hook file, so start_probes is empty and
        # the startability question had no PRESENT reading to answer from. The launcher those
        # settings record is the one that has to start, and both of its halves are already read
        # for the cells beside this. Carried here, an entry point or interpreter deleted since
        # the last invocation is named as the repair, instead of an old journal record standing
        # in for a reading of what is there now.
        # APPENDED, not substituted. Plugin-owned settings can sit beside a hook-file
        # registration on a hand-edited host, and dropping the recorded launcher whenever the
        # hook file named anything let the other registration's records hide its failure.
        # Supplied only where those settings actually record a launcher: where they record
        # none, there is nothing present to read and inventing an unjudged probe would put
        # uncertainty on the table that no reading pointed at.
        "startProbes": start_probes + launcher_probe,
        "unregisteredRecords": unregistered_records,
    })

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
        "firingJournal": journal_cell,
        "firingRecordAbsence": _cell(
            absence["value"], absence["evidence"], candidates=absence["candidates"],
            ruledOut=absence["ruledOut"], notEvaluated=absence["notEvaluated"],
            note=absence["note"]),
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
