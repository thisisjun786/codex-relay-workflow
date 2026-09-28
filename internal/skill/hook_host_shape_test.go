package skill

import (
	"encoding/json"
	"fmt"
	"strings"
)

type hostShapePath struct {
	file string
	path []string
	seed func() map[string]any
}

func hostShapeCases() []skillShapeCase {
	paths := []hostShapePath{
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"capabilityRecord"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"version"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"stopInput"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"stopInput", "fields"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"stopInput", "fields", "entry"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"stopInput", "types"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"stopInput", "types", "cwd"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"observations"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"observations", "H1"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"observations", "H1", "question"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"observations", "H1", "status"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"observations", "H1", "observed"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"observations", "H1", "evidence"}},
		{file: "host/host-observation-codex-0.154.0.json", path: []string{"observations", "H1", "whyUnresolved"}, seed: hostUnresolvedShapeSeed},
		{file: "host/host-capability-codex-0.154.0.json", path: []string{"events"}},
		{file: "host/host-capability-codex-0.154.0.json", path: []string{"events", "stop"}},
		{file: "host/host-capability-codex-0.154.0.json", path: []string{"events", "stop", "input"}},
		{file: "host/host-capability-codex-0.154.0.json", path: []string{"events", "stop", "input", "required"}},
		{file: "host/host-capability-codex-0.154.0.json", path: []string{"events", "stop", "input", "required", "entry"}},
	}

	cases := make([]skillShapeCase, 0, len(paths)*len(shapeVariants())+2)
	for _, target := range paths {
		for _, variant := range shapeVariants() {
			files := hostShapeSeed()
			if target.seed != nil {
				files = target.seed()
			}
			files[target.file] = shapeSet(files[target.file], target.path, variant)
			cases = append(cases, hostShapeCase(
				strings.Join(target.path, ".")+"/"+variant.name,
				files,
			))
		}
	}
	for _, filename := range []string{
		"host/host-observation-codex-0.154.0.json",
		"host/host-capability-codex-0.154.0.json",
	} {
		for _, variant := range shapeVariants() {
			files := hostShapeSeed()
			if variant.absent {
				delete(files, filename)
			} else {
				files[filename] = variant.value
			}
			name := strings.TrimSuffix(strings.TrimPrefix(filename, "host/host-"), ".json")
			cases = append(cases, hostShapeCase("root/"+name+"/"+variant.name, files))
		}
	}
	for _, test := range []struct {
		name                string
		declared, delivered []any
	}{
		{"equal-list", []any{[]any{}}, []any{[]any{}}},
		{"equal-object", []any{map[string]any{}}, []any{map[string]any{}}},
		{"equal-null", []any{nil}, []any{nil}},
		{"equal-bool-int", []any{true}, []any{1}},
		{"equal-int-float", []any{1}, []any{json.Number("1.0")}},
		{"numeric-order", []any{10, 2}, []any{"cwd"}},
		{"numeric-difference", []any{true, 2}, []any{1, 3}},
	} {
		files := hostShapeSeed()
		capability := files["host/host-capability-codex-0.154.0.json"].(map[string]any)
		capability["events"].(map[string]any)["stop"].(map[string]any)["input"].(map[string]any)["required"] = test.declared
		record := files["host/host-observation-codex-0.154.0.json"].(map[string]any)
		record["stopInput"].(map[string]any)["fields"] = test.delivered
		cases = append(cases, hostShapeCase("paired-fields/"+test.name, files))
	}
	return cases
}

func hostShapeCase(name string, files map[string]any) skillShapeCase {
	return skillShapeCase{
		name:   "host/" + name,
		family: "hook-probe",
		args: []string{
			"replay",
			"--fixtures", "$TMP/decisions",
			"--host-fixtures", "$TMP/host",
			"--allow-unreached",
		},
		files: files,
	}
}

func hostShapeSeed() map[string]any {
	rows := map[string]any{}
	for i := 1; i <= 7; i++ {
		rows[fmt.Sprintf("H%d", i)] = map[string]any{
			"question": "question",
			"status":   "resolved",
			"observed": "observed",
			"evidence": "evidence",
		}
	}
	return map[string]any{
		"decisions/unmanaged-session.json": map[string]any{
			"note":     "An ordinary session with no assignment directory is ignored, even when its last message claims completion.",
			"expected": map[string]any{"decision": "release", "observation": "unmanaged", "state": "unmanaged"},
			"observation": map[string]any{
				"marker": nil,
				"stop_input": map[string]any{
					"cwd": "/workspace/example", "hook_event_name": "Stop",
					"last_assistant_message": "All done.", "model": "<model>",
					"permission_mode": "default", "session_id": "session-1111",
					"stop_hook_active": false, "transcript_path": nil, "turn_id": "turn-0001",
				},
			},
		},
		"host/host-capability-codex-0.154.0.json": map[string]any{
			"events": map[string]any{"stop": map[string]any{"input": map[string]any{"required": []any{"cwd"}}}},
		},
		"host/host-observation-codex-0.154.0.json": map[string]any{
			"capabilityRecord": "host-capability-codex-0.154.0.json",
			"version":          "codex-cli 0.154.0",
			"stopInput": map[string]any{
				"fields": []any{"cwd"},
				"types":  map[string]any{"cwd": "str"},
			},
			"observations": rows,
		},
	}
}

func hostUnresolvedShapeSeed() map[string]any {
	files := hostShapeSeed()
	record := files["host/host-observation-codex-0.154.0.json"].(map[string]any)
	rows := record["observations"].(map[string]any)
	rows["H1"] = map[string]any{
		"question":      "question",
		"status":        "unresolved",
		"whyUnresolved": "unknown",
	}
	return files
}
