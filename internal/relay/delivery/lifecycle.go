package delivery

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Withhold reasons (lifecycle.py).
const (
	RecipientArchived      = "recipient_archived"
	RecipientPaused        = "recipient_paused"
	RecipientUsageLimited  = "recipient_usage_limited"
	RecipientBudgetLimited = "recipient_budget_limited"
	RecipientCannotAccept  = "recipient_cannot_accept_input"
	RecipientBusy          = "recipient_busy"
	RecipientSystemError   = "recipient_system_error"
	LifecycleUnknown       = "lifecycle_unknown"
)

var blockingGoalStatus = map[string]string{"paused": RecipientPaused, "usageLimited": RecipientUsageLimited, "budgetLimited": RecipientBudgetLimited}

// Lifecycle is lifecycle.Lifecycle: what the host says about a recipient right now.
type Lifecycle struct {
	TaskID         string
	RuntimeStatus  any
	Archived       *bool
	GoalStatus     any
	CanAcceptInput any
	Deliverable    string
	WithholdReason any
	Detail         string
}

func (l Lifecycle) MaySend() bool { return l.Deliverable == "yes" }
func (l Lifecycle) IsBusy() bool  { return l.Deliverable == "busy" }

// Observe is lifecycle.observe: read the host, decide, and never guess.
func Observe(adapter Adapter, task string, cwd any, requireEvidence bool) Lifecycle {
	var runtime, goal, accepts any
	var archived *bool
	var problems []string
	if facts, err := adapter.ReadThread(task); err != nil {
		problems = append(problems, "thread read failed: "+errorLabel(err))
	} else {
		runtime, accepts = facts.RuntimeStatus, facts.CanAcceptInput
	}
	if value, err := adapter.IsArchived(task, cwd); err != nil {
		problems = append(problems, "archived check failed: "+errorLabel(err))
	} else {
		archived = value
	}
	if value, err := adapter.ReadGoalStatus(task); err != nil {
		problems = append(problems, "goal read failed: "+errorLabel(err))
	} else {
		goal = value
	}
	detail := strings.Join(problems, "; ")
	decide := func(state string, reason any) Lifecycle {
		return Lifecycle{task, runtime, archived, goal, accepts, state, reason, detail}
	}
	goalText, _ := goal.(string)
	// Existing in-process hosts return *bool; the wire adapter preserves JSON values.
	if p, ok := accepts.(*bool); ok {
		accepts = nil
		if p != nil {
			accepts = *p
		}
	}
	switch {
	case archived != nil && *archived:
		return decide("no", RecipientArchived)
	case blockingGoalStatus[goalText] != "":
		return decide("no", blockingGoalStatus[goalText])
	case isBool(accepts, false):
		return decide("no", RecipientCannotAccept)
	case isText(runtime, "systemError"):
		return decide("no", RecipientSystemError)
	case isText(runtime, "active"):
		return decide("busy", RecipientBusy)
	case len(problems) > 0 && requireEvidence:
		return decide("unknown", LifecycleUnknown)
	case archived == nil && requireEvidence:
		return decide("unknown", LifecycleUnknown)
	case isText(runtime, "idle") || isText(runtime, "notLoaded"):
		return decide("yes", nil)
	case runtime == nil && !requireEvidence:
		return decide("yes", nil)
	}
	return decide("unknown", LifecycleUnknown)
}

// Host JSON fields remain untyped until their consumer needs a scalar. Typed comparisons make
// Python's "container == scalar is false" rule explicit and cannot compare two interfaces whose
// dynamic values are maps or slices.
func isText(value any, expected string) bool {
	text, ok := value.(string)
	return ok && text == expected
}

func isBool(value any, expected bool) bool {
	boolean, ok := value.(bool)
	return ok && boolean == expected
}

func boolInt(value any) (any, error) {
	if p, ok := value.(*bool); ok {
		if p == nil {
			return nil, nil
		}
		value = *p
	}
	switch v := value.(type) {
	case nil:
		return nil, nil
	case bool:
		if v {
			return int64(1), nil
		}
		return int64(0), nil
	case int, int64:
		return v, nil
	case float64:
		return sqliteFloatInt(v)
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			f, err := v.Float64()
			if err != nil {
				return nil, err
			}
			return sqliteFloatInt(f)
		}
		n, ok := new(big.Int).SetString(string(v), 10)
		if !ok {
			return nil, &hostError{"ValueError", "invalid literal for int() with base 10: " + store.PyRepr(string(v))}
		}
		if !n.IsInt64() {
			return nil, &hostError{"OverflowError", "Python int too large to convert to SQLite INTEGER"}
		}
		return n.Int64(), nil
	case string:
		return sqliteIntString(v)
	}
	kind := "dict"
	if _, ok := value.([]any); ok {
		kind = "list"
	}
	return nil, &hostError{"TypeError", "int() argument must be a string, a bytes-like object or a real number, not '" + kind + "'"}
}

func sqliteFloatInt(value float64) (int64, error) {
	if math.IsNaN(value) {
		return 0, &hostError{"ValueError", "cannot convert float NaN to integer"}
	}
	if math.IsInf(value, 0) {
		return 0, &hostError{"OverflowError", "cannot convert float infinity to integer"}
	}
	integer, _ := new(big.Float).SetFloat64(value).Int(nil)
	if !integer.IsInt64() {
		return 0, &hostError{"OverflowError", "Python int too large to convert to SQLite INTEGER"}
	}
	return integer.Int64(), nil
}

// RecordLifecycle is lifecycle.record.
func RecordLifecycle(ctx context.Context, s *store.Store, clock Clock, l Lifecycle) error {
	archived, err := boolInt(l.Archived)
	if err != nil {
		return err
	}
	accepts, err := boolInt(l.CanAcceptInput)
	if err != nil {
		return err
	}
	return s.Transaction(ctx, func(ctx context.Context, _ *sqlConn) error {
		_, err := execSQL(ctx, s, `INSERT INTO recipient_lifecycle (task_id, runtime_status, archived, goal_status, can_accept_input, deliverable, withhold_reason, detail, observed_at) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET runtime_status=excluded.runtime_status, archived=excluded.archived, goal_status=excluded.goal_status, can_accept_input=excluded.can_accept_input, deliverable=excluded.deliverable, withhold_reason=excluded.withhold_reason, detail=excluded.detail, observed_at=excluded.observed_at`,
			l.TaskID, l.RuntimeStatus, archived, l.GoalStatus, accepts, l.Deliverable, l.WithholdReason, l.Detail, clock.ISO())
		return err
	})
}
