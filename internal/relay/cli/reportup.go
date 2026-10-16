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
// the same row and the same message. The same report id with a changed subject, evidence or text
// restates the report (CRW-1361), and the row and the message never disagree: while the message is
// unsent and the channel restates it, the row is updated in the same transaction (a lead_report_restated
// journal row records the change); when the channel refuses to stage, neither is changed and the
// refusal warns; once the message may have gone (read from the message itself, not the row), neither is
// changed and the answer warns that the sent text stands. One id is one report of one kind for one
// initiative: the same id with another kind or initiative is an input error and writes nothing.

const reportUpSchema = "relay-report-up/1"

// leadReportKind is the journal kind of a lead's report row.
const leadReportKind = "lead_report"

// leadReportID is the id a lead's report gets when the caller names none: the kind, the initiative,
// the subject and the digest of the text.
func leadReportID(kind, initiative, subject, digest string) string {
	return "lrp_" + pyvalue.SHA256Hex(strings.Join([]string{"crw-lead-report", kind, initiative, subject, digest}, "\x00"))[:24]
}

// reportDigest is the digest of what a report says: its subject, its evidence and its text together.
// A report again under its id with another digest is a restatement (CRW-1361).
func reportDigest(subject string, evidence []string, text string) string {
	if evidence == nil {
		evidence = []string{}
	}
	encoded, _ := json.Marshal([]any{subject, evidence, text})
	return "sha256:" + pyvalue.SHA256Hex(string(encoded))
}

// reportIDReused is the refusal of a report under a recorded id with another kind or initiative: the
// message id is the kind, the initiative and the id, so it would stage a second message beside the
// first. It is an input error; nothing is written.
type reportIDReused struct {
	report, recordedKind, recordedInitiative, kind, initiative string
}

func (e reportIDReused) Error() string {
	return "report " + pyvalue.StrRepr(e.report) + " is recorded as a " + pyvalue.StrRepr(e.recordedKind) + " for initiative " + pyvalue.StrRepr(e.recordedInitiative) +
		", and this report names a " + pyvalue.StrRepr(e.kind) + " for initiative " + pyvalue.StrRepr(e.initiative) +
		": one --id is one report of one kind for one initiative, and another kind or initiative would stage a second message beside the first. Nothing was written; report it under a new --id"
}

// recordedReport is what a lead_report row says, as far as a restatement reads it.
type recordedReport struct {
	Kind       string   `json:"kind"`
	Initiative string   `json:"initiative"`
	Subject    string   `json:"subject"`
	Text       string   `json:"text"`
	Evidence   []string `json:"evidence"`
	BodyDigest string   `json:"bodyDigest"`
	MessageID  any      `json:"messageId"`
}

// readReport is the lead_report row of report: its seq, its stored detail and that detail read; found
// is false when there is none. A detail this command cannot read comes back with ok false.
func readReport(ctx context.Context, q store.Querier, report string) (seq int64, stored string, previous recordedReport, found, ok bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT seq, detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq LIMIT 1", leadReportKind, report).Scan(&seq, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", previous, false, false, nil
	}
	if err != nil {
		return 0, "", previous, false, false, err
	}
	ok = json.Unmarshal([]byte(stored), &previous) == nil
	return seq, stored, previous, true, ok, nil
}

// stagedText is a member of the channel's answer as text: "" when it is absent or null.
func stagedText(staged map[string]any, key string) string {
	if value, ok := staged[key].(string); ok {
		return value
	}
	return ""
}

var reportUpCommand = dispatch.Command{Name: "report-up", Run: runReportUp}

