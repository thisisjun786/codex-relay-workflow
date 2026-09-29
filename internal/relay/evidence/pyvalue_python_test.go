package evidence

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

func TestDumpsLivePython(t *testing.T) {
	python, err := filepath.Abs("../../../.venv/bin/python")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input string
		value       any
	}{
		{"integral-float", `1.0`, float64(1)},
		{"large-exponent", `1e20`, float64(1e20)},
		{"small-exponent", `1e-7`, float64(1e-7)},
		{"negative-zero", `-0.0`, jsonFloat24(t, `-0.0`)},
		{"fixed-boundary", `0.0001`, float64(0.0001)},
		{"exponent-boundary", `1e16`, float64(1e16)},
		{"unicode-html-separator", `{"z":"<>&\u2028\u2029","é":"雪😀"}`, map[string]any{"z": "<>&\u2028\u2029", "é": "雪😀"}},
		{"integer", `9007199254740993`, json.Number("9007199254740993")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ascii := range []bool{false, true} {
				a := "0"
				if ascii {
					a = "1"
				}
				cmd := exec.Command(python, "-c", `import json,sys;sys.stdout.write(json.dumps(json.loads(sys.argv[1]),sort_keys=True,ensure_ascii=sys.argv[2]=='1'))`, tc.input, a)
				want, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("Python: %v %s", err, want)
				}
				got := Dumps(tc.value, false, true, ascii)
				if got != string(want) {
					t.Errorf("Dumps diff (ascii=%v)\nGo: %q\nPython: %q", ascii, got, want)
				}
			}
		})
	}
}
func Test24ProviderReprPythonBytes(t *testing.T) {
	python, err := filepath.Abs("../../../.venv/bin/python")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input string
		value any
	}{
		{`{"external":["43"],"dev-gate":["42"]}`, contract.OrderedObject{{Key: "external", Value: []string{"43"}}, {Key: "dev-gate", Value: []string{"42"}}}},
		{`{"dev-gate":["42"],"external":["43"]}`, map[string][]string{"external": {"43"}, "dev-gate": {"42"}}},
		{`{"z":{"b":true,"a":null},"a":[1,"x"]}`, contract.OrderedObject{{Key: "z", Value: contract.OrderedObject{{Key: "b", Value: true}, {Key: "a", Value: nil}}}, {Key: "a", Value: []any{1, "x"}}}},
	} {
		cmd := exec.Command(python, "-c", `import json,sys;sys.stdout.write(repr(json.loads(sys.argv[1])))`, tc.input)
		want, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("oracle: %v %s", err, want)
		}
		if got := Repr(tc.value); got != string(want) {
			t.Fatalf("repr byte diff\nGo: %q\nPython: %q", got, want)
		}
	}
}

func jsonFloat24(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// DumpsIndent is json.dumps with an integer indent: every member on its own line whatever the
// indent, so indent=0 (and a negative one) gives newlines with no indentation, not the compact
// form only indent=None gives. The expected bytes are Python 3's json.dumps output, captured once.
func TestDumpsIndentMatchesPythonForEveryIndent(t *testing.T) {
	value := contract.OrderedObject{
		{Key: "b", Value: []any{int64(1), contract.OrderedObject{{Key: "c", Value: "é"}}, []any{}}},
		{Key: "a", Value: contract.OrderedObject{}},
		{Key: "d", Value: nil},
		{Key: "e", Value: contract.OrderedObject{{Key: "x", Value: true}, {Key: "y", Value: 1.5}}},
	}
	for indent, want := range map[int]string{
		0: "{\n\"a\": {},\n\"b\": [\n1,\n{\n\"c\": \"\\u00e9\"\n},\n[]\n],\n\"d\": null,\n\"e\": {\n\"x\": true,\n\"y\": 1.5\n}\n}",
		1: "{\n \"a\": {},\n \"b\": [\n  1,\n  {\n   \"c\": \"\\u00e9\"\n  },\n  []\n ],\n \"d\": null,\n \"e\": {\n  \"x\": true,\n  \"y\": 1.5\n }\n}",
	} {
		if got := DumpsIndent(value, indent, true, true); got != want {
			t.Errorf("indent=%d\n go %q\n py %q", indent, got, want)
		}
	}
	if got := DumpsIndent([]any{int64(1), []any{int64(2)}}, -1, false, true); got != "[\n1,\n[\n2\n]\n]" {
		t.Errorf("indent=-1: %q", got)
	}
	if got := DumpsIndent([]any{}, 0, false, true) + DumpsIndent("s", 0, false, true); got != `[]"s"` {
		t.Errorf("an empty list and a scalar: %q", got)
	}
}
