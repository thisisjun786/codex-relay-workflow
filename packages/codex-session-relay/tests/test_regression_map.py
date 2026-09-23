"""The regression map's own inventories, derived from source rather than asserted in prose.

Two independent review rounds on the plan behind this work re-opened the same two
categories: evidence claimed as new that already existed, and clock provenance stated
wrongly. Patching the instances a third time would have been the third round of one
mistake. This package already has the answer to that shape - guard.EVALUATION_STAGES is
checked against the injections that exercise it, so a stage added without being listed
fails here rather than in review - and this applies it to the map.

What this closes: a citation that no longer resolves, a criterion whose clock label
disagrees with the modules it names, a count the map prints that nothing reads, and - added
after the sweep this file backs turned out to be narrower than its own sentence - the reach of
that sweep itself. The boolean-summary inventory below is the third of these derived lists and
the largest: every boolean the package folds out of more than one input, every place the suite
measures something with one, and a written verdict beside each of those places.

What it does NOT close, stated so nobody reads more into a green run than is there. Whether a
new test asserts something an existing test already asserts: no scan can answer that, and the
map's reuse column is where that judgement is written down rather than enforced. And whether
any verdict below is the RIGHT reading of its site: the scan supplies the reach and which value
an assertion pins, a person supplies the verdict, and a wrong verdict fails nothing here. Two
of them were wrong when this landed and review caught both by mutation rather than by reading.
"""

import ast
import pathlib
import re
import textwrap
import unittest

from .support import written_table

TESTS = pathlib.Path(__file__).resolve().parent
MAP = TESTS.parent / "docs" / "contention-regression.md"

# Modules that read a real clock or start a process, so they spend wall time rather than
# moving an injected one. Derived below and compared against this tuple; the reasons are in
# the map. A new test that waits on anything real fails here until it is named.
REAL_TIME_MODULES = (
    "test_bridge_adapter.py",
    "test_cli.py",
    "test_daemon_cadence.py",
    "test_dispositions.py",
    "test_failure_recovery.py",
    "test_launch_policy.py",
    "test_management_cli.py",
    "test_managed_start.py",
    "test_operational_scale.py",
    "test_reporting_cli.py",
    "test_service.py",
    "test_stop_adapter.py",
    "test_worker_policy.py",
    "test_wp1_regressions.py",
)

CLOCK_NAMES = {"sleep", "monotonic", "time", "perf_counter"}
SUBPROCESS_NAMES = {"Popen", "run", "check_output", "call"}
# Spelled rather than typed, because the map is Markdown and a code span delimiter inside a
# pattern here would be one more thing to escape in two languages at once.
TICK = "\u0060"
MODULE = re.compile(TICK + r"(test_[a-z0-9_]+\.py)" + TICK)
SPANNED = re.compile(TICK + "[^" + TICK + "]*" + TICK)
CLASS = re.compile(r"\b([A-Z][A-Za-z0-9]+)\b")


def criterion_rows():
    """The criterion table, as (number, reused cell, new cell, clock cell)."""
    rows = []
    for line in MAP.read_text(encoding="utf-8").splitlines():
        if not line.startswith("|"):
            continue
        cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
        if len(cells) == 5 and cells[0].isdigit():
            rows.append((int(cells[0]), cells[2], cells[3], cells[4]))
    return rows


def spends_real_time(path):
    """Why this module spends wall time, or an empty set if it only moves an injected clock.

    The boundary is deliberate: reading a clock or starting a process is what produces
    elapsed-time evidence, and that is what the map's column is about. A barrier, a thread
    join or a lock acquisition blocks until another thread arrives rather than until a
    duration passes, so it synchronises without measuring. A test that did assert on how long
    one of them took would have to read a clock, and would be caught here. Modules that only
    synchronise stay injected, and the map records the boundary rather than leaving it to be
    inferred from this function.
    """
    reasons = set()
    for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
        if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)):
            continue
        function = node.func
        owner = function.value.id if isinstance(function.value, ast.Name) else None
        if owner == "time" and function.attr in CLOCK_NAMES:
            reasons.add("time." + function.attr)
        if owner == "subprocess" and function.attr in SUBPROCESS_NAMES:
            reasons.add("subprocess." + function.attr)
        if function.attr == "spawn_worker":
            reasons.add("spawn_worker")
    return reasons


class TheMapCitesOnlyEvidenceThatExists(unittest.TestCase):
    def setUp(self):
        self.rows = criterion_rows()
        self.assertTrue(self.rows, "no criterion table was found in " + str(MAP))

    def classes_in(self, name):
        path = TESTS / name
        self.assertTrue(path.exists(), "the map names " + name + ", which is not in this suite")
        return {
            node.name
            for node in ast.walk(ast.parse(path.read_text(encoding="utf-8")))
            if isinstance(node, ast.ClassDef)
        }

    def test_every_reused_class_is_a_class_in_the_module_it_is_attributed_to(self):
        checked = 0
        for number, reused, _new, _clock in self.rows:
            if reused.lower().startswith("none"):
                continue
            for segment in reused.split(";"):
                modules = MODULE.findall(segment)
                if not modules:
                    continue
                prose = SPANNED.sub(" ", segment)
                for name in CLASS.findall(prose):
                    self.assertIn(
                        name, self.classes_in(modules[0]),
                        f"criterion {number} reuses {modules[0]} {name}, which that module"
                        " does not define; the citation is stale",
                    )
                    checked += 1
        self.assertGreater(
            checked, 20, f"only {checked} citations were checked, so the parser stopped seeing"
            " the table it is supposed to be reading",
        )

    def test_every_module_the_map_names_exists(self):
        checked = 0
        for number, reused, new, _clock in self.rows:
            for cell in (reused, new):
                if cell.lower().startswith("none"):
                    continue
                for name in MODULE.findall(cell):
                    self.assertTrue(
                        (TESTS / name).exists(),
                        f"criterion {number} names {name}, which is not in this suite",
                    )
                    checked += 1
        # A loop over nothing asserts nothing. Every test in this module that walks the table
        # counts what it walked, because the failure mode being guarded against is the parser
        # silently matching no rows and the suite reporting that as agreement.
        self.assertGreater(checked, 12, f"only {checked} modules were checked")


class TheMapNamesEveryTestThatSpendsRealTime(unittest.TestCase):
    def test_the_declared_real_time_modules_are_exactly_the_modules_that_spend_real_time(self):
        derived = {
            path.name for path in sorted(TESTS.glob("test_*.py")) if spends_real_time(path)
        }
        declared = set(REAL_TIME_MODULES)
        self.assertEqual(
            derived, declared,
            "the map's real-time inventory and the source disagree."
            f" Only in source: {sorted(derived - declared)}."
            f" Only declared: {sorted(declared - derived)}."
            " Name the new one in REAL_TIME_MODULES and in the map, or stop it waiting.",
        )

    def test_a_criterion_naming_a_real_time_module_is_not_labelled_injected(self):
        """The check the first draft of this module was missing.

        A global module inventory can be perfectly correct while a criterion resting on one of
        those modules still says its evidence came off an injected clock. The label is per
        module, deliberately coarse: a criterion sharing a module with one real-time case
        inherits the label rather than claiming the half it likes.
        """
        real_time = set(REAL_TIME_MODULES)
        checked = 0
        for number, reused, new, clock in criterion_rows():
            named = set()
            for cell in (reused, new):
                if not cell.lower().startswith("none"):
                    named.update(MODULE.findall(cell))
            waiting = named & real_time
            checked += 1
            if waiting:
                self.assertNotEqual(
                    clock, "injected",
                    f"criterion {number} rests on {sorted(waiting)}, which spend real time,"
                    f" but its clock is recorded as {clock!r}",
                )
            else:
                self.assertEqual(
                    clock, "injected",
                    f"criterion {number} is recorded as {clock!r} but names no module that"
                    f" spends real time: {sorted(named)}",
                )
        self.assertGreater(checked, 6, f"only {checked} criterion rows were read")


# ---------------------------------------------------------- the sweep's own reach
#
# The map's sweep clause used to say five modules were checked. That is a claim about the
# sweeper. What follows produces the reach instead: which places in this suite measure a
# property with a boolean the production source folded, what each of those booleans folds,
# and - the part that matters - what this reader could NOT see, emitted as data rather than
# left to prose.
#
# What is derived and what is written down are different things, and conflating them would be
# the same overstatement. DERIVED: the inventory, the polarities, the blind spots. WRITTEN
# DOWN: whether the summary at a site means the same thing as the condition its test is named
# for. That is a reading judgement and no scan performs it; every site carries one.
#
# Boundaries, stated because a boundary nobody wrote down is how a sweep ends up claiming more
# than it reached:
#   - this package only, and only bool-annotated fields and bool-returning functions;
#   - matching is by name, so a local named like a summary function and then called counts;
#   - a value carried through an alias, a dict key or **kwargs is not followed;
#   - FOLD_FREE_BOOLEANS is declared by key alone, so it cannot show a constant producer being
#     swapped for another constant. It cannot hide a FOLD: a symbol that starts folding moves
#     lists and breaks the partition below. Listing those producers verbatim would copy
#     production SQL into this module and fail it on an unrelated query edit.

SUMMARIES = {
    # Replay equality folds entries, digest, source and mode; public tests independently
    # vary each input and assert that a stored set is preserved on refusal.
    ("criteria.py", None, "_same_registration", "function"): ((False,), (), ()),
    ("daemon.py", "TickReport", "quiet", "field"):
        ((False,), (("declaration default: True", 1),), ("notes", "skipped")),
    # A stated absence is a dict carrying one key, so the false side is reachable from either
    # input alone - anything that is not a mapping, and any mapping without the key.
    ("envelope.py", None, "is_absent", "function"): ((False,), (), ()),
    ("marker.py", None, "named", "function"): ((False,), (), ()),
    ("marker.py", None, "same_identity", "function"): ((False,), (), ()),
    ("receipts.py", None, "deliverable", "function"): ((False,), (), ()),
    ("scope.py", None, "is_within", "function"):
        ((True,), (("return: path.startswith('/')", 1),), ()),
    ("service.py", None, "_holder_is_ours", "function"):
        ((False,), (("return: False", 1), ("return: True", 1)), ()),
    ("service.py", None, "_is_live", "function"): ((True,), (("return: False", 4),), ()),
    ("service.py", None, "_worker_identified", "function"):
        ((False,), (("return: False", 3),), ()),
    ("service.py", None, "alive", "function"): ((False,), (), ()),
    # A name is a non-blank string, so the false side is reachable from either input alone:
    # anything that is not a string, and any string that is blank.
    ("supervision.py", None, "_named", "function"): ((False,), (), ()),
}

