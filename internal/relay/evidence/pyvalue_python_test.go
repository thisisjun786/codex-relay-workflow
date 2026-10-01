package evidence

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func TestDumpsMatchesTheGolden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"integral-float", float64(1)},
		{"large-exponent", float64(1e20)},
		{"small-exponent", float64(1e-7)},
		{"negative-zero", jsonFloat24(t, `-0.0`)},
		{"fixed-boundary", float64(0.0001)},
		{"exponent-boundary", float64(1e16)},
		{"unicode-html-separator", map[string]any{"z": "<>&\u2028\u2029", "é": "雪😀"}},
		{"integer", json.Number("9007199254740993")},
	} {
		// The expected bytes (json.dumps with sort_keys, with and without ensure_ascii) are read in
		// the parent test, so they share one golden.
		wants := map[bool][]byte{}
		for _, ascii := range []bool{false, true} {
			a := "0"
			if ascii {
				a = "1"
			}
			wants[ascii] = golden.Want(t, tc.name+"/ascii="+a, func() []byte { return []byte(pyjson.Dumps(tc.value, pyjson.Options{SortKeys: true, Unicode: !ascii})) })
		}
		t.Run(tc.name, func(t *testing.T) {
			for _, ascii := range []bool{false, true} {
				want := wants[ascii]
				got := pyjson.Dumps(tc.value, pyjson.Options{SortKeys: true, Unicode: !ascii})
				if got != string(want) {
					t.Errorf("Dumps diff (ascii=%v)\nGo: %q\ngolden: %q", ascii, got, want)
				}
			}
		})
	}
}
func Test24ProviderReprPythonBytes(t *testing.T) {
	for _, tc := range []struct {
		input string
		value any
	}{
		{`{"external":["43"],"dev-gate":["42"]}`, contract.OrderedObject{{Key: "external", Value: []string{"43"}}, {Key: "dev-gate", Value: []string{"42"}}}},
		{`{"dev-gate":["42"],"external":["43"]}`, map[string][]string{"external": {"43"}, "dev-gate": {"42"}}},
		{`{"z":{"b":true,"a":null},"a":[1,"x"]}`, contract.OrderedObject{{Key: "z", Value: contract.OrderedObject{{Key: "b", Value: true}, {Key: "a", Value: nil}}}, {Key: "a", Value: []any{1, "x"}}}},
	} {
		// The key is the JSON text whose Python repr the golden began as.
		golden.Check(t, tc.input, []byte(pyvalue.Repr(tc.value)))
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

// json.dumps with an integer indent: every member on its own line, indented that many spaces per
// level. The expected bytes are Python 3's json.dumps output, captured once. (indent=0 and a
// negative indent, newlines with no indentation, no caller asks for; pyjson spells indent=None
// as 0.)
func TestDumpsIndentMatchesPythonForEveryIndent(t *testing.T) {
	value := contract.OrderedObject{
		{Key: "b", Value: []any{int64(1), contract.OrderedObject{{Key: "c", Value: "é"}}, []any{}}},
		{Key: "a", Value: contract.OrderedObject{}},
		{Key: "d", Value: nil},
		{Key: "e", Value: contract.OrderedObject{{Key: "x", Value: true}, {Key: "y", Value: 1.5}}},
	}
	for indent, want := range map[int]string{
		1: "{\n \"a\": {},\n \"b\": [\n  1,\n  {\n   \"c\": \"\\u00e9\"\n  },\n  []\n ],\n \"d\": null,\n \"e\": {\n  \"x\": true,\n  \"y\": 1.5\n }\n}",
	} {
		if got := pyjson.Dumps(value, pyjson.Options{Indent: indent, SortKeys: true}); got != want {
			t.Errorf("indent=%d\n go %q\n py %q", indent, got, want)
		}
	}
}
