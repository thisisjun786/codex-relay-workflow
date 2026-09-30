package skill

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func call(args []string, input string) (int, string, string) {
	var out, err bytes.Buffer
	code := Run(args, strings.NewReader(input), &out, &err)
	return code, out.String(), err.String()
}
func TestSkillFixtureReplays(t *testing.T) {
	for _, args := range [][]string{{"hook-probe", "replay"}, {"parent-title", "replay"}, {"start-policy", "selftest"}} {
		code, out, err := call(args, "")
		if code != 0 {
			t.Fatalf("%v: %d\n%s\n%s", args, code, out, err)
		}
	}
}

// The shipped fixtures keep changing, so the replays over them are held to what the fixtures
// themselves say rather than to a recorded Python answer: every decision fixture matches, every
// documented trace has a fixture, every return site is reached, and every title fixture replays.
// The Python parity of replay is kept over the frozen inputs (python_oracle_test.go).
func TestSkillReplaysEveryShippedFixture(t *testing.T) {
	count := func(name string) int {
		paths, err := fs.Glob(bundledSkillFiles, defaultFixture(name)+"/*.json")
		if err != nil || len(paths) == 0 {
			t.Fatalf("%s fixtures: %d, %v", name, len(paths), err)
		}
		return len(paths)
	}
	traces, err := replayTraceIDs(bundledSkillFiles, defaultContract("hook-contract.md"))
	if err != nil || len(traces) == 0 {
		t.Fatalf("documented traces: %v %v", traces, err)
	}
	for _, test := range []struct {
		args  []string
		lines []string
	}{
		{[]string{"hook-probe", "replay"}, []string{
			fmt.Sprintf("%d/%d fixtures matched", count("decisions"), count("decisions")),
			fmt.Sprintf("documented traces backed by a fixture: %d/%d", len(traces), len(traces)),
			fmt.Sprintf("return-site coverage: %d/%d sites reached", len(hookReturnSites), len(hookReturnSites)),
		}},
		{[]string{"parent-title", "replay"}, []string{
			fmt.Sprintf("Replayed %d title fixtures against their recorded expectations.", count("titles")),
		}},
	} {
		t.Run(test.args[0], func(t *testing.T) {
			code, out, errOut := call(test.args, "")
			if code != 0 || errOut != "" {
				t.Fatalf("%v: exit %d\n%s\n%s", test.args, code, out, errOut)
			}
			for _, line := range test.lines {
				if !strings.Contains(out, "\n"+line+"\n") && !strings.HasPrefix(out, line+"\n") {
					t.Errorf("%v does not print %q:\n%s", test.args, line, out)
				}
			}
		})
	}
}

func TestParentTitleMutationFails(t *testing.T) {
	src := filepath.Join(repositoryRoot(), "plugins", defaultFixture("titles"), "family-crw-verbatim.json")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), "[CRW] 설치형 플러그인 전환", "[WRONG] 설치형 플러그인 전환", 1)
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "wrong.json"), []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := call([]string{"parent-title", "replay", "--fixtures", dir, "--allow-unreached"}, "")
	if code != 1 || !strings.Contains(stderr, "wrong.json: title expected") {
		t.Fatalf("mutation survived: %d %q", code, stderr)
	}
}
func TestParentTitleCommands(t *testing.T) {
	request := `{"role":"parent","binding_verified":true,"project_labels":["CRW"],"family_candidates":["CRW"],"observed_title":"설치형 플러그인 전환","user_title":"none"}`
	code, out, err := call([]string{"parent-title", "decide"}, request)
	if code != 0 || err != "" || !strings.Contains(out, "[CRW] 설치형 플러그인 전환") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	code, out, _ = call([]string{"parent-title", "readback"}, `{"requested_title":"x","observed_title":null}`)
	if code != 1 || !strings.Contains(out, "unread") {
		t.Fatalf("%d %q", code, out)
	}
}
func TestStartPolicyCheck(t *testing.T) {
	code, out, err := call([]string{"start-policy", "check"}, "run_mode: loop\nobservation_path: blocked\n")
	if code != 0 || err != "" || !strings.Contains(out, "pairing: loop + blocked -> legal") {
		t.Fatalf("%d %q %q", code, out, err)
	}
}
