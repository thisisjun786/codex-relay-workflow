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
			cases = append(cases, skillShapeCase{name: "observe/registration/" + path + "/" + variant.name, family: "hook-probe", args: []string{"observe", "--binary", "$TMP/codex", "--codex-home", "$TMP/codex-home"}, files: map[string]any{"codex": shapeClone(t, input), "codex-home/hooks.json": shapeSet(map[string]any{"hooks": map[string]any{"Stop": []any{}}}, parts, variant)}})
		}
	}
	cases = append(cases, observeSortShapeCases(t, input, output)...)
	cases = append(cases, observeDefinitionOrderCases()...)
	return append(cases, observeScanCases()...)
}

// observeSortShapeCases seed the arrays capability_matrix passes to sorted():
// numbers order by value, lists lexicographically, and a pair '<' cannot order
// raises the TypeError of the first comparison CPython's sort makes. Values are
// spelled as raw JSON so 1.0, 1e400 and NaN reach both runtimes as written; a
// NaN, neither less nor greater than anything, stays where the sort leaves it.
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
		{"nan", `[3, NaN, 1]`},
		{"non-finite", `[Infinity, NaN, -Infinity, 0, NaN, 2.5]`},
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

// observeSchema is one embedded schema as the scan finds it: the needle, then
// the rest of the object spelled as given.
func observeSchema(title, rest string) string {
	return "{\n  \"$schema\": \"http://json-schema.org/draft-07/schema#\",\n  \"title\": \"" + title + "\"" + rest + "\n}"
}

// observeDefinitionOrderCases hold several definitions capability_matrix cannot
// read. Python walks definitions.values() in the order the schema writes them,
// so the first one written raises, on every run; each case repeats so an
// unordered walk cannot pass by chance.
func observeDefinitionOrderCases() []skillShapeCase {
	orders := []struct{ name, definitions string }{
		{"list-first", `{"A": [1], "B": "x", "C": 5, "D": {"properties": 7}}`},
		{"properties-first", `{"A": {"properties": 7}, "B": {"properties": true}, "C": {"properties": 2.5}}`},
		{"many", `{"d0": {"properties": 2.5}, "d1": [1], "d2": "x", "d3": 5, "d4": true, "d5": {"properties": 7}, "d6": null, "d7": {"properties": false}, "d8": 1.5}`},
		{"readable-then-bad", `{"ok": {"properties": {"additionalContext": {}}}, "later": {"properties": "additionalContext"}, "bad": 3, "worse": [1]}`},
	}
	var cases []skillShapeCase
	for _, order := range orders {
		binary := observeSchema("stop.command.output", ",\n  \"properties\": {},\n  \"definitions\": "+order.definitions)
		for repeat := range 8 {
			cases = append(cases, skillShapeCase{
				name:   fmt.Sprintf("observe/definition-order/%s/%d", order.name, repeat),
				family: "hook-probe",
				args:   []string{"observe", "--binary", "$TMP/codex", "--sanitize"},
				files:  map[string]any{"codex": shapeRawFile(binary)},
			})
		}
	}
	return cases
}

// observeScanCases pin how the scan reads each embedded schema:
// extract_schemas decodes a 64 KiB window with errors="replace" (one U+FFFD
// for each sequence the strict decoder refuses) and raw_decode reads the first
// value in it, NaN and Infinity included, ignoring what follows; a ValueError,
// from a stray byte outside a string, the integer-digit limit or a schema the
// window cuts off, skips that schema.
func observeScanCases() []skillShapeCase {
	window := 1 << 16
	digits := strings.Repeat("7", 4300)
	pad := func(schema string, total int) string { return schema + strings.Repeat(" ", total-len(schema)) }
	fits := observeSchema("stop.command.input", ",\n  \"required\": [\"a\"],\n  \"properties\": {\"p\": \""+strings.Repeat("x", window-200)+"\"}")
	long := observeSchema("stop.command.input", ",\n  \"required\": [\"a\"],\n  \"properties\": {\"p\": \""+strings.Repeat("x", window)+"\"}")
	binaries := []struct{ name, binary string }{
		{"nan-elsewhere", observeSchema("stop.command.input", ",\n  \"minimum\": NaN,\n  \"maximum\": -Infinity,\n  \"required\": [\"a\"],\n  \"properties\": {\"b\": Infinity}")},
		{"invalid-utf8-strings", observeSchema("stop.command.input", ",\n  \"required\": [\"a\xffb\", \"\xe2\x82x\", \"\xed\xa0\x80\", \"\xc0\xaf\", \"\xf4\x90\x80\x80\", \"\xef\xbf\xbd\", \"z\"],\n  \"properties\": {\"\xf0\x9f\": 1, \"\xe0\x80\x80\": 2}")},
		{"invalid-utf8-title", observeSchema("stop\xff.command.input", ",\n  \"required\": [\"a\"]")},
		{"invalid-utf8-outside-string", observeSchema("stop.command.input", ",\n  \"required\": [1\xff]")},
		{"invalid-utf8-after-schema", "\xff\xfe" + observeSchema("stop.command.input", ",\n  \"required\": [\"a\"]") + "\xe2\x82 trailing {]"},
		{"misspelt-nan", observeSchema("stop.command.input", ",\n  \"required\": [Nan]")},
		{"integer-at-digit-limit", observeSchema("stop.command.input", ",\n  \"required\": ["+digits+", 1]")},
		{"integer-past-digit-limit", observeSchema("stop.command.input", ",\n  \"required\": ["+digits+"7]")},
		{"schema-fills-window", pad(fits, window-1) + "\xc3\xa9"},
		{"schema-past-window", long},
		{"schema-past-window-then-readable", long + "\n" + observeSchema("stop.command.output", ",\n  \"properties\": {}")},
	}
	cases := make([]skillShapeCase, 0, len(binaries))
	for _, b := range binaries {
		cases = append(cases, skillShapeCase{
			name:   "observe/scan/" + b.name,
			family: "hook-probe",
			args:   []string{"observe", "--binary", "$TMP/codex", "--sanitize"},
			files:  map[string]any{"codex": shapeRawFile(b.binary)},
		})
	}
	return cases
}
