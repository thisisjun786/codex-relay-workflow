package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Direct messages (CRW-1333, design CRW-1314 §2): the upward and owner-bound messages the supervisor
// channel carries without an event behind them. Each is staged by StageDirect the way StageNotice
// stages a fault notice: resolved before and again under the write lock, one deterministic message
// per obligation, re-addressed or restated only while nothing of it can have gone. The transport
// (claim, fence, idle/notLoaded/busy handling, readback) is the channel's own and is not changed.
// No schema changes: the kinds are new obligation_kind values and the relationship ids carry the
// prefixes 'initiative:', 'project:' and 'run:' that Resolve reads.

// The direct obligation kinds. blocked and decision_request are deliberately not among them: those
// are the event path's kinds, which refreshProposal and newestRaising re-derive from an event.
const (
	KindLeadReport                 = "lead_report"
	KindLeadDecisionRequest        = "lead_decision_request"
	KindLeadBlocked                = "lead_blocked"
	KindFlowMilestoneReport        = "flow_milestone_report"
	KindFlowProjectBlocked         = "flow_project_blocked"
	KindFlowProjectDecisionRequest = "flow_project_decision_request"
	KindFlowEscalation             = "flow_escalation"
	KindFlowCompletion             = "flow_completion"
	KindFlowNoNodeStarted          = "flow_no_node_started"
	KindFlowJunDecisionRequest     = "flow_jun_decision_request"
	KindJunDecisionRequest         = "jun_decision_request"
	KindSessionStepWaiting         = "session_step_waiting"
)

// The relationship prefixes a direct message is addressed through (Resolve).
const (
	prefixInitiative = "initiative:"
	prefixProject    = "project:"
	prefixRun        = "run:"
)

// directKind is what one kind is: the relationship prefix its sender reports through, the
// direction and layer the envelope names, the two roles, and whether it asks for a decision and
// who owes the answer. A project kind goes to the management session above the project on the 1.0
// path (two session layers, CRW-1314); under a legacy initiative lead the envelope names the lead's
// layer and role instead (directLayer), while the direction, and so the message id, stays the kind's.
type directKind struct {
	prefix, direction, layer, senderRole, recipientRole string
	decision                                            bool
	owedBy                                              string
}

var directKinds = map[string]directKind{
	KindLeadReport:                 {prefixInitiative, "lead_to_management", "management", "lead", "management", false, ""},
	KindLeadDecisionRequest:        {prefixInitiative, "lead_to_management", "management", "lead", "management", true, "recipient"},
	KindLeadBlocked:                {prefixInitiative, "lead_to_management", "management", "lead", "management", false, ""},
	KindJunDecisionRequest:         {prefixInitiative, "lead_to_management", "management", "lead", "management", true, "user"},
	KindFlowMilestoneReport:        {prefixProject, "orchestrator_to_management", "management", "orchestrator", "management", false, ""},
	KindFlowProjectBlocked:         {prefixProject, "orchestrator_to_management", "management", "orchestrator", "management", false, ""},
	KindFlowProjectDecisionRequest: {prefixProject, "orchestrator_to_management", "management", "orchestrator", "management", true, "recipient"},
	KindFlowEscalation:             {prefixProject, "orchestrator_to_management", "management", "orchestrator", "management", false, ""},
	KindFlowCompletion:             {prefixProject, "orchestrator_to_management", "management", "orchestrator", "management", false, ""},
	KindFlowNoNodeStarted:          {prefixProject, "orchestrator_to_management", "management", "orchestrator", "management", false, ""},
	KindFlowJunDecisionRequest:     {prefixProject, "orchestrator_to_management", "management", "orchestrator", "management", true, "user"},
	KindSessionStepWaiting:         {prefixRun, "run_to_orchestrator", "orchestrator", "run", "orchestrator", false, ""},
}

