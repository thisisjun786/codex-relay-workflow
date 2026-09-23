"""Product routing against the real fault ledger: the CRW-206 acceptance matrix.

A FakeLinear designated test target stands in for the tracker, and a Holder carries every
publication the ledger hands out through the ledger's own holder protocol (next, claim,
operation, complete, fail, reconcile) with injected losses. Two products that are not CRW:
alpha-notes, in development (team ALN, project proj-aln-editor, issues ALN-3/7/9), and
beta-meter, in use (team BTM, triage project proj-btm-triage). Every incident here is a
simulated EVENT fed to routing; what these tests observe is the ledger's and FakeLinear's
state, never a real product. The live readback on a designated Linear test target is the
coordination parent's obligation and is not claimed here.
"""

import json
import re
import unittest

from codex_session_relay import faults, products, projects, routes
from codex_session_relay.routing import ProductRouter

from .support import RelayTestCase

WORKSPACE = "example-ws"


def registry(product, team, **fields):
    record = {"schema": "product-registry/1", "product": product, "workspace": WORKSPACE,
              "team": team, "familyLabel": f"product:{product}",
              "repositories": [f"example-org/{product}"], "surfaces": {}}
    record.update(fields)
    return record


ALPHA = registry("alpha-notes", "ALN", surfaces={
    "dev_run": {"method": "CRW managed run tool results", "active": True},
    "verification": {"method": "CRW verification verdicts", "active": True},
    "user_report": {"method": "reports forwarded by the parent", "active": True}},
    testTarget={"team": "TST", "project": "proj-test"})
BETA = registry("beta-meter", "BTM", surfaces={
    "real_use": {"method": "error events the product forwards", "active": True},
    "user_report": {"method": "support inbox", "active": False}},
    triageProject="proj-btm-triage")
GAMMA = registry("gamma-kit", "GMK", surfaces={
    "real_use": {"method": "error events the product forwards", "active": True}})
POLICY = {"schema": "routing-policy/1", "policy": "project_creation", "enabled": True,
          "minIndependentFixes": 2, "requireSharedGoal": True,
          "requireCompletionCriteria": True, "basis": "CRW-206, Jun 2026-09-22"}


def binding(product, kind, ref, **fields):
    record = {"schema": "product-binding/1", "product": product, "kind": kind, "ref": ref,
              "title": ref, "state": "open" if kind == "issue" else "active",
              "components": [], "source": "linear readback"}
    record.update(fields)
    return record


def incident(**fields):
    record = {"schema": "product-incident/1", "product": "alpha-notes",
              "repository": "example-org/alpha-notes", "surface": "dev_run",
              "phase": "development", "component": "editor", "symptom": "cursor_jump",
              "severity": "broken", "occurrenceKey": "run-1:step-1",
              "detail": {"impact": "the cursor jumps", "expected": "stays",
                         "actual": "jumps", "reproduction": "type fast"}}
    record.update(fields)
    return record


class FakeLinear:
    """The designated test target: issues and projects with ids, what a holder reads back."""

    def __init__(self):
        self.issues, self.projects, self.comments = {}, {}, []
        self.wrong_project = None

    def create_issue(self, team, project, text):
        ref = f"{team}-{100 + len(self.issues)}"
        self.issues[ref] = {"team": team, "project": self.wrong_project or project,
                            "text": text, "state": "open", "relations": [], "labels": []}
        return ref

    def create_project(self, team, text):
        ref = f"proj-new-{len(self.projects) + 1}"
        self.projects[ref] = {"team": team, "text": text}
        return ref

    def find(self, marker):
        """(ref, where) of the issue or project whose body carries the marker, or (None, None)."""
        for ref, issue in self.issues.items():
            if marker in issue["text"]:
                return ref, issue
        for ref, project in self.projects.items():
            if marker in project["text"]:
                return ref, project
        return None, None

    def search(self, marker):
        """Every body that carries the marker, as a holder's attested search would read."""
        bodies = [i["text"] for i in self.issues.values()]
        bodies += [p["text"] for p in self.projects.values()]
        bodies += [c["text"] for c in self.comments]
        return "\n".join(body for body in bodies if marker in body)


class Holder:
    """The credential holder, in process, driving only the ledger's holder protocol.

    One place reads the operation's fields, so a shape the ledger's implementation settles on
    is corrected here once.
    """

    def __init__(self, ledger, linear):
        self.ledger, self.linear = ledger, linear
        self.lose_next_response = False

    def pending(self):
        return self.ledger.next(limit=50)

    def run(self, rounds=5):
        done = []
        for _ in range(rounds):
            rows = self.pending()
            if not rows:
                return done
            for row in rows:
                done.append(self.carry(row))
        return done

    def carry(self, row):
        publication = row.get("publicationId") or row.get("publication_id")
        claim = self.ledger.claim(publication, owner="holder")
        token = claim["claimToken"]
        operation = self.ledger.operation(publication, claim_token=token)
        kind = operation["kind"]
        block = operation.get("block")
        if kind == "open_record":
            ref = self.linear.create_issue(operation["trackerRef"], operation["projectRef"], block)
            project = self.linear.issues[ref]["project"]
            if self.lose(publication):
                return {"publication": publication, "outcome": "lost", "ref": ref}
            return self.ledger.complete(publication, claim_token=token, readback=block,
                                        external_ref=ref, project_ref=project)
        if kind == projects.KIND:
            team = operation["payload"]["team"]
            ref = self.linear.create_project(team, block)
            if self.lose(publication):
                return {"publication": publication, "outcome": "lost", "ref": ref}
            return self.ledger.complete(publication, claim_token=token, readback=block,
                                        external_ref=ref, observed={"team": team})
        if kind == "append_comment":
            self.linear.comments.append({"issue": operation["externalRef"], "text": block})
            return self.ledger.complete(publication, claim_token=token, readback=block)
        if kind == "update_record":
            issue = self.linear.issues.setdefault(operation["externalRef"], {
                "team": None, "project": None, "text": "", "state": "done", "relations": [],
                "labels": []})
            self.apply(issue, operation)
            return self.ledger.complete(publication, claim_token=token,
                                        observed=self.observe(operation["externalRef"]))
        raise AssertionError(f"unexpected kind {kind}")

    def lose(self, publication):
        if self.lose_next_response:
            self.lose_next_response = False
            return True
        return False

    def recover(self, publication):
        """After a lost response: search the target for the write's marker, reconcile, and
        complete from what was found, as the ledger's protocol asks of an uncertain create."""
        marker = faults.start_marker(publication)
        text = self.linear.search(marker)
        answer = self.ledger.reconcile(publication, observed_text=text, searched=True)
        if answer["outcome"] != "present":
            return answer
        ref, found = self.linear.find(marker)
        return self.ledger.complete(publication, readback=found["text"], external_ref=ref,
                                    project_ref=found.get("project"),
                                    observed={"team": found.get("team")})

    @staticmethod
    def apply(issue, operation):
        update = operation.get("update") or {}
        op, value = update.get("op"), update.get("value")
        if op == "set_project":
            issue["project"] = value
        elif op == "reopen":
            issue["state"] = "open"
        elif op == "add_relation":
            issue["relations"].append(value)
        elif op == "add_label":
            issue["labels"].append(value)

    def observe(self, ref):
        issue = self.linear.issues[ref]
        return {"issue": ref, "projectId": issue["project"], "open": issue["state"] == "open",
                "relations": issue["relations"], "labels": issue["labels"]}


class ProductRoutingCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.router = ProductRouter(self.store, self.clock)
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.linear = FakeLinear()
        self.holder = Holder(self.ledger, self.linear)
        for record in (ALPHA, BETA, GAMMA):
            self.router.register_product(record)
        for record in (
                binding("alpha-notes", "project", "proj-aln-editor",
                        components=["editor", "sync"]),
                binding("alpha-notes", "issue", "ALN-3", components=["editor"],
                        project="proj-aln-editor"),
                binding("alpha-notes", "issue", "ALN-7", components=["sync"],
                        symptoms=["conflict_lost"], project="proj-aln-editor"),
                binding("alpha-notes", "issue", "ALN-9", state="done", components=["editor"],
                        symptoms=["save_mismatch"], fixRef="PR#12", project="proj-aln-editor")):
            self.router.bind(record)

    def route(self, **fields):
        return self.router.intake(incident(**fields))

    def issues_of(self, team):
        return {ref: i for ref, i in self.linear.issues.items() if i["team"] == team}


class TwoProductsTwoPhases(ProductRoutingCase):
    def test_a_development_defect_lands_in_its_team_and_existing_project(self):
        answer = self.route(component="sync", symptom="merge_drop")
        self.assertEqual(("new_issue", "proj-aln-editor"), (answer["disposition"],
                                                            answer["project"]))
        self.holder.run()
        (ref, issue), = self.issues_of("ALN").items()
        self.assertEqual("proj-aln-editor", issue["project"])
        self.assertEqual("linked", self.router.port.get(answer["faultId"])["linkState"])
        self.assertEqual({}, self.issues_of("CRW"))

    def test_an_in_use_defect_lands_in_its_team_and_triage_project(self):
        answer = self.route(product="beta-meter", repository="example-org/beta-meter",
                             surface="real_use", phase="in_use", component="billing",
                             symptom="charge_twice")
        self.holder.run()
        (ref, issue), = self.issues_of("BTM").items()
        self.assertEqual("proj-btm-triage", issue["project"])
        self.assertEqual(ref, self.router.port.get(answer["faultId"])["external_ref"])



class ControlGroups(ProductRoutingCase):
    def test_a_transient_error_files_no_issue(self):
        self.route(severity="degraded", occurrenceKey="once")
        self.holder.run()
        self.assertEqual({}, self.linear.issues)

    def test_the_same_incident_n_times_is_one_tracking_item(self):
        answers = [self.route(occurrenceKey=f"run-{n}") for n in range(6)]
        self.assertEqual(1, len({a["faultId"] for a in answers}))
        self.holder.run()
        self.assertEqual(1, len(self.linear.issues))
        again = [self.route(occurrenceKey="run-0") for _ in range(3)]
        self.assertEqual({False}, {a["recorded"] for a in again})

    def test_distinct_causes_split_and_similar_wording_in_two_products_stays_two(self):
        one = self.route(symptom="cursor_jump")
        two = self.route(symptom="selection_lost")
        beta = self.route(product="beta-meter", repository="example-org/beta-meter",
                           surface="real_use", phase="in_use", symptom="cursor_jump")
        self.assertEqual(3, len({one["faultId"], two["faultId"], beta["faultId"]}))
        self.holder.run()
        self.assertEqual(2, len(self.issues_of("ALN")))
        self.assertEqual(1, len(self.issues_of("BTM")))

    def test_a_single_severe_incident_is_escalated_at_once(self):
        answer = self.route(severity="broken", occurrenceKey="only-once")
        self.assertIsNotNone(answer["publication"])
        pending = self.router.port.notifications(state="pending", limit=50)
        rows = pending.get("notifications", pending) if isinstance(pending, dict) else pending
        self.assertTrue(any(n.get("fault_id", n.get("faultId")) == answer["faultId"]
                            for n in rows))

    def test_a_lost_create_response_makes_no_duplicate(self):
        answer = self.route()
        self.holder.lose_next_response = True
        self.holder.run(rounds=1)
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.ledger.expire_leases()
        (publication,) = self.router.port.publications(answer["faultId"], kind="open_record")
        pid = publication.get("publication_id") or publication.get("publicationId")
        self.assertEqual("uncertain", self.router.port.publication(pid)["state"])
        self.assertEqual([], [r for r in self.ledger.next(limit=50)
                              if (r.get("publicationId") or r.get("publication_id")) == pid])
        self.holder.recover(pid)
        self.assertEqual("confirmed", self.router.port.publication(pid)["state"])
        self.holder.run()
        self.assertEqual(1, len(self.linear.issues))

    def test_a_legitimate_done_is_kept_and_writes_nothing(self):
        before = self.store.one("SELECT COUNT(*) AS n FROM fault_ledger")["n"]
        answer = self.router.check_completion(reading("ALN-9"))
        self.assertEqual("consistent", answer["verdict"])
        self.assertEqual([], answer["recorded"])
        self.assertEqual(before, self.store.one("SELECT COUNT(*) AS n FROM fault_ledger")["n"])
        self.holder.run()
        self.assertEqual({}, self.linear.issues)

    def test_a_false_completion_stays_flagged_for_reverification_on_its_owner(self):
        answer = self.router.check_completion(reading("ALN-9", observed={"acceptance": "absent"}))
        self.assertEqual("mismatch", answer["verdict"])
        (entry,) = answer["recorded"]
        self.holder.run()
        self.assertEqual({}, self.linear.issues)  # no new issue, and no state change
        self.assertEqual(["ALN-9"], [c["issue"] for c in self.linear.comments])
        attention = self.router.show(attention=True)["routes"]
        self.assertIn(entry["faultId"], [r["faultId"] for r in attention])
        closed = self.router.check_completion(reading("ALN-9", observedAt="later", evidence={
            "acceptance": {"fix": {"ref": "PR#44", "source": "github"},
                           "verification": {"ref": "suite#9", "source": "ci"}}}))
        self.assertIn("closed", [e["recorded"] for e in closed["recorded"]])
        self.assertEqual("resolved", self.router.port.get(entry["faultId"])["state"])
        again = self.router.check_completion(reading("ALN-9", observedAt="again",
                                                     observed={"acceptance": "failed"}))
        self.assertNotEqual(entry["faultId"], again["recorded"][0]["faultId"])
        self.holder.run()
        self.assertNotIn("open", [i["state"] for i in self.linear.issues.values()])

    def test_a_closure_records_its_own_fix_after_an_earlier_one(self):
        first = self.router.check_completion(reading("ALN-9", observed={"acceptance": "absent"}))
        (entry,) = first["recorded"]
        self.router.port.record_fix(entry["faultId"], ref="PR#10", detail="an earlier attempt")
        closed = self.router.check_completion(reading("ALN-9", observedAt="later", evidence={
            "acceptance": {"fix": {"ref": "PR#11", "source": "github"},
                           "verification": {"ref": "suite#12", "source": "ci"}}}))
        self.assertIn("closed", [e["recorded"] for e in closed["recorded"]])
        timeline = [(r["kind"], r["ref"]) for r in
                    self.router.port.remediations(entry["faultId"], limit=20)]
        self.assertEqual(["PR#10", "PR#11", "suite#12"], [ref for _kind, ref in timeline])
        self.assertEqual("resolved", self.router.port.get(entry["faultId"])["state"])

    def test_a_withdrawn_claim_leaves_an_open_mismatch_standing_without_a_new_occurrence(self):
        first = self.router.check_completion(reading("ALN-9", observed={"acceptance": "absent"}))
        (entry,) = first["recorded"]
        self.holder.run()
        count = self.router.port.get(entry["faultId"])["occurrence_count"]
        # The subject was taken back out of Done: that answers nothing about the evidence.
        withdrawn = self.router.check_completion(reading(
            "ALN-9", observedAt="reopened", claims={"linearDone": False, "prMerged": True}))
        self.assertEqual("mismatch", withdrawn["verdict"])
        acceptance = {c["check"]: c for c in withdrawn["checks"]}["acceptance"]
        self.assertEqual("claim_withdrawn_without_closure", acceptance["verdict"])
        self.assertEqual([("acceptance", "standing")],
                         [(e["check"], e["recorded"]) for e in withdrawn["recorded"]])
        self.assertEqual(count, self.router.port.get(entry["faultId"])["occurrence_count"])
        self.assertEqual("open", self.router.port.get(entry["faultId"])["state"])
        attention = self.router.show(attention=True)["routes"]
        self.assertIn(entry["faultId"], [r["faultId"] for r in attention])
        # An exception nobody approved changes nothing: still standing, still not counted.
        excused = self.router.check_completion(reading(
            "ALN-9", observedAt="excused", claims={"linearDone": False, "prMerged": True},
            exceptions=[{"check": "acceptance", "kind": "scope_reduction", "ref": "doc#1"}]))
        acceptance = {c["check"]: c for c in excused["checks"]}["acceptance"]
        self.assertEqual("claim_withdrawn_without_closure", acceptance["verdict"])
        self.assertIn("exception is unverified", acceptance["reason"])
        self.assertEqual(count, self.router.port.get(entry["faultId"])["occurrence_count"])
        # Nothing open, nothing claimed: not applicable, as before.
        clean = self.router.check_completion(reading(
            "ALN-3", claims={"linearDone": False, "prMerged": False}))
        self.assertEqual(("consistent", []), (clean["verdict"], clean["recorded"]))


