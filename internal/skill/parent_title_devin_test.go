package skill

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestParentTitleDevinCases(t *testing.T) {
	goldenRoot(t)
	base := map[string]any{
		"role":              "parent",
		"binding_verified":  true,
		"project_labels":    []string{"CRW"},
		"family_candidates": []string{"CRW"},
		"user_title":        "none",
	}
	tests := []struct {
		name    string
		request map[string]any
	}{
		{name: "trailing spaces do not panic", request: parentTitleRequest(base, "CRW  ")},
		{name: "literal Unicode escapes remain literal", request: parentTitleRequest(base, `literal \u2028 and \\u2029`)},
		{name: "empty observed title uses summary", request: parentTitleRequest(base, "", "summary", "Summary body")},
		{name: "empty requires serializes as list", request: parentTitleRequest(base, "Body")},
		{name: "nonempty requires serializes as list", request: map[string]any{"role": "parent", "binding_verified": false, "observed_title": "Body"}},
	}

	pythonSpaces := []rune{
		'\t', '\n', '\v', '\f', '\r', '\u001c', '\u001d', '\u001e', '\u001f', ' ',
		'\u0085', '\u00a0', '\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004',
		'\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u2028', '\u2029',
		'\u202f', '\u205f', '\u3000',
	}
	for _, space := range pythonSpaces {
		name := fmt.Sprintf("U+%04X", space)
		tests = append(tests,
			struct {
				name    string
				request map[string]any
			}{name: "own bracket gap " + name, request: parentTitleRequest(base, "[CRW]"+string(space)+"Body")},
			struct {
				name    string
				request map[string]any
			}{name: "foreign bracket gap " + name, request: parentTitleRequest(base, "[OLD]"+string(space)+"Body", "bracket_disposition", map[string]any{"bracket": "[OLD]", "action": "replace"})},
			struct {
				name    string
				request map[string]any
			}{name: "foreign bracket without body " + name, request: parentTitleRequest(base, "[OLD]"+string(space))},
			struct {
				name    string
				request map[string]any
			}{name: "bare prefix gap " + name, request: parentTitleRequest(base, "CRW"+string(space)+"-"+string(space)+"Body")},
			struct {
				name    string
				request map[string]any
			}{name: "bare separator gap " + name, request: parentTitleRequest(base, "CRW -"+string(space)+"Body")},
		)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}

			goExit, goOut, goErr := call([]string{"parent-title", "decide"}, string(payload))
			checkSkillAnswer(t, "", "", []string{"skill", "parent-title", "decide"}, skillProcessResult{goExit, goOut, goErr})
		})
	}
}

func parentTitleRequest(base map[string]any, observed string, extra ...any) map[string]any {
	request := make(map[string]any, len(base)+1+len(extra)/2)
	for key, value := range base {
		request[key] = value
	}
	request["observed_title"] = observed
	for index := 0; index < len(extra); index += 2 {
		request[extra[index].(string)] = extra[index+1]
	}
	return request
}
