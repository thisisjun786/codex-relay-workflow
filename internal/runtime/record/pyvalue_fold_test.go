package record

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
)

func TestFoldRecordTypeName(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) { return reading.Decode([]byte(doc)) })
	keep := pyjsontest.Types("pyjson.Object", "map[string]interface {}", "[]interface {}", "string", "bool", "nil", "int64", "float64", "json.Number")
	pyjsontest.SameText(t, "pyType", values, pyjsontest.Both(keep, pyjsontest.IntegerNumbers), pyType, pyvalue.TypeName)
}
