package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Delivery states and phases (transport.py, delivery.py, policy.py, mergeturn.py).
const (
	stQueued          = "queued"
	stSending         = "sending"
	stDispatched      = "dispatched"
	stDeferredBusy    = "deferred_busy"
	stWithheldPreSend = "withheld_pre_send"
	stHeldUncertain   = "held_uncertain"
	stInboxOnly       = "inbox_only"
	stSuperseded      = "superseded"

	kindRevision       = "revision_request"
	kindMergeTurnGrant = "merge_turn_grant"

	pushChannelClosed  = "push_channel_closed"
	attemptCap         = "attempt_cap"
	hostLostTurn       = "host_lost_turn"
	turnCheckUndecided = "turn_check_undecided"
	hourlyCap          = "hourly_cap"
	minSendInterval    = "min_send_interval"

	mergeTurnAbsent        = "merge_turn_absent"
	mergeTurnClosed        = "merge_turn_closed"
	mergeTurnRegranted     = "merge_turn_regranted"
	mergeTurnGrantAnswered = "merge_turn_grant_answered"
	mergeTurnGrantBad      = "merge_turn_grant_unreadable"
)

// retryPolicy holds the RetryPolicy defaults status reads.
const (
	minSendIntervalSeconds = 5.0
	maxSendsPerHour        = 12
	rateWindowSeconds      = 3600
	staleAfterSeconds      = 900.0
)

// clockNow is SystemClock.now(): exact microseconds, as datetime.timestamp() computes them.
var clockNow = func() float64 { return float64(time.Now().UnixMicro()) / 1e6 }

// sqlText renders a column as Python's sqlite3 returns it (TEXT as str).
func col(row store.Row, name string) any {
	switch v := row.Get(name).(type) {
	case []byte:
		return string(v)
	default:
		return v
	}
}

func colText(row store.Row, name string) string {
	s, _ := col(row, name).(string)
	return s
}

// rowRecord is dict(row): every column in statement order.
func rowRecord(row store.Row) contract.OrderedObject {
	out := make(contract.OrderedObject, 0, len(row))
	for _, column := range row {
		out = append(out, contract.Field{Key: column.Name, Value: col(row, column.Name)})
	}
	return out
}

func loads(text string) any {
	value, err := decodeJSON([]byte(text))
	if err != nil {
		return nil
	}
	return value
}

// snapshot is DeliveryService.snapshot.
func snapshot(ctx context.Context, s *store.Store, relationship string) (contract.OrderedObject, error) {
	query, args := "SELECT * FROM deliveries", []any{}
	if relationship != "" {
		query += " WHERE relationship_id = ?"
		args = append(args, relationship)
	}
	rows, err := s.All(ctx, query+" ORDER BY created_at", args...)
	if err != nil {
		return nil, err
	}
	now := clockNow()
	paced := map[string]contract.OrderedObject{}
	items := []any{}
	for _, row := range rows {
		event := colText(row, "event_id")
		attempts, err := s.All(ctx, "SELECT request_id, attempt_no, internal_state, state, affirmative_evidence,"+
			" operation_observation, recipient_scan, record FROM attempts WHERE event_id = ?"+
			" ORDER BY attempt_no", event)
		if err != nil {
			return nil, err
		}
		ack, err := s.One(ctx, "SELECT * FROM acks WHERE event_id = ?", event)
		if err != nil {
			return nil, err
		}
		verdict, err := s.One(ctx, "SELECT verdict FROM verdicts WHERE event_id = ?", event)
		if err != nil {
			return nil, err
		}
		failure, err := lastFailure(ctx, s, event)
		if err != nil {
			return nil, err
		}
		superseded, err := s.One(ctx, "SELECT reason, noted_at FROM delivery_supersession WHERE event_id = ?", event)
		if err != nil {
			return nil, err
		}
		grant, err := grantState(ctx, s, row)
		if err != nil {
			return nil, err
		}
		var pacing contract.OrderedObject
		state := colText(row, "state")
		if (state == stQueued || state == stDeferredBusy || state == stWithheldPreSend) && !truthy(col(row, "hold_reason")) {
			recipient := colText(row, "recipient_task_id")
			reading, seen := paced[recipient]
			if !seen {
				if reading, err = sendPacing(ctx, s, recipient, now); err != nil {
					return nil, err
				}
				paced[recipient] = reading
			}
			pacing = pacingHolding(reading, col(row, "next_eligible_at"))
		}
		reading, err := currentSettingsHold(ctx, s, event)
		if err != nil {
			return nil, err
		}
		var settingsHold, recovery any
		if hold, ok := get(reading, "hold").(contract.OrderedObject); ok {
			withKind := append(append(contract.OrderedObject{}, hold...), contract.Field{Key: "kind", Value: get(reading, "kind")})
			settingsHold = withKind
			recovery = settingsRecoveryRecord(s, withKind, event, colText(row, "recipient_task_id"), colText(row, "kind") == kindRevision)
		}
		var ackVerified, verdictValue any
		if ack != nil {
			ackVerified = col(ack, "verified")
		}
		if verdict != nil {
			verdictValue = col(verdict, "verdict")
		}
		attemptDetail := []any{}
		for _, a := range attempts {
			attemptDetail = append(attemptDetail, rowRecord(a))
		}
		var failureValue, supersededValue, pacingValue any
		if failure != nil {
			failureValue = failure
		}
		if superseded != nil {
			supersededValue = rowRecord(superseded)
		}
		if pacing != nil {
			pacingValue = pacing
		}
		items = append(items, contract.OrderedObject{
			{Key: "eventId", Value: event},
			{Key: "kind", Value: col(row, "kind")},
			{Key: "recipient", Value: col(row, "recipient_task_id")},
			{Key: "state", Value: state},
			{Key: "reported", Value: reportedState(row, ack, grant, superseded)},
			{Key: "attempts", Value: col(row, "attempt_count")},
			{Key: "holdReason", Value: col(row, "hold_reason")},
			{Key: "nextEligibleAt", Value: col(row, "next_eligible_at")},
			{Key: "dispatchEvidence", Value: col(row, "dispatch_evidence")},
			{Key: "acknowledged", Value: ack != nil},
			{Key: "ackVerified", Value: ackVerified},
			{Key: "verdict", Value: verdictValue},
			{Key: "attemptDetail", Value: attemptDetail},
			{Key: "phase", Value: phase(row, attempts, ack, failure, superseded, grant, pacing, reading)},
			{Key: "pacing", Value: pacingValue},
			{Key: "lastFailedOperation", Value: failureValue},
			{Key: "settingsHold", Value: settingsHold},
			{Key: "recovery", Value: recovery},
			{Key: "nextRetryAt", Value: col(row, "next_eligible_at")},
			{Key: "supersededNote", Value: supersededValue},
		})
	}
	intentQuery := "SELECT i.* FROM delivery_intent i  LEFT JOIN deliveries d ON d.event_id = i.event_id WHERE d.event_id IS NULL"
	intentArgs := []any{}
	if relationship != "" {
		intentQuery += " AND i.relationship_id = ?"
		intentArgs = append(intentArgs, relationship)
	}
	intentRows, err := s.All(ctx, intentQuery+" ORDER BY i.noted_at", intentArgs...)
	if err != nil {
		return nil, err
	}
	intents := []any{}
	for _, row := range intentRows {
		intents = append(intents, contract.OrderedObject{
			{Key: "eventId", Value: col(row, "event_id")}, {Key: "relationshipId", Value: col(row, "relationship_id")},
			{Key: "kind", Value: col(row, "kind")}, {Key: "recipient", Value: col(row, "recipient_task_id")},
			{Key: "phase", Value: "refused_pre_queue"}, {Key: "attempts", Value: col(row, "attempts")},
			{Key: "nextRetryAt", Value: col(row, "next_retry_at")}, {Key: "lastError", Value: col(row, "last_error")},
			{Key: "notedAt", Value: col(row, "noted_at")},
		})
	}
	return contract.OrderedObject{{Key: "deliveries", Value: items}, {Key: "pendingIntents", Value: intents}}, nil
}

