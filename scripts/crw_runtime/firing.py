"""Why there is no record of this hook having fired.

The observation that a journal holds nothing has several causes and they need different
repairs: the hook may not be registered at all, it may be registered against settings this
command cannot identify, its adapter may not be startable, journalling may be switched off, or
it may be registered and recording into a journal nobody looked at. Reported as one absence,
an operator either guesses at the cause or stops there, and the operator procedure closed that
gap honestly by writing that the tool does not distinguish them. This module is that
distinction.

It takes no readings of its own. Every rule decides over cells status() has already produced,
and CAUSE_RULES declares which observation answers each cause, so a cause cannot be decided on
a reading it never names.

Two things this deliberately does NOT do. It does not stop at the first cause it establishes:
whether the host can start the adapter is not downstream of the settings, and a run that
stopped early reported a deleted settings file while saying nothing about a deleted adapter
beside it. And it never resolves an ambiguity by choosing. Where two causes both stand, the
answer says so and carries them; where a reading could not decide, the answer is unreadable and
carries what is still standing.

The one thing no answer here can establish, stated because a detector that hides its blind spot
is worse than one that has none: a journal write that fails cannot record its own failure. So
NOTHING_RECORDED is named for what was observed rather than for what it suggests. "The journal
holds nothing" and "the hook never ran" are not the same sentence, and this module only ever
says the first.
"""

from . import reading

# Why there is no record. Every one of these is an answer; none of them is a default.
RECORDS_FOUND = "records_found"
NOT_REGISTERED = "not_registered"
RECORD_PATH_UNIDENTIFIED = "record_path_unidentified"
ADAPTER_CANNOT_RUN = "adapter_cannot_run"
SETTINGS_ABSENT = "settings_absent"
SETTINGS_UNUSABLE = "settings_unusable"
RECORDED_ON_ANOTHER_PATH = "recorded_on_another_path"
JOURNALLING_OFF = "journalling_off"
POLICY_RECORDS_ONLY_FAULTS = "policy_records_only_faults"
NOTHING_RECORDED = "nothing_recorded"
# Two answers about the answer itself, and they are not the same thing. SEVERAL_CAUSES is an
# absence that is over-determined: two causes are established and both repairs are needed.
# CAUSE_UNREADABLE is an absence whose cause a reading could not settle. Collapsing them would
# report "nobody could tell" for a host where the command told you two things.
SEVERAL_CAUSES = "several_causes"
CAUSE_UNREADABLE = "cause_unreadable"

CAUSES = (RECORDS_FOUND, NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, ADAPTER_CANNOT_RUN,
          SETTINGS_ABSENT, SETTINGS_UNUSABLE, RECORDED_ON_ANOTHER_PATH, JOURNALLING_OFF,
          POLICY_RECORDS_ONLY_FAULTS, NOTHING_RECORDED, SEVERAL_CAUSES, CAUSE_UNREADABLE)

# What a rule says about its own cause. Four, because "its reading says no" and "its reading
# could not say" are the pair this whole design exists to keep apart, and because a question
# that was never reachable is neither.
ESTABLISHED = "established"
RULED_OUT = "ruled_out"
NOT_RULED_OUT = "not_ruled_out"
NOT_EVALUATED = "not_evaluated"
STANDINGS = (ESTABLISHED, RULED_OUT, NOT_RULED_OUT, NOT_EVALUATED)

# The standings that leave a cause on the table. Asked as a set rather than tested against one
# member, because an established cause and one nobody could rule out are both still candidates
# and only one of them is an answer.
STANDING = (ESTABLISHED, NOT_RULED_OUT)

# What one named settings path says about records kept under it.
COUNTED = "counted"
NO_RECORDS_KEPT = "no_records_kept"
UNESTABLISHED = "unestablished"
RECORD_ANSWERS = (COUNTED, NO_RECORDS_KEPT, UNESTABLISHED)

# A target or an interpreter in one of these states cannot be started. PRESENT is the only
# value that says it can; everything else is a spelling this command did not judge, and those
# are not ruled out rather than established either way.
CANNOT_START = (reading.ABSENT, reading.UNREADABLE)


def _values(observed, *names):
    return [observed.get(name) for name in names]


def _not_registered(observed):
    if not observed.get("registrationReadable"):
        return NOT_RULED_OUT, ("the hook file could not be read, so whether this adapter is"
                               " registered for the event was not established")
    found = observed.get("adapterRegistrations") or 0
    if found:
        return RULED_OUT, (str(found) + " registration(s) in the hook file run this adapter")
    return ESTABLISHED, ("the hook file was read and registers this adapter for nothing, so no"
                         " invocation of it can have happened and no record of one can exist")


