package compile

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// TestCompileUsageNamesTheChildBinding (CRW-1478): the help of `crw flow compile` names the 자식 binding the project key is
// compared with, in the words crw flow start and the key warning use, and carries no bare role word.
func TestCompileUsageNamesTheChildBinding(t *testing.T) {
	if hits := layername.Find(usageHelp); len(hits) != 0 {
		t.Errorf("bare role words %v in the usage", hits)
	}
	for _, want := range []string{"the owner thread whose live 자식 bindings each project key is compared with", "holds no live 자식 binding"} {
		if !strings.Contains(usageHelp, want) {
			t.Errorf("the usage lacks %q", want)
		}
	}
}
