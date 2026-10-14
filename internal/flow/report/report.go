package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The results a delivery row carries (the result member of a report_delivery row).
const (
	ResultAccepted = "accepted"
	ResultQueued   = "queued"
	ResultRefused  = "refused"
	ResultUnknown  = "unknown"
)

// maxMessageBody is how much of a report body goes into the notice; the artifact holds the whole of it.
const maxMessageBody = 6000

// Writer makes the upward reports of a run: a report row first, the delivery after it, the delivery's result as a row that follows.
//
//	report row (kind report): report_id, report_kind, run, milestone, subject, body_digest, body_path, the recipient the linkage walk
//	  named (recipient_state, recipient_thread, recipient_initiative, recipient_reason), logical_id
//	delivery row (kind report_delivery): report_id, result (accepted, queued, refused, unknown), request_id, observation, reason
//	read row (kind report_read): report_id, turn
//
// A report row is written once for a report id, and a delivery is not made again once a row says it was accepted, queued or refused.
type Writer struct {
	Store    *store.Store
	Rows     Appender
	Resolver Resolver
	Sender   Sender
	// ArtifactDir holds the body of each report (<id>.md); empty keeps the body in the row's digest only.
	ArtifactDir string
}

// UpRequest is one report to send up.
type UpRequest struct {
	// ID is the report's id; empty derives it from the kind, run, milestone, subject and the digest of the body.
	ID   string
	Kind string
	Run  string
	// Milestone names the milestone a milestone_report is about; Subject is one line that says what the other kinds are about.
	Milestone, Subject string
	Body               []byte
	// Plan is the plan the run pinned; empty reads it from the run record.
	Plan string
}

// UpResult is what a report-up came to.
type UpResult struct {
	ReportID  string    `json:"report_id"`
	Kind      string    `json:"kind"`
	Run       string    `json:"run"`
	Recipient Recipient `json:"recipient"`
	// Replayed is set when the report row existed already.
	Replayed bool `json:"replayed"`
	// Delivery is the result of the delivery this call made or found: "" when the report went to no thread.
	Delivery string `json:"delivery,omitempty"`
	// Standing is set when no accepted delivery stands: the report waits where it is written.
	Standing    bool   `json:"standing"`
	RequestID   string `json:"request_id,omitempty"`
	Observation string `json:"observation,omitempty"`
	Reason      string `json:"reason,omitempty"`
	LogicalID   string `json:"logical_id,omitempty"`
	BodyDigest  string `json:"body_digest"`
}

var reportKinds = map[string]bool{KindMilestoneReport: true, KindProjectBlocked: true, KindProjectDecisionRequest: true, KindJunDecisionRequest: true}

// ReportID is the id a report of a run gets when the caller names none.
func ReportID(kind, run, milestone, subject, digest string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"crw-report", kind, run, milestone, subject, digest}, "\x00")))
	return "rep_" + hex.EncodeToString(sum[:])[:24]
}

// Digest is the digest of a report body.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LogicalID is the logical id the delivery of a report goes under: <run>:<milestone>:report for a milestone report, and for the other
// kinds the same shape with the kind and the head of the report id where the milestone is.
func LogicalID(kind, run, milestone, id string) string {
	if kind == KindMilestoneReport && milestone != "" {
		return run + ":" + milestone + ":report"
	}
	return run + ":" + kind + "-" + strings.TrimPrefix(id, "rep_")[:min(12, len(strings.TrimPrefix(id, "rep_")))] + ":report"
}

