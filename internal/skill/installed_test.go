package skill

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type skillProcessResult struct {
	exit           int
	stdout, stderr string
}

func captureSkillProcess(t *testing.T, command *exec.Cmd) skillProcessResult {
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

func TestSkillInstalledOutsideCheckoutLivePython(t *testing.T) {
	// Given an installed, release-shaped binary and a cwd without any checkout.
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	root := repositoryRoot()
	dir := t.TempDir()
	binary := filepath.Join(dir, "crw")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/crw")
	build.Dir = root
	if result := captureSkillProcess(t, build); result.exit != 0 {
		t.Fatalf("build failed: %+v", result)
	}
	for _, test := range []struct {
		name, script string
		args         []string
		input        string
		wantExit     int
	}{
		{"hook replay", "hook_probe.py", []string{"hook-probe", "replay"}, "", 0},
		{"title replay", "parent_title.py", []string{"parent-title", "replay"}, "", 0},
		{"policy selftest", "start_policy.py", []string{"start-policy", "selftest"}, "", 0},
		{"policy vocabulary", "start_policy.py", []string{"start-policy", "vocabulary"}, "", 0},
		{"policy legal", "start_policy.py", []string{"start-policy", "check"}, "run_mode: loop\nobservation_path: blocked\n", 0},
		{"policy illegal", "start_policy.py", []string{"start-policy", "check"}, "run_mode: blocked\nobservation_path: event-driven-idle\n", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			// When Python and the installed binary run identical inputs from that cwd.
			pythonArgs := append([]string{filepath.Join(root, "plugins/crw/skills/crw-run/scripts", test.script)}, test.args[1:]...)
			python := exec.Command(filepath.Join(root, ".venv/bin/python"), pythonArgs...)
			python.Dir, python.Stdin = dir, strings.NewReader(test.input)
			want := captureSkillProcess(t, python)
			if want.exit != test.wantExit {
				t.Fatalf("Python oracle failed: %+v", want)
			}
			command := exec.Command(binary, append([]string{"skill"}, test.args...)...)
			command.Dir, command.Stdin = dir, strings.NewReader(test.input)
			command.Env = oracleEnv("PATH=" + dir)
			got := captureSkillProcess(t, command)
			// Then exit code and both streams must agree byte for byte.
			if got != want {
				t.Fatalf("installed CLI mismatch\nPython: %+v\nGo: %+v", want, got)
			}
		})
	}
}

func TestSkillStartPolicyLivePython(t *testing.T) {
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	root := repositoryRoot()
	for _, test := range []struct {
		name, input string
		args        []string
		wantExit    int
	}{
		{"selftest", "", []string{"selftest"}, 0},
		{"vocabulary", "", []string{"vocabulary"}, 0},
		{"passing check", "run_mode: loop\nobservation_path: blocked\n", []string{"check"}, 0},
		{"failing check", "run_mode: blocked\nobservation_path: event-driven-idle\n", []string{"check"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			pythonArgs := append([]string{filepath.Join(root, "plugins/crw/skills/crw-run/scripts/start_policy.py")}, test.args...)
			python := exec.Command(filepath.Join(root, ".venv/bin/python"), pythonArgs...)
			python.Stdin = strings.NewReader(test.input)
			want := captureSkillProcess(t, python)
			if want.exit != test.wantExit {
				t.Fatalf("Python oracle failed: %+v", want)
			}
			code, stdout, stderr := call(append([]string{"start-policy"}, test.args...), test.input)
			if got := (skillProcessResult{code, stdout, stderr}); got != want {
				t.Fatalf("policy mismatch\nPython: %+v\nGo: %+v", want, got)
			}
		})
	}
}
