"""The CXC report vocabulary this relay accepts, and the promotion it refuses.

CXC owns the dispatch lifecycle, the PABCD state machine and the provider retry ladder.
None of that is rebuilt here and none of its prose is copied: every rule below points at
the installed skill that owns it, so a version bump is re-read rather than trusted from a
transcription that has quietly gone stale.

What this module owns is the seam. Both systems have a word for how an execution ended,
and where the two vocabularies touch, one of them has to say what the other one means.
The dangerous overlap is "verified". CXC glosses DONE as verified success, meaning the
child proved its own recorded criteria. The relay's "verified" is the PARENT's
disposition, recorded through a verdict with criteria coverage and an acknowledged event.
Same English word, two different authorities, and nothing here may let the first become
the second.

A CXC report status is therefore recorded BESIDE a relay outcome, never used to derive
one. The outcome still comes from the child's receipt, which is the only thing the frozen
contract lets assert it. That keeps the original status and its reason intact, adds no
value to any frozen enum, and gives NOOP somewhere honest to go instead of being forced
into a failure it is not.
"""

from dataclasses import dataclass

from .errors import ReceiptRefused, RefusalReason

PACKAGE = "codexclaw"
VERSION = "0.2.28+codex.20260914090142"


@dataclass(frozen=True)
class Source:
    """One rule, where it lives, and what the file said when we read it.

    The digest is what makes this a pointer rather than a copy. When the installed payload
    changes, the digest stops matching and the mapping it supports is re-derived from the
    file instead of being assumed to still hold.
    """

    rule: str
    path: str
    anchor: str
    sha256: str


SOURCES = (
    Source(
        "report-outcomes-are-not-phases", "skills/loop/SKILL.md", "146-148",
        "61167d152d3c84f01b4d05ccb7ded5457db07ec42a84ae85e911eb75a44e5e2b",
    ),
    Source(
        "terminal-state-vocabulary", "skills/pabcd/references/loop-engineering.md", "32-40",
        "c5fb8c4e9fdd7dd9ce675a712cc9eca5b8e21301e8bfe49f7e27a9185db67fc7",
    ),
    Source(
        "REVIEW-SYNTHESIS-01", "skills/pabcd/references/loop-engineering.md", "41-60",
        "c5fb8c4e9fdd7dd9ce675a712cc9eca5b8e21301e8bfe49f7e27a9185db67fc7",
    ),
    Source(
        "DISPATCH-TASK-01", "skills/pabcd/references/delegation.md", "20-22",
        "ba773dbd5bbe3fd975bfca9f149205907ec9ca4c5a24bd2ce3c86d52d0c578b2",
    ),
    Source(
        "plan-output-nine-concepts", "skills/pabcd/references/plan-output.md", "7-20",
        "4535762ade768fad5aafb121e9c3b44b7cf6df17eb6b2a04cbfaa2dc366c7868",
    ),
    Source(
        "REVIEW-OUTPUT-01", "skills/dev-code-reviewer/SKILL.md", "86-95",
        "1d66f37c7754d852931bcf958063f30b2ab261a583ec080d280daf912e5d2d28",
    ),
    Source(
        "ATTEST-EVIDENCE-01", "skills/pabcd/references/phase-control.md", "45-48",
        "6982abddc1ceb52d046403b2a16a01ea22fab3fc30900d20c26a04a35a8b476a",
    ),
    Source(
        "LOOP-WAIT-EVIDENCE-01", "skills/loop/references/waiting.md", "31-63",
        "988451dee609d3949fa21f68fb2453a210f8fbbd06819db4219e710ab15150c8",
    ),
    Source(
        "DISPATCH-SURFACE-01", "skills/pabcd/references/dispatch-surfaces.md", "10-20",
        "dd3e6721718f05a45ac0031d861823441bba15592640dbe7785cae6611c26e98",
    ),
)


