package skill

import (
	"fmt"
	"testing"
)

func titleShapeCases(t *testing.T) []skillShapeCase {
	t.Helper()
	base := map[string]any{
		"role":              "parent",
		"binding_verified":  true,
		"observed_title":    "Body",
		"summary":           "Summary",
		"user_title":        "none",
		"family_candidates": []any{"CRW"},
		"project_labels":    []any{"CRW"},
	}
	readback := map[string]any{"requested_title": "Wanted", "observed_title": "Wanted"}
	bracket := shapeClone(t, base).(map[string]any)
	bracket["observed_title"] = "[OLD] Body"
	bracket["bracket_disposition"] = map[string]any{"bracket": "[OLD]", "action": "replace"}

	var cases []skillShapeCase
	add := func(prefix string, args []string, seed any, paths ...[]string) {
		for _, path := range paths {
			for _, variant := range shapeVariants() {
				cases = append(cases, skillShapeCase{
					name:   fmt.Sprintf("title/%s/%s/%s", prefix, titleShapePath(path), variant.name),
					family: "parent-title",
					args:   args,
					stdin:  shapeSet(shapeClone(t, seed), path, variant),
				})
			}
		}
	}
	addFixture := func(prefix string, seed any, paths ...[]string) {
		for _, path := range paths {
			for _, variant := range shapeVariants() {
				cases = append(cases, skillShapeCase{
					name:   fmt.Sprintf("title/replay/%s/%s/%s", prefix, titleShapePath(path), variant.name),
					family: "parent-title",
					args:   []string{"replay", "--fixtures", "$TMP", "--allow-unreached"},
					stdin:  nil,
					files:  map[string]any{"case.json": shapeSet(shapeClone(t, seed), path, variant)},
				})
			}
		}
	}

	add("decide", []string{"decide"}, base,
		nil,
		[]string{"role"}, []string{"binding_verified"}, []string{"observed_title"},
		[]string{"summary"}, []string{"user_title"}, []string{"family_candidates"},
		[]string{"project_labels"}, []string{"family_candidates", "0"},
		[]string{"project_labels", "0"})
	add("decide-bracket", []string{"decide"}, bracket,
		[]string{"bracket_disposition"}, []string{"bracket_disposition", "bracket"},
		[]string{"bracket_disposition", "action"})
	add("readback", []string{"readback"}, readback,
		nil, []string{"requested_title"}, []string{"observed_title"})

	fixture := map[string]any{
		"subcommand": "decide",
		"input":      base,
		"expected":   map[string]any{"decision": "apply"},
	}
	addFixture("decide", fixture,
		nil,
		[]string{"subcommand"}, []string{"input"}, []string{"expected"},
		[]string{"input", "role"}, []string{"input", "binding_verified"},
		[]string{"input", "observed_title"}, []string{"input", "summary"},
		[]string{"input", "user_title"}, []string{"input", "family_candidates"},
		[]string{"input", "project_labels"}, []string{"input", "family_candidates", "0"},
		[]string{"input", "project_labels", "0"})
	readbackFixture := map[string]any{
		"subcommand": "readback",
		"input":      readback,
		"expected":   map[string]any{"readback": "verified"},
	}
	addFixture("readback", readbackFixture,
		[]string{"input"}, []string{"expected"}, []string{"expected", "readback"},
		[]string{"input", "requested_title"}, []string{"input", "observed_title"})
	cases = append(cases, titleReadbackPairCases()...)
	cases = append(cases, titleReplayFailureCases()...)
	return append(cases, titleBracketShapeCases(t)...)
}

