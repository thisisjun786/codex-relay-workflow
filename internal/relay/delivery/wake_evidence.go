package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// WakeObserved is an append-only relay journal entry, independent of the transient
// delivery_wakes scheduling row. It records one transport attempt, including failures.
const WakeObserved = "delivery_wake_observed"

const wakeSchema = "relay-wake/1"

// WakeIdentity names the configuration under which a wake was attempted. Instance
// identities must change on restart; paths, PIDs alone and a previous wake cannot
// establish the current App Server/service identity. PairSource names the role,
// recorded or exception-authorized route, including the exception identity if any.
type WakeIdentity struct {
	OperatingScope    string `json:"operatingScope"`
	AppServerInstance string `json:"appServerInstance"`
	Socket            string `json:"socket"`
	ServiceInstance   string `json:"serviceInstance"`
	ApprovalPolicy    string `json:"approvalPolicy"`
	RolePolicy        string `json:"rolePolicy"`
	Role              string `json:"role"`
	PairSource        string `json:"pairSource"`
	TransportRevision string `json:"transportRevision"`
	Disposition       string `json:"disposition"`
}

func (i WakeIdentity) complete() bool {
	for _, s := range []string{i.OperatingScope, i.AppServerInstance, i.Socket, i.ServiceInstance,
		i.ApprovalPolicy, i.RolePolicy, i.Role, i.PairSource, i.TransportRevision, i.Disposition} {
		if strings.TrimSpace(s) == "" {
			return false
		}
	}
	return true
}

// WakeAttempt freezes the pre-send observation and the answer to THAT request.
// The sender supplies the actual required settings, never settings reread after
// sending. NewTurn is affirmative host evidence that TurnID is newly opened, not
// an existing active turn that was steered. Unknown outcomes remain recorded but
// cannot prove a wake. Unattended distinguishes a service tick from a manual send.
type WakeAttempt struct {
	RequestID             string          `json:"requestId"`
	RecipientTaskID       string          `json:"recipientTaskId"`
	RecipientStatusBefore string          `json:"recipientStatusBefore"`
	TurnID                string          `json:"turnId"`
	NewTurn               bool            `json:"newTurn"`
	DeliveryMethod        string          `json:"delivery_method"`
	DeliveryState         string          `json:"deliveryState"`
	SendAttempted         string          `json:"sendAttempted"`
	Unattended            bool            `json:"unattended"`
	RequiredSettings      json.RawMessage `json:"requiredSettings"`
	Identity              WakeIdentity    `json:"identity"`
	ObservedAt            string          `json:"observedAt"`

	// Probe also permits a pre-send preparation failure before a recipient exists.
	// Such a row is never proof of a turn.
	Probe  bool   `json:"probe,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type wakeRecord struct {
	Schema  string `json:"schema"`
	StoreID string `json:"storeId"`
	WakeAttempt
}

// RecordWakeAttempt appends inside the caller's transaction when there is one, so
// a sender can settle its transport receipt and retain this evidence atomically.
// It neither updates a prior attempt nor consumes a delivery_wakes row. Repeated
// observations of one request are appended too; an earlier observation is never
// replaced. Missing identity/settings are retained as unmeasured evidence.
func RecordWakeAttempt(ctx context.Context, s *store.Store, attempt WakeAttempt) error {
	missingRecipient := strings.TrimSpace(attempt.RecipientTaskID) == ""
	preparationFailure := attempt.Probe && attempt.Reason != "" &&
		attempt.SendAttempted == "no" && !attempt.NewTurn && attempt.TurnID == ""
	if strings.TrimSpace(attempt.RequestID) == "" || (missingRecipient && !preparationFailure) {
		return fmt.Errorf("wake evidence needs a request and recipient")
	}
	if _, err := time.Parse(time.RFC3339Nano, attempt.ObservedAt); err != nil {
		return fmt.Errorf("wake observation time: %w", err)
	}
	var storeID string
	if err := s.Q(ctx).QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='store_id'`).Scan(&storeID); err != nil {
		return err
	}
	data, err := json.Marshal(wakeRecord{Schema: wakeSchema, StoreID: storeID, WakeAttempt: attempt})
	if err != nil {
		return fmt.Errorf("encode wake evidence: %w", err)
	}
	return journal(ctx, s, WakeObserved, attempt.RequestID, string(data), attempt.ObservedAt)
}

