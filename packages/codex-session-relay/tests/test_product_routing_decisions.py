"""Product routing before the ledger: the registry, bindings, placement and completion verdicts.

Nothing here reaches the fault ledger. Placement and completion verdicts are pure functions of
readings, and the registry and bindings are this store's own rows, so every case below runs
against real validation and a real store with an injected clock. The paths that record, adopt or
target through the ledger are exercised by the scenario suite once the ledger's corrected
contract is bound; until then the adapter refuses them by name, which is asserted here.
"""

import json
import unittest

from codex_session_relay import completion, ledger_port, placement, products
from codex_session_relay.errors import RefusalReason
from codex_session_relay.routing import ProductRouter

from .support import RelayTestCase

ALPHA = {
    "schema": "product-registry/1", "product": "alpha-notes", "workspace": "jun786",
    "team": "ALN", "familyLabel": "제품:Alpha Notes",
    "repositories": ["thisisjun786/alpha-notes"],
    "surfaces": {"dev_run": {"method": "CRW managed run tool results", "active": True},
                 "verification": {"method": "CRW verification verdicts", "active": True},
                 "user_report": {"method": "reports filed through the parent", "active": True}},
    "testTarget": {"team": "TST", "project": "proj-test"},
}
BETA = {
    "schema": "product-registry/1", "product": "beta-meter", "workspace": "jun786",
    "team": "BTM", "familyLabel": "제품:Beta Meter", "repositories": ["thisisjun786/beta-meter"],
    "surfaces": {"real_use": {"method": "error events the product forwards", "active": True},
                 "user_report": {"method": "support inbox forwarded by the parent",
                                 "active": False}},
    "triageProject": "proj-btm-triage",
}


def binding(product, kind, ref, **fields):
    record = {"schema": "product-binding/1", "product": product, "kind": kind, "ref": ref,
              "title": f"{ref} title", "state": "open" if kind == "issue" else "active",
              "components": [], "source": "linear readback"}
    record.update(fields)
    return record


ALPHA_BINDINGS = [
    binding("alpha-notes", "project", "proj-aln-editor", components=["editor", "sync"]),
    binding("alpha-notes", "issue", "ALN-3", components=["editor"], project="proj-aln-editor"),
    binding("alpha-notes", "issue", "ALN-7", components=["sync"], symptoms=["conflict_lost"],
            project="proj-aln-editor"),
    binding("alpha-notes", "issue", "ALN-9", state="done", components=["editor"],
            symptoms=["save_roundtrip_mismatch"], fixRef="PR#12", project="proj-aln-editor"),
]


def incident(**fields):
    record = {"schema": "product-incident/1", "product": "alpha-notes", "surface": "dev_run",
              "phase": "development", "component": "editor", "symptom": "cursor_jump",
              "severity": "degraded", "occurrenceKey": "run-1:step-4"}
    record.update(fields)
    return products.read_incident(record)


class Validation(unittest.TestCase):
    def test_a_registry_record_with_a_key_nobody_defined_is_refused(self):
        record = dict(ALPHA, transcript="every message of the run")
        with self.assertRaises(products.RouteRefused) as caught:
            products.read_registry(record)
        self.assertEqual(RefusalReason.ROUTE_INPUT_MALFORMED, caught.exception.reason)

    def test_an_incident_with_a_key_nobody_defined_is_refused_rather_than_kept(self):
        with self.assertRaises(products.RouteRefused):
            incident(conversation="the whole chat")

    def test_the_pending_bucket_cannot_be_registered_as_a_product(self):
        with self.assertRaises(products.RouteRefused):
            products.read_registry(dict(ALPHA, product=products.UNCLASSIFIED))

    def test_an_issue_ref_that_is_not_a_linear_identifier_is_refused(self):
        with self.assertRaises(products.RouteRefused):
            products.read_binding(binding("alpha-notes", "issue", "a1b2-uuid"))

    def test_a_policy_that_would_create_projects_from_counts_alone_is_refused(self):
        base = {"schema": "routing-policy/1", "policy": "project_creation", "enabled": True,
                "minIndependentFixes": 2, "requireSharedGoal": True,
                "requireCompletionCriteria": True, "basis": "CRW-206, Jun 2026-09-22"}
        products.read_policy(base)
        for change in ({"requireSharedGoal": False}, {"requireCompletionCriteria": False},
                       {"minIndependentFixes": 1}, {"basis": ""}):
            with self.subTest(change=change), self.assertRaises(products.RouteRefused):
                products.read_policy(dict(base, **change))

    def test_a_follow_up_names_the_checks_it_took_over(self):
        read = products.read_binding(binding(
            "alpha-notes", "issue", "ALN-20",
            followUpOf=[{"issue": "ALN-9", "checks": ["install", "install"]}]))
        self.assertEqual([{"issue": "ALN-9", "checks": ["install"]}], read["followUpOf"])
        with self.assertRaises(products.RouteRefused):
            products.read_binding(binding("alpha-notes", "issue", "ALN-21",
                                          followUpOf=[{"issue": "ALN-9", "checks": []}]))


