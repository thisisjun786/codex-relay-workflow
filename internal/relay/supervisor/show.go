package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func optionalText(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}
func (c *Channel) Reach(ctx context.Context, id string) (map[string]any, error) {
	ladder := reachUnmeasured()
	attempts, err := c.Store.SupervisorAttempts(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(attempts) > 0 {
		a := attempts[len(attempts)-1]
		switch a.State {
		case "dispatched":
			ladder["transport_accepted"] = map[string]any{"state": "yes", "source": "supervisor_attempts", "detail": "the transport accepted " + a.RequestID}
		case "withheld_pre_send", "deferred_busy":
			ladder["transport_accepted"] = map[string]any{"state": "no", "source": "supervisor_attempts", "detail": "nothing was sent: " + a.State}
		}
	}
	r, err := c.Store.SupervisorReadback(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if r.Verified == "host_read" {
			origin := readDetail(r)["turnOrigin"]
			ladder["received"] = map[string]any{"state": "yes", "source": "supervisor_readbacks", "detail": "read back from turn " + r.ReadTurnID + " (" + pyvalue.Str(origin) + "): " + establishes(r.Verified, origin)}
		} else {
			ladder["received"] = map[string]any{"state": "unmeasured", "source": nil, "detail": "a readback was recorded and did not verify: " + r.Verified}
		}
	}
	return ladder, nil
}
func (c *Channel) Show(ctx context.Context, id string) (map[string]any, error) {
	row, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	attempts, err := c.Store.SupervisorAttempts(ctx, id)
	if err != nil {
		return nil, err
	}
	attemptRecords := []any{}
	for _, a := range attempts {
		attemptRecords = append(attemptRecords, map[string]any{"requestId": a.RequestID, "attemptNo": a.AttemptNo, "state": a.State, "sendAttempted": a.SendAttempted, "retrySafe": a.RetrySafe != 0, "turnId": optionalText(a.TurnID), "sentAt": a.SentAt, "transportStartedAt": optionalText(a.TransportStartedAt), "observedAt": a.ObservedAt, "message": a.Message})
	}
	var packet any
	if err = json.Unmarshal([]byte(row.Packet), &packet); err != nil {
		return nil, err
	}
	readback, err := c.Store.SupervisorReadback(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var received any
	var origin any
	var establishesRead any
	if err == nil {
		detail := readDetail(readback)
		origin = detail["turnOrigin"]
		received = map[string]any{"readTurnId": readback.ReadTurnID, "verified": readback.Verified, "turnOrigin": origin, "establishes": establishes(readback.Verified, origin), "requestId": optionalText(readback.RequestID), "readAt": readback.ReadAt, "detail": detail}
		if row.State == "read" {
			establishesRead = establishes(readback.Verified, origin)
		}
	}
	ladder, err := c.Reach(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.State != "read" {
		origin = nil
	}
	var stagedFrom any
	if row.Reading.Valid && row.Reading.String != "" {
		reading := evidence.Decode(row.Reading.String)
		if reading != nil {
			// _selectors explicitly rejects non-object readings; they are still
			// returned verbatim as the frozen observation.
			var object map[string]any
			if _, ok := evidence.Object(reading); ok {
				object = projectStateValue(reading).(map[string]any)
			}
			stagedFrom = map[string]any{"reading": projectStateValue(reading), "recheck": observationRecheck(c.Program, object)}
		}
	}
	return map[string]any{"schema": channelVersion, "messageId": id, "obligationId": row.ObligationID, "kind": row.ObligationKind, "relationshipId": row.RelationshipID, "projectKey": optionalText(row.ProjectKey), "purpose": row.Purpose, "sender": row.SenderTaskID, "recipient": row.RecipientTaskID, "state": row.State, "turnOrigin": origin, "readEstablishes": establishesRead, "holdReason": optionalText(row.HoldReason), "stagedAt": row.StagedAt, "packet": packet, "stagedFrom": stagedFrom, "attempts": attemptRecords, "readback": received, "reach": ladder, "limits": "this is what the relay staged, sent and was told. Whether the supervisor acted on it is not here, and what discharges the obligation is still the Linear record it reads for itself, confirmed"}, nil
}