# Folds where the reduction rule cannot follow it, so the fold leaves a trace anyway.
# guard.receipt_matches is the case that forced this list to exist: four checks folded through
# early returns inside an if not (...), so no producer path reduces.
FOLDS_BEYOND_ITS_PATHS = (
    ("ack.py", None, "_re_review_open", "function"),
    ("ack.py", None, "_ruling_is_current", "function"),
    ("ack.py", None, "certainly_before", "function"),
    ("admission.py", "Admission", "admitted", "field"),
    ("assignment.py", None, "_criteria_current", "function"),
    ("bridge_adapter.py", None, "_scan_listing", "function"),
    ("bridge_adapter.py", None, "same_ledger", "function"),
    ("cli.py", None, "_reads_no_selected_store", "function"),
    ("daemon.py", None, "_already_observed", "function"),
    ("daemon.py", None, "_reads_were_complete", "function"),
    ("daemon.py", None, "_worth_polling", "function"),
    ("envelope.py", None, "stage_holds", "function"),
    ("guard.py", None, "receipt_matches", "function"),
    ("guard.py", None, "reserve_hold", "function"),
    ("hostadapter.py", "TokenScan", "exhausted", "field"),
    ("hostadapter.py", "TokenScan", "found", "field"),
    ("intent.py", None, "_ambiguity_resolved", "function"),
    ("intent.py", None, "correlated", "function"),
    ("intent.py", None, "covered", "function"),
    ("intent.py", None, "identity_contested", "function"),
    ("manifest.py", None, "_is_access_failure", "function"),
    ("marker.py", None, "valid_assignment", "function"),
    ("marker.py", None, "valid_segment", "function"),
    ("reconcile.py", None, "_is_current", "function"),
    ("report.py", None, "_may_have_reached", "function"),
    ("scope.py", None, "acquire", "function"),
    ("scope.py", None, "still_held", "function"),
    ("service.py", None, "_existing_lock_held", "function"),
    ("service.py", None, "lock_is_held", "function"),
    ("service.py", None, "send", "function"),
    ("transport.py", "TransportFacts", "retry_safe", "field"),
)

# Reaches its value down one producer path that folds nothing.
FOLD_FREE_BOOLEANS = (
    ("admission.py", None, "_stored", "function"),
    ("assignment.py", None, "_any_receipt", "function"),
    ("assignment.py", None, "_claimed", "function"),
    ("daemon.py", None, "_alternate", "function"),
    # One predicate now, shared with the supervisor channel's claim; the fold it used to do
    # lives in delivery.send_refusal, which returns a reason rather than a boolean.
    ("delivery.py", None, "_rate_limited", "function"),
    ("lifecycle.py", None, "is_busy", "function"),
    ("lifecycle.py", None, "may_send", "function"),
    ("scope.py", None, "at_least", "function"),
    ("service.py", None, "stop_requested", "function"),
    ("service.py", None, "usable", "function"),
    ("store.py", None, "in_transaction", "function"),
)

# A write context naming a declared boolean that is not one of the producer forms above. All
# three are locals bound by tuple unpacking that happen to share a declared field's name; the
# reader cannot tell that from a producer, so it says so rather than guessing.
UNACCOUNTED_OCCURRENCES = (
    ("bridge_adapter.py", "_scan_listing", "exhausted"),
    ("currency.py", "head_revision", "covered"),
    ("guard.py", "hold_counters", "found"),
)

# A read of a folded boolean whose asserted value this reader cannot attribute.
UNRESOLVED_READS = (
    ("test_daemon_cadence.py", "test_a_real_run_spends_its_deadline_polling", "quiet",
     "reached through GeneratorExp/Call"),
    ("test_guard_property.py", "test_marker_commands_are_exempt_from_the_store_selection_refusal",
     "_reads_no_selected_store", "reached through assertIs"),
    ("test_service.py", "holder", "lock_is_held", "no enclosing assertion"),
)