// lastFailure is _last_failure: the newest failed_operations row for the event, as a dict.
func lastFailure(ctx context.Context, s *store.Store, event string) (contract.OrderedObject, error) {
	row, err := s.One(ctx, "SELECT * FROM failed_operations WHERE scope_key = ? ORDER BY occurred_at DESC", event)
	if err != nil || row == nil {
		return nil, err
	}
	return rowRecord(row), nil
}

// grantState is _grant_state: a grant delivery's own turn's answer, or "" for any other kind.
func grantState(ctx context.Context, s *store.Store, row store.Row) (string, error) {
	if colText(row, "kind") != kindMergeTurnGrant {
		return "", nil
	}
	event, err := s.One(ctx, "SELECT relationship_id, execution_generation, outcome, event_id, receipt  FROM events WHERE event_id = ?", colText(row, "event_id"))
	if err != nil || event == nil {
		return "", err
	}
	envelope, ok := loads(colText(event, "receipt")).(contract.OrderedObject)
	if !ok {
		return mergeTurnGrantBad, nil
	}
	turn, turnOK := get(envelope, "turnId").(string)
	grant, grantOK := get(envelope, "grantId").(string)
	if !turnOK || turn == "" || !grantOK || grant == "" {
		return mergeTurnGrantBad, nil
	}
	return grantSupersession(ctx, s, turn, grant)
}

// grantSupersession is mergeturn.grant_supersession_in.
func grantSupersession(ctx context.Context, s *store.Store, turn, grant string) (string, error) {
	row, err := s.One(ctx, "SELECT state, tenure FROM merge_turns WHERE turn_id = ?", turn)
	if err != nil {
		return "", err
	}
	if row == nil {
		return mergeTurnAbsent, nil
	}
	answered, err := s.One(ctx, "SELECT 1 FROM merge_turn_ledger WHERE turn_id = ? AND idempotency_key = ?", turn, "grant_acknowledged:"+grant)
	if err != nil {
		return "", err
	}
	if answered != nil {
		return mergeTurnGrantAnswered, nil
	}
	switch colText(row, "state") {
	case "holding", "merging", "unknown":
	default:
		return mergeTurnClosed, nil
	}
	current, err := currentGrant(ctx, s, turn, col(row, "tenure"))
	if err != nil {
		return "", err
	}
	if current != "" && current != grant {
		return mergeTurnRegranted, nil
	}
	return "", nil
}

