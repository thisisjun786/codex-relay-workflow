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

# The probe's answer when the host refused to create the process at all -- an invalid
# executable format, a missing loader. Nothing ran, and that is a repair rather than an
# uncertainty: the host gets the same refusal on the next Stop.
COULD_NOT_BE_RUN = "not_started"

# It is a Python and it is too old to run this adapter. A separate answer from "not an
# interpreter" because it is a different repair: one path needs a program, the other needs a
# newer one, and collapsing them sends the operator looking for the wrong thing.
BELOW_SUPPORTED_PYTHON = "below_supported_python"

# A target or an interpreter in one of these states cannot be started. PRESENT is the only
# value that says it can; everything else is a spelling this command did not judge, and those
# are not ruled out rather than established either way.
#
# Every consumer reads THIS tuple rather than restating its members, because a state added
# here and dropped by one consumer is the defect that produced it in the first place.
CANNOT_START = (reading.ABSENT, reading.UNREADABLE, COULD_NOT_BE_RUN, BELOW_SUPPORTED_PYTHON)


def _not_registered(observed):
    """Whether anything in the hook file runs this adapter.

    What an empty hook file establishes is the PRESENT. A registration removed after the hook
    had already fired leaves its journal exactly where it was, so "no record of one can exist"
    is a claim about the past that the journal can refute -- and the count sits in the same
    payload, which made that payload contradict itself. This answer claims the half it reads.
    """
    if not observed.get("registrationReadable"):
        # Unless a record settles it, for exactly the reason the plugin-owned branch below
        # gives. A record this hook wrote is proof that something invoked this adapter, and it
        # is the one kind of evidence this command can have about a registration it cannot
        # read. Left unsettled anyway, the payload carried "maybe it is not registered" beside
        # its own count of an invocation that happened, and -- because every rule below
        # requires this one -- blocked RECORDS_FOUND, the answer that says there is no absence
        # to explain. A host holding a record then reported cause_unreadable for an absence it
        # was not showing. The rule was already written for the branch below; it was the
        # unreadable half of the same class that had nobody asking it.
        found_records = observed.get("unregisteredRecords") or 0
        if found_records:
            return RULED_OUT, (str(found_records) + " record(s) this hook wrote are under the"
                               " journal this command settled on, so something invoked this"
                               " adapter. The hook file could not be read, so whether that"
                               " registration is still in place is the registration cell's"
                               " question and not a cause of an absence this host does not"
                               " show")
        return NOT_RULED_OUT, ("the hook file could not be read, so whether this adapter is"
                               " registered for the event was not established")
    found = observed.get("adapterRegistrations") or 0
    if found:
        return RULED_OUT, (str(found) + " registration(s) in the hook file run this adapter")
    if not observed.get("registrationReadHere", True):
        # Unless a record settles it. A record this hook wrote is proof that something invoked
        # this adapter, which is the question -- and it is the one kind of evidence this
        # command can have about a registration it cannot read. Left unsettled anyway, the
        # payload carried "maybe it is not registered" beside its own count of an invocation
        # that happened, and blocked every rule below it including the one that says there is
        # no absence to explain.
        found_records = observed.get("unregisteredRecords") or 0
        if found_records:
            return RULED_OUT, (str(found_records) + " record(s) this hook wrote are under the"
                               " journal this command settled on, so something invoked this"
                               " adapter; whether that registration is still in place is the"
                               " registration cell's question and not a cause of an absence"
                               " this host does not show")
        # An empty hook file is what a CORRECTLY installed plugin-owned host looks like: the
        # registration lives in a package manifest this command does not read. Establishing an
        # absence from the one file it is deliberately not in reported a repair that would put
        # a second owner on one event, which the ownership rules exist to refuse.
        #
        # Two roads reach here and they do not say the same thing. Where the plugin owner was
        # POSITIVELY read, the manifest is the explanation. Where nothing could be read at all
        # -- settings that are absent, unreadable, not an object, or naming an owner this
        # reader does not know -- no owner and no registration location was established, and
        # saying the settings record a manifest owner told the operator something this command
        # never read, beside a registrationOwner cell saying not_read in the same payload.
        if observed.get("registrationElsewhere"):
            return NOT_RULED_OUT, ("these settings record an owner whose registration lives in"
                                   " a package manifest rather than in this hook file, and"
                                   " this command does not read that manifest, so an empty"
                                   " hook file establishes nothing about whether this adapter"
                                   " is registered")
        return NOT_RULED_OUT, ("nothing established who owns this registration or where it"
                               " lives, so the hook file is not established as the place it"
                               " would be and an empty one establishes nothing about whether"
                               " this adapter is registered")
    kept = observed.get("unregisteredRecords") or 0
    if kept:
        return ESTABLISHED, ("the hook file was read and registers this adapter for nothing, so"
                             " nothing on this host invokes it now. The " + str(kept)
                             + " record(s) under the journal this command settled on are what"
                             " an earlier registration left: this answer explains why no"
                             " FURTHER record can appear and does not claim none exists")
    return ESTABLISHED, ("the hook file was read and registers this adapter for nothing, so"
                         " nothing on this host invokes it and no further record of one can be"
                         " written")


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
        # Nothing named a command. Where the hook file IS where this host's registration
        # lives, that is not uncertainty about startability: there is no command to start and
        # not_registered owns the host, so this question was never reachable. Scoping it here
        # rather than as a prerequisite is what lets the plugin-scoped probe below be asked at
        # all, on a host where nobody can rule not_registered out.
        if observed.get("registrationReadHere", True):
            return NOT_EVALUATED, ("no registration named a command here for this question to"
                                   " be about")
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
    # What this rules out is the cause it is named for: no registered half is in a state that
    # says the host cannot start it. It is NOT "these registrations start". The interpreter was
    # asked to answer as a supported Python at the spelling the host resolves, and the adapter
    # script was read for presence and readability; the registered command was never run as
    # written, and a wrapper's own target is deliberately not followed. Saying the stronger
    # thing here would settle startability from a reading that never established it.
    return RULED_OUT, ("no registered adapter or interpreter is in a state that stops the host"
                       " starting it: the interpreter answered as a supported Python at the"
                       " spelling registered and the adapter script was read. That the"
                       " registered command as a whole starts is not established here -- it"
                       " was never run as written, and a wrapper's own target is not followed")