# Every place this suite measures something with a folded boolean: module, test, symbol, the
# value asserted, the assertion verbatim, and the verdict a reader wrote after following the
# symbol into what computes it. The assertion is part of the key on purpose - keying by test
# name and polarity alone let a case be replaced by a duplicate of its neighbour with the
# inventory unchanged, which is how a derived list quietly narrows.
#
# Both kinds are here. For a symbol in SUMMARIES the rule knows which value one input alone can
# produce, so it can say which side is cheap. For one in FOLDS_BEYOND_ITS_PATHS it does not, and
# the verdict is the only thing that weighs the site at all.
SUMMARY_SITES = (
    ('test_observation_budget.py', 'test_future_legacy_admission_stays_ineligible_after_generation_opens', 'admitted', False, "self.assertFalse(AnchorOrExplicit().admit(self.store, current, current['generations'][-1], 'unrelated').admitted)", 'the previously absent generation now exists and the legacy row remains; false pins the missing anchor binding, with no settlement or event after a daemon tick'),
    ('test_observation_budget.py', 'test_fresh_explicit_admission_upgrades_legacy_record', 'admitted', False, "self.assertFalse(AnchorOrExplicit().admit(self.store, relation, relation['generations'][0], 'legitimate').admitted)", 'the bound generation and legacy row exist, so false distinguishes missing binding from absent input; the same row becomes admitted only after explicit readmission'),
    ('test_observation_budget.py', 'test_fresh_explicit_admission_upgrades_legacy_record', 'admitted', True, "self.assertTrue(AnchorOrExplicit().admit(self.store, relation, relation['generations'][0], 'legitimate').admitted)", 'fresh explicit readmission follows the measured legacy rejection, preserves the single row and binds its exact anchor'),
    ("test_ack_reconcile.py", "test_a_turn_starting_in_the_same_second_as_its_send_is_not_refused",
     "certainly_before", False, "self.assertFalse(certainly_before(1789420929, sent))",
     "the function is the subject and both arguments are literals, so the case is identified;"
     " its two parse-failure paths return the same value and no case here reaches them"),
    ("test_ack_reconcile.py", "test_a_genuinely_earlier_whole_second_turn_is_still_refused",
     "certainly_before", True, "self.assertTrue(certainly_before(1789420920, sent))",
     "this value is reachable only through the chronology comparison, so it pins the branch"),
    ("test_ack_reconcile.py", "test_a_genuinely_earlier_whole_second_turn_is_still_refused",
     "certainly_before", True, "self.assertTrue(certainly_before(1789420928, sent))",
     "the boundary one second inside the precision window, named apart from the case above"),
    ("test_ack_reconcile.py", "test_a_later_turn_is_never_refused",
     "certainly_before", False, "self.assertFalse(certainly_before(1789420930, sent))",
     "false here is also what a parse failure answers, and nothing in the case distinguishes"
     " them; the literals are well formed, so the reading rests on the argument rather than on"
     " the assertion"),
    ("test_bridge_adapter.py", "test_a_token_beyond_the_first_page_is_found_with_the_forward_cursor",
     "found", True, "self.assertTrue(scan.found)",
     "the scan result is the subject, and the token it found is asserted on the next line"),
    ("test_bridge_adapter.py", "test_a_bounded_scan_reports_that_it_did_not_exhaust_the_history",
     "found", False, "self.assertFalse(scan.found)",
     "paired with the exhausted assertion below it, which is what separates a bounded stop from"
     " an absence"),
    ("test_bridge_adapter.py", "test_a_bounded_scan_reports_that_it_did_not_exhaust_the_history",
     "exhausted", False,
     "self.assertFalse(scan.exhausted, 'a bounded stop is not proof of absence')",
     "the field is the subject: this is the distinction the record exists to carry"),
    ("test_bridge_adapter.py", "test_an_exhausted_scan_says_so", "found", False,
     "self.assertFalse(scan.found)",
     "paired with the exhausted assertion below, the positive control for the case above"),
    ("test_bridge_adapter.py", "test_an_exhausted_scan_says_so", "exhausted", True,
     "self.assertTrue(scan.exhausted)", "the field is the subject"),
    ("test_bridge_adapter.py", "_assert_no_start", "retry_safe", True,
     "self.assertTrue(facts.retry_safe)",
     "a shared helper rather than a case: it asserts the classification beside the absence of a"
     " transport call, and its callers name the refusal each is about"),
    ("test_bridge_adapter.py",
     "test_a_non_never_approval_policy_is_inbox_only_not_a_settings_mismatch", "retry_safe",
     False, "self.assertFalse(facts.retry_safe, 'a closed push channel is not a retry loop')",
     "paired: the refusal code is asserted beside it, so this names which of the several"
     " not-retry-safe refusals produced the value"),
    ("test_bridge_adapter.py", "test_a_withheld_send_classifies_as_busy_and_retry_safe",
     "retry_safe", True,
     "self.assertTrue(facts.retry_safe, 'a send that never happened must stay retryable')",
     "paired with the busy classification asserted beside it"),
    ("test_daemon.py", "test_two_consecutive_unchanged_ticks_write_no_journal_rows", "quiet",
     True, "self.assertTrue(report.quiet)",
     "the flag is the subject, and this value pins the seven counters it folds; the two it does"
     " not fold, skipped and notes, are asserted separately beside it because the journal-row"
     " count does not cover them"),
    ("test_daemon.py", "test_a_tick_that_changed_something_is_not_quiet", "quiet", False,
     "self.assertFalse(report.quiet)",
     "the flag is the subject - the name is about quiet itself - and assertEqual(report"
     ".delivered, 1) beside it names which of the seven counters moved"),
    ("test_daemon.py", "test_a_failed_turn_suppresses_the_staged_claim_instead_of_delivering_it",
     "deliverable", False, "self.assertFalse(self.intake.deliverable(payload['eventId']))",
     "paired: deliverable folds a row existing with its stage final, and the stage is asserted"
     " suppressed two lines below, so an absent row cannot read as a suppressed one"),
    ("test_fairness.py", "test_cancelling_one_assignment_leaves_the_others_served", "quiet",
     False, "self.assertFalse(report.quiet)",
     "redundant rather than wrong: the condition the name states is carried by the send-list"
     " assertions above it, and this line on its own pins none of the seven counters"),
    ("test_guard_property.py", "test_marker_commands_are_exempt_from_the_store_selection_refusal",
     "_reads_no_selected_store", True,
     "self.assertTrue(cli._reads_no_selected_store(Namespace(handler=cli.cmd_intent_register,"
     " db_path='/named/relay.sqlite3')))",
     "the predicate is the subject and the namespace is written out, so the case names which of"
     " its three branches it is about"),
    ("test_guard_property.py", "test_marker_commands_are_exempt_from_the_store_selection_refusal",
     "_reads_no_selected_store", True,
     "self.assertTrue(cli._reads_no_selected_store(Namespace(handler=cli.cmd_intent_declare,"
     " no_db_path=True)))",
     "the documented exception, named apart from the case above"),
    ("test_guard_property.py", "test_marker_commands_are_exempt_from_the_store_selection_refusal",
     "_reads_no_selected_store", False,
     "self.assertFalse(cli._reads_no_selected_store(Namespace(handler=cli.cmd_emit)))",
     "the negative control: a command outside the marker set"),
    ("test_guard_property.py", "test_a_blank_never_equals_an_identity", "named", False,
     "self.assertFalse(marker.named(blank))",
     "the function is the subject; a blank fails both of its conjuncts and the cases enumerate"
     " the blanks rather than asking the summary to tell them apart"),
    ("test_guard_property.py", "test_a_blank_never_equals_an_identity", "same_identity", False,
     "self.assertFalse(marker.same_identity(blank, blank))",
     "the function is the subject and both arguments are written out, so this value names the"
     " case rather than standing in for it"),
    ("test_guard_property.py", "test_a_blank_never_equals_an_identity", "same_identity", False,
     "self.assertFalse(marker.same_identity(blank, 'a-real-identity'))",
     "as above, and the asymmetric pair is the case a symmetric one would miss"),
    ("test_intent.py", "test_correlation_requires_the_preimage_the_intent_hashed", "correlated",
     False, "self.assertFalse(intent.correlated(self.facts(), SESSION))",
     "paired with the positive below it in the same case, which is what separates a wrong"
     " preimage from no claim at all"),
    ("test_intent.py", "test_correlation_requires_the_preimage_the_intent_hashed", "correlated",
     True, "self.assertTrue(intent.correlated(self.facts(), 'second'))",
     "the positive control for the line above"),
    ("test_intent.py", "test_t7_a_second_claim_after_a_bind_contests_the_assignment",
     "identity_contested", False, "self.assertFalse(intent.identity_contested(self.facts()))",
     "the before half of a before-and-after pair in one case, so the transition is what is"
     " measured rather than either value alone"),
    ("test_intent.py", "test_t7_a_second_claim_after_a_bind_contests_the_assignment",
     "identity_contested", True, "self.assertTrue(intent.identity_contested(self.facts()))",
     "the after half; the second claim between them is the change under test"),
    ("test_intent.py", "test_t16_a_competing_claim_that_leaves_its_session_blank_still_competes",
     "identity_contested", True, "self.assertTrue(intent.identity_contested(self.facts()))",
     "the blank-session branch the docstring of identity_contested names, reached only through"
     " same_identity refusing to match nobody with nobody"),
    ("test_intent.py", "test_a_resolution_clears_a_contest_only_by_naming_the_bound_identity",
     "identity_contested", True, "self.assertTrue(intent.identity_contested(self.facts()))",
     "the before half of a pair, with a resolution naming a competitor applied between them"),
    ("test_intent.py", "test_a_resolution_clears_a_contest_only_by_naming_the_bound_identity",
     "identity_contested", False, "self.assertFalse(intent.identity_contested(self.facts()))",
     "the after half; only the resolution naming the bound identity may produce it"),
    ("test_intent.py", "test_coverage_needs_the_digest_to_match_the_content", "covered", False,
     "self.assertFalse(intent.covered(competing, intent._resolutions(self.facts())), 'a"
     " resolution carrying the wrong digest covered the fact anyway')",
     "the quantity the name is about, and it was missing until this inventory looked: review"
     " measured that forcing covered() to answer False for every input left the case green, so"
     " identity_contested alone never established which refusal produced it"),
    ("test_intent.py", "test_coverage_needs_the_digest_to_match_the_content", "identity_contested",
     True, "self.assertTrue(intent.identity_contested(self.facts()))",
     "the consequence, paired with the coverage assertion above it; on its own it is true for a"
     " missing resolution and for one naming somebody else as well"),
    ("test_intent.py", "test_coverage_needs_the_digest_to_match_the_content", "covered", True,
     "self.assertTrue(intent.covered(competing, intent._resolutions(self.facts())), 'a"
     " resolution carrying the recorded digest did not cover the fact')",
     "the positive control, and review measured why it is load-bearing: without it the negative"
     " above is satisfied by a covered() that answers False to everything. The two resolutions"
     " differ only in the digest, which is the comparison the name is about"),
    ("test_intent.py", "test_coverage_needs_the_digest_to_match_the_content", "identity_contested",
     False, "self.assertFalse(intent.identity_contested(self.facts()))",
     "the consequence of the control above; it closes the contest only because the covering"
     " resolution also names the bound identity"),
    ("test_manifest_scope.py", "test_component_containment_not_string_prefix", "is_within", True,
     "self.assertTrue(is_within('/a/b', '/a/b'))",
     "the disjunction is the contract, and this case names its first branch, a path equal to"
     " the root"),
    ("test_manifest_scope.py", "test_component_containment_not_string_prefix", "is_within", True,
     "self.assertTrue(is_within('/a/b', '/a/b/c'))",
     "the second branch, a path descending from the root; between them the two cases cover both"
     " sides of the or rather than either one twice"),
    ("test_manifest_scope.py", "test_component_containment_not_string_prefix", "is_within", False,
     "self.assertFalse(is_within('/a/b', '/a/bc'))",
     "this value pins both branches at once, and the sibling-with-a-longer-name is the"
     " string-prefix trap the function exists to refuse"),
    ("test_manifest_scope.py", "test_component_containment_not_string_prefix", "is_within", False,
     "self.assertFalse(is_within('/a/b', '/a/bc/d'))",
     "the same trap one component deeper, where a prefix test would still answer yes"),
    ("test_manifest_scope.py", "test_component_containment_not_string_prefix", "is_within", False,
     "self.assertFalse(is_within('/a/b', '/a'))",
     "containment in the wrong direction; an ancestor is not inside its own descendant"),
    ("test_manifest_scope.py", "test_a_root_of_slash_answers_from_its_own_branch", "is_within",
     True, "self.assertTrue(is_within('/', '/a/b'))",
     "the producer path this inventory lists as unreduced, and until now no case reached it:"
     " a root of slash answers before the component comparison runs, so this branch carried the"
     " whole answer for one root with nothing naming it"),
    ("test_manifest_scope.py", "test_a_root_of_slash_answers_from_its_own_branch", "is_within",
     False, "self.assertFalse(is_within('/', 'a/b'))",
     "the negative on the same branch, so the case pins the startswith rather than only"
     " confirming that something absolute is inside everything"),
    ("test_manifest_scope.py", "test_a_broken_lease_refuses_the_read", "still_held", False,
     "self.assertFalse(handle._lease.still_held())",
     "false here is also what an unheld lease and an OSError answer, and the case does not"
     " separate them; what carries the name is the refusal asserted beside it, so this line is"
     " a fixture check rather than the claim"),
    ("test_marker.py", "test_two_records_naming_nothing_never_match", "same_identity", False,
     "self.assertFalse(marker.same_identity(None, None))",
     "the function is the subject and the pair is written out, so this names the case"),
    ("test_marker.py", "test_two_records_naming_nothing_never_match", "same_identity", False,
     "self.assertFalse(marker.same_identity('', ''))",
     "as above, for the empty string rather than the absent value"),
    ("test_marker.py", "test_two_records_naming_nothing_never_match", "same_identity", False,
     "self.assertFalse(marker.same_identity('  ', '  '))",
     "as above, for whitespace, which named() strips rather than rejects"),
    ("test_marker.py", "test_two_records_naming_nothing_never_match", "same_identity", False,
     "self.assertFalse(marker.same_identity('a', None))",
     "as above, asymmetric, so a blank on one side alone is enough"),
    ("test_marker.py", "test_two_records_naming_nothing_never_match", "same_identity", True,
     "self.assertTrue(marker.same_identity('a', 'a'))",
     "this value pins both named() conjuncts and the equality, which is the whole function; it"
     " is the positive control that stops the four negatives passing on a broken summary"),
    ("test_marker.py", "test_nothing_names_nothing", "named", False,
     "self.assertFalse(marker.named(value), repr(value))",
     "table-driven over a written list of unnamed values, and the message names which member"
     " failed, so the summary is the subject and the case is identified"),
    ("test_registration_contention.py",
     "test_two_concurrent_binds_leave_one_winner_and_one_recorded_conflict", "identity_contested",
     True,
     "self.assertTrue(intent.identity_contested(facts), 'a recorded conflict left the assignment"
     " reading as uncontested, so nobody has to adjudicate an identity two coordinators both"
     " tried to bind')",
     "paired: the recorded conflict itself is asserted above, and this line is about what the"
     " predicate makes of it"),
    ("test_registration_contention.py",
     "test_two_concurrent_binds_leave_one_winner_and_one_recorded_conflict", "identity_contested",
     False,
     "self.assertFalse(intent.identity_contested(resolved), 'a resolution naming the bound"
     " identity did not settle the contest it covers')",
     "the after half of the pair, with the resolution written out between them"),
    ("test_service.py", "test_enable_is_refused_while_a_foreign_supervisor_is_shutting_down",
     "lock_is_held", True,
     "self.assertTrue(service.lock_is_held(), 'the fixture needs the lock still held')",
     "the message says what it is: a fixture precondition, not the claim the test carries"),
    ("test_service.py", "test_disable_is_refused_while_a_foreign_supervisor_is_shutting_down",
     "lock_is_held", True,
     "self.assertTrue(service.lock_is_held(), 'the fixture needs the lock still held')",
     "as above, a fixture precondition for the sibling case"),
    ("test_service.py",
     "test_stop_writes_no_request_for_a_lock_held_by_an_unidentified_process", "lock_is_held",
     True, "self.assertTrue(service.lock_is_held())",
     "a fixture precondition; the claim is the absent stop request asserted below it"),
    ("test_service.py", "test_stop_terminates_a_process_this_installation_owns", "lock_is_held",
     False, "self.assertFalse(service.lock_is_held())",
     "false is also what an absent lock file and an unopenable one answer; here the file was"
     " created by the fixture, so the reading rests on the fixture rather than on the assertion,"
     " and the process outcome asserted beside it is what carries the name"),
    ("test_service.py", "test_a_stale_record_does_not_block_a_fresh_start", "lock_is_held", False,
     "self.assertFalse(service.lock_is_held())",
     "PROXY, and review measured it: the case starts nothing, so the three assertions in it are"
     " the preconditions a start reads rather than a start that succeeded. Replacing"
     " RelayService.start with a raise leaves it green. What it does establish is that a"
     " terminated holder leaves no ownership and no lock. LaunchReporting below it exercises a"
     " start, but a clean one rather than one after this record, so nothing joins the two and"
     " the name reaches further than the body"),
    ("test_service.py", "test_a_worker_that_exits_is_replaced_on_the_same_store", "lock_is_held",
     False,
     "self.assertFalse(service.lock_is_held(), 'the supervisor released on the way out')",
     "the release is the named condition and this is the only line about it; the two other ways"
     " to reach false need an absent or unopenable lock file, and the supervisor created one"),
    ("test_settings_preservation.py", "test_an_unrecognised_refusal_code_stays_uncertain",
     "retry_safe", False, "self.assertFalse(facts.retry_safe)",
     "paired: the refusal code and the uncertainty are asserted beside it"),
    ("test_settings_preservation.py",
     "test_an_absent_policy_withholds_while_a_reported_one_closes_the_channel", "retry_safe", True,
     "self.assertTrue(absent.retry_safe)",
     "one of a matched pair in the same case, and the pair is the contrast the name states"),
    ("test_settings_preservation.py",
     "test_an_absent_policy_withholds_while_a_reported_one_closes_the_channel", "retry_safe",
     False, "self.assertFalse(reported.retry_safe)", "the other half of that pair"),
    ("test_settings_preservation.py", "test_approval_policy_is_decided_before_the_generic_mismatch",
     "retry_safe", False, "self.assertFalse(facts.retry_safe)",
     "paired: which refusal won the precedence is asserted beside it, and that is the name"),
    ("test_settings_preservation.py", "test_every_settings_refusal_is_a_pre_send_refusal",
     "retry_safe", True, "self.assertTrue(facts.retry_safe)",
     "inside a loop over a written list of refusals, with the pre-send state asserted beside it"),
    ("test_transfer_phases.py",
     "test_an_establishment_expiry_before_the_read_is_a_completed_non_delivery",
     "retry_safe", True, "self.assertTrue(facts.retry_safe)",
     "paired: the delivery state, the send-attempted value and the failed operation are all"
     " asserted beside it, and the record is then put through assert_attempt_invariants, which"
     " pins retry_safe to exactly this combination rather than letting true stand alone"),
    ("test_transfer_phases.py",
     "test_an_establishment_expiry_before_the_resume_is_also_a_completed_non_delivery",
     "retry_safe", True, "self.assertTrue(facts.retry_safe)",
     "the same shape one request later, and the absent resumed block is asserted beside it -"
     " which is the fact that separates this from a refusal decided FROM a resume response"),
    ("test_transfer_phases.py",
     "test_an_establishment_expiry_before_the_turn_stays_pessimistic",
     "retry_safe", False, "self.assertFalse(facts.retry_safe)",
     "false is the summary agreeing with the frozen schema rather than with the receipt: no"
     " turn/start frame was written either, and the held_uncertain state asserted beside it is"
     " what makes this a deliberate over-reading rather than an unread value"),
    ("test_transfer_phases.py", "test_a_transmit_expiry_stays_uncertain",
     "retry_safe", False, "self.assertFalse(facts.retry_safe)",
     "paired with the outcome_unknown receipt status and held_uncertain state; the frame had"
     " already been handed to the socket, so false is the only reading available"),
    ("test_transfer_phases.py", "test_an_acknowledgement_expiry_stays_uncertain",
     "retry_safe", False, "self.assertFalse(facts.retry_safe)",
     "the pre-existing response-wait behaviour, asserted unchanged beside its receipt status"),
    ("test_wp1_regressions.py", "test_a_claim_from_a_live_turn_is_accepted_but_staged",
     "deliverable", False, "self.assertFalse(self.intake.deliverable(payload['eventId']))",
     "paired: the stage is asserted staged on the line above and the turn status below, so an"
     " absent row cannot read as a staged one"),
    ("test_wp1_regressions.py", "test_normal_completion_finalizes_the_staged_claim_exactly_once",
     "deliverable", True, "self.assertTrue(self.intake.deliverable(payload['eventId']))",
     "this value pins both conjuncts, and the finalized list beside it names the event"),
    ("test_wp1_regressions.py", "test_a_failed_ending_suppresses_the_staged_claim", "deliverable",
     False, "self.assertFalse(self.intake.deliverable(payload['eventId']))",
     "paired: the row's stage is asserted suppressed two lines below"),
    ("test_wp1_regressions.py", "test_an_interrupted_ending_suppresses_the_staged_claim",
     "deliverable", False, "self.assertFalse(self.intake.deliverable(payload['eventId']))",
     "paired now, and it was not before: this inventory found it asserting nothing but the"
     " summary, which is equally false for a row that is absent, so it could not tell"
     " suppression from the claim never having been recorded. The stage is asserted beside it"),
    ("test_wp1_regressions.py", "test_a_still_running_turn_leaves_the_claim_staged", "deliverable",
     False, "self.assertFalse(self.intake.deliverable(payload['eventId']))",
     "paired now. result['pending'] above it is returned before resolve_staged reads the stored"
     " claim, so it was true for a suppressed row too and named nothing; review measured that."
     " The stage is asserted beside it"),
    ("test_wp1_regressions.py", "test_a_receipt_from_a_completed_turn_is_final_immediately",
     "deliverable", True, "self.assertTrue(self.intake.deliverable(payload['eventId']))",
     "this value pins both conjuncts and the stage is asserted final above it"),
    ("test_supervisor_envelope.py", "test_a_conditional_acceptance_is_not_a_held_stage",
     "stage_holds", False, "self.assertFalse(envelope.stage_holds(ladder, envelope.AGREED))",
     "the stage is set to conditional with its source on the line above, so false is"
     " attributable to the conditional and not to the stage being absent or unanswered. That"
     " distinction is the case: a conditional acceptance is the one value a reader is most"
     " likely to round up to yes"),
    ("test_supervisor_channel.py",
     "test_a_delivered_message_is_read_back_and_only_then_says_received", "stage_holds", True,
     "self.assertTrue(envelope.stage_holds(ladder, envelope.TRANSPORT_ACCEPTED))",
     "true pins both conjuncts - the stage is present AND its state is yes - so the folding"
     " cannot hide a missing stage here. The ladder is the one the channel derived from a"
     " dispatched attempt, and the line below asserts received is still unmeasured, which is"
     " what makes this the accepted-but-unread state rather than a reached one"),
    ("test_supervisor_channel.py",
     "test_a_delivered_message_is_read_back_and_only_then_says_received", "stage_holds", True,
     "self.assertTrue(envelope.stage_holds(ladder, envelope.RECEIVED))",
     "the same folding after a verified readback, and true again pins both conjuncts. The"
     " source is asserted on the next line, so this cannot be satisfied by a stage answered"
     " from a record this direction does not read"),
    ("test_supervisor_channel.py", "test_an_unverified_readback_cannot_replace_a_verified_one",
     "stage_holds", True,
     "self.assertTrue(envelope.stage_holds(self.channel.reach(message_id),"
     " envelope.RECEIVED))",
     "true pins both conjuncts, and the case is about what SURVIVED: the stored readback is"
     " asserted host_read on the line above, so this reads the ladder derived from it rather"
     " than from the unverified answer that arrived second"),
    ("test_supervisor_channel.py",
     "test_a_verified_readback_survives_a_racing_unverified_one", "stage_holds", True,
     "self.assertTrue(envelope.stage_holds(self.channel.reach(message_id),"
     " envelope.RECEIVED))",
     "the same folding, in the case that reaches the in-transaction guard rather than the"
     " precheck. The stored row is asserted host_read and the message state read on the two"
     " lines above, so true here reads a ladder derived from the survivor; and the case fails"
     " when the guard is mutated out, which is what says the assertion is load-bearing"),
    ("test_supervisor_envelope.py",
     "test_silence_is_never_agreement_and_a_state_needs_a_source", "stage_holds", False,
     "self.assertFalse(envelope.stage_holds(ladder, envelope.AGREED))",
     "paired: stage_holds folds the stage being present with its state equalling yes, and false"
     " alone cannot tell an unanswered stage from a missing one. The line above asserts the"
     " state is unmeasured, which names which of the two this is"),
    ("test_supervisor_envelope.py", "test_a_word_nobody_defined_is_not_one_of_the_three",
     "is_absent", False, "self.assertFalse(envelope.is_absent({'absent': 'probably'}))",
     "the subject is a literal written into the assertion and it IS a mapping, so the false"
     " side is attributable to the reason alone rather than to the type check that shares it."
     " This is the case the symbol was widened for: a word nobody defined used to pass"),
    ("test_supervisor_envelope.py", "test_a_word_nobody_defined_is_not_one_of_the_three",
     "is_absent", False, "self.assertFalse(envelope.is_absent({'absent': None}))",
     "the same shape with the reason absent rather than wrong, written out beside the case"
     " above so the two failing inputs are named apart"),
    ("test_supervisor_envelope.py", "test_a_word_nobody_defined_is_not_one_of_the_three",
     "is_absent", True, "self.assertTrue(envelope.is_absent(envelope.absent(reason)))",
     "the positive control, over every declared absence in turn: true pins both conjuncts,"
     " and it is what stops the two assertions above from being satisfied by a predicate that"
     " answered false to everything"),
)