// currentGrant is MergeTurn._current_grant_in through grant_envelope.
func currentGrant(ctx context.Context, s *store.Store, turn string, tenure any) (string, error) {
	rows, err := s.All(ctx, "SELECT evidence, evidence_kind, idempotency_key FROM merge_turn_ledger  WHERE turn_id = ? AND evidence_kind = ?", turn, "grant")
	if err != nil {
		return "", err
	}
	best, bestSequence := "", int64(math.MinInt64)
	for _, row := range rows {
		envelope, ok := loads(colText(row, "evidence")).(contract.OrderedObject)
		if !ok {
			continue
		}
		sequence, ok := pyInt(get(envelope, "sequence"))
		if !ok || get(envelope, "turnId") != turn || !pyEqual(get(envelope, "tenure"), tenure) {
			continue
		}
		derived := "mtg-" + sha256Hex(turn + "|" + fmt.Sprint(tenure) + "|" + strconv.FormatInt(sequence, 10))[:32]
		if get(envelope, "grantId") != derived || colText(row, "idempotency_key") != "grant:"+derived {
			continue
		}
		if r, ok := get(envelope, "recipientTaskId").(string); !ok || r == "" {
			continue
		}
		if c, ok := get(envelope, "candidateHead").(string); !ok || c == "" {
			continue
		}
		if sequence > bestSequence {
			best, bestSequence = derived, sequence
		}
	}
	return best, nil
}

// sendPacing is send_pacing with RetryPolicy.pacing.
func sendPacing(ctx context.Context, s *store.Store, recipient string, now float64) (contract.OrderedObject, error) {
	window := math.Floor(now/rateWindowSeconds) * rateWindowSeconds
	reach := rateWindowSeconds * (1 + math.Floor(minSendIntervalSeconds/rateWindowSeconds))
	last, err := s.One(ctx, "SELECT MAX(last_send_at) AS last FROM recipient_rate WHERE recipient_task_id = ?   AND window_start BETWEEN ? AND ?", recipient, window-reach, window)
	if err != nil {
		return nil, err
	}
	used, err := s.One(ctx, "SELECT sends FROM recipient_rate WHERE recipient_task_id = ? AND window_start = ?", recipient, window)
	if err != nil {
		return nil, err
	}
	var sends int64
	if used != nil {
		sends, _ = col(used, "sends").(int64)
	}
	var lastSend any
	if last != nil {
		lastSend = col(last, "last")
	}
	reading := contract.OrderedObject{{Key: "sends", Value: sends}, {Key: "cap", Value: int64(maxSendsPerHour)}, {Key: "windowStart", Value: int64(window)}}
	var gapEnds any
	if lastSend != nil {
		l, _ := pyNumber(lastSend)
		gapEnds = l + minSendIntervalSeconds
	}
	if sends >= maxSendsPerHour {
		reopens := window + rateWindowSeconds
		if g, ok := gapEnds.(float64); ok && g > reopens {
			return append(contract.OrderedObject{{Key: "reason", Value: hourlyCap}, {Key: "reopensAt", Value: g}}, reading...), nil
		}
		return append(contract.OrderedObject{{Key: "reason", Value: hourlyCap}, {Key: "reopensAt", Value: int64(reopens)}}, reading...), nil
	}
	if g, ok := gapEnds.(float64); ok && now < g {
		return append(contract.OrderedObject{{Key: "reason", Value: minSendInterval}, {Key: "reopensAt", Value: g}}, reading...), nil
	}
	return nil, nil
}

// pacingHolding is policy.pacing_holding.
func pacingHolding(pacing contract.OrderedObject, nextEligible any) contract.OrderedObject {
	if pacing == nil {
		return nil
	}
	reopens := get(pacing, "reopensAt")
	if reopens == nil || nextEligible == nil {
		return pacing
	}
	n, _ := pyNumber(nextEligible)
	r, _ := pyNumber(reopens)
	if n <= r {
		return pacing
	}
	return nil
}

// settingsHoldColumns is SETTINGS_HOLD_COLUMNS for FROM deliveries d.
const latestSettled = "(SELECT %[1]s.request_id FROM attempts %[1]s WHERE %[1]s.event_id = d.event_id AND %[1]s.internal_state = 'settled' ORDER BY %[1]s.attempt_no DESC LIMIT 1)"

var settingsHoldColumns = "d.state AS sh_state, d.hold_reason AS sh_hold_reason," +
	" " + fmt.Sprintf(latestSettled, "sha") + " AS sh_request," +
	" (SELECT json_object('seq', shj.seq, 'detail', shj.detail) FROM journal shj" +
	"   WHERE shj.subject = d.event_id AND +shj.kind = 'delivery_attempted'" +
	"     AND (CASE WHEN json_valid(shj.detail)" +
	"          THEN json_extract(shj.detail, '$.requestId') END)" +
	"         = " + fmt.Sprintf(latestSettled, "shb") +
	"   ORDER BY shj.seq DESC LIMIT 1) AS sh_settled_sent," +
	" (SELECT json_object('seq', shr.seq, 'detail', shr.detail) FROM journal shr" +
	"   WHERE shr.subject = " + fmt.Sprintf(latestSettled, "shc") +
	"     AND +shr.kind = 'reconciled'" +
	"   ORDER BY shr.seq DESC LIMIT 1) AS sh_settled_reconciled," +
	" (SELECT json_object('seq', shp.seq, 'detail', shp.detail) FROM journal shp" +
	"   WHERE shp.subject = d.event_id AND +shp.kind = 'delivery_presend_withheld'" +
	"   ORDER BY shp.seq DESC LIMIT 1) AS sh_presend," +
	" (SELECT shf.occurred_at FROM failed_operations shf WHERE shf.scope_key = d.event_id" +
	"   AND shf.operation = 'settings_check') AS sh_settings_at," +
	" (SELECT shl.occurred_at FROM failed_operations shl WHERE shl.scope_key = d.event_id" +
	"   AND shl.operation = 'lifecycle_read') AS sh_lifecycle_at," +
	" (SELECT shi.at FROM journal shi WHERE shi.subject = d.event_id" +
	"   AND +shi.kind = 'delivery_withheld_inactive' ORDER BY shi.seq DESC LIMIT 1)" +
	"   AS sh_inactive_at"

