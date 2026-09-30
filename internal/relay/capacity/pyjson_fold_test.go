package capacity

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

func TestFoldCapacityFloat(t *testing.T) {
	for _, f := range pyjsontest.Floats() {
		if want, got := pyFloat(f), pyjson.Float(f); want != got {
			t.Errorf("pyFloat(%v) = %q, folded %q", f, want, got)
		}
	}
}
