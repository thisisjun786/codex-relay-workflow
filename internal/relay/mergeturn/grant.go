// Package mergeturn is merge-turn coordination (mergeturn.py).
//
// Subset ported for todo 21; todo 26 owns this package. Only grant_supersession_in and the
// _current_grant_in it reads are here: what a merge-turn grant notice's own turn says about it,
// which delivery asks before a send, in status and in reconciliation. Requests, promotion,
// acknowledgement and every write are todo 26's. (internal/relay/cli/status.go carries todo 20's
// private copy of the same reading; todo 26 folds it onto this one.)
package mergeturn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Why a grant notice has nothing left to tell (mergeturn.py).
const (
	Absent          = "merge_turn_absent"
	Closed          = "merge_turn_closed"
	Regranted       = "merge_turn_regranted"
	GrantAnswered   = "merge_turn_grant_answered"
	GrantUnreadable = "merge_turn_grant_unreadable"
)

var occupying = []string{"holding", "merging", "unknown"}

func text(r store.Row, name string) string {
	switch v := r.Get(name).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

func decode(raw string) map[string]any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// GrantSupersessionFor is DeliveryService._grant_supersession: a grant notice's receipt names
// its turn and grant; "" means the notice is still current.
func GrantSupersessionFor(ctx context.Context, s *store.Store, receipt string) (string, error) {
	envelope := decode(receipt)
	if envelope == nil {
		return GrantUnreadable, nil
	}
	turn, turnOK := envelope["turnId"].(string)
	grant, grantOK := envelope["grantId"].(string)
	if !turnOK || turn == "" || !grantOK || grant == "" {
		return GrantUnreadable, nil
	}
	return GrantSupersessionIn(ctx, s, turn, grant)
}

// GrantSupersessionIn is grant_supersession_in, read on ctx's querier.
func GrantSupersessionIn(ctx context.Context, s *store.Store, turn, grant string) (string, error) {
	row, err := s.One(ctx, "SELECT state, tenure FROM merge_turns WHERE turn_id = ?", turn)
	if err != nil {
		return "", err
	}
	if row == nil {
		return Absent, nil
	}
	answered, err := s.One(ctx, "SELECT 1 FROM merge_turn_ledger WHERE turn_id = ? AND idempotency_key = ?", turn, "grant_acknowledged:"+grant)
	if err != nil {
		return "", err
	}
	if answered != nil {
		return GrantAnswered, nil
	}
	state := text(row, "state")
	isOccupying := false
	for _, o := range occupying {
		isOccupying = isOccupying || o == state
	}
	if !isOccupying {
		return Closed, nil
	}
	current, err := currentGrantIn(ctx, s, turn, row.Get("tenure"))
	if err != nil {
		return "", err
	}
	if current != "" && current != grant {
		return Regranted, nil
	}
	return "", nil
}

// currentGrantIn is MergeTurn._current_grant_in through grant_envelope: the newest well-formed
// grant of this tenure, "" when none is readable.
func currentGrantIn(ctx context.Context, s *store.Store, turn string, tenure any) (string, error) {
	rows, err := s.All(ctx, "SELECT evidence, evidence_kind, idempotency_key FROM merge_turn_ledger  WHERE turn_id = ? AND evidence_kind = ?", turn, "grant")
	if err != nil {
		return "", err
	}
	best, bestSequence := "", int64(math.MinInt64)
	for _, row := range rows {
		envelope := decode(text(row, "evidence"))
		if envelope == nil {
			continue
		}
		sequence, ok := integer(envelope["sequence"])
		if !ok || envelope["turnId"] != turn || !sameNumber(envelope["tenure"], tenure) {
			continue
		}
		sum := sha256.Sum256([]byte(turn + "|" + fmt.Sprint(tenure) + "|" + strconv.FormatInt(sequence, 10)))
		derived := "mtg-" + hex.EncodeToString(sum[:])[:32]
		if envelope["grantId"] != derived || text(row, "idempotency_key") != "grant:"+derived {
			continue
		}
		if r, ok := envelope["recipientTaskId"].(string); !ok || r == "" {
			continue
		}
		if c, ok := envelope["candidateHead"].(string); !ok || c == "" {
			continue
		}
		if sequence > bestSequence {
			best, bestSequence = derived, sequence
		}
	}
	return best, nil
}

func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// sameNumber is Python == between a JSON number and the tenure column.
func sameNumber(a, b any) bool {
	n, ok := a.(json.Number)
	if !ok {
		return a == nil && b == nil
	}
	switch t := b.(type) {
	case int64:
		i, err := n.Int64()
		return err == nil && i == t
	case float64:
		f, err := n.Float64()
		return err == nil && f == t
	}
	return false
}
