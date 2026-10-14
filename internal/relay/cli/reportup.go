package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// report-up (CRW-1333, design CRW-1314 §2): a lead's report upward. The report row is a lead_report
// journal row (the report's id, kind, initiative, subject, text, its digest, the evidence and the
// thread that reported it); the message is a direct message on the supervisor channel, addressed
// through 'initiative:KEY' to the management seat above the initiative (the store seat when no
// management edge stands). Both are written in one transaction. A report with nobody above to tell is
// still recorded and the refusal says the report stands; running the same report again converges on
// the same row and the same message. The same report id with a changed text restates the report
// (CRW-1361): while its message is unsent, the channel rewrites the message and the row is updated
// with it (a lead_report_restated journal row records the change); once the message may have gone,
// neither the message nor the row is changed and the answer warns that the sent text stands.

const reportUpSchema = "relay-report-up/1"

// leadReportKind is the journal kind of a lead's report row.
const leadReportKind = "lead_report"

// leadReportID is the id a lead's report gets when the caller names none: the kind, the initiative,
// the subject and the digest of the text.
func leadReportID(kind, initiative, subject, digest string) string {
	return "lrp_" + pyvalue.SHA256Hex(strings.Join([]string{"crw-lead-report", kind, initiative, subject, digest}, "\x00"))[:24]
}

var reportUpCommand = dispatch.Command{Name: "report-up", Run: runReportUp}

func init() { dispatch.Register(nil, reportUpCommand) }