type packedRow struct {
	seq    float64
	detail contract.OrderedObject
}

// packed is _packed.
func packed(value any) *packedRow {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	outer, ok := loads(text).(contract.OrderedObject)
	if !ok {
		return nil
	}
	detail := contract.OrderedObject{}
	if raw := get(outer, "detail"); truthy(raw) {
		rawText, ok := raw.(string)
		if !ok {
			return nil
		}
		decoded, err := decodeJSON([]byte(rawText))
		if err != nil {
			return nil
		}
		if object, ok := decoded.(contract.OrderedObject); ok {
			detail = object
		}
	}
	seq, _ := pyNumber(get(outer, "seq"))
	return &packedRow{seq, detail}
}

// currentSettingsHold is current_settings_hold + settings_hold_reading.
func currentSettingsHold(ctx context.Context, s *store.Store, event string) (contract.OrderedObject, error) {
	row, err := s.One(ctx, "SELECT "+settingsHoldColumns+" FROM deliveries d WHERE d.event_id = ?", event)
	if err != nil {
		return nil, err
	}
	reading := contract.OrderedObject{{Key: "kind", Value: nil}, {Key: "chosen", Value: nil}, {Key: "definitive", Value: false}, {Key: "hold", Value: nil}, {Key: "presendOperation", Value: nil}}
	set := func(key string, value any) { reading[fieldIndex(reading, key)].Value = value }
	if row == nil {
		return reading, nil
	}
	state := colText(row, "sh_state")
	kind := map[string]string{stWithheldPreSend: "withheld", stInboxOnly: "channel_closed"}[state]
	if state == stWithheldPreSend && truthy(col(row, "sh_hold_reason")) {
		kind = ""
		if colText(row, "sh_hold_reason") == attemptCap {
			kind = "capped"
		}
	}
	if kind == "" {
		return reading, nil
	}
	set("kind", kind)
	var settlement *packedRow
	for _, one := range []*packedRow{packed(col(row, "sh_settled_sent")), packed(col(row, "sh_settled_reconciled"))} {
		if one != nil && (settlement == nil || one.seq > settlement.seq) {
			settlement = one
		}
	}
	if presend := packed(col(row, "sh_presend")); presend != nil && (settlement == nil || presend.seq > settlement.seq) {
		operation, _ := get(presend.detail, "operation").(string)
		set("chosen", "pre_send")
		set("definitive", true)
		set("presendOperation", nullableText(operation))
		if reason, ok := get(presend.detail, "reason").(string); ok && presendSettingsRefusals[reason] {
			text, _ := get(presend.detail, "detail").(string)
			set("hold", contract.OrderedObject{{Key: "source", Value: "pre_send"}, {Key: "reason", Value: reason},
				{Key: "field", Value: nil}, {Key: "requestId", Value: nil}, {Key: "detail", Value: nullableText(text)}})
		}
		return reading, nil
	}
	if settlement != nil && has(settlement.detail, "settingsRefusal") {
		set("chosen", "attempt")
		set("definitive", true)
		refusal, ok := get(settlement.detail, "settingsRefusal").(contract.OrderedObject)
		if reason, isText := get(refusal, "reason").(string); ok && isText {
			field, _ := get(refusal, "field").(string)
			set("hold", contract.OrderedObject{{Key: "source", Value: "attempt"}, {Key: "reason", Value: reason},
				{Key: "field", Value: nullableText(field)}, {Key: "requestId", Value: col(row, "sh_request")}})
		}
		return reading, nil
	}
	if col(row, "sh_request") != nil {
		set("chosen", "legacy")
	}
	settingsAt := col(row, "sh_settings_at")
	cleared := false
	if at, ok := settingsAt.(string); ok {
		for _, later := range []any{col(row, "sh_lifecycle_at"), col(row, "sh_inactive_at")} {
			if l, ok := later.(string); ok && l > at {
				cleared = true
			}
		}
	}
	if kind == "channel_closed" || (settingsAt != nil && !cleared) {
		set("hold", contract.OrderedObject{{Key: "source", Value: "undetermined"}, {Key: "reason", Value: nil},
			{Key: "field", Value: nil}, {Key: "requestId", Value: nil}})
	}
	return reading, nil
}

// settingsRecoveryRecord is assignment.settings_recovery_record with settings_hold_recovery.
func settingsRecoveryRecord(s *store.Store, hold contract.OrderedObject, event, recipient string, revision bool) contract.OrderedObject {
	kind, _ := get(hold, "kind").(string)
	code, _ := get(hold, "reason").(string)
	source, _ := get(hold, "source").(string)
	actor, command, then := "", "", ""
	var later any
	roleThen, hasRole := holdRefusalThen[code]
	channelThen := shHoldChannelThen
	if revision {
		channelThen = shHoldCorrectionChannelThen
	}
	switch {
	case source == "undetermined" && kind == "capped":
		actor, command, then = "parent", shShowEvent, shHoldCappedThen+"; "+shHoldUndeterminedCause
	case source == "undetermined" && kind == "channel_closed":
		actor, command, then, later = "parent", shShowEvent, channelThen+"; "+shHoldUndeterminedCause, shLaterByOwner
	case source == "undetermined":
		actor, command, then = "daemon", shShowEvent, shHoldUndeterminedThen
	case kind == "capped":
		actor, command, then, later = "parent", shShowEvent, shHoldCappedThen, shLaterByOperator
		if hasRole {
			later = "for later deliveries, " + roleThen
		}
	case kind == "channel_closed":
		actor, command, then, later = "parent", shShowEvent, channelThen, shLaterByOwner
	case daemonSettingsCodes[code]:
		actor, command, then = "daemon", shSettingsShow, shHoldDaemonThen
	case hasRole:
		actor, command, then = "operator", shSettingsShow, roleThen
	default:
		actor, command, then = "operator", shSettingsShow, shHoldOperatorThen
	}
	directory := filepath.Dir(absolutePath(s.Path))
	program := relayProgram()
	rendered := shellCommand(append(program, "--state", directory, "show", "--event", event)...)
	if command != shShowEvent && recipient != "" {
		rendered = shellCommand(append(program, "--state", directory, "settings-show", "--task", recipient)...)
	}
	reason := any("undetermined")
	if code != "" {
		reason = code
	}
	return contract.OrderedObject{
		{Key: "actor", Value: actor}, {Key: "reason", Value: reason}, {Key: "command", Value: rendered},
		{Key: "then", Value: then}, {Key: "laterDeliveries", Value: later}, {Key: "refusalDetail", Value: get(hold, "detail")},
	}
}

func absolutePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, path)
}

// relayProgram is supervisorchannel.relay_program: the absolute command that runs THIS relay.
// Python names the console script installed beside its interpreter; the Go installation's
// equivalent is the codex-session-relay link beside the crw binary, else crw itself.
func relayProgram() []string {
	executable, err := os.Executable()
	if err != nil {
		return []string{"codex-session-relay"}
	}
	if real, err := filepath.EvalSymlinks(executable); err == nil {
		executable = real
	}
	script := filepath.Join(filepath.Dir(executable), "codex-session-relay")
	if info, err := os.Stat(script); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		return []string{script}
	}
	return []string{executable, "relay"}
}

func shellCommand(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, one := range argv {
		quoted[i] = shellQuote(one)
	}
	return strings.Join(quoted, " ")
}

// reportedState is _reported_state.
func reportedState(row, ack store.Row, grant string, superseded store.Row) string {
	state, hold := colText(row, "state"), colText(row, "hold_reason")
	if ack != nil && colText(ack, "verified") == "verified" && truthy(col(ack, "accepted")) {
		return "acknowledged"
	}
	if colText(row, "kind") == kindMergeTurnGrant {
		if grant == mergeTurnGrantAnswered {
			return "grant_acknowledged"
		}
		if grant != "" {
			return "superseded:" + grant
		}
		if state == stDispatched {
			return "dispatched_awaiting_grant_acknowledgement"
		}
	}
	switch {
	case state == stInboxOnly:
		return "stored_not_woken"
	case state == stDispatched:
		return "dispatched_awaiting_ack"
	case state == stHeldUncertain:
		if superseded != nil {
			return "superseded:" + colText(superseded, "reason")
		}
		if hold != "" {
			return "held:" + hold
		}
		return "held_uncertain_awaiting_evidence"
	case state == stQueued && colText(row, "dispatch_evidence") == hostLostTurn && hold == "":
		return "redelivering:" + hostLostTurn
	case hold != "":
		return "held:" + hold
	}
	return state
}

// phase is _phase: which stage a delivery is actually at, without inventing certainty.
func phase(row store.Row, attempts []store.Row, ack store.Row, failure contract.OrderedObject, superseded store.Row, grant string, pacing, reading contract.OrderedObject) string {
	state, kind, hold := colText(row, "state"), colText(row, "kind"), colText(row, "hold_reason")
	if ack != nil && colText(ack, "verified") == "verified" {
		if truthy(col(ack, "accepted")) {
			return "acknowledged"
		}
		return "rejected"
	}
	if kind == kindMergeTurnGrant && grant != "" {
		if grant == mergeTurnGrantAnswered {
			return "grant_acknowledged"
		}
		return "superseded:" + grant
	}
	if state == stSuperseded {
		return "superseded"
	}
	if superseded != nil {
		return "superseded:" + colText(superseded, "reason")
	}
	if state == stInboxOnly || hold == pushChannelClosed {
		return "channel_closed"
	}
	if state == stDispatched {
		if kind == kindRevision {
			return "awaiting_child_receipt"
		}
		if kind == kindMergeTurnGrant {
			return "awaiting_grant_acknowledgement"
		}
		if len(attempts) > 0 && strings.HasPrefix(colText(attempts[len(attempts)-1], "recipient_scan"), turnCheckUndecided+":") {
			return "awaiting_ack:" + turnCheckUndecided
		}
		return "awaiting_ack"
	}
	if state == stDeferredBusy {
		return "parent_busy"
	}
	var latest store.Row
	for _, a := range attempts {
		if colText(a, "internal_state") == "settled" {
			latest = a
		}
	}
	record := contract.OrderedObject{}
	if latest != nil && truthy(col(latest, "record")) {
		if decoded, ok := loads(colText(latest, "record")).(contract.OrderedObject); ok {
			record = decoded
		}
	}
	failed := get(record, "failedOperation")
	failedText := ""
	if truthy(failed) {
		failedText = fmt.Sprint(failed)
	}
	if state == stHeldUncertain {
		if hold != "" {
			return "held:" + hold
		}
		if truthy(get(record, "turnId")) {
			return "turn_accepted"
		}
		return "outcome_unknown"
	}
	if state == stWithheldPreSend && truthy(reading) && truthy(get(reading, "definitive")) {
		if get(reading, "chosen") == "attempt" {
			if get(reading, "hold") != nil {
				return "settings_rejected"
			}
			if failedText != "" {
				return "withheld:" + failedText
			}
			return "withheld_pre_send"
		}
		if operation, ok := get(reading, "presendOperation").(string); ok && operation != "" {
			return "withheld:" + operation
		}
		return "withheld_pre_send"
	}
	if state == stWithheldPreSend && latest != nil {
		if failedText == "thread/resume" && failure != nil && get(failure, "operation") == "settings_check" {
			return "settings_rejected"
		}
		if failedText != "" {
			return "withheld:" + failedText
		}
		return "withheld_pre_send"
	}
	if state == stWithheldPreSend {
		if failure != nil {
			return "withheld:" + fmt.Sprint(get(failure, "operation"))
		}
		return "withheld_pre_send"
	}
	if hold != "" {
		return "held:" + hold
	}
	switch {
	case state == stSending:
		return "in_flight"
	case state == stQueued && colText(row, "dispatch_evidence") == hostLostTurn:
		return "redelivering:" + hostLostTurn
	case state == stQueued && pacing != nil && get(pacing, "reason") == hourlyCap:
		return "awaiting_send:" + hourlyCap
	case state == stQueued:
		return "awaiting_send"
	}
	return "awaiting_receipt"
}