// titleReplayFailureCases hold fixtures whose expectations fail. command_replay
// compares expected key by key in the order the fixture writes it, with
// Python's != and repr(), names an unknown subcommand with str(), and prints
// the failures it collected only after every fixture was replayed, so a later
// fixture that raises prints none of them. sys.stderr writes a lone surrogate
// in a subcommand or key as its \uXXXX escape (errors="backslashreplace").
// Cases whose output depends on key order repeat, so an unordered walk cannot
// pass by chance.
func titleReplayFailureCases() []skillShapeCase {
	decideInput := `{"role": "parent", "binding_verified": true, "observed_title": "Body", "summary": "S", "user_title": "none", "family_candidates": ["CRW"], "project_labels": ["CRW"]}`
	decide := func(expected string) string {
		return `{"subcommand": "decide", "input": ` + decideInput + `, "expected": ` + expected + `}`
	}
	readback := func(expected string) string {
		return `{"subcommand": "readback", "input": {"requested_title": "a", "observed_title": "a"}, "expected": ` + expected + `}`
	}
	readbackOrder := readback(`{"z": 1, "readback": "mismatch", "a": 2}`)
	fixtures := []struct {
		name    string
		repeats int
		files   map[string]string
	}{
		{"readback-key-order", 4, map[string]string{"case.json": readbackOrder}},
		{"decide-key-order", 4, map[string]string{"case.json": decide(`{"zeta": 1, "decision": "x", "alpha": 2, "reason": "y", "body": "B", "matched": null, "title": "T"}`)}},
		{"decide-repr", 1, map[string]string{"case.json": decide(`{"decision": ["x", "true", "it's", "say \"hi\""], "reason": {"b": 1.0, "a": "é"}, "matched": 1, "prefix": false, "title": NaN, "summary": 1e400, "unknown": NaN, "stripped": [null, -0.0, 12345678901234567890]}`)}},
		{"decide-equal", 1, map[string]string{"case.json": decide(`{"requires": [], "stripped": null, "unknown": null, "decision": "apply", "prefix": "CRW"}`)}},
		{"readback-repr", 1, map[string]string{"case.json": readback(`{"readback": ["verified"], "absent": NaN, "other": {"b": [1.0, "it's \"q\""], "a": null}}`)}},
		{"readback-equal", 1, map[string]string{"case.json": readback(`{"readback": "verified", "absent": null}`)}},
		{"unknown-subcommand", 1, map[string]string{
			"a.json": `{"subcommand": {"b": 1.0, "a": [true, "x"]}, "expected": {}}`,
			"b.json": `{"subcommand": 1.0, "expected": {}}`,
			"c.json": `{"subcommand": "d\u00e9", "expected": {}}`,
			"d.json": `{"subcommand": null, "expected": {}}`,
			"e.json": `{"subcommand": ["decide"], "expected": {}}`,
			"f.json": `{"subcommand": "\ud800", "expected": {}}`,
			"g.json": `{"subcommand": "a\udc80b\ud83d\ude00\udfff", "expected": {}}`,
		}},
		{"surrogate-key", 1, map[string]string{
			"a.json": readback(`{"\ud800": 1, "readback": "verified", "x\udcff\u00e9": null}`),
			"b.json": decide(`{"\udfff": "\ud800", "decision": "apply"}`),
		}},
		{"failures-then-fixture-raises", 1, map[string]string{"a.json": readbackOrder, "b.json": `[1]`}},
		{"failures-then-input-raises", 1, map[string]string{"a.json": readbackOrder, "b.json": `{"subcommand": "readback", "input": [1], "expected": {}}`}},
		{"failures-in-fixture-order", 1, map[string]string{
			"a.json": readbackOrder,
			"b.json": `{"subcommand": "readback"}`,
			"c.json": `{"subcommand": "decide", "input": "bad", "expected": {}}`,
			"d.json": decide(`{"reason": "y", "decision": "x"}`),
		}},
	}
	var cases []skillShapeCase
	// An empty --fixtures directory is named as sys.argv holds it: each byte
	// outside well-formed UTF-8 is a lone surrogate (os.fsdecode), which
	// sys.stderr writes as its \udcXX escape.
	for _, name := range []string{"\xff", "\xed\xa0\x80", "caf\u00e9\xed\xb2\x80x"} {
		cases = append(cases, skillShapeCase{
			name:   "title/replay/failures/no-fixtures-under/" + fmt.Sprintf("%x", name),
			family: "parent-title",
			args:   []string{"replay", "--fixtures", "$TMP/" + name, "--allow-unreached"},
		})
	}
	for _, fixture := range fixtures {
		files := map[string]any{}
		for name, text := range fixture.files {
			files[name] = shapeRawFile(text)
		}
		for repeat := range fixture.repeats {
			cases = append(cases, skillShapeCase{
				name:   fmt.Sprintf("title/replay/failures/%s/%d", fixture.name, repeat),
				family: "parent-title",
				args:   []string{"replay", "--fixtures", "$TMP", "--allow-unreached"},
				files:  files,
			})
		}
	}
	return cases
}

