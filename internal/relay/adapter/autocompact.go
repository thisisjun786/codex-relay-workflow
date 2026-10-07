package adapter

import (
	bridgesettings "github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// autoCompactLimit is the model_auto_compact_token_limit a resume of this record carries.
//
// The host never reports the value back, so it is not part of a task's recorded settings, and a
// resume built from that record alone would drop it: a child created under a pair that declares a
// limit would return to the host's own (larger) window the first time it was unloaded and resumed.
// The relay's delivery and correction sends build their parameters from the record, so the limit is
// resolved here instead, from the policy, by the pair the record states.
//
// A record that cites an exception, names no role, or names a role the policy does not declare
// sends none: an exception and a supervisor name no pair, and the policy file is the only place a
// limit is written. Where the role lists several pairs the record's own pair says which one it is,
// and a pair that declares no limit sends none rather than inheriting a sibling pair's.
func (a *Adapter) autoCompactLimit(record *delivery.TaskSettings) *int64 {
	if a.bridge == nil || record == nil {
		return nil
	}
	if cited, _ := record.Data.Lookup("citedException"); cited != nil {
		return nil
	}
	named, _ := record.Data.Lookup("citedRole")
	role, _ := named.(string)
	if role == "" {
		return nil
	}
	declared, ok := a.bridge.Policy.Role(role)
	if !ok {
		return nil
	}
	model, _ := record.Data.Lookup("model")
	effort, _ := record.Data.Lookup("reasoningEffort")
	modelText, _ := model.(string)
	effortText, _ := effort.(string)
	for _, pair := range declared.Pairs {
		if pair.Model == modelText && pair.Effort == effortText {
			return pair.AutoCompactTokenLimit
		}
	}
	return nil
}

// withAutoCompactLimit adds the record's pair limit to a resume's parameters. It is additive: the
// config object a resume already carries keeps every key it had, and a record whose pair declares no
// limit adds nothing.
func (a *Adapter) withAutoCompactLimit(record *delivery.TaskSettings, params map[string]any) {
	limit := a.autoCompactLimit(record)
	if limit == nil {
		return
	}
	config, _ := params["config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	config[bridgesettings.AutoCompactTokenLimitKey] = *limit
	params["config"] = config
	record.AutoCompactLimitTransmitted = true
}
