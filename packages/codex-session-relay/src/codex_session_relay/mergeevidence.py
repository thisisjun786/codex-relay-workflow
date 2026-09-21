"""MERGE-EVIDENCE-01: the review and checks predicates, stated once.

Two places ask the same question about one pull request. The child asks it when it decides
whether its work may be handed to the parent at all, and the parent asks it again when it
restates the candidate immediately before merging. It is the same question both times: was
the review enumerated to the end with nothing unresolved, and is every declared required
check green on this exact head at its newest attempt.

Written twice, the two answers drift, and the drift is invisible because both sides stay
green. So the predicates live here as pure functions over plain data.

They return PROBLEMS, each carrying a code and a detail, rather than a refusal. A caller has
to choose a named refusal reason, and the next action genuinely differs: an undeclared
required set is a thing the producer must go and read, a stale check is a thing it must wait
for or fix, and an unenumerated review is a thing it must finish. Returning prose alone would
make the caller parse sentences to tell those apart. Each caller maps the code to its own
vocabulary; the detail is the sentence a human reads.

Nothing here contacts a forge. These functions establish that a restatement is internally
consistent and about the head it claims. What a forge actually requires is the caller's
declaration, which is why `required` is an argument rather than something discovered.
"""

from collections import namedtuple

#: `incumbent` is the party or name a refusal points at, and it is carried here rather than
#: rebuilt by the caller because Refusal.to_record writes it into the conflict ledger. A
#: delegation that let it default would keep every verdict identical and quietly blank a
#: stored field, which is the kind of change a pass/refuse assertion never notices.
Problem = namedtuple("Problem", "code detail incumbent", defaults=("",))

MALFORMED = "malformed_evidence"
REVIEW_UNSTATED = "review_unstated"
REVIEW_INCOMPLETE = "review_incomplete"
CHECKS_STALE = "checks_stale"
REQUIRED_UNDECLARED = "required_undeclared"

REVIEW_FIELDS = ("hasNextPage", "pagesRead", "totalCount", "threadsSeen", "unresolved")
_COUNTS = ("pagesRead", "totalCount", "unresolved")
_CHECK_STRINGS = ("runId", "name", "headSha", "conclusion")

#: `required` is UNDECLARED when nobody looked it up. An empty list is a different
#: statement: somebody read the branch protection and it requires nothing. Collapsing the
#: two is how "I did not check" passes for "nothing to check".
UNDECLARED = None


def _is_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


def details(problems):
    """The sentences, in order, for a caller joining them into one message."""
    return [problem.detail for problem in problems]


