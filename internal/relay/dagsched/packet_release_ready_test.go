package dagsched

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// packetReleaseCriteria are the criteria every packet node in these tests is registered with: the covers a
// packet declares are criteria the release request must actually register (CRW-839), so the request and the
// node's criteria_set_digest carry the feature's whole set.
var packetReleaseCriteria = []Criterion{{ID: "c1", Title: "c1", Required: true}, {ID: "c2", Title: "c2", Required: true}, {ID: "c3", Title: "c3", Required: true}}

func packetCriteriaDigest() string {
	list := make([]delivery.Criterion, len(packetReleaseCriteria))
	for i, c := range packetReleaseCriteria {
		list[i] = delivery.Criterion{ID: c.ID, Title: c.Title, Required: c.Required}
	}
	return delivery.SetDigest(list)
}

// packetRequest is the release request of a packet node: the kit's request with the feature's criteria.
func packetRequest(k *releaseKit) ReleaseRequest {
	req := k.request(true)
	req.Criteria = packetReleaseCriteria
	return req
}

// The release path of a packet node (CRW-839). resourceHold counts an open relationship of the issue as
// ownership, but a second packet of one feature issue is not owned by the first packet's relationship:
// for a node that carries a packet_id, a rival relationship holds it only when that relationship is not
// an execution of another distinct registered packet of the same plan. A node without a packet_id keeps
// the rule it always had.

