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
	// Asked is, for a node sized for a bundle split, whether the 부모 was asked (CRW-1437): the notice of the bundle_split_request
	// escalation its lane wrote was delivered (bundleSplitRequest). A send that failed writes no notice row, a failed upward hook does
	// not stop the run, and a notice the channel staged is not delivered yet, so neither the escalation nor a notice row alone says the
	// request reached the 부모. Absent for any other kind.
	Asked bool `json:"asked,omitempty"`
	// Request is, for a node sized for a bundle split, what became of the notice of its request (bundleSplitRequest): delivered
	// (Asked), or queued when no delivery of it is recorded (a staged notice, which a held_uncertain send of a flow escalation is
	// answered as too, or a notice row with no result member, which an earlier binary wrote). Absent with no notice row, and for any
	// other kind.
	Request string `json:"request,omitempty"`
}

// EvEscalationNotice is the flow row that records that an escalation of the run was told to the level above (report.KindEscalationNotice,
// written by the run's upward hook once the channel accepted or queued the notice): its escalation_id names the escalation, its result
// what the channel did with the notice (accepted, the one delivered result, or queued).
const EvEscalationNotice = "escalation_notice"

// EvMessageDelivery is the flow row the daemon's settle of a staged flow message appends to the run (supervisor.FlowMessageDeliveryKind,
// CRW-1380): its message_id names the message, its result what the transport came to (accepted: delivered; unknown; refused).
const EvMessageDelivery = "message_delivery"

// NoticeDelivered is the result of an escalation_notice row, or of a message_delivery row, whose notice was delivered (report.ResultAccepted).
const NoticeDelivered = "accepted"

// askBundleSplit is the ask of the escalation a node sized for a bundle split goes up with (project-lane.js, CRW-1437).
const askBundleSplit = "bundle_split_request"

// The requests a released plan of a bundle split names (ReleasedPlan.Request).
const (
	requestDelivered = "delivered"
	requestQueued    = "queued"
)

// bundleSplitRequest tells what became of the bundle split request of node: the last escalation of the node that asks
// bundle_split_request and the escalation_notice row of it. It is delivered when that row's result is accepted (the channel delivered
// the notice), or when a message_delivery row the journal holds says the message the row names (request_id) was accepted (the daemon
// delivered a notice the channel staged). Any other row is queued: a staged notice with no delivery, a delivery that reads unknown or
// refused, a delivery the daemon records after the run read its journal (the run's next open reads it), or a notice row with no result
// member. With no notice row it is "" (not told).
func bundleSplitRequest(r rows, node string) string {
	esc, ok := r.flow(engine.EvEscalation).last(func(ev engine.Event) bool { return dataStr(ev, "node") == node && dataStr(ev, "ask") == askBundleSplit })
	if !ok {
		return ""
	}
	id := dataStr(esc, "escalation_id")
	notice, told := r.flow(EvEscalationNotice).last(func(ev engine.Event) bool { return id != "" && dataStr(ev, "escalation_id") == id })
	if !told {
		return ""
	}
	if dataStr(notice, "result") == NoticeDelivered {
		return requestDelivered
	}
	message := dataStr(notice, "request_id")
	if _, ok := r.flow(EvMessageDelivery).last(func(ev engine.Event) bool {
		return message != "" && dataStr(ev, "message_id") == message && dataStr(ev, "result") == NoticeDelivered
	}); ok {
		return requestDelivered
	}
	return requestQueued
}

func (o *releasedPlansOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.releasedPlans(milestone): %w", err)
	}
	out, err := releasedPlans(rows(req.Journal.Events()), ms)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.releasedPlans: %w", err)
	}
	nodes := make([]string, 0, len(out))
	for _, p := range out {
		nodes = append(nodes, p.Node)
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
