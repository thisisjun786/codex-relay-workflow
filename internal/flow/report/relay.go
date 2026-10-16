package report

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
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
// nothing is written and the report stands, so a replay stages it again. A replay that finds its message sent with an unknown outcome
// (held_uncertain, which the daemon recorded unknown), and a report's replay that finds its message read by its recipient (the readback
// that settled such a send recorded accepted), answer no class either: nothing new was staged, and the writer answers what the rows read.
// A notice of the run (an escalation, the completion digest) found held_uncertain or read is answered as staged (queued), so the run
// writes its notice row and does not stage it again (CRW-1406).
//
// Fallback carries what the channel has no kind for: the plan summary notice to the orchestrator thread, and a notice of a run whose plan
// names no project. It is crw manage send-parent in the binary; nil leaves those notices undelivered (an error).
type RelaySender struct {
	Store    *store.Store
	Fallback Sender
}

var _ Sender = RelaySender{}

// messageRead is the state of a supervisor message its recipient's verified readback found in its thread.
const messageRead = "read"

// relayKinds is the direct kind each report kind goes up as.
var relayKinds = map[string]string{
	KindMilestoneReport:        supervisor.KindFlowMilestoneReport,
	KindProjectBlocked:         supervisor.KindFlowProjectBlocked,
	KindProjectDecisionRequest: supervisor.KindFlowProjectDecisionRequest,
	KindJunDecisionRequest:     supervisor.KindFlowJunDecisionRequest,
	KindMergeRequest:           supervisor.KindFlowMergeRequest,
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

// relayReportKind says whether a direct kind is one a report goes up as.
func relayReportKind(kind string) bool {
	for _, k := range relayKinds {
		if k == kind {
			return true
		}
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
		return SendResult{}, fmt.Errorf("report: report %s names no kind, run, or project for the 부모 channel to stage it under", s.ReportID)
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
		return SendResult{}, errors.New("report: the 부모 channel has no kind for notice " + p.LogicalID + " and no fallback is set")
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
	id, _ := answer["messageId"].(string)
	if row, _ := answer["message"].(store.Row); row != nil {
		switch row.Text("state") {
		case delivery.HeldUncertain:
			if relayReportKind(m.Kind) {
				// the message was sent and nobody heard back what the send did (CRW-1390): the channel leaves it as it is, the daemon
				// recorded the delivery unknown, and a queued row now would read as if it waited to be sent. A notice found held_uncertain
				// is answered as staged (CRW-1406): its notice row, not a delivery row, is what the run records of it, and a run process
				// that died before that row would otherwise stage the notice again on every reconcile and never record it
				return SendResult{Reason: delivery.HeldUncertain, Observation: "message " + id + " was sent and what the send did is unknown: it is not staged again, " +
					"and its delivery reads unknown until the recipient's readback settles it"}, nil
			}
		case messageRead:
			if relayReportKind(m.Kind) {
				// the recipient's verified readback found the report in its thread, and the delivery row of that settle is written
				// (accepted): a replay that read the rows before the readback (an unknown send it would stage again) stages nothing. A
				// notice found read is answered as staged, as before: its notice row, not a delivery row, is what the run records of it
				return SendResult{Reason: messageRead, Observation: "message " + id + " was read by its recipient: it is not staged again, and its delivery reads accepted"}, nil
			}
		}
	}
	observation := "staged"
	if reason, _ := answer["reason"].(string); reason != "" && reason != "staged" {
		observation = reason
	} else if answer["staged"] != true {
		observation = "restaged"
	}
	recipient, _ := answer["recipient"].(string)
	return SendResult{Class: ResultQueued, RequestID: id, Observation: observation, State: ResultQueued, Recipient: recipient}, nil
}
