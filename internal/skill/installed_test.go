package skill

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

type skillProcessResult struct {
	exit           int
	stdout, stderr string
}

func captureSkillProcess(t testing.TB, command *exec.Cmd) skillProcessResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	exit := 0
	if err := command.Run(); err != nil {
		var failed *exec.ExitError
		if !errors.As(err, &failed) {
			t.Fatal(err)
		}
		exit = failed.ExitCode()
	}
	return skillProcessResult{exit, stdout.String(), stderr.String()}
}

// An installed, release-shaped binary run from a directory with no checkout answers every skill
// command exactly as this package answers it in process: it reads its own embedded contracts
// and fixtures, never a checkout found from its working directory.
func TestSkillInstalledOutsideCheckout(t *testing.T) {
	// Given an installed, release-shaped binary and a cwd without any checkout.
	dir := t.TempDir()
	binary := testsupport.CRW(t)
	for _, test := range []struct {
		name     string
		args     []string
		input    string
		wantExit int
	}{
		{"hook replay", []string{"hook-probe", "replay"}, "", 0},
		{"title replay", []string{"parent-title", "replay"}, "", 0},
		{"policy selftest", []string{"start-policy", "selftest"}, "", 0},
		{"policy vocabulary", []string{"start-policy", "vocabulary"}, "", 0},
		{"policy legal", []string{"start-policy", "check"}, "run_mode: loop\nobservation_path: blocked\n", 0},
		{"policy illegal", []string{"start-policy", "check"}, "run_mode: blocked\nobservation_path: event-driven-idle\n", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			// When the installed binary runs from that cwd with nothing else on PATH.
			command := exec.Command(binary, append([]string{"skill"}, test.args...)...)
			command.Dir, command.Stdin = dir, strings.NewReader(test.input)
			command.Env = oracleEnv("PATH=" + dir)
			got := captureSkillProcess(t, command)
			// Then exit code and both streams are the in-process answer, byte for byte.
			code, stdout, stderr := call(test.args, test.input)
			if want := (skillProcessResult{code, stdout, stderr}); got != want || got.exit != test.wantExit {
				t.Fatalf("installed CLI mismatch (want exit %d)\nin process: %+v\ninstalled:  %+v", test.wantExit, want, got)
			}
		})
	}
}

// The start-policy commands answer from the contract they embed: the selftest and the vocabulary
// succeed, and a record is judged by the pairing table.
func TestSkillStartPolicyCommands(t *testing.T) {
	for _, test := range []struct {
		name, input string
		args        []string
		wantExit    int
		want        string
	}{
		{"selftest", "", []string{"selftest"}, 0, "vocabulary: "},
		{"vocabulary", "", []string{"vocabulary"}, 0, "legal pairings, each one two lines to copy:\n"},
		{"passing check", "run_mode: loop\nobservation_path: blocked\n", []string{"check"}, 0, "run_mode: loop -> ok\nobservation_path: blocked -> ok\npairing: loop + blocked -> legal\n"},
		{"failing check", "run_mode: blocked\nobservation_path: event-driven-idle\n", []string{"check"}, 1, "pairing: blocked + event-driven-idle -> illegal; this is a record to repair\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := call(append([]string{"start-policy"}, test.args...), test.input)
			if code != test.wantExit || stderr != "" || !strings.Contains(stdout, test.want) {
				t.Fatalf("exit %d (want %d), stdout %q (want to contain %q), stderr %q", code, test.wantExit, stdout, test.want, stderr)
			}
		})
	}
}