// isoAge is observation_health.age: seconds since an ISO stamp, or nil when unreadable.
func isoAge(stamp any, now float64) any {
	text, ok := stamp.(string)
	if !ok || text == "" {
		return nil
	}
	seen, ok := parseISO(strings.Replace(text, "Z", "+00:00", 1))
	if !ok {
		return nil
	}
	return math.Max(0, now-seen)
}

// parseISO is datetime.fromisoformat(...).timestamp() for the stamps this store writes; a
// naive stamp is read as local time, as Python does.
func parseISO(text string) (float64, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05.999999-07:00", "2006-01-02T15:04:05-07:00", "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return float64(parsed.UnixMicro()) / 1e6, true
		}
	}
	return 0, false
}

// observationHealth is DeliveryService.observation_health.
func observationHealth(ctx context.Context, s *store.Store, relationship string, now float64) (contract.OrderedObject, error) {
	scoped, args := "", []any{}
	if relationship != "" {
		scoped, args = " AND e.relationship_id = ?", []any{relationship}
	}
	stagedRows, err := s.All(ctx, "SELECT e.event_id, e.turn_id, e.staged_at, e.first_seen_at FROM events e"+
		"  JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" WHERE e.stage = 'staged'"+
		"   AND r.status = 'active' AND r.superseded_by IS NULL"+scoped+" ORDER BY e.first_seen_at", args...)
	if err != nil {
		return nil, err
	}
	staged := []any{}
	oldest := 0.0
	for _, row := range stagedRows {
		stamp := col(row, "staged_at")
		if !truthy(stamp) {
			stamp = col(row, "first_seen_at")
		}
		ageValue := isoAge(stamp, now)
		if a, ok := ageValue.(float64); ok && a > oldest {
			oldest = a
		}
		staged = append(staged, contract.OrderedObject{{Key: "eventId", Value: col(row, "event_id")}, {Key: "turnId", Value: col(row, "turn_id")}, {Key: "ageSeconds", Value: ageValue}})
	}
	anchorScope := ""
	if relationship != "" {
		anchorScope = " AND r.relationship_id = ?"
	}
	anchorRows, err := s.All(ctx, "SELECT g.relationship_id, g.dispatch_turn_id, p.last_polled_at, p.last_error"+
		"     , r.child_task_id"+
		"     , (SELECT COUNT(*) FROM assignment_settlements o"+
		"         WHERE o.thread_id = r.child_task_id"+
		"           AND o.turn_id = g.dispatch_turn_id"+
		"           AND o.relationship_id = r.relationship_id) AS observed"+
		"     , (SELECT COUNT(*) FROM events e"+
		"         WHERE e.turn_thread_id = r.child_task_id"+
		"           AND e.turn_id = g.dispatch_turn_id"+
		"           AND e.relationship_id = r.relationship_id"+
		"           AND e.stage = 'staged') AS staged_here"+
		"  FROM relationships r"+
		"  JOIN generations g ON g.relationship_id = r.relationship_id"+
		"   AND g.execution_generation = r.execution_generation"+
		"  LEFT JOIN poll_observations p"+
		"    ON p.relationship_id = g.relationship_id"+
		"   AND p.execution_generation = g.execution_generation"+
		"   AND p.turn_id = g.dispatch_turn_id"+
		" WHERE r.status = 'active' AND r.superseded_by IS NULL"+anchorScope, args...)
	if err != nil {
		return nil, err
	}
	anchors := contract.OrderedObject{}
	setAnchor := func(id string, value contract.OrderedObject) {
		if at := fieldIndex(anchors, id); at >= 0 {
			anchors[at].Value = value
			return
		}
		anchors = append(anchors, contract.Field{Key: id, Value: value})
	}
	for _, row := range anchorRows {
		id := colText(row, "relationship_id")
		if col(row, "dispatch_turn_id") == nil {
			setAnchor(id, contract.OrderedObject{{Key: "turnId", Value: nil}, {Key: "lastPolledAt", Value: nil}, {Key: "ageSeconds", Value: nil},
				{Key: "lastError", Value: nil}, {Key: "settled", Value: false}, {Key: "anchorPending", Value: true}})
			continue
		}
		settled := truthy(col(row, "observed")) && !truthy(col(row, "staged_here"))
		setAnchor(id, contract.OrderedObject{{Key: "turnId", Value: col(row, "dispatch_turn_id")}, {Key: "lastPolledAt", Value: col(row, "last_polled_at")},
			{Key: "ageSeconds", Value: isoAge(col(row, "last_polled_at"), now)}, {Key: "lastError", Value: col(row, "last_error")},
			{Key: "settled", Value: settled}, {Key: "anchorPending", Value: false}})
	}
	backlogRows, err := s.All(ctx, "SELECT e.relationship_id AS relationship_id, COUNT(*) AS n FROM events e"+
		"  JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" WHERE e.stage = 'staged'"+
		"   AND r.status = 'active' AND r.superseded_by IS NULL"+scoped+" GROUP BY e.relationship_id", args...)
	if err != nil {
		return nil, err
	}
	backlog := contract.OrderedObject{}
	for _, row := range backlogRows {
		backlog = append(backlog, contract.Field{Key: colText(row, "relationship_id"), Value: col(row, "n")})
	}
	never, stale := 0, 0
	for _, field := range anchors {
		a := field.Value.(contract.OrderedObject)
		if truthy(get(a, "settled")) || truthy(get(a, "anchorPending")) {
			continue
		}
		if get(a, "lastPolledAt") == nil {
			never++
		}
		if age, ok := get(a, "ageSeconds").(float64); ok && age > staleAfterSeconds {
			stale++
		}
	}
	health, reason := "healthy", ""
	switch {
	case never > 0 || stale > 0:
		health, reason = "stalled", fmt.Sprintf("%d anchors never successfully polled, %d not polled for over %.0fs", never, stale, staleAfterSeconds)
	case oldest > staleAfterSeconds:
		health, reason = "degraded", fmt.Sprintf("a staged event has waited %s", pythonRoundFormat(oldest))
	}
	return contract.OrderedObject{
		{Key: "stagedEvents", Value: staged}, {Key: "oldestStagedAgeSeconds", Value: oldest},
		{Key: "anchors", Value: anchors}, {Key: "backlog", Value: backlog},
		{Key: "health", Value: health}, {Key: "reason", Value: reason},
		{Key: "note", Value: "process liveness is reported separately and is not health"},
	}, nil
}

