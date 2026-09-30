package pyjson_test

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// A Python reading refuses exactly what json.loads refuses, with its text; an encoding/json
// reading refuses exactly what encoding/json refuses.
func TestLoads_refuses_what_its_reading_refuses(t *testing.T) {
	for _, doc := range pyjsontest.Sample(t) {
		_, err := pyjson.Loads(doc, pyjson.LoadOptions{Python: true, Constants: true, Deep: true})
		if message := pyjson.Error(doc); (err == nil) != (message == "") || err != nil && err.Error() != message {
			t.Errorf("Loads(%q) python reading: %v, json.loads: %q", doc, err, message)
		}
		_, err = pyjson.Loads(doc, pyjson.LoadOptions{})
		if valid := json.Valid([]byte(doc)); valid != (err == nil) {
			t.Errorf("Loads(%q) encoding/json reading: %v, json.Valid %v", doc, err, valid)
		}
	}
}

// What Dumps writes of a value Loads read is read back as the same value.
func TestDumps_writes_what_Loads_reads_back(t *testing.T) {
	options := pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers}
	for _, doc := range pyjsontest.Sample(t) {
		value, err := pyjson.Loads(doc, options)
		if err != nil || len(doc) > 1<<14 {
			continue // an indent grows with the depth: a document 10000 deep is written a line per level
		}
		for _, o := range []pyjson.Options{{}, {Indent: 2, SortKeys: true}, {Compact: true, Unicode: true}} {
			text := pyjson.Dumps(value, o)
			again, err := pyjson.Loads(text, options)
			if err != nil || !pyjsontest.Same(value, again) && !o.SortKeys {
				t.Errorf("Dumps(Loads(%q), %+v) = %q, read back as %#v, %v", doc, o, text, again, err)
			}
		}
	}
}