def reading(subject, **fields):
    record = {"schema": "completion-reading/1", "product": "alpha-notes", "subject": subject,
              "claims": {"linearDone": True, "prMerged": True},
              "requires": {"acceptance": True, "install": False, "realUse": False},
              "observed": {"acceptance": "passed"}}
    record.update(fields)
    return record


class Projects(ProductRoutingCase):
    def gamma(self, component, symptom, key, goal=True):
        fields = dict(product="gamma-kit", repository="example-org/gamma-kit",
                      surface="real_use", phase="in_use", component=component,
                      symptom=symptom, occurrenceKey=key)
        if goal:
            fields["goal"] = {"key": "offline_sync", "criteria": "edits survive reconnect"}
        return self.route(**fields)

    def test_a_project_is_created_once_for_independent_fixes_sharing_a_goal(self):
        self.router.set_policy(POLICY)
        first = self.gamma("cache", "stale", "g1")
        self.assertEqual("no_project", first["hold"])
        self.gamma("queue", "lost", "g2")
        self.router.evaluate_projects("gamma-kit")  # a second evaluation converges
        self.holder.run()
        self.assertEqual(1, len(self.linear.projects))
        (created,) = self.linear.projects
        self.router.digest()
        gamma = self.router.show("gamma-kit")["routes"]
        self.assertEqual({created}, {r["project"] for r in gamma})
        self.holder.run()
        self.assertEqual({created}, {i["project"] for i in self.issues_of("GMK").values()})

    def test_members_declaring_different_criteria_for_one_goal_make_no_project(self):
        self.router.set_policy(POLICY)
        self.gamma("cache", "stale", "g1")
        self.route(product="gamma-kit", repository="example-org/gamma-kit", surface="real_use",
                   phase="in_use", component="queue", symptom="lost", occurrenceKey="g2",
                   goal={"key": "offline_sync", "criteria": "conflicts are surfaced"})
        answer = self.router.evaluate_projects("gamma-kit")
        self.assertEqual([], answer["queued"])
        self.assertIn("different completion criteria", answer["skipped"][0]["reasons"][0])
        self.holder.run()
        self.assertEqual({}, self.linear.projects)

    def test_a_queued_create_is_cancelled_when_its_goal_gains_other_criteria(self):
        self.router.set_policy(POLICY)
        self.gamma("cache", "stale", "g1")
        self.gamma("queue", "lost", "g2")
        self.route(product="gamma-kit", repository="example-org/gamma-kit", surface="real_use",
                   phase="in_use", component="sync", symptom="dropped", occurrenceKey="g3",
                   goal={"key": "offline_sync", "criteria": "conflicts are surfaced"})
        (row,) = [r for r in self.holder.pending() if r.get("kind") == projects.KIND]
        pid = row.get("publicationId") or row.get("publication_id")
        token = self.ledger.claim(pid, owner="holder")["claimToken"]
        with self.assertRaises(faults.FaultRefused) as caught:
            self.ledger.operation(pid, claim_token=token)
        self.assertIn("a project needs one completion contract", str(caught.exception))
        self.assertEqual("cancelled", self.ledger.publication(pid)["state"])
        self.holder.run()
        self.assertEqual({}, self.linear.projects)

    def test_settled_proposals_are_history_not_attention(self):
        self.router.set_policy(POLICY)
        self.gamma("cache", "stale", "g1")
        self.gamma("queue", "lost", "g2")
        self.holder.run()
        self.router.digest()
        self.assertEqual(1, len(self.router.show("gamma-kit")["projects"]))
        self.assertEqual([], self.router.show("gamma-kit", attention=True)["projects"])

    def test_an_existing_suitable_project_is_reused(self):
        self.router.set_policy(POLICY)
        self.router.bind(binding("gamma-kit", "project", "proj-gmk-cache",
                                 components=["cache"]))
        answer = self.gamma("cache", "stale", "g1")
        self.assertEqual("proj-gmk-cache", answer["project"])
        self.holder.run()
        self.assertEqual({}, self.linear.projects)

    def test_counts_alone_or_no_policy_create_nothing(self):
        for n in range(4):
            self.gamma("cache", f"stale_{n}", f"g{n}", goal=False)
        self.router.set_policy(POLICY)
        self.router.evaluate_projects("gamma-kit")
        self.holder.run()
        self.assertEqual({}, self.linear.projects)
        self.assertEqual({}, self.issues_of("GMK"))

    def test_a_project_bound_after_the_claim_cancels_the_create(self):
        self.router.set_policy(POLICY)
        self.gamma("cache", "stale", "g1")
        self.gamma("queue", "lost", "g2")
        (row,) = [r for r in self.ledger.next(limit=50) if r["kind"] == projects.KIND]
        pid = row.get("publicationId") or row.get("publication_id")
        claim = self.ledger.claim(pid, owner="holder")
        self.router.bind(binding("gamma-kit", "project", "proj-gmk-cache",
                                 components=["cache"]))
        with self.assertRaises(faults.FaultRefused) as caught:
            self.ledger.operation(pid, claim_token=claim["claimToken"])
        self.assertIn("cancelled before issue", str(caught.exception))
        self.assertEqual("cancelled", self.router.port.publication(pid)["state"])
        self.assertEqual({}, self.linear.projects)


