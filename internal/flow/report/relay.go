package report

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// RelaySender delivers the upward half of a run through the supervisor channel (CRW-1380, design CRW-1314 §3): a report, an escalation
// and the completion digest are staged as direct messages (supervisor.Channel.StageDirect, the flow_* kinds, the message id derived from
// the relationship project:<key>, the kind and the subject <run>/<report id or logical id>), so the daemon opens a turn in the idle or
// unloaded recipient - the management seat above the project, else a legacy lead, else the store seat - and retries a busy one.
//
// Staging is all it does, and what it answers is queued: the message is in the channel, and the delivery row the Writer writes from that
// answer says queued. Whether the message was accepted or refused is the daemon's to write, in the transaction of its settle
// (supervisor's recordFlowDelivery). A staging the channel refuses (nobody above the project, a hierarchy that moved) answers no class:
// nothing is written and the report stands, so a replay stages it again.
//
// Fallback carries what the channel has no kind for: the plan summary notice to the orchestrator thread, and a notice of a run whose plan
// names no project. It is crw manage send-parent in the binary; nil leaves those notices undelivered (an error).
type RelaySender struct {
	Store    *store.Store
	Fallback Sender
}

var _ Sender = RelaySender{}

// relayKinds is the direct kind each report kind goes up as.
var relayKinds = map[string]string{
	KindMilestoneReport:        supervisor.KindFlowMilestoneReport,
	KindProjectBlocked:         supervisor.KindFlowProjectBlocked,
	KindProjectDecisionRequest: supervisor.KindFlowProjectDecisionRequest,
	KindJunDecisionRequest:     supervisor.KindFlowJunDecisionRequest,
}

// Delivers says which recipients the channel delivers to: every recipient the linkage walk named a thread for, the store seat included
// (D1c d2: a report that falls to the store seat wakes that seat too). A store seat's report whose run's plan names no project has no
// project to be staged through, and crw manage send-parent does not go to the store seat: it stands for the store seat's pump, which
// tells the seat of it, and `crw flow reports --unread` lists it.
func (r RelaySender) Delivers(rec Recipient) bool {
	switch rec.State {
	case RecipientManagement, RecipientInitiative:
		return rec.Thread != ""
	case RecipientStore:
		return rec.Thread != "" && rec.ProjectKey != ""
	}
	return false
}

// SendSupervisor implements Sender: the report is staged through project:<project>.
func (r RelaySender) SendSupervisor(ctx context.Context, s SupervisorSend) (SendResult, error) {
	kind, ok := relayKinds[s.Kind]
	if !ok || s.Project == "" || s.Run == "" || s.ReportID == "" {
		if r.Fallback != nil {
			return r.Fallback.SendSupervisor(ctx, s)
		}
		return SendResult{}, fmt.Errorf("report: report %s names no kind, run and project the supervisor channel stages it under", s.ReportID)
	}
	summary := strings.TrimSpace(s.Subject)
	if summary == "" {
		summary = s.Kind + " of run " + s.Run
	}
	return r.stage(ctx, supervisor.DirectMessage{Kind: kind, Relationship: "project:" + s.Project, Subject: supervisor.FlowSubject(s.Run, s.ReportID),
		Summary: summary, Text: s.Text, Evidence: []string{"crw flow reports --project " + s.Project + " --unread"}})
}

// SendParent implements Sender: a notice with a channel kind is staged through project:<project>; any other goes to the fallback.
func (r RelaySender) SendParent(ctx context.Context, p ParentSend) (SendResult, error) {
	if p.Kind == "" || p.Project == "" || p.Run == "" {
		if r.Fallback != nil {
			return r.Fallback.SendParent(ctx, p)
		}
		return SendResult{}, errors.New("report: the supervisor channel has no kind for notice " + p.LogicalID + " and no fallback is set")
	}
	summary, _, _ := strings.Cut(strings.TrimSpace(p.Text), "\n")
	return r.stage(ctx, supervisor.DirectMessage{Kind: p.Kind, Relationship: "project:" + p.Project, Subject: supervisor.FlowSubject(p.Run, p.LogicalID),
		Summary: summary, Text: p.Text, Evidence: []string{"crw flow show " + p.Run}})
}

// stage stages one message. A decision only Jun can make is not staged for a legacy initiative lead (a lead does not decide for Jun): it
// stands for the store seat's pump, as before.
func (r RelaySender) stage(ctx context.Context, m supervisor.DirectMessage) (SendResult, error) {
	if r.Store == nil {
		return SendResult{}, errors.New("report: the relay sender has no store")
	}
	c := &supervisor.Channel{Store: r.Store}
	if m.Kind == supervisor.KindFlowJunDecisionRequest {
		if res, err := c.Resolve(ctx, m.Relationship); err == nil && res.RecipientScopeKind == RecipientInitiative {
			return SendResult{Reason: "lead_does_not_decide_for_jun",
				Observation: "the level above the project is the initiative lead " + res.Recipient + ", who does not decide for Jun; the report stands for the store seat"}, nil
		}
	}
	answer, err := c.StageDirect(ctx, m, nil)
	if err != nil {
		var refusal supervisor.Refusal
		if errors.As(err, &refusal) {
			return SendResult{Reason: refusal.Reason, Observation: refusal.Detail}, nil
		}
		return SendResult{}, err
	}
	observation := "staged"
	if reason, _ := answer["reason"].(string); reason != "" && reason != "staged" {
		observation = reason
	} else if answer["staged"] != true {
		observation = "restaged"
	}
	id, _ := answer["messageId"].(string)
	return SendResult{Class: ResultQueued, RequestID: id, Observation: observation, State: ResultQueued}, nil
}