def _halves(probe):
    """A registration's two halves. Both have to be there for the host to start it, so they are
    read as a pair: a present interpreter under a missing adapter starts nothing."""
    return {str(probe.get("adapter")), str(probe.get("interpreter"))}


def _startable(probe):
    return _halves(probe) == {reading.PRESENT}


def _settled_limit(settled):
    """What answering from the settled reading does NOT establish.

    These settings really are the ones this command settled on and they really are rejected or
    gone. What is not established is that anything reads them: the registration they record
    lives in a package manifest this command does not open, and an installed package may not be
    there at all. Said in the answer rather than left for the operator to infer, because a
    repair presented as the settled cause of an absence is a claim about the absence too.
    """
    if settled is None:
        return ""
    return (". Whether anything reads them was not established here: the registration these"
            " settings record lives in a package manifest this command does not open, and"
            " whether such a package is installed at all is not read either")


def _settled_settings(observed):
    """The settings this command settled on, where they are the only ones in scope.

    A registration in the hook file names its own settings and namedJournals carries those. A
    plugin-owned host registers through a package manifest, so nothing in the hook file names
    one and namedJournals is empty -- and the settings causes then had nothing to read on
    exactly the host whose repair they exist to name. This is that host's entry.

    Only where the registration is POSITIVELY established to live somewhere else. "Not here"
    is not that: it is also the answer when nobody could read who owns the registration at all,
    and a host whose registration names a settings path this command cannot resolve reached
    exactly that state -- so the settled reading, which is about a different file entirely, was
    handed to the settings causes and they named a repair for a path nothing here can read.
    The support case guarding that direction is what caught it.

    And where the hook file IS where the registration lives while nothing is registered there,
    a missing settings file is not a cause of anything, because registering the hook writes it.
    """
    if observed.get("namedJournals"):
        return None
    if not observed.get("registrationElsewhere"):
        return None
    entry = observed.get("settledSettings")
    return entry if isinstance(entry, dict) and entry.get("settings") else None


