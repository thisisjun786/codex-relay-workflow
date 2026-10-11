package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1189 (R3-S2-F3, R3-S2-F4): the answers of both guards to the trial's commands say which program is refused and give the way
// past it, uncut, and are still denials.
func TestCRW1189GuardAnswersNameTheProgramAndTheCacheRemoval(t *testing.T) {
	cwd, _, env := gateScene(t)
	for _, name := range []string{"calc.py", "test_calc.py"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	carried := "awk '{print}' test_calc.py | git apply --check; python3 -B -m unittest -v"
	mem := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, carried), env))
	gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, carried)))
	for _, r := range []string{mem, gh} {
		for _, want := range []string{"[crw command-reader]", "the `awk` program may rewrite module imports", "run it in a separate command, before or after the module run"} {
			if !strings.Contains(r, want) {
				t.Errorf("the carried program refusal lacks %q: %s", want, r)
			}
		}
		if strings.Contains(r, "...") {
			t.Errorf("the reason is cut: %s", r)
		}
	}

	crw1178Cache(t, cwd)
	for _, r := range []string{
		gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, "python3 -m unittest"), env)),
		githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "python3 -m unittest"))),
	} {
		for _, want := range []string{"`rm` that file", "`find . -name __pycache__ -prune -exec rm -r {} +`", "in a separate command first"} {
			if !strings.Contains(r, want) {
				t.Errorf("the cache refusal lacks %q: %s", want, r)
			}
		}
		if strings.Contains(r, "...") || strings.Contains(r, "remove __pycache__") {
			t.Errorf("the reason is cut or asks for the refused form: %s", r)
		}
	}
}
