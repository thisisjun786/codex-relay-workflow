package fakehost

import (
	"slices"
	"strings"
)

// Override is one -c override the fake server was started with: a dotted key and its value.
type Override struct {
	Key   string
	Value any
}

// ServerOverrides are the overrides the fake server was started with (codex app-server -c key=value). Every thread the host loads
// from then on builds its override layer from them and then from the loading request's config (overrideLayer).
func (t *Threads) ServerOverrides(overrides ...Override) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.serverOver = slices.Clone(overrides)
}

// overrideLayer is the override layer of Codex 0.154 (app-server config_manager.rs load_with_cli_overrides, config overrides.rs
// build_cli_overrides_layer): the server's overrides, then the request's config entries, each applied in turn by its dotted key. The
// key is walked segment by segment, a missing or non-table step made a table, and the last segment's value inserted, replacing what
// was there: a table value replaces the table, a leaf key changes that leaf only. The request's keys are applied in sorted order (the
// host's map has none; the keys a test sends do not overlap).
func overrideLayer(server []Override, request map[string]any) map[string]any {
	root := map[string]any{}
	for _, o := range server {
		applyOverride(root, o.Key, cloneValue(o.Value))
	}
	keys := make([]string, 0, len(request))
	for k := range request {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		applyOverride(root, k, cloneValue(request[k]))
	}
	return root
}

func applyOverride(root map[string]any, path string, value any) {
	segments := strings.Split(path, ".")
	current := root
	for _, seg := range segments[:len(segments)-1] {
		next, ok := current[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[seg] = next
		}
		current = next
	}
	current[segments[len(segments)-1]] = value
}

// cloneValue copies the maps and slices of a JSON value, so a layer never shares them with a request or a reader.
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cloneValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	}
	return v
}

// cloneLayer is a copy of a layer, nil for none.
func cloneLayer(layer map[string]any) map[string]any {
	if layer == nil {
		return nil
	}
	return cloneValue(layer).(map[string]any)
}
