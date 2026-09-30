package delivery

// omitted.py was assigned to todo 21 and is carried here for todo 24's reporting
// observer and supervisor omission contracts. This file owns only read-side diagnosis.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	OmittedSchema              = "reporting-observation/1"
	OmittedStoreSource         = "relay_store"
	OmittedMaxRecords          = 128
	OmittedMaxFacts            = 512
	OmittedMaxBytes            = 1024 * 1024
	OmittedReason              = "terminal_without_report"
	OmittedNotOwed             = "not_an_omission"
	OmittedLaterTurn           = "later_turn_admitted"
	OmittedReceipted           = "turn_receipted"
	OmittedWithinGrace         = "within_report_grace"
	OmittedGraceUnmeasured     = "report_grace_unmeasured"
	OmittedDeclarationsMissing = "declarations_not_recorded"
)

var omissionLabels = map[string]bool{"managed_unregistered": true, "receipt_missing": true, "undeclared_turn_end": true}

type OmissionFacts struct {
	Witness                                   *bool
	Admission                                 string
	Settlements                               []OmissionSettlement
	Label                                     string
	ExecutionReport, Receipted, LaterAdmitted bool
	Now                                       string
	Grace                                     float64
}
type OmissionSettlement struct {
	Status string `json:"status"`
	At     string `json:"at"`
}

func omissionAnswer(state, reason, notOwed string) Obj {
	owed := state == "unreported" && notOwed == ""
	owedReason := OmittedReason
	if !owed {
		owedReason = notOwed
		if owedReason == "" {
			owedReason = OmittedNotOwed
		}
	}
	return Obj{{Key: "reportingState", Value: state}, {Key: "reason", Value: reason}, {Key: "owed", Value: owed}, {Key: "owedReason", Value: owedReason}}
}

// ClassifyOmission is omitted.classify, the one pure predicate shared by both readers.
func ClassifyOmission(f OmissionFacts) Obj {
	if f.Witness == nil {
		return omissionAnswer("unmeasured", "stop_unobserved", "")
	}
	if f.Admission != "admitted" {
		reason := "admission_unrecorded"
		if f.Admission == "bootstrap" {
			reason = "bootstrap"
		}
		return omissionAnswer("unmeasured", reason, "")
	}
	terminal, ok := omissionTerminal(f.Settlements)
	if !ok {
		return omissionAnswer("unmeasured", "terminal_conflict", "")
	}
	if f.Label == "declared_in_progress" {
		return omissionAnswer("in_progress", f.Label, "")
	}
	if strings.HasPrefix(f.Label, "declared_") {
		return omissionAnswer("reported", f.Label, "")
	}
	if (terminal == "failed" || terminal == "interrupted") && f.ExecutionReport {
		return omissionAnswer("reported", "daemon_execution_report", "")
	}
	if terminal == "unobserved" {
		return omissionAnswer("unmeasured", "host_terminal_unobserved", "")
	}
	if !*f.Witness || !omissionLabels[f.Label] {
		return omissionAnswer("unmeasured", "no_confirmed_omission", "")
	}
	if f.Receipted {
		return omissionAnswer("unreported", OmittedReason, OmittedReceipted)
	}
	if f.LaterAdmitted {
		return omissionAnswer("unreported", OmittedReason, OmittedLaterTurn)
	}
	if f.Grace > 0 {
		now := Moment(f.Now)
		var latest *time.Time
		for _, s := range f.Settlements {
			at := Moment(s.At)
			if now == nil || at == nil {
				return omissionAnswer("unreported", OmittedReason, OmittedGraceUnmeasured)
			}
			if latest == nil || at.After(*latest) {
				latest = at
			}
		}
		if latest == nil {
			return omissionAnswer("unreported", OmittedReason, OmittedGraceUnmeasured)
		}
		if now.Sub(*latest).Seconds() < f.Grace {
			return omissionAnswer("unreported", OmittedReason, OmittedWithinGrace)
		}
	}
	return omissionAnswer("unreported", OmittedReason, "")
}
func omissionTerminal(rows []OmissionSettlement) (string, bool) {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Status] = true
	}
	if len(seen) > 1 {
		return "", false
	}
	for status := range seen {
		if status != "completed" && status != "failed" && status != "interrupted" {
			return "", false
		}
		return status, true
	}
	return "unobserved", true
}

func omissionBase(now string) Obj {
	return Obj{{Key: "schema", Value: OmittedSchema}, {Key: "reportingState", Value: "unmeasured"}, {Key: "reason", Value: nil}, {Key: "observedAt", Value: now}}
}
func appendAnswer(o Obj, a Obj) Obj {
	for _, f := range a {
		o = set(o, f.Key, f.Value)
	}
	return o
}
func omissionUnmeasured(o Obj, reason string) Obj {
	return appendAnswer(o, omissionAnswer("unmeasured", reason, ""))
}

// OmissionStop is one bounded Stop record.
type OmissionStop struct {
	Sequence int
	Path     string
	Record   Obj
}

