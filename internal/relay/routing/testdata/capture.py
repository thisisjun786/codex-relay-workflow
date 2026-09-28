"""Developer-only oracle: replay each property's own original Python scenarios.

Only arguments are handed to Go. Python independently computes the entire expected
return value (or refusal). No Python subprocess is part of the product.
"""
import copy
import importlib.util
import json
import pathlib
import sys
import unittest
from unittest import mock

root = pathlib.Path(__file__).resolve().parents[4]
package = root / "packages/codex-session-relay"
sys.path[:0] = [str(package / "src"), str(root / "packages/codex-thread-bridge/src")]
spec = importlib.util.spec_from_file_location(
    "routing_python_tests", package / "tests/__init__.py",
    submodule_search_locations=[str(package / "tests")])
tests = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = tests
spec.loader.exec_module(tests)
from routing_python_tests import test_product_routing_decisions as scenarios
from codex_session_relay import products, placement, completion, intake, routes, projects
from codex_session_relay.errors import RelayError

PROPERTIES = {
    "PRD-1": [("Validation", name) for name in (
        "test_a_registry_record_with_a_key_nobody_defined_is_refused",
        "test_an_incident_with_a_key_nobody_defined_is_refused_rather_than_kept",
        "test_a_product_is_a_plain_identifier",
        "test_the_pending_bucket_cannot_be_registered_as_a_product",
        "test_an_issue_ref_that_is_not_a_linear_identifier_is_refused",
        "test_an_empty_value_of_the_wrong_type_is_refused_not_read_as_missing",
        "test_numbers_without_json_text_are_refused_everywhere_a_scalar_is_read",
        "test_a_named_causes_signature_is_identity_bounded_and_never_rewritten")]
        + [("FinalReviewFindings", "test_evidence_is_references_bounded_in_count_shape_and_size")],
    "PRD-2-library": [("Validation", "test_a_policy_that_would_create_projects_from_counts_alone_is_refused")],
    "PRD-3": [("Validation", "test_a_follow_up_names_the_checks_it_took_over")],
    "PRD-4": [("Coverage", "test_every_surface_is_listed_and_an_unconnected_one_reads_unobserved")],
    "PRD-5": [("ResolveProduct", name) for name in unittest.defaultTestLoader.getTestCaseNames(scenarios.ResolveProduct)]
        + [("FinalReviewFindings", "test_a_declared_product_whose_repository_another_product_claims_waits")],
    "PRD-6": [("Workspace", name) for name in unittest.defaultTestLoader.getTestCaseNames(scenarios.Workspace)],
    "PRD-7": [("Decide", name) for name in unittest.defaultTestLoader.getTestCaseNames(scenarios.Decide)
              if name not in ("test_a_simulated_incident_uses_only_test_bindings_and_the_test_project",
                              "test_a_simulated_signature_never_equals_an_observed_one",
                              "test_the_same_placement_follows_from_the_same_readings")]
        + [("FinalReviewFindings", "test_the_current_issue_comes_first_and_is_linked_to_a_same_symptom_owner"),
           ("FinalReviewFindings", "test_several_covering_projects_are_held_even_with_a_triage_project")],
    "PRD-8": [("Decide", name) for name in (
        "test_a_simulated_incident_uses_only_test_bindings_and_the_test_project",
        "test_a_simulated_signature_never_equals_an_observed_one",
        "test_the_same_placement_follows_from_the_same_readings")],
    "PRD-9": [("Labels", "test_an_issue_carries_its_repository_label_and_never_the_family_label"),
        ("FoldedReviewFindings", "test_a_created_issue_owes_its_repository_label_and_an_adopted_one_does_not"),
        ("FinalReviewFindings", "test_attached_evidence_is_the_current_issues_own_record")],
    "PRD-11": [("Completion", name) for name in unittest.defaultTestLoader.getTestCaseNames(scenarios.Completion)
               if name not in ("test_a_mismatch_that_became_consistent_says_which_closure_evidence_is_missing",
                               "test_a_recurrence_after_a_fix_keeps_the_subject_flagged",
                               "test_the_verdict_never_proposes_a_state_change")],
    "PRD-12": [("Completion", name) for name in (
        "test_a_mismatch_that_became_consistent_says_which_closure_evidence_is_missing",
        "test_a_recurrence_after_a_fix_keeps_the_subject_flagged",
        "test_the_verdict_never_proposes_a_state_change")]
        + [("ClosurePending", "test_a_passing_reading_without_closure_evidence_is_not_reported_consistent")],
    "PRD-17-library": [("Classification", "test_a_classification_names_a_product_and_who_made_it")],
    "PRD-20": [("ProjectEligibility", name) for name in (
        "test_no_enabled_policy_creates_nothing",
        "test_a_create_queued_for_the_products_old_team_is_refused",
        "test_a_project_create_payload_is_checked_whatever_json_it_is",
        "test_members_count_only_as_this_products_routes_and_components_are_theirs",
        "test_members_that_declare_other_criteria_for_the_goal_do_not_count",
        "test_a_member_that_left_the_group_cancels_the_create",
        "test_a_project_bound_meanwhile_that_covers_a_member_cancels_the_create")]
        + [("FoldedReviewFindings", "test_a_project_readback_must_show_the_team_it_was_made_in")],
    "PRD-19": [("RouteRows", "test_one_decision_name_per_waiting_state"),
        ("FinalReviewFindings", "test_a_standing_cause_claim_never_hides_a_missing_project_link")],
}