// Up writes the report row, then delivers.
func (w *Writer) Up(ctx context.Context, req UpRequest) (UpResult, error) {
	var res UpResult
	if !reportKinds[req.Kind] {
		return res, fmt.Errorf("report: %q is no report kind (milestone_report, project_blocked, project_decision_request, jun_decision_request)", req.Kind)
	}
	if req.Run == "" || len(req.Body) == 0 {
		return res, errors.New("report: a report names its run and has a body")
	}
	if w.Rows == nil || w.Store == nil || w.Resolver == nil {
		return res, errors.New("report: a store, a row writer and a resolver are required")
	}
	run, ok, err := ReadRun(ctx, w.Store.DB, req.Run)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, fmt.Errorf("report: the store holds no run %q", req.Run)
	}
	plan := firstText(req.Plan, run.PlanID)
	digest := Digest(req.Body)
	id := firstText(req.ID, ReportID(req.Kind, req.Run, req.Milestone, req.Subject, digest))
	res = UpResult{ReportID: id, Kind: req.Kind, Run: req.Run, BodyDigest: digest, LogicalID: LogicalID(req.Kind, req.Run, req.Milestone, id)}

	history, err := w.history(ctx, id)
	if err != nil {
		return res, err
	}
	if history.report != nil {
		res.Replayed = true
		res.Recipient = recipientOf(*history.report)
		// the delivery goes under the id the report row recorded: a replay never derives another
		res.LogicalID = firstText(history.report.Str("logical_id"), res.LogicalID)
	} else {
		rec, err := w.Resolver.Recipient(ctx, plan, req.Kind)
		if err != nil {
			return res, err
		}
		res.Recipient = rec
		if res.LogicalID, err = w.freeLogicalID(ctx, res.LogicalID, id); err != nil {
			return res, err
		}
		path, err := w.writeBody(id, req.Body)
		if err != nil {
			return res, err
		}
		payload := map[string]any{
			"report_id": id, "report_kind": req.Kind, "run": req.Run, "milestone": req.Milestone, "subject": req.Subject, "plan": plan,
			"body_digest": digest, "body_path": path, "logical_id": res.LogicalID,
			"recipient_state": rec.State, "recipient_thread": rec.Thread, "recipient_initiative": rec.InitiativeKey, "recipient_project": rec.ProjectKey,
			"recipient_reason": rec.Reason, "recipient_detail": rec.Detail,
		}
		// the report row first: whatever happens to the delivery, the report is on record
		if _, _, err := w.Rows.AppendOnce(ctx, NewRow{Kind: KindReport, Payload: payload}, "report_id"); err != nil {
			return res, err
		}
		history, err = w.history(ctx, id)
		if err != nil {
			return res, err
		}
	}
	res.Standing = true
	if last := history.lastDelivery(); last != nil {
		res.Delivery, res.RequestID, res.Observation, res.Reason = last.Str("result"), last.Str("request_id"), last.Str("observation"), last.Str("reason")
		res.Standing = res.Delivery != ResultAccepted
		if res.Delivery == ResultAccepted || res.Delivery == ResultQueued || res.Delivery == ResultRefused {
			// a report that was delivered is not sent again on a replay
			return res, nil
		}
	}
	deliverable := res.Recipient.Deliverable()
	if d, ok := w.Sender.(interface{ Delivers(Recipient) bool }); ok {
		// a sender over the supervisor channel delivers to the store seat too (CRW-1380, D1c d2)
		deliverable = d.Delivers(res.Recipient)
	}
	if !deliverable {
		return res, nil
	}
	if w.Sender == nil {
		return res, errors.New("report: a sender is required to deliver")
	}
	project := res.Recipient.ProjectKey
	if project == "" {
		project, _ = ProjectOf(ctx, w.Store, plan)
	}
	sent, err := w.Sender.SendSupervisor(ctx, SupervisorSend{Thread: res.Recipient.Thread, LogicalID: res.LogicalID, Text: message(req, res, digest), ReportID: id,
		Initiative: res.Recipient.InitiativeKey, Kind: req.Kind, Run: req.Run, Project: project, Subject: req.Subject})
	if err != nil {
		return res, err
	}
	if sent.Class == "" {
		// nothing was staged (the channel refused it, or a lead would hear a decision only Jun can make): no row, the report stands and a
		// replay stages it again
		res.Reason, res.Observation = sent.Reason, sent.Observation
		return res, nil
	}
	result := resultOf(sent.Class)
	if _, err := w.Rows.Append(ctx, NewRow{Kind: KindDelivery, Payload: map[string]any{
		"report_id": id, "result": result, "request_id": sent.RequestID, "observation": sent.Observation, "reason": sent.Reason, "logical_id": res.LogicalID,
	}}); err != nil {
		return res, err
	}
	res.Delivery, res.RequestID, res.Observation, res.Reason = result, sent.RequestID, sent.Observation, sent.Reason
	res.Standing = result != ResultAccepted
	return res, nil
}

