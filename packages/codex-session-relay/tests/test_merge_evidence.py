"""The predicates in mergeevidence must answer exactly as the landed merge turn does.

CRW-128 needs the same review and checks rules on both sides of the handoff: the child
applies them when it records a ready-for-merge report, and the parent applies them again
when it restates the candidate to begin_merge. Two implementations of one rule drift, and
the drift is invisible because each side stays green on its own tests.

So the corpus below is driven through BOTH, and the answers are compared. A change to either
side that the other does not receive fails here.
"""

import pytest

from codex_session_relay import mergeevidence
from codex_session_relay.mergeturn import MergeTurn
from codex_session_relay import report
from codex_session_relay.errors import ReceiptRefused, RefusalReason

import json
import pathlib

ORACLE = json.loads(
    (pathlib.Path(__file__).parent / "fixtures" / "merge_turn_oracle.json")
    .read_text(encoding="utf-8")
)

ROW = {"target_key": "owner/repo@dev"}
ACTOR = "task-parent"
HEAD = "c68be165ae8ee4a645f3266eae3e9c543a851382"
OTHER = "0000000000000000000000000000000000000000"


def _clean_review(**overrides):
    review = {
        "hasNextPage": False, "pagesRead": 1, "totalCount": 2,
        "threadsSeen": ["t1", "t2"], "unresolved": 0,
    }
    review.update(overrides)
    return review


def _run(name, head=HEAD, conclusion="success", attempt=1, run_id=None):
    return {"runId": run_id or ("run-" + name), "name": name, "headSha": head,
            "conclusion": conclusion, "attempt": attempt}


REVIEW_CORPUS = [
    ("all clear", _clean_review()),
    ("hasNextPage unstated", {k: v for k, v in _clean_review().items() if k != "hasNextPage"}),
    ("pagesRead unstated", {k: v for k, v in _clean_review().items() if k != "pagesRead"}),
    ("totalCount unstated", {k: v for k, v in _clean_review().items() if k != "totalCount"}),
    ("threadsSeen unstated", {k: v for k, v in _clean_review().items() if k != "threadsSeen"}),
    ("unresolved unstated", {k: v for k, v in _clean_review().items() if k != "unresolved"}),
    ("nothing stated at all", {}),
    ("still paginating", _clean_review(hasNextPage=True)),
    ("no page read", _clean_review(pagesRead=0)),
    ("blank identifier", _clean_review(threadsSeen=["t1", "   "])),
    ("duplicated identifier", _clean_review(threadsSeen=["t1", "t1"], totalCount=2)),
    ("count disagrees", _clean_review(totalCount=14)),
    ("threads unresolved", _clean_review(unresolved=14)),
    ("several at once", _clean_review(hasNextPage=True, pagesRead=0, unresolved=3)),
]

CHECKS_CORPUS = [
    ("all clear", ["dev-gate"], [_run("dev-gate")]),
    ("nothing restated", ["dev-gate"], []),
    ("nothing restated and nothing required", [], []),
    ("entry without a runId", ["dev-gate"], [{"runId": "  ", "name": "dev-gate",
                                              "headSha": HEAD, "conclusion": "success",
                                              "attempt": 1}]),
    ("entry without a name", ["dev-gate"], [{"runId": "run-1", "name": "",
                                             "headSha": HEAD, "conclusion": "success",
                                             "attempt": 1}]),
    ("entry about another head", ["dev-gate"], [_run("dev-gate", head=OTHER)]),
    ("required check failed", ["dev-gate"], [_run("dev-gate", conclusion="failure")]),
    ("newest attempt failed after an older pass", ["dev-gate"], [
        _run("dev-gate", conclusion="success", attempt=1, run_id="run-1"),
        _run("dev-gate", conclusion="failure", attempt=2, run_id="run-1"),
    ]),
    ("older attempt failed and newest passed", ["dev-gate"], [
        _run("dev-gate", conclusion="failure", attempt=1, run_id="run-1"),
        _run("dev-gate", conclusion="success", attempt=2, run_id="run-1"),
    ]),
    ("required name absent from the set", ["dev-gate"], [_run("lint")]),
    ("optional check failing beside a green required one", ["dev-gate"], [
        _run("dev-gate"), _run("lint", conclusion="failure"),
    ]),
    ("nothing required and nothing succeeded", [], [_run("lint", conclusion="failure")]),
    ("nothing required and something succeeded", [], [_run("lint")]),
]