def _record_path_unidentified(observed):
    """Whether the file this hook records THROUGH can be named from here at all.

    This is a question about readability and never about how many registrations there are.
    Several registrations naming several ABSOLUTE settings files are all openable from here, so
    that host is answered by reading each of them rather than by refusing; it is the spelling
    that cannot be resolved from outside a session that leaves the path unidentified.
    """
    if observed.get("relativeSettings"):
        return ESTABLISHED, ("a registration spells its settings with a relative path, which"
                             " the hook resolves against each session's own workspace, so no"
                             " file reachable from here answers for it")
    if observed.get("silentRegistrations"):
        return ESTABLISHED, ("a registration names no settings, so the hook resolves its own at"
                             " every Stop; the path this command would resolve is not"
                             " established to be the one the host resolves")
    return RULED_OUT, "every registration names an absolute settings file"


def _adapter_cannot_run(observed):
    """Whether the host can start the program at all.

    Deliberately not downstream of the settings. The host resolves and runs the command before
    the adapter opens anything, so a missing interpreter means no invocation, no decision and
    no journal entry, on a host whose settings may be perfectly fine.
    """
    values = _values(observed, "targetValue", "interpreterValue")
    blocked = [value for value in values if value in CANNOT_START]
    if blocked:
        return ESTABLISHED, ("the registered command names an adapter or an interpreter the"
                             " host cannot start (" + ", ".join(blocked) + "), so it cannot"
                             " have run and cannot have recorded")
    if all(value == reading.PRESENT for value in values):
        return RULED_OUT, "the registered adapter and its interpreter are both there"
    return NOT_RULED_OUT, ("whether the host can start the registered command was not"
                           " established: " + ", ".join(str(value) for value in values))


def _settings_states(observed):
    return [entry.get("settingsState") for entry in (observed.get("namedJournals") or [])]


def _settings_absent(observed):
    states = _settings_states(observed)
    if not states:
        return NOT_RULED_OUT, "no settings file was named to be read"
    if all(state == reading.ABSENT for state in states):
        return ESTABLISHED, ("every settings file the registrations name is established absent,"
                             " so nothing tells this hook where to record and it keeps no"
                             " journal")
    if any(state == reading.ACCESS_ERROR for state in states):
        # A permission failure HERE says nothing about what the hook can open in a session.
        return NOT_RULED_OUT, ("a settings file could not be reached from here, which does not"
                               " establish that the hook cannot read it")
    return RULED_OUT, "a settings file the registrations name exists"


def _settings_unusable(observed):
    """Settings that were read and cannot be acted on. The bytes are the bytes, so unlike a
    permission failure this is established from here: the hook reads the same file, rejects it
    the same way, releases the turn and writes nothing anywhere."""
    entries = observed.get("namedJournals") or []
    if not entries:
        return NOT_RULED_OUT, "no settings file was named to be read"
    if any(entry.get("settingsState") == reading.ACCESS_ERROR for entry in entries):
        return NOT_RULED_OUT, "a settings file could not be reached from here"
    unusable = [entry["settings"] for entry in entries
                if entry.get("settingsState") != reading.ABSENT and not entry.get("usable")]
    if unusable and len(unusable) == len([entry for entry in entries
                                          if entry.get("settingsState") != reading.ABSENT]):
        return ESTABLISHED, ("every settings file that exists is one this hook's own reader"
                             " rejects (" + ", ".join(unusable) + "), so every invocation"
                             " releases without recording")
    return RULED_OUT, "a settings file the registrations name reads back usable"


def _record_answers(observed):
    entries = observed.get("namedJournals") or []
    holding = [entry for entry in entries
               if entry.get("recordsAnswer") == COUNTED and (entry.get("records") or 0) > 0]
    empty = [entry for entry in entries
             if entry.get("recordsAnswer") == COUNTED and not entry.get("records")]
    off = [entry for entry in entries if entry.get("recordsAnswer") == NO_RECORDS_KEPT]
    unread = [entry for entry in entries if entry.get("recordsAnswer") == UNESTABLISHED]
    return holding, empty, off, unread


def _named(entries):
    return ", ".join(str(entry.get("journalRoot") or entry.get("settings")) for entry in entries)


def _records_found(observed):
    holding, empty, _off, unread = _record_answers(observed)
    if holding and not empty and not unread:
        return ESTABLISHED, ("every journal these registrations name holds records this hook"
                             " wrote, so there is no absence to explain: " + _named(holding))
    if unread and not holding:
        return NOT_RULED_OUT, "a named journal could not be listed: " + _named(unread)
    return RULED_OUT, "a journal these registrations name holds no record"