// WakeExpectation is independently observed CURRENT configuration, not copied
// from the wake being judged. RequiredStatus defaults to notLoaded because an
// ordinary wait can evict the recipient; idle is allowed only for a bounded wait
// whose caller has established that eviction cannot occur.
type WakeExpectation struct {
	Identity         WakeIdentity
	RequiredSettings json.RawMessage
	RequiredStatus   string
}

// WakeEvidence identifies the lasting row and its observed new turn.
type WakeEvidence struct {
	Sequence int64
	WakeAttempt
}

func settingsObject(raw json.RawMessage, policy string) (map[string]any, bool) {
	decoded, err := registry.DecodeJSON(string(raw))
	data, ok := decoded.(Obj)
	if err != nil || !ok || (registry.TaskSettings{Data: data}).RequireUsable() != nil {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || len(obj) == 0 || obj["approvalPolicy"] != policy {
		return nil, false
	}
	return obj, true
}

// FindWakeEvidence reads only append-only wake observations. It never promotes a
// pending wake, latest lifecycle row, accepted request without a turn, or evidence
// from another configuration. reason explains an unmeasured result; err means the
// evidence could not be read. The same function serves the owner and a matched probe.
func FindWakeEvidence(ctx context.Context, s *store.Store, want *WakeExpectation, now time.Time) (*WakeEvidence, string, error) {
	if want == nil || !want.Identity.complete() {
		return nil, "current wake identity has not been independently observed", nil
	}
	settings, ok := settingsObject(want.RequiredSettings, want.Identity.ApprovalPolicy)
	if !ok {
		return nil, "current required settings and approval policy are unmeasured", nil
	}
	status := want.RequiredStatus
	if status == "" {
		status = "notLoaded"
	}
	if status != "idle" && status != "notLoaded" {
		return nil, "required wake lifecycle must be idle or notLoaded", nil
	}
	var storeID string
	if err := s.Q(ctx).QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='store_id'`).Scan(&storeID); err != nil {
		return nil, "", err
	}
	rows, err := s.Q(ctx).QueryContext(ctx, `SELECT seq, at, subject, detail FROM journal WHERE kind = ? ORDER BY seq DESC`, WakeObserved)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var at, subject, detail string
		if err := rows.Scan(&seq, &at, &subject, &detail); err != nil {
			return nil, "", err
		}
		var record wakeRecord
		if err := json.Unmarshal([]byte(detail), &record); err != nil {
			return nil, "", fmt.Errorf("wake journal row %d: %w", seq, err)
		}
		observed, err := time.Parse(time.RFC3339Nano, record.ObservedAt)
		if err != nil || observed.After(now) || record.ObservedAt != at ||
			record.Schema != wakeSchema || record.StoreID != storeID || record.Identity != want.Identity ||
			record.RequestID != subject || strings.TrimSpace(subject) == "" ||
			strings.TrimSpace(record.RecipientTaskID) == "" || strings.TrimSpace(record.TurnID) == "" ||
			!record.NewTurn || record.DeliveryMethod != "turn/start" || !record.Unattended || record.DeliveryState != Dispatched ||
			record.SendAttempted != "yes" || record.RecipientStatusBefore != status {
			continue
		}
		gotSettings, valid := settingsObject(record.RequiredSettings, record.Identity.ApprovalPolicy)
		if valid && reflect.DeepEqual(settings, gotSettings) {
			return &WakeEvidence{Sequence: seq, WakeAttempt: record.WakeAttempt}, "", nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return nil, "no matching unattended " + status + "-to-new-turn wake in the relay journal", nil
}
