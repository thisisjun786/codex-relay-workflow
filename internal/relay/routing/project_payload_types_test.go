package routing

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Test23ProjectPayloadWrongTypesMatchTheGolden sets each field of a valid project payload, or an item of
// its members or components, to a value of the wrong type and checks the problems the queue and
// pre-issue validator reports.
func Test23ProjectPayloadWrongTypesMatchTheGolden(t *testing.T) {
	var cases []struct {
		Field string
		Value any
	}
	for _, field := range []string{"team", "name", "lead", "members", "components"} {
		for _, value := range []any{json.Number("123"), true, nil, []any{}, map[string]any{}} {
			cases = append(cases, struct {
				Field string
				Value any
			}{field, value})
		}
	}
	for _, field := range []string{"members", "components"} {
		first := map[string]string{"members": "m1", "components": "c1"}[field]
		for _, value := range []any{json.Number("123"), true, nil, []any{}, map[string]any{}, "", "  "} {
			cases = append(cases, struct {
				Field string
				Value any
			}{field + "-item", []any{first, value}})
		}
	}
	base := map[string]any{"product": "p", "workspace": "w", "team": "t", "familyLabel": "f", "goal": "g", "criteria": "c", "name": "n", "members": []any{"m1", "m2"}, "components": []any{"c1"}}
	for i, tc := range cases {
		name := tc.Field + "/" + evidence.TypeName(tc.Value)
		var problems []string
		if !t.Run(name, func(t *testing.T) {
			one := make(map[string]any, len(base)+1)
			for k, v := range base {
				one[k] = v
			}
			field := strings.TrimSuffix(tc.Field, "-item")
			one[field] = tc.Value
			problems = ValidateProjectPayload(one)
		}) {
			continue
		}
		golden.CheckJSON(t, fmt.Sprintf("%02d %s", i, name), problems)
	}
}
