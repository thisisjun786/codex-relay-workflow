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

// withheldRecord is the record of an attempt that sent nothing and may be retried: the fields every
// such record carries, plus extra.
func withheldRecord(requestID, id string, attemptNo int64, extra map[string]any) string {
	record := map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true}
	for key, value := range extra {
		record[key] = value
	}
	return pyjson.Dumps(record, pyjson.Options{SortKeys: true})
}

// settleWithheld ends the claimed attempt before its transport started: the attempt row is settled as
// withheld before the send with record, and the message goes back to queued, due at next when it is set.
func (c *Channel) settleWithheld(ctx context.Context, requestID, id string, attemptNo int64, at, owner, record string, next sql.NullFloat64) error {
	if err := c.Store.SettleSupervisorAttempt(ctx, requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
		return err
	}
	_, err := c.Store.SettleSupervisorMessage(ctx, id, "queued", next, sql.NullString{}, at, "sending", attemptNo, sql.NullString{String: owner, Valid: true})
	return err
}

func (c *Channel) cancelTransport(ctx context.Context, id, requestID string, attemptNo int64, at string, next float64, reason, owner string) error {
	var eligible sql.NullFloat64
	if next != 0 {
		eligible = sql.NullFloat64{Float64: next, Valid: true}
	}
	return c.settleWithheld(ctx, requestID, id, attemptNo, at, owner, withheldRecord(requestID, id, attemptNo, map[string]any{"reason": reason}), eligible)
}