def provenance() -> dict:
    """What was read, from where, at which version.

    A work report stores the version string only. The digests live here, pinned in source,
    because copying nine of them onto every row would store the same constant many times and
    still not prove anything a reader could not get from this function.
    """
    return {
        "package": PACKAGE,
        "version": VERSION,
        "sources": [
            {"rule": s.rule, "path": s.path, "anchor": s.anchor, "sha256": s.sha256}
            for s in SOURCES
        ],
    }


def sources_for(rule: str) -> tuple:
    return tuple(s for s in SOURCES if s.rule == rule)


def affected_by(changed_paths) -> tuple:
    """Which rules a changed install actually touches.

    A version bump does not invalidate the whole mapping, and treating it as though it did
    is how a re-verification becomes expensive enough to skip. Only the rules whose files
    moved need re-reading, and only the scenarios resting on those rules need re-running.
    """
    changed = set(changed_paths)
    return tuple(sorted({s.rule for s in SOURCES if s.path in changed}))


# ------------------------------------------------------------------ report status

DONE = "DONE"
NOOP = "NOOP"
BLOCKED = "BLOCKED"
UNSAFE = "UNSAFE"
NEEDS_HUMAN = "NEEDS_HUMAN"
BUDGET_EXHAUSTED = "BUDGET_EXHAUSTED"

REPORT_STATUSES = (DONE, NOOP, BLOCKED, UNSAFE, NEEDS_HUMAN, BUDGET_EXHAUSTED)

# What each one means, in the relay's own words, so a recipient reading a collapsed mapping
# can still tell BLOCKED from UNSAFE from NEEDS_HUMAN.
MEANING = {
    DONE: "the child proved every recorded criterion against its own work",
    NOOP: "nothing needed doing, and the finding that established that is the deliverable",
    BLOCKED: "an external dependency is in the way",
    UNSAFE: "a human risk decision is required before this can proceed",
    NEEDS_HUMAN: "a judgment only the user can make",
    BUDGET_EXHAUSTED: "a bound the plan actually stated ran out; best-so-far is adopted",
}

# The relay outcomes each status may accompany. This is a COMPATIBILITY relation, not a
# derivation: the outcome is asserted by the child's receipt and checked against the status
# here. Three statuses collapse onto blocked_needs_input because the frozen outcome enum has
# one slot for "a human has to decide something" and inventing a second is not available to
# this layer. Nothing is lost, because the status and its reason are stored alongside.
COMPATIBLE_OUTCOMES = {
    DONE: ("ready_for_review",),
    NOOP: ("ready_for_review",),
    BLOCKED: ("blocked_needs_input",),
    UNSAFE: ("blocked_needs_input",),
    NEEDS_HUMAN: ("blocked_needs_input",),
    BUDGET_EXHAUSTED: ("interrupted", "failed"),
}


def check_known(status: str) -> None:
    """Is this a status this build understands, without pairing it to a receipt outcome.

    The parent-to-child direction has no child receipt to pair with: a revision request is
    the parent stating a judgment, not a child asserting how its execution ended. Reusing the
    compatibility check there would force the caller to invent an outcome, and an invented
    outcome is exactly what the rest of this package refuses.
    """
    if not isinstance(status, str) or status not in COMPATIBLE_OUTCOMES:
        raise ReceiptRefused(
            RefusalReason.OUTCOME_INCONSISTENT,
            f"{status!r} is not a CXC report status this build maps. Accepted: "
            f"{', '.join(REPORT_STATUSES)}. Read against {PACKAGE} {VERSION}; a newer "
            "contract needs the mapping extended rather than the value guessed at",
        )


def check_status(status: str, outcome: str) -> None:
    """Refuse an unknown status by name, and an incompatible pair by both names.

    Silently defaulting an unrecognised status is the failure this exists to prevent: a
    newer CXC reporting a word this build has never seen must be diagnosed, not mapped to
    whichever neighbour happens to be first in a dict.
    """
    check_known(status)
    allowed = COMPATIBLE_OUTCOMES[status]
    if outcome not in allowed:
        raise ReceiptRefused(
            RefusalReason.OUTCOME_INCONSISTENT,
            f"a {status} report cannot accompany outcome {outcome!r}: {status} means "
            f"{MEANING[status]}, which this contract pairs with {', '.join(allowed)}",
        )