class ProjectLinkage(ProductRoutingCase):
    def test_a_create_read_back_in_the_wrong_project_is_repaired_on_the_same_issue(self):
        self.linear.wrong_project = "proj-elsewhere"
        answer = self.route()
        self.holder.run(rounds=1)
        self.linear.wrong_project = None
        self.assertEqual("unlinked", self.router.port.get(answer["faultId"])["linkState"])
        self.assertIn(answer["faultId"], [r["faultId"] for r in
                                          self.router.show(attention=True)["routes"]])
        self.holder.run()
        (ref, issue), = self.issues_of("ALN").items()
        self.assertEqual("proj-aln-editor", issue["project"])
        self.assertEqual("linked", self.router.port.get(answer["faultId"])["linkState"])

    def test_a_product_with_no_project_files_nothing_until_one_is_bound(self):
        answer = self.route(product="gamma-kit", repository="example-org/gamma-kit",
                             surface="real_use", phase="in_use", component="cache",
                             symptom="stale")
        self.assertEqual("no_project", answer["hold"])
        self.holder.run()
        self.assertEqual({}, self.linear.issues)
        self.router.bind(binding("gamma-kit", "project", "proj-gmk-cache",
                                 components=["cache"]))
        self.holder.run()
        (ref, issue), = self.issues_of("GMK").items()
        self.assertEqual("proj-gmk-cache", issue["project"])


class SimulatedAndObserved(ProductRoutingCase):
    def test_a_simulated_event_goes_only_to_the_test_target_and_never_meets_a_real_one(self):
        real = self.route(occurrenceKey="real-1")
        fake = self.route(occurrenceKey="sim-1", origin="simulated")
        self.assertNotEqual(real["faultId"], fake["faultId"])
        self.assertEqual("proj-test", fake["project"])
        self.holder.run()
        self.assertEqual({"ALN", "TST"}, {i["team"] for i in self.linear.issues.values()})

    def crw_fault(self, *, simulated):
        self.ledger.set_target(product="crw", team="CRW", project="relay",
                               project_ref="proj-crw-relay")
        signature = {"recipient": "rel-1", **({"simulated": True} if simulated else {})}
        recorded = self.ledger.record(faults.observation(
            product="crw", fault_class="delivery_stalled", severity="degraded",
            signature=signature, occurrence_key="crw-1", scope={"projectKey": "relay"}))
        return recorded["faultId"], signature

    def test_a_cause_counts_only_incidents_of_its_own_origin(self):
        real, real_signature = self.crw_fault(simulated=False)
        fake, fake_signature = self.crw_fault(simulated=True)
        before = {f: self.router.port.get(f)["occurrence_count"] for f in (real, fake)}
        crossed = [
            self.route(origin="simulated", occurrenceKey="sim-1",
                       cause={"product": "crw", "faultId": real, "signature": real_signature}),
            self.route(occurrenceKey="real-1",
                       cause={"product": "crw", "faultId": fake, "signature": fake_signature})]
        self.assertEqual([real, fake], [a["unverifiedCause"]["faultId"] for a in crossed])
        self.assertEqual(before, {f: self.router.port.get(f)["occurrence_count"]
                                  for f in (real, fake)})
        for answer in crossed:
            target = routes.get(self.store, answer["faultId"])["target"]
            self.assertIsNone(target["cause"])
            self.assertEqual([], [o for o in target["obligations"] if o["toFault"]])
        # The same origin on both sides is a verified cause, test target included.
        matched = self.route(origin="simulated", occurrenceKey="sim-2",
                             cause={"product": "crw", "faultId": fake,
                                    "signature": fake_signature})
        self.assertEqual((None, None), (matched["hold"], matched["unverifiedCause"]))
        self.assertEqual(before[fake] + 1, self.router.port.get(fake)["occurrence_count"])
        self.assertEqual(before[real], self.router.port.get(real)["occurrence_count"])



class FoldedReviewScenarios(ProductRoutingCase):
    """The review findings that only the real ledger can show (review of 59a2ddef..c7583a00)."""

    def test_an_old_reading_handed_in_again_after_many_others_opens_no_round(self):
        first = reading("ALN-9", observedAt="t0", observed={"acceptance": "absent"})
        self.router.check_completion(first)
        for n in range(1, 18):
            self.router.check_completion(reading("ALN-9", observedAt=f"t{n}",
                                                 observed={"acceptance": "failed"}))
        self.router.check_completion(reading("ALN-9", observedAt="fixed", evidence={
            "acceptance": {"fix": {"ref": "PR#50", "source": "github"},
                           "verification": {"ref": "suite#10", "source": "ci"}}}))
        again = self.router.check_completion(first)
        self.assertEqual(["replayed"], [e["recorded"] for e in again["recorded"]])

    def test_a_digest_that_binds_a_created_project_reports_no_hold_for_its_members(self):
        self.router.set_policy(POLICY)
        held = [Projects.gamma(self, "cache", "stale", "g1"),
                Projects.gamma(self, "queue", "lost", "g2")]
        self.holder.run()
        answer = self.router.digest()
        self.assertTrue(answer["projectsBound"])
        self.assertNotIn("held_no_project", [d["decision"] for d in answer["decisions"]])
        stages = {r["faultId"]: r["stage"] for r in self.router.show("gamma-kit")["routes"]}
        self.assertEqual({"filed"}, {stages[h["faultId"]] for h in held})




