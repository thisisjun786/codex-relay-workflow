package supervisor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"math"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const readbackLimits = "a verified readback says this attempt's delivery token is in a named turn of the recipient's thread - the turn it names, or one that turn follows - and that the named turn did not certainly begin before this attempt's transport started. It does not say that turn answered or who computed the proof: the proof is two identifiers this store holds, and when the named turn is the one the send opened (turnOrigin relay_opened) no act of the recipient's is needed at all, so it shows arrival rather than reading. The turn and the transcript are read by separate host calls with no shared snapshot, which is sound for an append-only history and blind to one rewritten between them"

func Proof(messageID, turnID string) string {
	sum := sha256.Sum256([]byte(messageID + "|" + turnID))
	return hex.EncodeToString(sum[:])
}
func establishes(verified string, origin any) string {
	if verified != "host_read" {
		return "nothing: this readback did not verify"
	}
	evidence.HashKey(origin) // Python's ESTABLISHES.get rejects unhashable keys.
	switch origin {
	case "relay_opened":
		return "arrival only: the turn this send opened holds this attempt's delivery token, and no act of the recipient's was needed"
	case "recipient_opened":
		return "a turn the recipient's thread opened after the send, and this attempt's delivery token in its transcript - not who wrote the answer or that anybody read or acted"
	}
	return "a verified turn whose origin the host did not establish"
}
func readDetail(r store.SupervisorReadbacksRow) map[string]any {
	if !r.Detail.Valid || r.Detail.String == "" {
		return map[string]any{}
	}
	value := evidence.Decode(r.Detail.String)
	object := evidence.Dict(value, false)
	for key, v := range object {
		object[key] = projectStateValue(v)
	}
	return object
}
func readAnswer(r store.SupervisorReadbacksRow, recorded, raced bool) map[string]any {
	detail := readDetail(r)
	origin := detail["turnOrigin"]
	return map[string]any{"schema": channelVersion, "messageId": r.MessageID, "recorded": recorded, "raced": raced, "verified": r.Verified, "readTurnId": r.ReadTurnID, "turnOrigin": detail["turnOrigin"], "detail": detail["detail"], "delivered": detail["delivered"], "readAt": r.ReadAt, "assertedBy": detail["assertedBy"], "reconciled": detail["reconciled"], "establishes": establishes(r.Verified, origin), "limits": readbackLimits}
}
func (c *Channel) ReadBack(ctx context.Context, id, turnID, proof, assertedBy string, adapter SendAdapter, now float64) (map[string]any, error) {
	row, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	row, err = c.recoverStranded(ctx, row, now)
	if err != nil {
		return nil, err
	}
	if assertedBy != "" && assertedBy != row.RecipientTaskID {
		return nil, Refusal{"recipient_not_authorized", store.PyRepr(assertedBy) + " is not the recipient of this message, which is " + store.PyRepr(row.RecipientTaskID) + ". The declaration is checked against the row; it is not evidence of who is calling, and nothing on this side could be"}
	}
	if row.State != "dispatched" && row.State != "held_uncertain" && row.State != "read" {
		return nil, Refusal{"not_claimable", "message " + store.PyRepr(id) + " is " + store.PyRepr(row.State) + "; only a message that was sent, or whose send nobody heard back from, is read back, because otherwise there is nothing yet to have read"}
	}
	if turnID == "" {
		return nil, Refusal{"malformed_receipt", "a readback names the turn it was written in; without one there is nothing to check against the host"}
	}
	settled, err := c.Store.SupervisorReadback(ctx, id)
	if err == nil && settled.Verified == "host_read" {
		return readAnswer(settled, false, false), nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if proof != Proof(id, turnID) {
		return nil, Refusal{"ack_proof_mismatch", "the proof does not match this message and turn. It is sha256(messageId|your own turn id), and quoting the delivered fields back cannot produce it"}
	}
	attempts, err := c.Store.SupervisorAttempts(ctx, id)
	if err != nil {
		return nil, err
	}
	var attempt *store.SupervisorAttemptsRow
	for i := len(attempts) - 1; i >= 0; i-- {
		if row.State == "held_uncertain" && attempts[i].AttemptNo == row.AttemptCount || row.State != "held_uncertain" && attempts[i].State == "dispatched" {
			attempt = &attempts[i]
			break
		}
	}
	if row.State == "held_uncertain" && attempt == nil {
		return nil, Refusal{"not_claimable", "message " + store.PyRepr(id) + " is held uncertain with no attempt " + fmt.Sprint(row.AttemptCount) + " to read it back against, so there is no token to look for"}
	}
	origin := "unknown"
	if attempt != nil && attempt.TurnID.Valid {
		origin = "recipient_opened"
		if attempt.TurnID.String == turnID {
			origin = "relay_opened"
		}
	}
	verified, detail := "host_read", "the host lists this turn on the recipient's thread and it did not begin before the send"
	var readTurn *delivery.TurnInfo
	if adapter == nil {
		verified, detail = "unverified_turn", "no host adapter in this process, so whether this turn is real was not established"
	} else {
		readTurn, err = adapter.ReadTurn(row.RecipientTaskID, turnID)
		if err != nil {
			verified, detail = "unverified_turn", "the turn could not be read: "+err.Error()
		} else if readTurn == nil {
			verified, detail = "turn_not_found", "the host has no such turn on this thread"
		} else if readTurn.StartedAt == nil || math.IsNaN(*readTurn.StartedAt) || math.IsInf(*readTurn.StartedAt, 0) {
			verified, detail = "unverified_turn", "the host gave no start time for this turn that is a time, and an unknown chronology is not a verification"
		} else if attempt == nil || !attempt.TransportStartedAt.Valid {
			verified, detail = "unverified_turn", "this attempt has no recorded transport start, so there is no send to measure the turn against"
		} else if stamp, parseErr := time.Parse("2006-01-02T15:04:05.000000+00:00", attempt.TransportStartedAt.String); parseErr != nil {
			verified, detail = "unverified_turn", "this attempt has no recorded transport start, so there is no send to measure the turn against"
		} else if *readTurn.StartedAt+1.0 <= float64(stamp.UnixMicro())/1e6 {
			verified, detail = "turn_predates_send", "this turn began before the send, so it cannot be the turn that read it"
		}
	}
	delivered := map[string]any{"scanned": false, "reason": "no host adapter in this process, so the transcript was not read"}
	if adapter != nil {
		delivered = map[string]any{"scanned": false, "reason": "no dispatched attempt, so there is no token to look for"}
		if attempt != nil && attempt.DeliveryToken.Valid {
			scan, e := adapter.FindToken(row.RecipientTaskID, attempt.DeliveryToken.String, 200, false)
			if e != nil {
				delivered = map[string]any{"scanned": false, "reason": "the transcript could not be read: " + e.Error()}
			} else {
				delivered = map[string]any{"scanned": true, "found": scan.Found, "turnId": func() any {
					if scan.TurnID == "" {
						return nil
					}
					return scan.TurnID
				}(), "exhausted": scan.Exhausted, "itemsRead": scan.Scanned, "token": attempt.DeliveryToken.String}
			}
		}
	}
	if verified == "host_read" && delivered["found"] != true {
		verified = "transcript_unconfirmed"
		reason, _ := delivered["reason"].(string)
		if reason == "" {
			reason = fmt.Sprintf("scanned %v items and did not find %v", delivered["itemsRead"], delivered["token"])
		}
		detail = "the turn is real and the transcript does not confirm the message: " + reason
	}
	if verified == "host_read" && delivered["turnId"] == nil {
		verified = "transcript_unconfirmed"
		detail = "the transcript holds this attempt's request id and the host did not say which turn it is in, so it is not tied to the named turn or to any turn that turn follows"
	}
	if verified == "host_read" && origin == "relay_opened" && delivered["turnId"] != turnID {
		verified = "transcript_turn_mismatch"
		detail = "this readback names the turn the send opened, " + turnID + ", and the delivered token is in " + fmt.Sprint(delivered["turnId"]) + "; where the message landed is where a reader of it reads"
	}
	if verified == "host_read" && delivered["turnId"] != nil && delivered["turnId"] != turnID && origin != "relay_opened" && readTurn != nil && readTurn.StartedAt != nil {
		landed := fmt.Sprint(delivered["turnId"])
		turn, readErr := adapter.ReadTurn(row.RecipientTaskID, landed)
		if readErr != nil || turn == nil || turn.StartedAt == nil || math.IsNaN(*turn.StartedAt) || math.IsInf(*turn.StartedAt, 0) {
			verified, detail = "unverified_turn", "the turn this message landed in, "+landed+", has no start time the host would give, so whether "+turnID+" followed it is not established"
		} else if *readTurn.StartedAt+1.0 <= *turn.StartedAt {
			verified, detail = "turn_predates_send", turnID+" began before "+landed+", the turn this message landed in, so it cannot be the turn that read it"
		}
	}
	if verified == "host_read" && delivered["turnId"] != nil && delivered["turnId"] != turnID && (attempt == nil || !attempt.TurnID.Valid || delivered["turnId"] != attempt.TurnID.String) {
		landed := fmt.Sprint(delivered["turnId"])
		turn, readErr := adapter.ReadTurn(row.RecipientTaskID, landed)
		var started time.Time
		var parseErr error
		if attempt == nil || !attempt.TransportStartedAt.Valid {
			parseErr = errors.New("missing transport start")
		} else {
			started, parseErr = time.Parse("2006-01-02T15:04:05.000000+00:00", attempt.TransportStartedAt.String)
		}
		if readErr != nil || turn == nil || turn.StartedAt == nil || math.IsNaN(*turn.StartedAt) || math.IsInf(*turn.StartedAt, 0) || parseErr != nil {
			verified, detail = "unverified_turn", "the turn this attempt's delivery token is in, "+landed+", has no start time the host would give, so whether it came after this send is not established"
		} else if *turn.StartedAt+1.0 <= float64(started.UnixMicro())/1e6 {
			verified, detail = "turn_predates_send", "this attempt's delivery token is in "+landed+", a turn that began before this attempt's transport started, so it was there before the send and is not evidence the send arrived"
		}
	}
	if verified == "host_read" && row.State == "held_uncertain" && attempt != nil && attempt.TransportStartedAt.Valid {
		holder := fmt.Sprint(delivered["turnId"])
		var holderTurn *delivery.TurnInfo
		var holderErr error
		if holder == turnID {
			holderTurn = readTurn
		} else {
			holderTurn, holderErr = adapter.ReadTurn(row.RecipientTaskID, holder)
		}
		started, parseErr := time.Parse("2006-01-02T15:04:05.000000+00:00", attempt.TransportStartedAt.String)
		if holderErr != nil || holderTurn == nil || holderTurn.StartedAt == nil || parseErr != nil || math.IsNaN(*holderTurn.StartedAt) || math.IsInf(*holderTurn.StartedAt, 0) {
			verified, detail = "unverified_turn", "the turn this attempt's delivery token is in, "+holder+", has no start time the host would give, so whether it followed this send is not established, and an uncertain send is settled only when it is"
		} else if *holderTurn.StartedAt < float64(started.UnixMicro())/1e6 {
			verified, detail = "turn_predates_send", "this attempt's delivery token is in "+holder+", which began before this attempt's transport started. A send nobody heard back from is settled only on a token in a turn that began at or after that instant, without the allowance a turn's start is otherwise given"
		}
	}
	var reconciled any
	if verified == "host_read" && row.State == "held_uncertain" && attempt != nil {
		reconciled = map[string]any{"from": "held_uncertain", "by": "readback", "requestId": attempt.RequestID, "attemptNo": attempt.AttemptNo, "deliveredTurnId": delivered["turnId"]}
		detail += "; the send's own response was never heard, and this attempt's request id in the recipient's transcript is what settles it"
	}
	asserted := assertedBy
	if asserted == "" {
		asserted = "undeclared"
	}
	var request sql.NullString
	if attempt != nil {
		request = sql.NullString{String: attempt.RequestID, Valid: true}
	}
	at := delivery.ISOOf(now)
	data := map[string]any{"turnOrigin": origin, "detail": detail, "delivered": delivered, "assertedBy": asserted, "reconciled": reconciled}
	encoded := evidence.Dumps(data, false, true, false)
	var answer map[string]any
	err = c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		existing, e := c.Store.SupervisorReadback(tx, id)
		if e == nil && existing.Verified == "host_read" {
			answer = readAnswer(existing, false, true)
			return nil
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		current, e := c.Get(tx, id)
		if e != nil {
			return e
		}
		if current.State != row.State || current.AttemptCount != row.AttemptCount {
			return Refusal{"not_claimable", "message " + store.PyRepr(id) + " moved while this readback was being checked, so the checks describe a message that is no longer there. Nothing was recorded; answer again"}
		}
		record := store.SupervisorReadbacksRow{MessageID: id, ReadTurnID: turnID, Proof: proof, Verified: verified, RequestID: request, Detail: sql.NullString{String: encoded, Valid: true}, ReadAt: at}
		if e = c.Store.RecordSupervisorReadback(tx, record); e != nil {
			return e
		}
		if verified == "host_read" {
			_, e = c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET state='read',updated_at=? WHERE message_id=? AND state=? AND attempt_count=?", at, id, row.State, row.AttemptCount)
			if e != nil {
				return e
			}
		}
		if reconciled != nil {
			journal := evidence.Dumps(contract.OrderedObject{{Key: "from", Value: "held_uncertain"}, {Key: "by", Value: "readback"}, {Key: "requestId", Value: attempt.RequestID}, {Key: "attemptNo", Value: attempt.AttemptNo}, {Key: "deliveredTurnId", Value: delivered["turnId"]}, {Key: "readTurnId", Value: turnID}}, false, false, true)
			if _, e = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_reconciled',?,?)", at, id, string(journal)); e != nil {
				return e
			}
		}
		readJournal := evidence.Dumps(contract.OrderedObject{{Key: "verified", Value: verified}, {Key: "turnOrigin", Value: origin}, {Key: "readTurnId", Value: turnID}, {Key: "deliveredEvidence", Value: delivered["found"]}, {Key: "reconciled", Value: reconciled != nil}}, false, false, true)
		if _, e = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_read',?,?)", at, id, readJournal); e != nil {
			return e
		}
		answer = readAnswer(record, true, false)
		return nil
	})
	return answer, err
}