def shape_problems(review, checks, required=UNDECLARED, head_sha=None):
    """Types and shapes, before any rule reads a value.

    The predicates below were written for a caller that had already validated its payload,
    which is true of the merge turn and is not true of a child assembling a record for the
    first time. Reading unvalidated data with them fails in the wrong way: a non-mapping entry
    raises AttributeError out of `.get`, a non-numeric attempt raises ValueError out of
    `int()`, and both escape as host exceptions rather than as something the producer is told.

    Worse than an exception is a shape that passes. `threadsSeen` given as a string iterates
    per CHARACTER, so "ab" beside totalCount 2 counts as two threads seen and the record
    passes having enumerated nothing. Identifiers that are not strings are coerced by `str`
    and start agreeing with each other. An attempt of 0 is turned into 1 by the `or 1`
    fallback, so a run's real newest attempt can be hidden behind a zero. Every one of those
    is the same silence-reads-as-satisfied failure R0 exists for, arriving through the type
    system instead of through an absent key.
    """
    problems = []

    def bad(detail):
        problems.append(Problem(MALFORMED, detail))

    if head_sha is not None and (not isinstance(head_sha, str) or not head_sha.strip()):
        bad("the head this evidence is about is a non-empty commit sha, not "
            + repr(head_sha))
    if not isinstance(review, dict):
        bad("the review record is an object stating " + ", ".join(REVIEW_FIELDS)
            + ", not a " + type(review).__name__)
    else:
        if "hasNextPage" in review and not isinstance(review["hasNextPage"], bool):
            bad("hasNextPage is true or false, not a " + type(review["hasNextPage"]).__name__
                + "; a truthy value of another type says nothing about pagination")
        for field in _COUNTS:
            if field not in review:
                continue
            value = review[field]
            if not _is_int(value):
                bad(field + " is a whole number, not a " + type(value).__name__)
            elif value < 0:
                bad(field + " is " + repr(value) + ", and a count is never negative")
        if "threadsSeen" in review:
            seen = review["threadsSeen"]
            if not isinstance(seen, (list, tuple)):
                bad("threadsSeen is a list of thread identifiers, not a "
                    + type(seen).__name__
                    + "; a string would be counted one character at a time")
            else:
                for position, one in enumerate(seen):
                    if not isinstance(one, str):
                        bad("threadsSeen entry " + str(position) + " is a thread identifier"
                            " string, not a " + type(one).__name__
                            + "; coercing it would let two different values agree")
    if required is not UNDECLARED:
        if not isinstance(required, (list, tuple, set, frozenset)):
            bad("the required check names are a list, not a " + type(required).__name__)
        else:
            for one in required:
                if not isinstance(one, str):
                    bad("required check name " + repr(one) + " is a string, not a "
                        + type(one).__name__)
    if not isinstance(checks, (list, tuple)):
        bad("the restated checks are a list of check runs, not a " + type(checks).__name__)
    else:
        for position, entry in enumerate(checks):
            where = "check entry " + str(position)
            if not isinstance(entry, dict):
                bad(where + " is an object naming its runId, name, headSha, conclusion and"
                    " attempt, not a " + type(entry).__name__)
                continue
            for field in _CHECK_STRINGS:
                if field in entry and not isinstance(entry[field], str):
                    bad(where + " states " + field + " as a "
                        + type(entry[field]).__name__ + ", not a string; coercing it would"
                        " let two different runs agree")
            if "attempt" not in entry:
                # Defaulting to 1 let an omission stand for a fact. An older successful entry
                # submitted with no attempt reads as the first one, and the newest attempt -
                # the failing one this rule exists to catch - is simply never mentioned.
                bad(where + " does not state attempt, so which attempt is newest cannot be"
                    " decided; an omitted attempt is not evidence that this is the newest")
                continue
            attempt = entry["attempt"]
            if not _is_int(attempt):
                bad(where + " states attempt " + repr(attempt) + ", which is not a whole"
                    " number, so which attempt is newest cannot be decided")
            elif attempt < 1:
                bad(where + " states attempt " + repr(attempt) + ", and attempts are counted"
                    " from one; a lower value is read as the first attempt and hides the"
                    " newest one")
    return problems


def review_problems(review):
    """Every page read, every thread seen, nothing unresolved.

    An unstated field is reported on its own, and reported INSTEAD of the value checks,
    because absence used to read as satisfied: a missing hasNextPage was falsy, a missing
    totalCount was zero and matched an empty threadsSeen, and a missing unresolved was zero.
    A record that said nothing passed every check. Reporting absence alone also keeps the
    value checks from commenting on values nobody supplied, which would otherwise produce a
    second, invented complaint about a default this module chose.

    Assumes shape_problems already passed. Run it first.
    """
    review = review if isinstance(review, dict) else {}
    unstated = [
        Problem(REVIEW_UNSTATED, "the review record does not state " + field)
        for field in REVIEW_FIELDS if field not in review
    ]
    if unstated:
        return unstated
    problems = []

    def incomplete(detail):
        problems.append(Problem(REVIEW_INCOMPLETE, detail))

    if review.get("hasNextPage"):
        incomplete("hasNextPage is still true, so the review was not enumerated")
    if int(review.get("pagesRead", 0) or 0) < 1:
        incomplete("no review page was read")
    seen = review.get("threadsSeen") or []
    total = int(review.get("totalCount", 0) or 0)
    identifiers = [str(one) for one in seen if str(one or "").strip()]
    distinct = set(identifiers)
    if len(identifiers) != len(seen):
        incomplete("threadsSeen contains a blank identifier")
    if len(distinct) != len(identifiers):
        # Counting entries does not establish that each one is a different thread. A
        # duplicated page substitutes a thread nobody read without changing the length.
        incomplete("threadsSeen repeats an identifier, so its length is not a count"
                   " of threads actually seen")
    if len(distinct) != total:
        incomplete("totalCount is " + str(total) + " and " + str(len(seen))
                   + " threads were seen")
    if int(review.get("unresolved", 0) or 0) != 0:
        incomplete(str(review.get("unresolved")) + " threads are unresolved")
    return problems


