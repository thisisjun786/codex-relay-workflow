package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

func TestScanArgsMapOrder(t *testing.T) {
	for _, pairs := range [][]string{{"qb=constraint", "qa=goal"}, {"qa=goal", "qb=constraint"}, {"qb=goal", "qa=goal", "qb=success", "2=constraint", "1=ontology", "constructor=goal", "__proto__=success"}} {
		argv := []string{"record", "--session", "s1"}
		var wantOrder []string
		wantMap := map[string]interview.Dimension{}
		for _, pair := range pairs {
			key, value, _ := strings.Cut(pair, "=")
			if _, seen := wantMap[key]; !seen {
				wantOrder = append(wantOrder, key)
			}
			wantMap[key] = interview.Dimension(value)
			argv = append(argv, "--map", pair)
		}
		parsed := ParseScanCliArgs(argv, t.TempDir())
		if parsed.Error != "" {
			t.Fatal(parsed.Error)
		}
		// Reflect permits this test to compile before the new field exists.
		field := reflect.ValueOf(*parsed.Args).FieldByName("MapOrder")
		if !field.IsValid() || !reflect.DeepEqual(field.Interface(), wantOrder) {
			t.Fatalf("first insertion order: field=%v want=%v", field, wantOrder)
		}
		if !reflect.DeepEqual(parsed.Args.Map, wantMap) {
			t.Fatal("map overwrite behavior changed")
		}
		encoded, err := json.Marshal(parsed.Args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "MapOrder") || strings.Contains(string(encoded), "mapOrder") {
			t.Fatal("internal order serialized")
		}
	}
}