def _settings_absent(observed):
    """ANY registration whose settings are gone, not every one of them.

    Every registration runs and reads its own settings, so a peer that is fine says nothing
    about a peer that is broken. Requiring all of them to be absent meant one good registration
    suppressed the other's repair entirely: the operator was told the journal was empty and
    never that a second registration releases every invocation until its settings come back.
    """
    entries = observed.get("namedJournals") or []
    settled = _settled_settings(observed)
    if settled is not None:
        entries = [settled]
    if not entries:
        # Not "could not tell": no registration named a file for this question to be about.
        # record_path_unidentified owns that state, and answering NOT_RULED_OUT here would put
        # a candidate on the table that no reading ever pointed at.
        return NOT_EVALUATED, "no registration named a settings file this command could read"
    gone = [entry["settings"] for entry in entries
            if entry.get("settingsState") == reading.ABSENT]
    if gone:
        return ESTABLISHED, ("these settings files are established absent, so whatever reads"
                             " them is told nothing about where to record and keeps no"
                             " journal: " + ", ".join(gone) + _settled_limit(settled))
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
    settled = _settled_settings(observed)
    if settled is not None:
        entries = [settled]
    if not entries:
        return NOT_EVALUATED, "no registration named a settings file this command could read"
    unusable = [entry["settings"] for entry in entries
                if entry.get("settingsState") not in (reading.ABSENT, reading.ACCESS_ERROR)
                and not entry.get("usable")]
    if unusable:
        return ESTABLISHED, ("these settings files are ones this hook's own reader rejects ("
                             + ", ".join(unusable) + "), so every invocation that reads them"
                             " releases without recording" + _settled_limit(settled))
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


def _unjudged_peer(observed):
    """A registration whose startability was never established, so its journal is neither in
    the set nor safely out of it.

    Excluding it silently was the bug: a peer spelled WORKSPACE_DEPENDENT, or one whose probe
    hit an access error, was treated exactly like a peer that positively cannot start, and its
    journal -- which may hold records -- stopped counting while a blocked neighbour supplied a
    settled explanation for the whole host.

    A registration that keeps NO journal is not one of these, however its startability reads.
    Every rule asking this is a rule about counts, and a registration configured never to
    record contributes no count whether or not the host can start it: repairing its startability
    would still leave it recording nothing. Left in, it kept every count-based cause unsettled
    beside a peer whose journal had been read -- uncertainty about a registration that cannot
    hold a record either way. journalling_off owns that registration, and it does not ask this.
    """
    return [entry for entry in (observed.get("namedJournals") or [])
            if entry.get("usable") and entry.get("startable") is None
            and entry.get("recordsAnswer") != NO_RECORDS_KEPT]


def _named(entries):
    return ", ".join(str(entry.get("journalRoot") or entry.get("settings")) for entry in entries)


def _distinct_journals(entries):
    """How many DIFFERENT journals these entries name.

    Registrations are not journals. Two of them can name separate settings files that
    configure one journalRoot, and questions about two journals disagreeing are
    meaningless for a single directory.
    """
    return {str(entry.get("journalRoot") or entry.get("settings")) for entry in entries}


