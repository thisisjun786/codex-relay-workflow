package hook

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// The hook's truth and os.fsencode answer as internal/pyvalue does over the values its reader
// builds.
func TestFoldHookValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) { return Decode([]byte(doc)) })
	// Its reader leaves a json.Number only for an integer past int64.
	keep := pyjsontest.Both(pastInt64, pyjsontest.Types("pyjson.Object", "map[string]interface {}", "[]interface {}", "string", "bool", "nil", "int64", "float64", "json.Number"))
	pyjsontest.SameTruth(t, "truthy", values, keep, truthy, pyvalue.Truthy)
	pyjsontest.SameText(t, "fsencode", values, pyjsontest.Types("string"), func(v any) string {
		path, ok := fsencode(v.(string))
		return path + map[bool]string{true: "|ok", false: "|refused"}[ok]
	}, func(v any) string {
		path, ok := pyvalue.FSEncode(v.(string))
		return path + map[bool]string{true: "|ok", false: "|refused"}[ok]
	})
}

func pastInt64(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return true
	}
	_, err := strconv.ParseInt(string(n), 10, 64)
	return !strings.ContainsAny(string(n), ".eE") && err != nil
}