func runReportUp(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	initiative := strings.TrimSpace(args.Text("initiative"))
	kind := args.Text("kind")
	subject := strings.TrimSpace(args.Text("subject"))
	text := args.Text("text")
	if path, ok := strings.CutPrefix(text, "@"); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &dispatch.UsageError{Detail: "--text @" + path + " cannot be read: " + err.Error(), Code: contract.ExitUsage}
		}
		text = string(data)
	}
	if initiative == "" {
		return nil, &dispatch.UsageError{Detail: "--initiative names the initiative whose lead is reporting", Code: contract.ExitUsage}
	}
	if strings.TrimSpace(text) == "" && subject == "" {
		return nil, &dispatch.UsageError{Detail: "a report says something: give --text (or --text @FILE) or --subject", Code: contract.ExitUsage}
	}
	if kind == supervisor.KindLeadDecisionRequest && subject == "" {
		return nil, &dispatch.UsageError{Detail: "a lead_decision_request names the decision it asks for with --subject", Code: contract.ExitUsage}
	}
	evidence := []string{}
	for _, pointer := range args.Strings("evidence") {
		if strings.TrimSpace(pointer) != "" {
			evidence = append(evidence, pointer)
		}
	}
	digest := "sha256:" + pyvalue.SHA256Hex(text)
	reported := text
	report := strings.TrimSpace(args.Text("id"))
	if report == "" {
		report = leadReportID(kind, initiative, subject, digest)
	}
	reportedBy := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
	c, closeChannel, err := supervisorChannel(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeChannel()
	at := delivery.ISOOf(clockNow())
	// Who reports is not refused (a message is reversible: warn and record). A caller that is not
	// the initiative's lead is named in the message the management seat reads, recorded on the
	// report row and warned in the answer; an unknown caller is recorded and warned.
	var reporterIsLead any
	warning := ""
	if lead, err := c.Resolve(ctx, "initiative:"+initiative); err == nil {
		switch {
		case reportedBy == "":
			warning = "the calling thread is unknown (CODEX_THREAD_ID is not set), so whether the initiative's lead " + pyvalue.StrRepr(lead.Sender) + " made this report is not known; it is sent in the lead's name and recorded without a reporter"
		case reportedBy != lead.Sender:
			reporterIsLead = false
			warning = "the calling thread " + pyvalue.StrRepr(reportedBy) + " is not the initiative's lead " + pyvalue.StrRepr(lead.Sender) + "; the report is sent in the lead's name, and the message and the report row say who reported it"
			text = "Reported by thread " + reportedBy + ", which is not the initiative's lead (" + lead.Sender + ").\n\n" + text
		default:
			reporterIsLead = true
		}
	}
	message := supervisor.DirectMessage{Kind: kind, Relationship: "initiative:" + initiative, Subject: report, Summary: subject, Text: text, Evidence: evidence, At: at}
	// sentStands is set when a restatement found the report's message already gone: the row and the
	// message keep what was sent.
	sentStands := false
	record := func(ctx context.Context, staged map[string]any) (bool, error) {
		detail := contract.OrderedObject{{Key: "reportId", Value: report}, {Key: "kind", Value: kind}, {Key: "initiative", Value: initiative},
			{Key: "subject", Value: subject}, {Key: "text", Value: reported}, {Key: "bodyDigest", Value: digest}, {Key: "evidence", Value: anyStrings(evidence)},
			{Key: "reportedBy", Value: nullableText(reportedBy)}, {Key: "reporterIsLead", Value: reporterIsLead}, {Key: "messageId", Value: nullableText(pyvalue.Str(staged["messageId"]))},
			{Key: "recipient", Value: nullableText(pyvalue.Str(staged["recipient"]))}, {Key: "staged", Value: staged["messageId"] != nil}}
		var seq int64
		var stored string
		err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT seq, detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq LIMIT 1", leadReportKind, report).Scan(&seq, &stored)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, leadReportKind, report,
				pyjson.Dumps(detail, pyjson.Options{}))
			return false, err
		}
		if err != nil {
			return false, err
		}
		var previous struct {
			BodyDigest string `json:"bodyDigest"`
			MessageID  string `json:"messageId"`
		}
		if err := json.Unmarshal([]byte(stored), &previous); err != nil || previous.BodyDigest == digest {
			// The same report again (or a row this command cannot read, which it leaves as it is): a replay.
			return true, nil
		}
		// The text changed under the same report id. The row follows the message: it is rewritten only
		// while that message was never sent (the channel restated it above in this transaction), so the
		// row never says something the recipient did not get.
		if previous.MessageID != "" {
			var unsent int
			err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM supervisor_messages WHERE message_id = ? AND "+store.SupervisorNeverSentSQL(), previous.MessageID).Scan(&unsent)
			if err != nil {
				return false, err
			}
			var exists int
			if err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM supervisor_messages WHERE message_id = ?", previous.MessageID).Scan(&exists); err != nil {
				return false, err
			}
			if exists == 1 && unsent == 0 {
				sentStands = true
				return true, nil
			}
		}
		if _, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE journal SET detail = ? WHERE seq = ?", pyjson.Dumps(detail, pyjson.Options{}), seq); err != nil {
			return false, err
		}
		_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, "lead_report_restated", report,
			pyjson.Dumps(contract.OrderedObject{{Key: "reportId", Value: report}, {Key: "fromDigest", Value: previous.BodyDigest}, {Key: "toDigest", Value: digest},
				{Key: "messageId", Value: nullableText(pyvalue.Str(staged["messageId"]))},
				{Key: "reason", Value: "the same report id was reported again with a changed text and its message had not been sent"}}, pyjson.Options{}))
		return false, err
	}
	var replayed bool
	answer, err := c.StageDirect(ctx, message, func(ctx context.Context, staged map[string]any) error {
		var err error
		replayed, err = record(ctx, staged)
		return err
	})
	if err != nil {
		var refused supervisor.Refusal
		if !errors.As(err, &refused) {
			return nil, err
		}
		// Nobody to tell (or a hierarchy that moved, or a message this channel does not stage): the
		// report stays on record, and the refusal says it stands where it is written.
		if refused.Reason != "malformed_receipt" {
			if err := c.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
				var err error
				replayed, err = record(ctx, map[string]any{})
				return err
			}); err != nil {
				return nil, err
			}
		}
		return nil, &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: refused.Reason},
			{Key: "detail", Value: refused.Detail}, {Key: "schema", Value: reportUpSchema}, {Key: "reportId", Value: report}, {Key: "kind", Value: kind},
			{Key: "initiative", Value: initiative}, {Key: "replayed", Value: replayed}, {Key: "standing", Value: refused.Reason != "malformed_receipt"}}, Code: contract.ExitRefused}
	}
	out := contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: reportUpSchema}, {Key: "reportId", Value: report}, {Key: "kind", Value: kind},
		{Key: "initiative", Value: initiative}, {Key: "replayed", Value: replayed}, {Key: "staged", Value: answer["staged"]},
		{Key: "messageId", Value: answer["messageId"]}, {Key: "sender", Value: answer["sender"]}, {Key: "recipient", Value: answer["recipient"]},
		{Key: "recipientScopeKind", Value: answer["recipientScopeKind"]}, {Key: "reportedBy", Value: nullableText(reportedBy)}}
	if sentStands {
		stands := "report " + pyvalue.StrRepr(report) + " was already sent with its first text, so neither the sent message nor the report row is changed and the new text is not sent; report it under a new --id"
		if warning != "" {
			warning += "; " + stands
		} else {
			warning = stands
		}
	}
	if warning != "" {
		out = append(out, contract.Field{Key: "warning", Value: warning})
	}
	if reason, ok := answer["reason"].(string); ok {
		out = append(out, contract.Field{Key: "reason", Value: reason})
	}
	if readdressed, ok := answer["readdressed"]; ok {
		out = append(out, contract.Field{Key: "readdressed", Value: readdressed})
	}
	return out, nil
}

// anyStrings is values as a JSON array.
func anyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
