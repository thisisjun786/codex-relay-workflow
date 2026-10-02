package dagsched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// criteriaDigest is the digest of the criteria every released node is registered with, so the request's criteria match the plan's.
var releaseCriteria = []Criterion{{ID: "c1", Title: "preserve the contract", Required: true}}

func releaseCriteriaDigest() string {
	return delivery.SetDigest([]delivery.Criterion{{ID: "c1", Title: "preserve the contract", Required: true}})
}

// relNode is a node of a release plan: its criteria digest is the one the request's criteria produce.
func relNode(id, kind string) doc {
	n := nodeDoc(id, kind)
	n["criteria_set_digest"] = releaseCriteriaDigest()
	return n
}

func addRelNode(id, kind string) doc { return doc{"op": dag.OpAddNode, "node": relNode(id, kind)} }

// releasePlan is the plan the release tests share:
//
//	A (non_pr) -(ab artifact_verified)-> B (non_pr)
//	I (implementation) -(ij artifact_verified, code pin)-> J (implementation)
func releasePlan(f *fixture, plan string) {
	f.t.Helper()
	pin := doc{"pins_code_head": true, "target_repository": "owner/repo", "target_base_ref": "dev"}
	f.putPlan(plan, 0, plan+"-r1", addRelNode("A", dag.NodeNonPR), addRelNode("B", dag.NodeNonPR), addRelNode("I", dag.NodeImplementation), addRelNode("J", dag.NodeImplementation),
		addEdge("ab", "A", "B", dag.EdgeArtifactVerified, nil), addEdge("ij", "I", "J", dag.EdgeArtifactVerified, pin))
}

// scriptedHost is the host the managed engine talks to: it retains operations by request id the way the bridge does, counts what it was asked to create and send, and lets a
// test lose a creation response, answer with other settings than were asked for, or fail a send.
type scriptedHost struct {
	mu         sync.Mutex
	operations map[string]map[string]any
	created    int
	sent       []string // the business messages, in order
	settings   map[string]any
	ledger     map[string]any
	// loseFirstCreation: the host creates the thread and retains the receipt, but the answer to the first CreateThread is lost (status unknown).
	loseFirstCreation bool
	// alterSetting: the creation receipt reports another value for this settings key.
	alterSetting string
}

func newScriptedHost(t *testing.T, settings map[string]any) *scriptedHost {
	return &scriptedHost{operations: map[string]map[string]any{}, settings: settings, ledger: map[string]any{"realPath": filepath.Join(t.TempDir(), "ledger"), "device": 1, "inode": 2}}
}

func (h *scriptedHost) RequireLedger(_ context.Context, expected map[string]any) error { return nil }
func (h *scriptedHost) LedgerIdentityRecord(context.Context) (map[string]any, error) {
	return h.ledger, nil
}
func (h *scriptedHost) GetOperation(_ context.Context, id string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.operations[id], nil
}
func (h *scriptedHost) CreateThread(_ context.Context, in managed.CreateThreadRequest) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.created++
	created := map[string]any{}
	for k, v := range h.settings {
		created[k] = v
	}
	if h.alterSetting != "" {
		created[h.alterSetting] = "something-else"
	}
	environments := created["environments"]
	thread := fmt.Sprintf("child-%d", h.created)
	created["thread"] = map[string]any{"id": thread, "environments": environments}
	delete(created, "environments")
	receipt := map[string]any{"status": "accepted", "threadId": thread, "turnId": "standby", "creation": created}
	h.operations[in.RequestID] = receipt
	if h.loseFirstCreation && h.created == 1 {
		return map[string]any{"status": "unknown"}, nil
	}
	return receipt, nil
}
func (h *scriptedHost) SendMessage(_ context.Context, in managed.SendRequest) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, in.Message)
	receipt := map[string]any{"status": "accepted", "threadId": in.ThreadID, "turnId": "business", "requestId": in.RequestID}
	h.operations[in.RequestID] = receipt
	return receipt, nil
}
func (h *scriptedHost) ReadTurn(_ context.Context, _, turn string) (*managed.Turn, error) {
	return &managed.Turn{ID: turn, Status: "completed"}, nil
}
func (h *scriptedHost) HostCall(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}
func (h *scriptedHost) Lifecycle(context.Context, string, string) (bool, string, error) {
	return true, "", nil
}

func (h *scriptedHost) counts() (created, sent int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.created, len(h.sent)
}

// tips reads a fixed tip for every branch.
type tips struct {
	sha string
	err error
}

func (t *tips) Tip(_ context.Context, repository, base string) (mergeturn.Tip, error) {
	return mergeturn.Tip{SHA: t.sha, Repository: repository, Reference: base}, t.err
}

// prs answers pull request reads from a table and records every read.
type prs struct {
	mu    sync.Mutex
	by    map[string]PullRequest
	calls []string
	err   error
}

