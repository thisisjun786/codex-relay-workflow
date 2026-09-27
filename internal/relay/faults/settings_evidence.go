package faults

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func field(object contract.OrderedObject, key string) any {
	for _, item := range object {
		if item.Key == key {
			return item.Value
		}
	}
	return nil
}
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
	return registry.SettingsHoldReading(&registry.HoldColumns{State: text(row, "sh_state"), HoldReason: text(row, "sh_hold_reason"), SettledSent: text(row, "sh_settled_sent"), SettledReconciled: text(row, "sh_settled_reconciled"), Presend: text(row, "sh_presend"), Request: text(row, "sh_request"), SettingsAt: text(row, "sh_settings_at"), LifecycleAt: text(row, "sh_lifecycle_at"), InactiveAt: text(row, "sh_inactive_at")}), nil
}
func quoteCommand(parts []string) string {
	for i, p := range parts {
		if p == "" {
			parts[i] = "''"
			continue
		}
		safe := true
		for _, r := range p {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
				safe = false
				break
			}
		}
		if !safe {
			parts[i] = "'" + strings.ReplaceAll(p, "'", `'"'"'`) + "'"
		}
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
		if found, ok := field(reading, "hold").(contract.OrderedObject); ok {
			candidate := objectMap(found)
			source, _ := candidate["source"].(string)
			if request == "" || source == "undetermined" || source == "attempt" && candidate["requestId"] == request {
				hold = candidate
				kind, _ = field(reading, "kind").(string)
			}
		}
	}
	if hold != nil {
		reason, _ := hold["reason"].(string)
		source, _ := hold["source"].(string)
		observed := map[string]any{"reason": hold["reason"], "field": hold["field"], "source": source, "kind": kind, "current": true}
		chosen := registry.SettingsHoldRecovery(kind, reason, source, false)
		directory, err := filepath.Abs(filepath.Dir(sw.Store.Path))
		if err != nil {
			return nil, "", err
		}
		command := []string{"--state", directory, "show", "--event", event}
		if field(chosen, "command") != registry.ShowEvent && recipient != "" {
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
		recovery := map[string]any{"actor": field(chosen, "actor"), "reason": recoveredReason, "command": rendered, "then": field(chosen, "then"), "laterDeliveries": field(chosen, "laterDeliveries"), "refusalDetail": hold["detail"]}
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