func omissionStops(directory, session, turn string) ([]OmissionStop, error) {
	folder := filepath.Join(directory, "hook", session, turn)
	paths, readable := listing(folder, "*.json", "")
	if !readable {
		return nil, fmt.Errorf("stop_unreadable")
	}
	out := []OmissionStop{}
	seen := map[int]bool{}
	for _, path := range paths {
		name := filepath.Base(path)
		if name == "hold.json" || strings.HasPrefix(name, ".") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		n, err := strconv.Atoi(stem)
		if err != nil || len(out) >= OmittedMaxRecords {
			return nil, fmt.Errorf("stop_history_invalid")
		}
		if seen[n] {
			return nil, fmt.Errorf("stop_sequence_ambiguous")
		}
		seen[n] = true
		if _, err = confined(path, directory); err != nil {
			return nil, err
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		raw := make([]byte, OmittedMaxBytes+1)
		read, readErr := io.ReadFull(file, raw)
		_ = file.Close()
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, readErr
		}
		if read > OmittedMaxBytes {
			return nil, fmt.Errorf("stop_record_limit")
		}
		if message := store.PythonJSONError(string(raw[:read])); message != "" {
			return nil, errors.New(message)
		}
		value, err := loads(string(raw[:read]))
		if err != nil {
			return nil, err
		}
		record, ok := value.(Obj)
		if !ok || fieldOf(record, "sessionId") != session || fieldOf(record, "turnId") != turn || !Named(fieldOf(record, "observation")) || !Named(fieldOf(record, "decisionState")) || Moment(fieldOf(record, "at")) == nil {
			return nil, fmt.Errorf("stop_identity_or_shape")
		}
		out = append(out, OmissionStop{n, path, record})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}
func omissionConfinedFacts(directory, root, session, turn string) error {
	count := 0
	checked := func(path string) error {
		if _, err := confined(path, root); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("marker_symlink")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("marker_not_regular")
		}
		if info.Mode().IsRegular() && info.Size() > OmittedMaxBytes {
			return fmt.Errorf("marker_record_limit")
		}
		return nil
	}
	if err := checked(directory); err != nil {
		return err
	}
	for _, fact := range singleFacts {
		if err := checked(filepath.Join(directory, fact.name)); err != nil {
			return err
		}
	}
	if err := checked(filepath.Join(directory, "dispositions", session, turn+".json")); err != nil {
		return err
	}
	folders := []string{"attempts", "conflicts", "resolutions"}
	for _, name := range folders {
		paths, ok := listing(filepath.Join(directory, name), "*.json", "")
		if !ok {
			return fmt.Errorf("marker_listing_unreadable")
		}
		for _, p := range paths {
			if strings.HasPrefix(filepath.Base(p), ".") {
				continue
			}
			count++
			if count > OmittedMaxFacts {
				return fmt.Errorf("marker_history_limit")
			}
			if err := checked(p); err != nil {
				return err
			}
		}
	}
	claims, ok := listing(filepath.Join(directory, "claims"), "", "directories")
	if !ok {
		return fmt.Errorf("marker_listing_unreadable")
	}
	for _, p := range claims {
		count++
		if count > OmittedMaxFacts {
			return fmt.Errorf("marker_history_limit")
		}
		if err := checked(filepath.Join(p, claimFile)); err != nil {
			return err
		}
	}
	hooks, ok := listing(filepath.Join(directory, "hook", session, turn), "*.json", "")
	if !ok {
		return fmt.Errorf("marker_listing_unreadable")
	}
	for _, p := range hooks {
		if filepath.Base(p) == "hold.json" {
			continue
		}
		count++
		if count > OmittedMaxFacts {
			return fmt.Errorf("marker_history_limit")
		}
		if err := checked(p); err != nil {
			return err
		}
	}
	return nil
}

type omissionQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type omissionContext struct {
	Relationship, Issue, Status, Parent, Child string
	ChildCwd, Superseded, AnchorState          sql.NullString
	Generation, Opened                         int64
	Dispatch, DispatchTurn                     string
	Settlements                                []OmissionSettlement
	Events                                     []map[string]any
	Managed                                    []any
	Admitted                                   []admissionFact
	OwnAdmission                               *admissionFact
}
type admissionFact struct {
	Turn string `json:"turn"`
	At   string `json:"at"`
	Row  int64  `json:"row"`
}