def _recorded_on_another_path(observed):
    """The hook fired, and a reading of one named journal would have reported an absence.

    Symmetric over the set, with no distinguished member: the answer does not depend on which
    settings file sorts first, because there is no reference path. What it reports is that one
    of the journals these registrations name holds records while another was read and holds
    none, which is exactly the host on which looking at a single journal misleads.
    """
    holding, empty, _off, unread = _record_answers(observed)
    if holding and empty:
        return ESTABLISHED, ("this hook has recorded into " + _named(holding) + " while "
                             + _named(empty) + " holds nothing, so a reading of the latter"
                             " alone would report an absence for a hook that has fired")
    if holding and unread:
        return NOT_RULED_OUT, ("records were found under " + _named(holding) + " and another"
                               " named journal could not be listed")
    return RULED_OUT, "no two named journals disagree about holding records"


def _journalling_off(observed):
    _holding, empty, off, unread = _record_answers(observed)
    if off and not empty and not unread and not _holding:
        return ESTABLISHED, ("the settings these registrations name keep no journal, so this"
                             " hook records nothing about its own invocations by"
                             " configuration; the absence says nothing about firing")
    if off and unread:
        return NOT_RULED_OUT, "some named settings keep no journal and another could not be read"
    return RULED_OUT, "a journal is configured"


def _policy_records_only_faults(observed):
    """Never established, and that is the point.

    Under faults_only the guard records only an invocation that faulted, so an empty journal is
    what a hook that fires constantly and never faults looks like, and it is also what a hook
    that never fired looks like. One observation, two explanations, and no reading here
    separates them. Reporting either as established would be choosing.
    """
    _holding, empty, _off, _unread = _record_answers(observed)
    faults = [entry for entry in empty if entry.get("faultsOnly")]
    if faults:
        return NOT_RULED_OUT, ("these settings record only invocations that faulted ("
                               + _named(faults) + "), so an empty journal is equally what a"
                               " hook that fired and never faulted leaves behind")
    return RULED_OUT, "no named settings record only faults over an empty journal"


def _nothing_recorded(observed):
    """The journal was read and holds nothing this hook wrote.

    Named for the observation and not for its most likely explanation. A journal write that
    fails removes what it left and cannot record that it failed, so "it never ran" and "it ran
    and every record failed to be written" are one observation here. This value claims only the
    first half of that sentence, and the cell's note says the rest.
    """
    holding, empty, _off, unread = _record_answers(observed)
    if holding:
        return RULED_OUT, "a named journal holds records this hook wrote"
    if unread:
        return NOT_RULED_OUT, "a named journal could not be listed: " + _named(unread)
    if empty and all(entry.get("faultsOnly") for entry in empty):
        return NOT_RULED_OUT, ("every journal that was read keeps only faults, so an empty one"
                               " does not establish that nothing was recorded")
    if empty:
        return ESTABLISHED, ("every journal these registrations name was read and holds no"
                             " record this hook wrote: " + _named(empty))
    return RULED_OUT, "no journal was read for this question"


# Each cause, the observations that answer it, and the rule that decides it. A member carries
# its predicate as well as its provenance for the same reason the swap gate's cells do: a rule
# written at the site it is applied is a rule that drifts from the one that was declared.
CAUSE_RULES = {
    NOT_REGISTERED: (("registrationReadable", "adapterRegistrations"), _not_registered),
    RECORD_PATH_UNIDENTIFIED: (("relativeSettings", "silentRegistrations"),
                               _record_path_unidentified),
    ADAPTER_CANNOT_RUN: (("targetValue", "interpreterValue"), _adapter_cannot_run),
    SETTINGS_ABSENT: (("namedJournals",), _settings_absent),
    SETTINGS_UNUSABLE: (("namedJournals",), _settings_unusable),
    RECORDS_FOUND: (("namedJournals",), _records_found),
    RECORDED_ON_ANOTHER_PATH: (("namedJournals",), _recorded_on_another_path),
    JOURNALLING_OFF: (("namedJournals",), _journalling_off),
    POLICY_RECORDS_ONLY_FAULTS: (("namedJournals",), _policy_records_only_faults),
    NOTHING_RECORDED: (("namedJournals",), _nothing_recorded),
}