@pytest.mark.parametrize("label,review", REVIEW_CORPUS, ids=[c[0] for c in REVIEW_CORPUS])
def test_review_predicate_matches_the_landed_merge_turn(label, review):
    landed = MergeTurn._review_refusal(ROW, ACTOR, dict(review))
    problems = mergeevidence.details(mergeevidence.review_problems(dict(review)))
    if landed is None:
        assert problems == [], label
    else:
        assert problems, label
        assert landed.detail == "; ".join(problems), label


@pytest.mark.parametrize("label,required,checks", CHECKS_CORPUS,
                         ids=[c[0] for c in CHECKS_CORPUS])
def test_checks_predicate_matches_the_landed_merge_turn(label, required, checks):
    landed = MergeTurn._check_refusal(ROW, ACTOR, HEAD, sorted(required),
                                      [dict(entry) for entry in checks])
    problems = mergeevidence.details(mergeevidence.checks_problems(
        HEAD, sorted(required), [dict(entry) for entry in checks]))
    if landed is None:
        assert problems == [], label
    else:
        assert len(problems) == 1, label
        assert landed.detail == problems[0], label


def test_the_corpus_exercises_both_outcomes_on_both_predicates():
    """A corpus that only ever refuses, or only ever passes, proves nothing about agreement."""
    review_clear = [r for _, r in REVIEW_CORPUS if not mergeevidence.review_problems(dict(r))]
    review_refused = [r for _, r in REVIEW_CORPUS if mergeevidence.review_problems(dict(r))]
    checks_clear = [c for _, q, c in CHECKS_CORPUS
                    if not mergeevidence.checks_problems(HEAD, sorted(q), c)]
    checks_refused = [c for _, q, c in CHECKS_CORPUS
                      if mergeevidence.checks_problems(HEAD, sorted(q), c)]
    assert review_clear and review_refused
    assert checks_clear and checks_refused


def test_an_unstated_field_is_reported_instead_of_the_value_checks():
    """R0: absence read as satisfied is the hole this closes, so it must not be diluted.

    A record missing unresolved is missing the answer, not answering zero. Reporting the
    value checks alongside would invite a producer to fix the visible complaint and leave
    the unstated field exactly as it was.
    """
    problems = mergeevidence.details(
        mergeevidence.review_problems({"hasNextPage": True, "pagesRead": 0}))
    assert problems == [
        "the review record does not state totalCount",
        "the review record does not state threadsSeen",
        "the review record does not state unresolved",
    ]


def test_a_record_that_says_nothing_does_not_pass():
    """The exact EQP-29 shape at the vector level: silence is not a pass."""
    assert len(mergeevidence.review_problems({})) == len(mergeevidence.REVIEW_FIELDS)
    assert mergeevidence.review_problems(None)


def test_fourteen_unresolved_threads_are_refused():
    """EQP-29 itself: a fully enumerated review with fourteen open threads is not ready."""
    review = _clean_review(totalCount=14, threadsSeen=["t%d" % n for n in range(14)],
                           unresolved=14)
    assert mergeevidence.details(
        mergeevidence.review_problems(review)) == ["14 threads are unresolved"]


SINGLE_FAULT_REVIEWS = [
    ("still paginating", _clean_review(hasNextPage=True)),
    ("no page read", _clean_review(pagesRead=0)),
    ("blank identifier", _clean_review(threadsSeen=["t1", "   "], totalCount=1)),
    ("duplicated identifier", _clean_review(threadsSeen=["t1", "t1"], totalCount=1)),
    ("count disagrees", _clean_review(totalCount=14)),
    ("threads unresolved", _clean_review(unresolved=14)),
]


@pytest.mark.parametrize("label,review", SINGLE_FAULT_REVIEWS,
                         ids=[c[0] for c in SINGLE_FAULT_REVIEWS])
def test_each_review_rule_fires_alone(label, review):
    """A corpus of composite failures cannot tell a missing rule from a redundant one.

    Two implementations both saying "refused" agree on nothing in particular when the input
    breaks three rules at once: drop one check from either side and the verdict is unchanged.
    So every rule needs an input that activates IT and nothing else, and the assertion is on
    the problem list rather than on refusal as a boolean.
    """
    problems = mergeevidence.details(mergeevidence.review_problems(review))
    assert len(problems) == 1, (label, problems)
    landed = MergeTurn._review_refusal(ROW, ACTOR, dict(review))
    assert landed is not None and landed.detail == problems[0], label