// All registry, terminal and admission facts come from ONE SQLite statement. Selecting the
// dispatch, not the current ordinal, preserves the evidence needed to diagnose a stale marker.
const omissionContextSQL = `SELECT r.relationship_id,r.issue_key,r.status,r.parent_task_id,
 r.child_task_id,r.child_cwd,r.execution_generation,r.superseded_by,g.execution_generation,
 g.dispatch_request_id,COALESCE(g.dispatch_turn_id,''),g.anchor_state,
 (SELECT json_group_array(json_object('status',s.terminal_status,'at',s.settled_at))
  FROM assignment_settlements s WHERE s.relationship_id=r.relationship_id AND s.thread_id=? AND s.turn_id=?),
 (SELECT json_group_array(json_object('eventId',e.event_id,'outcome',e.outcome,'stage',e.stage,
  'producer',e.producer,'status',e.turn_status)) FROM events e WHERE e.relationship_id=r.relationship_id
  AND e.execution_generation=g.execution_generation AND e.turn_thread_id=? AND e.turn_id=?),
 (SELECT json_group_array(json_object('requestId',m.request_id,'state',m.state,'child',m.child_task_id,
  'standby',m.standby_turn_id,'relationship',m.relationship_id,'generation',m.execution_generation,
  'workspace',m.workspace,'markerRoot',m.marker_root,'issue',m.issue_key))
  FROM managed_start_requests m WHERE m.dispatch_request_id=g.dispatch_request_id),
 (SELECT json_group_array(json_object('turn',t.turn_id,'at',t.admitted_at,'row',t.rowid))
  FROM generation_turns t WHERE t.relationship_id=r.relationship_id AND t.execution_generation=g.execution_generation
  AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id <> ''
  AND t.evidence = ('explicit_admission_bound:' || g.dispatch_turn_id)),
 (SELECT json_object('at',a.admitted_at,'row',a.rowid) FROM generation_turns a
  WHERE a.relationship_id=r.relationship_id AND a.execution_generation=g.execution_generation AND a.turn_id=?)
 FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id
 WHERE r.relationship_id=? AND g.dispatch_request_id=?`

func omissionContextArgs(rid, dispatch, session, turn string) []any {
	return []any{session, turn, session, turn, turn, rid, dispatch}
}
func scanOmissionContext(row store.RowScanner) (omissionContext, error) {
	var c omissionContext
	var settlements, events, managed, admitted string
	var own sql.NullString
	err := row.Scan(&c.Relationship, &c.Issue, &c.Status, &c.Parent, &c.Child, &c.ChildCwd,
		&c.Generation, &c.Superseded, &c.Opened, &c.Dispatch, &c.DispatchTurn, &c.AnchorState,
		&settlements, &events, &managed, &admitted, &own)
	if err != nil {
		return c, err
	}
	for _, item := range []struct {
		text   string
		target any
	}{
		{settlements, &c.Settlements}, {events, &c.Events}, {managed, &c.Managed}, {admitted, &c.Admitted},
	} {
		if err := json.Unmarshal([]byte(item.text), item.target); err != nil {
			return c, err
		}
	}
	if own.Valid {
		if err := json.Unmarshal([]byte(own.String), &c.OwnAdmission); err != nil {
			return c, err
		}
	}
	return c, nil
}
func readOmissionContext(ctx context.Context, s omissionQuerier, rid, session, turn, dispatch string) (omissionContext, error) {
	rows, err := s.QueryContext(ctx, omissionContextSQL, omissionContextArgs(rid, dispatch, session, turn)...)
	if err != nil {
		return omissionContext{}, err
	}
	defer rows.Close()
	var contexts []omissionContext
	for rows.Next() {
		c, err := scanOmissionContext(rows)
		if err != nil {
			return c, err
		}
		contexts = append(contexts, c)
	}
	if err := rows.Err(); err != nil {
		return omissionContext{}, err
	}
	if len(contexts) != 1 {
		return omissionContext{}, sql.ErrNoRows
	}
	return contexts[0], nil
}
func observeOmissionContext(ctx context.Context, selection store.StateSelection, rid, dispatch, session, turn string) (omissionContext, store.RowsRead, error) {
	var contexts []omissionContext
	reading := store.ReadOnlyRows(ctx, selection, omissionContextSQL, omissionContextArgs(rid, dispatch, session, turn), func(row store.RowScanner) error {
		c, err := scanOmissionContext(row)
		contexts = append(contexts, c)
		return err
	})
	if reading.Raised != nil {
		return omissionContext{}, reading, reading.Raised
	}
	if !reading.Readable || reading.Detail != "" {
		return omissionContext{}, reading, errors.New("store_unreadable: " + reading.Detail)
	}
	if len(contexts) != 1 {
		return omissionContext{}, reading, errors.New("registration_unresolved")
	}
	return contexts[0], reading, nil
}

