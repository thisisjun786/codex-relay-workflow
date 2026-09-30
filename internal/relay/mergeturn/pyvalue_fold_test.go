package mergeturn

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

func TestFoldSHA256Hex(t *testing.T) {
	pyjsontest.SameText(t, "sha256Hex", pyjsontest.Values(), pyjsontest.Types("string"), func(v any) string { return sha256Hex(v.(string)) }, func(v any) string { return pyvalue.SHA256Hex(v.(string)) })
}
