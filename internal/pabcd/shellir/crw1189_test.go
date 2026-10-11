package shellir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func crw1189Reason(t *testing.T, cmd, cwd string) string {
	t.Helper()
	_, err := Analyze(cmd, cwd)
	var u *Unreadable
	if !errors.As(err, &u) {
		t.Fatalf("%q: allowed or failed otherwise: %v", cmd, err)
	}
	return u.Reason
}

// CRW-1189 (R3-S2-F3): the refusal of a carried program next to a module run names the program and the way past it, and the verdict
// of CRW-1178's conservative rule is unchanged (the command is still refused).
func TestCRW1189CarriedProgramRefusalNamesTheProgramAndTheRoute(t *testing.T) {
	cwd := crw1178Project(t, nil)
	if err := os.WriteFile(filepath.Join(cwd, "fix.sh"), []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cmd, program string }{
		{"awk '{print}' test_calc.py | git apply --check; python3 -B -m unittest -v", "`awk`"},
		{"sed -n p calc.py | cat; python3 -m unittest", "`sed`"},
		{"python3 -c 'print(1)'; python3 -m unittest", "`python3`"},
		{"tar xf a.tar; python3 -m unittest", "`tar`"},
		{"sh fix.sh; python3 -m unittest", "`fix.sh`"},
	} {
		t.Run(c.cmd, func(t *testing.T) {
			reason := crw1189Reason(t, c.cmd, cwd)
			for _, want := range []string{c.program, "may rewrite module imports", "separate command", "before or after the module run"} {
				if !strings.Contains(reason, want) {
					t.Errorf("the reason lacks %q: %q", want, reason)
				}
			}
			if len(reason) > 320 || strings.Contains(reason, "...") {
				t.Errorf("the reason must fit the hooks' reason bound uncut: %d %q", len(reason), reason)
			}
		})
	}
	// the carried program that runs beside the module run is named too
	reason := crw1189Reason(t, "python3 -m unittest | awk '{print}'", cwd)
	if !strings.Contains(reason, "`awk`") || !strings.Contains(reason, "beside the module run") || !strings.Contains(reason, "separate command") {
		t.Errorf("beside: %q", reason)
	}
	// the verdict is the same without the program in the text: the module run alone is allowed
	if _, err := Analyze("python3 -B -m unittest -v", cwd); err != nil {
		t.Errorf("the module run alone: %v", err)
	}
	// and the program alone is not a module run, so it is not refused by this rule
	if _, err := Analyze("awk '{print}' calc.py", cwd); err != nil {
		t.Errorf("the program alone: %v", err)
	}
}

// CRW-1189 (R3-S2-F4): the way past a cache refusal is spelled in forms the Codex execution policy lets through (`rm <file>` and
// `find ... -prune -exec rm -r {} +`), never as a request to remove __pycache__, which an agent spells `rm -rf` and the policy refuses.
func TestCRW1189CacheRefusalGivesACommandTheHostPolicyAccepts(t *testing.T) {
	for _, name := range []string{"__pycache__/calc.cpython-312.pyc", "pkg/__pycache__/calc.cpython-312.pyc", "legacy.pyc"} {
		t.Run(name, func(t *testing.T) {
			cwd := crw1178Project(t, map[string][]byte{name: append(pycHeader(0), "code"...)})
			reason := crw1189Reason(t, "python3 -B -m unittest", cwd)
			if !strings.Contains(reason, name) || !strings.Contains(reason, "`rm` that file") || !strings.Contains(reason, "separate command first") {
				t.Errorf("the reason lacks the file or the rm route: %q", reason)
			}
			if strings.HasSuffix(filepath.Dir(name), "__pycache__") && !strings.Contains(reason, "`find . -name __pycache__ -prune -exec rm -r {} +`") {
				t.Errorf("the cache entry's reason lacks the find route: %q", reason)
			}
			if strings.Contains(reason, "remove __pycache__") || strings.Contains(reason, "rm -rf") || strings.Contains(reason, "rm -f") {
				t.Errorf("the reason must not ask for the form the host policy refuses: %q", reason)
			}
		})
	}
	// the longest name the reason carries still fits the hooks' bound uncut
	long := "__pycache__/" + strings.Repeat("a", 200) + ".cpython-312.pyc"
	cwd := crw1178Project(t, map[string][]byte{long: append(pycHeader(0), "code"...)})
	reason := crw1189Reason(t, "python3 -B -m unittest", cwd)
	if len(reason) > 320 {
		t.Errorf("reason over the bound: %d", len(reason))
	}
}