class ExistingItemsAndObligations(ProductRoutingCase):
    def test_a_completed_defect_that_comes_back_reopens_its_own_issue_and_comments_there(self):
        answer = self.route(symptom="save_mismatch", occurrenceKey="again-1")
        self.assertEqual(("reopen", "ALN-9"), (answer["disposition"], answer["owner"]))
        self.holder.run()
        self.router.reconcile()
        self.holder.run()
        self.assertEqual({}, {r: i for r, i in self.linear.issues.items() if r != "ALN-9"})
        self.assertEqual("open", self.linear.issues["ALN-9"]["state"])
        self.assertIn("ALN-9", [c["issue"] for c in self.linear.comments])

    def test_a_regression_gets_a_follow_up_linked_to_the_fixed_issue_and_its_label(self):
        answer = self.route(symptom="save_dropped", context={"regressionOf": "PR#12"})
        self.assertEqual("follow_up", answer["disposition"])
        self.holder.run()
        self.router.reconcile()
        self.holder.run()
        ref = self.router.port.get(answer["faultId"])["external_ref"]
        issue = self.linear.issues[ref]
        self.assertIn({"type": "related", "issue": "ALN-9"}, issue["relations"])
        self.assertEqual(["example-org/alpha-notes"], issue["labels"])
        self.assertEqual([], self.router.reconcile()["queued"])

    def test_an_owner_bound_after_a_create_was_queued_takes_the_defect_instead(self):
        answer = self.route(symptom="scroll_lock")
        self.router.bind(binding("alpha-notes", "issue", "ALN-11", components=["editor"],
                                 symptoms=["scroll_lock"], project="proj-aln-editor"))
        self.holder.run()
        self.assertEqual({}, {r: i for r, i in self.linear.issues.items() if r != "ALN-11"})
        self.assertEqual("ALN-11", self.router.port.get(answer["faultId"])["external_ref"])
        self.assertIn("ALN-11", [c["issue"] for c in self.linear.comments])


class SharedCause(ProductRoutingCase):
    def crw_fault(self):
        self.ledger.set_target(product="crw", team="CRW", project="relay",
                               project_ref="proj-crw-relay")
        signature = {"recipient": "rel-1"}
        recorded = self.ledger.record(faults.observation(
            product="crw", fault_class="delivery_stalled", severity="degraded",
            signature=signature, occurrence_key="crw-1", scope={"projectKey": "relay"}))
        return recorded["faultId"], signature

    def test_an_unverified_cause_merges_nothing_and_stands_apart_from_the_defect(self):
        answer = self.route(cause={"product": "crw", "faultId": "0" * 32})
        self.assertEqual(("new_issue", None), (answer["disposition"], answer["hold"]))
        self.assertEqual("0" * 32, answer["unverifiedCause"]["faultId"])
        self.holder.run()
        (ref, issue), = self.issues_of("ALN").items()
        self.assertEqual(([], {}), (issue["relations"], self.issues_of("CRW")))
        (shown,) = self.router.show("alpha-notes", attention=True)["routes"]
        self.assertEqual("cause_unverified", shown["attention"])

    def test_an_unverified_cause_on_a_filed_defect_stands_until_a_verified_one(self):
        first = self.route()
        self.holder.run()
        self.router.digest()
        claimed = self.route(occurrenceKey="run-2", cause={"product": "crw", "faultId": "0" * 32})
        self.assertEqual((first["faultId"], "filed"), (claimed["faultId"], claimed["stage"]))
        self.assertEqual("0" * 32, claimed["unverifiedCause"]["faultId"])
        decisions = [d["decision"] for d in self.router.digest()["decisions"]]
        self.assertEqual(["cause_unverified"], decisions)
        # An occurrence naming no cause says nothing about the claim, which stays standing.
        plain = self.route(occurrenceKey="run-3")
        self.assertEqual("0" * 32, plain["unverifiedCause"]["faultId"])
        # A verified cause answers it: the claim is released and the relation is owed.
        cause, signature = self.crw_fault()
        verified = self.route(occurrenceKey="run-4",
                              cause={"product": "crw", "faultId": cause, "signature": signature})
        self.assertIsNone(verified["unverifiedCause"])
        target = routes.get(self.store, first["faultId"])["target"]
        self.assertEqual(cause, target["cause"])
        self.assertIn(cause, [o["toFault"] for o in target["obligations"]])
        self.assertEqual([], self.router.show("alpha-notes", attention=True)["routes"])

    def test_a_linked_cause_never_answers_a_later_unverified_one(self):
        cause, signature = self.crw_fault()
        linked = self.route(cause={"product": "crw", "faultId": cause, "signature": signature})
        claimed = self.route(occurrenceKey="run-2", cause={"product": "crw", "faultId": "0" * 32})
        plain = self.route(occurrenceKey="run-3")
        self.assertEqual({linked["faultId"]}, {claimed["faultId"], plain["faultId"]})
        self.assertEqual("0" * 32, plain["unverifiedCause"]["faultId"])
        target = routes.get(self.store, linked["faultId"])["target"]
        self.assertEqual((cause, "0" * 32), (target["cause"], target["unverifiedCause"]["faultId"]))
        # The cause verified again by a later incident is that incident's answer, and releases it.
        again = self.route(occurrenceKey="run-4",
                           cause={"product": "crw", "faultId": cause, "signature": signature})
        self.assertIsNone(again["unverifiedCause"])

    def test_a_binding_that_places_a_held_defect_keeps_its_unverified_cause(self):
        held = self.route(product="gamma-kit", repository="example-org/gamma-kit",
                          surface="real_use", phase="in_use", component="cache",
                          symptom="stale", cause={"product": "crw", "faultId": "0" * 32})
        self.assertEqual("no_project", held["hold"])
        self.router.bind(binding("gamma-kit", "project", "proj-gmk-cache", components=["cache"]))
        route = routes.get(self.store, held["faultId"])
        self.assertEqual(("filed", "proj-gmk-cache"), (route["stage"],
                                                       route["target"]["project"]))
        self.assertEqual("0" * 32, route["target"]["unverifiedCause"]["faultId"])

    def test_a_verified_cause_links_the_two_records_and_keeps_each_severity(self):
        cause, signature = self.crw_fault()
        answer = self.route(cause={"product": "crw", "faultId": cause, "signature": signature},
                            severity="broken")
        self.assertEqual("ALN", self.router.show("alpha-notes")["routes"][0]["team"])
        self.assertEqual("degraded", self.router.port.get(cause)["severity"])
        self.route(cause={"product": "crw", "faultId": cause, "signature": signature},
                   severity="broken", occurrenceKey="run-2")
        self.route(cause={"product": "crw", "faultId": cause, "signature": signature},
                   severity="broken", occurrenceKey="run-3")
        self.holder.run()
        self.router.reconcile()
        self.holder.run()
        mine = self.router.port.get(answer["faultId"])["external_ref"]
        theirs = self.router.port.get(cause)["external_ref"]
        self.assertTrue(mine and theirs)
        self.assertIn({"type": "related", "issue": theirs}, self.linear.issues[mine]["relations"])
        self.assertEqual([], self.linear.issues[theirs]["labels"])


