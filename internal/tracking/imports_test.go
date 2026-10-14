package tracking

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The adapter is a leaf: it imports none of the packages whose state the tracking moves out of,
// and not the config package another lane is changing.
func TestPackageImportsNoRelayFlowManageOrConfig(t *testing.T) {
	const mod = "github.com/thisisjun786/codex-relay-workflow/internal/"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			for _, banned := range []string{"relay", "flow", "manage", "crwconfig"} {
				if path == mod+banned || strings.HasPrefix(path, mod+banned+"/") {
					t.Errorf("%s imports %s", e.Name(), path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Go files read")
	}
}
