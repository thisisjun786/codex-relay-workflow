package skill

import (
	"strings"
	"testing"
)

func observeShapeCases(t *testing.T) []skillShapeCase {
	t.Helper()
	input := map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "title": "stop.command.input", "required": []any{"session_id"}, "properties": map[string]any{"session_id": map[string]any{"type": "string"}}}
	output := map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "title": "stop.command.output", "properties": map[string]any{"hookSpecificOutput": map[string]any{}}, "definitions": map[string]any{"BlockDecisionWire": map[string]any{"enum": []any{"block"}, "properties": map[string]any{"additionalContext": map[string]any{}}}, "PreToolUseDecisionWire": map[string]any{"enum": []any{"allow"}}}}
	var cases []skillShapeCase
	for _, target := range []struct {
		name  string
		seed  any
		paths []string
	}{
		{"input", input, []string{"", "title", "required", "required.0", "properties", "properties.session_id"}},
		{"output", output, []string{"title", "properties", "properties.hookSpecificOutput", "definitions", "definitions.BlockDecisionWire", "definitions.BlockDecisionWire.enum", "definitions.BlockDecisionWire.enum.0", "definitions.BlockDecisionWire.properties", "definitions.BlockDecisionWire.properties.additionalContext", "definitions.PreToolUseDecisionWire", "definitions.PreToolUseDecisionWire.enum"}},
	} {
		for _, path := range target.paths {
			var parts []string
			if path != "" {
				parts = strings.Split(path, ".")
			}
			for _, variant := range shapeVariants() {
				cases = append(cases, skillShapeCase{name: "observe/" + target.name + "/" + path + "/" + variant.name, family: "hook-probe", args: []string{"observe", "--binary", "$TMP/codex", "--sanitize"}, files: map[string]any{"codex": shapeSet(shapeClone(t, target.seed), parts, variant)}})
			}
		}
	}
	for _, path := range []string{"", "hooks", "hooks.Stop"} {
		var parts []string
		if path != "" {
			parts = strings.Split(path, ".")
		}
		for _, variant := range shapeVariants() {
			cases = append(cases, skillShapeCase{name: "observe/registration/" + path + "/" + variant.name, family: "hook-probe", args: []string{"observe", "--binary", "$TMP/codex", "--codex-home", "$TMP/home"}, files: map[string]any{"codex": shapeClone(t, input), "home/hooks.json": shapeSet(map[string]any{"hooks": map[string]any{"Stop": []any{}}}, parts, variant)}})
		}
	}
	return cases
}