def _records_found(observed):
    unjudged = _unjudged_peer(observed)
    if unjudged:
        return NOT_RULED_OUT, ("a registration whose startability was never established names "
                               + _named(unjudged) + ", so whether its journal counts toward"
                                 " this question was not settled either")
    holding, empty, _off, unread = _record_answers(observed)
    if holding and not empty and not _off and not unread:
        return ESTABLISHED, ("every journal these registrations name holds records this hook"
                             " wrote, so there is no absence to explain: " + _named(holding))
    if not (holding or empty or _off or unread):
        # No registration here named a journal, which is what a plugin-owned host looks like.
        # The journal this command settled on is then the only one in scope, and a record in it
        # is the same terminal answer: there is no absence to explain.
        found_records = observed.get("unregisteredRecords") or 0
        if found_records:
            return ESTABLISHED, ("the journal this command settled on holds "
                                 + str(found_records) + " record(s) this hook wrote and no"
                                 " registration here names another, so there is no absence to"
                                 " explain")
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
    # An unjudged peer only matters here when it could be a SECOND journal: this cause is two
    # journals disagreeing, and a peer sharing the one journal its neighbour already named
    # cannot disagree with itself however its startability reads.
    eligible = _distinct_journals(holding + empty + unread)
    unjudged = [entry for entry in _unjudged_peer(observed)
                if _distinct_journals([entry]) - eligible]
    # Novelty is not enough: a SECOND journal has to exist for there to be a disagreement at
    # all. On a host whose only registration is unjudged that set is empty, so its journal was
    # novel by default and this cause stood on a host that has exactly one directory -- and
    # one directory cannot disagree with itself whoever failed to judge it.
    if unjudged and len(eligible | _distinct_journals(unjudged)) > 1:
        return NOT_RULED_OUT, ("a registration whose startability was never established names "
                               + _named(unjudged) + ", so whether its journal counts toward"
                                 " this question was not settled either")
    if holding and unread:
        return NOT_RULED_OUT, ("records were found under " + _named(holding) + " and another"
                               " named journal could not be listed")
    if len(_distinct_journals(unread)) > 1:
        # One unread journal may hold records while another is empty, which is this
        # cause. Neither was read, so neither side is settled and the candidate stands.
        #
        # Counted over DISTINCT journals rather than registrations: two registrations
        # can name settings files that configure one journalRoot, and one journal cannot
        # disagree with itself. Counting entries reported possible divergence for a host
        # that has a single directory nobody could list.
        return NOT_RULED_OUT, ("none of " + _named(unread) + " could be listed, so whether"
                               " they disagree about holding records was not settled")
    if empty and unread:
        # The unread journal may hold records, and against a journal that was read and
        # holds none that is exactly this cause. Ruling it out because the holding side
        # happens to be the one nobody could list drops a candidate the readings leave
        # standing.
        return NOT_RULED_OUT, ("" + _named(unread) + " could not be listed and may hold records, which against "
                               + _named(empty) + ", read and holding none, would be this cause")
    return RULED_OUT, "no two named journals disagree about holding records"


def _journalling_off(observed):
    # Read over every registration whose settings are usable, startable or not. Unlike an
    # empty journal, a disabled policy is NOT explained by the adapter being unstartable:
    # repairing the adapter still produces no firing evidence until journalling is switched
    # back on, so filtering this cause by startability hid a repair the other one does not
    # cover.
    entries = observed.get("namedJournals") or []
    settled = _settled_settings(observed)
    if settled is not None:
        # The plugin-owned host, whose registration lives in a manifest this command does not
        # read. Nothing in the hook file names a settings file, so namedJournals is empty and
        # this cause had nothing to read on a host that states its policy plainly.
        entries = [settled]
    if not entries:
        # Not "could not tell": no settings file was read for this question to be about, which
        # is a question this host does not have rather than one left open.
        return NOT_EVALUATED, ("no registration named a settings file this command could read,"
                               " so there is no journal policy here for this question to be"
                               " about")
    off = [entry for entry in entries
           if entry.get("usable") and entry.get("recordsAnswer") == NO_RECORDS_KEPT]
    # ANY such registration, for the same reason a missing settings file is: a peer that keeps
    # a journal is not evidence that this one does. Requiring every registration to be off hid
    # a registration configured never to record behind a neighbour that records normally, and
    # that registration can never produce firing evidence at all.
    if off:
        return ESTABLISHED, ("these registrations keep no journal, so they record nothing about"
                             " their own invocations by configuration and their absence says"
                             " nothing about firing: " + _named(off))
    # No guard for an unjudged peer here: whether a registration keeps a journal is what
    # its settings say, and no startability reading can change that. Letting uncertainty
    # about a peer reopen it reported a policy the settings conclusively rule out.
    return RULED_OUT, "every registration with usable settings keeps a journal"


