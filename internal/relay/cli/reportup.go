package cli

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// report-up (CRW-1333, design CRW-1314 §2): a lead's report upward. The report row is a lead_report
// journal row (the report's id, kind, initiative, subject, text, its digest, the evidence and the
// thread that reported it); the message is a direct message on the supervisor channel, addressed
// through 'initiative:KEY' to the management seat above the initiative (the store seat when no
// management edge stands). Both are written in one transaction. A report with nobody above to tell is
// still recorded and the refusal says the report stands; running the same report again converges on
// the same row and the same message.

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
	message := supervisor.DirectMessage{Kind: kind, Relationship: "initiative:" + initiative, Subject: report, Summary: subject, Text: text, Evidence: evidence, At: at}
	record := func(ctx context.Context, staged map[string]any) (bool, error) {
		var one int
		err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT 1 FROM journal WHERE kind = ? AND subject = ? LIMIT 1", leadReportKind, report).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		detail := contract.OrderedObject{{Key: "reportId", Value: report}, {Key: "kind", Value: kind}, {Key: "initiative", Value: initiative},
			{Key: "subject", Value: subject}, {Key: "text", Value: text}, {Key: "bodyDigest", Value: digest}, {Key: "evidence", Value: anyStrings(evidence)},
			{Key: "reportedBy", Value: nullableText(reportedBy)}, {Key: "messageId", Value: nullableText(pyvalue.Str(staged["messageId"]))},
			{Key: "recipient", Value: nullableText(pyvalue.Str(staged["recipient"]))}, {Key: "staged", Value: staged["messageId"] != nil}}
		_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, leadReportKind, report,
			pyjson.Dumps(detail, pyjson.Options{}))
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
		{Key: "recipientScopeKind", Value: answer["recipientScopeKind"]}}
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