def test_a_masked_rule_would_be_caught():
    """The reviewer's hole, closed: an unstated field alongside a value fault.

    R0 short-circuits, so this input refuses for the absence alone. If it were the only
    coverage of unresolved, a side that dropped the unresolved check would still look
    equivalent. The single-fault case above is what actually pins that rule; this asserts the
    short-circuit really does suppress the value complaint rather than merging the two.
    """
    problems = mergeevidence.details(mergeevidence.review_problems(
        {"hasNextPage": False, "totalCount": 1, "threadsSeen": ["t1"], "unresolved": 1}))
    assert problems == ["the review record does not state pagesRead"]


def test_an_undeclared_required_set_is_a_child_side_refusal():
    """Empty required means 'none required' to a merge turn and 'I did not look' to a child.

    The concrete false pass: a failing dev-gate beside a passing lint satisfies "something
    succeeded on this head" when nothing was declared required, so the child would hand over
    a red candidate. The merge turn keeps its existing meaning; only the child asks for the
    declaration.
    """
    red = [_run("dev-gate", conclusion="failure"), _run("lint", conclusion="success")]
    assert mergeevidence.checks_problems(HEAD, [], red) == []
    assert MergeTurn._check_refusal(ROW, ACTOR, HEAD, [], [dict(e) for e in red]) is None
    child = mergeevidence.details(mergeevidence.checks_problems(
        HEAD, mergeevidence.UNDECLARED, red, require_declared=True))
    assert len(child) == 1 and "does not state which checks" in child[0]
    # An explicitly empty declaration is a different statement and keeps its meaning.
    assert mergeevidence.checks_problems(HEAD, [], red, require_declared=True) == []
    declared = mergeevidence.details(mergeevidence.checks_problems(
        HEAD, ["dev-gate"], red, require_declared=True))
    assert len(declared) == 1 and "dev-gate" in declared[0]


def test_declaring_the_required_names_still_passes_a_green_candidate():
    green = [_run("dev-gate"), _run("lint")]
    assert mergeevidence.checks_problems(HEAD, ["dev-gate"], green,
                                         require_declared=True) == []


SHAPE_CORPUS = [
    ("threadsSeen as a string counts characters",
     _clean_review(threadsSeen="ab", totalCount=2), [], "threadsSeen is a list"),
    ("review is not an object", "ready", [], "the review record is an object"),
    ("a count is not a number", _clean_review(unresolved="none"), [], "unresolved is a whole"),
    ("a count given as a boolean", _clean_review(pagesRead=True), [], "pagesRead is a whole"),
    ("checks is not a list", _clean_review(), "dev-gate", "the restated checks are a list"),
    ("a check entry is not an object", _clean_review(), ["dev-gate"], "check entry 0 is an"),
    ("attempt is not a number", _clean_review(),
     [_run("dev-gate", attempt="newest")], "which attempt is newest"),
]


@pytest.mark.parametrize("label,review,checks,fragment", SHAPE_CORPUS,
                         ids=[c[0] for c in SHAPE_CORPUS])
def test_shape_is_checked_before_any_rule_reads_a_value(label, review, checks, fragment):
    problems = mergeevidence.details(mergeevidence.shape_problems(review, checks))
    assert any(fragment in problem for problem in problems), (label, problems)


def test_a_well_formed_payload_has_no_shape_problem():
    assert mergeevidence.shape_problems(_clean_review(), [_run("dev-gate")]) == []


def test_the_string_threads_seen_would_otherwise_have_passed():
    """Why shape runs first: this payload satisfies every semantic rule while enumerating
    nothing, because a two-character string is two 'threads'."""
    sneaky = _clean_review(threadsSeen="ab", totalCount=2)
    assert mergeevidence.review_problems(sneaky) == []
    assert mergeevidence.shape_problems(sneaky, []) != []


BASE = "c56576d5be412b5bc352dd93b9eb37ab279a12f6"


def _ready_handoff(**overrides):
    base = {
        "isDraft": False,
        "baseVerifiedAt": "2026-09-21T02:00:00Z",
        "requiredDeclared": ["dev-gate"],
        "checks": [_run("dev-gate")],
        "reviewCoverage": _clean_review(totalCount=1, threadsSeen=["t1"]),
        "threadDispositions": [{"threadId": "t1", "disposition": "fixed",
                                "evidence": "fixed and rechecked", "addressedBy": "abc1234"}],
    }
    base.update(overrides)
    return base


