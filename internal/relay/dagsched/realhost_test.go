package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// realKit is a release kit over the real Go bridge adapter talking to a fake App Server on a unix socket: thread creation, the standby turn, the business turn and the permission
// settings read-back all cross the real transport, the real ledger and the real managed engine. Only the host's answers are scripted.
type realKit struct {
	*fixture
	host   *fakehost.Server
	root   string
	marker string
	state  string
	tips   *tips
	forge  *prs
	// model is what the fake host reports for the created thread.
	model string
	mu    sync.Mutex
	turns int
}

func newRealKit(t *testing.T) *realKit {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(root, "scopes"))
	workspace := filepath.Join(root, "work")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	policyRaw := []byte(`{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`)
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, policyRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, policyPath)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	policy, err := execution.FromBytes(policyRaw, policyPath)
	if err != nil {
		t.Fatal(err)
	}
	host := fakehost.Start(t)
	state := filepath.Join(root, "state")
	k := &realKit{host: host, root: workspace, marker: filepath.Join(root, "markers"), state: state, tips: &tips{sha: head1}, forge: &prs{by: map[string]PullRequest{}}, model: "gpt-5.4"}
	k.fixture = newFixtureOn(t, filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	k.fixture.projectParent()
	settings := k.settings()
	environments := settings["environments"]
	response := func() map[string]any {
		r := map[string]any{}
		for key, v := range settings {
			if key != "environments" {
				r[key] = v
			}
		}
		k.mu.Lock()
		r["model"] = k.model
		k.mu.Unlock()
		r["thread"] = map[string]any{"id": "real-child", "environments": environments}
		return r
	}
	host.Handle("thread/start", func(json.RawMessage) fakehost.Reply { return fakehost.Reply{Result: response()} })
	host.Respond("thread/name/set", fakehost.Reply{Result: map[string]any{}})
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.turns++
		id := "standby"
		if k.turns > 1 {
			id = "business"
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": id}}}
	})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true, "model": "gpt-5.4", "reasoningEffort": "medium", "cwd": workspace}}})
	host.Handle("thread/resume", func(json.RawMessage) fakehost.Reply { return fakehost.Reply{Result: response()} })
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "standby", "status": "completed"}}, "nextCursor": nil}})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		data := []any{}
		if params["archived"] != true {
			data = append(data, map[string]any{"id": "real-child"})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	a, err := adapter.Open(host.SocketPath, state, adapter.Options{Policy: policy, Clock: delivery.NewFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s := k.sched
	s.Tips, s.PRs = k.tips, k.forge.read
	s.Selectors = Selectors{MarkerRoot: k.marker, Socket: host.SocketPath, StateSelector: state}
	s.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) {
		engine := &managed.Start{Store: s.Store, Adapter: adapter.Managed{Adapter: a}, Now: delivery.NewFakeClock().ISO, Socket: host.SocketPath, MarkerRoot: k.marker, StateSelector: state,
			Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
		answer, err := engine.Run(ctx, raw)
		if err != nil {
			return StartAnswer{}, err
		}
		return answerOf(answer), nil
	}
	return k
}

func (k *realKit) settings() map[string]any {
	return map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": k.root, "runtimeWorkspaceRoots": []any{k.root},
		"model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": k.root, "runtimeWorkspaceRoots": []any{k.root}}}}
}

func (k *realKit) request() ReleaseRequest {
	return ReleaseRequest{Schema: SchemaReleaseRequest, Instructions: "Verify the design and report.", Criteria: releaseCriteria, CriteriaSource: "issue:real", ScopeRef: "issue:real",
		RuleVersion:   RuleVersion{SkillsDigest: dig("skills"), Model: "gpt-5.4", Effort: "medium", PromptTemplate: "template-1", RelayBuild: "test-build"},
		ArtifactRoots: []string{k.root}, Parent: Endpoint{HostID: "host", Settings: k.settings()}, Child: Endpoint{HostID: "host", Title: "Real child", Settings: k.settings()}}
}

// Criterion c3, c11 over the real transport: one release creates one thread and starts the standby and the business turn; five more wakes create nothing and end on the same child.
func TestReleaseRealSocketHappyPathAndDuplicateWakes(t *testing.T) {
	k := newRealKit(t)
	releasePlan(k.fixture, "rp")
	res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
	if err != nil || !res.Bound || res.ChildTaskID != "real-child" || res.Generation != 1 {
		t.Fatalf("release = %v %+v", err, res)
	}
	if k.host.Count("thread/start") != 1 || k.host.Count("turn/start") != 2 {
		t.Fatalf("host saw %d thread/start and %d turn/start, want 1 and 2", k.host.Count("thread/start"), k.host.Count("turn/start"))
	}
	for i := 0; i < 5; i++ {
		again, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
		if err != nil || !again.Bound || !again.Replayed || again.ChildTaskID != "real-child" {
			t.Fatalf("wake %d = %v %+v", i, err, again)
		}
	}
	if k.host.Count("thread/start") != 1 || k.host.Count("turn/start") != 2 {
		t.Fatalf("after the duplicate wakes the host saw %d thread/start and %d turn/start", k.host.Count("thread/start"), k.host.Count("turn/start"))
	}
	if rows := countRows(k.fixture); rows.releases != 1 || rows.executions != 1 || rows.slots != 1 || rows.manifests != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	var issue, parent, child string
	if err := k.s.DB.QueryRow("SELECT issue_key, parent_task_id, child_task_id FROM relationships WHERE relationship_id = ?", res.RelationshipID).Scan(&issue, &parent, &child); err != nil || issue != "CRW-A" || parent != "parent" || child != "real-child" {
		t.Fatalf("relationship = %s %s %s %v", issue, parent, child, err)
	}
	if n := k.read("rp").node("A"); n.Reason != SkipAlreadyOwned || n.State != StateRunning {
		t.Fatalf("A = %+v", n)
	}
}