// freeLogicalID is the logical id a new report is delivered under. The delivery layer keys a message by its logical id and refuses other
// text under an id it holds, so a report whose id differs from that of an earlier report on the same logical id (a milestone closed again
// over a tip that moved on) takes the head of its own id as a suffix. The first report of a milestone keeps <run>:<milestone>:report.
func (w *Writer) freeLogicalID(ctx context.Context, logicalID, id string) (string, error) {
	rows, err := ReadRows(ctx, w.Store.DB, "", KindReport)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Str("logical_id") == logicalID && r.Str("report_id") != id {
			short := strings.TrimPrefix(id, "rep_")
			return logicalID + "-" + short[:min(12, len(short))], nil
		}
	}
	return logicalID, nil
}

func resultOf(class string) string {
	switch class {
	case "accepted":
		return ResultAccepted
	case "queued":
		return ResultQueued
	case "refused":
		return ResultRefused
	}
	return ResultUnknown
}

func recipientOf(r Row) Recipient {
	return Recipient{State: r.Str("recipient_state"), Thread: r.Str("recipient_thread"), InitiativeKey: r.Str("recipient_initiative"), ProjectKey: r.Str("recipient_project"),
		Reason: r.Str("recipient_reason"), Detail: r.Str("recipient_detail")}
}

// message is the text of the notice: what the report is, where its body is, and the body (cut at maxMessageBody).
func message(req UpRequest, res UpResult, digest string) string {
	body := string(req.Body)
	cut := ""
	if len(body) > maxMessageBody {
		body, cut = strings.ToValidUTF8(body[:maxMessageBody], ""), "\n[cut: the whole body is the artifact of the report]"
	}
	head := fmt.Sprintf("[crw report-up] %s run=%s", req.Kind, req.Run)
	if req.Milestone != "" {
		head += " milestone=" + req.Milestone
	}
	if req.Subject != "" {
		head += " subject=" + req.Subject
	}
	reports := "crw flow reports --unread"
	switch {
	case res.Recipient.InitiativeKey != "":
		reports = "crw flow reports --initiative " + res.Recipient.InitiativeKey + " --unread"
	case res.Recipient.State == RecipientManagement && res.Recipient.ProjectKey != "":
		// A management session hears several projects of its own, and another session's projects are not its to read.
		reports = "crw flow reports --project " + res.Recipient.ProjectKey + " --unread"
	}
	return fmt.Sprintf("%s\nreport: %s (body %s)\nRead it with `%s` and acknowledge it with `crw flow report-ack %s --turn <your turn id>`.\n\n%s%s",
		head, res.ReportID, digest, reports, res.ReportID, body, cut)
}