func (c *Channel) holdUnaddressed(ctx context.Context, row store.SupervisorMessagesRow, refusal Refusal, now float64) error {
	at := delivery.ISOOf(now)
	if c.clockISO != nil {
		at = c.clockISO()
	}
	return c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason='"+store.SupervisorHoldUnaddressed+"',updated_at=? WHERE message_id=? AND hold_reason IS NULL AND "+store.SupervisorUnsentSQL(""), at, row.MessageID)
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

// oldestAhead names the oldest message to recipient, other than id, that goes ahead of the one
// staged at (stagedAt, id): staged earlier, and satisfying ahead, the SQL condition (binding now
// twice) of the vantage that asks. Attempt asks it before any host work with
// store.SupervisorAheadSQL, and the claim asks it again under its write lock with
// store.SupervisorAheadInClaimSQL, because another writer can make an older message claimable in
// between. found is false when nothing goes first.
func (c *Channel) oldestAhead(ctx context.Context, recipient, stagedAt, id, ahead string, now float64) (older string, found bool, err error) {
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT message_id FROM supervisor_messages WHERE recipient_task_id=? AND message_id<>? AND "+ahead+" AND "+store.SupervisorOlderThanSQL("")+" ORDER BY staged_at,message_id LIMIT 1", recipient, id, now, now, stagedAt, stagedAt, id).Scan(&older)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return older, err == nil, err
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
		if !current.Unsent() {
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
		older, found, err := c.oldestAhead(tx, r.Recipient, current.StagedAt, id, store.SupervisorAheadInClaimSQL(""), now)
		if err != nil {
			return err
		}
		if found {
			return Refusal{"not_claimable", "message " + pyvalue.StrRepr(older) + " was staged for " + pyvalue.StrRepr(r.Recipient) + " first and can be sent now, so this one waits. Two facts reach the level above in the order they arose"}
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
		if err := c.deferMessage(tx, row, row.State, store.SupervisorHoldSuperseded, at, next); err != nil {
			return err
		}
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_superseded',?,?)", at, row.MessageID, pyjson.Dumps(map[string]any{"detail": detail}, pyjson.Options{SortKeys: true}))
		return err
	})
}

// attemptRun is one pass of Channel.attempt over one message. It lives for one call: the retry after
// a restatement builds a new one, so nothing here is carried into it.
type attemptRun struct {
	c        *Channel
	id       string
	adapter  SendAdapter
	now      float64
	owner    string
	row      store.SupervisorMessagesRow // as read before the claim
	r        Resolution                  // the hierarchy now, which the message must still match
	at       string                      // the time of the lifecycle gate's journal entry, the claim and the fence: the injected clock when set, else the call's
	service  *delivery.Service
	settings *delivery.TaskSettings
	claimed  claimedMessage
}

// transportOutcome is what the transport-start transaction decided.
type transportOutcome struct {
	refusal  error // why the claim was cancelled before its transport started, when it was
	started  bool
	restated bool
}

// sentReceipt is what the host answered, classified, and the record of it that settle stores.
type sentReceipt struct {
	facts     delivery.Facts
	result    map[string]any
	encoded   string
	settledAt string
}

// attempt sends one staged message: the steps below in order. A step that can end the attempt returns
// stop, and the attempt then returns no record and that step's error, which may be nil (a quiet
// stop); a step that can only fail returns the error. restatements counts the retries this call has
// made after the proposal was restated at the transport start.
func (c *Channel) attempt(ctx context.Context, id string, adapter SendAdapter, now float64, restatements int, owner string) (map[string]any, error) {
	a := &attemptRun{c: c, id: id, adapter: adapter, now: now, owner: owner}
	if err := a.read(ctx); err != nil {
		return nil, err
	}
	if stop, err := a.eligible(ctx); stop {
		return nil, err
	}
	if err := a.resolve(ctx); err != nil {
		return nil, err
	}
	a.service = delivery.NewService(c.Store, delivery.SystemClock{})
	if stop, err := a.rateGate(ctx); stop {
		return nil, err
	}
	if stop, err := a.lifecycleGate(ctx); stop {
		return nil, err
	}
	if stop, err := a.settingsGate(ctx); stop {
		return nil, err
	}
	if stop, err := a.claimSend(ctx); stop {
		return nil, err
	}
	outcome, err := a.startTransport(ctx)
	if err != nil {
		return nil, err
	}
	if outcome.refusal != nil {
		var refusal Refusal
		if errors.As(outcome.refusal, &refusal) && refusal.Reason == "paced" {
			return nil, nil
		}
		return nil, outcome.refusal
	}
	if outcome.restated {
		if restatements >= 2 {
			return nil, Refusal{"superseded_revision", "the proposal was restated at its transport start three times in one call and nothing was sent; send it again once what it reports has settled"}
		}
		return c.attempt(ctx, id, adapter, now, restatements+1, owner)
	}
	if !outcome.started {
		return nil, nil
	}
	return a.settle(ctx, a.transmit(ctx))
}

// read loads the message, recovers a stranded send and releases a hold the live state no longer
// justifies, so the rest of the attempt sees the row as it stands.
func (a *attemptRun) read(ctx context.Context) error {
	c := a.c
	row, err := c.Get(ctx, a.id)
	if err != nil {
		return err
	}
	row, err = c.recoverStranded(ctx, row, a.now)
	if err != nil {
		return err
	}
	if row.HoldReason.String == store.SupervisorHoldSuperseded {
		if err := c.reopenProposal(ctx, row, delivery.ISOOf(a.now)); err != nil {
			return err
		}
		row, err = c.Get(ctx, a.id)
		if err != nil {
			return err
		}
	}
	if row.HoldReason.String == store.SupervisorHoldUnaddressed {
		if err := c.reopenAddressed(ctx, row, delivery.ISOOf(a.now)); err != nil {
			return err
		}
		row, err = c.Get(ctx, a.id)
		if err != nil {
			return err
		}
	}
	a.row = row
	return nil
}

// eligible stops quietly for a message that cannot be claimed now (held, not due, not unsent) and
// refuses one that an older message to the same recipient goes ahead of.
func (a *attemptRun) eligible(ctx context.Context) (stop bool, err error) {
	row := a.row
	if !row.ClaimableAt(a.now) {
		return true, nil
	}
	older, found, err := a.c.oldestAhead(ctx, row.RecipientTaskID, row.StagedAt, a.id, store.SupervisorAheadSQL(""), a.now)
	if err != nil {
		return true, err
	}
	if found {
		return true, Refusal{"not_claimable", "message " + pyvalue.StrRepr(older) + " was staged for " + pyvalue.StrRepr(row.RecipientTaskID) + " first and can be sent now, so this one waits. Two facts reach the level above in the order they arose, which is not a property an ordered selection can have on its own while any caller may name any row"}
	}
	return false, nil
}

// resolve reads the live hierarchy. A hierarchy that no longer names the message's endpoints holds
// the message as unaddressed and refuses.
func (a *attemptRun) resolve(ctx context.Context) error {
	c, row := a.c, a.row
	r, err := c.Resolve(ctx, row.RelationshipID)
	if err != nil {
		var refusal Refusal
		if errors.As(err, &refusal) {
			if holdErr := c.holdUnaddressed(ctx, row, refusal, a.now); holdErr != nil {
				return holdErr
			}
		}
		return err
	}
	if r.Sender != row.SenderTaskID || r.Recipient != row.RecipientTaskID || r.ProjectKey != row.ProjectKey.String {
		refusal := Refusal{"relation_owner_drift", "this message was staged from " + pyvalue.StrRepr(row.SenderTaskID) + " to " + pyvalue.StrRepr(row.RecipientTaskID) + " and the linkage now says " + pyvalue.StrRepr(r.Sender) + " reports to " + pyvalue.StrRepr(r.Recipient) + "; the hierarchy moved under a staged report, so it is held rather than sent to either. It was never attempted, so staging it again re-addresses it to the live supervisor"}
		if err := c.holdUnaddressed(ctx, row, refusal, a.now); err != nil {
			return err
		}
		return refusal
	}
	a.r = r
	return nil
}

// rateGate defers the message five seconds, without a hold, while the recipient's send rate is spent.
func (a *attemptRun) rateGate(ctx context.Context) (stop bool, err error) {
	c, row := a.c, a.row
	refused, err := c.preflightRate(ctx, a.service, row.RelationshipID, a.r.Recipient, a.now)
	if err != nil {
		return true, err
	}
	if refused != "" {
		return true, c.deferMessage(ctx, row, row.State, "", delivery.ISOOf(a.now), a.now+5)
	}
	return false, nil
}

// lifecycleGate reads and records the recipient's lifecycle. A busy recipient defers the message; a
// recipient that cannot take it withholds the message and journals why.
func (a *attemptRun) lifecycleGate(ctx context.Context) (stop bool, err error) {
	c, row := a.c, a.row
	a.at = delivery.ISOOf(a.now)
	if c.clockISO != nil {
		a.at = c.clockISO()
	}
	lifecycle := delivery.Observe(ctx, a.adapter, a.r.Recipient, nil, true)
	lifecycleNow := a.now
	if c.clockISO != nil {
		if stamp, parseErr := time.Parse("2006-01-02T15:04:05.000000+00:00", a.at); parseErr == nil {
			lifecycleNow = float64(stamp.UnixMicro()) / 1e6
		}
	}
	if err = delivery.RecordLifecycle(ctx, c.Store, &delivery.FakeClock{T: lifecycleNow}, lifecycle); err != nil {
		return true, err
	}
	if lifecycle.IsBusy() {
		return true, c.deferBusy(ctx, row, a.now)
	}
	if !lifecycle.MaySend() {
		if err := c.deferMessage(ctx, row, "withheld_pre_send", "", delivery.ISOOf(a.now), a.now+60); err != nil {
			return true, err
		}
		detail := pyjson.Dumps(contract.OrderedObject{{Key: "deliverable", Value: lifecycle.Deliverable}, {Key: "reason", Value: lifecycle.WithholdReason}}, pyjson.Options{})
		_, err := c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", a.at, a.id, string(detail))
		return true, err
	}
	return false, nil
}

// settingsGate takes the channel's settings or reads the recipient's authorized ones. Settings that
// cannot be read withhold the message and journal why.
func (a *attemptRun) settingsGate(ctx context.Context) (stop bool, err error) {
	c, row := a.c, a.row
	a.settings = c.Settings
	if a.settings != nil {
		return false, nil
	}
	settings, err := c.loadSettings(ctx, a.r.Recipient)
	if err != nil {
		reason := "settings_unavailable"
		if strings.Contains(err.Error(), "role policy") {
			reason = "role_policy_unconfigured"
		}
		if e := c.deferMessage(ctx, row, "withheld_pre_send", "", delivery.ISOOf(a.now), a.now+60); e != nil {
			return true, e
		}
		detailText := err.Error()
		if reason == "role_policy_unconfigured" {
			detailText = pyvalue.StrRepr(a.r.Recipient) + " is bound as 'supervisor' and this process cannot read a role policy to check its authorization against: CODEX_THREAD_BRIDGE_EXECUTION_POLICY is not set in this process, so no role policy can be read. Nothing was sent and no turn was started. Set the policy for this process and the held deliveries resume on the next pass."
		}
		detail := pyjson.Dumps(contract.OrderedObject{{Key: "reason", Value: reason}, {Key: "detail", Value: detailText}}, pyjson.Options{})
		_, e := c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", a.at, a.id, detail)
		return true, e
	}
	a.settings = settings
	return false, nil
}

// claimSend claims the send. A claim that is not available or whose pacing refuses it ends the
// attempt quietly (the pacing defers the message); a claim that found the proposal superseded holds
// the message as superseded and refuses.
func (a *attemptRun) claimSend(ctx context.Context) (stop bool, err error) {
	c, row := a.c, a.row
	claim, err := c.claim(ctx, a.id, a.r, a.now, a.at, a.owner)
	if err != nil {
		if errors.Is(err, errQuietNotClaimable) {
			return true, nil
		}
		var refusal Refusal
		if errors.As(err, &refusal) && refusal.Reason == "paced" {
			return true, c.deferMessage(ctx, row, row.State, "", a.at, a.now+5)
		}
		if errors.As(err, &refusal) && refusal.Reason == "superseded_revision" {
			if holdErr := c.holdObsoleteClaim(ctx, row, a.at, refusal.Detail); holdErr != nil {
				return true, holdErr
			}
		}
		return true, err
	}
	a.claimed = claim
	return false, nil
}

// startTransport runs the hook and then the transaction that fences the claim: it commits the transport
// start, the cancellation of the claim, or nothing when the claim no longer holds the message, and says
// which.
func (a *attemptRun) startTransport(ctx context.Context) (transportOutcome, error) {
	if a.c.beforeTransport != nil {
		a.c.beforeTransport()
	}
	var out transportOutcome
	err := a.c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		return a.fence(tx, &out)
	})
	return out, err
}