func omissionAdmission(c omissionContext, root, workspace, session, turn string) (string, error) {
	admission, ok := admissionFor(c, turn)
	if !ok {
		return "", errors.New("stale_generation")
	}
	if len(c.Managed) > 1 {
		return "", errors.New("managed_request_ambiguous")
	}
	if len(c.Managed) == 1 {
		r := c.Managed[0].(map[string]any)
		work, err := resolvedPath(r["workspace"].(string))
		if err != nil {
			return "", err
		}
		markerRoot, err := resolvedPath(r["markerRoot"].(string))
		if err != nil {
			return "", err
		}
		if r["child"] != session || r["relationship"] != c.Relationship || r["generation"] != float64(c.Opened) || r["issue"] != c.Issue || work != workspace || markerRoot != root {
			return "", errors.New("managed_request_identity_mismatch")
		}
		if r["standby"] == turn {
			return "bootstrap", nil
		}
	}
	return admission, nil
}
func admissionFor(c omissionContext, turn string) (string, bool) {
	if c.Generation != c.Opened || c.Superseded.Valid {
		return "", false
	}
	if turn == c.DispatchTurn {
		return "admitted", true
	}
	for _, a := range c.Admitted {
		if a.Turn == turn {
			return "admitted", true
		}
	}
	return "unadmitted", true
}
func laterAdmission(c omissionContext, turn string) bool {
	own := c.OwnAdmission
	for i := range c.Admitted {
		if c.Admitted[i].Turn == turn {
			continue
		}
		if own == nil || admissionLess(*own, c.Admitted[i]) {
			return true
		}
	}
	return false
}
func admissionLess(a, b admissionFact) bool {
	at, bt := Moment(a.At), Moment(b.At)
	if at == nil {
		return false
	}
	if bt == nil {
		return true
	}
	if at.Equal(*bt) {
		return a.Row < b.Row
	}
	return at.Before(*bt)
}
func factsFromContext(c omissionContext, turn string, witness *bool, admission, label, now string, grace float64) OmissionFacts {
	terminal, _ := omissionTerminal(c.Settlements)
	execution := false
	receipted := false
	for _, e := range c.Events {
		if e["producer"] == "daemon_observation" && e["stage"] == "final" && e["outcome"] == terminal && e["status"] == terminal {
			execution = true
		}
		if e["producer"] == "child" && e["stage"] == "final" {
			receipted = true
		}
	}
	return OmissionFacts{witness, admission, c.Settlements, label, execution, receipted, laterAdmission(c, turn), now, grace}
}
func declarationLabel(disposition Obj, session, turn string, receipted bool) string {
	if fieldOf(disposition, "sessionId") != session || fieldOf(disposition, "turnId") != turn {
		return "undeclared_turn_end"
	}
	outcome, _ := fieldOf(disposition, "outcome").(string)
	switch outcome {
	case "in_progress", "blocked_needs_input", "failed", "interrupted":
		return "declared_" + outcome
	case "ready_for_review":
		if receipted {
			return "declared_ready_receipted"
		}
		return "receipt_missing"
	}
	return "undeclared_turn_end"
}
func omissionDetail(label string, receipt Obj) string {
	if label == "undeclared_turn_end" {
		return "No usable turn disposition was recorded for this turn. Record in_progress, blocked_needs_input, interrupted, failed, or ready_for_review with a receipt."
	}
	if label != "receipt_missing" {
		return "The child declared this turn."
	}
	detail := str(receipt, "detail")
	switch str(receipt, "evidence") {
	case "generation_absent":
		return "The relay's store holds no record of the generation it reports as current for this relationship: " + detail + ". Nothing can be attributed to this assignment while the store cannot say which dispatch opened the generation it is on, and no receipt this session emits changes that. The relay's store is what needs repair."
	case "registration_generation_mismatch", "generation_dispatch_mismatch":
		return "This assignment's registration does not name the generation the relay is on: " + detail + ". No receipt this session emits can satisfy it, because the registration fact is create-once and cannot be republished onto the live generation, and a receipt earned there belongs to work this assignment never registered. Recovery is a new assignment, declared for a fresh dispatch request id."
	}
	return "Readiness is declared but no receipt stands at the current head revision for this session and turn. Emit the receipt over the actual artifacts."
}

