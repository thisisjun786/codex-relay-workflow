package delivery

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// HostTime reads every host start value as the fence's hostadapter.host_time reads the same
// JSON value: the answer (a finite number of seconds, or none) is the Python package's, recorded
// (internal/testsupport/pyoracle), never a table written here.
func TestHostTimeReadsEveryValueAsTheFence(t *testing.T) {
	values := []string{
		`1789420929`, `1789420929.5`, `-0`, `0`, `1e400`, `-1e400`, `1e-400`, `9007199254740993`, `1` + strings.Repeat("0", 400),
		`true`, `false`, `null`, `[1]`, `{"at": 1}`,
		`"1789420929"`, `" 1e3 "`, `"1_000"`, `"1__0"`, `"_1"`, `"1_"`, `"1_.5"`, `"1._5"`, `"1.5_0e1_0"`, `"+1.5"`, `"-0"`, `".5"`, `"5."`,
		`"1e"`, `"e1"`, `"0x10"`, `"inf"`, `"-Infinity"`, `"nan"`, `"NaN"`, `"  "`, `""`, `"1 2"`, `"١٢٣"`, `"１２３"`, `" 1 "`,
		`"\u001c1"`, `"1\u0000"`, `"\t42\n"`, `"1e400"`, `"-1e400"`, `"9007199254740993"`, `"1.7976931348623157e308"`, `"٣_٤"`,
	}
	input, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	out := pyAnswer(t, "host_time", func() ([]byte, error) {
		repo, err := filepath.Abs("../../..")
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("uv", "run", "--no-sync", "python", "-c", `import json, sys
from codex_session_relay.hostadapter import host_time
print(json.dumps([host_time(json.loads(value)) for value in json.load(sys.stdin)]))`)
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(input)
		return pythonOutput(cmd)
	})
	var python []*float64
	if err := json.Unmarshal(out, &python); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(python) != len(values) {
		t.Fatalf("python answered %d values for %d", len(python), len(values))
	}
	for i, raw := range values {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(raw, err)
		}
		got, want := HostTime(value), python[i]
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Errorf("host time of %s: Go %v, Python %v", raw, show(got), show(want))
		}
	}
}

func show(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