SOURCE = TESTS.parent / "src" / "codex_session_relay"


def weak_value(node):
    """The value this expression can take from ONE of its inputs alone, or None.

    This is why a boolean summary can be measured without measuring anything. An or answers
    True to any single truthy input, an and answers False to any single falsy one, and a not
    swaps which side is cheap. So asserting a summary's weak value pins none of its inputs,
    while asserting the other value pins all of them.
    """
    if isinstance(node, ast.BoolOp):
        return isinstance(node.op, ast.Or)
    if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
        inner = weak_value(node.operand)
        return None if inner is None else (not inner)
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "bool":
        return weak_value(node.args[0]) if node.args else None
    return None


def _is_bool(annotation):
    return isinstance(annotation, ast.Name) and annotation.id == "bool"


def _declared_default(node):
    """The value a field takes when nobody assigns it, including the field(...) spellings."""
    value = node.value
    if value is None:
        return None
    if isinstance(value, ast.Call) and isinstance(value.func, ast.Name) and value.func.id == "field":
        for keyword in value.keywords:
            if keyword.arg in ("default", "default_factory"):
                return keyword.value
        return None
    return value


def _record_layout(node):
    order, booleans = [], set()
    for statement in node.body:
        if isinstance(statement, ast.AnnAssign) and isinstance(statement.target, ast.Name):
            order.append(statement.target.id)
            if _is_bool(statement.annotation):
                booleans.add(statement.target.id)
    return order, booleans


def declared_booleans():
    """Every bool-annotated dataclass field and every bool-returning function in the package."""
    declared, layouts = {}, {}
    for path in sorted(SOURCE.glob("*.py")):
        tree = ast.parse(path.read_text(encoding="utf-8"))
        for node in ast.walk(tree):
            if isinstance(node, ast.ClassDef):
                order, booleans = _record_layout(node)
                layouts[node.name] = (order, booleans)
                for name in booleans:
                    declared[(path.name, node.name, name, "field")] = tuple(order)
            if isinstance(node, ast.FunctionDef) and _is_bool(node.returns):
                declared[(path.name, None, node.name, "function")] = ()
    return declared, layouts


def producer_paths():
    """Every occurrence of a declared name, sorted into producers and what is left over.

    Occurrence accounting rather than a list of producer forms, because three review rounds
    each found a form the list did not have: a partially reducible return, then a declaration
    default. A form nobody enumerated now lands in the leftovers and fails this module until
    somebody writes down what it is, instead of disappearing from a reach that claims to be
    complete.
    """
    declared, layouts = declared_booleans()
    names = {key[2] for key in declared}
    paths, leftover = [], []
    for path in sorted(SOURCE.glob("*.py")):
        tree = ast.parse(path.read_text(encoding="utf-8"))
        accounted, owner = set(), {}
        for node in ast.walk(tree):
            if isinstance(node, ast.FunctionDef):
                for child in ast.walk(node):
                    owner.setdefault(child, node.name)
        for node in ast.walk(tree):
            if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name):
                if node.target.id in names:
                    accounted.add(id(node.target))
                    default = _declared_default(node)
                    if default is not None:
                        accounted.add(id(default))
                        paths.append((node.target.id, None, "declaration default", default))
            if isinstance(node, ast.FunctionDef) and node.name in names:
                accounted.add(id(node))
                if _is_bool(node.returns):
                    for inner in ast.walk(node):
                        if isinstance(inner, ast.Return) and inner.value is not None:
                            paths.append((node.name, path.name, "return", inner.value))
            if isinstance(node, ast.Assign):
                for target in node.targets:
                    if isinstance(target, ast.Attribute) and target.attr in names:
                        accounted.add(id(target))
                        paths.append((target.attr, None, "attribute assignment", node.value))
                    if isinstance(target, ast.Name) and target.id in names:
                        accounted.add(id(target))
                        paths.append((target.id, None, "name assignment", node.value))
            if isinstance(node, ast.AugAssign) and isinstance(node.target, ast.Attribute):
                if node.target.attr in names:
                    accounted.add(id(node.target))
                    paths.append((node.target.attr, None, "augmented assignment", node.value))
            if isinstance(node, ast.Call):
                callee = node.func.id if isinstance(node.func, ast.Name) else None
                order, booleans = layouts.get(callee, ((), set()))
                for index, argument in enumerate(node.args):
                    if index < len(order) and order[index] in booleans:
                        paths.append((order[index], None, "positional construction", argument))
                for keyword in node.keywords:
                    if keyword.arg in names:
                        paths.append((keyword.arg, None, "keyword construction", keyword.value))
        for node in ast.walk(tree):
            name = None
            if isinstance(node, ast.Attribute) and node.attr in names:
                name = node.attr
            elif isinstance(node, ast.Name) and node.id in names:
                name = node.id
            if name is None or id(node) in accounted:
                continue
            if isinstance(getattr(node, "ctx", None), ast.Load):
                continue
            # Keyed by the function rather than the line, because a line number turns an
            # unrelated edit anywhere above it into a failure here, and this list exists to
            # catch a producer form nobody enumerated rather than to pin a location.
            leftover.append((path.name, owner.get(node, "<module>"), name))
    return paths, tuple(sorted(leftover))


def _order(key):
    module, holder, name, kind = key
    return (module, holder or "", name, kind)