class Coverage(unittest.TestCase):
    def test_every_surface_is_listed_and_an_unconnected_one_reads_unobserved(self):
        answer = products.coverage(products.read_registry(BETA))
        self.assertEqual(set(products.SURFACES), set(answer))
        self.assertEqual("watched", answer["real_use"]["state"])
        self.assertEqual("unobserved", answer["user_report"]["state"])
        self.assertEqual("declared and switched off", answer["user_report"]["reason"])
        self.assertEqual("unobserved", answer["dev_run"]["state"])
        self.assertIsNone(answer["dev_run"]["method"])


class ResolveProduct(unittest.TestCase):
    def setUp(self):
        alpha = products.read_registry(ALPHA)
        beta = products.read_registry(BETA)
        shared = products.read_registry(dict(BETA, product="gamma-tools",
                                             repositories=["thisisjun786/shared-lib"]))
        also = products.read_registry(dict(ALPHA, product="delta-kit",
                                           repositories=["thisisjun786/shared-lib"]))
        self.registries = {r["product"]: r for r in (alpha, beta, shared, also)}

    def resolve(self, **fields):
        return placement.resolve_product(self.registries, incident(**fields))[0]

    def test_a_declared_registered_product_wins(self):
        self.assertEqual("beta-meter", self.resolve(product="beta-meter"))

    def test_a_declared_unregistered_product_resolves_to_nothing(self):
        self.assertIsNone(self.resolve(product="omega"))

    def test_a_repository_one_product_claims_names_that_product(self):
        self.assertEqual("alpha-notes",
                         self.resolve(product=None, repository="thisisjun786/alpha-notes"))

    def test_a_repository_several_products_claim_resolves_to_nothing(self):
        self.assertIsNone(self.resolve(product=None, repository="thisisjun786/shared-lib"))

    def test_a_defect_seen_in_a_crw_run_is_not_filed_under_crw(self):
        # The run is CRW-managed, but the repository is alpha-notes', so alpha-notes owns it.
        registries = dict(self.registries,
                          crw=products.read_registry(dict(ALPHA, product="crw", team="CRW",
                                                          repositories=["thisisjun786/crw"])))
        found = placement.resolve_product(
            registries, incident(product=None, repository="thisisjun786/alpha-notes"))
        self.assertEqual("alpha-notes", found[0])


class Workspace(unittest.TestCase):
    def test_a_resolved_product_files_in_its_own_workspace(self):
        registry = products.read_registry(ALPHA)
        self.assertEqual("jun786", placement.workspace_for(incident(), registry))

    def test_an_incident_claiming_another_workspace_for_a_product_is_refused(self):
        registry = products.read_registry(ALPHA)
        with self.assertRaises(products.RouteRefused):
            placement.workspace_for(incident(workspace="elsewhere"), registry)

    def test_an_unresolved_incident_keeps_its_workspace_or_the_sentinel(self):
        self.assertEqual("acme", placement.workspace_for(incident(workspace="acme")))
        self.assertEqual(placement.UNASSIGNED, placement.workspace_for(incident()))

    def test_two_workspaces_give_two_pending_identities(self):
        first, second = incident(workspace="acme"), incident(workspace="umbrella")
        self.assertEqual(placement.pending_signature(first), placement.pending_signature(second))
        self.assertNotEqual(placement.workspace_for(first), placement.workspace_for(second))


