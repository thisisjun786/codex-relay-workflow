package supervisor

import (
	"context"
	"testing"
)

func Test24_SR_9_PlacementGrid(t *testing.T) {
	f := fixture24(t)
	states := []string{"reported", "in_progress", "unmanaged", "unmeasured", "unreported", "foreign_schema", "something"}
	scopes := []struct {
		name    string
		value   any
		present bool
	}{
		{"absent", nil, false}, {"null", nil, true}, {"ours", "rel-1", true},
		{"foreign", "rel-somewhere-else", true}, {"malformed-list", []any{"rel-1"}, true}, {"malformed-blank", "   ", true},
	}
	for _, scope := range scopes {
		for _, state := range states {
			t.Run(scope.name+"/"+state, func(t *testing.T) {
				reading := map[string]any{"schema": "reporting-observation/1", "reportingState": state, "reason": "terminal_without_report", "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
				if scope.present {
					reading["relationshipId"] = scope.value
				}
				if state == "foreign_schema" {
					reading["schema"] = "something-else/1"
					reading["reportingState"] = "reported"
				}
				answer, err := f.c.Standing(context.Background(), "PRJ-1", []any{reading})
				if err != nil {
					t.Fatal(err)
				}
				standing := answer["standing"].([]any)
				gaps := answer["gaps"].([]any)
				want := "nothing"
				switch {
				case scope.name == "foreign":
				case scope.name == "malformed-list" || scope.name == "malformed-blank":
					want = "reading_unusable"
				case state == "unmeasured":
					want = "reporting_unmeasured"
				case state == "unreported" && scope.name == "ours":
					want = "unreported"
				case state == "unreported" || state == "foreign_schema" || state == "something":
					want = "reading_unusable"
				}
				switch want {
				case "nothing":
					if len(standing) != 1 || len(gaps) != 0 {
						t.Fatalf("standing=%s gaps=%s", jsonText(standing), jsonText(gaps))
					}
				case "unreported":
					if len(standing) != 2 || standing[1].(map[string]any)["kind"] != "unreported" || len(gaps) != 0 {
						t.Fatalf("standing=%s gaps=%s", jsonText(standing), jsonText(gaps))
					}
				default:
					if len(standing) != 1 || len(gaps) != 1 || gaps[0].(map[string]any)["gap"] != want {
						t.Fatalf("standing=%s gaps=%s", jsonText(standing), jsonText(gaps))
					}
				}
			})
		}
	}
}