// omissionReceipt replays guard.lookup_receipt: the marker's generation stamp and claimed
// dispatch constrain the current head, staged receipts count, and the artifact bytes must match.
func omissionReceipt(ctx context.Context, path, rid, session, turn string, generation any, dispatch string) (Obj, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &store.Store{DB: db, Path: path}
	base := Obj{{Key: "relationshipId", Value: rid}, {Key: "sessionId", Value: session}, {Key: "turnId", Value: turn}, {Key: "atCurrentHead", Value: false}}
	var relationship, event Row
	// Release the SQLite snapshot before hashing any files, just as the Python guard does.
	err = func() (err error) {
		if _, err = db.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
			return err
		}
		defer func() {
			_, rollbackErr := db.ExecContext(ctx, "ROLLBACK")
			err = errors.Join(err, rollbackErr)
		}()
		relationship, err = one(ctx, s, "SELECT status,superseded_by,execution_generation,artifact_roots FROM relationships WHERE relationship_id=?", rid)
		if err != nil {
			return err
		}
		if relationship == nil {
			base = set(base, "evidence", "relationship_absent")
			return nil
		}
		if relationship.S("status") != "active" || truthy(relationship.Opt("superseded_by")) {
			base = set(base, "evidence", "relationship_not_active")
			return nil
		}
		current := relationship.I("execution_generation")
		if generation != nil && fmt.Sprint(generation) != strconv.FormatInt(current, 10) {
			base = set(base, "evidence", "registration_generation_mismatch")
			base = set(base, "detail", fmt.Sprintf("the assignment registered generation %v and the relationship now stands on generation %d", generation, current))
			return nil
		}
		opened, err := one(ctx, s, "SELECT dispatch_request_id FROM generations WHERE relationship_id=? AND execution_generation=?", rid, current)
		if err != nil {
			return err
		}
		if opened == nil {
			base = set(base, "evidence", "generation_absent")
			base = set(base, "detail", fmt.Sprintf("the relationship reports generation %d and the store holds no record of which dispatch opened it", current))
			return nil
		}
		if opened.S("dispatch_request_id") != dispatch {
			base = set(base, "evidence", "generation_dispatch_mismatch")
			base = set(base, "detail", fmt.Sprintf("the relationship stands on generation %d, which a different dispatch request opened", current))
			return nil
		}
		head, err := HeadRevision(ctx, s, rid, current)
		if err != nil {
			return err
		}
		if slices.Contains(ambiguousEvidence, str(head, "evidence")) {
			base = set(base, "evidence", "head_"+str(head, "evidence"))
			return nil
		}
		if str(head, "eventId") == "" {
			base = set(base, "evidence", "no_reviewable_revision")
			return nil
		}
		event, err = one(ctx, s, "SELECT event_id,stage,revision_hash,turn_thread_id,turn_id,producer,receipt,manifest_ref FROM events WHERE event_id=?", str(head, "eventId"))
		return err
	}()
	if err != nil {
		return nil, err
	}
	if str(base, "evidence") != "" {
		return base, nil
	}
	if event == nil {
		return set(base, "evidence", "no_reviewable_revision"), nil
	}
	if event.S("producer") != "child" || !slices.Contains([]string{"staged", "final"}, event.S("stage")) {
		return set(base, "evidence", "head_is_not_a_child_receipt"), nil
	}
	if event.S("turn_thread_id") != session || event.S("turn_id") != turn {
		return set(base, "evidence", "head_belongs_to_another_turn"), nil
	}
	var roots []string
	var payload struct {
		Manifest []store.ManifestEntry `json:"manifest"`
		Revision string                `json:"revisionHash"`
	}
	for _, item := range []struct {
		text   string
		target any
	}{
		{relationship.S("artifact_roots"), &roots}, {event.S("receipt"), &payload},
	} {
		if err := json.Unmarshal([]byte(item.text), item.target); err != nil {
			base = set(base, "evidence", "stored_receipt_unreadable")
			base = set(base, "detail", "JSONDecodeError: "+store.PythonJSONError(item.text))
			return set(base, "eventId", event.S("event_id")), nil
		}
	}
	binding, detail, err := omissionDeliverable(payload.Manifest, payload.Revision, event.S("manifest_ref"), roots)
	if err != nil {
		return nil, err
	}
	if binding == "" {
		base = set(base, "evidence", "artifacts_changed_since_receipt")
		base = set(base, "detail", detail)
		base = set(base, "eventId", event.S("event_id"))
		return set(base, "revisionHash", event.S("revision_hash")), nil
	}
	base = set(base, "atCurrentHead", true)
	base = set(base, "evidence", "at_head")
	base = set(base, "eventId", event.S("event_id"))
	base = set(base, "revisionHash", event.S("revision_hash"))
	base = set(base, "stage", event.S("stage"))
	return set(base, "deliverableBinding", binding), nil
}
func omissionDeliverable(entries []store.ManifestEntry, revision, reference string, roots []string) (string, string, error) {
	if len(entries) == 0 {
		return "", "the stored receipt carries no manifest to verify", nil
	}
	digest, err := store.ManifestRevision(entries)
	if err != nil {
		return "", "ValueError: " + err.Error(), nil
	}
	if revision == "" {
		return "", "the stored receipt names no revision", nil
	}
	if digest != revision {
		return "", "the stored manifest hashes to " + digest + " but the receipt claims " + revision, nil
	}
	// guard.deliverable_state from here: a read that could not happen, of the live bytes or of the
	// frozen copy that answers for them, is never reported as a revision that changed (the error,
	// which the omission reads as receipt_unreadable), and a changed one never as unreadable.
	problems, _, unreachableLive := store.VerifyAgainstDiskDetailed(entries, roots, false)
	if len(problems) == 0 {
		return "live", "", nil
	}
	if reference != "" {
		_, frozen, unreachable, err := store.VerifyFrozenDetailed(reference, entries)
		if err != nil {
			// The fence raises here. An OSError or a ScopeError is a comparison that did not
			// happen, and a RecursionError leaves deliverable_state (omissionReceiptFailure names
			// both); anything else read the frozen copy and found no manifest in it.
			var exception *store.ManifestException
			if errors.As(err, &exception) && !exception.OSError() && !exception.RuntimeError() {
				return "", exception.PythonText(), nil
			}
			return "", "", err
		}
		if len(frozen) == 0 {
			return "frozen", "", nil
		}
		if len(unreachable) > 0 {
			return "", "", errors.New(strings.Join(unreachable[:min(3, len(unreachable))], "; "))
		}
		problems = append(problems, frozen...)
	}
	if len(unreachableLive) > 0 {
		return "", "", errors.New(strings.Join(unreachableLive[:min(3, len(unreachableLive))], "; "))
	}
	return "", strings.Join(problems[:min(3, len(problems))], "; "), nil
}