def checks_problems(head_sha, required, checks, *, require_declared=False, providers=None):
    """Present, successful, on this head, and at the highest attempt submitted for its run.

    The attempt rule matters because a rerun is how a red check becomes green: accepting any
    successful entry would let an older passing attempt stand for a run whose newest attempt
    failed.

    At most one problem is returned. Unlike the review vector, where the fields are
    independent and a producer is better told all of them at once, these rules are sequential:
    once a set has no identity there is nothing to say about its conclusions, so a second
    complaint derived from the first would be noise rather than information.

    `require_declared` is what separates the two callers, because an UNDECLARED required set
    does not mean the same thing on both sides. A merge turn is restating a declaration and an
    empty one is a fact about the target: it requires nothing, so one green run is enough. A
    child assembling its own record has usually not read the branch protection at all, and
    treating that ignorance as "nothing required" is a false pass with a specific shape - a
    failing dev-gate beside a passing lint satisfies "something succeeded" and hands over a red
    candidate. So the child passes True and an UNDECLARED set is refused. An explicitly empty
    list still means nothing is required, on either side; the distinction is between not having
    looked and having looked.

    `providers` carries the identity a branch rule can bind a context to. GitHub's required
    status checks name an integration as well as a context, and matching on the name alone
    accepts a namesake from anywhere: the branch requires `dev-gate` from app 42, app 42 never
    runs, app 99 publishes a successful `dev-gate`, and the gate opens on a check nobody
    required. Where a provider is declared, only an entry from that provider answers for the
    context - and an entry from another one is not the required check, so its failure does not
    block either. Omitted, nothing changes: a caller that did not read the integration out of
    the rule has not learnt anything about it.

    Assumes shape_problems already passed. Run it first.
    """
    if require_declared and required is UNDECLARED:
        return [Problem(
            REQUIRED_UNDECLARED,
            "the record does not state which checks this branch requires, so a failing"
            " required check cannot be told from a failing optional one; read the branch"
            " protection and declare the names, or declare an empty list to say it requires"
            " none")]
    checks = list(checks or [])
    required = list(required or [])
    providers = dict(providers or {})

    def answers_for(entry, name):
        """Whether this entry is the check the branch named, provider and all."""
        wanted = providers.get(name)
        return wanted is None or str(entry.get("provider") or "") == str(wanted)

    def stale(detail):
        return [Problem(CHECKS_STALE, detail)]

    def stale_at(detail, incumbent):
        return [Problem(CHECKS_STALE, detail, incumbent)]

    if not checks:
        return stale("no check runs were restated, so nothing says this head is green")
    nameless = [
        entry for entry in checks
        if not str(entry.get("runId", "")).strip() or not str(entry.get("name", "")).strip()
    ]
    if nameless:
        # A conclusion with nothing identifying it cannot be checked against anything,
        # and with no declared required names it was the only evidence there was.
        return stale("a restated check carries no runId or no name, so there is nothing to say"
                     " which check it is or to compare against a required set")
    highest = {}
    for entry in checks:
        run = str(entry.get("runId", ""))
        highest[run] = max(highest.get(run, -1), int(entry.get("attempt", 1) or 1))
    for entry in checks:
        run = str(entry.get("runId", ""))
        if int(entry.get("attempt", 1) or 1) != highest[run]:
            continue
        # Every entry has to be ABOUT this head, because one that is not is evidence
        # about another commit and has no business in this set.
        if entry.get("headSha") != head_sha:
            return stale_at("check run " + repr(run) + " reports head "
                            + repr(entry.get("headSha")) + ", not " + repr(head_sha), run)
        # A conclusion is only binding for a check the caller declared required. An
        # optional lint failing alongside a green dev-gate is not a reason to refuse a
        # merge, and refusing it made the declared set mean nothing.
        if (str(entry.get("name", "")) in required
                and answers_for(entry, str(entry.get("name", "")))
                and entry.get("conclusion") != "success"):
            return stale_at("required check " + repr(entry.get("name")) + " (run " + repr(run)
                            + ") concluded " + repr(entry.get("conclusion"))
                            + " on its newest attempt", run)
    present = {
        str(entry.get("name", "")) for entry in checks
        if int(entry.get("attempt", 1) or 1) == highest[str(entry.get("runId", ""))]
        and entry.get("conclusion") == "success"
        and answers_for(entry, str(entry.get("name", "")))
    }
    missing = [name for name in required if name not in present]
    if missing:
        return stale_at(
            "these checks were declared required and are not present and successful in the"
            " restated set" + (" from the provider the branch rule names" if any(
                name in providers for name in missing) else "") + ": " + repr(missing),
            missing[0])
    if not required and not present:
        # With nothing declared required, the set still has to contain something green on
        # this head; otherwise an all-red restatement would pass for want of a rule.
        return stale("no check declared required and nothing in the restated set succeeded on "
                     + repr(head_sha) + ", so nothing says this head is green")
    return []


