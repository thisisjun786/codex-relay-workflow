package integrate

import (
	"bytes"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1026 (d2): dag-integrate-push prints the stale nodes of the pushed head next to the outcome. A node is named by plan,
// node and acceptance; the checkout and the integration ref are not part of a node's identity. The list is empty and the
// error null when nothing is stale.
func TestPushDocumentCarriesTheStaleNodes(t *testing.T) {
	emit := func(r dagsched.PushResult) string {
		var out bytes.Buffer
		if err := contract.Emit(&out, pushObject(r)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	base := dagsched.PushResult{Remote: "origin", RemoteRef: "dev", LocalHead: "aaa", RemoteHead: "aaa", Outcome: dagsched.PushUpToDate}
	if got, want := emit(base), `{
  "ok": true,
  "schema": "dag-integrate-push/1",
  "remote": "origin",
  "remote_ref": "dev",
  "local_head": "aaa",
  "remote_head": "aaa",
  "outcome": "up_to_date",
  "detail": "",
  "stale_nodes": [],
  "stale_nodes_error": null
}
`; got != want {
		t.Fatalf("no stale node:\n%s\nwant\n%s", got, want)
	}
	stale := base
	stale.Outcome = dagsched.PushPushed
	stale.StaleNodes = []dagsched.StaleNode{
		{PlanID: "g", NodeID: "a", AcceptanceID: "acc-a", Reason: dagsched.StaleNodeCriteriaChanged, AcceptedCriteria: "sha256:old", CurrentCriteria: "sha256:new"},
		{PlanID: "g", NodeID: "b", AcceptanceID: "acc-b", Reason: dagsched.StaleNodeAcceptanceSuperseded},
	}
	stale.StaleNodesError = ""
	if got, want := emit(stale), `{
  "ok": true,
  "schema": "dag-integrate-push/1",
  "remote": "origin",
  "remote_ref": "dev",
  "local_head": "aaa",
  "remote_head": "aaa",
  "outcome": "pushed",
  "detail": "",
  "stale_nodes": [
    {
      "plan_id": "g",
      "node_id": "a",
      "acceptance_id": "acc-a",
      "reason": "criteria_changed",
      "accepted_criteria_digest": "sha256:old",
      "current_criteria_digest": "sha256:new"
    },
    {
      "plan_id": "g",
      "node_id": "b",
      "acceptance_id": "acc-b",
      "reason": "acceptance_superseded",
      "accepted_criteria_digest": null,
      "current_criteria_digest": null
    }
  ],
  "stale_nodes_error": null
}
`; got != want {
		t.Fatalf("stale nodes:\n%s\nwant\n%s", got, want)
	}
	unread := base
	unread.StaleNodesError = "the store is busy"
	if got := emit(unread); !bytes.Contains([]byte(got), []byte(`"stale_nodes_error": "the store is busy"`)) || !bytes.Contains([]byte(got), []byte(`"stale_nodes": []`)) {
		t.Fatalf("an unreadable list is named and the list is empty:\n%s", got)
	}
}

// CRW-1301: a push that found the remote on a head the relay never recorded for the integration ref names it after the stale
// nodes: the fault class and id, the ref, the remote head and the newest recorded heads. A push on the integration path
// prints neither key (TestPushDocumentCarriesTheStaleNodes holds that document byte for byte).
func TestPushDocumentNamesTheMovedTarget(t *testing.T) {
	r := dagsched.PushResult{Remote: "origin", RemoteRef: "dev", LocalHead: "ccc", RemoteHead: "ccc", Outcome: dagsched.PushPushed,
		TargetMoved: &dagsched.TargetMoved{IntegrationRef: "dev-int", RemoteHead: "xxx", Expected: []string{"bbb", "aaa"}, ExpectedTotal: 2, FaultID: "f1"}}
	var out bytes.Buffer
	if err := contract.Emit(&out, pushObject(r)); err != nil {
		t.Fatal(err)
	}
	if want := `  "stale_nodes_error": null,
  "target_moved": {
    "fault_class": "integration_target_moved",
    "fault_id": "f1",
    "fault_error": null,
    "integration_ref": "dev-int",
    "remote_head": "xxx",
    "expected_heads": [
      "bbb",
      "aaa"
    ],
    "expected_total": 2
  }
}
`; !bytes.HasSuffix(out.Bytes(), []byte(want)) {
		t.Fatalf("moved target:\n%s\nwant the suffix\n%s", out.String(), want)
	}
	r.TargetMoved, r.TargetCheckError = nil, "the store is busy"
	out.Reset()
	if err := contract.Emit(&out, pushObject(r)); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasSuffix(got, "  \"target_check_error\": \"the store is busy\"\n}\n") || strings.Contains(got, "target_moved") {
		t.Fatalf("an unreadable comparison is named and no moved target is:\n%s", got)
	}
}