// omissionReceiptFailure is the reason a receipt lookup that failed leaves the omission
// unmeasured with. A receipt nobody could read, or a deliverable nobody could compare, is
// lookup_receipt's readable False (receipt_unreadable). The RecursionError of a frozen copy nested
// past json.loads's depth leaves lookup_receipt instead, and observe and derive catch it as a
// RuntimeError: evidence_unreadable with its words.
func omissionReceiptFailure(err error) string {
	var exception *store.ManifestException
	if errors.As(err, &exception) && exception.RuntimeError() {
		return "evidence_unreadable: " + exception.Error()
	}
	return "receipt_unreadable"
}

func resolvedPath(value string) (string, error) {
	expanded, err := store.ExpandUser(value)
	if err != nil {
		return "", err
	}
	return store.ResolvePath(expanded)
}

// ObserveOmission reads exact marker and store evidence without writing either.
func ObserveOmission(ctx context.Context, selection store.StateSelection, root, workspace, assignment, session, turn, now string, grace float64) Obj {
	return observeOmission(ctx, selection, root, workspace, assignment, session, turn, now, grace, nil)
}

// afterCurrent is the deterministic race boundary: tests commit the captured competing write
// after reading the declaration and before the registry recheck. Production passes nil.
func observeOmission(ctx context.Context, selection store.StateSelection, root, workspace, assignment, session, turn, now string, grace float64, afterCurrent func()) Obj {
	result := append(omissionBase(now), F{Key: "selectors", Value: Obj{{Key: "state", Value: selection.Path}, {Key: "markerRoot", Value: root}, {Key: "workspace", Value: workspace}, {Key: "assignment", Value: assignment}, {Key: "session", Value: session}, {Key: "turn", Value: turn}}}, F{Key: "stopObservation", Value: nil}, F{Key: "terminalObservation", Value: Obj{{Key: "source", Value: "relay_settlement"}, {Key: "status", Value: "unobserved"}}}, F{Key: "currentObservation", Value: nil}, F{Key: "turnAdmission", Value: "unmeasured"}, F{Key: "relationshipStatus", Value: nil}, F{Key: "owed", Value: false}, F{Key: "owedReason", Value: OmittedNotOwed})
	fail := func(err error) Obj {
		reason := strings.TrimPrefix(pythonStr(err), "ValueError: ")
		if strings.ContainsAny(reason, " /:") || !strings.Contains(reason, "_") {
			reason = "evidence_unreadable: " + reason
		}
		return omissionUnmeasured(result, reason)
	}
	rootPath, err := resolvedPath(root)
	if err != nil {
		return fail(err)
	}
	workPath, err := resolvedPath(workspace)
	if err != nil {
		return fail(err)
	}
	directory, err := AssignmentDir(rootPath, workPath, assignment)
	if err != nil {
		return fail(err)
	}
	if err = omissionConfinedFacts(directory, rootPath, session, turn); err != nil {
		return fail(err)
	}
	marker, unreadable := ReadAssignment(directory)
	if len(unreadable) > 0 {
		return omissionUnmeasured(result, "marker_unreadable")
	}
	intent := markerFact(marker, "intent")
	if len(intent) == 0 {
		return appendAnswer(result, omissionAnswer("unmanaged", "marker_absent", ""))
	}
	if malformed := Malformed(marker); malformed != "" {
		return omissionUnmeasured(result, "marker_malformed")
	}
	if !Correlated(marker, session, assignment) {
		return omissionUnmeasured(result, "dispatch_uncorrelated")
	}
	bound := markerFact(marker, "bound")
	if fieldOf(bound, "sessionId") != session {
		return omissionUnmeasured(result, "session_unbound_or_foreign")
	}
	declaredWorkValue := fieldOf(intent, "workspace")
	declaredWork, isPath := declaredWorkValue.(string)
	if truthy(declaredWorkValue) && !isPath {
		return fail(fmt.Errorf("argument should be a str or an os.PathLike object where __fspath__ returns a str, not '%s'", pyTypeName(declaredWorkValue)))
	}
	declaredDB, _ := fieldOf(intent, "dbPath").(string)
	dw, _ := resolvedPath(declaredWork)
	dd, _ := resolvedPath(declaredDB)
	db, _ := resolvedPath(selection.DBPath())
	if fieldOf(intent, "dispatchRequestIdHash") != assignment || dw != workPath || dd != db {
		return omissionUnmeasured(result, "marker_selector_mismatch")
	}
	stops, err := omissionStops(directory, session, turn)
	if err != nil {
		return fail(err)
	}
	if len(stops) > 0 {
		last := stops[len(stops)-1]
		result = set(result, "stopObservation", Obj{{Key: "sequence", Value: int64(last.Sequence)}, {Key: "path", Value: last.Path}, {Key: "record", Value: last.Record}})
		result = set(result, "stopRecordCount", int64(len(stops)))
	}
	rid, _ := fieldOf(markerFact(marker, "relationship"), "relationshipId").(string)
	if rid == "" {
		return omissionUnmeasured(result, "registration_unresolved")
	}
	var dispatch string
	for _, raw := range markerFactList(marker, "claims") {
		claim, _ := raw.(Obj)
		if Claimant(claim) == session && AssignmentID(fmt.Sprint(fieldOf(claim, "dispatchRequestId"))) == assignment {
			dispatch, _ = fieldOf(claim, "dispatchRequestId").(string)
			break
		}
	}
	if dispatch == "" {
		return omissionUnmeasured(result, "registration_unresolved")
	}
	c, snapshot, err := observeOmissionContext(ctx, selection, rid, dispatch, session, turn)
	if err != nil {
		return omissionUnmeasured(result, err.Error())
	}
	result = set(result, "relationshipId", rid)
	result = set(result, "relationshipStatus", c.Status)
	result = set(result, "executionGeneration", c.Opened)
	result = set(result, "parentTaskId", c.Parent)
	if c.Child != session || fieldOf(bound, "taskId") != session || c.Issue != fmt.Sprint(fieldOf(intent, "issueKey")) || !c.ChildCwd.Valid {
		return omissionUnmeasured(result, "registry_identity_mismatch")
	}
	cwd, _ := resolvedPath(c.ChildCwd.String)
	if cwd != workPath || AssignmentID(c.Dispatch) != assignment || c.Dispatch != dispatch {
		return omissionUnmeasured(result, "registry_identity_mismatch")
	}
	admission, err := omissionAdmission(c, rootPath, workPath, session, turn)
	if err != nil {
		return omissionUnmeasured(result, err.Error())
	}
	result = set(result, "turnAdmission", admission)
	result = set(result, "managedRequests", c.Managed)
	dispositionRaw, readable := ReadDisposition(directory, session, turn)
	if !readable {
		return omissionUnmeasured(result, "disposition_unreadable_or_malformed")
	}
	disposition, _ := dispositionRaw.(Obj)
	var receipt Obj
	if str(disposition, "outcome") == "ready_for_review" {
		receipt, err = omissionReceipt(ctx, selection.DBPath(), rid, session, turn, fieldOf(markerFact(marker, "relationship"), "executionGeneration"), dispatch)
		if err != nil {
			return omissionUnmeasured(result, omissionReceiptFailure(err))
		}
	}
	label := declarationLabel(disposition, session, turn, fieldOf(receipt, "atCurrentHead") == true)
	result = set(result, "currentObservation", Obj{{Key: "label", Value: label}, {Key: "detail", Value: omissionDetail(label, receipt)}})
	result = set(result, "declaration", nullableObj(disposition))
	result = set(result, "receipt", nullableObj(receipt))
	if afterCurrent != nil {
		afterCurrent()
	}
	var witness *bool
	if len(stops) > 0 {
		v := omissionLabels[fmt.Sprint(fieldOf(stops[len(stops)-1].Record, "observation"))] || fieldOf(stops[len(stops)-1].Record, "decisionState") == "unresolved_handoff"
		witness = &v
	}
	verdict := ClassifyOmission(factsFromContext(c, turn, witness, admission, label, now, grace))
	result = recordOmissionTerminal(result, c, verdict)
	after, identity, err := observeOmissionContext(ctx, selection, rid, dispatch, session, turn)
	if err != nil {
		return omissionUnmeasured(result, err.Error())
	}
	if snapshot != identity || !reflect.DeepEqual(c, after) {
		return omissionUnmeasured(result, "registry_changed_during_read")
	}
	return appendAnswer(result, verdict)
}
func nullableObj(o Obj) any {
	if len(o) == 0 {
		return nil
	}
	return o
}
func recordOmissionTerminal(result Obj, c omissionContext, verdict Obj) Obj {
	reason := fmt.Sprint(fieldOf(verdict, "reason"))
	if reason == "stop_unobserved" || reason == "bootstrap" || reason == "admission_unrecorded" || reason == "terminal_conflict" {
		return result
	}
	terminal, _ := omissionTerminal(c.Settlements)
	records := make([]any, len(c.Settlements))
	for i, r := range c.Settlements {
		records[i] = Obj{{Key: "status", Value: r.Status}, {Key: "at", Value: r.At}}
	}
	if reason == "daemon_execution_report" {
		reports := []any{}
		for _, event := range c.Events {
			if event["producer"] == "daemon_observation" && event["stage"] == "final" && event["outcome"] == terminal && event["status"] == terminal {
				reports = append(reports, event)
			}
		}
		result = set(result, "executionReports", reports)
	}
	return set(result, "terminalObservation", Obj{{Key: "source", Value: "relay_settlement"}, {Key: "records", Value: records}, {Key: "status", Value: terminal}})
}