def _policy_records_only_faults(observed):
    """Never established, and that is the point.

    Under faults_only the guard records only an invocation that faulted, so an empty journal is
    what a hook that fires constantly and never faults looks like, and it is also what a hook
    that never fired looks like. One observation, two explanations, and no reading here
    separates them. Reporting either as established would be choosing.
    """
    # Read over every usable registration rather than over the startable subset, and on the
    # entry's own policy and its own count. Both halves of this question are the
    # registration's own, so an unjudged peer neither creates nor removes the ambiguity.
    entries = observed.get("namedJournals") or []
    settled = _settled_settings(observed)
    if settled is not None:
        entries = [settled]
    if not entries:
        return NOT_EVALUATED, ("no registration named a settings file this command could read,"
                               " so there is no journal policy here for this question to be"
                               " about")
    faults = [entry for entry in entries
              if entry.get("usable") and entry.get("faultsOnly")
              and entry.get("recordsAnswer") == COUNTED and not entry.get("records")]
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
    # An unjudged peer only reopens this count when its journal is one nobody has read. A peer
    # sharing a journal already read and found EMPTY cannot change that observation: if the
    # host can start it, it writes into the very directory this command listed; if it cannot,
    # it is out of the journal question entirely. Counted anyway, the answer reported an
    # unsettled count for a directory it had just listed, and hid an established
    # nothing_recorded behind uncertainty about a peer that shares its reading.
    unjudged = [entry for entry in _unjudged_peer(observed)
                if _distinct_journals([entry]) - _distinct_journals(empty)]
    if unjudged:
        return NOT_RULED_OUT, ("a registration whose startability was never established names "
                               + _named(unjudged) + ", so whether its journal counts toward"
                                 " this question was not settled either")
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
    NOT_REGISTERED: (("registrationReadable", "adapterRegistrations", "registrationReadHere",
                      "unregisteredRecords"), _not_registered),
    RECORD_PATH_UNIDENTIFIED: (("relativeSettings", "silentRegistrations"),
                               _record_path_unidentified),
    ADAPTER_CANNOT_RUN: (("startProbes", "registrationReadHere"), _adapter_cannot_run),
    SETTINGS_ABSENT: (("namedJournals", "settledSettings", "registrationElsewhere"),
                      _settings_absent),
    SETTINGS_UNUSABLE: (("namedJournals", "settledSettings", "registrationElsewhere"),
                        _settings_unusable),
    RECORDS_FOUND: (("namedJournals", "unregisteredRecords"), _records_found),
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
    # ADAPTER_CANNOT_RUN does not require it either, and for the reason the settings causes
    # do not: a launcher this command CAN read is a present reading, and waiting for
    # not_registered to be ruled out hid it on exactly the host that cannot rule it out. A
    # plugin-owned installation with an empty journal and a deleted entry point reported only
    # "maybe it is not registered" while adapterEntryPoint read ABSENT beside it. The rule
    # answers NOT_EVALUATED by itself where the hook file is where the registration lives and
    # named no command, so a user-owned host reads exactly as before.
    ADAPTER_CANNOT_RUN: (),
    # Nor do they require RECORD_PATH_UNIDENTIFIED. That cause is established when ANY
    # registration spells its settings relatively or names none, and as a prerequisite it then
    # blanked out every downstream cause for the registrations whose paths ARE known --
    # reporting only that one path could not be identified while a peer's settings file sat
    # readably absent. The rules read namedJournals, which contains only the paths this command
    # could name, so the scoping is already in the data and does not belong here too. Where
    # nothing was named at all they answer NOT_EVALUATED rather than putting an unsupported
    # candidate on the table.
    # The settings causes no longer require NOT_REGISTERED either, for the reason the paragraph
    # above gives about RECORD_PATH_UNIDENTIFIED: the scoping is in the data. Requiring it
    # blanked them out on the one host whose repair they name -- a plugin-owned host, where
    # NOT_REGISTERED is unsettled because the registration lives in a manifest this command does
    # not read, and the settings it settled on are rejected and say so. The rules answer
    # NOT_EVALUATED by themselves where the hook file IS where the registration lives and
    # nothing is registered there, so a user-owned host reads exactly as before.
    SETTINGS_ABSENT: (),
    SETTINGS_UNUSABLE: (),
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
    # The two POLICY causes no longer require it either, for the reason given above about the
    # settings causes: what a journal policy says is read from a settings file, the scoping is
    # in the data, and requiring NOT_REGISTERED blanked them out on the one host that cannot
    # rule it out. A plugin-owned host configured no_journal reported only "maybe it is not
    # registered" while its own journalPolicy sat readable in the payload beside it -- an
    # answerable cause withheld because a different question was open. They answer
    # NOT_EVALUATED by themselves where no settings file was read for this question, so a
    # user-owned host reads exactly as before.
    #
    # RECORDED_ON_ANOTHER_PATH keeps the requirement: it is a claim about two journals
    # DISAGREEING, which needs two registrations this command could read, and there is no
    # settled-settings host that can answer it.
    JOURNALLING_OFF: (),
    POLICY_RECORDS_ONLY_FAULTS: (),
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


