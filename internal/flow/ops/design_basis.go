package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file holds what the design loop reads as the basis of a DESIGN: the one head the first DESIGN's phase reads
// (crw.designHead), and the plans the started nodes were released with, which the critic of a re-plan compares the DESIGN with
// (crw.releasedPlans).

const (
	NameDesignHead    = "designHead"
	NameReleasedPlans = "releasedPlans"
	OpDesignHead      = "crw." + NameDesignHead
	OpReleasedPlans   = "crw." + NameReleasedPlans
)

// ---- crw.designHead ----

// designHeadOp is crw.designHead(milestone): the design head of the milestone's first DESIGN, the integration tip of the run's plan
// sampled once before the phase explore. Every background explorer and the architect's first call name it as their head, so they
// read one commit whatever lands on the target meanwhile, and the architect's checkout, and so the head r0 records the DESIGN ran
// at, is made at it. The row is the replay: a resume reads the head the run sampled, never the tip of the moment.
type designHeadOp struct{ l *Lane }

// DesignHead is the answer of crw.designHead.
type DesignHead struct {
	Milestone string `json:"milestone"`
	Head      string `json:"head"`
}

func (o *designHeadOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designHead(milestone): %w", err)
	}
	r := rows(req.Journal.Events())
	plan := planOf(r, ms)
	if plan == "" {
		return engine.OpResult{}, fmt.Errorf("crw.designHead: the run read no milestone %s (crw.milestone)", ms)
	}
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	out := DesignHead{Milestone: ms, Head: tip}
	return engine.OpResult{Result: result(out), Data: map[string]any{"milestone": ms, "plan": plan, "head": tip}}, nil
}

func (o *designHeadOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.releasedPlans ----

// releasedPlansOp is crw.releasedPlans(milestone): for each node of the milestone that has started, the issue plan of the ledger
// revision it started under, as crw.issuePlan gives it to its lane. A node has started as the signal path counts it (startRowOf): a
// release of it to an executor stands, or a design row of it stands that found its issue sized for a split or a child, which its lane
// recorded (released false, kind split or child) before it escalated the node on that plan. The critic of a re-plan reads them
// (RELEASED PLANS): the whole DESIGN keeps each started node's issue as it started, every field.
type releasedPlansOp struct{ l *Lane }

// ReleasedPlan is the plan a started node was released with: the whole issue of the revision it was released under, every field of
// it, and the whole decisions its decision_refs name, so the critic sees any drift of the DESIGN from them.
type ReleasedPlan struct {
	Node string `json:"node"`
	// Kind is released (to an executor), or split or child (sized for one: no executor started, the lane escalated the node).
	Kind         string      `json:"kind"`
	Revision     string      `json:"revision"`
	DesignDigest string      `json:"design_digest"`
	Plan         DesignIssue `json:"plan"`
	Decisions    []Decision  `json:"decisions"`
}

func (o *releasedPlansOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.releasedPlans(milestone): %w", err)
	}
	r := rows(req.Journal.Events())
	nodes, out := []string{}, []ReleasedPlan{}
	for _, node := range milestoneNodes(r, ms) {
		// the started predicate of the signal path (startRowOf): a node it counts started is listed, and only such a node
		rel, kind, ok := startRowOf(r, node)
		if !ok {
			continue
		}
		revision := dataStr(rel, "revision")
		led, ok := ledgerOf(r, ms, revision)
		if !ok {
			return engine.OpResult{}, fmt.Errorf("crw.releasedPlans: the ledger has no revision %s of milestone %s, which node %s was released under", revision, ms, node)
		}
		is, found, err := designIssue(led, node)
		if err != nil {
			return engine.OpResult{}, fmt.Errorf("crw.releasedPlans: node %s: %w", node, err)
		}
		if !found {
			return engine.OpResult{}, fmt.Errorf("crw.releasedPlans: revision %s plans no issue for node %s, which was released under it", revision, node)
		}
		is.EditRegions, is.TestsFirst, is.DecisionRefs, is.OrderAfter = nonNil(is.EditRegions), nonNil(is.TestsFirst), nonNil(is.DecisionRefs), nonNil(is.OrderAfter)
		p := ReleasedPlan{Node: node, Kind: kind, Revision: revision, DesignDigest: dataStr(rel, "design_digest"), Plan: is, Decisions: []Decision{}}
		design, err := ledgerDesign(led)
		if err != nil {
			return engine.OpResult{}, fmt.Errorf("crw.releasedPlans: node %s: %w", node, err)
		}
		for _, ref := range is.DecisionRefs {
			for _, d := range design.Decisions {
				if d.ID == ref {
					p.Decisions = append(p.Decisions, d)
				}
			}
		}
		out = append(out, p)
		nodes = append(nodes, node)
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{"milestone": ms, "nodes": nodes}}, nil
}

func (o *releasedPlansOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ledgerDesign is the DESIGN a ledger row holds.
func ledgerDesign(led engine.Event) (Design, error) {
	raw, err := json.Marshal(led.Data["design"])
	if err != nil {
		return Design{}, err
	}
	return DecodeDesign(raw)
}
