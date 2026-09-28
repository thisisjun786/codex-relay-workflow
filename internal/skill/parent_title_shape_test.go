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
	return append(cases, titleBracketShapeCases(t)...)
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