# Preserve NaN/Inf in arguments as explicit values; Go reconstructs them before
# evaluating its own validator. Answers remain exact Python JSON strings.
def encode(value):
    if isinstance(value, float) and not __import__("math").isfinite(value):
        return {"$float": repr(value)}
    if isinstance(value, dict):
        return {key: encode(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [encode(item) for item in value]
    return value

records = []
def install(module, name, patches):
    original = getattr(module, name)
    def capture(*args, **kwargs):
        if name == "eligibility":
            tables = {table: [dict(row) for row in args[0].execute("SELECT * FROM " + table)]
                      for table in ("product_registry", "product_bindings", "routing_policy", "incident_routes", "route_incidents")}
            arguments = copy.deepcopy(encode([[tables, args[1]], kwargs]))
        else:
            arguments = copy.deepcopy(encode([args, kwargs]))
        try:
            result = original(*args, **kwargs)
        except RelayError as error:
            reply = {"error": "refused", "reason": error.reason.value, "detail": str(error)}
            records.append({"operation": name, "arguments": arguments,
                            "expected": json.dumps(reply, sort_keys=True, ensure_ascii=False)})
            raise
        record = {"operation": name, "arguments": arguments,
                  "expected": json.dumps(result, sort_keys=True, ensure_ascii=False)}
        if name in ("decide", "evaluate"):
            record["wire"] = json.dumps(result)
        records.append(record)
        return result
    patches.append(mock.patch.object(module, name, capture))

patches = []
for module, names in (
    (products, ("read_registry", "read_binding", "read_incident", "read_policy", "coverage", "canonical")),
    (placement, ("resolve_product", "workspace_for", "defect_signature", "pending_signature", "decide", "issue_labels", "detail_text")),
    (completion, ("read_reading", "evaluate")),
    (intake, ("_obligations", "read_classification")),
    (routes, ("attention",)),
    (projects, ("_validate", "_confirm", "eligibility")),
):
    for name in names:
        install(module, name, patches)

property_id = sys.argv[1]
for patch in patches:
    patch.start()
try:
    for class_name, method in PROPERTIES[property_id]:
        case = getattr(scenarios, class_name)(method)
        # These selected methods are pure. The inherited store fixture is not
        # involved in their assertions and must not cause unrelated disk effects.
        if class_name not in ("FoldedReviewFindings", "RouteRows"):
            case.setUp()
        try:
            getattr(case, method)()
        finally:
            case.doCleanups()
    # Source-only public branches absent from this revision's test file.
    if property_id in ("PRD-11", "PRD-12"):
        case = scenarios.Completion()
        case.setUp()
        context = {"bindings": case.bindings, "openMismatches": [], "recurrences": []}
        if property_id == "PRD-11":
            for observed in ("passed", "unobservable"):
                reading = case.reading(observed={"acceptance": observed, "handoff": "present"},
                    exceptions=[{"check": "acceptance", "kind": "scope_reduction", "ref": "decision:2026-09-22"}])
                completion.evaluate(reading, context)
            for exceptions in ([], [{"check": "acceptance", "kind": "scope_reduction", "ref": "doc#1"}]):
                completion.evaluate(case.reading(claims={"linearDone": False}, exceptions=exceptions),
                                    dict(context, openMismatches=["acceptance"]))
        else:
            completion.evaluate(case.reading(), dict(context, recurrenceUnknown="records unreadable"))
    if property_id == "PRD-19":
        for state in ("observed", "open", "fix_pending", "resolved", "withdrawn"):
            routes.attention({"stage": "filed", "disposition": "project_proposal", "hold": None,
                              "project": None, "state": state, "linkState": "none"})
finally:
    for patch in reversed(patches):
        patch.stop()
print(json.dumps(records, ensure_ascii=False))