// A permission mismatch over the real transport: the host reports the thread it created with another model than the request asked for. Nothing is registered and no turn is started, the
// release binds nothing and keeps its intent and slot, and the same call can be repeated.
func TestReleaseRealSocketRefusesPermissionMismatch(t *testing.T) {
	k := newRealKit(t)
	releasePlan(k.fixture, "rp")
	k.mu.Lock()
	k.model = "gpt-5.4-mini"
	k.mu.Unlock()
	for i := 0; i < 2; i++ {
		res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
		if err != nil {
			t.Fatal(err)
		}
		// the bridge compares what the host created with what was asked before the engine does: either way the child is not bound.
		if res.Bound || (res.State != "refused" && res.State != "incomplete") || (res.Reason != "creation_settings_unverified" && res.Reason != "creation_failed") {
			t.Fatalf("call %d = state %s stage %s reason %s bound %v", i, res.State, res.Stage, res.Reason, res.Bound)
		}
	}
	if k.host.Count("turn/start") != 0 {
		t.Fatalf("a turn was started on a thread whose permissions did not match (%d)", k.host.Count("turn/start"))
	}
	if rows := countRows(k.fixture); rows.executions != 0 || rows.releases != 1 || rows.slots != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

func countRows(f *fixture) rowCounts {
	return rowCounts{
		releases:   f.count("SELECT COUNT(*) FROM dag_releases"),
		requests:   f.count("SELECT COUNT(*) FROM dag_release_requests"),
		manifests:  f.count("SELECT COUNT(*) FROM dag_input_manifests"),
		executions: f.count("SELECT COUNT(*) FROM dag_node_executions"),
		slots:      f.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node'"),
		conflicts:  f.count("SELECT COUNT(*) FROM coordination_conflicts"),
		merges:     f.count("SELECT COUNT(*) FROM dag_merge_checks"),
	}
}

// A forged completed: the child's turn is completed on the host, a daemon wrote a failed event, a receipt is only staged. None of these is a verified result, so the node is never accepted and its
// successor is never released: a child's completion, a transport answer and a statement in a report open no edge by themselves (contract 2.0).
func TestForgedCompletedOpensNothing(t *testing.T) {
	k := newRealKit(t)
	releasePlan(k.fixture, "rp")
	res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
	if err != nil || !res.Bound {
		t.Fatalf("release = %v %+v", err, res)
	}
	accept := func() error {
		_, err := k.sched.Accept(context.Background(), "rp", "A", "parent", AcceptInput{RuleVersion: verifier})
		return err
	}
	// the host says the business turn is completed (the scripted turns/list answers completed for every turn): no report exists
	if err := accept(); refusalReason(err) != "not_acknowledged" {
		t.Fatalf("a completed turn with no report = %v", err)
	}
	// a failure the daemon observed is not a report either
	k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES ('evt-failed', ?, 1, ?, 'failed', 'relay', 'real-child', 'business', 'failed', '{}', 'final', 'a', 'a')", res.RelationshipID, dig("failed"))
	if err := accept(); refusalReason(err) != "not_acknowledged" {
		t.Fatalf("a daemon-written failure = %v", err)
	}
	// a child's receipt that is only staged (its turn has not ended normally)
	k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES ('evt-staged', ?, 1, ?, 'ready_for_review', 'child', 'real-child', 'business', 'inProgress', '{}', 'staged', 'b', 'b')", res.RelationshipID, dig("staged"))
	if err := accept(); refusalReason(err) != "not_acknowledged" {
		t.Fatalf("a staged receipt = %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_acceptances") != 0 {
		t.Fatal("a forged completion was accepted")
	}
	reading := k.read("rp")
	if b := reading.node("B"); b.Reason != WaitEdge("ab") {
		t.Fatalf("B = %+v: a forged completion opened the successor's edge", b)
	}
	if _, err := k.sched.Release(context.Background(), "rp", "B", "parent", k.request()); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("release of B = %v", err)
	}
	if k.host.Count("thread/start") != 1 {
		t.Fatalf("the host created %d threads", k.host.Count("thread/start"))
	}
}
