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