// DeriveOmission reads the declaration mirror from one already-open store.
func DeriveOmission(ctx context.Context, s *store.Store, stateDir, rid, turn, now string, grace float64) Obj {
	result := append(Obj{{Key: "schema", Value: OmittedSchema}, {Key: "source", Value: OmittedStoreSource}}, omissionBase(now)[1:]...)
	result = append(result, F{Key: "selectors", Value: nil}, F{Key: "relationshipId", Value: rid}, F{Key: "relationshipStatus", Value: nil}, F{Key: "terminalObservation", Value: Obj{{Key: "source", Value: "relay_settlement"}, {Key: "status", Value: "unobserved"}}}, F{Key: "currentObservation", Value: nil}, F{Key: "turnAdmission", Value: "unmeasured"}, F{Key: "declaration", Value: nil}, F{Key: "receipt", Value: nil}, F{Key: "owed", Value: false}, F{Key: "owedReason", Value: OmittedNotOwed})
	var status, parent, child, issue, dispatch, dispatchTurn string
	var generation int64
	var superseded sql.NullString
	err := s.Q(ctx).QueryRowContext(ctx, `SELECT r.status,r.parent_task_id,r.child_task_id,r.issue_key,r.execution_generation,r.superseded_by,g.dispatch_request_id,COALESCE(g.dispatch_turn_id,'') FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id AND g.execution_generation=r.execution_generation WHERE r.relationship_id=?`, rid).Scan(&status, &parent, &child, &issue, &generation, &superseded, &dispatch, &dispatchTurn)
	if errors.Is(err, sql.ErrNoRows) {
		return omissionUnmeasured(result, "registration_unresolved")
	}
	if err != nil {
		return omissionUnmeasured(result, "evidence_unreadable: "+pythonStr(err))
	}
	result = set(result, "relationshipStatus", status)
	result = set(result, "executionGeneration", generation)
	result = set(result, "parentTaskId", parent)
	assignment := AssignmentID(dispatch)
	claimed, err := s.ReportingSession(ctx, assignment, child)
	if err != nil || claimed.DispatchRequestID != dispatch || claimed.Capability != "declarations/1" {
		return omissionUnmeasured(result, OmittedDeclarationsMissing)
	}
	c, err := readOmissionContext(ctx, s.Q(ctx), rid, child, dispatchTurn, dispatch)
	if err != nil {
		return omissionUnmeasured(result, "registration_unresolved")
	}
	if turn == "" {
		turn = dispatchTurn
		if len(c.Admitted) > 0 {
			sort.Slice(c.Admitted, func(i, j int) bool { return admissionLess(c.Admitted[i], c.Admitted[j]) })
			turn = c.Admitted[len(c.Admitted)-1].Turn
		}
	}
	if !ValidSegment(turn) {
		return omissionUnmeasured(result, "admission_unrecorded")
	}
	result = set(result, "selectors", Obj{{Key: "state", Value: stateDir}, {Key: "markerRoot", Value: claimed.MarkerRoot}, {Key: "workspace", Value: claimed.Workspace}, {Key: "assignment", Value: assignment}, {Key: "session", Value: child}, {Key: "turn", Value: turn}})
	c, err = readOmissionContext(ctx, s.Q(ctx), rid, child, turn, dispatch)
	if err != nil {
		return omissionUnmeasured(result, "registration_unresolved")
	}
	claimedWorkspace, workspaceErr := resolvedPath(claimed.Workspace)
	childWorkspace, childErr := resolvedPath(c.ChildCwd.String)
	if c.Child != child || !claimed.IssueKey.Valid || c.Issue != claimed.IssueKey.String || AssignmentID(c.Dispatch) != assignment || !c.ChildCwd.Valid || workspaceErr != nil || childErr != nil || claimedWorkspace != childWorkspace {
		return omissionUnmeasured(result, "registry_identity_mismatch")
	}
	admission, ok := admissionFor(c, turn)
	if !ok {
		return omissionUnmeasured(result, "stale_generation")
	}
	result = set(result, "turnAdmission", admission)
	result = set(result, "managedRequests", []any{})
	declared, err := s.TurnDeclaration(ctx, assignment, child, turn)
	var disposition Obj
	if err == nil {
		disposition = Obj{{Key: "sessionId", Value: child}, {Key: "turnId", Value: turn}, {Key: "outcome", Value: declared.Outcome}, {Key: "at", Value: declared.DeclaredAt}, {Key: "recordedAt", Value: declared.RecordedAt}}
	}
	var receipt Obj
	if str(disposition, "outcome") == "ready_for_review" {
		receipt, err = omissionReceipt(ctx, s.Path, rid, child, turn, generation, dispatch)
		if err != nil {
			return omissionUnmeasured(result, omissionReceiptFailure(err))
		}
	}
	label := declarationLabel(disposition, child, turn, fieldOf(receipt, "atCurrentHead") == true)
	result = set(result, "currentObservation", Obj{{Key: "label", Value: label}})
	result = set(result, "declaration", nullableObj(disposition))
	result = set(result, "receipt", nullableObj(receipt))
	w := omissionLabels[label]
	verdict := ClassifyOmission(factsFromContext(c, turn, &w, admission, label, now, grace))
	result = recordOmissionTerminal(result, c, verdict)
	return appendAnswer(result, verdict)
}

// pythonStr is str(error) for a failure an omission reading reports: a str sqlite3 or a hash could
// not encode is its UnicodeEncodeError's own words, whatever wrapped it on the way here.
func pythonStr(err error) string {
	if encode := store.EncodeError(err); encode != nil {
		return encode.Error()
	}
	return err.Error()
}
