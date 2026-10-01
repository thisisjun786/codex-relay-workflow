package execution

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

func TestFoldExecutionStrip(t *testing.T) {
	for r := rune(0); r <= 0x10ffff; r++ {
		s := string(r) + "x" + string(r)
		if want, got := pyStrip(s), pyvalue.Strip(s); want != got {
			t.Errorf("pyStrip(%q) = %q, folded %q", s, want, got)
		}
	}
}