class UnclearOwnership(ProductRoutingCase):
    def unknown(self, **fields):
        return self.route(product=None, repository="example-org/unknown", **fields)

    def test_one_pending_record_is_classified_and_later_incidents_are_forwarded(self):
        first = self.unknown(occurrenceKey="u1")
        again = self.unknown(occurrenceKey="u2")
        self.assertEqual(first["faultId"], again["faultId"])
        self.holder.run()
        self.assertEqual({}, self.linear.issues)
        moved = self.router.classify(first["faultId"], {"product": "alpha-notes",
                                                        "by": "operator"})
        later = self.unknown(occurrenceKey="u3")
        self.assertEqual(moved["successor"], later["faultId"])
        self.holder.run()
        self.assertEqual(1, len(self.issues_of("ALN")))

    def test_a_classified_incident_keeps_the_cause_it_named(self):
        cause, signature = SharedCause.crw_fault(self)
        before = self.router.port.get(cause)["occurrence_count"]
        first = self.unknown(cause={"product": "crw", "faultId": cause, "signature": signature})
        self.assertEqual(before, self.router.port.get(cause)["occurrence_count"])
        moved = self.router.classify(first["faultId"], {"product": "alpha-notes",
                                                        "by": "operator"})
        self.assertEqual(before + 1, self.router.port.get(cause)["occurrence_count"])
        self.assertEqual(cause, routes.get(self.store, moved["successor"])["target"]["cause"])

    def test_two_workspaces_never_share_a_pending_record(self):
        a = self.unknown(workspace="ws-a")
        b = self.unknown(workspace="ws-b")
        self.assertNotEqual(a["faultId"], b["faultId"])


class Reporting(ProductRoutingCase):
    def test_a_nonsense_page_bound_is_refused_before_anything_is_done(self):
        self.route()
        for call in (lambda: self.router.digest(limit=0), lambda: self.router.digest(limit=True),
                     lambda: self.router.reconcile(limit=-1),
                     lambda: self.router.show(limit=0), lambda: self.router.show(after=-1),
                     lambda: self.router.show(after=2 ** 63),
                     lambda: self.router.reconcile(after="x")):
            with self.subTest(call=call), self.assertRaises(products.RouteRefused) as caught:
                call()
            self.assertEqual("route_input_malformed", caught.exception.reason.value)
        self.assertIsNone(routes.listing(self.store)["routes"][0]["reported"])

    def test_a_second_digest_with_nothing_changed_is_quiet(self):
        self.route()
        self.route(product="beta-meter", repository="example-org/beta-meter",
                   surface="real_use", phase="in_use", component="billing",
                   symptom="charge_twice")
        self.holder.run()
        first = self.router.digest()
        self.assertFalse(first["quiet"])
        self.assertEqual(2, len(first["severe"]))
        self.assertTrue(self.router.digest()["quiet"])

    def test_a_capped_product_does_not_hide_another_products_work(self):
        self.ledger.set_limit("alpha-notes", "open_record", max_count=1, window=3600)
        for n in range(3):
            self.route(symptom=f"crash_{n}", occurrenceKey=f"c{n}")
        beta = self.route(product="beta-meter", repository="example-org/beta-meter",
                          surface="real_use", phase="in_use", component="billing",
                          symptom="charge_twice")
        self.holder.run()
        self.assertEqual(1, len(self.issues_of("ALN")))
        self.assertTrue(self.router.port.get(beta["faultId"])["external_ref"])




class BoundedProposals(ProductRoutingCase):
    def propose(self, product_record, goal):
        self.router.register_product(product_record)
        product = product_record["product"]
        for n, component in enumerate(("cache", "queue")):
            self.route(product=product, repository=f"example-org/{product}",
                       surface="real_use", phase="in_use", component=component,
                       symptom=f"s{n}", occurrenceKey=f"{product}-{n}",
                       goal={"key": goal, "criteria": "works"})

    def outstanding(self):
        return routes.listing(self.store, stages=("filed",),
                              dispositions=("project_proposal",), limit=100)["routes"]

    def test_a_settled_proposal_is_not_read_again(self):
        self.router.set_policy(POLICY)
        Projects.gamma(self, "cache", "stale", "g1")
        Projects.gamma(self, "queue", "lost", "g2")
        self.assertEqual(1, len(self.outstanding()))
        self.holder.run()
        self.router.digest()
        self.assertEqual([], self.outstanding())
        self.assertEqual([], [r for r in self.router.show(attention=True)["routes"]])

    def test_a_proposal_waiting_on_its_create_does_not_starve_a_confirmed_one(self):
        self.router.set_policy(POLICY)
        self.propose(registry("delta-app", "DLT", surfaces={"real_use": {"method": "events",
                                                                          "active": True}}),
                     "sync_a")
        self.propose(registry("omega-app", "OMG", surfaces={"real_use": {"method": "events",
                                                                          "active": True}}),
                     "sync_b")
        self.assertEqual(2, len(self.outstanding()))
        # Only omega's create is carried; delta's stays pending and is checked first.
        (omega,) = [r for r in self.ledger.next(limit=50) if r["kind"] == projects.KIND
                    and r["payload"]["product"] == "omega-app"]
        self.holder.carry(omega)
        first = self.router.digest(limit=1)
        self.assertEqual([], first["projectsBound"])
        self.assertEqual(1, first["proposalsUnreached"])
        self.assertNotIn("omega-app", [d["product"] for d in first["decisions"]
                                       if d["decision"] == "held_no_project"])
        second = self.router.digest(limit=1)
        self.assertEqual(1, len(second["projectsBound"]))
        stages = {r["stage"] for r in self.router.show("omega-app")["routes"]}
        self.assertEqual({"filed"}, stages)
        self.assertEqual(1, len(self.outstanding()))


class ManyRounds(ProductRoutingCase):
    def test_a_replay_is_recognised_even_after_the_last_round_closed(self):
        from codex_session_relay import completion as check

        first = reading("ALN-9", observedAt="r0-fail", observed={"acceptance": "absent"})
        for n in range(check.MAX_ROUNDS):
            failing = first if n == 0 else reading(
                "ALN-9", observedAt=f"r{n}-fail", observed={"acceptance": "absent"})
            self.router.check_completion(failing)
            self.router.check_completion(reading("ALN-9", observedAt=f"r{n}-fixed", evidence={
                "acceptance": {"fix": {"ref": f"PR#{100 + n}", "source": "github"},
                               "verification": {"ref": f"suite#{n}", "source": "ci"}}}))
        again = self.router.check_completion(first)
        self.assertEqual(["replayed"], [e["recorded"] for e in again["recorded"]])
        with self.assertRaises(products.RouteRefused):
            self.router.check_completion(reading("ALN-9", observedAt="new",
                                                 observed={"acceptance": "failed"}))