func (w *Writer) writeBody(id string, body []byte) (string, error) {
	if w.ArtifactDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(w.ArtifactDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(w.ArtifactDir, id+".md")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// reportHistory is the rows of one report.
type reportHistory struct {
	report     *Row
	deliveries []Row
	reads      []Row
}

// lastDelivery is the delivery row that says what the report's delivery came to: the newest, unless it is a queued row after an
// accepted one (SupersedesDelivery).
func (h reportHistory) lastDelivery() *Row {
	var last *Row
	for i := range h.deliveries {
		if last == nil || SupersedesDelivery(last.Str("result"), h.deliveries[i].Str("result")) {
			last = &h.deliveries[i]
		}
	}
	return last
}

// SupersedesDelivery says whether a delivery row with result next, written after one with result prev, is what the report's delivery
// came to. A queued row is the writer's record of its staging, and the relay daemon may claim, send and settle the message (appending
// accepted) before the writer has appended it (CRW-1380): a queued row after an accepted one does not undo the acceptance. Any other
// later row stands for the earlier one.
func SupersedesDelivery(prev, next string) bool {
	return !(prev == ResultAccepted && next == ResultQueued)
}

func (w *Writer) history(ctx context.Context, id string) (reportHistory, error) {
	return ReportHistory(ctx, w.Store.DB, id)
}

// ReportHistory reads the report row of an id with the delivery and read rows that follow it, in the order they were written.
func ReportHistory(ctx context.Context, db DB, id string) (reportHistory, error) {
	var h reportHistory
	rows, err := ReadRows(ctx, db, "", KindReport, KindDelivery, KindRead)
	if err != nil {
		return h, err
	}
	for i := range rows {
		r := rows[i]
		if r.Str("report_id") != id {
			continue
		}
		switch r.Kind {
		case KindReport:
			if h.report == nil {
				h.report = &r
			}
		case KindDelivery:
			h.deliveries = append(h.deliveries, r)
		case KindRead:
			h.reads = append(h.reads, r)
		}
	}
	return h, nil
}

// Reporter is ops.Reporter over a Writer: the report of a milestone the run closed goes up.
type Reporter struct {
	W   *Writer
	Run string
}

var _ ops.Reporter = Reporter{}

// ReportUp implements ops.Reporter. The report's id is derived from the run, the milestone and the tip the close judged, so a replay of the
// operation finds its row; it is returned for the operation's own row.
func (r Reporter) ReportUp(ctx context.Context, rep ops.Report) (string, error) {
	body := milestoneBody(rep)
	id := "rep_" + shortHash(r.Run, rep.Milestone, rep.Range.Tip)
	res, err := r.W.Up(ctx, UpRequest{ID: id, Kind: KindMilestoneReport, Run: r.Run, Milestone: rep.Milestone, Plan: rep.Plan, Body: body})
	if err != nil {
		return "", err
	}
	return res.ReportID, nil
}

func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:24]
}

func milestoneBody(rep ops.Report) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Milestone %s of plan %s is closed.\n\nIntegrated range: %s..%s\n", rep.Milestone, rep.Plan, rep.Range.Base, rep.Range.Tip)
	fmt.Fprintf(&b, "\nFinal gate: %s\nFinal review: %s\nAudit: %s\n", compact(rep.Close.Gate), compact(rep.Close.Final), compact(rep.Close.Audit))
	return []byte(b.String())
}

