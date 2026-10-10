package pyjson_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A Skim reading accepts and refuses exactly what the Deep reading does, with the same error, and
// answers no value for a container.
func TestLoads_skim_agrees_with_the_deep_reading(t *testing.T) {
	deep := strings.Repeat("[", 12000)
	docs := []string{
		``, ` `, `null`, `1`, `"a"`, `[]`, `{}`, `[1,2,[3,{"a":[]}]]`, `{"a":1,"b":{"c":[true,false,null]}}`,
		`[1,]`, `[1 2]`, `{"a":1,}`, `{"a" 1}`, `{1:2}`, `[`, `{`, `[1`, `{"a":1`, `[{]`, `[}]`, `{"a":[}}`,
		`[1]x`, `[1] [2]`, `["\ud800"]`, `["\u12"]`, `[NaN]`, `{"a":Infinity}`, `[01]`, `[1e5,-0.5]`,
		deep + strings.Repeat("]", 12000), deep + strings.Repeat("]", 11999), deep + `,` + strings.Repeat("]", 12000),
		deep + `1` + strings.Repeat("]", 12000), strings.Repeat(`{"a":`, 12000) + `1` + strings.Repeat(`}`, 12000),
		strings.Repeat(`{"a":`, 12000) + `1` + strings.Repeat(`}`, 11999), strings.Repeat(`{"a":[`, 6000) + strings.Repeat(`]}`, 6000),
		strings.Repeat(`{"a":[`, 6000) + strings.Repeat(`}]`, 6000),
	}
	for _, trailing := range []pyjson.Trailing{pyjson.TrailingNothing, pyjson.TrailingClose, pyjson.TrailingAnything} {
		for _, doc := range docs {
			options := pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers, Trailing: trailing, Deep: true}
			_, want := pyjson.Loads(doc, options)
			options.Skim = true
			got, err := pyjson.Loads(doc, options)
			if (err == nil) != (want == nil) || err != nil && err.Error() != want.Error() {
				name := doc
				if len(name) > 40 {
					name = name[:40]
				}
				t.Errorf("Skim Loads(%q..., trailing %d) = %v, Deep gives %v", name, trailing, err, want)
			}
			if got != nil && strings.ContainsAny(doc[:min(1, len(doc))], "[{") {
				t.Errorf("Skim Loads answered a value %v", got)
			}
		}
	}
}

// A Skim reading of a document nested millions deep allocates in proportion to the document, not to
// a frame and a value per level.
func TestLoads_skim_does_not_build_the_value(t *testing.T) {
	doc := strings.Repeat("[", 2_000_000) + strings.Repeat("]", 2_000_000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := pyjson.Loads(doc, pyjson.LoadOptions{Deep: true, Skim: true})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("allocated %d MiB for a %d byte document, want under 16", allocated>>20, len(doc))
	}
}