# What has to be RULED OUT before a cause's question means anything. Declared rather than
# implied by the order of a list, because one of these dependencies is NOT what the order
# suggests: whether the host can start the adapter is independent of the settings, and making
# it downstream of them reported a deleted settings file on a host whose adapter was also gone.
#
# A cause whose requirements are not met is never evaluated, and never a candidate. It is
# reported as not evaluated, so what the answer did not ask is visible rather than absent.
CAUSE_REQUIRES = {
    NOT_REGISTERED: (),
    RECORD_PATH_UNIDENTIFIED: (NOT_REGISTERED,),
    ADAPTER_CANNOT_RUN: (NOT_REGISTERED,),
    SETTINGS_ABSENT: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED),
    SETTINGS_UNUSABLE: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED),
    # Records EXISTING is evidence in its own right and is not downstream of whether the
    # adapter can be started today: a host whose interpreter moved after the hook ran still has
    # the records it wrote, and refusing to look at them would answer "unreadable" about an
    # absence that is not there.
    RECORDS_FOUND: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, SETTINGS_ABSENT, SETTINGS_UNUSABLE),
    RECORDED_ON_ANOTHER_PATH: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, SETTINGS_ABSENT,
                               SETTINGS_UNUSABLE),
    # The three that explain an EMPTY journal are downstream of the adapter, because a program
    # the host cannot start leaves an empty journal as a consequence rather than as a second
    # cause. Without this, a deleted adapter beside its untouched empty journal was reported as
    # two causes needing two repairs, when restoring the adapter is the whole repair.
    JOURNALLING_OFF: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, ADAPTER_CANNOT_RUN,
                      SETTINGS_ABSENT, SETTINGS_UNUSABLE),
    POLICY_RECORDS_ONLY_FAULTS: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, ADAPTER_CANNOT_RUN,
                                 SETTINGS_ABSENT, SETTINGS_UNUSABLE),
    NOTHING_RECORDED: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, ADAPTER_CANNOT_RUN,
                       SETTINGS_ABSENT, SETTINGS_UNUSABLE),
}

# The order causes are reported in. Their dependencies come from CAUSE_REQUIRES and not from
# this sequence, which exists so a reader meets them upstream first.
CAUSE_ORDER = (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, ADAPTER_CANNOT_RUN, SETTINGS_ABSENT,
               SETTINGS_UNUSABLE, RECORDS_FOUND, RECORDED_ON_ANOTHER_PATH, JOURNALLING_OFF,
               POLICY_RECORDS_ONLY_FAULTS, NOTHING_RECORDED)

NOTE = ("Registered, startable and observed to have recorded are separate claims. What no"
        " answer here establishes: a journal write that fails removes what it left and cannot"
        " record its own failure, so a journal holding nothing is not proof that the hook never"
        " ran. That is why the answer is named for the journal and not for the hook.")


def decide(observed):
    """Why there is no record, decided over the cells status() already produced.

    Every rule runs. The verdict is a single cause only when exactly one is established and
    nothing was left unsettled; two established causes are reported as two, because an absence
    that needs two repairs is not an absence nobody could explain.
    """
    standings, details = {}, {}
    for cause in CAUSE_ORDER:
        required = CAUSE_REQUIRES[cause]
        blocked = [name for name in required if standings.get(name) != RULED_OUT]
        if blocked:
            standings[cause] = NOT_EVALUATED
            details[cause] = ("not asked, because " + ", ".join(blocked) + " would have to be"
                              " ruled out first for this question to mean anything")
            continue
        standings[cause], details[cause] = CAUSE_RULES[cause][1](observed)

    def entries(*wanted):
        return [{"cause": cause, "standing": standings[cause], "detail": details[cause]}
                for cause in CAUSE_ORDER if standings[cause] in wanted]

    candidates = entries(*STANDING)
    established = [entry["cause"] for entry in candidates if entry["standing"] == ESTABLISHED]
    unsettled = [entry["cause"] for entry in candidates if entry["standing"] == NOT_RULED_OUT]

    if unsettled:
        value = CAUSE_UNREADABLE
        evidence = ("the cause was not settled: " + ", ".join(unsettled) + " could not be ruled"
                    " out" + (", beside established " + ", ".join(established)
                              if established else "") + ". Every candidate is carried rather"
                    " than one of them chosen")
    elif len(established) == 1:
        value = established[0]
        evidence = details[value]
    elif established:
        value = SEVERAL_CAUSES
        evidence = ("more than one cause is established and each needs its own repair: "
                    + "; ".join(cause + " (" + details[cause] + ")" for cause in established))
    else:
        value = CAUSE_UNREADABLE
        evidence = ("no rule answered this absence, which is reported as an unsettled cause"
                    " rather than as any particular one")
    return {"value": value, "evidence": evidence, "candidates": candidates,
            "ruledOut": entries(RULED_OUT), "notEvaluated": entries(NOT_EVALUATED),
            "note": NOTE}