// pythonRoundFormat is f"{x:.0f}s": round-half-even, as Python formats.
func pythonRoundFormat(x float64) string {
	return strconv.FormatFloat(math.RoundToEven(x), 'f', 0, 64) + "s"
}

// faultAttention is FaultLedger.attention for the built-in kinds (the only kinds a process
// without --kind-module registers).
// Like Python it reads inside one BEGIN IMMEDIATE transaction, every read on the body's ctx.
func faultAttention(ctx context.Context, s *store.Store, now float64) (contract.OrderedObject, error) {
	var result contract.OrderedObject
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		result, err = attentionIn(ctx, s, now)
		return err
	})
	return result, err
}

func count(ctx context.Context, s *store.Store, query string, args ...any) (int64, error) {
	row, err := s.One(ctx, query, args...)
	if err != nil || row == nil {
		return 0, err
	}
	n, _ := col(row, "n").(int64)
	return n, nil
}

var builtInKinds = map[string]struct {
	requiresIssue bool
	target        string
}{
	"open_record": {false, "team+project"}, "append_comment": {true, ""}, "update_record": {true, ""},
}

var defaultLimits = map[string][2]float64{"open_record": {5, 3600}, "append_comment": {20, 3600}, "update_record": {20, 3600}, "notification": {10, 3600}}