def _settled_enough(standings, asked, name):
    """Whether a cause a later rule depends on is settled enough for that rule to mean anything.

    Ruled out is the plain case. The other one is a cause whose OWN rule answered NOT_EVALUATED:
    that rule read this host and reported there is no question of that kind here -- no
    registration named a command, no registration named a settings file this command could
    read. A question that does not exist is not a question left open, and counting it as one is
    the same substitution this module exists to remove, inverted: an inapplicable answer read as
    an unresolved one. It left a host whose journal holds a record with every defect cause ruled
    out, the terminal answer unasked, and cause_unreadable reported for an absence it was not
    showing.

    A cause the loop SKIPPED also carries NOT_EVALUATED, and that one still blocks: nothing read
    the host for it, so it establishes nothing either way.
    """
    if standings.get(name) == RULED_OUT:
        return True
    return name in asked and standings.get(name) == NOT_EVALUATED


def decide(observed):
    """Why there is no record, decided over the cells status() already produced.

    Every rule runs. The verdict is a single cause only when exactly one is established and
    nothing was left unsettled; two established causes are reported as two, because an absence
    that needs two repairs is not an absence nobody could explain.
    """
    standings, details = {}, {}
    # Which causes their own rule actually answered. A cause the loop SKIPPED carries
    # NOT_EVALUATED because something upstream is still open; a cause the rule answered
    # NOT_EVALUATED carries it because the rule read this host and found no question of that
    # kind here at all. Those are not the same standing to depend on, and the stored value
    # cannot tell them apart.
    asked = set()
    for cause in CAUSE_ORDER:
        required = CAUSE_REQUIRES[cause]
        blocked = [name for name in required if not _settled_enough(standings, asked, name)]
        if blocked:
            standings[cause] = NOT_EVALUATED
            details[cause] = ("not asked, because " + ", ".join(blocked) + " would have to be"
                              " ruled out first for this question to mean anything")
            continue
        standings[cause], details[cause] = CAUSE_RULES[cause][1](observed)
        asked.add(cause)

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
