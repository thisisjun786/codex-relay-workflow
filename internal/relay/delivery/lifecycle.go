package delivery

import (
	"context"
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
	CanAcceptInput *bool
	Deliverable    string
	WithholdReason any
	Detail         string
}

func (l Lifecycle) MaySend() bool { return l.Deliverable == "yes" }
func (l Lifecycle) IsBusy() bool  { return l.Deliverable == "busy" }

// Observe is lifecycle.observe: read the host, decide, and never guess.
func Observe(adapter Adapter, task string, cwd any, requireEvidence bool) Lifecycle {
	var runtime, goal any
	var archived, accepts *bool
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
	switch {
	case archived != nil && *archived:
		return decide("no", RecipientArchived)
	case blockingGoalStatus[goalText] != "":
		return decide("no", blockingGoalStatus[goalText])
	case accepts != nil && !*accepts:
		return decide("no", RecipientCannotAccept)
	case runtime == "systemError":
		return decide("no", RecipientSystemError)
	case runtime == "active":
		return decide("busy", RecipientBusy)
	case len(problems) > 0 && requireEvidence:
		return decide("unknown", LifecycleUnknown)
	case archived == nil && requireEvidence:
		return decide("unknown", LifecycleUnknown)
	case runtime == "idle" || runtime == "notLoaded":
		return decide("yes", nil)
	case runtime == nil && !requireEvidence:
		return decide("yes", nil)
	}
	return decide("unknown", LifecycleUnknown)
}

func boolInt(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return int64(1)
	}
	return int64(0)
}

// RecordLifecycle is lifecycle.record.
func RecordLifecycle(ctx context.Context, s *store.Store, clock Clock, l Lifecycle) error {
	return s.Transaction(ctx, func(ctx context.Context, _ *sqlConn) error {
		_, err := execSQL(ctx, s, `INSERT INTO recipient_lifecycle (task_id, runtime_status, archived, goal_status, can_accept_input, deliverable, withhold_reason, detail, observed_at) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET runtime_status=excluded.runtime_status, archived=excluded.archived, goal_status=excluded.goal_status, can_accept_input=excluded.can_accept_input, deliverable=excluded.deliverable, withhold_reason=excluded.withhold_reason, detail=excluded.detail, observed_at=excluded.observed_at`,
			l.TaskID, l.RuntimeStatus, boolInt(l.Archived), l.GoalStatus, boolInt(l.CanAcceptInput), l.Deliverable, l.WithholdReason, l.Detail, clock.ISO())
		return err
	})
}
