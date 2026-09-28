"""Capture original route-row scenarios as operations plus whole outputs and persisted bytes."""
import copy
import importlib.util
import json
import pathlib
import sys
from unittest import mock

root = pathlib.Path(__file__).resolve().parents[4]
package = root / "packages/codex-session-relay"
sys.path[:0] = [str(package / "src"), str(root / "packages/codex-thread-bridge/src")]
spec = importlib.util.spec_from_file_location("routing_python_tests", package / "tests/__init__.py", submodule_search_locations=[str(package / "tests")])
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
from routing_python_tests import test_product_routing_decisions as scenarios
from codex_session_relay import routes, completion, intake
from codex_session_relay.routing import ProductRouter
from codex_session_relay.errors import RelayError

properties = {
    "PRD-18": [("RouteRows", "test_a_route_keeps_its_newest_incidents_and_its_highest_claimed_severity"),
        ("RouteRows", "test_listing_pages_by_a_stable_cursor"),
        ("RouteRows", "test_a_target_nobody_wrote_is_refused_rather_than_guessed"),
        ("ProposalRotation", "test_a_goal_is_replaced_when_written_and_kept_when_not")],
    "PRD-21-replay": [("FoldedReviewFindings", "test_a_reading_a_closed_round_recorded_is_recognised_when_handed_in_again"), ("FoldedReviewFindings", "test_reconcile_reaches_past_its_first_page")],
    "PRD-22": [("ProposalRotation", "test_each_check_moves_a_proposal_to_the_back_so_every_one_is_reached"),
        ("ProposalRotation", "test_what_is_not_reached_is_answered_exactly_and_without_a_cap")],
}
records = []
mutations = {"upsert": 2, "store_incident": 2, "checked": 1, "settle": 2}
reads = {"get": 1, "incidents": 1, "listing": 1, "outstanding_proposals": 1,
         "unreached_proposal": 1, "unreached_count": 1, "_replayed": 1}

def capture(name, original):
    def wrapped(*args, **kwargs):
        start = mutations.get(name, reads.get(name))
        call = {"operation": name, "args": copy.deepcopy(args[start:]), "kwargs": copy.deepcopy(kwargs)}
        if name in mutations and name != "checked":
            call["stamp"] = args[1].iso()
        try:
            value = original(*args, **kwargs)
        except RelayError as error:
            call["expected"] = json.dumps({"error": "refused", "reason": error.reason.value, "detail": str(error)}, sort_keys=True, ensure_ascii=False)
            records.append(call)
            raise
        call["expected"] = json.dumps(value, sort_keys=True, ensure_ascii=False)
        records.append(call)
        return value
    return wrapped

for class_name, name in properties[sys.argv[1]]:
    case = getattr(scenarios, class_name)(name)
    # The replay property uses only route rows, not its class's gated router fixture.
    scenarios.RouteRows.setUp(case)
    case.router = ProductRouter(case.store,case.clock)
    records.append({"operation": "reset", "args": [], "kwargs": {}, "expected": "null"})
    patches = []
    for method in {**mutations, **reads}:
        owner = completion if method == "_replayed" else routes
        patches.append(mock.patch.object(owner, method, capture(method, getattr(owner, method))))
    if name == 'test_reconcile_reaches_past_its_first_page':
        original_reconcile = intake.reconcile
        def reconcile(router, **kwargs):
            start = len(records)
            answer = original_reconcile(router, **kwargs)
            del records[start:]
            records.append({'operation':'reconcile','args':[],'kwargs':kwargs,'expected':json.dumps(answer,sort_keys=True,ensure_ascii=False)})
            return answer
        patches.append(mock.patch.object(intake,'reconcile',reconcile))
    # The unknown-target case deliberately corrupts a row outside the route API.
    # Record the same literal corruption immediately before its reader is called.
    if name == "test_a_target_nobody_wrote_is_refused_rather_than_guessed":
        original_get = routes.get
        def corrupt_get(store, fault):
            row = store.one("SELECT target FROM incident_routes WHERE fault_id=?", (fault,))
            records.append({"operation": "sql", "args": ["UPDATE incident_routes SET target = ? WHERE fault_id = ?", [row["target"], fault]], "kwargs": {}, "expected": "null"})
            return original_get(store, fault)
        patches.append(mock.patch.object(routes, "get", capture("get", corrupt_get)))
    for patch in patches:
        patch.start()
    try:
        getattr(case, name)()
        # Every table in the schema is compared. Routing writes cannot silently
        # gain/lose a journal, refusal, or shared table row.
        tables = {}
        names = [r[0] for r in case.store.db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='schema_meta' ORDER BY name")]
        for table in names:
            tables[table] = [dict(row) for row in case.store.db.execute('SELECT * FROM "' + table + '" ORDER BY rowid')]
        records.append({"operation": "tables", "args": [], "kwargs": {}, "expected": json.dumps(tables, sort_keys=True, ensure_ascii=False)})
    finally:
        for patch in reversed(patches):
            patch.stop()
        case.doCleanups()
print(json.dumps(records, ensure_ascii=False))