@pytest.mark.parametrize("outcome", ["blocked_needs_input", "interrupted", "failed"])
def test_an_unfinished_turn_may_still_name_its_pull_request(outcome):
    """Blocked is reported as blocked, which means it has to be reportable.

    Requiring a complete handoff from every report naming a pull request made the honest
    outcome the only one a child could not send: a turn that stopped because the review was
    not finished names its pull request too, and demanding finished-review evidence from it
    would leave lying as the only way to report.
    """
    assert report._check_handoff(None, 12, HEAD, BASE, outcome) is None


def test_a_readiness_claim_naming_a_pull_request_may_not_stay_silent():
    with pytest.raises(ReceiptRefused) as caught:
        report._check_handoff(None, 12, HEAD, BASE, "ready_for_review")
    assert caught.value.reason is RefusalReason.MERGE_EVIDENCE_REQUIRED


def test_a_report_with_no_pull_request_needs_no_handoff():
    assert report._check_handoff(None, None, HEAD, BASE, "ready_for_review") is None


def test_the_parent_is_given_a_base_it_can_compare():
    """The parent's job is to restate a base and compare it, so half a comparison is refused."""
    with pytest.raises(ReceiptRefused) as undated:
        report._check_handoff(_ready_handoff(baseVerifiedAt=None), 12, HEAD, BASE,
                              "ready_for_review")
    assert undated.value.reason is RefusalReason.MERGE_EVIDENCE_REQUIRED
    with pytest.raises(ReceiptRefused) as unnamed:
        report._check_handoff(_ready_handoff(), 12, HEAD, None, "ready_for_review")
    assert unnamed.value.reason is RefusalReason.MERGE_EVIDENCE_REQUIRED


def test_a_complete_handoff_is_accepted_and_normalised():
    accepted = report._check_handoff(_ready_handoff(), 12, HEAD, BASE, "ready_for_review")
    assert accepted["isDraft"] is False
    assert accepted["requiredDeclared"] == ["dev-gate"]
    assert [one["threadId"] for one in accepted["threadDispositions"]] == ["t1"]


def test_the_parent_sees_the_readiness_in_the_message_not_only_in_the_store():
    """A record written and never rendered is one the recipient has no reason to fetch."""
    lines = report._handoff_lines({
        "headSha": HEAD, "baseSha": BASE,
        "handoff": report._check_handoff(_ready_handoff(), 12, HEAD, BASE, "ready_for_review"),
    })
    rendered = chr(10).join(lines)
    assert "merge readiness" in rendered
    assert "0 unresolved" in rendered
    assert "dev-gate" in rendered
    assert report._handoff_lines({"headSha": HEAD, "baseSha": BASE}) == []


def test_a_minor_finding_a_parent_accepted_has_a_true_disposition_to_record():
    """CRW-25: the five older judgments could not say this, so the record had to be false.

    A real finding the owning parent decided not to fix now is not `not_applicable`, because
    it does apply, and not `disputed`, because nobody disputes it. With only those words
    available the child's choices were a false `fixed` or another round, and the gate
    counting unresolved threads made the false `fixed` the cheaper one.
    """
    handoff = _ready_handoff(threadDispositions=[{
        "threadId": "t1", "disposition": "accepted",
        "evidence": "wording residue in a comment; no criterion depends on it",
        "addressedBy": "parent task 01a0b406 accepted it on 2026-09-21",
        "followUp": "CRW-176 owns it; reopens if the wording reaches a criterion",
    }])
    recorded = report._check_handoff(handoff, 12, HEAD, BASE, "ready_for_review")
    assert recorded["threadDispositions"] == [{
        "threadId": "t1", "disposition": "accepted",
        "evidence": "wording residue in a comment; no criterion depends on it",
        "addressedBy": "parent task 01a0b406 accepted it on 2026-09-21",
        "followUp": "CRW-176 owns it; reopens if the wording reaches a criterion",
    }]


