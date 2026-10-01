package evidence

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Shown is the one envelope helper the product reads (the supervisor channel's rendered message).
// Its answers over every JSON value shape, directly and as an absence's detail, are the golden;
// they began as the Python implementation's.
func Test24EnvelopeAccessorPython(t *testing.T) {
	rows := golden.Want(t, "envelope-accessors", envelopeAccessorRows)
	for _, value := range Items(Decode(string(rows))) {
		row := Dict(value, false)
		tc := struct {
			Op                   string
			Value, Result, Error any
		}{pyvalue.Str(row["op"]), row["value"], row["result"], row["error"]}
		t.Run(tc.Op+"/"+pyvalue.Repr(tc.Value), func(t *testing.T) {
			got, failure := envelopeAccessor(tc.Op, tc.Value)
			if pyjson.Dumps(failure, pyjson.Options{SortKeys: true}) != pyjson.Dumps(tc.Error, pyjson.Options{SortKeys: true}) || (failure == nil && pyjson.Dumps(got, pyjson.Options{SortKeys: true}) != pyjson.Dumps(tc.Result, pyjson.Options{SortKeys: true})) {
				t.Fatalf("diff: Go=(%v,%v) golden=(%v,%v)", got, failure, tc.Result, tc.Error)
			}
		})
	}
}

// envelopeAccessorValues is every JSON value shape the envelope accessors are asked about.
const envelopeAccessorValues = `[null, false, true, 0, 2, 1.5, "", "x", [], [1], {}, {"a": 1}]`

// envelopeAccessorRows is Shown's answer for each value, directly ("shown") and as an absence's
// detail ("detail"), as json.dumps renders the rows.
func envelopeAccessorRows() []byte {
	var rows []any
	for _, value := range Items(Decode(envelopeAccessorValues)) {
		for _, op := range []string{"shown", "detail"} {
			result, failure := envelopeAccessor(op, value)
			rows = append(rows, contract.OrderedObject{{Key: "op", Value: op}, {Key: "value", Value: value}, {Key: "result", Value: result}, {Key: "error", Value: failure}})
		}
	}
	return []byte(pyjson.Dumps(rows, pyjson.Options{}) + "\n")
}

// envelopeAccessor runs one accessor on value: its result, or the refusal's text (nil when none).
func envelopeAccessor(op string, value any) (result, failure any) {
	err := func() (err error) {
		defer RecoverPython(&err)
		if op == "shown" {
			result = Shown(value)
		} else {
			result = Shown(map[string]any{"absent": "unknown", "detail": value})
		}
		return nil
	}()
	if err != nil {
		return nil, err.Error()
	}
	return result, nil
}