// titleReadbackPairCases replay readback fixtures whose two titles are both set,
// to values the readback subcommand refuses but a fixture can hold. Python's
// classify_readback compares them with ==: lists and objects structurally,
// True == 1 == 1.0 exactly, a NaN nested in a container equal to itself (json
// decodes every NaN to one object) and a bare NaN unequal to everything.
func titleReadbackPairCases() []skillShapeCase {
	pairs := []struct{ name, requested, observed string }{
		{"list", `["x"]`, `["x"]`},
		{"object", `{"a": 1}`, `{"a": 1}`},
		{"empty-list", `[]`, `[]`},
		{"empty-object", `{}`, `{}`},
		{"nested", `{"a": [1, {"b": null}]}`, `{"a": [1.0, {"b": null}]}`},
		{"list-order", `["x", "y"]`, `["y", "x"]`},
		{"list-longer", `["x"]`, `["x", "y"]`},
		{"object-extra-key", `{"a": 1}`, `{"a": 1, "b": 2}`},
		{"list-object", `[]`, `{}`},
		{"list-string", `["x"]`, `"x"`},
		{"int-float", `1`, `1.0`},
		{"bool-int", `true`, `1`},
		{"false-zero-float", `false`, `0.0`},
		{"list-bool-int", `[true]`, `[1]`},
		{"big-int-float", `12345678901234567890`, `12345678901234567890.0`},
		{"big-int", `12345678901234567890`, `12345678901234567890`},
		{"inexact-float", `9007199254740993`, `9007199254740992.0`},
		{"infinity", `Infinity`, `1e400`},
		{"nan", `NaN`, `NaN`},
		{"nested-nan", `[NaN]`, `[NaN]`},
		{"object-nan", `{"a": NaN}`, `{"a": NaN}`},
		{"string-number", `"1"`, `1`},
	}
	cases := make([]skillShapeCase, 0, len(pairs))
	for _, pair := range pairs {
		fixture := fmt.Sprintf(`{"subcommand": "readback", "input": {"requested_title": %s, "observed_title": %s}, "expected": {"readback": "verified"}}`, pair.requested, pair.observed)
		cases = append(cases, skillShapeCase{
			name:   "title/replay/readback-pair/" + pair.name,
			family: "parent-title",
			args:   []string{"replay", "--fixtures", "$TMP", "--allow-unreached"},
			files:  map[string]any{"case.json": shapeRawFile(fixture)},
		})
	}
	return cases
}

func titleBracketShapeCases(t *testing.T) []skillShapeCase {
	t.Helper()
	base := map[string]any{
		"role": "parent", "binding_verified": true, "observed_title": "[OLD] Body",
		"family_candidates": []any{"CRW"}, "project_labels": []any{"CRW"},
	}
	dispositions := []any{
		map[string]any{"bracket": "[OLD]", "action": "body"},
		map[string]any{"bracket": "[OLD]", "action": "replace"},
		map[string]any{"bracket": "[OTHER]", "action": "replace"},
		map[string]any{"bracket": "[OLD", "action": "replace"},
		map[string]any{"bracket": "[OLD]", "action": "bad"},
	}
	titles := []string{"[OLD] Body", "[OLD]", "[OLD]   ", "[O]LD] Body", "Body"}
	cases := make([]skillShapeCase, 0, len(dispositions)*len(titles))
	for titleIndex, title := range titles {
		for dispositionIndex, disposition := range dispositions {
			request := shapeClone(t, base).(map[string]any)
			request["observed_title"] = title
			request["bracket_disposition"] = disposition
			cases = append(cases, skillShapeCase{
				name:   fmt.Sprintf("title/bracket/title-%d/disposition-%d", titleIndex, dispositionIndex),
				family: "parent-title", args: []string{"decide"}, stdin: request,
			})
		}
	}
	return cases
}

func titleShapePath(path []string) string {
	if len(path) == 0 {
		return "root"
	}
	result := path[0]
	for _, part := range path[1:] {
		result += "-" + part
	}
	return result
}