def test_an_acceptance_that_names_no_decision_is_refused_like_a_fix_with_no_commit():
    """An acceptance with nothing to point at reads like a judgment and contains none.

    This is the failure the new word would otherwise introduce: `accepted` is the easiest
    value to write and the hardest to check, so it carries `fixed`'s burden rather than
    becoming the blank that clears every thread.
    """
    for missing in (None, "", "   "):
        with pytest.raises(ReceiptRefused) as caught:
            report._check_handoff(_ready_handoff(threadDispositions=[{
                "threadId": "t1", "disposition": "accepted",
                "evidence": "minor and separable", "addressedBy": missing,
                "followUp": "CRW-176 owns it",
            }]), 12, HEAD, BASE, "ready_for_review")
        assert caught.value.reason is RefusalReason.MERGE_REVIEW_INCOMPLETE
        assert "parent decision" in str(caught.value)


def test_an_acceptance_that_leaves_nobody_holding_the_residue_is_refused():
    """A known defect with no owner is how it stops being anybody's.

    The five older values all describe something that is over. An acceptance describes
    something that is not, so the owner and the reopen trigger are the part that makes it a
    decision rather than an abandonment.
    """
    for missing in (None, "", "   "):
        with pytest.raises(ReceiptRefused) as caught:
            report._check_handoff(_ready_handoff(threadDispositions=[{
                "threadId": "t1", "disposition": "accepted",
                "evidence": "minor and separable",
                "addressedBy": "parent task 01a0b406 accepted it", "followUp": missing,
            }]), 12, HEAD, BASE, "ready_for_review")
        assert caught.value.reason is RefusalReason.MERGE_REVIEW_INCOMPLETE
        assert "follow-up" in str(caught.value)


def test_a_follow_up_on_anything_but_an_acceptance_is_refused():
    """If a fix could carry one, "there is a follow-up" would stop meaning anything."""
    with pytest.raises(ReceiptRefused) as caught:
        report._check_handoff(_ready_handoff(threadDispositions=[{
            "threadId": "t1", "disposition": "fixed", "evidence": "fixed and rechecked",
            "addressedBy": "abc1234", "followUp": "CRW-176 owns the rest",
        }]), 12, HEAD, BASE, "ready_for_review")
    assert caught.value.reason is RefusalReason.MALFORMED_RECEIPT


def test_every_acceptance_reaches_the_parent_that_would_know_it_never_decided_it():
    """Nothing authenticates "the parent accepted this", so it is shown, not counted.

    An acceptance is the one disposition that legitimises a defect the candidate still
    carries. Left in the store it is a row the parent has no reason to fetch, and a forged
    one then clears the gate silently. Rendered, it lands in front of the only party who can
    recognise whether the decision happened, during the restatement it performs anyway.
    """
    lines = report._handoff_lines({
        "headSha": HEAD, "baseSha": BASE,
        "handoff": report._check_handoff(_ready_handoff(threadDispositions=[{
            "threadId": "t1", "disposition": "accepted",
            "evidence": "wording residue; no criterion depends on it",
            "addressedBy": "parent task 01a0b406 accepted it on 2026-09-21",
            "followUp": "CRW-176 owns it",
        }]), 12, HEAD, BASE, "ready_for_review"),
    })
    rendered = chr(10).join(lines)
    assert "confirm each was yours" in rendered
    assert "t1" in rendered
    assert "parent task 01a0b406" in rendered
    assert "CRW-176" in rendered
    # A candidate with nothing accepted says nothing about acceptances.
    plain = chr(10).join(report._handoff_lines({
        "headSha": HEAD, "baseSha": BASE,
        "handoff": report._check_handoff(_ready_handoff(), 12, HEAD, BASE, "ready_for_review"),
    }))
    assert "confirm each was yours" not in plain
def test_resolving_a_thread_is_still_not_among_the_judgments():
    """Adding a word to the enum must not turn it into a place to put the button."""
    with pytest.raises(ReceiptRefused) as caught:
        report._check_handoff(_ready_handoff(threadDispositions=[{
            "threadId": "t1", "disposition": "resolved", "evidence": "closed the thread",
        }]), 12, HEAD, BASE, "ready_for_review")
    assert caught.value.reason is RefusalReason.MERGE_REVIEW_INCOMPLETE


