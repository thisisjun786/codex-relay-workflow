package supervisor

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SendAdapter is the shared delivery adapter: no second host transport is introduced.
type SendAdapter = delivery.Adapter

// tokenSource permits deterministic capture replay; production uses crypto/rand.
var TokenSource io.Reader = rand.Reader

func (c *Channel) render(p Packet, requestID, token string) string {
	region := evidence.Item(map[string]any(p), "envelope")
	field := func(key string) any { return evidence.Item(region, key) }
	text := func(key string) string { return pyvalue.Str(field(key)) }
	lines := []string{"[codex-session-relay] supervisor report", "requestId: " + requestID}
	if token != "" {
		lines = append(lines, "deliveryToken: "+token)
	}
	owed := "an answer is owed by the recipient"
	if evidence.IsAbsent(field("answerOwedBy")) {
		owed = "no answer is owed"
	} else if pyvalue.ItemEqual(field("answerOwedBy"), "user") {
		owed = "a decision is owed by the user, not by the recipient"
	}
	lines = append(lines, "  message: "+text("kind")+" - "+owed, "  messageId: "+text("messageId")+"  "+text("direction")+"/"+text("purpose")+"  envelope: "+text("version"))
	sender, recipient := field("sender"), field("recipient")
	lines = append(lines, "  relation: "+text("relationId")+"  basis: "+evidence.Shown(field("basis")), "  from: "+pyvalue.Str(evidence.Item(sender, "role"))+" "+evidence.Shown(evidence.Item(sender, "taskId"))+"  to: "+pyvalue.Str(evidence.Item(recipient, "role"))+" "+evidence.Shown(evidence.Item(recipient, "taskId")), "  scope: "+evidence.Shown(field("scope")), "  observedAt: "+evidence.Shown(field("observedAt")))
	for _, pair := range [][2]string{{"relationRevision", "relationRevision"}, {"correlationId", "answering"}} {
		if value := field(pair[0]); !evidence.IsAbsent(value) {
			lines = append(lines, "  "+pair[1]+": "+pyvalue.Str(value))
		}
	}
	for _, pointer := range evidence.Iter(field("evidence")) {
		lines = append(lines, "  evidence: "+pyvalue.Str(pointer))
	}
	for _, key := range []string{"issue", "generation", "criteriaDigest"} {
		value := p[key]
		if value != nil && !evidence.IsAbsent(value) && (pyvalue.Truthy(value) || pyvalue.ItemEqual(value, 0)) {
			lines = append(lines, "  "+key+": "+pyvalue.Str(value))
		}
	}
	if pyvalue.Truthy(p["artifact"]) {
		artifact := evidence.Dict(p["artifact"], false)
		if artifact["kind"] == "pull_request" {
			lines = append(lines, "  pull request: "+pyvalue.Str(artifact["repository"])+" #"+pyvalue.Str(artifact["number"])+" at "+pyvalue.Str(artifact["headSha"]))
		} else {
			lines = append(lines, "  artifact: "+pyvalue.Str(artifact["path"])+" digest "+pyvalue.Str(artifact["digest"]))
		}
	}
	socket := c.Socket
	if socket == "" {
		socket = "YOUR_RELAY_SOCKET"
	}
	head := []string{"--state", c.StoreDirectory(), "--socket", socket, "supervisor-read", "--message", p.ID(), "--turn", "YOUR_TURN_ID", "--proof", "YOUR_PROOF", "--as", evidence.Shown(evidence.Item(recipient, "taskId"))}
	placeholders := []string{"YOUR_TURN_ID with your own turn id,", "YOUR_PROOF with the proof over it."}
	if c.Socket == "" {
		placeholders = append([]string{"YOUR_RELAY_SOCKET with your relay socket path, which these bytes do not know,"}, placeholders...)
	}
	lines = append(lines, "", "To record that this reached your thread, run from inside a turn of your own:", "  "+programCommand(c.Program, head...), "", strconv.Itoa(len(placeholders))+" words on that line are yours to replace: "+strings.Join(placeholders, " "), "Every other argument is filled in and quoted for a POSIX shell, including the", "--state that selects the store this report was staged in; --socket and --state", "are global and go BEFORE the subcommand, and --as is required.", "", "The proof is sha256(messageId|<your own turn id>). This message cannot contain", "that turn id, so quoting it back does not produce the proof - and that is all the", "proof rules out. Anyone holding the relay's store can compute it as well. A", "readback records that this attempt's deliveryToken is in your thread and that the", "turn you name is real there. Where that turn is the one this message opened, it", "records arrival and nothing you did. It never records that you read, agreed to", "or acted on anything.", "", "Full record: "+c.command("supervisor-show", "--message", p.ID()))
	return strings.Join(lines, "\n")
}
func (c *Channel) StoreDirectory() string { return store.PathlibParent(c.Store.Path) }
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func nullable(value any) sql.NullString {
	s, ok := value.(string)
	return sql.NullString{String: s, Valid: ok && s != ""}
}
func (c *Channel) deferMessage(ctx context.Context, row store.SupervisorMessagesRow, state, hold, at string, next float64) error {
	var eligible any
	if next != 0 {
		eligible = next
	}
	var holdValue any
	if hold != "" {
		holdValue = hold
	}
	var previousHold, previousEligible any
	if row.HoldReason.Valid {
		previousHold = row.HoldReason.String
	}
	if row.NextEligibleAt.Valid {
		previousEligible = row.NextEligibleAt.Float64
	}
	_, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE supervisor_messages SET state=?,next_eligible_at=?,hold_reason=?,updated_at=? WHERE message_id=? AND state=? AND hold_reason IS ? AND next_eligible_at IS ? AND attempt_count=? AND recipient_task_id=?", state, eligible, holdValue, at, row.MessageID, row.State, previousHold, previousEligible, row.AttemptCount, row.RecipientTaskID)
	return err
}