// DirectKinds is the sorted list of the direct obligation kinds.
func DirectKinds() []string {
	out := make([]string, 0, len(directKinds))
	for kind := range directKinds {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// IsDirectKind reports whether kind is one StageDirect stages.
func IsDirectKind(kind string) bool { _, ok := directKinds[kind]; return ok }

// DirectMessage is one direct message: its kind, the relationship it goes through
// ('initiative:KEY', 'project:KEY' or 'run:ID'), the obligation it discharges (Subject, the
// caller's stable id for the report: one subject is one message), a one-line summary (the decision
// asked for, for a decision kind), the text, the evidence pointers and the staging time.
type DirectMessage struct {
	Kind, Relationship, Subject, Summary, Text string
	Evidence                                   []string
	At                                         string
}

// maxDirectBody is how much of a message's text the packet carries; the caller's own record holds
// the whole of it.
const maxDirectBody = 6000

// DirectMessageID is the message id of m: the direction, the relationship, the kind and the subject.
func DirectMessageID(m DirectMessage) string {
	return pyvalue.SHA256Hex(directKinds[m.Kind].direction + "|" + m.Relationship + "|" + m.Kind + "|" + m.Subject)[:32]
}

// checkDirect refuses a message StageDirect does not stage, before anything is read.
func checkDirect(m DirectMessage) error {
	k, ok := directKinds[m.Kind]
	if !ok {
		return Refusal{"malformed_receipt", "a direct message is one of " + strings.Join(DirectKinds(), ", ") + ", not " + pyvalue.StrRepr(m.Kind) + "; blocked and decision_request are raised by events"}
	}
	key, ok := strings.CutPrefix(m.Relationship, k.prefix)
	if !ok || strings.TrimSpace(key) == "" {
		return Refusal{"malformed_receipt", "a " + m.Kind + " goes through " + pyvalue.StrRepr(k.prefix+"<key>") + ", not " + pyvalue.StrRepr(m.Relationship)}
	}
	if strings.TrimSpace(m.Subject) == "" {
		return Refusal{"malformed_receipt", "a direct message names the obligation it discharges (its subject), so that one report is one message"}
	}
	if k.decision && strings.TrimSpace(m.Summary) == "" {
		return Refusal{"malformed_receipt", "a " + m.Kind + " states the decision it asks for (its summary)"}
	}
	if strings.TrimSpace(m.Text) == "" && strings.TrimSpace(m.Summary) == "" {
		return Refusal{"malformed_receipt", "a direct message says something: its text and its summary are both empty"}
	}
	return nil
}

// composeDirect is the role-neutral packet of a direct message: the relay-envelope/1 shape the
// channel renders and reads back, with the layer and both roles named by the kind rather than fixed
// to parent and supervisor.
func (c *Channel) composeDirect(m DirectMessage, r Resolution, at string) Packet {
	k := directKinds[m.Kind]
	id := DirectMessageID(m)
	evidence := []any{}
	for _, pointer := range m.Evidence {
		if strings.TrimSpace(pointer) != "" {
			evidence = append(evidence, pointer)
		}
	}
	if len(evidence) == 0 {
		evidence = append(evidence, c.command("supervisor-show", "--message", id))
	}
	key := strings.TrimPrefix(m.Relationship, k.prefix)
	scope := strings.TrimSuffix(k.prefix, ":") + " " + key
	if r.ProjectKey != "" && k.prefix != prefixProject {
		scope += ", project " + r.ProjectKey
	}
	kind := "notification"
	var answer any = absent("not_applicable", "this kind owes no answer")
	var decision any = absent("not_applicable", "no decision is being asked for")
	if k.decision {
		kind, answer, decision = "decision", k.owedBy, strings.TrimSpace(m.Summary)
	}
	var basis any = absent("not_applicable", "the message states its own summary")
	if summary := strings.TrimSpace(m.Summary); summary != "" {
		basis = summary
	}
	var body any
	if text := strings.TrimSpace(m.Text); text != "" {
		body = truncateText(text, maxDirectBody)
	}
	layer, recipientRole := directLayer(k, r)
	envelope := map[string]any{"version": "relay-envelope/1", "direction": k.direction, "layer": layer, "kind": kind, "purpose": m.Kind,
		"messageId": id, "relationId": m.Relationship, "relationRevision": absent("unknown", "the link revision was not read"),
		"sender":    map[string]any{"role": k.senderRole, "taskId": r.Sender},
		"recipient": map[string]any{"role": recipientRole, "taskId": r.Recipient, "scopeKind": r.RecipientScopeKind},
		"subject":   m.Subject, "scope": scope, "basis": basis, "observedAt": at, "evidence": evidence,
		"correlationId": absent("not_applicable", "this message answers nothing earlier"),
		"replyTo":       absent("not_applicable", "no reply is directed at one message"), "answerOwedBy": answer, "decision": decision,
		"reach": reachUnmeasured()}
	return Packet{"version": packetVersion, "envelope": envelope, "issue": nil, "generation": nil, "criteriaDigest": nil, "policy": nil,
		"callback": nil, "artifact": nil, "evidence": evidence, "body": body, "activation": nil}
}

// directLayer is the layer and the recipient role the envelope of a kind names for resolution r: the
// kind's own, except that a project kind resolved to a legacy initiative lead names the lead's.
func directLayer(k directKind, r Resolution) (string, string) {
	if k.prefix == prefixProject && r.RecipientScopeKind == scopeKindInitiative {
		return "lead", "lead"
	}
	return k.layer, k.recipientRole
}

// truncateText keeps at most limit bytes of text without splitting a character.
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// directDumps is the stored encoding of a direct packet (the notice encoding: sorted keys).
func directDumps(p Packet) string { return noticeDumps(map[string]any(p)) }

// StageDirect stages one direct message. The resolution is read, the packet composed, and under the
// write lock the resolution is read again: a hierarchy that moved in between refuses
// relation_owner_drift and writes nothing. A message already staged is left alone when nothing about
// it moved; one never sent is re-addressed to the live recipient or restated with the new text; one
// that may have gone is never touched. within, when set, runs inside the same transaction with the
// answer, so a caller's own record of the report commits with the staging or not at all. A refusal
// to resolve (nobody above to tell) stages nothing and is returned as the Refusal it is.
func (c *Channel) StageDirect(ctx context.Context, m DirectMessage, within func(context.Context, map[string]any) error) (map[string]any, error) {
	if err := checkDirect(m); err != nil {
		return nil, err
	}
	// The kind's grade: a decision the user owes is still heard through a store seat laid down for a
	// management seat, and nothing else is (ResolveFor). Both resolves read at it.
	grade := directGrade(directKinds[m.Kind])
	resolution, err := c.ResolveFor(ctx, m.Relationship, grade)
	if err != nil {
		return nil, err
	}
	at := m.At
	if at == "" {
		at = c.nowISO()
	}
	packet := c.composeDirect(m, resolution, at)
	id := DirectMessageID(m)
	var answer map[string]any
	err = c.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		live, err := c.ResolveFor(ctx, m.Relationship, grade)
		if err != nil {
			return err
		}
		if live != resolution {
			return Refusal{"relation_owner_drift", "the hierarchy moved while this was being decided: it was read as " + pyvalue.StrRepr(resolution.Sender) + " reporting to " +
				pyvalue.StrRepr(resolution.Recipient) + ", and under the write lock it is " + pyvalue.StrRepr(live.Sender) + " reporting to " + pyvalue.StrRepr(live.Recipient) +
				". Nothing was written; staging again addresses the message to the one there now"}
		}
		finish := func(staged bool, reason string, extra map[string]any) error {
			row, err := c.Store.One(ctx, "SELECT * FROM supervisor_messages WHERE message_id=?", id)
			if err != nil {
				return err
			}
			answer = map[string]any{"schema": channelVersion, "staged": staged, "messageId": id, "kind": m.Kind, "relationshipId": m.Relationship,
				"subject": m.Subject, "message": row, "recipient": row.Get("recipient_task_id"), "sender": row.Get("sender_task_id"),
				"recipientScopeKind": live.RecipientScopeKind}
			if reason != "" {
				answer["reason"] = reason
			}
			for key, value := range extra {
				answer[key] = value
			}
			if within != nil {
				return within(ctx, answer)
			}
			return nil
		}
		existing, err := c.Store.One(ctx, "SELECT * FROM supervisor_messages WHERE message_id=?", id)
		if err != nil {
			return err
		}
		encoded := directDumps(packet)
		if existing == nil {
			row := store.SupervisorMessagesRow{MessageID: id, ObligationID: m.Subject, ObligationKind: m.Kind, RelationshipID: m.Relationship,
				ProjectKey: nullString(live.ProjectKey), Purpose: m.Kind, Kind: pyvalue.Str(packet["envelope"].(map[string]any)["kind"]),
				SenderTaskID: live.Sender, RecipientTaskID: live.Recipient, Subject: m.Subject, Packet: encoded, State: "queued", StagedAt: at, UpdatedAt: at}
			staged, err := c.Store.StageSupervisorMessage(ctx, row)
			if err != nil {
				return err
			}
			if staged {
				if err := c.directJournal(ctx, at, "supervisor_direct_staged", id, map[string]any{"kind": m.Kind, "relationshipId": m.Relationship,
					"subject": m.Subject, "sender": live.Sender, "recipient": live.Recipient, "recipientScopeKind": live.RecipientScopeKind}); err != nil {
					return err
				}
				return finish(true, "staged", nil)
			}
			return finish(false, "another caller staged this message first", nil)
		}
		// The time a message was observed is not part of what it says: the comparison keeps the
		// stored observedAt, so a replay with a new clock is not a restatement.
		stored := pyvalue.Str(existing.Get("packet"))
		if observed := storedObservedAt(stored); observed != "" {
			packet["envelope"].(map[string]any)["observedAt"] = observed
			encoded = directDumps(packet)
		}
		moved := existing.Text("sender_task_id") != live.Sender || existing.Text("recipient_task_id") != live.Recipient
		same := encoded == stored
		if same && !moved && existing.Get("hold_reason") == nil {
			return finish(false, "this message is already staged; one report is one message", nil)
		}
		// A superseded_by_report hold is kept or released by the hold rule (holdDecision) alone. The
		// statement is the same when the report composed with the stored sender is the stored packet, so
		// another sender session repeating it states the same thing; a direct report is owed while its
		// sender stages it. The update below writes a release; a kept hold returns here.
		if existing.Text("hold_reason") == store.SupervisorHoldSuperseded {
			sameRecipient := existing.Text("recipient_task_id") == live.Recipient
			sameStatement := same
			if !sameStatement {
				stored := live
				stored.Sender = existing.Text("sender_task_id")
				restated := c.composeDirect(m, stored, at)
				if observed := storedObservedAt(pyvalue.Str(existing.Get("packet"))); observed != "" {
					restated["envelope"].(map[string]any)["observedAt"] = observed
				}
				sameStatement = directDumps(restated) == pyvalue.Str(existing.Get("packet"))
			}
			kind, err := holdKindOf(ctx, c.Store, id, existing.Text("hold_reason"), existing.Text("obligation_kind"))
			if err != nil {
				return err
			}
			verdict, why, err := holdDecision(kind, holdChange{Entry: entryRestage, SameStatement: sameStatement, SameRecipient: sameRecipient}, nil)
			if err != nil {
				return err
			}
			if verdict != holdRelease {
				return finish(false, why, nil)
			}
		}
		result, err := noticeExec(ctx, c.Store, "UPDATE supervisor_messages SET packet=?,sender_task_id=?,recipient_task_id=?,project_key=?,state='queued',next_eligible_at=NULL,"+
			"hold_reason=CASE WHEN hold_reason IN (?,?) THEN NULL ELSE hold_reason END,updated_at=? WHERE message_id=? AND "+store.SupervisorNeverSentSQL()+
			" AND packet=? AND sender_task_id=? AND recipient_task_id=?", encoded, live.Sender, live.Recipient, nullString(live.ProjectKey),
			store.SupervisorHoldSuperseded, store.SupervisorHoldUnaddressed, at, id, stored, existing.Text("sender_task_id"), existing.Text("recipient_task_id"))
		if err != nil {
			return err
		}
		if result != 1 {
			return finish(false, "this message may already have gone, so it is left exactly as it is", nil)
		}
		if moved {
			if err := c.directJournal(ctx, at, "supervisor_message_readdressed", id, map[string]any{"from": existing.Get("recipient_task_id"), "to": live.Recipient,
				"fromSender": existing.Get("sender_task_id"), "toSender": live.Sender, "releasedHold": existing.Get("hold_reason"), "releasedState": existing.Get("state"),
				"reason": "the message was never sent and the level above moved, so it goes to the one there now"}); err != nil {
				return err
			}
		}
		if !same {
			if err := c.directJournal(ctx, at, "supervisor_message_restated", id, map[string]any{"at": "staging",
				"reason": "what the message says or whom it is addressed to moved and nothing had been sent"}); err != nil {
				return err
			}
		}
		return finish(false, "", map[string]any{"readdressed": moved, "restated": !same})
	})
	return answer, err
}

// storedObservedAt is the observedAt of a stored packet, or "".
func storedObservedAt(packet string) string {
	var top struct {
		Envelope struct {
			ObservedAt string `json:"observedAt"`
		} `json:"envelope"`
	}
	if err := json.Unmarshal([]byte(packet), &top); err != nil {
		return ""
	}
	return top.Envelope.ObservedAt
}

// directJournal writes one journal row of a direct message, its detail in sorted-key JSON.
func (c *Channel) directJournal(ctx context.Context, at, kind, id string, detail map[string]any) error {
	_, err := noticeExec(ctx, c.Store, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", at, kind, id, noticeDumps(detail))
	return err
}

// nowISO is the channel's clock as the store writes it.
func (c *Channel) nowISO() string {
	if c.clockISO != nil {
		return c.clockISO()
	}
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}