# --------------------------------------------------------------- the promotion ban

# Facts that look like verification and are not. Each one is something a child, a forge or
# a CI run can produce on its own; the relay's verified disposition is something only the
# parent produces, from inside its own turn, against registered criteria.
NOT_VERIFICATION = {
    "cxc_done": "a DONE report is the child proving its own criteria, not the parent's verdict",
    "cxc_report": "a child report is the child describing its own execution, not a verdict",
    "pull_request_opened": "an open pull request is a place to review, not a review",
    "review_pass": "a review PASS is one reviewer's judgment, not the parent's disposition",
    "required_checks_green": "a green required check is evidence for a verdict, not a verdict",
    "turn_completed": "a completed turn is the trigger to look, as protocol v1 section 2 says",
    "dispatched": "a dispatched delivery is not an acknowledgement and not a verification",
}


def refuse_promotion(fact: str) -> str:
    """Why this fact does not become a relay verdict. Every caller gets a reason, not a bool."""
    if fact not in NOT_VERIFICATION:
        raise KeyError(
            f"{fact!r} is not a recorded non-verification fact; add it with its reason "
            "rather than letting an unlisted fact through by omission"
        )
    return NOT_VERIFICATION[fact]


# ------------------------------------------------------------------- review verdict

PASS = "PASS"
GO_WITH_FIXES = "GO-WITH-FIXES"
FAIL = "FAIL"
VERDICT_KINDS = (PASS, GO_WITH_FIXES, FAIL)
PREFIX = "VERDICT: "
# A review with more blockers than this is not a review, and the count lands on a line the
# message cannot shorten, so an unbounded one made it unrenderable.
BLOCKERS_MAX = 9999


def verdict_line(kind: str, blockers=None) -> str:
    """The machine-scannable final line REVIEW-OUTPUT-01 fixes.

    GO-WITH-FIXES without a blocker count is a PASS wearing a hedge, and a blocker count on
    PASS or FAIL is a number nobody can act on. Both are refused rather than normalised.
    """
    if kind not in VERDICT_KINDS:
        raise ValueError(
            f"{kind!r} is not a review verdict; REVIEW-OUTPUT-01 fixes "
            f"{', '.join(VERDICT_KINDS)}"
        )
    if kind == GO_WITH_FIXES:
        if not isinstance(blockers, int) or isinstance(blockers, bool) or blockers < 1:
            raise ValueError(
                "GO-WITH-FIXES states how many blockers it is going ahead with; a count "
                "below one is a PASS and should say so"
            )
        if blockers > BLOCKERS_MAX:
            raise ValueError(
                f"a blocker count of {blockers} is past the point of being a review, and it "
                f"sits on a line the message cannot shorten; the limit is {BLOCKERS_MAX}"
            )
        return f"{PREFIX}{GO_WITH_FIXES} (blockers={blockers})"
    if blockers is not None:
        raise ValueError(f"a {kind} verdict carries no blocker count")
    return f"{PREFIX}{kind}"


def parse_verdict_line(line: str):
    """Read one back, or None. Used to tell a real verdict from prose that resembles one."""
    text = (line or "").strip()
    if not text.startswith(PREFIX):
        return None
    body = text[len(PREFIX):].strip()
    if body in (PASS, FAIL):
        return {"kind": body, "blockers": None}
    if body.startswith(GO_WITH_FIXES):
        rest = body[len(GO_WITH_FIXES):].strip()
        if rest.startswith("(blockers=") and rest.endswith(")"):
            digits = rest[len("(blockers="):-1]
            # The same ceiling the renderer applies, so both directions speak one language.
            if digits.isdigit() and 1 <= int(digits) <= BLOCKERS_MAX:
                return {"kind": GO_WITH_FIXES, "blockers": int(digits)}
    return None


