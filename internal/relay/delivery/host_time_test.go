package delivery

import (
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	"strings"
	"testing"
)

// HostTime reads every host start value as the fence's hostadapter.host_time reads the same
// JSON value: the answers (a finite number of seconds, or none) are checked against the golden,
// which began as the Python package's answers, never a table written here.
func TestHostTimeReadsEveryValueAsTheFence(t *testing.T) {
	values := []string{
		`1789420929`, `1789420929.5`, `-0`, `0`, `1e400`, `-1e400`, `1e-400`, `9007199254740993`, `1` + strings.Repeat("0", 400),
		`true`, `false`, `null`, `[1]`, `{"at": 1}`,
		`"1789420929"`, `" 1e3 "`, `"1_000"`, `"1__0"`, `"_1"`, `"1_"`, `"1_.5"`, `"1._5"`, `"1.5_0e1_0"`, `"+1.5"`, `"-0"`, `".5"`, `"5."`,
		`"1e"`, `"e1"`, `"0x10"`, `"inf"`, `"-Infinity"`, `"nan"`, `"NaN"`, `"  "`, `""`, `"1 2"`, `"١٢٣"`, `"１２３"`, `" 1 "`,
		`"\u001c1"`, `"1\u0000"`, `"\t42\n"`, `"1e400"`, `"-1e400"`, `"9007199254740993"`, `"1.7976931348623157e308"`, `"٣_٤"`,
	}
	var answers []any
	for _, raw := range values {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(raw, err)
		}
		answers = append(answers, show(HostTime(value)))
	}
	golden.CheckJSON(t, "host_time", answers)
}

func show(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