class Decide(unittest.TestCase):
    def setUp(self):
        self.registry = products.read_registry(ALPHA)
        self.bindings = [products.read_binding(b, self.registry) for b in ALPHA_BINDINGS]

    def decide(self, *, bindings=None, registry=None, run_issue=None, **fields):
        return placement.decide(incident(**fields), registry or self.registry,
                                self.bindings if bindings is None else bindings, run_issue)

    def test_the_current_issue_takes_evidence_from_its_own_run(self):
        answer = self.decide(context={"currentIssue": "ALN-3", "run": "rel-1"},
                             run_issue="ALN-3")
        self.assertEqual(products.ATTACH_CURRENT, answer["disposition"])
        self.assertEqual("ALN-3", answer["owner"])
        self.assertEqual("proj-aln-editor", answer["project"])

    def test_a_run_recorded_for_another_issue_attaches_nothing(self):
        answer = self.decide(context={"currentIssue": "ALN-3", "run": "rel-2"},
                             run_issue="ALN-4")
        self.assertEqual(products.NEW_ISSUE, answer["disposition"])
        self.assertIn("belongs to ALN-4", answer["reason"])

    def test_a_run_this_store_does_not_know_attaches_nothing(self):
        answer = self.decide(context={"currentIssue": "ALN-3", "run": "rel-9"}, run_issue=None)
        self.assertEqual(products.NEW_ISSUE, answer["disposition"])

    def test_a_user_report_is_never_attached_to_a_current_issue(self):
        answer = self.decide(surface="user_report", context={"currentIssue": "ALN-3",
                                                             "run": "rel-1"},
                             run_issue="ALN-3")
        self.assertEqual(products.NEW_ISSUE, answer["disposition"])

    def test_a_symptom_another_open_issue_owns_goes_there_even_with_a_current_issue(self):
        answer = self.decide(component="sync", symptom="conflict_lost",
                             context={"currentIssue": "ALN-3", "run": "rel-1"},
                             run_issue="ALN-3")
        self.assertEqual(products.ACCUMULATE, answer["disposition"])
        self.assertEqual("ALN-7", answer["owner"])

    def test_a_shared_component_alone_never_merges_two_defects(self):
        answer = self.decide(component="sync", symptom="icon_blurry")
        self.assertEqual(products.NEW_ISSUE, answer["disposition"])
        self.assertIsNone(answer["owner"])

    def test_several_open_owners_are_held_rather_than_one_chosen(self):
        extra = products.read_binding(binding("alpha-notes", "issue", "ALN-8",
                                              components=["sync"], symptoms=["conflict_lost"],
                                              project="proj-aln-editor"), self.registry)
        answer = self.decide(bindings=self.bindings + [extra], component="sync",
                             symptom="conflict_lost")
        self.assertEqual(products.HELD, answer["disposition"])
        self.assertEqual(products.AMBIGUOUS_OWNER, answer["hold"])

    def test_the_same_defect_after_completion_reopens_its_issue(self):
        answer = self.decide(symptom="save_roundtrip_mismatch")
        self.assertEqual(products.REOPEN, answer["disposition"])
        self.assertTrue(answer["reopen"])
        self.assertEqual("ALN-9", answer["owner"])

    def test_a_regression_of_a_fix_is_a_follow_up_linked_to_the_fixed_issue(self):
        # A different symptom from the completed issue's own, so this is not a reopen.
        answer = self.decide(symptom="undo_lost", context={"regressionOf": "PR#12"})
        self.assertEqual(products.FOLLOW_UP, answer["disposition"])
        self.assertEqual(["ALN-9"], answer["relate"])
        self.assertEqual("proj-aln-editor", answer["project"])

    def test_a_regression_naming_no_known_fix_links_nothing(self):
        answer = self.decide(symptom="undo_lost", context={"regressionOf": "PR#999"})
        self.assertEqual(products.NEW_ISSUE, answer["disposition"])
        self.assertEqual([], answer["relate"])

    def test_several_covering_projects_are_decided_by_goal_or_held(self):
        other = products.read_binding(binding("alpha-notes", "project", "proj-aln-mobile",
                                              components=["editor"], goal="mobile"),
                                      self.registry)
        with_goal = self.decide(bindings=self.bindings + [other],
                                goal={"key": "mobile", "criteria": "works on phones"})
        self.assertEqual("proj-aln-mobile", with_goal["project"])
        without = self.decide(bindings=self.bindings + [other])
        self.assertEqual(products.HELD, without["disposition"])
        self.assertEqual(products.AMBIGUOUS_PROJECT, without["hold"])

    def test_no_covering_project_uses_triage_else_holds(self):
        beta = products.read_registry(BETA)
        answer = placement.decide(incident(product="beta-meter", surface="real_use",
                                           component="ingest"), beta, [])
        self.assertEqual(products.NEW_ISSUE, answer["disposition"])
        self.assertEqual("proj-btm-triage", answer["project"])
        held = self.decide(bindings=[], component="search")
        self.assertEqual(products.NO_PROJECT, held["hold"])
        self.assertIsNone(held["project"])

    def test_an_owner_without_a_project_is_held_when_there_is_no_triage(self):
        loose = products.read_binding(binding("alpha-notes", "issue", "ALN-11",
                                              components=["sync"], symptoms=["drop"]),
                                      self.registry)
        answer = self.decide(bindings=[loose], component="sync", symptom="drop")
        self.assertEqual(products.OWNER_PROJECT_MISSING, answer["hold"])
        self.assertEqual("ALN-11", answer["owner"])

    def test_an_expected_state_is_observed_and_never_filed(self):
        for expected in products.EXPECTED:
            with self.subTest(expected=expected):
                answer = self.decide(expected=expected)
                self.assertEqual(products.OBSERVE, answer["disposition"])
                self.assertEqual(products.STAGE_OBSERVED, answer["stage"])

    def test_a_simulated_incident_uses_only_test_bindings_and_the_test_project(self):
        test_issue = products.read_binding(binding(
            "alpha-notes", "issue", "TST-5", components=["editor"], symptoms=["cursor_jump"],
            project="proj-test", test=True), self.registry)
        answer = self.decide(bindings=self.bindings + [test_issue], origin="simulated")
        self.assertEqual(products.ACCUMULATE, answer["disposition"])
        self.assertEqual("TST-5", answer["owner"])
        self.assertEqual("proj-test", answer["project"])
        real_only = self.decide(origin="simulated", symptom="save_roundtrip_mismatch")
        self.assertEqual(products.NEW_ISSUE, real_only["disposition"])
        self.assertEqual("proj-test", real_only["project"])

    def test_a_simulated_signature_never_equals_an_observed_one(self):
        self.assertNotEqual(placement.defect_signature(incident()),
                            placement.defect_signature(incident(origin="simulated")))

    def test_the_same_placement_follows_from_the_same_readings(self):
        first = self.decide(symptom="undo_lost", context={"regressionOf": "PR#12"})
        again = self.decide(symptom="undo_lost", context={"regressionOf": "PR#12"})
        self.assertEqual(first, again)


