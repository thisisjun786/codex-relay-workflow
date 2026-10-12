package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The reasons a managed start refuses a parent whose declared settings the live thread does not bear out. They are
// answers of the start (state refused), not errors: nothing is recorded under a refusal, and the same request goes
// through once the declaration and the thread agree.
const (
	// ReasonParentSettingsDiffer: the host read the parent thread and its model or reasoning effort is not the declared one.
	ReasonParentSettingsDiffer = "parent_settings_differ_from_thread"
	// ReasonParentSettingsUnobservable: the host did not give the parent's live model and effort (no such thread, an answer
	// without the fields), so the declaration is not believed.
	ReasonParentSettingsUnobservable = "parent_settings_unobservable"
)

// parentCheck is what the check of the parent's declared settings left on a start: the live values when it passed, the
// observation the refusal carries when it did not.
type parentCheck struct {
	live     *liveParent
	observed contract.OrderedObject
}

// liveParent is the model and reasoning effort the host reports for the parent thread. effort is nil for a thread that
// runs without one.
type liveParent struct {
	model  string
	effort any
}

func (l liveParent) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "model", Value: l.model}, {Key: "reasoningEffort", Value: l.effort}}
}

// declaredPair is the model and effort a request's parent settings declare (the request requires both as nonblank text).
func declaredPair(settings map[string]any) (model any, effort any) {
	model = settings["model"]
	if text, ok := settings["reasoningEffort"].(string); ok && text != "" {
		effort = text
	}
	return model, effort
}

func declaredObject(settings map[string]any) contract.OrderedObject {
	model, effort := declaredPair(settings)
	return contract.OrderedObject{{Key: "model", Value: model}, {Key: "reasoningEffort", Value: effort}}
}

// readLiveParent reads the parent thread's settings with thread/read. The answer is a refusal reason and the detail
// that goes with it when the host answers but not with a model and an effort field, or says the thread cannot be read;
// an error that is not the host's own answer (the transport, the caller's cancellation) is returned as it is, like
// any other failed host call of the start.
func (m *Start) readLiveParent(ctx context.Context, task string) (live liveParent, reason, problem string, err error) {
	answer, err := m.Adapter.HostCall(ctx, "thread/read", map[string]any{"threadId": task, "includeTurns": false})
	if err != nil {
		if ctx.Err() != nil {
			return liveParent{}, "", "", ctx.Err()
		}
		var rpc *appserver.RPCError
		if errors.As(err, &rpc) || unreadable(err) {
			return liveParent{}, ReasonParentSettingsUnobservable, "thread/read refused the parent thread: " + err.Error(), nil
		}
		return liveParent{}, "", "", err
	}
	thread := pyjson.Map(answer["thread"])
	if thread == nil {
		return liveParent{}, ReasonParentSettingsUnobservable, "thread/read answered no thread object", nil
	}
	model, ok := thread["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return liveParent{}, ReasonParentSettingsUnobservable, "thread/read answered the parent thread without a model", nil
	}
	rawEffort, present := thread["reasoningEffort"]
	if !present {
		return liveParent{}, ReasonParentSettingsUnobservable, "thread/read answered the parent thread without the reasoningEffort field", nil
	}
	live = liveParent{model: model}
	switch effort := rawEffort.(type) {
	case nil:
	case string:
		if effort != "" {
			live.effort = effort
		}
	default:
		return liveParent{}, ReasonParentSettingsUnobservable, fmt.Sprintf("thread/read answered a reasoningEffort that is not text (%T)", rawEffort), nil
	}
	return live, "", "", nil
}

// checkParentSettings compares the parent settings a request declares with the live parent thread. It answers the
// live values when they are the declared ones, and otherwise the refusal reason with the observation that explains it
// (the declared and the live values), which the refused answer carries.
func (m *Start) checkParentSettings(ctx context.Context, req map[string]any) (*liveParent, string, contract.OrderedObject, error) {
	parent := pyjson.Map(req["parent"])
	task := pyjson.Text(parent["taskId"])
	declared := pyjson.Map(parent["settings"])
	live, reason, problem, err := m.readLiveParent(ctx, task)
	if err != nil {
		return nil, "", nil, err
	}
	observed := func(live any, extra ...contract.Field) contract.OrderedObject {
		out := contract.OrderedObject{{Key: "taskId", Value: task}, {Key: "declared", Value: declaredObject(declared)}, {Key: "live", Value: live}}
		return append(out, extra...)
	}
	if reason != "" {
		return nil, reason, contract.OrderedObject{{Key: "parentSettings", Value: observed(nil, contract.Field{Key: "problem", Value: problem})}}, nil
	}
	declaredModel, declaredEffort := declaredPair(declared)
	var differing []any
	if declaredModel != live.model {
		differing = append(differing, "model")
	}
	if declaredEffort != live.effort {
		differing = append(differing, "reasoningEffort")
	}
	if len(differing) > 0 {
		return nil, ReasonParentSettingsDiffer, contract.OrderedObject{{Key: "parentSettings", Value: observed(live.object(), contract.Field{Key: "differing", Value: differing})}}, nil
	}
	return &live, "", nil, nil
}

// recordedParentSettings is the parent settings authorized_settings keeps: the declaration with the model and the
// effort the host confirmed (the check has found them equal to the declared ones, so the record is the same value as the
// declaration for every later start of the same parent).
func recordedParentSettings(declared map[string]any, live *liveParent) map[string]any {
	if live == nil {
		return declared
	}
	out := make(map[string]any, len(declared)+1)
	for key, value := range declared {
		out[key] = value
	}
	out["model"] = live.model
	out["reasoningEffort"] = live.effort
	return out
}
