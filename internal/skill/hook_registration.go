package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func hookRegistrations(codexHome string, knownEvents map[string]any) (map[string]any, error) {
	candidates := []string{filepath.Join(codexHome, "hooks.json")}
	for _, pattern := range []string{"plugins/cache/*/*/*/hooks/*.json", "plugins/*/hooks/*.json"} {
		paths, _ := filepath.Glob(filepath.Join(codexHome, pattern))
		candidates = append(candidates, paths...)
	}
	declared := []map[string]any{}
	events := map[string]bool{}
	seen := map[string]bool{}
	for _, path := range candidates {
		if seen[path] {
			continue
		}
		seen[path] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		decoded, err := decodeJSON(raw)
		if err != nil {
			continue
		}
		value := orderedPlain(decoded)
		payload, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %w", path, notObject(value))
		}
		hooks, ok := payload["hooks"].(map[string]any)
		if !ok {
			continue
		}
		names := make([]string, 0, len(hooks))
		for name := range hooks {
			names = append(names, name)
			events[name] = true
		}
		sort.Strings(names)
		declared = append(declared, map[string]any{"source": path, "events": names})
	}
	wanted := map[string]bool{}
	for name := range knownEvents {
		wanted[strings.ReplaceAll(name, "-", "_")] = true
	}
	hostEvents := map[string]bool{}
	if raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "[hooks.state.") || !strings.HasSuffix(line, "]") {
				continue
			}
			key := strings.Trim(line[len("[hooks.state."):len(line)-1], "\"")
			for _, part := range strings.Split(key, ":") {
				if wanted[part] {
					hostEvents[part] = true
				}
			}
		}
	}
	declaredEvents := make([]string, 0, len(events))
	for name := range events {
		declaredEvents = append(declaredEvents, name)
	}
	sort.Strings(declaredEvents)
	hostRecordedEvents := make([]string, 0, len(hostEvents))
	for name := range hostEvents {
		hostRecordedEvents = append(hostRecordedEvents, name)
	}
	sort.Strings(hostRecordedEvents)
	return map[string]any{
		"declaredBy":         declared,
		"declaredEvents":     declaredEvents,
		"hostRecordedEvents": hostRecordedEvents,
		"note":               "Declaration files and the host's recorded hook state are separate signals. Neither proves a handler ran for this session; only an observed invocation does.",
	}, nil
}