func attentionIn(ctx context.Context, s *store.Store, moment float64) (contract.OrderedObject, error) {
	states := map[string]int64{}
	for _, state := range []string{"failed", "uncertain", "issued"} {
		n, err := count(ctx, s, "SELECT COUNT(*) AS n FROM fault_publications WHERE state = ?", state)
		if err != nil {
			return nil, err
		}
		states[state] = n
	}
	claimed, err := s.One(ctx, "SELECT COUNT(*) AS n, SUM(CASE WHEN lease_until <= ? THEN 1 ELSE 0 END) AS lapsed FROM fault_publications WHERE state = ?", moment, "claimed")
	if err != nil {
		return nil, err
	}
	issuedLapsed, err := count(ctx, s, "SELECT COUNT(*) AS n FROM fault_publications WHERE state = ? AND (lease_until IS NULL OR lease_until <= ?)", "issued", moment)
	if err != nil {
		return nil, err
	}
	pending := contract.OrderedObject{{Key: "ready", Value: int64(0)}, {Key: "awaitingTarget", Value: int64(0)}, {Key: "held", Value: int64(0)},
		{Key: "awaitingRecord", Value: int64(0)}, {Key: "scopeKeyContested", Value: int64(0)}, {Key: "backingOff", Value: int64(0)},
		{Key: "kindUnregistered", Value: int64(0)}, {Key: "issueOwned", Value: int64(0)}}
	names := map[string]string{"": "ready", "awaiting_target": "awaitingTarget", "budget_spent": "held", "awaiting_record": "awaitingRecord",
		"scope_key_contested": "scopeKeyContested", "backing_off": "backingOff", "kind_unregistered": "kindUnregistered", "issue_owned": "issueOwned"}
	rows, err := s.All(ctx, "SELECT * FROM fault_publications WHERE state = ? ORDER BY rowid LIMIT 1000", "pending")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		reason, err := heldReason(ctx, s, row, moment)
		if err != nil {
			return nil, err
		}
		at := fieldIndex(pending, names[reason])
		pending[at].Value = pending[at].Value.(int64) + 1
	}
	total, err := count(ctx, s, "SELECT COUNT(*) AS n FROM fault_publications WHERE state = ?", "pending")
	if err != nil {
		return nil, err
	}
	pending = append(pending, contract.Field{Key: "unclassified", Value: total - int64(len(rows))})
	unlinked, err := count(ctx, s, "SELECT COUNT(*) AS n FROM fault_links l"+
		"  JOIN fault_ledger f ON f.fault_id = l.fault_id"+
		"  LEFT JOIN fault_targets t ON t.scope_key = f.scope_key"+
		"  LEFT JOIN fault_target_projects tp ON tp.scope_key = f.scope_key"+
		" WHERE NOT (l.state = ? AND t.scope_key IS NOT NULL"+
		"   AND tp.product IS f.product AND tp.project_ref IS NOT NULL"+
		"   AND l.observed_project_ref IS tp.project_ref"+
		"   AND NOT EXISTS (SELECT 1 FROM fault_ledger o"+
		"                    WHERE o.scope_key = f.scope_key AND o.product != f.product))", "linked")
	if err != nil {
		return nil, err
	}
	notifications := contract.OrderedObject{}
	for _, state := range []string{"pending", "reserved", "uncertain"} {
		n, err := count(ctx, s, "SELECT COUNT(*) AS n FROM fault_notifications WHERE state = ?", state)
		if err != nil {
			return nil, err
		}
		notifications = append(notifications, contract.Field{Key: state, Value: n})
	}
	reservedLapsed, err := count(ctx, s, "SELECT COUNT(*) AS n FROM fault_notifications WHERE state = ? AND (lease_until IS NULL OR lease_until <= ?)", "reserved", moment)
	if err != nil {
		return nil, err
	}
	notifications = append(notifications, contract.Field{Key: "reservedLapsed", Value: reservedLapsed})
	claimedN, _ := col(claimed, "n").(int64)
	claimedLapsed, _ := col(claimed, "lapsed").(int64)
	unsent := append(pending, contract.OrderedObject{
		{Key: "claimed", Value: claimedN}, {Key: "claimedLapsed", Value: claimedLapsed},
		{Key: "issued", Value: states["issued"]}, {Key: "issuedLapsed", Value: issuedLapsed},
		{Key: "failed", Value: states["failed"]}, {Key: "uncertain", Value: states["uncertain"]},
	}...)
	var sum int64
	var parts []string
	for _, field := range unsent {
		value := field.Value.(int64)
		if field.Key != "claimedLapsed" && field.Key != "issued" {
			sum += value
		}
		if value != 0 {
			parts = append(parts, fmt.Sprintf("%d %s", value, field.Key))
		}
	}
	doubtful := get(notifications, "uncertain").(int64) + reservedLapsed
	var warning any
	if sum != 0 || unlinked != 0 || doubtful != 0 {
		if unlinked != 0 {
			parts = append(parts, fmt.Sprintf("%d issue(s) not in their project", unlinked))
		}
		if doubtful != 0 {
			parts = append(parts, fmt.Sprintf("%d notification(s) uncertain", doubtful))
		}
		warning = "fault writes need attention: " + strings.Join(parts, ", ")
	}
	return contract.OrderedObject{{Key: "unsent", Value: unsent}, {Key: "unlinked", Value: unlinked},
		{Key: "notifications", Value: notifications}, {Key: "warning", Value: warning}}, nil
}

// heldReason is FaultLedger._held_reason for the built-in kinds.
func heldReason(ctx context.Context, s *store.Store, row store.Row, moment float64) (string, error) {
	kind := colText(row, "kind")
	spec, known := builtInKinds[kind]
	if !known {
		return "kind_unregistered", nil
	}
	if next, ok := pyNumber(col(row, "next_attempt_at")); ok && col(row, "next_attempt_at") != nil && next > moment {
		return "backing_off", nil
	}
	fault, err := s.One(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", col(row, "fault_id"))
	if err != nil {
		return "", err
	}
	if kind == "open_record" && truthy(col(fault, "external_ref")) {
		return "issue_owned", nil
	}
	if spec.requiresIssue && !truthy(col(fault, "external_ref")) {
		return "awaiting_record", nil
	}
	product, scope := colText(fault, "product"), colText(fault, "scope_key")
	if spec.target != "" {
		target, err := s.One(ctx, "SELECT t.tracker_ref, p.project_ref, p.product FROM fault_targets t  LEFT JOIN fault_target_projects p ON p.scope_key = t.scope_key WHERE t.scope_key = ?", scope)
		if err != nil {
			return "", err
		}
		if target == nil || col(target, "product") == nil {
			return "awaiting_target", nil
		}
		if colText(target, "product") != product {
			return "scope_key_contested", nil
		}
		contested, err := s.One(ctx, "SELECT 1 FROM fault_ledger WHERE scope_key = ? AND product != ? LIMIT 1", scope, product)
		if err != nil {
			return "", err
		}
		if contested != nil {
			return "scope_key_contested", nil
		}
		if spec.target == "team+project" && !truthy(col(target, "project_ref")) {
			return "awaiting_target", nil
		}
	}
	limit, window := defaultLimits[kind][0], defaultLimits[kind][1]
	override, err := s.One(ctx, "SELECT max_count, window_seconds FROM fault_limits WHERE product = ? AND kind = ?", product, kind)
	if err != nil {
		return "", err
	}
	if override != nil {
		limit, _ = pyNumber(col(override, "max_count"))
		window, _ = pyNumber(col(override, "window_seconds"))
	}
	used, err := count(ctx, s, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product = ? AND kind = ? AND used_ts > ?", product, kind, moment-window)
	if err != nil {
		return "", err
	}
	if limit-float64(used) <= 0 {
		return "budget_spent", nil
	}
	return "", nil
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