func (c *Channel) cancelTransport(ctx context.Context, id, requestID string, attemptNo int64, at string, next float64, reason, owner string) error {
	record := pyjson.Dumps(map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true, "reason": reason}, pyjson.Options{SortKeys: true})
	if err := c.Store.SettleSupervisorAttempt(ctx, requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
		return err
	}
	var eligible sql.NullFloat64
	if next != 0 {
		eligible = sql.NullFloat64{Float64: next, Valid: true}
	}
	_, err := c.Store.SettleSupervisorMessage(ctx, id, "queued", eligible, sql.NullString{}, at, "sending", attemptNo, sql.NullString{String: owner, Valid: true})
	return err
}

func (c *Channel) holdUnaddressed(ctx context.Context, row store.SupervisorMessagesRow, refusal Refusal, now float64) error {
	at := delivery.ISOOf(now)
	if c.clockISO != nil {
		at = c.clockISO()
	}
	return c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved',updated_at=? WHERE message_id=? AND hold_reason IS NULL AND state IN ('queued','deferred_busy','withheld_pre_send')", at, row.MessageID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 1 {
			detail := pyjson.Dumps(contract.OrderedObject{{Key: "refusal", Value: refusal.Reason}, {Key: "detail", Value: refusal.Detail}}, pyjson.Options{})
			_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_unaddressed',?,?)", at, row.MessageID, detail)
		}
		return err
	})
}

// Attempt enforces the live hierarchy, lifecycle and settings gates before the
// claim and again at transport start. The attempt row and frozen bytes commit first.
func (c *Channel) preflightRate(ctx context.Context, service *delivery.Service, relationship, recipient string, now float64) (string, error) {
	if c.skipPreflightRate {
		return "", nil
	}
	return service.SendRefusal(ctx, relationship, recipient, now)
}

func (c *Channel) Attempt(ctx context.Context, id string, adapter SendAdapter, now float64) (map[string]any, error) {
	return c.attempt(ctx, id, adapter, now, 0, "relay")
}

var errQuietNotClaimable = errors.New("supervisor message is not claimable")

type claimedMessage struct {
	requestID, message string
	attemptNo          int64
}