def declared_boolean_rows():
    """One row per declared boolean. No symbol is classified out of the inventory.

    Four parts, and the last three are the reason this exists. A reach that reports only what
    it reached is the overstatement this file was written to stop, so every row also carries
    every producer path whose value this rule could not reduce, the sibling fields of a record
    that the boolean does not fold at all, and whether the symbol folds somewhere the reduction
    rule cannot follow.

    An empty first part means the symbol folds nothing this reader can see: it is not a
    summary. It still gets a row, because a symbol that falls out of the inventory is exactly
    how a sweep ends up claiming a reach it does not have.
    """
    declared, _layouts = declared_booleans()
    paths, _leftover = producer_paths()
    rows = {}
    for key, order in sorted(declared.items(), key=lambda item: _order(item[0])):
        module, _holder, name, kind = key
        if kind == "function":
            mine = [p for p in paths if p[0] == name and p[2] == "return" and p[1] == module]
        else:
            mine = [p for p in paths if p[0] == name and p[2] != "return"]
        folded = tuple(sorted({
            weak_value(e) for _n, _m, _k, e in mine if weak_value(e) is not None
        }))
        seen = {}
        for _n, _m, k, e in mine:
            if weak_value(e) is None:
                label = k + ": " + ast.unparse(e)
                seen[label] = seen.get(label, 0) + 1
        unreduced = tuple(sorted(seen.items()))
        touched = set()
        for _n, _m, _k, expression in mine:
            if weak_value(expression) is not None:
                touched |= {x.attr for x in ast.walk(expression) if isinstance(x, ast.Attribute)}
        siblings = tuple(sorted(set(order) - touched - {name})) if folded else ()
        beyond = _folds_outside_its_paths(paths, module, name, kind)
        rows[key] = (folded, unreduced if folded else (), siblings, beyond)
    return rows


def partitioned_booleans():
    """Every declared boolean in exactly one of three lists, so none is classified out.

    A symbol whose fold reduces to a weak value is a summary and carries its whole row. A
    symbol that folds where the rule cannot compute that value is named anyway, because a fold
    nobody can weigh is still a fold. Everything else reaches its value one way, down one
    producer path that folds nothing. Membership is derived; a symbol that starts folding moves
    lists and fails this module until somebody says so.
    """
    rows = declared_boolean_rows()
    summaries = {key: row for key, row in rows.items() if row[0]}
    beyond = tuple(sorted((key for key, row in rows.items() if not row[0] and row[3]), key=_order))
    plain = tuple(sorted(
        (key for key, row in rows.items() if not row[0] and not row[3]), key=_order))
    return summaries, beyond, plain