def test_an_omitted_attempt_is_not_evidence_that_this_one_is_newest():
    """Defaulting to 1 let an omission stand for a fact.

    An older successful dev-gate submitted with no attempt reads as the first one, and the
    failing newest attempt this rule exists to catch is simply never mentioned. The entry is
    refused for not saying, rather than assumed to be first.
    """
    entry = {"runId": "run-1", "name": "dev-gate", "headSha": HEAD, "conclusion": "success"}
    problems = mergeevidence.details(mergeevidence.shape_problems(_clean_review(), [entry]))
    assert len(problems) == 1 and "does not state attempt" in problems[0]
    assert mergeevidence.shape_problems(_clean_review(), [dict(entry, attempt=1)]) == []


@pytest.mark.parametrize("name", ["dev-gate\ud800", "dev" + chr(10) + "gate", "g" * 400])
def test_a_required_check_name_that_cannot_be_rendered_is_refused_when_recorded(name):
    """Every required name is rendered into the completion message.

    Rendering happens inside the delivery claim, so a name that cannot be encoded or that
    breaks the line fails there instead: the claim rolls back and the delivery never goes out.
    A shape that cannot be rendered is refused where the producer can still fix it.
    """
    handoff = _ready_handoff(requiredDeclared=[name],
                             checks=[_run(name)],
                             reviewCoverage=_clean_review(totalCount=1, threadsSeen=["t1"]))
    with pytest.raises(ReceiptRefused):
        report._check_handoff(handoff, 12, HEAD, BASE, "ready_for_review")


@pytest.mark.parametrize("value", ["not-a-date", "2026-09-21T02:00:00", "", "   ", None, 17])
def test_a_base_verification_time_that_is_not_a_time_is_refused(value):
    """Presence is not a time, and a naive stamp does not say which clock it came from."""
    with pytest.raises(ReceiptRefused) as caught:
        report._check_handoff(_ready_handoff(baseVerifiedAt=value), 12, HEAD, BASE,
                              "ready_for_review")
    assert caught.value.reason is RefusalReason.MERGE_EVIDENCE_REQUIRED


@pytest.mark.parametrize("value", ["2026-09-21T02:00:00Z", "2026-09-21T02:00:00+00:00",
                                   "2026-09-21T11:00:00+09:00"])
def test_an_offset_bearing_timestamp_is_accepted_and_normalised(value):
    accepted = report._check_handoff(_ready_handoff(baseVerifiedAt=value), 12, HEAD, BASE,
                                     "ready_for_review")
    assert accepted["baseVerifiedAt"].endswith("+00:00") or "+09:00" in accepted["baseVerifiedAt"]


def _observable(refusal):
    if refusal is None:
        return None
    return [refusal.reason.value, refusal.detail, refusal.incumbent, refusal.challenger,
            refusal.domain, refusal.subject]


@pytest.mark.parametrize("label,expected", ORACLE["review"],
                         ids=[row[0] for row in ORACLE["review"]])
def test_review_refusal_matches_the_frozen_oracle(label, expected):
    """The oracle was captured from the landed implementation BEFORE it delegated.

    Comparing mergeturn against mergeevidence stopped proving anything the moment mergeturn
    started calling mergeevidence: a function equals itself. What survives delegation is a
    table of answers recorded while the two were still independent, so these are literal
    expected values rather than a live comparison.

    It pins the whole observable result, not the verdict. incumbent, challenger, domain and
    subject reach the conflict ledger through Refusal.to_record, so a delegation that dropped
    one would change a stored record while every pass/refuse assertion stayed green.
    """
    review = dict(dict(REVIEW_CORPUS)[label])
    assert _observable(MergeTurn._review_refusal(ROW, ACTOR, review)) == expected


@pytest.mark.parametrize("label,expected", ORACLE["checks"],
                         ids=[row[0] for row in ORACLE["checks"]])
def test_check_refusal_matches_the_frozen_oracle(label, expected):
    required, checks = next((q, c) for lab, q, c in CHECKS_CORPUS if lab == label)
    observed = MergeTurn._check_refusal(
        ROW, ACTOR, HEAD, sorted(required), [dict(entry) for entry in checks])
    assert _observable(observed) == expected


def test_the_oracle_records_both_outcomes_and_every_incumbent_kind():
    """An oracle of all-None rows would pass against an implementation that never refuses."""
    rows = [row[1] for row in ORACLE["review"] + ORACLE["checks"]]
    assert any(row is None for row in rows)
    assert any(row is not None for row in rows)
    incumbents = {row[2] for row in rows if row is not None}
    assert "" in incumbents and any(one for one in incumbents)