// claim is the transaction-level operation shared by the send and its parity tests.
func (c *Channel) claim(ctx context.Context, id string, r Resolution, now float64, at, owner string) (claimedMessage, error) {
	service := delivery.NewService(c.Store, delivery.SystemClock{})
	var requestID, message string
	var attemptNo int64
	token := ""
	err := c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) (err error) {
		defer evidence.RecoverPython(&err)
		if c.beforeClaimRead != nil {
			c.beforeClaimRead(tx)
		}
		current, err := c.Get(tx, id)
		if err != nil {
			return err
		}
		live, err := c.Resolve(tx, current.RelationshipID)
		if err != nil {
			return err
		}
		if live != r || current.RecipientTaskID != r.Recipient || current.SenderTaskID != r.Sender {
			return errQuietNotClaimable
		}
		if current.State != "queued" && current.State != "deferred_busy" && current.State != "withheld_pre_send" {
			return errQuietNotClaimable
		}
		if changed, err := c.refreshProposal(tx, current, r, at, 0); err != nil {
			return err
		} else if changed {
			current, err = c.Get(tx, id)
			if err != nil {
				return err
			}
		}
		err = c.Store.Q(tx).QueryRowContext(tx, "SELECT message_id FROM supervisor_messages WHERE recipient_task_id=? AND message_id<>? AND hold_reason IS NULL AND ((state IN ('queued','deferred_busy','withheld_pre_send') AND (next_eligible_at IS NULL OR next_eligible_at<=?)) OR (state='sending' AND lease_until>?)) AND (staged_at<? OR (staged_at=? AND message_id<?)) ORDER BY staged_at,message_id LIMIT 1", r.Recipient, id, now, now, current.StagedAt, current.StagedAt, id).Scan(&message)
		if err == nil {
			return Refusal{"not_claimable", "message " + pyvalue.StrRepr(message) + " was staged for " + pyvalue.StrRepr(r.Recipient) + " first and can be sent now, so this one waits. Two facts reach the level above in the order they arose"}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		attemptNo = current.AttemptCount + 1
		requestID = "sup-" + id[:min(12, len(id))] + "-a" + strconv.FormatInt(attemptNo, 10)
		var clash string
		if err := c.Store.Q(tx).QueryRowContext(tx, "SELECT message_id FROM supervisor_attempts WHERE request_id=?", requestID).Scan(&clash); err == nil {
			if clash != id {
				return Refusal{"not_claimable", "request id " + pyvalue.StrRepr(requestID) + " already belongs to message " + pyvalue.StrRepr(clash) + "; two message ids share the prefix this id keeps, so this attempt cannot be told apart from that one"}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET state='sending',lease_owner=?,lease_until=?,attempt_count=attempt_count+1,updated_at=? WHERE message_id=? AND state=? AND attempt_count=?", owner, now+300, at, id, current.State, current.AttemptCount)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return Refusal{"not_claimable", "message changed during its claim"}
		}
		p := Packet(evidence.Dict(evidence.Decode(current.Packet), false))
		// Draw after eligibility and collision checks but before transactional pacing,
		// as Python does: an obsolete claim consumes nothing, a paced claim does.
		tokenBytes := make([]byte, 8)
		if _, err = io.ReadFull(TokenSource, tokenBytes); err != nil {
			return err
		}
		token = requestID + "." + hex.EncodeToString(tokenBytes)
		message = c.render(p, requestID, token)
		if refusal, err := service.SendRefusal(tx, current.RelationshipID, r.Recipient, now); err != nil {
			return err
		} else if refusal != "" {
			return Refusal{"paced", refusal}
		}
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO supervisor_attempts(request_id,message_id,attempt_no,message,state,send_attempted,retry_safe,record,sent_at,observed_at,delivery_token) VALUES(?,?,?,?,'held_uncertain','unknown',0,?,?,?,?)", requestID, id, attemptNo, message, pyjson.Dumps(map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "held_uncertain"}, pyjson.Options{SortKeys: true}), at, at, token)
		return err
	})
	if err != nil {
		return claimedMessage{}, err
	}
	return claimedMessage{requestID, message, attemptNo}, nil
}