def _folds_outside_its_paths(paths, module, name, kind):
    """Does this symbol fold where the reduction rule cannot compute a weak value?

    Two shapes, and the second was missing until review found it. The first is a fold the rule
    cannot follow: guard.receipt_matches runs four checks through early returns inside an
    if not (...), so no producer path reduces. The second needs no boolean operator at all -
    more than one producer path IS a fold, because the value summarises which branch was taken.
    ack.certainly_before folds two parse failures and a chronology comparison that way and has
    no BoolOp anywhere, and calling that fold-free was a misclassification rather than a
    declared blind spot.
    """
    if kind == "function":
        mine = [p for p in paths if p[0] == name and p[2] == "return" and p[1] == module]
    else:
        mine = [p for p in paths if p[0] == name and p[2] != "return"]
    if len(mine) >= 2:
        return True
    if kind != "function":
        return False
    tree = ast.parse((SOURCE / module).read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == name and _is_bool(node.returns):
            return any(isinstance(x, ast.BoolOp) for x in ast.walk(node))
    return False


def boolean_summaries():
    """The rows that actually fold something. A summary is a row with a weak value."""
    return {key: row for key, row in declared_boolean_rows().items() if row[0]}


def _is_assertion(node):
    return (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
            and node.func.attr.startswith("assert"))


def _asserted_value(call, read):
    """Which boolean this assertion pins the read to, or None if this reader cannot say."""
    name, args = call.func.attr, call.args
    if name in ("assertTrue", "assertFalse") and args and args[0] is read:
        return name == "assertTrue"
    if name in ("assertEqual", "assertIs", "assertNotEqual", "assertIsNot") and len(args) >= 2:
        for left, right in ((args[0], args[1]), (args[1], args[0])):
            if left is read and isinstance(right, ast.Constant) and isinstance(right.value, bool):
                return right.value if name in ("assertEqual", "assertIs") else not right.value
    return None

def summary_reads():
    """Every place this suite measures something with a summary, and every read it cannot read.

    One entry per occurrence rather than per distinct text: two identical assertions in one
    test are two rows, so deleting one of them is a failure rather than a silent narrowing.

    Both kinds of folded boolean are in scope. For a summary the rule knows which value is
    cheap; for one that folds beyond its paths it does not, and a reader supplies what the rule
    cannot. Leaving the second kind out would put the larger half of the class outside the
    reach while the inventory still called itself complete.
    """
    summaries, beyond, _plain = partitioned_booleans()
    folded = list(summaries) + list(beyond)
    fields = {key[2] for key in folded if key[3] == "field"}
    functions = {key[2] for key in folded if key[3] == "function"}
    sites, unresolved = [], []
    for path in sorted(TESTS.glob("test_*.py")):
        tree = ast.parse(path.read_text(encoding="utf-8"))
        parent, owner = {}, {}
        for node in ast.walk(tree):
            for child in ast.iter_child_nodes(node):
                parent[child] = node
            if isinstance(node, ast.FunctionDef):
                for child in ast.walk(node):
                    owner.setdefault(child, node.name)
        for node in ast.walk(tree):
            name = None
            if isinstance(node, ast.Attribute) and node.attr in fields:
                if isinstance(node.ctx, ast.Load):
                    name = node.attr
            elif isinstance(node, ast.Call):
                called = node.func
                callee = called.id if isinstance(called, ast.Name) else (
                    called.attr if isinstance(called, ast.Attribute) else None)
                if callee in functions:
                    name = callee
            if name is None:
                continue
            cursor, through = node, []
            while cursor in parent:
                cursor = parent[cursor]
                if _is_assertion(cursor):
                    break
                through.append(type(cursor).__name__)
            else:
                cursor = None
            where = owner.get(node, "<module>")
            if cursor is None:
                unresolved.append((path.name, where, name, "no enclosing assertion"))
                continue
            value = _asserted_value(cursor, node)
            if value is None:
                shape = "/".join(through) if through else cursor.func.attr
                unresolved.append((path.name, where, name, "reached through " + shape))
                continue
            sites.append((path.name, where, name, value, ast.unparse(cursor)))
    return tuple(sites), tuple(sorted(unresolved))

class TheSweepDerivesItsOwnReachRatherThanClaimingIt(unittest.TestCase):
    """The reach above is produced from source here, so a new place cannot go unexamined.

    Three review rounds on CRW-3 found the same shape three times, the sweep that followed
    claimed five modules and missed a fourth instance anyway, and two more arrived after that
    work merged. A sentence naming how much was checked is the thing that keeps being wrong.
    These cases replace it: the inventory is derived, a symbol or a site that is not declared
    fails here, and what the derivation cannot see is declared beside what it can.
    """

    def test_every_declared_boolean_is_in_exactly_one_of_the_three_lists(self):
        """The property that stops a symbol falling between classifications.

        Four earlier drafts each lost a producer form somewhere - a partially reducible
        return, a declaration default, a constant positional argument. Partitioning every
        declared boolean rather than selecting the interesting ones is what makes that
        impossible rather than unlikely.
        """
        summaries, beyond, plain = partitioned_booleans()
        declared, _layouts = declared_booleans()
        listed = list(summaries) + list(beyond) + list(plain)
        self.assertEqual(
            len(listed), len(set(listed)), "a boolean is in more than one list",
        )
        self.assertEqual(
            set(listed), set(declared),
            "the partition and the declarations disagree."
            f" Only declared: {sorted(map(str, set(declared) - set(listed)))}."
            f" Only listed: {sorted(map(str, set(listed) - set(declared)))}.",
        )
        self.assertGreater(
            len(declared), 40, f"only {len(declared)} booleans were found, so the scan stopped"
            " seeing the source it is supposed to be reading",
        )

    def test_the_derived_summaries_and_their_blind_spots_are_the_declared_ones(self):
        summaries, beyond, plain = partitioned_booleans()
        self.assertEqual(
            {key: row[:3] for key, row in summaries.items()}, SUMMARIES,
            "the summaries this suite measures with and the source disagree. A row is"
            " (weak values, producer paths this rule could not reduce, record siblings it does"
            " not fold); all three move when the source does.",
        )
        self.assertEqual(beyond, FOLDS_BEYOND_ITS_PATHS)
        self.assertEqual(plain, FOLD_FREE_BOOLEANS)

    def test_no_occurrence_of_a_declared_boolean_goes_unaccounted(self):
        """Occurrence accounting is the rule, so a producer form nobody listed fails here."""
        _paths, leftover = producer_paths()
        self.assertEqual(
            leftover, UNACCOUNTED_OCCURRENCES,
            "an occurrence of a declared boolean in a write context is neither a producer form"
            " this reader knows nor declared. Say what it is, or teach the reader the form.",
        )

    def test_every_place_a_summary_is_measured_is_declared_with_a_verdict(self):
        sites, unresolved = summary_reads()
        declared = tuple(row[:5] for row in SUMMARY_SITES)
        self.assertEqual(
            sorted(sites), sorted(declared),
            "the places this suite measures a property with a folded boolean, and the places"
            " written down, disagree. Read the new one, say whether the summary means what the"
            " test is named for, and declare it.",
        )
        self.assertEqual(len(sites), len(declared), "an occurrence was dropped or duplicated")
        self.assertEqual(unresolved, UNRESOLVED_READS)
        self.assertGreater(
            len(sites), 20, f"only {len(sites)} sites were read, so the scan stopped matching",
        )

    def test_the_counts_this_map_prints_are_the_counts_the_suite_produces(self):
        """Prose numbers, read back out of the file and compared.

        The landed table and the sweep paragraph both quote counts, and nothing parsed either
        of them: the criterion-table reader above only looks at five-cell numbered rows. Two
        went stale exactly that way - a module's case count stayed at 6 across a change that
        took it to 13, and the partition counts outlived the rule that produced them. A map
        whose stated reach can drift without failing anything is the shape this file exists to
        close, so the numbers it prints are derived here too.
        """
        text = MAP.read_text(encoding="utf-8")
        landed = re.findall(
            r"^\| " + TICK + r"(test_[a-z0-9_]+\.py)" + TICK + r" \| (\d+) \|", text, re.M,
        )
        self.assertGreater(len(landed), 4, "the landed table stopped being readable")
        for name, printed in landed:
            path = TESTS / name
            self.assertTrue(path.exists(), f"the landed table names {name}, which is not here")
            cases = sum(
                1 for node in ast.walk(ast.parse(path.read_text(encoding="utf-8")))
                if isinstance(node, ast.FunctionDef) and node.name.startswith("test_")
            )
            self.assertEqual(
                int(printed), cases,
                f"the map says {name} has {printed} cases and it has {cases}",
            )

        summaries, beyond, plain = partitioned_booleans()
        sites, _unresolved = summary_reads()
        printed = re.search(
            r"(\d+) booleans as (\d+) / (\d+) / (\d+), and (\d+) measured\s+places", text,
        )
        self.assertIsNotNone(printed, "the sweep section stopped printing its counts")
        self.assertEqual(
            tuple(int(group) for group in printed.groups()),
            (
                len(summaries) + len(beyond) + len(plain),
                len(summaries), len(beyond), len(plain), len(sites),
            ),
            "the counts the map prints and the ones the suite produces disagree",
        )

    def test_no_declared_site_is_left_without_a_reading(self):
        for module, test, summary, _value, _source, verdict in SUMMARY_SITES:
            self.assertTrue(
                verdict.strip(),
                f"{module} {test} measures {summary} with no verdict written beside it",
            )


# --------------------------------------------- where this suite injects store faults
#
# The sweep above derives which booleans this suite measures WITH. This derives where it
# breaks things on purpose, which is the other way a case can assert nothing. The store's
# fault hook is global and fires just before every COMMIT, so an arming reaches whichever
# transaction runs first rather than the one the case is named for - and a case can then
# pass in a situation its own name does not describe. Every hook-shaped CANDIDATE this
# reader recognises is enumerated from source and carries the interval it reaches and a
# reading. Candidate, not arming: this reader cannot tell a Store from anything else, so it
# over-reports on purpose in the one direction that is safe, and the paragraph below says
# how far recognition goes.
#
# What this is, and what it does NOT claim. The claim was rewritten four times under review
# and was wrong three of them, which is the best argument for stating it exactly.
#
# This is a scope-blind, flow-blind reader of syntax, not Python name resolution. It
# recognises: an attribute assignment whose attribute is the hook; a call whose callee names
# the helper; a call to setattr, exec, eval or __setattr__; a __dict__ subscript assignment;
# a string containing the hook's name; a read of the hook; the helper named without being
# called; and a binding of the helper or of setattr, exec or eval to some other name, read
# one level down an assignment, an annotated assignment, a walrus or an import-as. Every one
# of those becomes a site or an unaccounted occurrence, and THAT is the only totality
# claimed. Over-reporting is deliberate where it happens: a hook-named attribute on an
# unrelated object costs somebody a declaration, and missing one hides an arming.
#
# It deliberately does not follow a binding. Resolving aliases is what an earlier version
# did, and it answered wrongly twice: once reporting a raw reflective arming as a helper site
# because the same local name appeared in two functions, and once for obj.arm(). A reader
# that answers "helper" for a raw arming hides exactly what this exists to surface, which is
# worse than one that says "somebody bound this name, go and read it". So a binding is
# reported and the call made through it is not classified at all.
#
# Out of reach, and not claimed absent: an arming reached through a resolved alias, a
# closure, a callback, a name looked up at runtime, or any other reflection.
#
# As with SUMMARY_SITES above: the scan supplies the reach, a person supplies the verdict,
# and a wrong verdict fails nothing here.

HOOK = "fault_hook"
HELPER = "killed_before_commit"
HIDING_CALLS = ("setattr", "exec", "eval")
UNREADABLE = "unreadable"

FAULT_SITES = (
    ("support.py", "killed_before_commit", "raw", None,
     "the helper's own arming, and the only raw one outside a declared row below: every"
     " other case reaches the hook through it"),
    ("test_ack_reconcile.py", "test_a_fault_at_commit_time_also_rolls_back", "raw", None,
     "record_verdict runs exactly one transaction, measured as generations, relationships,"
     " journal, delivery_supersession, events, deliveries, verdicts and verdict_context"
     " together, so an unconditional arming cannot land anywhere but the transaction the"
     " name is about. Left raw deliberately: a predicate would pin a fact the single"
     " transaction already guarantees"),
    ("test_failure_recovery.py",
     "test_a_tick_killed_at_its_first_write_leaves_no_partial_state_and_a_new_daemon"
     "_delivers_once", "helper", None,
     "no predicate, because first IS the interval this case is named for. It asserts the"
     " interrupted transaction wrote the poll observation and did NOT reach an attempt, so"
     " the two cases in that class cannot collapse into one"),
    ("test_failure_recovery.py",
     "test_a_tick_killed_inside_the_attempt_transaction_rolls_that_attempt_back_and_a_new"
     "_daemon_delivers_once", "helper", "attempts",
     "attempts alone would not identify the claim - delivery's _settle and reconcile write"
     " that table too, and _settle writes deliveries beside it - so the case asserts"
     " attempt_messages was in the same transaction, which has exactly one writer and it is"
     " inside _claim, with an empty send list as the independent check. The reserved capacity"
     " is in that subset too, so the empty-table checks cannot pass for a reservation that was"
     " never written"),
    ("test_register_atomicity.py",
     "test_killed_before_the_relationship_commit_leaves_no_settings_behind", "helper",
     "relationships",
     "cmd_register composes its writes into ONE transaction, so naming either table reaches"
     " the same interval; the predicate is what makes the case say which end of it it is"
     " about. This one kills at the relationship write and asserts authorized_settings was"
     " in the same set, which is the direction that already held before CRW-173 and is kept"
     " so neither order can pass this file alone"),
    ("test_register_atomicity.py",
     "test_killed_before_the_settings_commit_leaves_no_relationship_behind", "helper",
     "authorized_settings",
     "the interval CRW-173 is about. Before the fix this predicate reached the SECOND of two"
     " transactions and the relationship was already committed; the case asserts"
     " relationships was in the interrupted set, so it fails again the moment the two writes"
     " come apart rather than only reporting an empty store, which a registration that never"
     " started would also produce"),
    ("test_store.py", "test_a_failed_registration_is_not_a_registration", "helper",
     "relationships",
     "register() runs three transactions and this name held only because the relationship"
     " write happens to come first; the predicate says so now, and the case asserts the"
     " first generation was in the same transaction"),
    ("test_store.py", "test_a_fault_after_the_body_still_rolls_back", "raw", None,
     "the case opens the only transaction in scope itself, so the arming has nowhere else"
     " to land. Left raw: there is nothing for a predicate to disambiguate"),
)

# A shape that could hide an arming and that this reader cannot classify. Two entries, and
# both are this machinery looking at itself. The HOOK constant: from the outside a string
# naming the hook is a string naming the hook, and nothing here can tell a scan's own
# subject from an attribute name assembled for a setattr. The helper's own def: nothing
# here can tell the real definition from one that shadows it, and a shadow is how a call
# gets classified as the helper when it is something else. Both are declared rather than
# excluded, because excluding a file is how you create the one place an arming sits unseen.
UNACCOUNTED_FAULT_OCCURRENCES = (
    ("support.py", "killed_before_commit", "defines the helper"),
    ("test_regression_map.py", "<module>", "names the hook in a string"),
    ("test_regression_map.py",
     "test_the_names_this_reader_tracks_are_the_ones_written_down_here",
     "names the hook in a string"),
)


def _referred(node):
    """The bare name an expression refers to, for the two spellings that matter here."""
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        return node.attr
    return None


TRACKED = (HELPER, *HIDING_CALLS)


def _binds_a_tracked_name(node):
    """The tracked names this statement binds to some other name, reported and not followed.

    Seven review rounds each produced another spelling of "bind it elsewhere and call that":
    a plain assignment, an annotated one, a tuple, a starred tuple, a walrus, an import-as.
    Resolving them answered WRONGLY twice - a raw reflective arming was reported as a helper
    site once the same local name appeared in two functions, and again for obj.arm(). Name
    resolution is not an AST-shape problem, so this stopped trying.

    A binding is reported; the call made through it is not classified at all. What the
    resolver was pretending to do, a person now has to do: read the binding and write down
    what it is.

    Shallow on purpose. The right-hand side is read one level: the value itself, or the
    elements of a tuple or list literal. A binding buried deeper is not seen, and the reach
    comment says so rather than implying otherwise.
    """
    if isinstance(node, (ast.Import, ast.ImportFrom)):
        return [alias.name.split(".")[-1] for alias in node.names
                if alias.asname and alias.name.split(".")[-1] in TRACKED]
    if isinstance(node, ast.Assign):
        values = [node.value]
    elif isinstance(node, (ast.AnnAssign, ast.NamedExpr)):
        values = [node.value] if node.value is not None else []
    else:
        return []
    found = []
    for value in values:
        parts = value.elts if isinstance(value, (ast.Tuple, ast.List)) else [value]
        for part in parts:
            if _referred(part) in TRACKED:
                found.append(_referred(part))
    return found


def _flatten(node):
    """Every leaf of an assignment target, tuples and lists opened recursively."""
    if isinstance(node, (ast.Tuple, ast.List)):
        leaves = []
        for element in node.elts:
            leaves.extend(
                _flatten(element.value if isinstance(element, ast.Starred) else element)
            )
        return leaves
    return [node]


def _pairs(target, value):
    """(leaf, the value reaching it) when the two shapes correspond, else None.

    Flattening both sides and zipping was wrong: store.fault_hook, (a, b) = (None, cb), pair
    assigns a tuple to the hook, and a flat zip handed it the inner None instead, so an
    arming became invisible. Structures have to line up or this declines to say anything,
    and the caller reports that it could not pair them.
    """
    if isinstance(target, (ast.Tuple, ast.List)):
        if not isinstance(value, (ast.Tuple, ast.List)):
            return None
        if len(target.elts) != len(value.elts):
            return None
        found = []
        for element, reaching in zip(target.elts, value.elts):
            if isinstance(element, ast.Starred):
                # *store.fault_hook, other = [None, 1] hands the hook the LIST [None], not
                # the scalar. Stripping the star paired it with None and read an arming as a
                # disarming, so the whole structure is unreadable instead.
                return None
            if isinstance(reaching, ast.Starred):
                # The mirror image: (*[None], 1) expands before assignment, so positions
                # after the star are not the positions written here. Pairing the hook with
                # the Starred node itself read a disarming as an arming.
                return None
            inner = _pairs(element, reaching)
            if inner is None:
                return None
            found.extend(inner)
        return found
    return [(target, value)]


def _binds_the_helper_name(node, alias):
    """Does this import alias bind the helper's NAME, whatever it reads to get there.

    The bound name differs by form, and reading the wrong end of it went both ways during
    review. "from x import y as z" binds z. "from x import y" binds y. "import a.b.c" binds
    a, not c, so a dotted import merely NAMED after the helper shadows nothing. And a star
    import may or may not export the name; this reader cannot open the other module to find
    out, so it assumes the binding it cannot rule out.
    """
    if alias.asname:
        return alias.asname == HELPER
    if alias.name == "*":
        return True
    if isinstance(node, ast.Import):
        return alias.name.split(".")[0] == HELPER
    return alias.name == HELPER


def _is_canonical(node, alias):
    """The one import that makes a bare call the real helper: from .support import it."""
    return (
        isinstance(node, ast.ImportFrom) and node.level == 1 and node.module == "support"
        and alias.name == HELPER and not alias.asname
    )


def _binds_name(node, name):
    """Does this node bind NAME, whatever syntax it uses to do it.

    Enumerated as a class rather than one construct at a time. Review found shadowing
    through a def, a class, a parameter, an import, an assignment, and then a match-case
    capture, each needing its own patch; the last one is the argument for listing the
    binding forms the language has instead of the ones somebody thought of. A Name in Store
    or Del context already covers assignment, augmented assignment, walrus, for targets,
    with-as, except-as and comprehension targets, which is most of them.
    """
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
        return node.name == name
    if isinstance(node, ast.arg):
        return node.arg == name
    if isinstance(node, ast.Name):
        return node.id == name and isinstance(node.ctx, (ast.Store, ast.Del))
    if isinstance(node, (ast.Global, ast.Nonlocal)):
        return name in node.names
    if isinstance(node, ast.ExceptHandler):
        return node.name == name
    if isinstance(node, (ast.MatchAs, ast.MatchStar)):
        return node.name == name
    if isinstance(node, ast.MatchMapping):
        return node.rest == name
    return False


def _predicate_of(call):
    """The writing predicate a helper call passes, or that it cannot be read."""
    writing = None
    for keyword in call.keywords:
        if keyword.arg is None:
            # **something. The predicate may be in there and this reader cannot see it;
            # answering None would assert there is none, which is a different claim.
            return UNREADABLE
        if keyword.arg == "writing":
            writing = (
                keyword.value.value
                if isinstance(keyword.value, ast.Constant) else UNREADABLE
            )
    return writing


def _injection_sites_in(tree, module):
    """Sites and leftovers for one parsed module.

    Split out from the file walk so the rules can be pinned against synthetic source.
    Review measured the reason: replacing the binding reader with one that returns nothing
    left every other case in this module green, so the behaviour was checked by a scratch
    harness and by nothing that ships.
    """
    sites, leftover = [], []
    owner = {}
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            for child in ast.walk(node):
                owner.setdefault(child, node.name)
    # A string whose value is thrown away cannot name an attribute for any purpose, so a
    # docstring mentioning the hook is not a hiding place.
    discarded = {
        id(node.value) for node in ast.walk(tree)
        if isinstance(node, ast.Expr) and isinstance(node.value, ast.Constant)
    }
    accounted = set()
    # Provenance for the helper name, and it is the exact import or nothing. A bare call is
    # only the helper when this module did "from .support import killed_before_commit"; a
    # parameter, a class, a local def or an import from anywhere else is a different object
    # that happens to share a name, and guessing which is how this reader answered wrongly.
    # Module body only. An import inside a function binds a local name there and says
    # nothing about a bare call somewhere else, and treating it as provenance let such a
    # call be recorded as the helper.
    imported = any(
        _is_canonical(node, alias)
        for node in tree.body if isinstance(node, ast.ImportFrom)
        for alias in node.names
    )
    # A competing binding of the same name defeats the import, wherever the import sits. An
    # earlier draft only looked for the canonical import and stopped, so a second import of
    # the name from somewhere else left every later call classified as the real helper.
    shadowed = any(
        # Per ALIAS, not per statement. The name an import BINDS is what matters, and
        # exempting a whole statement let "from .support import WorkerKilled as
        # killed_before_commit" ride along beside the canonical one.
        _binds_the_helper_name(node, alias) and not _is_canonical(node, alias)
        for node in ast.walk(tree) if isinstance(node, (ast.Import, ast.ImportFrom))
        for alias in node.names
    ) or any(
        _binds_name(node, HELPER)
        for node in ast.walk(tree)
        if not isinstance(node, (ast.Import, ast.ImportFrom))
    )

    for node in ast.walk(tree):
        for bound in _binds_a_tracked_name(node):
            leftover.append((module, owner.get(node, "<module>"), "binds " + bound + " elsewhere"))
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == HELPER:
            # This reader cannot tell the real definition from one that shadows it, so it
            # declines to and says which module it found.
            leftover.append((module, node.name, "defines the helper"))
        targets = []
        if isinstance(node, ast.Assign):
            targets = node.targets
        elif isinstance(node, ast.AnnAssign):
            targets = [node.target]
        for target in targets:
            # store.fault_hook, other = die, 1 is an arming and ... = None, 1 is not, so the
            # target has to be paired with the value that reaches it. Without that, the
            # attribute fell through to the read pass and came back as "reads the hook", and
            # a tuple disarming was reported as an arming.
            leaves = _flatten(target)
            paired = dict(_pairs(target, node.value) or []) if node.value is not None else {}
            for leaf in leaves:
                if isinstance(leaf, ast.Subscript) and _referred(leaf.value) == "__dict__":
                    leftover.append(
                        (module, owner.get(node, "<module>"), "assigns through __dict__")
                    )
                if not (isinstance(leaf, ast.Attribute) and leaf.attr == HOOK):
                    continue
                accounted.add(id(leaf))
                if node.value is None:
                    continue           # a bare annotation binds nothing, so it arms nothing
                if leaf not in paired:
                    leftover.append(
                        (module, owner.get(node, "<module>"),
                         "assigns the hook from a value this reader cannot pair")
                    )
                    continue
                reaching = paired[leaf]
                if isinstance(reaching, ast.Constant) and reaching.value is None:
                    continue           # disarming
                sites.append((module, owner.get(node, "<module>"), "raw", None))
        if isinstance(node, ast.Call):
            callee = _referred(node.func)
            if isinstance(node.func, ast.Name) and callee == HELPER:
                accounted.add(id(node.func))
                if imported and not shadowed:
                    sites.append(
                        (module, owner.get(node, "<module>"), "helper", _predicate_of(node))
                    )
                else:
                    leftover.append(
                        (module, owner.get(node, "<module>"), "calls a helper-named local")
                    )
            elif callee == HELPER:
                # obj.killed_before_commit(). Same final name, no reason to believe it is the
                # same function, and guessing is how a raw arming got called a helper site.
                accounted.add(id(node.func))
                leftover.append((module, owner.get(node, "<module>"), "calls a helper-named attribute"))
            elif callee in HIDING_CALLS or callee == "__setattr__":
                leftover.append((module, owner.get(node, "<module>"), "calls " + str(callee)))

    for node in ast.walk(tree):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            if HOOK in node.value and id(node) not in discarded:
                leftover.append((module, owner.get(node, "<module>"), "names the hook in a string"))
            continue
        if isinstance(node, ast.Attribute) and node.attr == HOOK:
            if id(node) not in accounted:
                leftover.append((module, owner.get(node, "<module>"), "reads the hook"))
        if isinstance(node, (ast.Name, ast.Attribute)) and _referred(node) == HELPER:
            if id(node) not in accounted:
                leftover.append((module, owner.get(node, "<module>"), "names the helper uncalled"))
    return sites, leftover


def fault_injection_sites():
    """The armings this reader recognises across the suite, and the shapes it cannot read.

    Occurrence accounting, in the same spirit as producer_paths above: a shape that could
    hide an arming and is not one of the forms this reader knows becomes a leftover and
    fails this module, rather than disappearing from a reach that calls itself complete.
    """
    sites, leftover = [], []
    for path in sorted(TESTS.rglob("*.py")):
        found, left = _injection_sites_in(ast.parse(path.read_text(encoding="utf-8")), path.name)
        sites.extend(found)
        leftover.extend(left)
    return tuple(sorted(sites, key=_site_order)), tuple(sorted(leftover))



def _site_order(site):
    module, where, kind, writing = site
    return (module, where, kind, writing or "")


class EveryFaultInjectionSiteIsDeclared(unittest.TestCase):
    """The reach of the fault injections, produced here rather than described in prose.

    CRW-97 opened because one case's arming reached a transaction its name did not name.
    Fixing that instance would leave the next one to be found by hand, so the sites are
    derived and each carries what it reaches and why a reader believes that is the interval
    the case is about.
    """

    def test_every_place_this_suite_arms_the_fault_hook_is_declared_with_its_interval(self):
        sites, _leftover = fault_injection_sites()
        declared = tuple(row[:4] for row in FAULT_SITES)
        self.assertEqual(
            sorted(sites, key=_site_order), sorted(declared, key=_site_order),
            "the armings in this suite and the ones written down disagree."
            f" Only in source: {sorted(set(sites) - set(declared))}."
            f" Only declared: {sorted(set(declared) - set(sites))}."
            " Read the new one, say which transaction it reaches and whether that is the"
            " interval its case is named for, and declare it.",
        )
        self.assertGreater(
            len(sites), 4,
            f"only {len(sites)} armings were found, so the scan stopped seeing the suite it"
            " is supposed to be reading",
        )

    def test_nothing_arms_the_hook_outside_the_helper_without_saying_why(self):
        """The helper is the route; a bare assignment has to argue for itself."""
        sites, _leftover = fault_injection_sites()
        raw = {(module, where) for module, where, kind, _ in sites if kind == "raw"}
        self.assertEqual(
            raw, {(row[0], row[1]) for row in FAULT_SITES if row[2] == "raw"},
            "an arming bypasses killed_before_commit without a declared reason. Either route"
            " it through the helper, which makes the interval it reaches assertable, or"
            f" declare why this one does not need to: {sorted(raw)}",
        )

    def test_a_shape_that_could_hide_an_arming_is_declared(self):
        _sites, leftover = fault_injection_sites()
        self.assertEqual(
            leftover, UNACCOUNTED_FAULT_OCCURRENCES,
            "a construct that could name the hook by a route this reader cannot follow is"
            " neither a form it knows nor declared. Say what it is, or teach the reader the"
            " form. Arbitrary reflective mutation stays out of reach either way.",
        )

    def test_no_declared_arming_is_left_without_a_reading(self):
        for module, where, _kind, _writing, verdict in FAULT_SITES:
            self.assertTrue(
                verdict.strip(),
                f"{module} {where} arms the hook with no reading written beside it",
            )


class TheWriteClassifierSaysWhatItCannotRead(unittest.TestCase):
    """support.written_table decides which transaction a fault landed in.

    So its reach is not a detail. A statement it cannot read makes a transaction look
    emptier than it is, and a case could then assert it killed somewhere it did not.
    """

    def test_the_write_forms_this_package_issues_are_classified(self):
        for statement, table in (
            ("INSERT INTO attempts (request_id) VALUES (?)", "attempts"),
            ("INSERT INTO poll_observations (a) VALUES (?)"
             " ON CONFLICT(a) DO UPDATE SET b = 1", "poll_observations"),
            ("INSERT OR IGNORE INTO schema_meta VALUES ('k','v')", "schema_meta"),
            ("INSERT OR REPLACE INTO attempt_report_submissions (x) VALUES (?)",
             "attempt_report_submissions"),
            ("UPDATE deliveries SET state = ?", "deliveries"),
            ("DELETE FROM holds WHERE id = ?", "holds"),
            ("  update  relationships  set x = 1", "relationships"),
            ("INSERT INTO main.journal (at) VALUES (?)", "journal"),
        ):
            self.assertEqual(written_table(statement), table, statement)

    def test_the_forms_it_cannot_read_answer_nothing_rather_than_guessing(self):
        """The blind spots, as data. A reader that guessed here would be worse than one
        that declines, because a wrong table is a wrong transaction."""
        for statement in (
            "WITH recent AS (SELECT 1) INSERT INTO attempts (a) VALUES (1)",
            "SELECT * FROM attempts",
            "BEGIN IMMEDIATE",
            "CREATE TABLE attempts (a)",
            "SAVEPOINT inner",
        ):
            self.assertIsNone(written_table(statement), statement)

    def test_no_write_statement_in_this_package_escapes_the_classifier(self):
        """The blind spot above, watched instead of only described.

        Screened case-sensitively, because every SQL statement in this package is uppercase
        and a case-insensitive screen fires on ordinary prose - "With a host this verifies",
        "Replace bounded workers". What that rests on is a convention, so state it: a
        lowercase CTE would evade this, and that is the gap this cannot close.
        """
        head = re.compile(r"^\s*(?:INSERT|REPLACE|UPDATE|DELETE|WITH)\b")
        screened, unreadable = 0, []
        for path in sorted(SOURCE.glob("*.py")):
            for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
                if not (isinstance(node, ast.Constant) and isinstance(node.value, str)):
                    continue
                if not head.match(node.value):
                    continue
                screened += 1
                if written_table(node.value) is None:
                    unreadable.append((path.name, node.lineno, node.value[:60]))
        self.assertEqual(
            unreadable, [],
            "a write statement in this package is one the transaction watcher cannot"
            " attribute, so a transaction holding it would look emptier than it is",
        )
        self.assertGreater(
            screened, 60,
            f"only {screened} write statements were screened, so this scan stopped matching",
        )



class TheInjectionReaderIsPinnedToTheAnswersItGives(unittest.TestCase):
    """Synthetic source, because this suite is not a test corpus for its own reader.

    Review measured why this exists: replacing the binding reader with one that returns
    nothing left every other case in this module green. The reach was checked by a scratch
    harness and by nothing that ships, which is the same shape as a fault firing before the
    interval a case is named for - right today, and right for a reason nothing records.

    The expectations here are written out rather than derived from the rules under test. An
    earlier draft took its hiding name from HIDING_CALLS[0], and review measured that
    deleting exec and eval from that tuple left every case green. A test whose oracle moves
    with the code cannot fail when the code is wrong.

    The wrong-answer rows matter most. They are not gaps; they are the reader saying
    "helper" about something that is not one, which hides what the inventory exists to show.
    """

    IMPORT = f"from .support import {HELPER}\n"

    def read(self, source):
        sites, leftover = _injection_sites_in(
            ast.parse(textwrap.dedent(source).strip() + "\n"), "probe.py",
        )
        return sorted(sites, key=_site_order), sorted(leftover)

    def kinds(self, leftover):
        return sorted(reason for _module, _where, reason in leftover)

    def test_the_names_this_reader_tracks_are_the_ones_written_down_here(self):
        """The oracle below names these three. If the rules stop tracking one, the cases
        that exercise it would silently stop exercising anything, so the sets are compared
        rather than shared."""
        self.assertEqual(HIDING_CALLS, ("setattr", "exec", "eval"))
        self.assertEqual(HOOK, "fault_hook")
        self.assertEqual(HELPER, "killed_before_commit")

    def test_a_direct_arming_is_a_site_however_it_is_spelled(self):
        for source in (
            f"def t(store, die):\n    store.{HOOK} = die\n",
            f"def t(store, die):\n    store.{HOOK}: object = die\n",
            f"def t(store, die):\n    store.{HOOK}, other = die, 1\n",
            f"def t(store, die):\n    [store.{HOOK}] = [die]\n",
            f"def t(store, die):\n    (store.{HOOK}, a), b = (die, 1), 2\n",
        ):
            sites, _leftover = self.read(source)
            self.assertEqual(sites, [("probe.py", "t", "raw", None)], source)

    def test_binding_nothing_arms_nothing(self):
        """Disarming, and a bare annotation. Neither assigns, so neither is an arming."""
        for source in (
            f"def t(store):\n    store.{HOOK} = None\n",
            f"def t(store):\n    store.{HOOK}: object\n",
            f"def t(store):\n    store.{HOOK}, other = None, 1\n",
        ):
            sites, leftover = self.read(source)
            self.assertEqual(sites, [], source)
            self.assertEqual(leftover, [], source)

    def test_a_value_this_reader_cannot_pair_is_a_leftover_not_a_site(self):
        """The target names the hook and the value cannot be matched to it, so the reader
        declines to say which it is rather than guessing arming or disarming."""
        for source in (
            f"def t(store, pair):\n    store.{HOOK}, other = pair\n",
            # The shapes differ one level in: Python hands the hook the whole (None, cb)
            # tuple, and a flat zip used to hand it the inner None and see a disarming.
            f"def t(store, cb, pair):\n    store.{HOOK}, (a, b) = (None, cb), pair\n",
            # A starred target takes a LIST, so pairing it with a scalar would read an
            # arming as a disarming.
            f"def t(store):\n    *store.{HOOK}, other = [None, 1]\n",
            # The mirror image: a starred value expands before assignment, so the positions
            # after it are not the ones written.
            f"def t(store):\n    store.{HOOK}, other = (*[None], 1)\n",
        ):
            sites, leftover = self.read(source)
            self.assertEqual(sites, [], source)
            self.assertIn(
                "assigns the hook from a value this reader cannot pair",
                self.kinds(leftover), source,
            )

    def test_a_direct_helper_call_carries_the_predicate_it_passes(self):
        for label, prefix in (
            ("the canonical import alone", ""),
            # import a.b.c binds a, so a dotted import merely NAMED after the helper takes
            # nothing away. Reading its last component instead turned real helper calls
            # into leftovers.
            ("beside a dotted import that only looks like it", f"import foreign.{HELPER}\n"),
        ):
            sites, _leftover = self.read(
                self.IMPORT + prefix
                + "def t(store):\n"
                + f"    with {HELPER}(store, writing='attempts'):\n"
                + "        pass\n"
            )
            self.assertEqual(sites, [("probe.py", "t", "helper", "attempts")], label)

    def test_a_predicate_this_reader_cannot_see_says_so_rather_than_saying_none(self):
        """None means the case passes no predicate. That is a claim, and ** is not it."""
        sites, _leftover = self.read(
            self.IMPORT
            + "def t(store, options):\n"
            + f"    with {HELPER}(store, **options):\n"
            + "        pass\n"
        )
        self.assertEqual(sites, [("probe.py", "t", "helper", UNREADABLE)])

    def test_every_reflective_spelling_review_proposed_is_visible(self):
        for label, source, expected in (
            ("direct call", "def t(store, die):\n    setattr(store, 'x', die)\n",
             "calls setattr"),
            ("direct exec", "def t(store):\n    exec('x')\n", "calls exec"),
            ("direct eval", "def t(store):\n    eval('x')\n", "calls eval"),
            ("object.__setattr__",
             "def t(store, die):\n    object.__setattr__(store, 'x', die)\n",
             "calls __setattr__"),
            ("__dict__ subscript", "def t(store, die):\n    store.__dict__['x'] = die\n",
             "assigns through __dict__"),
            ("assigned alias", "def t(store, die):\n    arm = setattr\n",
             "binds setattr elsewhere"),
            ("annotated alias", "def t(store, die):\n    arm: object = eval\n",
             "binds eval elsewhere"),
            ("tuple alias", "def t(store, die):\n    arm, spare = exec, None\n",
             "binds exec elsewhere"),
            ("list alias", "def t(store, die):\n    arm, spare = [setattr, None]\n",
             "binds setattr elsewhere"),
            ("starred target alias",
             "import builtins\ndef t(store, die):\n    arm, *spare = (builtins.setattr,)\n",
             "binds setattr elsewhere"),
            ("walrus alias", "def t(store, die):\n    (arm := eval)('x')\n",
             "binds eval elsewhere"),
            ("import alias", "from builtins import setattr as arm\n",
             "binds setattr elsewhere"),
            ("helper import alias",
             f"from support import {HELPER} as arm\n",
             "binds " + HELPER + " elsewhere"),
        ):
            _sites, leftover = self.read(source)
            self.assertIn(expected, self.kinds(leftover), label)

    def test_a_bound_name_never_produces_a_site(self):
        """The wrong answer that cost two review rounds, pinned so it cannot come back.

        A module-wide alias map reported the raw arming below as a HELPER site, because the
        same local name was bound to the helper in another function. Nothing is inferred
        from a binding now, so a call through a bound name is not classified at all.
        """
        sites, leftover = self.read(
            self.IMPORT
            + "def raw(store, die):\n"
            + "    arm = setattr\n"
            + "    arm(store, 'x', die)\n"
            + "def helper(store):\n"
            + f"    arm = {HELPER}\n"
            + "    with arm(store):\n"
            + "        pass\n"
        )
        self.assertEqual(sites, [])
        self.assertIn("binds setattr elsewhere", self.kinds(leftover))
        self.assertIn("names the helper uncalled", self.kinds(leftover))

    def test_a_helper_name_this_module_did_not_import_is_not_the_helper(self):
        """Provenance, because the name alone is not identity. A parameter, a class, a local
        definition or a foreign import can all carry it."""
        for label, source in (
            ("no import at all",
             f"def t(store):\n    {HELPER}(store)\n"),
            ("an import from somewhere else",
             f"from other import {HELPER}\ndef t(store):\n    {HELPER}(store)\n"),
            ("a plain module import",
             f"import {HELPER}\ndef t(store):\n    {HELPER}(store)\n"),
            ("a competing import beside the real one",
             self.IMPORT + f"from other import {HELPER}\n"
             + f"def t(store):\n    {HELPER}(store)\n"),
            ("a later rebinding of the name",
             self.IMPORT + f"{HELPER} = None\ndef t(store):\n    {HELPER}(store)\n"),
            ("something else imported under the helper's name",
             self.IMPORT + f"from other import replacement as {HELPER}\n"
             + f"def t(store):\n    {HELPER}(store)\n"),
            ("something else from support under the helper's name",
             self.IMPORT + f"from .support import WorkerKilled as {HELPER}\n"
             + f"def t(store):\n    {HELPER}(store)\n"),
            ("the canonical import, but local to another function",
             f"def setup():\n    from .support import {HELPER}\n"
             + f"def t(store):\n    {HELPER}(store)\n"),
            ("a match-case capture of the name",
             self.IMPORT + "def t(store, event):\n    match event:\n"
             + f"        case {{'killer': {HELPER}}}:\n            {HELPER}(store)\n"),
            ("a for-loop target",
             self.IMPORT + f"def t(store, items):\n    for {HELPER} in items:\n"
             + f"        {HELPER}(store)\n"),
            ("a with-as target",
             self.IMPORT + f"def t(store, ctx):\n    with ctx as {HELPER}:\n"
             + f"        {HELPER}(store)\n"),
            ("a star import that might export the name",
             self.IMPORT + "from foreign import *\n"
             + f"def t(store):\n    {HELPER}(store)\n"),
            ("parameter shadow",
             self.IMPORT + f"def t(store, {HELPER}):\n    {HELPER}(store)\n"),
            ("class shadow",
             self.IMPORT + f"class {HELPER}:\n    pass\n"
             + f"def t(store):\n    {HELPER}(store)\n"),
            ("attribute of something else",
             self.IMPORT + f"def t(obj):\n    obj.{HELPER}()\n"),
        ):
            sites, leftover = self.read(source)
            self.assertEqual(
                [site for site in sites if site[2] == "helper"], [], label,
            )
            self.assertTrue(leftover, label)

    def test_a_module_defining_the_helper_says_so_rather_than_trusting_the_name(self):
        _sites, leftover = self.read(
            f"def {HELPER}(store, *, writing=None):\n    pass\n"
        )
        self.assertIn("defines the helper", self.kinds(leftover))

    def test_a_docstring_naming_the_hook_is_not_a_hiding_place(self):
        """A discarded value cannot name an attribute, and the helper's own docstring
        mentions the hook. Excluding docstrings has to be principled or it is a loophole."""
        _sites, leftover = self.read(f'def t():\n    """arms store.{HOOK}"""\n')
        self.assertEqual(leftover, [])
        _sites, named = self.read(f'def t():\n    name = "{HOOK}"\n')
        self.assertIn("names the hook in a string", self.kinds(named))

    def test_an_unrelated_receiver_is_over_reported_rather_than_missed(self):
        """Deliberate, and the safe direction. This reader cannot know what config is, so a
        hook-named attribute on anything is a site that somebody has to declare. Over-
        reporting costs a declaration; under-reporting hides an arming."""
        sites, _leftover = self.read(f"def t(config, value):\n    config.{HOOK} = value\n")
        self.assertEqual(sites, [("probe.py", "t", "raw", None)])


if __name__ == "__main__":
    unittest.main()
