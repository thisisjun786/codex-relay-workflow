package adapter

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// dumps delegates to the existing shared Python encoder. Todo 24 moves this
// implementation from supervisor to evidence; this import is the only migration point.
func dumps(value any, sorted bool) string {
	return pyjson.Dumps(value, pyjson.Options{SortKeys: sorted})
}

// receiptObject preserves the guarded-send's Python insertion order. Ledger.save
// returns a copy, so updatedAt is appended after the operation's fields. Nested values are ordered
// at the RPC boundary and are not decoded into maps before being persisted.
func receiptObject(r ledger.Receipt) contract.OrderedObject {
	keys := []string{"requestId", "operation", "status", "startedAt", "retrySafe", "fingerprintVersion", "attempt", "priorAttempts", "threadId", "statusBeforeResume", "resumed", "settingsFreeResume", "settingsFindings", "settingsNotes", "turnId", "attemptedEffects", "error", "rpcError", "updatedAt", "replayed"}
	if r["operation"] == "create_thread" {
		keys = []string{"requestId", "operation", "status", "startedAt", "retrySafe", "fingerprintVersion", "attempt", "priorAttempts", "executionPolicy", "threadId", "creation", "settings", "title", "turnId", "desktopProjectAssociation", "error", "rpcError", "attemptedEffects", "updatedAt", "settingsAfterDispatch", "replayed"}
	}
	o := contract.OrderedObject{}
	seen := map[string]bool{}
	for _, k := range keys {
		if v, ok := r[k]; ok {
			if k == "priorAttempts" {
				rows, _ := v.([]any)
				history := make([]any, 0, len(rows))
				for _, row := range rows {
					var prior map[string]any
					switch value := row.(type) {
					case ledger.Receipt:
						prior = value
					case map[string]any:
						prior = value
					}
					history = append(history, contract.OrderedObject{{Key: "status", Value: prior["status"]}, {Key: "error", Value: prior["error"]}, {Key: "updatedAt", Value: prior["updatedAt"]}})
				}
				v = history
			}
			o = append(o, contract.Field{Key: k, Value: bridgeValue(k, v)})
			seen[k] = true
		}
	}
	rest := []string{}
	for k := range r {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	slicesSort(rest)
	for _, k := range rest {
		o = append(o, contract.Field{Key: k, Value: r[k]})
	}
	return o
}
func encodeReceipt(r ledger.Receipt) ([]byte, error) {
	return []byte(dumps(receiptObject(r), false)), nil
}
func (a *Adapter) receipt(ctx context.Context, id string, replay bool) (contract.OrderedObject, error) {
	raw, err := a.ledger.RawReceipt(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := registry.DecodeJSON(string(raw))
	if err != nil {
		return nil, err
	}
	o, ok := v.(contract.OrderedObject)
	if !ok {
		return nil, &HostUnavailable{"ledger receipt is not an object"}
	}
	if replay {
		o = append(o, contract.Field{Key: "replayed", Value: true})
	}
	return o, nil
}

// callValue retains every JSON shape and object insertion order. Each caller
// applies the Python operation it actually performs (verification, get or index).
func (a *Adapter) callValue(ctx context.Context, method string, params map[string]any) (any, error) {
	raw, err := a.rpc.Call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return registry.DecodeJSON(string(raw))
}
func (a *Adapter) callOrdered(ctx context.Context, method string, params map[string]any) (contract.OrderedObject, error) {
	v, err := a.callValue(ctx, method, params)
	if err != nil {
		return nil, err
	}
	o, ok := v.(contract.OrderedObject)
	if !ok {
		return nil, &HostUnavailable{"host response is not an object"}
	}
	return o, nil
}
func errorText(err error) string {
	if typed, ok := err.(interface{ PythonExceptionKind() string }); ok {
		return typed.PythonExceptionKind() + ": " + err.Error()
	}
	name := fmt.Sprintf("%T", err)
	if host, ok := err.(*HostUnavailable); ok {
		return "HostUnavailable: " + host.Message
	}
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			name = name[i+1:]
			break
		}
	}
	return name + ": " + err.Error()
}

// bridgeValue supplies insertion order to map-shaped values the in-process bridge
// returns. This is a boundary conversion, not a second settings implementation.
func bridgeValue(key string, value any) any {
	if findings, ok := value.([]settings.Finding); ok {
		out := make([]any, 0, len(findings))
		for _, f := range findings {
			out = append(out, contract.OrderedObject{{Key: "code", Value: f.Code}, {Key: "field", Value: f.Field}, {Key: "expected", Value: bridgeValue("expected", f.Expected)}, {Key: "returned", Value: bridgeValue("returned", f.Returned)}})
		}
		return out
	}
	m, ok := value.(map[string]any)
	if !ok {
		return value
	}
	var keys []string
	switch key {
	case "executionPolicy":
		keys = []string{"mode", "digest", "exception", "role", "roleExpectation", "model", "reasoningEffort", "limits"}
	case "roleExpectation":
		keys = []string{"role", "expectation", "model", "reasoningEffort", "overriddenBy"}
	case "settings":
		keys = []string{"requested", "actual", "verified", "unobservable", "findings", "verification", "observationLimits"}
	case "requested":
		keys = []string{"cwd", "model", "reasoningEffort", "runtimeWorkspaceRoots", "sandbox"}
	case "actual":
		keys = []string{"approvalPolicy", "cwd", "model", "reasoningEffort", "runtimeWorkspaceRoots", "sandbox"}
	case "sandbox", "expected", "returned":
		if m["type"] == "readOnly" {
			keys = []string{"networkAccess", "type"}
		}
		if m["type"] == "workspaceWrite" {
			keys = []string{"writableRoots", "networkAccess", "excludeTmpdirEnvVar", "excludeSlashTmp", "type"}
		}
	case "rpcError":
		keys = []string{"code", "message"}
	}
	if keys == nil {
		return ordered(m)
	}
	out := contract.OrderedObject{}
	seen := map[string]bool{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out = append(out, contract.Field{Key: k, Value: bridgeValue(k, v)})
			seen[k] = true
		}
	}
	rest := []string{}
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	slicesSort(rest)
	for _, k := range rest {
		out = append(out, contract.Field{Key: k, Value: bridgeValue(k, m[k])})
	}
	return out
}
