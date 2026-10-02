package skill

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSkillInProcess runs `crw skill <args>` in process.
func runSkillInProcess(args ...string) skillProcessResult {
	var stdout, stderr bytes.Buffer
	exit := Run(args, strings.NewReader(""), &stdout, &stderr)
	return skillProcessResult{exit, stdout.String(), stderr.String()}
}

// The command line: a family's usage lists its commands, a command's usage lists its flags, -h
// exits 0 on stdout, and every usage error exits 2 on stderr.
func TestSkillCommandLine(t *testing.T) {
	expect := func(t *testing.T, got skillProcessResult, exit int, stdout, stderr []string) {
		t.Helper()
		if got.exit != exit {
			t.Errorf("exit %d, want %d: %+v", got.exit, exit, got)
		}
		for _, want := range stdout {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("stdout lacks %q: %+v", want, got)
			}
		}
		for _, want := range stderr {
			if !strings.Contains(got.stderr, want) {
				t.Errorf("stderr lacks %q: %+v", want, got)
			}
		}
	}
	expect(t, runSkillInProcess(), 2, nil, []string{"crw skill: error: a command is required"})
	expect(t, runSkillInProcess("--help"), 0, []string{"usage: crw skill {hook-probe,issue-size,parent-title,start-policy,base-refresh}"}, nil)
	expect(t, runSkillInProcess("nope"), 2, nil, []string{`invalid command "nope"`})
	for _, f := range []family{hookProbe, issueSize, parentTitle, startPolicy, baseRefresh} {
		t.Run(f.name, func(t *testing.T) {
			var names []string
			for _, c := range f.commands {
				names = append(names, "  "+c[0])
			}
			expect(t, runSkillInProcess(f.name), 2, nil, append([]string{"crw skill " + f.name + ": error: a command is required"}, names...))
			expect(t, runSkillInProcess(f.name, "-h"), 0, append([]string{"usage: crw skill " + f.name + " <command> [flags]"}, names...), nil)
			expect(t, runSkillInProcess(f.name, "--help"), 0, names, nil)
			expect(t, runSkillInProcess(f.name, "bogus"), 2, nil, []string{`crw skill ` + f.name + `: error: invalid command "bogus"`})
			for _, c := range f.commands {
				expect(t, runSkillInProcess(f.name, c[0], "-h"), 0, []string{"usage: crw skill " + f.name + " " + c[0] + " [flags]"}, nil)
				expect(t, runSkillInProcess(f.name, c[0], "--unknown"), 2, nil, []string{"crw skill " + f.name + " " + c[0] + ": error: flag provided but not defined: -unknown"})
			}
		})
	}
	for _, c := range []struct {
		args  []string
		flags []string
	}{
		{[]string{"hook-probe", "observe", "--help"}, []string{"-binary", "-codex-home", "-sanitize"}},
		{[]string{"hook-probe", "replay", "--help"}, []string{"-fixtures", "-contract", "-host-fixtures", "-allow-unreached"}},
		{[]string{"hook-probe", "decide", "--help"}, []string{"usage: crw skill hook-probe decide [flags] observation"}},
		{[]string{"parent-title", "replay", "--help"}, []string{"-fixtures", "-allow-unreached"}},
		{[]string{"start-policy", "check", "--help"}, []string{"usage: crw skill start-policy check [flags] [record]"}},
		{[]string{"issue-size", "check", "--help"}, []string{"usage: crw skill issue-size check [flags] [file]"}},
	} {
		expect(t, runSkillInProcess(c.args...), 0, c.flags, nil)
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"hook-probe", "decide"}, "the argument observation is required"},
		{[]string{"hook-probe", "decide", "a", "b"}, "unexpected arguments: b"},
		{[]string{"hook-probe", "observe", "extra"}, "unexpected arguments: extra"},
		{[]string{"hook-probe", "observe", "--binary"}, "flag needs an argument: -binary"},
		{[]string{"hook-probe", "replay", "--fixtures"}, "flag needs an argument: -fixtures"},
		{[]string{"hook-probe", "replay", "--allow-unreached=maybe"}, "invalid boolean value"},
		{[]string{"hook-probe", "replay", "--", "extra"}, "unexpected arguments: extra"},
		{[]string{"parent-title", "decide", "extra"}, "unexpected arguments: extra"},
		{[]string{"parent-title", "readback", "--", "extra"}, "unexpected arguments: extra"},
		{[]string{"start-policy", "check", "a", "b"}, "unexpected arguments: b"},
		{[]string{"start-policy", "selftest", "extra"}, "unexpected arguments: extra"},
	} {
		expect(t, runSkillInProcess(c.args...), 2, nil, []string{c.says})
	}
}

// After --, an argument that looks like a flag is a positional argument the command reads.
func TestSkillDoubleDashPassesOptionLikePositionals(t *testing.T) {
	goldenRoot(t)
	binary := recordedCRW(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "-h"), []byte("run_mode: loop\nobservation_path: blocked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "--help"), []byte(`{"observation":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		family string
		args   []string
	}{
		{"hook-probe", []string{"decide", "--", "--help"}},
		{"start-policy", []string{"check", "--", "-h"}},
	} {
		t.Run(test.family, func(t *testing.T) {
			args := append([]string{"skill", test.family}, test.args...)
			cmd := exec.Command(binary, args...)
			cmd.Dir = dir
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			exit := 0
			if err := cmd.Run(); err != nil {
				var failed *exec.ExitError
				if !errors.As(err, &failed) {
					t.Fatal(err)
				}
				exit = failed.ExitCode()
			}
			got := skillProcessResult{exit, stdout.String(), stderr.String()}
			if got.exit != 0 {
				t.Errorf("exit %d: %+v", got.exit, got)
			}
			checkSkillAnswer(t, "", dir, args, got)
		})
	}
}
