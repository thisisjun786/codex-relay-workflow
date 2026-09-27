package supervisor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"

	py "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// Subset ported for todo 24; todo 23 owns and extends packet composition.
const packetVersion = "relay-packet/1"
const channelVersion = "relay-supervisor-channel/1"
const noMechanism = "a report upward is transported and read back, and nothing here records that a supervisor agreed, applied or verified anything; what became of it is read from the Linear record, confirmed"

type Packet map[string]any

func absent(reason, detail string) map[string]any {
	return map[string]any{"absent": reason, "detail": detail}
}
func messageID(o Obligation) string {
	d := sha256.Sum256([]byte("parent_to_supervisor|" + o.RelationID + "|" + purpose(o.Kind) + "|" + o.Subject))
	return hex.EncodeToString(d[:])[:32]
}
func purpose(kind string) string {
	if kind == "unreported" {
		return "blocked"
	}
	return kind
}
func packetKind(kind string) string {
	if kind == "decision_request" {
		return "decision"
	}
	return "notification"
}
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-", r)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
func command(parts ...string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = shellQuote(p)
	}
	return strings.Join(out, " ")
}
func programCommand(program string, parts ...string) string {
	if program == "" {
		program = "codex-session-relay"
	}
	if len(parts) == 0 {
		return program
	}
	return program + " " + command(parts...)
}
func (c *Channel) command(parts ...string) string {
	head := []string{"--state", filepath.Dir(c.Store.Path)}
	return programCommand(c.Program, append(head, parts...)...)
}
func reachUnmeasured() map[string]any {
	result := map[string]any{}
	for _, name := range []string{"transport_accepted", "received", "agreed", "applied", "verified"} {
		state, detail := "unmeasured", "nothing readable answered yet"
		if name != "transport_accepted" && name != "received" {
			state, detail = "not_applicable", noMechanism
		}
		result[name] = map[string]any{"state": state, "source": nil, "detail": detail}
	}
	return result
}
func (c *Channel) Compose(ctx context.Context, o Obligation, r Resolution, at string) (Packet, error) {
	kind := packetKind(o.Kind)
	issue := ""
	if o.Issue != nil {
		issue = *o.Issue
	}
	if issue == "" {
		rel, err := c.Store.Relationship(ctx, o.RelationID)
		if err != nil {
			return nil, err
		}
		issue = rel.IssueKey
	}
	if issue == "" {
		return nil, Refusal{"malformed_receipt", "a " + o.Kind + " packet cannot omit issue"}
	}
	evidence := c.command("supervisor-standing", "--project", r.ProjectKey)
	if event, ok := o.Basis["eventId"].(string); ok {
		evidence = c.command("show", "--event", event)
	}
	scope := "project " + r.ProjectKey + ", issue " + issue
	var basis any = absent("unknown", "no generation or revision was read")
	if o.Revision != nil && *o.Revision != "" {
		basis = "generation " + py.Text(o.Generation) + ", revision " + (*o.Revision)[:min(12, len(*o.Revision))]
	} else if o.Kind == "unreported" {
		description := "unreported: turn " + o.Subject + " ended without a report"
		if reason, ok := o.Basis["reason"].(string); ok && reason != "" {
			description += ", reading " + reason
		}
		if o.Generation != nil {
			description += ", generation " + py.Text(o.Generation)
		}
		basis = description
	}
	var answer any = absent("not_applicable", "this kind owes no answer")
	var decision any = absent("not_applicable", "no user decision is being asked for")
	if kind == "decision" {
		answer = "user"
		decision = strings.TrimSpace(o.Detail)
		if decision == "" {
			return nil, Refusal{"malformed_receipt", "a decision envelope cannot omit decision: <not_applicable: no user decision is being asked for>"}
		}
	}
	if at == "" {
		at = time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	}
	region := map[string]any{"version": "relay-envelope/1", "direction": "parent_to_supervisor", "kind": kind, "purpose": purpose(o.Kind), "messageId": messageID(o), "relationId": o.RelationID, "relationRevision": absent("unknown", "the link revision was not read"), "sender": map[string]any{"role": "parent", "taskId": r.Sender}, "recipient": map[string]any{"role": "supervisor", "taskId": r.Recipient}, "subject": o.Subject, "scope": scope, "basis": basis, "observedAt": at, "evidence": []string{evidence}, "correlationId": absent("not_applicable", "this message answers nothing earlier"), "replyTo": absent("not_applicable", "no reply is directed at one message"), "answerOwedBy": answer, "decision": decision, "reach": reachUnmeasured()}
	generation := o.Generation
	var artifact any
	if eventID, ok := o.Basis["eventId"].(string); ok {
		var repository, head string
		var number int64

		err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT repository,pr_number,head_sha FROM work_reports WHERE event_id=? AND pr_number IS NOT NULL AND head_sha IS NOT NULL ORDER BY submission_no DESC LIMIT 1", eventID).Scan(&repository, &number, &head)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && repository != "" {
			artifact = map[string]any{"kind": "pull_request", "repository": repository, "number": number, "headSha": head, "baseSha": nil, "url": nil}
		}
	}
	return Packet{"version": packetVersion, "envelope": region, "issue": issue, "generation": generation, "criteriaDigest": nil, "policy": nil, "callback": nil, "artifact": artifact, "evidence": []string{evidence}, "body": nil, "activation": nil}, nil
}
func (p Packet) ID() string {
	return py.Text(py.Item(py.Item(map[string]any(p), "envelope"), "messageId"))
}
