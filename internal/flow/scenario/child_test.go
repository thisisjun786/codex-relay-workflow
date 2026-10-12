package scenario

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// A ChildRun is the issue-lane run of a child (SP-CHILD-1): a run of its own - its own run id, journal and actor, the child thread's task
// - over the lab's store, host, repository and relay. It is what the project run's releaseChild starts in production; the lab starts it
// by hand and reads its journal the way `crw flow apply` does.
type ChildRun struct {
	lab     *Lab
	ID      string
	Actor   string
	journal *hookJournal
	backend *agent.Backend
	lane    *ops.Lane
	client  *appserver.Client
}

// allEvents are the rows of the lab's run and of every child run it started.
func (l *Lab) allEvents() []engine.Event {
	out := l.Events()
	l.mu.Lock()
	children := append([]*ChildRun(nil), l.children...)
	l.mu.Unlock()
	for _, c := range children {
		out = append(out, c.journal.Events()...)
	}
	return out
}

// NewChild makes the issue-lane run runID of the child task actor.
func (l *Lab) NewChild(runID, actor string) *ChildRun {
	l.t.Helper()
	c := &ChildRun{lab: l, ID: runID, Actor: actor}
	owned := agent.NewThreads()
	c.client = appserver.New(l.Host.SocketPath, appserver.DefaultBounds, agent.ClientOption(owned))
	l.t.Cleanup(func() { _ = c.client.Close() })
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: l.Repo, StateDir: l.State, RunID: runID,
		Base: func(string) (string, error) { return l.Relay.IntegrationTip(context.Background(), "") }})
	if err != nil {
		l.t.Fatal(err)
	}
	c.backend, err = agent.New(agent.Config{Client: c.client, Allocator: alloc, Threads: owned, PollEvery: 25 * time.Millisecond, InterruptWait: 5 * time.Second, Owner: runID,
		OwnerAlive: func(o string) bool { return o == runID }})
	if err != nil {
		l.t.Fatal(err)
	}
	j, err := journal.Open(context.Background(), l.Store, runID, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: actor, EpochNonce: "nonce-child"}, journal.Options{})
	if err != nil {
		l.t.Fatal(err)
	}
	c.journal = &hookJournal{Journal: j}
	c.journal.flush = func() { _ = j.Flush() }
	c.lane = ops.NewLane(ops.Deps{RunID: runID, Repo: l.Repo, Journal: c.journal, Relay: l.Relay, Verifier: l.Verify, Decisions: ops.StoreDecisions{DB: l.Store.DB}, Actor: actor,
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
	l.mu.Lock()
	l.children = append(l.children, c)
	l.mu.Unlock()
	return c
}

// IssuePlan is the plan of a node as the project run released the child with it (crw.issuePlan's IssuePlan): the child's run has no
// release of its own, so the plan is its argument.
func (l *Lab) IssuePlan(node string) map[string]any {
	l.t.Helper()
	for _, n := range l.Nodes {
		if n.ID != node {
			continue
		}
		crit := n.criteria()
		return map[string]any{
			"issue": n.Issue, "node": n.ID, "title": n.Title, "revision": "r0", "design_digest": sha("design of " + n.ID), "milestone": "M1", "approach": "approach of " + n.ID,
			"regions": nz(n.Regions), "tests_first": []string{"test of " + n.ID}, "done_condition": "done condition of " + n.ID, "criteria": crit, "criteria_digest": sha(strings.Join(crit, "\n")),
			"decisions": []any{}, "answers": []any{}, "size_fit": "child", "size_reason": n.SizeReason,
		}
	}
	l.t.Fatalf("no node %s", node)
	return nil
}

// Events are the rows of the child's journal.
func (c *ChildRun) Events() []engine.Event { return c.journal.Events() }

// Run runs the issue lane of a node.
func (c *ChildRun) Run(node string) *engine.Result {
	c.lab.t.Helper()
	prog, err := workflows.Load("issue-lane")
	if err != nil {
		c.lab.t.Fatal(err)
	}
	args, err := json.Marshal(map[string]any{"node": node, "issue": c.lab.IssuePlan(node)})
	if err != nil {
		c.lab.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := engine.Run(ctx, prog, engine.Config{Backend: c.backend, Policy: c.lab.Policy, Journal: c.journal, RunID: c.ID, Args: args, Ops: c.lane.Ops(), InputSnapshot: c.lane.InputSnapshot,
		Prover: ops.NewProver(), Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
	if err != nil {
		c.lab.t.Fatal(err)
	}
	c.journal.flush()
	return res
}
