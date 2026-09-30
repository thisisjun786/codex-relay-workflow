package skill

import (
	"encoding/json"
	"fmt"
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
	return append(cases, observeSortShapeCases(t, input, output)...)
}

// observeSortShapeCases seed the arrays capability_matrix passes to sorted():
// numbers order by value, lists lexicographically, and a pair '<' cannot order
// raises the TypeError of the first comparison CPython's sort makes. Values are
// spelled as raw JSON so 1.0 and 1e400 reach both runtimes as written. NaN is
// left to TestPySortedMatchesLivePython: the Go schema scan does not yet accept
// the NaN and Infinity literals Python's raw_decode does.
func observeSortShapeCases(t *testing.T, input, output map[string]any) []skillShapeCase {
	t.Helper()
	seeds := []struct{ name, value string }{
		{"ints", `[2, 10]`},
		{"floats", `[2.5, 10, 1]`},
		{"integral-floats", `[3.0, 1, 2.0, 1.0]`},
		{"bools", `[true, 0, 2, false]`},
		{"big-ints", `[12345678901234567890, 1e19, -12345678901234567890]`},
		{"infinities", `[1e400, 1, -1e400, 2]`},
		{"lists", `[["a", "b"], ["a"], []]`},
		{"number-string", `[1, "block"]`},
		{"string-number", `["b", 1]`},
		{"null-string", `[null, "block"]`},
		{"objects", `[{"a": 1}, {"b": 2}]`},
		{"list-string", `[["a"], "b"]`},
		{"lists-of-number-and-string", `[[1], ["a"]]`},
		{"non-ascii", `["é", "e", "😀", "z"]`},
	}
	marker := "\"__seed__\""
	document := func(schema map[string]any, path []string, value string) string {
		raw, err := json.MarshalIndent(shapeSet(shapeClone(t, schema), path, shapeVariant{value: "__seed__"}), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return strings.Replace(string(raw), marker, value, 1)
	}
	var cases []skillShapeCase
	for _, seed := range seeds {
		for _, target := range []struct {
			name   string
			schema map[string]any
			path   []string
		}{
			{"input-required", input, []string{"required"}},
			{"input-properties", input, []string{"properties"}},
			{"output-enum", output, []string{"definitions", "BlockDecisionWire", "enum"}},
		} {
			cases = append(cases, skillShapeCase{
				name:   "observe/sort/" + target.name + "/" + seed.name,
				family: "hook-probe",
				args:   []string{"observe", "--binary", "$TMP/codex", "--sanitize"},
				files:  map[string]any{"codex": shapeRawFile(document(target.schema, target.path, seed.value))},
			})
		}
	}
	// Python raises from the first schema it reduces, in the order the binary embeds them.
	required := document(input, []string{"required"}, `[1, "a"]`)
	enum := document(output, []string{"definitions", "BlockDecisionWire", "enum"}, `[null, "block"]`)
	for _, order := range []struct {
		name  string
		files []string
	}{
		{"input-first", []string{required, enum}},
		{"output-first", []string{enum, required}},
	} {
		for repeat := range 4 {
			cases = append(cases, skillShapeCase{
				name:   fmt.Sprintf("observe/sort/two-schemas/%s/%d", order.name, repeat),
				family: "hook-probe",
				args:   []string{"observe", "--binary", "$TMP/codex", "--sanitize"},
				files:  map[string]any{"codex": shapeRawFile(strings.Join(order.files, "\n"))},
			})
		}
	}
	return cases
}