// fence is the body of the transport-start transaction. The claim must still be the one that holds
// the message; then the hierarchy, the proposal, the settings and the send budget are each checked
// again, and the first that no longer holds cancels the claim; when all hold the transport starts.
func (a *attemptRun) fence(tx context.Context, out *transportOutcome) error {
	current, err := a.c.Get(tx, a.id)
	if err != nil {
		return err
	}
	if current.State != "sending" || current.AttemptCount != a.claimed.attemptNo || current.LeaseOwner.String != a.owner {
		return nil
	}
	if done, err := a.fenceHierarchy(tx, out); done {
		return err
	}
	if done, err := a.fenceProposal(tx, current, out); done {
		return err
	}
	if done, err := a.fenceSettings(tx); done {
		return err
	}
	if done, err := a.fenceBudget(tx, current, out); done {
		return err
	}
	transportAt := delivery.ISOOf(a.now)
	if a.c.clockISO != nil {
		transportAt = a.c.clockISO()
	}
	if err := a.c.Store.StartSupervisorTransport(tx, a.claimed.requestID, transportAt); err != nil {
		return err
	}
	out.started = true
	return nil
}

// fenceHierarchy cancels the claim when the hierarchy moved between the claim and the transport.
func (a *attemptRun) fenceHierarchy(tx context.Context, out *transportOutcome) (done bool, err error) {
	c, row, requestID, attemptNo := a.c, a.row, a.claimed.requestID, a.claimed.attemptNo
	live, err := c.Resolve(tx, row.RelationshipID)
	if err == nil && live != a.r {
		err = Refusal{"relation_owner_drift", "message " + pyvalue.StrRepr(a.id) + " names " + pyvalue.StrRepr(row.SenderTaskID) + " reporting to " + pyvalue.StrRepr(row.RecipientTaskID) + " for project " + pyvalue.StrRepr(row.ProjectKey.String) + ", and the linkage now says " + pyvalue.StrRepr(live.Sender) + " reports to " + pyvalue.StrRepr(live.Recipient) + " for project " + pyvalue.StrRepr(live.ProjectKey)}
	}
	if err == nil {
		return false, nil
	}
	var refusal Refusal
	if !errors.As(err, &refusal) {
		return true, err
	}
	out.refusal = Refusal{refusal.Reason, "the hierarchy this message names moved after its send was claimed and before its transport started: " + refusal.Detail + ". Nothing was sent, and the attempt is recorded as sending nothing, so staging it again re-addresses it to whoever the linkage names then"}
	reason := "the hierarchy moved between the claim and the transport"
	record := withheldRecord(requestID, a.id, attemptNo, map[string]any{"reason": reason, "refusal": refusal.Reason, "detail": refusal.Detail})
	if err := c.settleWithheld(tx, requestID, a.id, attemptNo, a.at, a.owner, record, sql.NullFloat64{}); err != nil {
		return true, err
	}
	_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", a.at, a.id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "reason", Value: reason}, {Key: "refusal", Value: refusal.Reason}}, pyjson.Options{}))
	return true, err
}