class KindModuleInAnotherProcess(ProductRoutingCase):
    """A holder is another process. Its project_create writes are offered only when it imports
    the module that registers the kind, so no holder can skip the kind's pre-issue check."""

    def holder_cli(self, *argv):
        import os
        import subprocess
        import sys

        source = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state",
             str(self.store.path.parent), *argv],
            capture_output=True, text=True, timeout=60,
            env=dict(os.environ, PYTHONPATH=source))
        return completed.returncode, json.loads(completed.stdout)

    def test_a_holder_without_the_kind_module_is_never_offered_the_project_create(self):
        self.router.set_policy(POLICY)
        Projects.gamma(self, "cache", "stale", "g1")
        Projects.gamma(self, "queue", "lost", "g2")
        (row,) = [r for r in self.ledger.next(limit=50) if r["kind"] == projects.KIND]
        pid = row.get("publicationId") or row.get("publication_id")
        _, bare = self.holder_cli("fault-next", "--limit", "50")
        offered = [p.get("publication_id") or p.get("publicationId") for p in bare["publications"]]
        self.assertNotIn(pid, offered)
        code, refused = self.holder_cli("fault-claim", "--publication", pid, "--owner", "holder")
        self.assertNotEqual(0, code)
        self.assertEqual("fault_kind_unregistered", refused["reason"])
        module = ("--kind-module", "codex_session_relay.projects")
        _, registered = self.holder_cli(*module, "fault-next", "--limit", "50")
        self.assertIn(pid, [p.get("publication_id") or p.get("publicationId")
                            for p in registered["publications"]])
        code, claim = self.holder_cli(*module, "fault-claim", "--publication", pid,
                                      "--owner", "holder")
        self.assertEqual(0, code, claim)
        code, operation = self.holder_cli(*module, "fault-operation", "--publication", pid,
                                          "--claim-token", claim["claimToken"])
        self.assertEqual(0, code, operation)
        self.assertEqual("GMK", operation["payload"]["team"])




class PlanAuditScenarios(ProductRoutingCase):
    """wp3m plan audit: what routing must keep true against the real ledger's own policy."""

    def test_an_owner_found_after_the_create_was_issued_is_held_without_moving_the_fault(self):
        answer = self.route(symptom="scroll_lock")
        (row,) = [r for r in self.ledger.next(limit=50) if r["kind"] == "open_record"]
        pid = row.get("publicationId") or row.get("publication_id")
        claim = self.ledger.claim(pid, owner="holder")
        self.ledger.operation(pid, claim_token=claim["claimToken"])  # issued, response pending
        before = json.loads(self.router.port.get(answer["faultId"])["scope"])
        self.router.bind(binding("alpha-notes", "issue", "ALN-11", components=["editor"],
                                 symptoms=["scroll_lock"], project="proj-aln-editor"))
        route = routes.get(self.store, answer["faultId"])
        self.assertEqual((products.STAGE_HELD, products.OWNER_FOUND_AFTER_CREATE),
                         (route["stage"], route["target"]["hold"]))
        self.assertEqual(before, json.loads(self.router.port.get(answer["faultId"])["scope"]))
        waiting = {r["faultId"]: r["attention"] for r in self.router.show(attention=True)["routes"]}
        self.assertEqual("held_owner_found_after_create", waiting[answer["faultId"]])

    def test_a_product_policy_that_raises_the_mismatch_threshold_is_honoured_and_visible(self):
        self.ledger.set_policy("alpha-notes", "completion_mismatch", "degraded", threshold=3,
                               reason="alpha-notes wants three readings before it is told")
        states = []
        for n in range(4):
            answer = self.router.check_completion(reading(
                "ALN-9", observedAt=f"t{n}", observed={"acceptance": "absent"}))
            self.assertEqual("mismatch", answer["verdict"])
            (entry,) = answer["recorded"]
            states.append(entry["ledgerState"])
            if n < 2:
                self.holder.run()
                self.assertEqual([], self.linear.comments)
                waiting = {r["faultId"]: r["attention"]
                           for r in self.router.show(attention=True)["routes"]}
                self.assertEqual("completion_mismatch_open", waiting[entry["faultId"]])
        self.assertEqual(["observed", "observed", "open", "open"], states)
        self.holder.run()
        self.assertEqual(["ALN-9"], sorted({c["issue"] for c in self.linear.comments}))
        self.assertNotIn("ALN-9", self.linear.issues)  # no update ever reached the subject

    def test_a_goal_that_qualifies_again_revives_its_cancelled_create_with_the_new_members(self):
        self.router.set_policy(POLICY)
        Projects.gamma(self, "cache", "stale", "g1")
        Projects.gamma(self, "queue", "lost", "g2")
        (row,) = [r for r in self.ledger.next(limit=50) if r["kind"] == projects.KIND]
        pid = row.get("publicationId") or row.get("publication_id")
        claim = self.ledger.claim(pid, owner="holder")
        self.router.bind(binding("gamma-kit", "project", "proj-gmk-cache",
                                 components=["cache"]))
        with self.assertRaises(faults.FaultRefused):
            self.ledger.operation(pid, claim_token=claim["claimToken"])
        self.assertEqual("cancelled", self.router.port.publication(pid)["state"])
        third = Projects.gamma(self, "sync", "drift", "g3")
        revived = self.router.port.publication(pid)
        self.assertEqual("pending", revived["state"])
        members = set(revived["payload"]["members"])
        self.assertEqual(2, len(members))
        self.assertIn(third["faultId"], members)
        self.holder.run()
        (created,) = self.linear.projects
        self.router.digest()
        stages = {r["faultId"]: (r["stage"], r["project"])
                  for r in self.router.show("gamma-kit")["routes"]}
        self.assertEqual(("filed", created), stages[third["faultId"]])




