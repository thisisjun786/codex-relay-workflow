package skill

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchShowDispatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRW_HOME", home)
	t.Setenv("CRW_BIN", "crw")
	cache := filepath.Join(home, "skill-cache")
	if err := os.MkdirAll(cache, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "jaw-aHR0cHM6Ly9yYXcuZ2l0aHVi.cache"), []byte(`{"skills":{"tdd":{"name":"TDD","description":"test driven development","superseded_by":"dev-testing"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		exit int
		out  string
	}{
		{[]string{"search"}, 1, "crw skill <search"},
		{[]string{"show"}, 1, "crw skill <search"},
		{[]string{"search", "tdd", "--json"}, 0, `"supersededBy": "dev-testing"`},
		{[]string{"search", "tdd"}, 0, "crw skill show <id>"},
	} {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := Run(c.args, strings.NewReader(""), &out, &errOut)
			if code != c.exit || !strings.Contains(out.String(), c.out) || errOut.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q; want exit=%d containing %q", code, out.String(), errOut.String(), c.exit, c.out)
			}
		})
	}
}