// fenceProposal cancels the claim when what the message reports moved between the claim and the
// transport: a proposal that is gone holds the message as superseded, one that changed is
// restated and the attempt starts over, and any other failure cancels the claim with its own text.
func (a *attemptRun) fenceProposal(tx context.Context, current store.SupervisorMessagesRow, out *transportOutcome) (done bool, err error) {
	c, requestID, attemptNo := a.c, a.claimed.requestID, a.claimed.attemptNo
	changed, err := c.refreshProposal(tx, current, a.r, a.at, attemptNo)
	if err != nil {
		out.refusal = err
		var refusal Refusal
		if errors.As(err, &refusal) && refusal.Reason == "superseded_revision" {
			return true, c.holdSuperseded(tx, a.id, requestID, attemptNo, a.at, refusal.Detail, a.owner)
		}
		return true, c.cancelTransport(tx, a.id, requestID, attemptNo, a.at, 0, err.Error(), a.owner)
	}
	if !changed {
		return false, nil
	}
	out.restated = true
	latest, getErr := c.Get(tx, a.id)
	if getErr != nil {
		return true, getErr
	}
	detail := "message " + pyvalue.StrRepr(a.id) + " was staged from event " + nullableStringRepr(current.EventID) + " submission " + nullableIntRepr(current.SubmissionNo) + " and what is owed now is event " + nullableStringRepr(latest.EventID) + " submission " + nullableIntRepr(latest.SubmissionNo)
	record := withheldRecord(requestID, a.id, attemptNo, map[string]any{"reason": "what is owed moved between the claim and the transport", "proposal": "restated", "detail": detail})
	if err := c.settleWithheld(tx, requestID, a.id, attemptNo, a.at, a.owner, record, sql.NullFloat64{}); err != nil {
		return true, err
	}
	_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", a.at, a.id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "proposal", Value: "restated"}, {Key: "detail", Value: detail}, {Key: "reason", Value: "restated at the transport start; nothing was sent"}}, pyjson.Options{}))
	return true, err
}

