package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// childHost is the host the managed engine of the lab's child starts talks to: it creates the thread it is asked for, answers the
// standby and business sends, and records the creation request (the settings and the title the child was started with). refuse makes
// the readiness of the start refuse.
type childHost struct {
	mu         sync.Mutex
	operations map[string]map[string]any
	created    []managed.CreateThreadRequest
	settings   map[string]any
	ledger     map[string]any
	// refuse is the readiness answer of the next starts ("" is ready)
	refuse string
}

func (h *childHost) RequireLedger(context.Context, map[string]any) error { return nil }
func (h *childHost) LedgerIdentityRecord(context.Context) (map[string]any, error) {
	return h.ledger, nil
}
func (h *childHost) GetOperation(_ context.Context, id string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.operations[id], nil
}
func (h *childHost) CreateThread(_ context.Context, in managed.CreateThreadRequest) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.created = append(h.created, in)
	created := map[string]any{}
	for k, v := range h.settings {
		created[k] = v
	}
	thread := fmt.Sprintf("child-thread-%d", len(h.created))
	created["thread"] = map[string]any{"id": thread, "environments": created["environments"]}
	delete(created, "environments")
	receipt := map[string]any{"status": "accepted", "threadId": thread, "turnId": "standby", "creation": created}
	h.operations[in.RequestID] = receipt
	return receipt, nil
}
func (h *childHost) SendMessage(_ context.Context, in managed.SendRequest) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	receipt := map[string]any{"status": "accepted", "threadId": in.ThreadID, "turnId": "business", "requestId": in.RequestID}
	h.operations[in.RequestID] = receipt
	return receipt, nil
}
func (h *childHost) ReadTurn(_ context.Context, _, turn string) (*managed.Turn, error) {
	return &managed.Turn{ID: turn, Status: "completed"}, nil
}
func (h *childHost) HostCall(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
	if method == "thread/read" {
		return map[string]any{"thread": map[string]any{"model": h.settings["model"], "reasoningEffort": h.settings["reasoningEffort"]}}, nil
	}
	return map[string]any{}, nil
}

// wireChild gives the run's relay the host settings doc and a managed engine over host, whose readiness answers host.refuse.
func (l *laneLab) wireChild(r *SchedRelay, doc ChildHost, host *childHost) {
	r.Child = &doc
	r.Sched.Selectors = dagsched.Selectors{MarkerRoot: doc.MarkerRoot, Socket: l.host.SocketPath, StateSelector: l.w.state}
	r.Sched.Start = func(ctx context.Context, raw []byte) (dagsched.StartAnswer, error) {
		engine := &managed.Start{Store: r.Sched.Store, Adapter: host, Socket: r.Sched.Selectors.Socket, MarkerRoot: doc.MarkerRoot, StateSelector: r.Sched.Selectors.StateSelector,
			Readiness: func(context.Context, map[string]any) (string, error) {
				host.mu.Lock()
				defer host.mu.Unlock()
				return host.refuse, nil
			}}
		answer, err := engine.Run(ctx, raw)
		if err != nil {
			return dagsched.StartAnswer{}, err
		}
		text := func(key string) string {
			for _, f := range answer {
				if f.Key == key {
					s, _ := f.Value.(string)
					return s
				}
			}
			return ""
		}
		return dagsched.StartAnswer{State: text("state"), Stage: text("stage"), Reason: text("reason"), Answer: answer}, nil
	}
}

// childHostDoc is the lab's crw-flow-child-host/1 document: one host, the settings of a workspace-write task in a directory of its own.
func childHostDoc(t *testing.T) (ChildHost, *childHost) {
	t.Helper()
	root := t.TempDir()
	settings := map[string]any{"sandbox": map[string]any{"type": "workspaceWrite"}, "approvalPolicy": "never", "cwd": root, "runtimeWorkspaceRoots": []any{root}, "model": "gpt-5",
		"reasoningEffort": "medium", "environments": []any{}}
	host := &childHost{operations: map[string]map[string]any{}, settings: map[string]any{}, ledger: map[string]any{"realPath": filepath.Join(t.TempDir(), "ledger"), "device": 1, "inode": 2}}
	for k, v := range settings {
		host.settings[k] = v
	}
	return ChildHost{Schema: ChildHostSchema, MarkerRoot: t.TempDir(), Parent: dagsched.Endpoint{HostID: "host", Settings: settings},
		Child: dagsched.Endpoint{HostID: "host", Title: "crw child", Settings: settings}}, host
}