func (c *Channel) holdObsoleteClaim(ctx context.Context, row store.SupervisorMessagesRow, at, detail string) error {
	return c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		current, err := c.Get(tx, row.MessageID)
		if err != nil {
			return err
		}
		if current.State != row.State || current.HoldReason != row.HoldReason || current.NextEligibleAt != row.NextEligibleAt || current.AttemptCount != row.AttemptCount {
			return nil
		}
		next := float64(0)
		if row.NextEligibleAt.Valid {
			next = row.NextEligibleAt.Float64
		}
		if err := c.deferMessage(tx, row, row.State, "superseded_by_report", at, next); err != nil {
			return err
		}
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_superseded',?,?)", at, row.MessageID, pyjson.Dumps(map[string]any{"detail": detail}, pyjson.Options{SortKeys: true}))
		return err
	})
}

func (c *Channel) attempt(ctx context.Context, id string, adapter SendAdapter, now float64, restatements int, owner string) (map[string]any, error) {
	row, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	row, err = c.recoverStranded(ctx, row, now)
	if err != nil {
		return nil, err
	}
	if row.HoldReason.String == "superseded_by_report" {
		if err := c.reopenProposal(ctx, row, delivery.ISOOf(now)); err != nil {
			return nil, err
		}
		row, err = c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	if row.HoldReason.String == "hierarchy_unresolved" {
		if err := c.reopenAddressed(ctx, row, delivery.ISOOf(now)); err != nil {
			return nil, err
		}
		row, err = c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	if row.HoldReason.Valid || row.State != "queued" && row.State != "deferred_busy" && row.State != "withheld_pre_send" || row.NextEligibleAt.Valid && row.NextEligibleAt.Float64 > now {
		return nil, nil
	}
	var older string
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT message_id FROM supervisor_messages WHERE recipient_task_id=? AND message_id<>? AND ((state IN ('queued','deferred_busy','withheld_pre_send') AND hold_reason IS NULL AND (next_eligible_at IS NULL OR next_eligible_at<=?)) OR (state='sending' AND lease_until IS NOT NULL AND lease_until>?)) AND (staged_at<? OR (staged_at=? AND message_id<?)) ORDER BY staged_at,message_id LIMIT 1", row.RecipientTaskID, id, now, now, row.StagedAt, row.StagedAt, id).Scan(&older)
	if err == nil {
		return nil, Refusal{"not_claimable", "message " + pyvalue.StrRepr(older) + " was staged for " + pyvalue.StrRepr(row.RecipientTaskID) + " first and can be sent now, so this one waits. Two facts reach the level above in the order they arose, which is not a property an ordered selection can have on its own while any caller may name any row"}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	r, err := c.Resolve(ctx, row.RelationshipID)
	if err != nil {
		var refusal Refusal
		if errors.As(err, &refusal) {
			if holdErr := c.holdUnaddressed(ctx, row, refusal, now); holdErr != nil {
				return nil, holdErr
			}
		}
		return nil, err
	}
	if r.Sender != row.SenderTaskID || r.Recipient != row.RecipientTaskID || r.ProjectKey != row.ProjectKey.String {
		refusal := Refusal{"relation_owner_drift", "this message was staged from " + pyvalue.StrRepr(row.SenderTaskID) + " to " + pyvalue.StrRepr(row.RecipientTaskID) + " and the linkage now says " + pyvalue.StrRepr(r.Sender) + " reports to " + pyvalue.StrRepr(r.Recipient) + "; the hierarchy moved under a staged report, so it is held rather than sent to either. It was never attempted, so staging it again re-addresses it to the live supervisor"}
		if err := c.holdUnaddressed(ctx, row, refusal, now); err != nil {
			return nil, err
		}
		return nil, refusal
	}
	service := delivery.NewService(c.Store, delivery.SystemClock{})
	if refused, err := c.preflightRate(ctx, service, row.RelationshipID, r.Recipient, now); err != nil {
		return nil, err
	} else if refused != "" {
		return nil, c.deferMessage(ctx, row, row.State, "", delivery.ISOOf(now), now+5)
	}
	at := delivery.ISOOf(now)
	if c.clockISO != nil {
		at = c.clockISO()
	}
	lifecycle := delivery.Observe(adapter, r.Recipient, nil, true)
	lifecycleNow := now
	if c.clockISO != nil {
		if stamp, parseErr := time.Parse("2006-01-02T15:04:05.000000+00:00", at); parseErr == nil {
			lifecycleNow = float64(stamp.UnixMicro()) / 1e6
		}
	}
	if err = delivery.RecordLifecycle(ctx, c.Store, &delivery.FakeClock{T: lifecycleNow}, lifecycle); err != nil {
		return nil, err
	}
	if lifecycle.IsBusy() {
		return nil, c.deferBusy(ctx, row, now)
	}
	if !lifecycle.MaySend() {
		if err := c.deferMessage(ctx, row, "withheld_pre_send", "", delivery.ISOOf(now), now+60); err != nil {
			return nil, err
		}
		detail := pyjson.Dumps(contract.OrderedObject{{Key: "deliverable", Value: lifecycle.Deliverable}, {Key: "reason", Value: lifecycle.WithholdReason}}, pyjson.Options{})
		_, err := c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", at, id, string(detail))
		return nil, err
	}
	settings := c.Settings
	if settings == nil {
		settings, err = c.loadSettings(ctx, r.Recipient)
		if err != nil {
			reason := "settings_unavailable"
			if strings.Contains(err.Error(), "role policy") {
				reason = "role_policy_unconfigured"
			}
			if e := c.deferMessage(ctx, row, "withheld_pre_send", "", delivery.ISOOf(now), now+60); e != nil {
				return nil, e
			}
			detailText := err.Error()
			if reason == "role_policy_unconfigured" {
				detailText = pyvalue.StrRepr(r.Recipient) + " is bound as 'supervisor' and this process cannot read a role policy to check its authorization against: CODEX_THREAD_BRIDGE_EXECUTION_POLICY is not set in this process, so no role policy can be read. Nothing was sent and no turn was started. Set the policy for this process and the held deliveries resume on the next pass."
			}
			detail := pyjson.Dumps(contract.OrderedObject{{Key: "reason", Value: reason}, {Key: "detail", Value: detailText}}, pyjson.Options{})
			_, e := c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", at, id, detail)
			return nil, e
		}
	}
	claim, err := c.claim(ctx, id, r, now, at, owner)
	requestID, message, attemptNo := claim.requestID, claim.message, claim.attemptNo
	if err != nil {
		if errors.Is(err, errQuietNotClaimable) {
			return nil, nil
		}
		var refusal Refusal
		if errors.As(err, &refusal) && refusal.Reason == "paced" {
			return nil, c.deferMessage(ctx, row, row.State, "", at, now+5)
		}
		if errors.As(err, &refusal) && refusal.Reason == "superseded_revision" {
			if holdErr := c.holdObsoleteClaim(ctx, row, at, refusal.Detail); holdErr != nil {
				return nil, holdErr
			}
		}
		return nil, err
	}
	if c.beforeTransport != nil {
		c.beforeTransport()
	}
	var transportRefusal error
	transportStarted := false
	restated := false
	err = c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		current, err := c.Get(tx, id)
		if err != nil {
			return err
		}
		if current.State != "sending" || current.AttemptCount != attemptNo || current.LeaseOwner.String != owner {
			return nil
		}
		live, err := c.Resolve(tx, row.RelationshipID)
		if err == nil && live != r {
			err = Refusal{"relation_owner_drift", "message " + pyvalue.StrRepr(id) + " names " + pyvalue.StrRepr(row.SenderTaskID) + " reporting to " + pyvalue.StrRepr(row.RecipientTaskID) + " for project " + pyvalue.StrRepr(row.ProjectKey.String) + ", and the linkage now says " + pyvalue.StrRepr(live.Sender) + " reports to " + pyvalue.StrRepr(live.Recipient) + " for project " + pyvalue.StrRepr(live.ProjectKey)}
		}
		if err != nil {
			var refusal Refusal
			if !errors.As(err, &refusal) {
				return err
			}
			transportRefusal = Refusal{refusal.Reason, "the hierarchy this message names moved after its send was claimed and before its transport started: " + refusal.Detail + ". Nothing was sent, and the attempt is recorded as sending nothing, so staging it again re-addresses it to whoever the linkage names then"}
			reason := "the hierarchy moved between the claim and the transport"
			record := pyjson.Dumps(map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true, "reason": reason, "refusal": refusal.Reason, "detail": refusal.Detail}, pyjson.Options{SortKeys: true})
			if err := c.Store.SettleSupervisorAttempt(tx, requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
				return err
			}
			if _, err := c.Store.SettleSupervisorMessage(tx, id, "queued", sql.NullFloat64{}, sql.NullString{}, at, "sending", attemptNo, sql.NullString{String: owner, Valid: true}); err != nil {
				return err
			}
			_, err := c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", at, id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "reason", Value: reason}, {Key: "refusal", Value: refusal.Reason}}, pyjson.Options{}))
			return err
		}
		if changed, err := c.refreshProposal(tx, current, r, at, attemptNo); err != nil {
			transportRefusal = err
			var refusal Refusal
			if errors.As(err, &refusal) && refusal.Reason == "superseded_revision" {
				return c.holdSuperseded(tx, id, requestID, attemptNo, at, refusal.Detail, owner)
			}
			return c.cancelTransport(tx, id, requestID, attemptNo, at, 0, err.Error(), owner)
		} else if changed {
			restated = true
			latest, getErr := c.Get(tx, id)
			if getErr != nil {
				return getErr
			}
			detail := "message " + pyvalue.StrRepr(id) + " was staged from event " + nullableStringRepr(current.EventID) + " submission " + nullableIntRepr(current.SubmissionNo) + " and what is owed now is event " + nullableStringRepr(latest.EventID) + " submission " + nullableIntRepr(latest.SubmissionNo)
			record := pyjson.Dumps(map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true, "reason": "what is owed moved between the claim and the transport", "proposal": "restated", "detail": detail}, pyjson.Options{SortKeys: true})
			if err := c.Store.SettleSupervisorAttempt(tx, requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
				return err
			}
			if _, err := c.Store.SettleSupervisorMessage(tx, id, "queued", sql.NullFloat64{}, sql.NullString{}, at, "sending", attemptNo, sql.NullString{String: owner, Valid: true}); err != nil {
				return err
			}
			_, err := c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", at, id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "proposal", Value: "restated"}, {Key: "detail", Value: detail}, {Key: "reason", Value: "restated at the transport start; nothing was sent"}}, pyjson.Options{}))
			return err
		}
		if c.Settings == nil {
			latest, settingsErr := c.loadSettings(tx, r.Recipient)
			if settingsErr != nil || !sameSettings24(settings, latest) {
				reason := "the recipient's authorized settings changed after this send read them and before its transport started"
				if settingsErr != nil {
					reason = "the recipient's authorized settings stopped standing before this send's transport started: " + settingsErr.Error()
					var refusal Refusal
					if errors.As(settingsErr, &refusal) {
						reason = "the recipient's authorized settings stopped standing before this send's transport started: " + refusal.Detail
					}
				}
				if err := c.cancelTransport(tx, id, requestID, attemptNo, at, 0, reason, owner); err != nil {
					return err
				}
				_, err := c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", at, id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "reason", Value: reason}}, pyjson.Options{}))
				return err
			}
		}
		transportNow := now
		if c.clockISO != nil {
			if stamp, parseErr := time.Parse("2006-01-02T15:04:05.000000+00:00", c.clockISO()); parseErr == nil {
				transportNow = max(now, float64(stamp.UnixMicro())/1e6)
			}
		}
		if refused, err := service.ReserveSend(tx, current.RelationshipID, r.Recipient, transportNow); err != nil {
			return err
		} else if refused != "" {
			transportRefusal = Refusal{"paced", refused}
			reason := "the recipient's send budget refused this send at its transport start"
			record := pyjson.Dumps(map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true, "reason": reason, "refusal": refused}, pyjson.Options{SortKeys: true})
			if err := c.Store.SettleSupervisorAttempt(tx, requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
				return err
			}
			if _, err := c.Store.SettleSupervisorMessage(tx, id, "queued", sql.NullFloat64{Float64: now + 5, Valid: true}, sql.NullString{}, at, "sending", attemptNo, sql.NullString{String: owner, Valid: true}); err != nil {
				return err
			}
			_, err := c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_paced',?,?)", at, id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "refusal", Value: refused}}, pyjson.Options{}))
			return err
		}
		transportAt := delivery.ISOOf(now)
		if c.clockISO != nil {
			transportAt = c.clockISO()
		}
		if err := c.Store.StartSupervisorTransport(tx, requestID, transportAt); err != nil {
			return err
		}
		transportStarted = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if transportRefusal != nil {
		var refusal Refusal
		if errors.As(transportRefusal, &refusal) && refusal.Reason == "paced" {
			return nil, nil
		}
		return nil, transportRefusal
	}
	if restated {
		if restatements >= 2 {
			return nil, Refusal{"superseded_revision", "the proposal was restated at its transport start three times in one call and nothing was sent; send it again once what it reports has settled"}
		}
		return c.attempt(ctx, id, adapter, now, restatements+1, owner)
	}
	if !transportStarted {
		return nil, nil
	}
	receipt, sendErr := adapter.SendMessage(requestID, r.Recipient, message, settings)
	facts := delivery.Classify(receipt)
	if sendErr != nil {
		facts = delivery.Classify(delivery.Obj{{Key: "requestId", Value: requestID}, {Key: "status", Value: delivery.OutcomeUnknown}, {Key: "error", Value: fmt.Sprintf("%T: %v", sendErr, sendErr)}})
	}
	transportDeliveryState := facts.DeliveryState
	if facts.DeliveryState == delivery.InboxOnly {
		facts.DeliveryState = delivery.WithheldPreSend
		facts.RetrySafe = true
	}
	settledAt := delivery.ISOOf(now)
	if c.clockISO != nil {
		settledAt = c.clockISO()
	}
	result := map[string]any{"schema": channelVersion, "requestId": requestID, "messageId": id, "attemptNo": attemptNo, "recipientTaskId": r.Recipient, "deliveryState": facts.DeliveryState, "sendAttempted": facts.SendAttempted, "retrySafe": facts.RetrySafe, "transportReceiptStatus": facts.ReceiptStatus, "failedOperation": facts.FailedOperation, "turnId": facts.TurnID, "observedAt": settledAt}
	if transportDeliveryState != facts.DeliveryState {
		result["transportDeliveryState"] = transportDeliveryState
		result["reason"] = "the recipient's thread reported an approval policy this transport cannot serve; nothing was started or stored for it, and the message is attempted again after its backoff"
	}
	encoded := pyjson.Dumps(result, pyjson.Options{SortKeys: true, Unicode: true})
	err = c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		if err := c.Store.SettleSupervisorAttempt(tx, requestID, facts.DeliveryState, facts.SendAttempted, int64(boolInt(facts.RetrySafe)), nullable(facts.TurnID), encoded, settledAt); err != nil {
			return err
		}
		state := facts.DeliveryState
		policy := delivery.DefaultPolicy()
		var next sql.NullFloat64
		var hold sql.NullString
		backoffReason := "send"
		if state == "deferred_busy" {
			backoffReason = "busy"
		}
		if state == "withheld_pre_send" || state == "deferred_busy" {
			next = sql.NullFloat64{Float64: now + policy.DelayFor(attemptNo, backoffReason), Valid: true}
			if attemptNo >= policy.CapFor(backoffReason) {
				hold = sql.NullString{String: policy.CapReason(backoffReason), Valid: true}
			}
		}
		moved, err := c.Store.SettleSupervisorMessage(tx, id, state, next, hold, settledAt, "sending", attemptNo, sql.NullString{String: owner, Valid: true})
		if err != nil {
			return err
		}
		standing, err := c.Get(tx, id)
		if err != nil {
			return err
		}
		var reason any
		if !moved {
			reason = "this claim no longer held the message when its receipt arrived, so the receipt is recorded on its attempt and the message is left where it is"
		}
		detail := pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "attemptNo", Value: attemptNo}, {Key: "deliveryState", Value: state}, {Key: "sendAttempted", Value: facts.SendAttempted}, {Key: "turnId", Value: facts.TurnID}, {Key: "holdReason", Value: optionalText(hold)}, {Key: "messageMoved", Value: moved}, {Key: "messageState", Value: standing.State}, {Key: "reason", Value: reason}}, pyjson.Options{})
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_attempted',?,?)", settledAt, id, string(detail))
		return err
	})
	if err != nil {
		return nil, err
	}
	settled, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	result["messageState"] = settled.State
	return result, nil
}