func (p *prs) read(_ context.Context, repository string, number int64) (PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := fmt.Sprintf("%s#%d", repository, number)
	p.calls = append(p.calls, key)
	if p.err != nil {
		return PullRequest{}, p.err
	}
	pr, ok := p.by[key]
	if !ok {
		return PullRequest{}, fmt.Errorf("the scripted forge holds no %s", key)
	}
	return pr, nil
}

// openPR is the snapshot of an open, ready pull request at a head.
func openPR(repository string, number int64, head string) PullRequest {
	return PullRequest{Repository: repository, Number: number, State: "open", HeadSHA: head, BaseRef: "dev", BaseSHA: head1, Verdict: "ready",
		Checks: []Check{{RunID: "1", Name: "test", HeadSHA: head, Conclusion: "success", Attempt: 1}}, RequiredDeclared: []string{"test"}, RequiredReadable: true}
}

// releaseKit is a fixture wired for releases: a scripted host behind the real managed engine, a fixed base tip and a scripted forge.
type releaseKit struct {
	*fixture
	host   *scriptedHost
	tips   *tips
	forge  *prs
	root   string // the artifact root of the children
	marker string
	state  string
}

func newReleaseKit(t *testing.T) *releaseKit {
	t.Helper()
	f := newFixture(t)
	root := t.TempDir()
	settings := map[string]any{"sandbox": map[string]any{"type": "workspaceWrite"}, "approvalPolicy": "never", "cwd": root, "runtimeWorkspaceRoots": []any{root}, "model": "gpt-5", "reasoningEffort": "medium", "environments": []any{}}
	k := &releaseKit{fixture: f, host: newScriptedHost(t, settings), tips: &tips{sha: head1}, forge: &prs{by: map[string]PullRequest{}}, root: root, marker: t.TempDir(), state: t.TempDir()}
	f.projectParent()
	k.wire(f.sched)
	return k
}

// wire makes a scheduler a releaser: the real managed engine over the scripted host, started with the selectors the release freezes.
func (k *releaseKit) wire(s *Scheduler) {
	s.Tips, s.PRs = k.tips, k.forge.read
	s.Selectors = Selectors{MarkerRoot: k.marker, Socket: filepath.Join(k.state, "socket"), StateSelector: k.state}
	s.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) {
		engine := &managed.Start{Store: s.Store, Adapter: k.host, Now: k.fixture.clock, Socket: s.Selectors.Socket, MarkerRoot: s.Selectors.MarkerRoot, StateSelector: s.Selectors.StateSelector,
			Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
		answer, err := engine.Run(ctx, raw)
		if err != nil {
			return StartAnswer{}, err
		}
		return answerOf(answer), nil
	}
}

// request is the release request of a node: the settings, the criteria and the roots.
func (k *releaseKit) request(base bool) ReleaseRequest {
	settings := map[string]any{}
	for key, v := range k.host.settings {
		settings[key] = v
	}
	req := ReleaseRequest{Schema: SchemaReleaseRequest, Instructions: "Implement the issue and report with the manifest of your artifacts.", Criteria: releaseCriteria, CriteriaSource: "issue:test", ScopeRef: "issue:test",
		RuleVersion:   RuleVersion{SkillsDigest: dig("skills"), Model: "gpt-5", Effort: "medium", PromptTemplate: "template-1", RelayBuild: "test-build"},
		ArtifactRoots: []string{k.root}, Parent: Endpoint{HostID: "host", Settings: settings}, Child: Endpoint{HostID: "host", Title: "Child", Settings: settings}}
	if base {
		req.Base = &BaseSpec{Repository: "owner/repo", Ref: "dev"}
	}
	return req
}

func (k *releaseKit) release(plan, node string) (ReleaseResult, error) {
	k.t.Helper()
	snap := k.snapshot(plan)
	n, _ := nodeOf(snap, node)
	return k.sched.Release(context.Background(), plan, node, "parent", k.request(n.Kind == dag.NodeImplementation))
}

func (k *releaseKit) mustRelease(plan, node string) ReleaseResult {
	k.t.Helper()
	res, err := k.release(plan, node)
	if err != nil {
		k.t.Fatalf("release %s: %v", node, err)
	}
	return res
}

// refusalReason is the existing refusal reason an error carries, or "" for another kind of error.
func refusalReason(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	if err == nil {
		return ""
	}
	return "not a refusal: " + err.Error()
}

// counts of the rows a release leaves.
type rowCounts struct{ releases, requests, manifests, executions, slots, conflicts, merges int }

func (k *releaseKit) rows() rowCounts {
	return rowCounts{
		releases:   k.count("SELECT COUNT(*) FROM dag_releases"),
		requests:   k.count("SELECT COUNT(*) FROM dag_release_requests"),
		manifests:  k.count("SELECT COUNT(*) FROM dag_input_manifests"),
		executions: k.count("SELECT COUNT(*) FROM dag_node_executions"),
		slots:      k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node'"),
		conflicts:  k.count("SELECT COUNT(*) FROM coordination_conflicts"),
		merges:     k.count("SELECT COUNT(*) FROM dag_merge_checks"),
	}
}

// writeFile writes a file under dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