class RegistryChanges(ProductRoutingCase):
    """A registry replaced whole: identity stays, placement follows the new record."""

    def test_a_routed_product_keeps_its_workspace_and_an_unrouted_one_may_move(self):
        first = self.route()
        with self.assertRaises(products.RouteRefused) as caught:
            self.router.register_product(dict(ALPHA, workspace="other-ws"))
        self.assertEqual("route_state_conflict", caught.exception.reason.value)
        self.assertEqual(WORKSPACE, self.router.registry("alpha-notes")["workspace"])
        again = self.route(occurrenceKey="run-2")
        self.assertEqual(first["faultId"], again["faultId"])
        moved = self.router.register_product(dict(GAMMA, workspace="other-ws"))
        self.assertEqual(("other-ws", []), (moved["workspace"], moved["redecided"]))

    def test_a_new_triage_project_places_a_defect_held_for_want_of_one(self):
        held = Projects.gamma(self, "cache", "stale", "g1", goal=False)
        self.assertEqual("no_project", held["hold"])
        answer = self.router.register_product(dict(GAMMA, triageProject="proj-gmk-triage"))
        self.assertEqual([held["faultId"]], [r["faultId"] for r in answer["redecided"]])
        self.holder.run()
        (ref, issue), = self.issues_of("GMK").items()
        self.assertEqual("proj-gmk-triage", issue["project"])

    def test_a_team_change_reaches_a_create_not_yet_written(self):
        answer = self.route(product="beta-meter", repository="example-org/beta-meter",
                            surface="real_use", phase="in_use", component="billing",
                            symptom="charge_twice")
        self.assertIsNone(self.router.port.get(answer["faultId"])["external_ref"])
        self.router.register_product(dict(BETA, team="BTX"))
        self.assertEqual("BTX", routes.get(self.store, answer["faultId"])["target"]["team"])
        self.holder.run()
        self.assertEqual(({}, 1), (self.issues_of("BTM"), len(self.issues_of("BTX"))))

    def test_a_team_change_cancels_a_project_create_not_yet_issued_and_queues_it_again(self):
        self.router.set_policy(POLICY)
        Projects.gamma(self, "cache", "stale", "g1")
        Projects.gamma(self, "queue", "lost", "g2")
        answer = self.router.register_product(dict(GAMMA, team="GMX"))
        revised = answer["projectsRevised"]
        self.assertEqual((1, 1), (len(revised["cancelled"]), len(revised["queued"])))
        self.assertIn("team is now 'GMX'", revised["cancelled"][0]["reasons"][0])
        self.holder.run()
        self.assertEqual(["GMX"], [p["team"] for p in self.linear.projects.values()])

    def test_a_create_not_yet_issued_waits_while_its_route_has_no_project(self):
        answer = self.route(product="beta-meter", repository="example-org/beta-meter",
                            surface="real_use", phase="in_use", component="billing",
                            symptom="charge_twice")
        untriaged = {key: value for key, value in BETA.items() if key != "triageProject"}
        self.router.register_product(untriaged)
        self.assertEqual("no_project", routes.get(self.store, answer["faultId"])["target"]["hold"])
        self.holder.run()
        self.assertEqual({}, self.linear.issues)
        self.router.register_product(BETA)
        self.holder.run()
        (ref, issue), = self.issues_of("BTM").items()
        self.assertEqual("proj-btm-triage", issue["project"])

    def test_a_project_made_in_a_team_the_product_left_is_never_bound_on_faith(self):
        self.router.set_policy(POLICY)
        members = [Projects.gamma(self, "cache", "stale", "g1"),
                   Projects.gamma(self, "queue", "lost", "g2")]
        (row,) = [r for r in self.holder.pending() if r.get("kind") == projects.KIND]
        pid = row.get("publicationId") or row.get("publication_id")
        token = self.ledger.claim(pid, owner="holder")["claimToken"]
        operation = self.ledger.operation(pid, claim_token=token)  # issued in GMK
        self.router.register_product(dict(GAMMA, team="GMX"))
        team = operation["payload"]["team"]
        ref = self.linear.create_project(team, operation["block"])
        self.ledger.complete(pid, claim_token=token, readback=operation["block"],
                             external_ref=ref, observed={"team": team})
        answer = self.router.digest()
        self.assertEqual(("GMK", []), (team, answer["projectsBound"]))
        self.assertIn("held_project_team_changed", [d["decision"] for d in answer["decisions"]])
        self.assertEqual([], [b for b in self.router.bindings("gamma-kit")
                              if b["kind"] == "project"])
        self.assertEqual({"held"}, {routes.get(self.store, m["faultId"])["stage"]
                                    for m in members})
        self.holder.run()
        self.assertEqual({}, self.linear.issues)
        # Bound by hand, the decision is made: the members move into it.
        self.router.bind(binding("gamma-kit", "project", ref, components=["cache", "queue"],
                                 goal="offline_sync"))
        self.router.digest()
        self.assertEqual({"filed"}, {routes.get(self.store, m["faultId"])["stage"]
                                     for m in members})

    def test_a_product_using_its_test_target_keeps_it(self):
        self.route(origin="simulated", occurrenceKey="sim-1")
        for changed in (None, {"team": "TS2", "project": "proj-test-2"}):
            with self.subTest(testTarget=changed),                     self.assertRaises(products.RouteRefused) as caught:
                self.router.register_product(dict(ALPHA, testTarget=changed))
            self.assertEqual("route_state_conflict", caught.exception.reason.value)
        self.assertEqual({"team": "TST", "project": "proj-test"},
                         self.router.registry("alpha-notes")["testTarget"])
        # Nothing of beta-meter is on a test target, so it may name one.
        moved = self.router.register_product(dict(BETA, testTarget={"team": "TST",
                                                                    "project": "proj-test"}))
        self.assertEqual("TST", moved["testTarget"]["team"])


class TheContractIsBound(unittest.TestCase):
    """The live binding proof: on this checkout the corrected ledger contract is present, every
    function, keyword and refusal value the port relies on included, so nothing refuses by name
    and routing's classes and project_create kind are registered in this process."""

    def test_the_port_binds_and_routing_registered_its_classes_and_kind(self):
        from codex_session_relay import ledger_port

        self.assertEqual([], ledger_port.missing())
        self.assertEqual([], projects.CLASS_GAPS)
        self.assertEqual([], projects.KIND_GAPS)
        self.assertIn(projects.KIND, faults.KINDS)
        self.assertIn(products.MISMATCH, faults.CLASS_POLICY)



class FinalReviewScenarios(ProductRoutingCase):
    """Behaviour the fresh-context review of 8eb2c5b8 required, against the real ledger."""

    def test_a_shared_cause_occurrence_never_outlives_a_filing_that_failed(self):
        from unittest import mock

        from codex_session_relay import intake

        cause, signature = SharedCause.crw_fault(self)
        before = self.router.port.get(cause)["occurrence_count"]
        with mock.patch.object(intake, "file", side_effect=RuntimeError("filing failed")):
            with self.assertRaises(RuntimeError):
                self.route(cause={"product": "crw", "faultId": cause, "signature": signature})
        self.assertEqual(before, self.router.port.get(cause)["occurrence_count"])
        self.assertEqual(0, self.store.one("SELECT COUNT(*) AS n FROM incident_routes")["n"])

    def test_the_current_issue_takes_its_runs_failure_and_links_the_symptoms_owner(self):
        self.register(issue_key="ALN-3")
        run = self.store.one("SELECT relationship_id FROM relationships WHERE issue_key = ?",
                             ("ALN-3",))["relationship_id"]
        self.router.bind(binding("alpha-notes", "issue", "ALN-12", components=["editor"],
                                 symptoms=["cursor_jump"], project="proj-aln-editor"))
        attached = self.route(context={"currentIssue": "ALN-3", "run": run})
        self.assertEqual(("attach_current", "ALN-3"), (attached["disposition"],
                                                       attached["owner"]))
        elsewhere = self.route(surface="user_report", occurrenceKey="report-1")
        self.assertEqual(("accumulate", "ALN-12"), (elsewhere["disposition"],
                                                    elsewhere["owner"]))
        self.assertNotEqual(attached["faultId"], elsewhere["faultId"])
        self.holder.run()
        self.router.reconcile()
        self.holder.run()
        self.assertEqual({}, {r: i for r, i in self.linear.issues.items()
                              if r not in ("ALN-3", "ALN-12")})
        self.assertIn("ALN-3", [c["issue"] for c in self.linear.comments])
        self.assertIn({"type": "related", "issue": "ALN-12"},
                      self.linear.issues["ALN-3"]["relations"])
