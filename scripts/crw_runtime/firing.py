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

    Counted per registration rather than worst-of. The target and interpreter cells report the
    worst probe they took, so on a host with two registrations one broken target used to answer
    for both -- and the whole empty-journal family was then skipped as though nothing could have
    run, while the working registration was running all along. ANY unstartable registration is a
    repair and is established as one; only NO startable registration at all makes an empty
    journal that program's absence, and the journal rules ask that question for themselves.
    """
    probes = list(observed.get("startProbes") or [])
    if not probes:
        return NOT_RULED_OUT, "no probe of a registered command was made"
    blocked = [probe for probe in probes if _halves(probe) & set(CANNOT_START)]
    unjudged = [probe for probe in probes
                if not (_halves(probe) & set(CANNOT_START)) and not _startable(probe)]
    if blocked:
        return ESTABLISHED, (
            str(len(blocked)) + " of " + str(len(probes)) + " registrations name an adapter or"
            " an interpreter the host cannot start ("
            + "; ".join(str(probe.get("registration")) + ": adapter "
                        + str(probe.get("adapter")) + ", interpreter "
                        + str(probe.get("interpreter")) for probe in blocked)
            + "), so those registrations cannot have run and cannot have recorded")
    if unjudged:
        return NOT_RULED_OUT, ("whether the host can start a registered command was not"
                               " established: "
                               + ", ".join(sorted({value for probe in unjudged
                                                   for value in _halves(probe)})))
    return RULED_OUT, "every registered adapter and interpreter is there"


def _halves(probe):
    """A registration's two halves. Both have to be there for the host to start it, so they are
    read as a pair: a present interpreter under a missing adapter starts nothing."""
    return {str(probe.get("adapter")), str(probe.get("interpreter"))}


def _startable(probe):
    return _halves(probe) == {reading.PRESENT}


def _settings_absent(observed):
    """ANY registration whose settings are gone, not every one of them.

    Every registration runs and reads its own settings, so a peer that is fine says nothing
    about a peer that is broken. Requiring all of them to be absent meant one good registration
    suppressed the other's repair entirely: the operator was told the journal was empty and
    never that a second registration releases every invocation until its settings come back.
    """
    entries = observed.get("namedJournals") or []
    if not entries:
        # Not "could not tell": no registration named a file for this question to be about.
        # record_path_unidentified owns that state, and answering NOT_RULED_OUT here would put
        # a candidate on the table that no reading ever pointed at.
        return NOT_EVALUATED, "no registration named a settings file this command could read"
    gone = [entry["settings"] for entry in entries
            if entry.get("settingsState") == reading.ABSENT]
    if gone:
        return ESTABLISHED, ("these settings files the registrations name are established"
                             " absent, so those registrations are told nothing about where to"
                             " record and keep no journal: " + ", ".join(gone))
    if any(entry.get("settingsState") == reading.ACCESS_ERROR for entry in entries):
        # A permission failure HERE says nothing about what the hook can open in a session.
        return NOT_RULED_OUT, ("a settings file could not be reached from here, which does not"
                               " establish that the hook cannot read it")
    return RULED_OUT, "every settings file the registrations name exists"


def _settings_unusable(observed):
    """Settings that were read and cannot be acted on. The bytes are the bytes, so unlike a
    permission failure this is established from here: the hook reads the same file, rejects it
    the same way, releases the turn and writes nothing anywhere.

    ANY such registration, for the same reason absence is: a usable file beside a rejected one
    is not evidence that the rejected one works.
    """
    entries = observed.get("namedJournals") or []
    if not entries:
        return NOT_EVALUATED, "no registration named a settings file this command could read"
    unusable = [entry["settings"] for entry in entries
                if entry.get("settingsState") not in (reading.ABSENT, reading.ACCESS_ERROR)
                and not entry.get("usable")]
    if unusable:
        return ESTABLISHED, ("these settings files are ones this hook's own reader rejects ("
                             + ", ".join(unusable) + "), so every invocation of those"
                             " registrations releases without recording")
    if any(entry.get("settingsState") == reading.ACCESS_ERROR for entry in entries):
        # No bytes were read, so nothing establishes that the hook's own reader rejects it.
        # Calling that unusable would recommend repairing a file this process merely could not
        # open, on a host where the session opens it perfectly well.
        return NOT_RULED_OUT, ("a settings file could not be reached from here, so whether its"
                               " contents are usable was never established")
    return RULED_OUT, "every settings file the registrations name reads back usable"


def _record_answers(observed):
    # Only registrations whose settings ARE usable AND that the host can start. A registration
    # with no settings, or with settings this reader rejects, has its own cause above; one the
    # host cannot start has adapter_cannot_run. Folding either in here let a broken peer answer
    # a question about a working one's journal -- and in the worst shape of it, an unstartable
    # registration's necessarily empty journal made its startable neighbour's records look like
    # a recording on another path.
    entries = [entry for entry in (observed.get("namedJournals") or [])
               if entry.get("usable") and entry.get("startable")]
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
    if holding and not empty and not _off and not unread:
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
    # Read over every registration whose settings are usable, startable or not. Unlike an
    # empty journal, a disabled policy is NOT explained by the adapter being unstartable:
    # repairing the adapter still produces no firing evidence until journalling is switched
    # back on, so filtering this cause by startability hid a repair the other one does not
    # cover.
    off = [entry for entry in (observed.get("namedJournals") or [])
           if entry.get("usable") and entry.get("recordsAnswer") == NO_RECORDS_KEPT]
    # ANY such registration, for the same reason a missing settings file is: a peer that keeps
    # a journal is not evidence that this one does. Requiring every registration to be off hid
    # a registration configured never to record behind a neighbour that records normally, and
    # that registration can never produce firing evidence at all.
    if off:
        return ESTABLISHED, ("these registrations keep no journal, so they record nothing about"
                             " their own invocations by configuration and their absence says"
                             " nothing about firing: " + _named(off))
    return RULED_OUT, "every registration that can start keeps a journal"


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
    ADAPTER_CANNOT_RUN: (("startProbes",), _adapter_cannot_run),
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
    # Nor do they require RECORD_PATH_UNIDENTIFIED. That cause is established when ANY
    # registration spells its settings relatively or names none, and as a prerequisite it then
    # blanked out every downstream cause for the registrations whose paths ARE known --
    # reporting only that one path could not be identified while a peer's settings file sat
    # readably absent. The rules read namedJournals, which contains only the paths this command
    # could name, so the scoping is already in the data and does not belong here too. Where
    # nothing was named at all they answer NOT_EVALUATED rather than putting an unsupported
    # candidate on the table.
    SETTINGS_ABSENT: (NOT_REGISTERED,),
    SETTINGS_UNUSABLE: (NOT_REGISTERED,),
    # The journal causes do NOT require the settings causes to be ruled out. Both sides are now
    # per-registration, so one registration with missing settings must not suppress what
    # another registration's journal says: the journal rules read only the entries whose
    # settings are usable, and a mixed host answers both causes rather than the louder one.
    #
    # Nor do the empty-journal causes require ADAPTER_CANNOT_RUN to be ruled out. That
    # requirement was right for the host where nothing can start and wrong for the host where
    # one of two registrations cannot: it suppressed a working registration's reading. The
    # journal readings carry their own registration's startability instead, so an unstartable
    # registration is simply not in the set those rules read -- which is both narrower and
    # exactly right, because its journal is empty BECAUSE it cannot start.
    RECORDED_ON_ANOTHER_PATH: (NOT_REGISTERED,),
    JOURNALLING_OFF: (NOT_REGISTERED,),
    POLICY_RECORDS_ONLY_FAULTS: (NOT_REGISTERED,),
    NOTHING_RECORDED: (NOT_REGISTERED,),
    # RECORDS_FOUND is the one terminal answer here: it says there is no absence to explain,
    # and unlike every cause above it cannot meaningfully stand beside one. It used to be
    # established off the subset of registrations this command could read, so a host with one
    # unreadable spelling and one journal holding records reported found-records as though it
    # were a repair. It is now asked last and only once every defect cause is ruled out.
    RECORDS_FOUND: (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, ADAPTER_CANNOT_RUN,
                    SETTINGS_ABSENT, SETTINGS_UNUSABLE, JOURNALLING_OFF,
                    RECORDED_ON_ANOTHER_PATH, POLICY_RECORDS_ONLY_FAULTS, NOTHING_RECORDED),
}

# The order causes are reported in. Their dependencies come from CAUSE_REQUIRES and not from
# this sequence, which exists so a reader meets them upstream first.
# RECORDS_FOUND is last, because it is the only terminal answer: every cause before it names
# something to repair and they can legitimately stand together, while "there is no absence to
# explain" cannot stand beside any of them. Its requirements are checked against causes already
# decided, so being asked last is what makes them askable at all.
CAUSE_ORDER = (NOT_REGISTERED, RECORD_PATH_UNIDENTIFIED, ADAPTER_CANNOT_RUN, SETTINGS_ABSENT,
               SETTINGS_UNUSABLE, RECORDED_ON_ANOTHER_PATH, JOURNALLING_OFF,
               POLICY_RECORDS_ONLY_FAULTS, NOTHING_RECORDED, RECORDS_FOUND)

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