// compact is a piece of the close's evidence on one line; a piece the close did not make is "none".
func compact(raw []byte) string {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "none"
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// ---- reading, acknowledging and settling ----

// ReportView is a report with what happened to it.
type ReportView struct {
	ID, Kind, Run, Milestone, Subject, RecordedAt string
	BodyDigest, BodyPath                          string
	Recipient                                     Recipient
	// LastDelivery is the result of the newest delivery row (a queued row after an accepted one aside, SupersedesDelivery), "" when
	// there is none.
	LastDelivery string
	RequestID    string
	Read         bool
	ReadTurn     string
	// Standing: no accepted delivery stands.
	Standing bool
}

// Reports lists the reports of the store, oldest first: those of one initiative when initiative is given, the unread ones when unread.
func Reports(ctx context.Context, db DB, initiative string, unread bool) ([]ReportView, error) {
	return ReportsMatching(ctx, db, ReportFilter{Initiative: initiative, Unread: unread})
}

// ReportFilter narrows a listing of reports: to the reports addressed for one initiative, to those of one project (the listing a
// management session reads, so two management sessions do not read each other's reports), to the unread ones. An empty field does not
// narrow.
type ReportFilter struct {
	Initiative, Project string
	Unread              bool
}

// ReportsMatching lists the reports of the store the filter keeps, oldest first.
func ReportsMatching(ctx context.Context, db DB, f ReportFilter) ([]ReportView, error) {
	rows, err := ReadRows(ctx, db, "", KindReport, KindDelivery, KindRead)
	if err != nil {
		return nil, err
	}
	byID := map[string]*ReportView{}
	var order []string
	for _, r := range rows {
		id := r.Str("report_id")
		switch r.Kind {
		case KindReport:
			if byID[id] != nil {
				continue
			}
			v := &ReportView{ID: id, Kind: r.Str("report_kind"), Run: r.Str("run"), Milestone: r.Str("milestone"), Subject: r.Str("subject"), RecordedAt: r.RecordedAt,
				BodyDigest: r.Str("body_digest"), BodyPath: r.Str("body_path"), Recipient: recipientOf(r), Standing: true}
			byID[id] = v
			order = append(order, id)
		case KindDelivery:
			if v := byID[id]; v != nil && SupersedesDelivery(v.LastDelivery, r.Str("result")) {
				v.LastDelivery, v.RequestID = r.Str("result"), r.Str("request_id")
				v.Standing = v.LastDelivery != ResultAccepted
			}
		case KindRead:
			if v := byID[id]; v != nil {
				v.Read, v.ReadTurn = true, r.Str("turn")
			}
		}
	}
	var out []ReportView
	for _, id := range order {
		v := byID[id]
		if f.Initiative != "" && v.Recipient.InitiativeKey != f.Initiative {
			continue
		}
		if f.Project != "" && v.Recipient.ProjectKey != f.Project {
			continue
		}
		if f.Unread && v.Read {
			continue
		}
		out = append(out, *v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RecordedAt < out[j].RecordedAt })
	return out, nil
}

// AppenderFor gives the writer of the rows of a run: a delivery or a reading is a row of the run that wrote the report.
type AppenderFor func(run string) Appender

// Ack appends the read row of a report: the initiative lead has read it in the turn. The report must exist; a second ack of the same
// report in the same turn is the same row.
func Ack(ctx context.Context, db DB, rows AppenderFor, id, turn string) (written bool, err error) {
	if strings.TrimSpace(turn) == "" {
		return false, errors.New("report: --turn is the lead's own turn id")
	}
	h, err := ReportHistory(ctx, db, id)
	if err != nil {
		return false, err
	}
	if h.report == nil {
		return false, fmt.Errorf("report: the store holds no report %q", id)
	}
	for _, r := range h.reads {
		if r.Str("turn") == turn {
			return false, nil
		}
	}
	_, err = rows(h.report.Run).Append(ctx, NewRow{Kind: KindRead, Payload: map[string]any{"report_id": id, "turn": turn}})
	return err == nil, err
}

// Delivered appends a delivery row to a report: the result a delivery the pump made came to. Only the flow command writes it.
func Delivered(ctx context.Context, db DB, rows AppenderFor, id, result, requestID, observation string) (written bool, err error) {
	switch result {
	case ResultAccepted, ResultQueued, ResultRefused, ResultUnknown:
	default:
		return false, fmt.Errorf("report: %q is no delivery result", result)
	}
	h, err := ReportHistory(ctx, db, id)
	if err != nil {
		return false, err
	}
	if h.report == nil {
		return false, fmt.Errorf("report: the store holds no report %q", id)
	}
	for _, d := range h.deliveries {
		if d.Str("result") == result && d.Str("request_id") == requestID {
			return false, nil
		}
	}
	_, err = rows(h.report.Run).Append(ctx, NewRow{Kind: KindDelivery, Payload: map[string]any{"report_id": id, "result": result, "request_id": requestID, "observation": observation}})
	return err == nil, err
}