// putPacketReleasePlan writes a revision that declares a feature's criteria beside its packet nodes. The
// nodes carry the criteria digest every release request in these tests registers, so they can be released.
func putPacketReleasePlan(t *testing.T, f *fixture, plan string, parent int, request string, criteria []doc, changes ...doc) {
	t.Helper()
	cs := make([]any, len(changes))
	for i, c := range changes {
		cs[i] = c
	}
	d := doc{"schema": dag.SchemaRevision, "plan_id": plan, "project_key": "P-TEST", "request_id": request,
		"expected_parent_revision": parent, "author_task_id": "task-test", "changes": cs}
	if len(criteria) > 0 {
		fcs := make([]any, len(criteria))
		for i, c := range criteria {
			fcs[i] = c
		}
		d["feature_criteria"] = fcs
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := f.repo.Put(context.Background(), rev); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// packetRelNode is a releasable implementation node that carries a packet identity.
func packetRelNode(id, issue, packet string, covers, owns []string) doc {
	n := relNode(id, dag.NodeImplementation)
	n["issue_key"] = issue
	n["packet_id"] = packet
	n["criteria_set_digest"] = packetCriteriaDigest()
	cs := make([]any, len(covers))
	for i, s := range covers {
		cs[i] = s
	}
	n["covers"] = cs
	if len(owns) > 0 {
		os := make([]any, len(owns))
		for i, s := range owns {
			os[i] = s
		}
		n["owns"] = os
	}
	return doc{"op": dag.OpAddNode, "node": n}
}

// declaredCriteriaDoc is one issue's feature criteria declaration.
func declaredCriteriaDoc(issue string, ids ...string) doc {
	cs := make([]any, len(ids))
	for i, id := range ids {
		cs[i] = doc{"id": id, "required": true}
	}
	return doc{"issue_key": issue, "criteria": cs}
}

// bindPacketExecution records the live relationship of a packet node and the dag_execution_packets row the
// duplicate guard and resourceHold resolve the packet by, the way dag-release writes it at bind.
func bindPacketExecution(t *testing.T, f *fixture, plan, node, packet string) string {
	t.Helper()
	rid := f.startNode(plan, node)
	if packet != "" {
		f.exec("INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES (?,?,?,?,?,NULL,'t')",
			rid, plan, node, nodeIssue(f, plan, node), packet)
	}
	return rid
}

// twoPacketPlan is one feature issue delivered by two packets: n1 owns c1, n2 takes c2, and both take c3
// with n1 owning it.
func twoPacketPlan(t *testing.T, f *fixture, plan string) {
	t.Helper()
	putPacketReleasePlan(t, f, plan, 0, plan+"-r1", []doc{declaredCriteriaDoc("CRW-F", "c1", "c2", "c3")},
		packetRelNode("n1", "CRW-F", "p1", []string{"c1", "c3"}, []string{"c1", "c3"}),
		packetRelNode("n2", "CRW-F", "p2", []string{"c2", "c3"}, nil))
	// Each packet declares the region it edits, and the two do not overlap: the edit-region rule is what
	// decides whether two live implementation nodes may run beside each other (CRW-409), and it is
	// unchanged by packets. An undeclared node overlaps every other and is deferred as exclusive.
	declarePacketRegion(t, f, plan, "n1", "packet-one.go")
	declarePacketRegion(t, f, plan, "n2", "packet-two.go")
}

// declarePacketRegion records one node's declared edit region.
func declarePacketRegion(t *testing.T, f *fixture, plan, node, path string) {
	t.Helper()
	if _, err := f.sched.DeclareRegions(context.Background(), plan, node, "parent", []Region{{Repository: "owner/repo", Path: path, Kind: "file", Key: "", Change: "edit"}}); err != nil {
		t.Fatalf("declare %s/%s: %v", node, path, err)
	}
}

// A second packet of one feature issue is released once the first packet's relationship is attached: the
// open relationship of the issue does not hold it, because it is the execution of another registered packet.
func TestPacketReleaseReadyBesideAnotherPacket(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	twoPacketPlan(t, k.fixture, "rp")
	bindPacketExecution(t, k.fixture, "rp", "n1", "p1")

	reading := k.read("rp")
	if got := reading.node("n1").Reason; got != SkipAlreadyOwned {
		t.Fatalf("the first packet's own node reads %q, want %q", got, SkipAlreadyOwned)
	}
	if got := reading.node("n2").Disposition; got != DispReady {
		t.Fatalf("the second packet of the issue reads %q (%s), want %q", got, reading.node("n2").Reason, DispReady)
	}

	res, err := k.sched.Release(context.Background(), "rp", "n2", "parent", packetRequest(k))
	if err != nil || !res.Bound {
		t.Fatalf("the second packet was not released: %v %+v", err, res)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("created %d threads", created)
	}
}

// The same packet twice and a node without a packet_id beside an owned issue keep the rule they always had.
func TestPacketReleaseReadyKeepsTheOldRule(t *testing.T) {
	t.Parallel()
	t.Run("the packet's own node is still owned", func(t *testing.T) {
		k := newReleaseKit(t)
		twoPacketPlan(t, k.fixture, "rp")
		bindPacketExecution(t, k.fixture, "rp", "n1", "p1")
		if got := k.read("rp").node("n1").Reason; got != SkipAlreadyOwned {
			t.Fatalf("the packet's own node reads %q, want %q", got, SkipAlreadyOwned)
		}
	})
	t.Run("a node without a packet keeps the issue-level rule", func(t *testing.T) {
		k := newReleaseKit(t)
		putPacketReleasePlan(t, k.fixture, "rp", 0, "rp-r1", nil, addRelNode("solo", dag.NodeImplementation))
		bindPacketExecution(t, k.fixture, "rp", "solo", "")
		if got := k.read("rp").node("solo").Reason; got != SkipAlreadyOwned {
			t.Fatalf("a node with no packet beside an open relationship reads %q, want %q", got, SkipAlreadyOwned)
		}
	})
	t.Run("an unresolvable rival relationship still holds the node", func(t *testing.T) {
		k := newReleaseKit(t)
		twoPacketPlan(t, k.fixture, "rp")
		// The rival relationship carries no dag_execution_packets row: its packet cannot be resolved, so
		// it holds the issue exactly as it did before there were packets.
		k.fixture.startNode("rp", "n1")
		if got := k.read("rp").node("n2").Reason; got != SkipAlreadyOwned {
			t.Fatalf("a node beside an unresolvable relationship reads %q, want %q", got, SkipAlreadyOwned)
		}
	})
	t.Run("a start in flight of the issue defers the other packet", func(t *testing.T) {
		k := newReleaseKit(t)
		twoPacketPlan(t, k.fixture, "rp")
		// One pending managed start per issue is what the shipped unique partial index
		// managed_start_one_pending_issue allows, so the packets of one issue start one after the other:
		// while the first is reserved the second packet waits, and it is released once the first attached.
		k.fixture.exec("INSERT INTO managed_start_requests (request_id, issue_key, request_fingerprint, fingerprint_version, workspace, marker_root, socket_identity, create_request_id, dispatch_request_id, state, revision, created_at, updated_at) VALUES ('req-n1','CRW-F','fp','1','/w','/m','/s','create-n1','business-n1','reserved',0,'t','t')")
		if got := k.read("rp").node("n2").Reason; got != SkipAlreadyOwned {
			t.Fatalf("a packet beside a start in flight reads %q, want %q", got, SkipAlreadyOwned)
		}
		k.fixture.exec("UPDATE managed_start_requests SET state = 'attached' WHERE request_id = 'req-n1'")
		if got := k.read("rp").node("n2").Disposition; got != DispReady {
			t.Fatalf("the packet reads %q after the start attached, want %q", got, DispReady)
		}
	})
}

// A packet release whose managed start completes on the replay path records the work branch the release
// itself named, not one read from the frozen managed request (which never held it).
func TestPacketReleaseReplayRecordsTheWorkBranch(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	putPacketReleasePlan(t, k.fixture, "rp", 0, "rp-r1", []doc{declaredCriteriaDoc("CRW-F", "c1")},
		packetRelNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	k.host.loseFirstCreation = true
	req := packetRequest(k)
	req.WorkBranch = "codex/crw-839-feature-packet-identity"
	first, err := k.sched.Release(context.Background(), "rp", "n1", "parent", req)
	if err != nil || first.Bound {
		t.Fatalf("first call = %v %+v", err, first)
	}
	// The frozen managed request is what the replay sends; it carries no work_branch key at all.
	var frozen string
	if err := k.s.DB.QueryRow("SELECT request_json FROM dag_release_requests").Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	var managedDoc map[string]any
	if err := json.Unmarshal([]byte(frozen), &managedDoc); err != nil {
		t.Fatalf("the frozen request does not read: %v", err)
	}
	if _, ok := managedDoc["work_branch"]; ok {
		t.Fatalf("the frozen managed request unexpectedly names a work branch")
	}

	again, err := k.sched.Release(context.Background(), "rp", "n1", "parent", packetRequest(k))
	if err != nil || !again.Bound || !again.Replayed {
		t.Fatalf("replay = %v %+v", err, again)
	}
	var branch *string
	if err := k.s.DB.QueryRow("SELECT branch FROM dag_execution_packets").Scan(&branch); err != nil {
		t.Fatal(err)
	}
	if branch == nil || *branch != req.WorkBranch {
		t.Fatalf("the replay recorded branch %v, want %q", branch, req.WorkBranch)
	}
}
