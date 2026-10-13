package compile

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t testing.TB, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunCLI(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCLICompileAndDryRun(t *testing.T) {
	file := fixtureDir + "initiative.input.json"
	code, out, errOut := runCLI(t, "", file)
	if code != 0 || !onlyTargetWarnings(errOut, "PAPI") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	golden, err := os.ReadFile(fixtureDir + "initiative.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(golden) {
		t.Errorf("crw flow compile prints other bytes than the golden file\n%s", firstDifference(string(golden), out))
	}

	code, dry, errOut := runCLI(t, "", "--dry-run", file)
	if code != 0 || !onlyTargetWarnings(errOut, "PAPI") {
		t.Fatalf("dry run: exit %d, stderr %q", code, errOut)
	}
	var res Result
	if err := json.Unmarshal([]byte(dry), &res); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dry, `"revision":`) || strings.Contains(dry, `"schema": "dag-plan-revision/1"`) {
		t.Error("a dry run printed a revision document")
	}
	if len(res.Plans) != 4 || res.Plans[0].Preview.ParallelWidth == 0 || len(res.Plans[0].Preview.CriticalPath) == 0 {
		t.Errorf("dry run result %+v", res.Plans)
	}

	// the same handoff on stdin
	raw, _ := os.ReadFile(file)
	if code, again, _ := runCLI(t, string(raw), "-"); code != 0 || again != out {
		t.Errorf("stdin: exit %d, same bytes %v", code, again == out)
	}
}

// onlyTargetWarnings: the stderr of the fixture's compile is the two target warnings of the one project that keeps the old target
// form (OWNER/NAME and refs/heads/BRANCH), and nothing else.
func onlyTargetWarnings(errOut, project string) bool {
	lines := strings.Split(strings.TrimSuffix(errOut, "\n"), "\n")
	return len(lines) == 2 &&
		strings.HasPrefix(lines[0], "crw flow compile: warning: [target_repository] "+project+" target.repository: ") &&
		strings.HasPrefix(lines[1], "crw flow compile: warning: [target_base_ref] "+project+" target.base_ref: ")
}

func TestCLIRefusalsAndUsage(t *testing.T) {
	dir := t.TempDir()
	cyc := filepath.Join(dir, "cycle.json")
	doc := `{"schema":"crw-flow-compile-input/1","projects":[{"key":"P","target":{"repository":"r","base_ref":"b"},"issues":[
	  {"key":"A","title":"a","criteria":["c"],"after":["B"]},{"key":"B","title":"b","criteria":["c"],"after":["A"]}]}]}`
	if err := os.WriteFile(cyc, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "", "--dry-run", cyc)
	if code != 1 || !strings.Contains(errOut, "cycle") {
		t.Errorf("refused handoff: exit %d, stderr %q", code, errOut)
	}
	var refusal struct {
		OK       bool      `json:"ok"`
		Refusals []Refusal `json:"refusals"`
	}
	if err := json.Unmarshal([]byte(out), &refusal); err != nil || refusal.OK || len(refusal.Refusals) == 0 || refusal.Refusals[0].Rule != "cycle" {
		t.Errorf("stdout %q: %v", out, err)
	}

	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"schema":`), 0o600)
	for name, args := range map[string][]string{
		"no file":      {},
		"two files":    {cyc, cyc},
		"missing file": {filepath.Join(dir, "none.json")},
		"not json":     {bad},
		"unknown flag": {"--grade", cyc},
		"missing base": {"--base", filepath.Join(dir, "none.json"), cyc},
	} {
		if code, _, errOut := runCLI(t, "", args...); code != 2 || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
	}
	if code, out, _ := runCLI(t, "", "--help"); code != 0 || !strings.Contains(out, "usage: crw flow compile") {
		t.Errorf("help: exit %d, %q", code, out)
	}
}
