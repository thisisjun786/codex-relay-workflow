//go:build dev

package cxccorpus

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A wait step is how a scenario outlasts a detached process: it returns when a file the glob names is there, and the next step sees it.
func TestRunScenario_waits_for_a_file_a_detached_process_writes(t *testing.T) {
	o := shOptions(t)
	go func() { // the file comes 300 ms after the case's workspace exists, however long the case takes to be made
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if dirs, _ := filepath.Glob(filepath.Join(o.Scratch, "cxc-rec-*", "ws")); len(dirs) > 0 {
				time.Sleep(300 * time.Millisecond)
				if err := os.WriteFile(filepath.Join(dirs[0], "late.exit"), []byte("3"), 0o644); err != nil {
					t.Error(err)
				}
				return
			}
		}
	}()
	got, err := RunScenario(shRuntime{}, o, Scenario{ID: "cli__sh__waits", Steps: []Step{{Wait: "ws/*.exit"}, {CLI: []string{"read code < late.exit; printf %s \"$code\""}}}})
	if err != nil || len(got.Steps) != 2 || got.Steps[0].Action != "wait" || got.Steps[1].StdoutBytes() != "3" {
		t.Fatalf("%+v %v", got.Steps, err)
	}
}

func TestRunScenario_a_wait_that_never_ends_or_mixes_kinds_is_an_error(t *testing.T) {
	o := shOptions(t)
	o.Timeout = 200 * time.Millisecond
	for want, step := range map[string]Step{"no file matches ws/*.exit": {Wait: "ws/*.exit"}, "runs nothing else": {Wait: "ws/*", CLI: []string{"true"}},
		"not under one of": {Wait: "../x"}} {
		if _, err := RunScenario(shRuntime{}, o, Scenario{ID: "cli__sh__waits", Steps: []Step{step}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v, want an error with %q", err, want)
		}
	}
}

// The pid a ps call names is live, so the recorder cannot normalise it by context: a number after -p is <PID>, anything else stays.
func TestReadCalls_names_the_pid_of_a_ps_call(t *testing.T) {
	c := &Case{Root: t.TempDir()}
	if err := os.Mkdir(filepath.Join(c.Root, ".rec"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := "{\"cmd\":\"ps\",\"argv\":[\"-o\",\"lstart=\",\"-p\",\"48213\"],\"cwd\":\"/x\"}\n{\"cmd\":\"gh\",\"argv\":[\"-p\",\"48213\"],\"cwd\":\"/x\"}\n{\"cmd\":\"ps\",\"argv\":[\"-p\",\"abc\"],\"cwd\":\"/x\"}\n"
	if err := os.WriteFile(filepath.Join(c.Root, ".rec", "calls.jsonl"), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	calls, err := readCalls(c, testNormaliser(t).NewSession(nil))
	if err != nil || len(calls) != 3 || !slices.Equal(calls[0].Argv, []string{"-o", "lstart=", "-p", "<PID>"}) || calls[1].Argv[1] != "48213" || calls[2].Argv[1] != "abc" {
		t.Errorf("%+v %v", calls, err)
	}
}
