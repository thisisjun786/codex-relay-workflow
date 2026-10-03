//go:build dev

package cxccorpus

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A wait step names the file it found: later steps read its stem as ${WAITED}, which is how a scenario reaches the id of a job a detached
// process made. The newest wait wins, and an empty stem (.exit) is still a binding.
func TestRunScenario_names_the_waited_file_in_later_steps(t *testing.T) {
	o := shOptions(t)
	go func() { // the files come 200 ms after the case's workspace exists
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if dirs, _ := filepath.Glob(filepath.Join(o.Scratch, "cxc-rec-*", "ws")); len(dirs) > 0 {
				time.Sleep(200 * time.Millisecond)
				for _, name := range []string{"late-7.exit", "other-9.exit", ".exit"} {
					if err := os.WriteFile(filepath.Join(dirs[0], name), nil, 0o644); err != nil {
						t.Error(err)
					}
				}
				return
			}
		}
	}()
	say := Step{CLI: []string{`printf '[%s]' "${WAITED}"`}}
	got, err := RunScenario(shRuntime{}, o, Scenario{ID: "cli__sh__names_the_waited_file", Steps: []Step{{Wait: "ws/late-*.exit"}, say, {Wait: "ws/other-*.exit"}, say, {Wait: "ws/.exit"}, say}})
	if err != nil || len(got.Steps) != 6 || got.Steps[1].StdoutBytes() != "[late-7]" || got.Steps[3].StdoutBytes() != "[other-9]" || got.Steps[5].StdoutBytes() != "[]" {
		t.Fatalf("%+v %v", got.Steps, err)
	}
}

// Before any wait the placeholder stays as written.
func TestCase_Expand_leaves_the_waited_placeholder_until_a_wait(t *testing.T) {
	c := &Case{Root: t.TempDir()}
	if got := c.Expand("bg cancel ${WAITED}"); got != "bg cancel ${WAITED}" {
		t.Errorf("%q", got)
	}
}