def assert_reviewed(has_review: bool) -> None:
    """A verdict line belongs to a review. An ordinary progress notice may not wear one."""
    if not has_review:
        raise ValueError(
            "a verdict line states a review judgment; a progress or completion notice that "
            "renders one is dressing an update as a review"
        )


# ------------------------------------------------------------------------- waiting

PROGRESS = "progress"
SUSPECTED_STAGNATION = "suspected_stagnation"
CONFIRMED_FAILURE = "confirmed_failure"
UNOBSERVABLE = "unobservable"
INPUT_NEEDED = "input_needed"
TIMED_OUT = "timed_out"

WAIT_STATES = (
    PROGRESS, SUSPECTED_STAGNATION, CONFIRMED_FAILURE, UNOBSERVABLE, INPUT_NEEDED, TIMED_OUT,
)

# Nothing on this list is permission to run the work again. A re-run is a decision somebody
# makes from evidence; these four are the states in which that evidence does not exist yet.
NEVER_AUTHORISES_RERUN = (TIMED_OUT, SUSPECTED_STAGNATION, UNOBSERVABLE, INPUT_NEEDED)


def classify_wait(*, terminal_error=None, input_requested=False, advancing_evidence=False,
                  observable=True, stagnation_confirmed=False, timed_out=False) -> dict:
    """Five endings that a wait can have, kept apart.

    The ordering is the point. A terminal error is a failure whatever else is true; a
    request for input is not a failure at all; fresh advancing evidence outranks a clock.
    A bare timeout lands on its own state and stays there, because elapsed time is not on
    the list of things that establish anything, here or in reconciliation.
    """
    if terminal_error:
        return _wait(CONFIRMED_FAILURE, str(terminal_error))
    if input_requested:
        return _wait(INPUT_NEEDED, "the work is waiting on an answer, not failing")
    if advancing_evidence:
        return _wait(PROGRESS, "observations advanced since the last look")
    if not observable:
        return _wait(
            UNOBSERVABLE,
            "available observations establish neither progress nor failure; the gap is the "
            "finding",
        )
    if stagnation_confirmed:
        return _wait(
            CONFIRMED_FAILURE, "no advancement at the stated review point, compared against "
            "the prior observation",
        )
    if timed_out:
        return _wait(
            TIMED_OUT,
            "the wait ended on its own bound. That is a normal outcome: it is neither a "
            "failure nor permission to start the work again",
        )
    return _wait(SUSPECTED_STAGNATION, "comparable observations show no advancement yet")


def _wait(state: str, reason: str) -> dict:
    return {
        "state": state,
        "reason": reason,
        "authorisesRerun": False,
        "isFailure": state == CONFIRMED_FAILURE,
    }


# ------------------------------------------------------------------ packet vocabulary

# DISPATCH-TASK-01. An instruction that omits one of these is an instruction the recipient
# has to guess at, which is the whole failure this shape prevents.
DISPATCH_FIELDS = ("TASK", "SCOPE", "MUST DO", "MUST NOT", "PROOF", "RETURN FORMAT")
DECISION_BOUNDARY = "DECISION BOUNDARY"

# Which owner to re-read when a message resumes work, rather than reloading everything.
SKILL_POINTERS = {
    "loop": "codexclaw:cxc-loop with codexclaw:cxc-pabcd",
    "development": "codexclaw:cxc-dev",
    "pull-request": "codexclaw:cxc-dev references/stacked-prs.md",
    "review-repair": "REVIEW-SYNTHESIS-01, cxc-pabcd references/loop-engineering.md 11.3",
    "lost-context": "codexclaw:cxc-recall",
}


def skill_pointer(activity: str) -> str:
    if activity not in SKILL_POINTERS:
        raise KeyError(
            f"{activity!r} has no recorded owner; name the owning skill rather than sending "
            "a recipient to reload everything"
        )
    return SKILL_POINTERS[activity]
