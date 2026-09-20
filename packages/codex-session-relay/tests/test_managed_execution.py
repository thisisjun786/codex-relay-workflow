"""The managed start sequence, run as the instructions tell a coordinator to run it.

CRW-125 asks for verification of the causal chain against a real relay store, and warns that a
hand-registered demo proves nothing about adoption. The honest strongest thing a test in this
repository can do is drive the ACTUAL command surface, in the order the skill text instructs,
against a real relay.sqlite3 - so that following the instruction demonstrably produces the chain,
and an instruction that stopped producing it would fail here.

What this does not prove, and what is therefore not claimed: that a live installed Run or Loop
with real Codex tasks ran this sequence. crw-run and crw-loop are markdown read by an agent;
there is no executable to invoke. That proof belongs to CRW-124 by the issue's own dependency
boundary, and source integration, installation and live use stay three separate facts.

Every command here is store-only or marker-only, which is why the sequence runs with no socket
and no App Server. The legs that genuinely need a host - deliver, and the acknowledgement it
carries - are exercised against the real store through the fake host in
test_revision_roundtrip.py, where the correction's own anchoring is asserted.
"""

import json
import os

from .support import CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT
from .test_cli import CliBase

WORKSPACE = "crw-125-workspace"


class TheInstructedStartSequence(CliBase):
    def marker_root(self):
        return os.path.join(self.tmp, "marker")

    def marker(self, *args):
        return self.run_cli(
            *args, "--marker-root", self.marker_root(), "--workspace", WORKSPACE
        )

    def test_the_sequence_produces_one_assignment_and_says_so_consistently(self):
        store_file = os.path.join(self.tmp, "relay.sqlite3")

        # 1. Determine first, before anything exists. This is the step whose absence left the
        #    question unasked. It must answer without creating what it was asked about.
        first = self.run_cli("doctor", "--issue", ISSUE)
        self.assertFalse(first["issue"]["readable"])
        self.assertIsNone(first["issue"]["holds"])
        self.assertFalse(os.path.exists(store_file))

        # 2. Declare the intent BEFORE the child exists. dbPath is recorded from --state; there
        #    is no --db-path on this command, only --no-db-path to suppress it.
        declared = self.marker(
            "intent-declare", "--dispatch-request-id", "dispatch-1", "--issue", ISSUE,
        )
        assignment = declared["assignmentId"]

        # 3. Register, which is the first command that may create the store.
        relationship = self.register()
        rid = relationship["relationshipId"]

        # 4. Bind the marker to the relationship the registration actually produced.
        self.marker(
            "intent-register", "--assignment", assignment, "--relationship", rid,
            "--dispatch-request-id", "dispatch-1", "--db-path", store_file,
        )

        # 5. Canonical criteria, so a later verdict rules on agreed obligations.
        self.run_cli(
            "criteria-register", "--relationship", rid,
            "--criterion", "c1=the deliverable behaves as the issue says",
        )

        # 6. Determine again. Now it holds, and the answer names the same store the probe saw.
        second = self.run_cli("doctor", "--issue", ISSUE)
        self.assertTrue(second["issue"]["holds"])
        self.assertEqual(second["issue"]["responsibleChild"], CHILD)
        self.assertEqual(second["issue"]["responsibleRelationship"], rid)
        self.assertEqual(second["issue"]["storeAgreement"], "same")
        self.assertEqual(second["issue"]["storeId"], second["store"]["storeId"])

        # 7. The pre-create lookup agrees with the determination, from the same store.
        found = self.run_cli("assignment-find", "--issue", ISSUE)
        self.assertEqual(found["responsibleRelationship"], rid)
        self.assertTrue(found["relay"]["holds"])
        self.assertEqual(found["relay"]["store"]["storeId"], second["issue"]["storeId"])

        # 8. Exactly one assignment exists after the whole sequence.
        self.assertEqual(len(found["assignments"]), 1)

    def test_the_determination_survives_a_coordinator_restart(self):
        """A resume must read the decision back rather than take it again.

        Re-deciding after a context loss is how one assignment acquires two routes, so what the
        marker recorded has to still be there for a process that kept nothing in memory.
        """
        store_file = os.path.join(self.tmp, "relay.sqlite3")
        declared = self.marker(
            "intent-declare", "--dispatch-request-id", "dispatch-1", "--issue", ISSUE,
        )
        assignment = declared["assignmentId"]
        relationship = self.register()
        self.marker(
            "intent-register", "--assignment", assignment,
            "--relationship", relationship["relationshipId"],
            "--dispatch-request-id", "dispatch-1", "--db-path", store_file,
        )

        # A separate process, holding nothing from the one above.
        recovered = self.marker("intent-show", "--assignment", assignment)
        body = json.dumps(recovered)
        self.assertIn(relationship["relationshipId"], body)
        self.assertIn(store_file, body)

    def test_a_second_child_for_the_same_issue_is_refused(self):
        """Querying first is only useful if the record also refuses the race it cannot see."""
        self.register()
        refused = self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", "01a-different-child", "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-2", "--dispatch-turn-id", DISPATCH_TURN,
            expect=2,
        )
        self.assertEqual(refused["reason"], "duplicate_assignment")
        self.assertEqual(
            len(self.run_cli("assignment-find", "--issue", ISSUE)["assignments"]), 1
        )

