package adapter

import "testing"

func Test28_SettingsMissingNullAndWrongShapeNeverAdmit(t *testing.T) {
	for _, key := range []string{"approvalPolicy", "sandbox", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort", "thread"} {
		for _, shape := range []string{"missing", "null", "object", "list", "number", "boolean"} {
			t.Run(key+"/"+shape, func(t *testing.T) {
				r := resume()
				switch shape {
				case "missing":
					delete(r, key)
				case "null":
					r[key] = nil
				case "object":
					r[key] = map[string]any{"wrong": "shape"}
				case "list":
					r[key] = []any{"a"}
				case "number":
					r[key] = 1.0
				case "boolean":
					r[key] = true
				}
				capture(t, sendScenario(r, []any{"send", "drop-field", "thread-1", "hi"}))
			})
		}
	}
}
