package faults

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func objectMap(object contract.OrderedObject) map[string]any {
	m := map[string]any{}
	for _, item := range object {
		m[item.Key] = item.Value
	}
	return m
}
func currentSettingsHold(ctx context.Context, s *store.Store, event string) (contract.OrderedObject, error) {
	row, err := s.One(ctx, "SELECT "+registry.SettingsHoldColumns("d.event_id", "d")+" FROM deliveries d WHERE d.event_id=?", event)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return registry.SettingsHoldReading(nil), nil
	}
	return registry.SettingsHoldReading(&registry.HoldColumns{State: row.Text("sh_state"), HoldReason: row.Text("sh_hold_reason"), SettledSent: row.Text("sh_settled_sent"), SettledReconciled: row.Text("sh_settled_reconciled"), Presend: row.Text("sh_presend"), Request: row.Text("sh_request"), SettingsAt: row.Text("sh_settings_at"), LifecycleAt: row.Text("sh_lifecycle_at"), InactiveAt: row.Text("sh_inactive_at")}), nil
}
func quoteCommand(parts []string) string {
	for i, p := range parts {
		parts[i] = quote.Shell(p)
	}
	return strings.Join(parts, " ")
}
func (sw *Sweeper) settingsEvidence(ctx context.Context, event, recipient, request string, own map[string]any, current bool) ([]any, string, error) {
	var hold map[string]any
	kind := ""
	if current {
		reading, err := currentSettingsHold(ctx, sw.Store, event)
		if err != nil {
			return nil, "", err
		}
		if found, ok := reading.Get("hold").(contract.OrderedObject); ok {
			candidate := objectMap(found)
			source, _ := candidate["source"].(string)
			if request == "" || source == "undetermined" || source == "attempt" && candidate["requestId"] == request {
				hold = candidate
				kind, _ = reading.Get("kind").(string)
			}
		}
	}
	if hold != nil {
		reason, _ := hold["reason"].(string)
		source, _ := hold["source"].(string)
		observed := map[string]any{"reason": hold["reason"], "field": hold["field"], "source": source, "kind": kind, "current": true}
		chosen := registry.SettingsHoldRecovery(kind, reason, source, false)
		directory, err := store.StoreDirectory(sw.Store.Path) // store_directory
		if err != nil {
			return nil, "", err
		}
		command := []string{"--state", directory, "show", "--event", event}
		if chosen.Get("command") != registry.ShowEvent && recipient != "" {
			command = []string{"--state", directory, "settings-show", "--task", recipient}
		}
		program := registry.RelayProgram()
		if sw.Program != nil {
			program = sw.Program()
		}
		rendered := quoteCommand(append(append([]string(nil), program...), command...))
		recoveredReason := reason
		if recoveredReason == "" {
			recoveredReason = "undetermined"
		}
		recovery := map[string]any{"actor": chosen.Get("actor"), "reason": recoveredReason, "command": rendered, "then": chosen.Get("then"), "laterDeliveries": chosen.Get("laterDeliveries"), "refusalDetail": hold["detail"]}
		if reason == "" {
			reason = "undetermined"
		}
		return []any{evidence("settings", "deliveries:"+event, observed), evidence("recovery", "deliveries:"+event, recovery)}, reason, nil
	}
	if own != nil {
		reason, _ := own["reason"].(string)
		return []any{evidence("settings", "deliveries:"+event, map[string]any{"reason": reason, "field": own["field"], "source": "attempt", "current": false})}, reason, nil
	}
	return nil, "", nil
}