// reportUpAfterPrecheck runs between report-up's check of a recorded --id and its staging, where a second call can land: a test
// seam (CRW-1390), nothing in the binary.
var reportUpAfterPrecheck = func() {}

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
	whole := reportDigest(subject, evidence, text)
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
	// report row and warned in the answer; an unknown caller is recorded and warned. The line that
	// names it is the channel's preface, not the reporter's words: the body still starts with it, and
	// who reported a report does not make it another statement (CRW-1558).
	var reporterIsLead any
	warning, preface := "", ""
	if lead, err := c.Resolve(ctx, "initiative:"+initiative); err == nil {
		switch {
		case reportedBy == "":
			warning = "the calling thread is unknown (CODEX_THREAD_ID is not set), so whether the initiative's lead " + pyvalue.StrRepr(lead.Sender) + " made this report is not known; it is sent in the lead's name and recorded without a reporter"
		case reportedBy != lead.Sender:
			reporterIsLead = false
			warning = "the calling thread " + pyvalue.StrRepr(reportedBy) + " is not the initiative's lead " + pyvalue.StrRepr(lead.Sender) + "; the report is sent in the lead's name, and the message and the report row say who reported it"
			preface = "Reported by thread " + reportedBy + ", which is not the initiative's lead (" + lead.Sender + ")."
		default:
			reporterIsLead = true
		}
	}
	message := supervisor.DirectMessage{Kind: kind, Relationship: "initiative:" + initiative, Subject: report, Summary: subject, Text: text, Preface: preface, Evidence: evidence, At: at}
	// The message this report is: one id, one kind and one initiative are one message, so whether it was
	// sent is read from it, whatever the row says (a row recorded with nobody above names none).
	messageID := supervisor.DirectMessageID(message)
	reused := func(previous recordedReport) error {
		if previous.Kind != kind || previous.Initiative != initiative {
			return reportIDReused{report: report, recordedKind: previous.Kind, recordedInitiative: previous.Initiative, kind: kind, initiative: initiative}
		}
		return nil
	}
	// The same id with another kind or initiative is refused before anything is staged (and again under
	// the write lock, below).
	if _, _, previous, found, ok, err := readReport(ctx, c.Store.Q(ctx), report); err != nil {
		return nil, err
	} else if found && ok {
		if err := reused(previous); err != nil {
			return nil, reportIDReusedExit(err.(reportIDReused), kind, initiative)
		}
	}
	reportUpAfterPrecheck()
	// sentStands is set when a restatement found the report's message already gone, and heldStands when
	// the channel refused to stage while the report's unsent message stands: in both the row and the
	// message keep what they say.
	sentStands, heldStands := false, false
	record := func(ctx context.Context, staged map[string]any) (bool, error) {
		detail := contract.OrderedObject{{Key: "reportId", Value: report}, {Key: "kind", Value: kind}, {Key: "initiative", Value: initiative},
			{Key: "subject", Value: subject}, {Key: "text", Value: text}, {Key: "bodyDigest", Value: digest}, {Key: "evidence", Value: anyStrings(evidence)},
			{Key: "reportedBy", Value: nullableText(reportedBy)}, {Key: "reporterIsLead", Value: reporterIsLead}, {Key: "messageId", Value: nullableText(stagedText(staged, "messageId"))},
			{Key: "recipient", Value: nullableText(stagedText(staged, "recipient"))}, {Key: "staged", Value: stagedText(staged, "messageId") != ""}}
		encoded := pyjson.Dumps(detail, pyjson.Options{})
		seq, stored, previous, found, ok, err := readReport(ctx, c.Store.Q(ctx), report)
		if err != nil {
			return false, err
		}
		if !found {
			_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, leadReportKind, report, encoded)
			return false, err
		}
		if !ok {
			// A row this command cannot read is left as it is: a replay.
			return true, nil
		}
		if err := reused(previous); err != nil {
			return false, err
		}
		var exists, unsent int
		if err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(CASE WHEN "+store.SupervisorNeverSentSQL()+" THEN 1 ELSE 0 END), 0) FROM supervisor_messages WHERE message_id = ?",
			messageID).Scan(&exists, &unsent); err != nil {
			return false, err
		}
		sent := exists == 1 && unsent == 0
		stagedNow := stagedText(staged, "messageId") != ""
		recordedID, _ := previous.MessageID.(string)
		if reportDigest(previous.Subject, previous.Evidence, previous.Text) == whole {
			// The same report again: a replay. When the channel staged it in this transaction and the row
			// does not say so (a row recorded with nobody above, or a message re-addressed while unsent),
			// the row takes the message's id and recipient, so the two agree; a sent message's row only
			// gains the id it lacked.
			if stagedNow && stored != encoded && (!sent || recordedID == "" || recordedID == "None") {
				if _, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE journal SET detail = ? WHERE seq = ?", encoded, seq); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		switch {
		case sent:
			// The message may have gone: neither it (the channel left it as it is) nor the row is changed.
			sentStands = true
			return true, nil
		case exists == 1 && !stagedNow:
			// The channel refused to stage, so the unsent message keeps what it says; so does the row.
			heldStands = true
			return true, nil
		}
		// The message was restated (or staged for the first time) in this transaction, or there is none
		// and nobody above to tell: the row says what the message says.
		if _, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE journal SET detail = ? WHERE seq = ?", encoded, seq); err != nil {
			return false, err
		}
		_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, "lead_report_restated", report,
			pyjson.Dumps(contract.OrderedObject{{Key: "reportId", Value: report}, {Key: "fromDigest", Value: previous.BodyDigest}, {Key: "toDigest", Value: digest},
				{Key: "fromReportDigest", Value: reportDigest(previous.Subject, previous.Evidence, previous.Text)}, {Key: "toReportDigest", Value: whole},
				{Key: "messageId", Value: nullableText(stagedText(staged, "messageId"))},
				{Key: "reason", Value: "the same report id was reported again with a changed subject, evidence or text and its message had not been sent"}}, pyjson.Options{}))
		return false, err
	}
	stands := func() string {
		switch {
		case sentStands:
			return "report " + pyvalue.StrRepr(report) + " was already sent with its first text, so neither the sent message nor the report row is changed and the new text is not sent; report it under a new --id"
		case heldStands:
			return "report " + pyvalue.StrRepr(report) + " was not restated: its message " + pyvalue.StrRepr(messageID) + " is staged and unsent with the first text, and the channel refused to stage the new one, so neither the message nor the report row is changed; report it again once it can be staged"
		}
		return ""
	}
	var replayed bool
	answer, err := c.StageDirect(ctx, message, func(ctx context.Context, staged map[string]any) error {
		var err error
		replayed, err = record(ctx, staged)
		return err
	})
	if err != nil {
		var reusedID reportIDReused
		if errors.As(err, &reusedID) {
			return nil, reportIDReusedExit(reusedID, kind, initiative)
		}
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
				if errors.As(err, &reusedID) {
					return nil, reportIDReusedExit(reusedID, kind, initiative)
				}
				return nil, err
			}
		}
		payload := contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: refused.Reason},
			{Key: "detail", Value: refused.Detail}, {Key: "schema", Value: reportUpSchema}, {Key: "reportId", Value: report}, {Key: "kind", Value: kind},
			{Key: "initiative", Value: initiative}, {Key: "replayed", Value: replayed}, {Key: "standing", Value: refused.Reason != "malformed_receipt"}}
		if notice := joinWarnings(warning, stands()); notice != "" {
			payload = append(payload, contract.Field{Key: "warning", Value: notice})
		}
		return nil, &dispatch.PayloadExit{Payload: payload, Code: contract.ExitRefused}
	}
	out := contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: reportUpSchema}, {Key: "reportId", Value: report}, {Key: "kind", Value: kind},
		{Key: "initiative", Value: initiative}, {Key: "replayed", Value: replayed}, {Key: "staged", Value: answer["staged"]},
		{Key: "messageId", Value: answer["messageId"]}, {Key: "sender", Value: answer["sender"]}, {Key: "recipient", Value: answer["recipient"]},
		{Key: "recipientScopeKind", Value: answer["recipientScopeKind"]}, {Key: "reportedBy", Value: nullableText(reportedBy)}}
	if notice := joinWarnings(warning, stands()); notice != "" {
		out = append(out, contract.Field{Key: "warning", Value: notice})
	}
	if reason, ok := answer["reason"].(string); ok {
		out = append(out, contract.Field{Key: "reason", Value: reason})
	}
	if readdressed, ok := answer["readdressed"]; ok {
		out = append(out, contract.Field{Key: "readdressed", Value: readdressed})
	}
	return out, nil
}

// reportIDReusedExit is the refusal of a report under a recorded id with another kind or initiative.
func reportIDReusedExit(e reportIDReused, kind, initiative string) error {
	return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "report_id_reused"},
		{Key: "detail", Value: e.Error()}, {Key: "schema", Value: reportUpSchema}, {Key: "reportId", Value: e.report}, {Key: "kind", Value: kind},
		{Key: "initiative", Value: initiative}, {Key: "recordedKind", Value: e.recordedKind}, {Key: "recordedInitiative", Value: e.recordedInitiative},
		{Key: "replayed", Value: false}, {Key: "standing", Value: true}}, Code: contract.ExitRefused}
}

// joinWarnings is the warnings that are not empty, joined.
func joinWarnings(warnings ...string) string {
	kept := []string{}
	for _, w := range warnings {
		if w != "" {
			kept = append(kept, w)
		}
	}
	return strings.Join(kept, "; ")
}

// anyStrings is values as a JSON array.
func anyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