// fenceSettings cancels the claim when the recipient's authorized settings stopped standing or
// changed after the settings gate read them (settings the channel was given are never re-read).
func (a *attemptRun) fenceSettings(tx context.Context) (done bool, err error) {
	c, requestID, attemptNo := a.c, a.claimed.requestID, a.claimed.attemptNo
	if c.Settings != nil {
		return false, nil
	}
	latest, settingsErr := c.loadSettings(tx, a.r.Recipient)
	if settingsErr == nil && sameSettings24(a.settings, latest) {
		return false, nil
	}
	reason := "the recipient's authorized settings changed after this send read them and before its transport started"
	if settingsErr != nil {
		reason = "the recipient's authorized settings stopped standing before this send's transport started: " + settingsErr.Error()
		var refusal Refusal
		if errors.As(settingsErr, &refusal) {
			reason = "the recipient's authorized settings stopped standing before this send's transport started: " + refusal.Detail
		}
	}
	if err := c.cancelTransport(tx, a.id, requestID, attemptNo, a.at, 0, reason, a.owner); err != nil {
		return true, err
	}
	_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_withheld',?,?)", a.at, a.id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "reason", Value: reason}}, pyjson.Options{}))
	return true, err
}

// fenceBudget reserves the send under the recipient's budget; a budget that refuses defers the
// message five seconds and the attempt ends quietly.
func (a *attemptRun) fenceBudget(tx context.Context, current store.SupervisorMessagesRow, out *transportOutcome) (done bool, err error) {
	c, requestID, attemptNo := a.c, a.claimed.requestID, a.claimed.attemptNo
	transportNow := a.now
	if c.clockISO != nil {
		if stamp, parseErr := time.Parse("2006-01-02T15:04:05.000000+00:00", c.clockISO()); parseErr == nil {
			transportNow = max(a.now, float64(stamp.UnixMicro())/1e6)
		}
	}
	refused, err := a.service.ReserveSend(tx, current.RelationshipID, a.r.Recipient, transportNow)
	if err != nil {
		return true, err
	}
	if refused == "" {
		return false, nil
	}
	out.refusal = Refusal{"paced", refused}
	reason := "the recipient's send budget refused this send at its transport start"
	record := withheldRecord(requestID, a.id, attemptNo, map[string]any{"reason": reason, "refusal": refused})
	if err := c.settleWithheld(tx, requestID, a.id, attemptNo, a.at, a.owner, record, sql.NullFloat64{Float64: a.now + 5, Valid: true}); err != nil {
		return true, err
	}
	_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_paced',?,?)", a.at, a.id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "refusal", Value: refused}}, pyjson.Options{}))
	return true, err
}

