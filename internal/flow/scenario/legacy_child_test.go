package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// The issue-child path is retired (CRW-1415), and the records an old run left stay read only. A run whose journal holds a crw.releaseChild
// row started an issue child under the old project lane; the current lane sends a node sized for a child up as a size split at the
// position where the old one decided and started the child, so a resume of that run under the current script would escalate the node
// live and leave the child it started running. The resume is refused before anything is written, with a reason that names the legacy
// issue-child path; the rows of the run are what they were.

// newChildBranch is the current project lane's handling of a node sized for a child; oldChildBranch is the old lane's (before CRW-1415):
// it decided the child (SP-CHILD-2) and started it (crw.releaseChild).
const (
	newChildBranch = `    // the issue-child path is retired (CRW-1415): a node sized for a child goes up as a size split, and no child is started
    const esc = await crw.escalate(node, { reason: 'size_split', detail: 'child_retired', evidence: { size_reason: plan.size_reason } })
    return { node, state: 'escalated', reason: 'size_split', escalation: esc.id }
`
	oldChildBranch = `    await crw.spawnDecision('CHILD_2', 'spawn', { node, role: 'child', reason: plan.size_reason })
    const child = await crw.releaseChild(node, plan)
    if (!child.started) return { node, state: 'unreleased', reason: child.reason }
    return { node, state: 'child', child: child.child }
`
)

// oldReleaseChild stands in for the old crw.releaseChild: it started the node's issue child and recorded it.
type oldReleaseChild struct{ started []string }

func (o *oldReleaseChild) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var a []json.RawMessage
	if err := json.Unmarshal(req.Args, &a); err != nil || len(a) == 0 {
		return engine.OpResult{}, err
	}
	var node string
	_ = json.Unmarshal(a[0], &node)
	o.started = append(o.started, node)
	child := "child-" + node
	res, _ := json.Marshal(map[string]any{"node": node, "child": child, "started": true})
	return engine.OpResult{Result: res, Data: map[string]any{"node": node, "child": child, "started": true}}, nil
}

func (o *oldReleaseChild) Proven(context.Context, []engine.Event, engine.Event) (bool, string, error) {
	return true, "", nil
}

// legacyNight runs the night batch over the lab's journal with the project lane given and the lab's operations plus extra: the night
// batch's own script is the same before and after CRW-1415, so its run's script digest is what a resume of it checks.
func legacyNight(l *Lab, lane *script.Program, extra map[string]engine.Op, resume bool) (*engine.Result, error) {
	l.t.Helper()
	prog, err := workflows.Load(cli.NightBatchWorkflow)
	if err != nil {
		l.t.Fatal(err)
	}
	cfg := l.Config(resume)
	cfg.Args = nightArgs("CRW-A")
	opsMap := map[string]engine.Op{}
	for k, v := range cfg.Ops {
		opsMap[k] = v
	}
	for k, v := range extra {
		opsMap[k] = v
	}
	cfg.Ops = opsMap
	cfg.Workflows = func(name, path string) (*script.Program, error) {
		if name == cli.ProjectLaneWorkflow {
			return lane, nil
		}
		return workflows.Load(name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := engine.Run(ctx, prog, cfg)
	l.journal.flush()
	return res, err
}

func TestLegacyIssueChildRunIsNotResumedUnderTheNewLane(t *testing.T) {
	l := NewLab(t, NodeSpec{ID: "A", Issue: "CRW-A", Title: "a big issue", Regions: []string{"a/"}, SizeFit: "child", SizeReason: "regions_gt3"})
	text, err := workflows.Source(cli.ProjectLaneWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(text, []byte(newChildBranch)) {
		t.Fatalf("project-lane.js no longer holds the retired child branch this test replaces")
	}
	oldText := bytes.Replace(text, []byte(newChildBranch), []byte(oldChildBranch), 1)
	oldLane, err := script.Load(script.Source{Name: "project-lane.js", Text: oldText, Dir: workflows.FS})
	if err != nil {
		t.Fatal(err)
	}
	newLane, err := workflows.Load(cli.ProjectLaneWorkflow)
	if err != nil {
		t.Fatal(err)
	}

	// the old run: the project lane decided the child and started it
	child := &oldReleaseChild{}
	res, err := legacyNight(l, oldLane, map[string]engine.Op{"releaseChild": child}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != engine.RunCompleted || len(child.started) != 1 || len(l.Rows(ops.OpReleaseChild)) != 1 {
		t.Fatalf("the old run: %s %s, children started %v, releaseChild rows %d", res.Status, res.Error, child.started, len(l.Rows(ops.OpReleaseChild)))
	}
	escBefore := len(l.FlowRows(engine.EvEscalation))
	rowsBefore := len(l.Events())

	// the resume under the current lane is refused up front
	res, err = legacyNight(l, newLane, nil, true)
	var esc []string
	for _, ev := range l.FlowRows(engine.EvEscalation)[escBefore:] {
		esc = append(esc, str(ev.Data, "node")+":"+str(ev.Data, "reason")+"/"+str(ev.Data, "detail"))
	}
	if err == nil {
		lanes := ""
		if res != nil {
			lanes = string(res.Value)
		}
		t.Fatalf("a run whose journal holds a crw.releaseChild row was resumed under the new lane: escalations written live %v, closes %v, result %s", esc, l.Relay.CloseReasons(), lanes)
	}
	if !strings.Contains(err.Error(), "issue-child") || !strings.Contains(err.Error(), ops.OpReleaseChild) {
		t.Errorf("the refusal %q does not name the legacy issue-child path", err)
	}
	if len(esc) != 0 || len(l.Events()) != rowsBefore {
		t.Errorf("the refused resume wrote rows: escalations %v, rows %d -> %d", esc, rowsBefore, len(l.Events()))
	}
	if rows := l.Rows(ops.OpReleaseChild); len(rows) != 1 || str(rows[0].Data, "child") != "child-A" {
		t.Errorf("the old run's crw.releaseChild row is not kept as it was: %+v", rows)
	}
}
