package reading

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

func TestFoldFSEncode(t *testing.T) {
	values := append(pyjsontest.Values(), "\xed\xb2\x80", "a\xed\xb3\xbfb", "\xed\xa0\x80", "\xed\xb1\xbf", "\xed\xb2")
	pyjsontest.SameText(t, "FSEncode", values, pyjsontest.Types("string"), func(v any) string {
		path, ok := FSEncode(v.(string))
		return path + map[bool]string{true: "|ok", false: "|refused"}[ok]
	}, func(v any) string {
		path, ok := pyvalue.FSEncode(v.(string))
		return path + map[bool]string{true: "|ok", false: "|refused"}[ok]
	})
}