// transmit hands the frozen message to the host and classifies what came back. An answer that only
// says the host's approval policy cannot be served counts as nothing sent, safe to retry.
func (a *attemptRun) transmit(ctx context.Context) sentReceipt {
	c, requestID := a.c, a.claimed.requestID
	receipt, sendErr := a.adapter.SendMessage(ctx, requestID, a.r.Recipient, a.claimed.message, a.settings)
	facts := delivery.Classify(receipt)
	if sendErr != nil {
		facts = delivery.Classify(delivery.Obj{{Key: "requestId", Value: requestID}, {Key: "status", Value: delivery.OutcomeUnknown}, {Key: "error", Value: fmt.Sprintf("%T: %v", sendErr, sendErr)}})
	}
	transportDeliveryState := facts.DeliveryState
	if facts.DeliveryState == delivery.InboxOnly {
		facts.DeliveryState = delivery.WithheldPreSend
		facts.RetrySafe = true
	}
	settledAt := delivery.ISOOf(a.now)
	if c.clockISO != nil {
		settledAt = c.clockISO()
	}
	result := map[string]any{"schema": channelVersion, "requestId": requestID, "messageId": a.id, "attemptNo": a.claimed.attemptNo, "recipientTaskId": a.r.Recipient, "deliveryState": facts.DeliveryState, "sendAttempted": facts.SendAttempted, "retrySafe": facts.RetrySafe, "transportReceiptStatus": facts.ReceiptStatus, "failedOperation": facts.FailedOperation, "turnId": facts.TurnID, "observedAt": settledAt}
	if transportDeliveryState != facts.DeliveryState {
		result["transportDeliveryState"] = transportDeliveryState
		result["reason"] = "the recipient's thread reported an approval policy this transport cannot serve; nothing was started or stored for it, and the message is attempted again after its backoff"
	}
	return sentReceipt{facts: facts, result: result, encoded: pyjson.Dumps(result, pyjson.Options{SortKeys: true, Unicode: true}), settledAt: settledAt}
}

// settle records the host's answer on the attempt and moves the message: a withheld or busy answer
// schedules the next try (and holds the message once the attempts are capped), anything else leaves
// the message where the answer put it. A claim that no longer holds the message keeps its receipt on
// the attempt and leaves the message alone.
func (a *attemptRun) settle(ctx context.Context, sent sentReceipt) (map[string]any, error) {
	c, id, requestID, attemptNo, owner, now := a.c, a.id, a.claimed.requestID, a.claimed.attemptNo, a.owner, a.now
	facts, settledAt := sent.facts, sent.settledAt
	err := c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		if err := c.Store.SettleSupervisorAttempt(tx, requestID, facts.DeliveryState, facts.SendAttempted, int64(boolInt(facts.RetrySafe)), nullable(facts.TurnID), sent.encoded, settledAt); err != nil {
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
	sent.result["messageState"] = settled.State
	return sent.result, nil
}
