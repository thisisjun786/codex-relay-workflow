//go:build dev

package cxccorpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A seed runs before each step that starts a process, with the environment that process gets: the
// step's own env and unset names are in it, so a harness puts its file where the process will look.
// The undos run in reverse after the last step, and the file is gone when the tree is observed.
func TestRunScenario_seedsEveryStepWithTheEnvironmentItRunsIn(t *testing.T) {
	o := shOptions(t)
	var seen []string
	var order []string
	o.Seed = func(c *Case, env []string) (func() error, error) {
		home := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "ELSEWHERE="); ok {
				home = v
			}
		}
		seen = append(seen, home)
		mark := filepath.Join(c.Root, "ws", "seeded-"+string(rune('a'+len(seen))))
		if err := os.WriteFile(mark, nil, 0o644); err != nil {
			return nil, err
		}
		return func() error {
			order = append(order, filepath.Base(mark))
			return os.Remove(mark)
		}, nil
	}
	s := Scenario{ID: "cli__sh__seed", Steps: []Step{
		{CLI: []string{`for f in *; do printf "%s " "$f"; done`}, Env: map[string]string{"ELSEWHERE": "first"}},
		{CLI: []string{`for f in *; do printf "%s " "$f"; done`}, Env: map[string]string{"ELSEWHERE": "second"}},
		{CLI: []string{`for f in *; do printf "%s " "$f"; done`}, Env: map[string]string{"ELSEWHERE": "gone"}, Unset: []string{"ELSEWHERE"}},
	}}
	got, err := RunScenario(shRuntime{}, o, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != "first" || seen[1] != "second" || seen[2] != "" {
		t.Errorf("the seed saw the step environments %q", seen)
	}
	if strings.Join(order, ",") != "seeded-d,seeded-c,seeded-b" {
		t.Errorf("undo order %q", order)
	}
	for i, want := range []string{"seeded-b", "seeded-b seeded-c", "seeded-b seeded-c seeded-d"} {
		if out := strings.TrimSpace(got.Steps[i].StdoutBytes()); !strings.Contains(out, want) {
			t.Errorf("step %d listed %q, want it to hold %q (the seed of this step is in place)", i, out, want)
		}
	}
}
