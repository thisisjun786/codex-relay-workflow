package configguard

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The text-lines helpers config-guard uses are the ones of internal/pabcd/text: the oracle's text-lines.ts is byte-identical in all five
// components that carry it (sha256 e43a46a40971277b79cf909efd3e284e3337e739e9f925f35b123831617d94bc), and the rows the recorder took
// from it must come out of that package unchanged.
func TestTextLinesAreTheOraclesHelpers(t *testing.T) {
	n := 0
	for _, row := range tomlOracleRows(t) {
		var want any
		if err := json.Unmarshal(row.Out, &want); err != nil {
			t.Fatal(err)
		}
		var got any
		switch row.Fn {
		case "splitLines":
			got = text.SplitLines(tomlArg[string](t, row, 0))
		case "splitLinesByteExact":
			got = text.SplitLinesByteExact(tomlArg[string](t, row, 0))
		case "dominantEol":
			got = string(text.DominantEOL(tomlArg[string](t, row, 0)))
		case "withEol":
			got = text.WithEOL(tomlArg[string](t, row, 0), text.EOL(tomlArg[string](t, row, 1)))
		default:
			continue
		}
		n++
		if g := tomlCanon(t, got); !reflect.DeepEqual(g, want) {
			t.Errorf("%s %s: got %v, want %v", row.Fn, row.In, g, want)
		}
	}
	if n == 0 {
		t.Fatal("no text-lines rows")
	}
}