def handoff_problems(head_sha, review, checks, required=UNDECLARED, *, providers=None,
                     reviews=None):
    """Everything a child-side record must satisfy, in the order that keeps it honest.

    Shape first, and nothing else when shape fails. The semantic rules read values with `str`
    and `int` coercion, so running them on a malformed payload does not merely risk an
    exception, it can produce a PASS: a two-character string threadsSeen enumerates two
    threads, and identifiers of the wrong type quietly agree. A verdict derived from coerced
    nonsense is worse than a refusal, so the ordering is part of the contract rather than an
    implementation detail, and it lives here so no caller has to remember it.
    """
    malformed = shape_problems(review, checks, required=required, head_sha=head_sha)
    if malformed:
        return malformed
    return (review_problems(review)
            + review_state_problems(reviews)
            + checks_problems(head_sha, required, checks, require_declared=True,
                              providers=providers))


CHANGES_REQUESTED = "review_changes_requested"


def review_state_problems(reviews):
    """A reviewer who asked for changes and has not since said otherwise.

    This is not content triage and deliberately reads nothing a reviewer wrote. A submitted
    review carries a formal STATE, and CHANGES_REQUESTED is a mechanical fact about the pull
    request in the same way a failing check is: the forge itself will hold the merge for it.
    Enumerating reviews and then grading none of them left a candidate with an outstanding
    changes-requested review reading as ready, which is the gap between collecting evidence and
    using it.

    Latest per author, because a reviewer who asked for changes and then approved has answered
    their own review, and the older state is history rather than an open request.
    """
    latest = {}
    for entry in reviews or ():
        if not isinstance(entry, dict):
            continue
        author = str(entry.get("author") or "")
        state = str(entry.get("state") or "").upper()
        if state not in ("APPROVED", "CHANGES_REQUESTED", "DISMISSED"):
            # COMMENTED and PENDING say nothing about whether the reviewer is waiting on a
            # change, so they neither block nor clear an earlier request.
            continue
        when = str(entry.get("submittedAt") or "")
        if author not in latest or when >= latest[author][0]:
            latest[author] = (when, state)
    asking = sorted(author for author, (_when, state) in latest.items()
                    if state == "CHANGES_REQUESTED")
    if not asking:
        return []
    return [Problem(
        CHANGES_REQUESTED,
        "these reviewers asked for changes and have not since approved or dismissed their own"
        " review: " + ", ".join(repr(one) for one in asking))]


#: The candidate itself, which is not the same question as its review or its checks. A pull
#: request can carry a fully enumerated review and a green required gate and still be unmergeable:
#: closed, conflicted, or blocked by the forge's own rules. Those facts had no home, so a caller
#: that read only the two predicates above could assemble a complete record about something nobody
#: could merge. They live here rather than in the observer for the reason everything else here
#: does: one place decides readiness, and an observer that also decided it would drift from this
#: one while both stayed green.
CANDIDATE_NOT_OPEN = "candidate_not_open"
CANDIDATE_DRAFT = "candidate_draft"
CANDIDATE_CONFLICTED = "candidate_conflicted"
CANDIDATE_BLOCKED = "candidate_blocked"
CANDIDATE_BEHIND = "candidate_behind"
CANDIDATE_UNKNOWN = "candidate_unknown"