class Labels(unittest.TestCase):
    def test_an_issue_carries_its_repository_label_and_never_the_family_label(self):
        labels = placement.issue_labels(incident(repository="thisisjun786/alpha-notes"))
        self.assertEqual(["thisisjun786/alpha-notes"], labels)
        self.assertNotIn(ALPHA["familyLabel"], labels)


class RegistryStore(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.router = ProductRouter(self.store, self.clock)
        self.router.register_product(ALPHA)

    def test_a_binding_for_an_unregistered_product_is_refused(self):
        with self.assertRaises(products.RouteRefused) as caught:
            self.router.bind(binding("omega", "project", "proj-x"))
        self.assertEqual(RefusalReason.ROUTE_PRODUCT_UNKNOWN, caught.exception.reason)

    def test_a_test_binding_off_the_test_target_is_refused(self):
        with self.assertRaises(products.RouteRefused):
            self.router.bind(binding("alpha-notes", "issue", "ALN-3", project="proj-aln-editor",
                                     components=["editor"], test=True))
        with self.assertRaises(products.RouteRefused):
            self.router.bind(binding("alpha-notes", "issue", "ALN-50", project="proj-test",
                                     components=["editor"], test=True))
        stored = self.router.bind(binding("alpha-notes", "issue", "TST-5", project="proj-test",
                                          components=["editor"], test=True))
        self.assertTrue(stored["test"])

    def test_a_rebind_replaces_the_snapshot(self):
        self.router.bind(binding("alpha-notes", "issue", "ALN-3", components=["editor"]))
        self.router.bind(binding("alpha-notes", "issue", "ALN-3", components=["editor"],
                                 state="done"))
        states = [b["state"] for b in self.router.bindings("alpha-notes")]
        self.assertEqual(["done"], states)

    def test_show_lists_bindings_coverage_and_the_absent_policy(self):
        self.router.bind(binding("alpha-notes", "project", "proj-aln-editor",
                                 components=["editor"]))
        shown = self.router.show_products("alpha-notes")
        entry = shown["products"][0]
        self.assertEqual("unobserved", entry["coverage"]["real_use"]["state"])
        self.assertEqual(["proj-aln-editor"], [b["ref"] for b in entry["bindings"]])
        self.assertIsNone(shown["policy"])

    def test_the_run_issue_is_read_from_the_relays_own_relationship(self):
        self.register(issue_key="ALN-3")
        run = self.store.one("SELECT relationship_id FROM relationships WHERE issue_key = ?",
                             ("ALN-3",))["relationship_id"]
        self.assertEqual("ALN-3", self.router.run_issue(run))
        self.assertIsNone(self.router.run_issue("rel-unknown"))


class Completion(unittest.TestCase):
    def setUp(self):
        self.registry = products.read_registry(ALPHA)
        self.bindings = {b["ref"]: b for b in (
            products.read_binding(binding(
                "alpha-notes", "issue", "ALN-20", components=["editor"],
                followUpOf=[{"issue": "ALN-9", "checks": ["install"]}]), self.registry),
            products.read_binding(binding(
                "alpha-notes", "issue", "ALN-21", components=["editor"],
                followUpOf=[{"issue": "ALN-9", "checks": ["handoff"]}]), self.registry),
            products.read_binding(binding(
                "alpha-notes", "issue", "ALN-22", state="done", components=["editor"],
                followUpOf=[{"issue": "ALN-9", "checks": ["install"]}]), self.registry),
            products.read_binding(binding("alpha-notes", "issue", "ALN-23",
                                          components=["editor"]), self.registry),
        )}

    def reading(self, **fields):
        record = {"schema": "completion-reading/1", "product": "alpha-notes", "subject": "ALN-9",
                  "claims": {"linearDone": True, "prMerged": True, "sessionEnded": True},
                  "requires": {"acceptance": True, "install": False, "realUse": False,
                               "handoff": True},
                  "observed": {"acceptance": "passed", "handoff": "present"}}
        record.update(fields)
        return completion.read_reading(record)

    def evaluate(self, reading, *, open_mismatches=(), recurrences=()):
        return completion.evaluate(reading, {"bindings": self.bindings,
                                             "openMismatches": list(open_mismatches),
                                             "recurrences": list(recurrences)})

    def verdicts(self, answer):
        return {entry["check"]: entry["verdict"] for entry in answer["checks"]}

    def test_a_legitimate_done_is_consistent_and_asks_for_nothing(self):
        answer = self.evaluate(self.reading())
        self.assertEqual(completion.CONSISTENT, answer["verdict"])
        self.assertEqual([], answer["recurrences"])

    def test_deployment_is_never_assumed_mandatory(self):
        answer = self.evaluate(self.reading())
        self.assertEqual(completion.CONSISTENT, self.verdicts(answer)["install"])

    def test_done_without_acceptance_evidence_is_a_mismatch(self):
        for observed in ("absent", "failed"):
            with self.subTest(observed=observed):
                answer = self.evaluate(self.reading(observed={"acceptance": observed,
                                                              "handoff": "present"}))
                self.assertEqual(completion.MISMATCH, answer["verdict"])
                self.assertEqual(completion.MISMATCH, self.verdicts(answer)["acceptance"])

    def test_merged_without_a_required_install_result_is_a_mismatch(self):
        reading = self.reading(requires={"acceptance": True, "install": True, "realUse": False,
                                         "handoff": True},
                               observed={"acceptance": "passed", "install": "absent",
                                         "handoff": "present"})
        self.assertEqual(completion.MISMATCH, self.verdicts(self.evaluate(reading))["install"])

    def test_an_ended_session_without_its_handoff_is_a_mismatch(self):
        reading = self.reading(observed={"acceptance": "passed", "handoff": "absent"})
        self.assertEqual(completion.MISMATCH, self.verdicts(self.evaluate(reading))["handoff"])

    def test_unobservable_and_unknown_are_unverified_rather_than_guessed(self):
        reading = self.reading(requires={"acceptance": True, "install": "unknown",
                                         "realUse": False, "handoff": True},
                               observed={"acceptance": "unobservable", "handoff": "present"})
        answer = self.evaluate(reading)
        self.assertEqual(completion.UNVERIFIED, answer["verdict"])
        self.assertEqual(completion.UNVERIFIED, self.verdicts(answer)["acceptance"])
        self.assertEqual(completion.UNVERIFIED, self.verdicts(answer)["install"])

    def test_nothing_claimed_is_not_applicable(self):
        reading = self.reading(claims={"linearDone": False, "prMerged": False,
                                       "sessionEnded": False})
        self.assertEqual({completion.NOT_APPLICABLE},
                         set(self.verdicts(self.evaluate(reading)).values()))

    def test_a_requirement_dropped_after_a_mismatch_leaves_the_mismatch_open(self):
        answer = self.evaluate(self.reading(), open_mismatches=["install"])
        self.assertEqual(completion.REQUIREMENT_CHANGED, self.verdicts(answer)["install"])
        self.assertEqual(completion.MISMATCH, answer["verdict"])

    def test_an_approved_scope_reduction_is_an_exception(self):
        reading = self.reading(exceptions=[{"check": "install", "kind": "scope_reduction",
                                            "ref": "decision:2026-09-22",
                                            "approvedBy": "Jun"}])
        answer = self.evaluate(reading, open_mismatches=["install"])
        self.assertEqual(completion.EXCEPTED, self.verdicts(answer)["install"])
        self.assertEqual(completion.CONSISTENT, answer["verdict"])

    def test_a_scope_reduction_nobody_approved_is_unverified(self):
        reading = self.reading(exceptions=[{"check": "install", "kind": "scope_reduction",
                                            "ref": "decision:2026-09-22"}])
        answer = self.evaluate(reading, open_mismatches=["install"])
        self.assertEqual(completion.EXCEPTION_UNVERIFIED, self.verdicts(answer)["install"])
        self.assertEqual(completion.MISMATCH, answer["verdict"])

    def test_a_follow_up_counts_only_for_the_check_it_took_over(self):
        def follow(ref, check="install"):
            reading = self.reading(requires={"acceptance": True, "install": True,
                                             "realUse": False, "handoff": True},
                                   observed={"acceptance": "passed", "install": "absent",
                                             "handoff": "present"},
                                   exceptions=[{"check": check, "kind": "follow_up",
                                                "ref": ref}])
            return self.verdicts(self.evaluate(reading))[check]
        self.assertEqual(completion.EXCEPTED, follow("ALN-20"))
        self.assertEqual(completion.EXCEPTION_UNVERIFIED, follow("ALN-21"))  # took handoff only
        self.assertEqual(completion.EXCEPTION_UNVERIFIED, follow("ALN-22"))  # already done
        self.assertEqual(completion.EXCEPTION_UNVERIFIED, follow("ALN-23"))  # no relation
        self.assertEqual(completion.EXCEPTION_UNVERIFIED, follow("ALN-9"))   # itself
        self.assertEqual(completion.EXCEPTION_UNVERIFIED, follow("ALN-77"))  # not bound

    def test_a_mismatch_that_became_consistent_says_which_closure_evidence_is_missing(self):
        reading = self.reading(evidence={"acceptance": {"fix": {"ref": "PR#31",
                                                                "source": "github"}}})
        entry = next(e for e in self.evaluate(reading, open_mismatches=["acceptance"])["checks"]
                     if e["check"] == "acceptance")
        self.assertEqual(completion.CONSISTENT, entry["verdict"])
        self.assertFalse(entry["closure"]["ready"])
        self.assertEqual(["verification"], entry["closure"]["missing"])

    def test_a_recurrence_after_a_fix_keeps_the_subject_flagged(self):
        answer = self.evaluate(self.reading(), recurrences=[{"faultId": "f" * 32,
                                                             "reason": "occurred after fix"}])
        self.assertEqual(completion.MISMATCH, answer["verdict"])
        self.assertEqual("f" * 32, answer["recurrences"][0]["faultId"])

    def test_the_verdict_never_proposes_a_state_change(self):
        answer = self.evaluate(self.reading(observed={"acceptance": "failed",
                                                      "handoff": "present"}))
        self.assertEqual({"subject", "product", "origin", "verdict", "checks", "recurrences"},
                         set(answer))


class LedgerPortBeforeBinding(RelayTestCase):
    def test_every_ledger_capability_refuses_by_name_until_the_contract_is_bound(self):
        port = ledger_port.LedgerPort(self.store, self.clock)
        for name in ledger_port.CAPABILITIES:
            with self.subTest(capability=name), self.assertRaises(products.RouteRefused) as caught:
                getattr(port, name)("anything")
            self.assertEqual(RefusalReason.ROUTE_LEDGER_PENDING, caught.exception.reason)
            self.assertIn(name, str(caught.exception))
        before = self.store.one("SELECT COUNT(*) AS n FROM fault_ledger")["n"]
        self.assertEqual(0, before)

    def test_routing_modules_reach_the_ledger_only_through_the_port(self):
        import ast
        import pathlib

        source = pathlib.Path(ledger_port.__file__).parent
        for name in ("products.py", "placement.py", "routing.py", "completion.py"):
            tree = ast.parse((source / name).read_text(encoding="utf-8"))
            imported = {node.module for node in ast.walk(tree)
                        if isinstance(node, ast.ImportFrom) and node.module}
            imported |= {alias.name for node in ast.walk(tree) if isinstance(node, ast.ImportFrom)
                         for alias in node.names}
            with self.subTest(module=name):
                self.assertNotIn("faults", imported)
                self.assertNotIn("faultsweep", imported)


class CommandLine(RelayTestCase):
    """In process, like test_faults.CommandLine, so this measures no wall time."""

    def invoke(self, *argv):
        import contextlib
        import io

        from codex_session_relay import cli

        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = cli.main(["--state", str(self.store.path.parent), *argv])
        return code, json.loads(buffer.getvalue())

    def test_a_product_is_registered_bound_and_shown_from_the_command_line(self):
        code, registered = self.invoke("product-register", "--record", json.dumps(ALPHA))
        self.assertEqual(0, code)
        self.assertEqual("alpha-notes", registered["product"])
        record = self.artifact("binding.json", json.dumps(ALPHA_BINDINGS[0]))
        code, bound = self.invoke("product-bind", "--record", "@" + record)
        self.assertEqual(0, code)
        self.assertEqual("proj-aln-editor", bound["ref"])
        code, shown = self.invoke("product-show", "--product", "alpha-notes")
        self.assertEqual(0, code)
        entry = shown["products"][0]
        self.assertEqual("watched", entry["coverage"]["dev_run"]["state"])
        self.assertEqual("unobserved", entry["coverage"]["real_use"]["state"])
        self.assertEqual(["proj-aln-editor"], [b["ref"] for b in entry["bindings"]])

    def test_a_binding_for_an_unregistered_product_is_a_refusal(self):
        code, refusal = self.invoke("product-bind", "--record", json.dumps(ALPHA_BINDINGS[0]))
        self.assertEqual(2, code)
        self.assertEqual("route_product_unknown", refusal["reason"])

    def test_unreadable_json_and_a_missing_file_are_refusals_not_outages(self):
        for argument in ("{not json", "@" + self.tmp + "/absent.json"):
            with self.subTest(argument=argument):
                code, refusal = self.invoke("product-register", "--record", argument)
                self.assertEqual(2, code)
                self.assertEqual("route_input_malformed", refusal["reason"])

    def test_a_policy_without_its_basis_is_refused(self):
        policy = {"schema": "routing-policy/1", "policy": "project_creation", "enabled": True,
                  "minIndependentFixes": 2, "requireSharedGoal": True,
                  "requireCompletionCriteria": True, "basis": ""}
        code, refusal = self.invoke("route-policy", "--record", json.dumps(policy))
        self.assertEqual(2, code)
        self.assertEqual("route_input_malformed", refusal["reason"])
        code, stored = self.invoke("route-policy", "--record",
                                   json.dumps(dict(policy, basis="CRW-206, Jun 2026-09-22")))
        self.assertEqual(0, code)
        self.assertEqual("CRW-206, Jun 2026-09-22", stored["basis"])


if __name__ == "__main__":
    unittest.main()