// TestReleaseChildStartsAManagedChildWithHostSettings (CRW-1285 (2), SP-CHILD-1): a node sized for a child is released by the managed
// start of dag-release with the host settings the run holds: the request carries the parent's and the child's host and settings and the
// child's title (the host receives it prefixed with the node's issue key), the child the start admitted is bound to the node and is the
// answer (Started, its task), and the same release again answers the same child without a second start. A run with no host settings, or settings missing a field, is refused
// with the reason before anything is written (an input error); a start the managed engine refuses is an error and binds no child, never
// a ChildResult with Started false. `crw flow start --child-host FILE` checks the document, refuses one that cannot start a child, and
// pins it into the run's arguments, from which the lane builds the relay's host settings.
func TestReleaseChildStartsAManagedChildWithHostSettings(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	doc, host := childHostDoc(t)
	req := func(key string) ops.ChildRequest {
		return ops.ChildRequest{Plan: l.plan, Node: l.nodeOf[key], RunID: "run-child00020", Revision: "r0", DesignDigest: "sha256:design", Reason: "fits_child", RequestID: "c-" + key}
	}
	written := func() int {
		return l.count("SELECT COUNT(*) FROM dag_releases") + l.count("SELECT COUNT(*) FROM dag_node_executions") + l.count("SELECT COUNT(*) FROM dag_coordinator_claims")
	}

	// no host settings, and settings that name no title and no parent settings: refused with the reason, nothing written
	unset := l.relayFor("run-child00010", l.handoffBytes(nil), 1)
	if res, err := unset.ReleaseChild(ctx, req("A-1")); !errors.Is(err, ErrChildHostUnset) || !strings.HasPrefix(err.Error(), "refused:child_host_unset:") || res.Started {
		t.Errorf("no host settings: %+v %v", res, err)
	}
	bad := doc
	bad.Child.Title, bad.Parent.Settings = " ", nil
	partial := l.relayFor("run-child00011", l.handoffBytes(nil), 1)
	l.wireChild(partial, bad, host)
	if res, err := partial.ReleaseChild(ctx, req("A-1")); err == nil || !strings.HasPrefix(err.Error(), "refused:child_host_settings:") || !strings.Contains(err.Error(), "child.title") ||
		!strings.Contains(err.Error(), "parent.settings") || res.Started {
		t.Errorf("settings missing a field: %+v %v", res, err)
	}
	if n := written(); n != 0 || len(host.created) != 0 {
		t.Fatalf("a refused child release wrote %d rows and created %d threads", n, len(host.created))
	}

	// the managed engine refuses the start: an error, and no child is bound to the node (its intent and slot stay, in a lab of its own)
	refusing := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	rr := refusing.relayFor("run-child00015", refusing.handoffBytes(nil), 1)
	refusedHost := &childHost{operations: map[string]map[string]any{}, settings: host.settings, ledger: host.ledger, refuse: "role_pair_refused"}
	refusing.wireChild(rr, doc, refusedHost)
	if res, err := rr.ReleaseChild(ctx, ops.ChildRequest{Plan: refusing.plan, Node: refusing.nodeOf["A-1"], RunID: rr.RunID, Reason: "fits_child", RequestID: "c-r"}); err == nil ||
		!strings.HasPrefix(err.Error(), "refused:child_not_started:") || res.Started || res.Child != "" {
		t.Errorf("a refused start: %+v %v", res, err)
	}
	if n := refusing.count("SELECT COUNT(*) FROM dag_node_executions"); n != 0 || len(refusedHost.created) != 0 {
		t.Errorf("a refused start bound %d children and created %d threads", n, len(refusedHost.created))
	}

	// a start the engine admits: the child is bound and answered (A-1, the node the ready pass ranks first: dagsched.Release judges a
	// candidate in the pass's order, and the lab's regions are undeclared, so a lower-ranked node would be deferred behind it)
	r := l.relayFor("run-child00020", l.handoffBytes(nil), 1)
	l.wireChild(r, doc, host)
	res, err := r.ReleaseChild(ctx, req("A-1"))
	if err != nil || !res.Started || res.Child == "" {
		t.Fatalf("the child release: %+v %v", res, err)
	}
	var child, request string
	if err := l.w.st.DB.QueryRow("SELECT r.child_task_id, q.request_json FROM dag_node_executions e JOIN relationships r ON r.relationship_id = e.relationship_id JOIN dag_release_requests q ON q.plan_id = e.plan_id AND q.node_id = e.node_id WHERE e.node_id = ?",
		l.nodeOf["A-1"]).Scan(&child, &request); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Parent struct {
			TaskID   string         `json:"taskId"`
			HostID   string         `json:"hostId"`
			Settings map[string]any `json:"settings"`
		} `json:"parent"`
		Child struct {
			HostID   string         `json:"hostId"`
			Title    string         `json:"title"`
			Settings map[string]any `json:"settings"`
		} `json:"child"`
		IssueKey string `json:"issueKey"`
		Prompt   string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(request), &sent); err != nil {
		t.Fatal(err)
	}
	if child != res.Child || sent.Parent.TaskID != labOwner || sent.Parent.HostID != "host" || sent.Child.HostID != "host" || sent.Child.Title != "crw child" ||
		sent.Child.Settings["cwd"] != doc.Child.Settings["cwd"] || sent.Parent.Settings["model"] != "gpt-5" || sent.IssueKey != "A-1" || !strings.Contains(sent.Prompt, "issue-lane") {
		t.Errorf("the managed request %s for the child %s (answered %s)", request, child, res.Child)
	}
	if len(host.created) != 1 || host.created[0].Title != "A-1 · crw child" || host.created[0].CWD != doc.Child.Settings["cwd"] {
		t.Errorf("the host created %+v", host.created)
	}
	again, err := r.ReleaseChild(ctx, req("A-1"))
	if err != nil || !again.Started || again.Child != res.Child || len(host.created) != 1 {
		t.Errorf("the same release again: %+v %v (%d threads)", again, err, len(host.created))
	}

	// crw flow start --child-host: a document that cannot start a child is refused before the run starts
	write := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "child-host.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	before := len(l.td.spawned)
	code, out, errOut := l.td.do("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file, "--child-host", write(bad), "--thread", labOwner,
		"--state", l.w.state, "--socket", l.host.SocketPath, "--repo", l.repo)
	if code == 0 || len(l.td.spawned) != before || !strings.Contains(errOut, "child.title") {
		t.Errorf("a child host document missing the title: exit %d\n%s%s", code, out, errOut)
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file, "--child-host", write(doc))
	ref, ok := ChildHostOf(spec.Args)
	if !ok || filepath.Dir(ref.Path) != RunDir(l.w.state, spec.RunID) {
		t.Fatalf("the run's arguments name no pinned child host: %s", spec.Args)
	}
	pinned, err := ReadChildHost(ref)
	if err != nil || pinned.Child.Title != "crw child" || pinned.MarkerRoot != doc.MarkerRoot {
		t.Errorf("the pinned child host: %+v %v", pinned, err)
	}
	built := l.buildLaneWith("run-child00030", l.handoffBytes(nil), map[string]any{"child_host": ref}, LaneOptions{})
	if built.Child == nil || built.Child.Child.Title != "crw child" || built.Sched.Start == nil || built.Sched.Selectors.MarkerRoot != doc.MarkerRoot {
		t.Errorf("the lane's relay holds no host settings: %+v", built.Child)
	}
}