#: What the forge's merge-state values mean here. Written as a table rather than as a chain of
#: ifs because the dangerous entry is the one nobody wrote: an unrecognised value has to be
#: UNKNOWN, and a forge that adds a state next year must not acquire a passing one by default.
#:
#: Two states deliberately do NOT refuse on their own. UNSTABLE says something non-passing exists
#: on the head; it does not say the thing is required, and checks_problems above exists precisely
#: to let an optional lint fail beside a green required gate. Refusing UNSTABLE would contradict
#: that rule from inside the same module. BEHIND is the same shape: being behind the base blocks
#: only where the branch declares a strict required-status-checks policy, which is why it is
#: passed in rather than assumed.
CLEAN_MERGE_STATES = ("clean", "has_hooks", "unstable")


def draft_problems(is_draft):
    """A draft is not a candidate, said once.

    The receipt contract refuses a draft handoff and so does the collector, and for a while each
    said it in its own words. Two spellings of one rule is how the rule starts meaning two things,
    so the sentence lives here and both callers raise it.
    """
    if not is_draft:
        return []
    return [Problem(
        CANDIDATE_DRAFT,
        "the pull request is still a draft, so the review it reports was never actually "
        "requested; mark it ready for review before handing it over")]


def candidate_problems(candidate, *, strict_base=False):
    """Is this pull request a thing that could be merged at all?

    Separate from the review and the checks because the next action is different again: a closed
    candidate is not waiting for anything, a conflicted one needs a rebase or a merge, and a
    merge state the forge has not finished computing needs only to be asked again.

    `strict_base` comes from the branch's own declared rules rather than from a guess, because
    "behind the base" is a refusal in a repository that requires branches to be current and is
    merely a fact in one that does not.
    """
    if not isinstance(candidate, dict):
        return [Problem(MALFORMED,
                        "the candidate is an object stating state, isDraft and mergeStateStatus,"
                        " not a " + type(candidate).__name__)]
    problems = []
    state = str(candidate.get("state") or "").lower()
    if candidate.get("merged"):
        problems.append(Problem(CANDIDATE_NOT_OPEN,
                                "this pull request is already merged, so there is nothing left to"
                                " hand over"))
    elif state and state != "open":
        problems.append(Problem(CANDIDATE_NOT_OPEN,
                                "this pull request is " + repr(state) + ", not open"))
    elif not state:
        problems.append(Problem(CANDIDATE_UNKNOWN,
                                "the candidate does not say whether it is open"))
    problems.extend(draft_problems(candidate.get("isDraft")))
    status = str(candidate.get("mergeStateStatus") or "").lower()
    if status in CLEAN_MERGE_STATES:
        pass
    elif status == "dirty":
        problems.append(Problem(CANDIDATE_CONFLICTED,
                                "the candidate does not merge cleanly into its base"))
    elif status == "blocked":
        problems.append(Problem(CANDIDATE_BLOCKED,
                                "the forge reports this candidate blocked by its own branch"
                                " rules, so something it requires is not satisfied yet"))
    elif status == "behind":
        if strict_base:
            problems.append(Problem(CANDIDATE_BEHIND,
                                    "the candidate is behind its base and this branch requires"
                                    " branches to be current before merging"))
    elif status == "draft":
        # Reached only when the forge says draft and the record's own isDraft did not, which is a
        # disagreement worth reporting rather than resolving in favour of either side.
        if not candidate.get("isDraft"):
            problems.append(Problem(CANDIDATE_DRAFT,
                                    "the forge reports this candidate as a draft although the"
                                    " record says it is not"))
    else:
        problems.append(Problem(
            CANDIDATE_UNKNOWN,
            "the forge reports merge state " + repr(candidate.get("mergeStateStatus"))
            + ", which is not a state this rule recognises; an unrecognised merge state is"
            " unknown rather than clean, because a forge that adds one must not acquire a"
            " passing verdict by default"))
    return problems
