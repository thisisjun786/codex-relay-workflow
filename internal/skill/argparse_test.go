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

type argparseCase struct {
	name string
	args []string
}

func TestSkillArgparseMatchesLivePython(t *testing.T) {
	goldenRoot(t)
	// Given the built Go CLI, the three command families the Python scripts defined, and every
	// parser level.
	binary := recordedCRW(t)
	tests := map[string][]argparseCase{
		"hook-probe": {
			{"missing command", nil}, {"short root help", []string{"-h"}}, {"long root help", []string{"--help"}},
			{"invalid command", []string{"bogus"}}, {"unknown root option", []string{"--unknown"}},
			{"observe short help", []string{"observe", "-h"}}, {"observe long help", []string{"observe", "--help"}},
			{"observe missing binary", []string{"observe", "--binary"}}, {"observe missing home", []string{"observe", "--codex-home"}},
			{"observe help after option", []string{"observe", "--binary", "x", "--help"}}, {"observe help before option", []string{"observe", "--help", "--binary", "x"}},
			{"observe inline option before help", []string{"observe", "--binary=-x", "--help"}},
			{"observe unknown option", []string{"observe", "--unknown"}}, {"observe extra", []string{"observe", "extra"}}, {"observe double dash extra", []string{"observe", "--", "extra"}},
			{"decide short help", []string{"decide", "-h"}}, {"decide long help", []string{"decide", "--help"}}, {"decide missing observation", []string{"decide"}},
			{"decide extra", []string{"decide", "a", "b"}}, {"decide unknown option", []string{"decide", "--unknown"}},
			{"replay short help", []string{"replay", "-h"}}, {"replay long help", []string{"replay", "--help"}},
			{"replay missing fixtures", []string{"replay", "--fixtures"}}, {"replay missing contract", []string{"replay", "--contract"}}, {"replay missing host fixtures", []string{"replay", "--host-fixtures"}},
			{"replay help after option", []string{"replay", "--fixtures", "x", "--help"}}, {"replay help before option", []string{"replay", "--help", "--fixtures", "x"}},
			{"replay abbreviated option before help", []string{"replay", "--fi", "x", "--help"}},
			{"replay unknown option", []string{"replay", "--unknown"}}, {"replay extra", []string{"replay", "extra"}}, {"replay double dash extra", []string{"replay", "--", "extra"}},
		},
		"parent-title": {
			{"missing command", nil}, {"short root help", []string{"-h"}}, {"long root help", []string{"--help"}},
			{"invalid command", []string{"bogus"}}, {"unknown root option", []string{"--unknown"}},
			{"decide short help", []string{"decide", "-h"}}, {"decide long help", []string{"decide", "--help"}},
			{"decide extra", []string{"decide", "extra"}}, {"decide unknown option", []string{"decide", "--unknown"}}, {"decide double dash extra", []string{"decide", "--", "extra"}},
			{"readback short help", []string{"readback", "-h"}}, {"readback long help", []string{"readback", "--help"}},
			{"readback extra", []string{"readback", "extra"}}, {"readback unknown option", []string{"readback", "--unknown"}},
			{"replay short help", []string{"replay", "-h"}}, {"replay long help", []string{"replay", "--help"}}, {"replay missing fixtures", []string{"replay", "--fixtures"}},
			{"replay help after option", []string{"replay", "--fixtures", "x", "--help"}}, {"replay help before option", []string{"replay", "--help", "--fixtures", "x"}},
			{"replay inline abbreviated option before help", []string{"replay", "--fi=x", "--help"}},
			{"replay unknown option", []string{"replay", "--unknown"}}, {"replay extra", []string{"replay", "extra"}}, {"replay double dash extra", []string{"replay", "--", "extra"}},
		},
		"start-policy": {
			{"missing mode", nil}, {"short root help", []string{"-h"}}, {"long root help", []string{"--help"}},
			{"invalid mode", []string{"bogus"}}, {"unknown root option", []string{"--unknown"}},
			{"vocabulary short help", []string{"vocabulary", "-h"}}, {"vocabulary long help", []string{"vocabulary", "--help"}},
			{"vocabulary extra", []string{"vocabulary", "extra"}}, {"vocabulary unknown option", []string{"vocabulary", "--unknown"}}, {"vocabulary double dash extra", []string{"vocabulary", "--", "extra"}},
			{"check short help", []string{"check", "-h"}}, {"check long help", []string{"check", "--help"}},
			{"check help after record", []string{"check", "file", "--help"}}, {"check help before record", []string{"check", "--help", "file"}},
			{"check extra", []string{"check", "a", "b"}}, {"check unknown option", []string{"check", "--unknown"}}, {"check double dash extras", []string{"check", "--", "a", "b"}},
			{"selftest short help", []string{"selftest", "-h"}}, {"selftest long help", []string{"selftest", "--help"}},
			{"selftest extra", []string{"selftest", "extra"}}, {"selftest unknown option", []string{"selftest", "--unknown"}}, {"selftest double dash extra", []string{"selftest", "--", "extra"}},
		},
	}

	// A bare trailing -- after a command that takes no positional: argparse
	// reports it as an unrecognized argument (exit 2), and so must Go.
	for family, commands := range map[string][][]string{
		"hook-probe":   {{"observe"}, {"replay"}, {"replay", "--allow-unreached"}},
		"parent-title": {{"decide"}, {"readback"}, {"replay"}, {"replay", "--allow-unreached"}},
		"start-policy": {{"vocabulary"}, {"selftest"}},
	} {
		for _, command := range commands {
			tests[family] = append(tests[family], argparseCase{strings.Join(command, " ") + " trailing double dash", append(command, "--")})
		}
	}
	// argparse names a refused value with repr(): a quote inside picks the other
	// quote, and control and non-ASCII characters print as Python escapes them.
	// sys.argv holds each byte outside well-formed UTF-8 as its own lone
	// surrogate U+DC00+byte (os.fsdecode), the three bytes of an encoded
	// surrogate included, and repr() escapes each one.
	for family := range pythonArgparseFamilies {
		for _, value := range []string{"it's", `say "hi"`, `it's "both"`, "tab\there", "caf\u00e9\x7f", "back\\slash", "\xff", "\xed\xa0\x80", "\xed\xb2\x80x", "a\xe2\x82", "\xf0\x9f\x98\x80\xed\xbf\xbf"} {
			tests[family] = append(tests[family], argparseCase{"repr of invalid choice", []string{value}}, argparseCase{"repr of help argument", []string{"--help=" + value}})
		}
	}
	// argparse prints an unrecognized argument as sys.argv holds it, and
	// sys.stderr writes each lone surrogate as its \udcXX escape
	// (errors="backslashreplace").
	for family, spec := range pythonArgparseFamilies {
		for _, value := range []string{"\xff", "\xed\xa0\x80", "caf\u00e9\xed\xb2\x80x"} {
			tests[family] = append(tests[family], argparseCase{"unrecognized root option bytes", []string{"--" + value, spec.order[0]}})
			for _, name := range spec.order {
				tests[family] = append(tests[family], argparseCase{name + " unrecognized option bytes", []string{name, "--" + value}})
				if spec.commands[name].maxArgs == 0 {
					tests[family] = append(tests[family], argparseCase{name + " unrecognized argument bytes", []string{name, value}})
				}
				for _, option := range spec.commands[name].options {
					if option.valueName == "" {
						tests[family] = append(tests[family], argparseCase{name + " repr of boolean value bytes", []string{name, option.name + "=" + value}})
					}
				}
			}
		}
	}
	for family, spec := range pythonArgparseFamilies {
		for _, args := range [][]string{{"--unknown", "--help"}, {"--he"}, {"-hh"}, {"--help=yes"}, {"--", "--help"}} {
			tests[family] = append(tests[family], argparseCase{"root edge", args})
		}
		for _, name := range spec.order {
			for _, edge := range [][]string{{"--unknown", "--help"}, {"--he"}, {"-hh"}, {"--help=yes"}, {"--", "--help", "extra"}, {"-1", "--help"}} {
				tests[family] = append(tests[family], argparseCase{name + " edge", append([]string{name}, edge...)})
			}
			for _, option := range spec.commands[name].options {
				if option.valueName == "" {
					tests[family] = append(tests[family], argparseCase{name + " boolean value", []string{name, option.name + "=yes"}})
				} else {
					for _, tail := range [][]string{{"--help"}, {"--unknown", "--help"}, {"-1", "--help"}} {
						tests[family] = append(tests[family], argparseCase{name + " option edge", append([]string{name, option.name}, tail...)})
					}
				}
			}
		}
	}
	// An argument argparse echoes back is repr() of the str Python holds for it: the quote repr
	// picks, a character str.isprintable() refuses escaped, and an argv byte that is not UTF-8 as
	// the surrogate Python decodes it to.
	for family := range tests {
		for _, text := range []string{"it's", "x\U000000a0y", "x\U00002028y", "x\xffy"} {
			tests[family] = append(tests[family], argparseCase{"invalid choice repr", []string{text}}, argparseCase{"ignored explicit argument repr", []string{"--help=" + text}})
		}
	}
	for family, cases := range tests {
		for _, test := range cases {
			t.Run(family+"/"+test.name, func(t *testing.T) {
				// When the arguments run through the built Go binary.
				goResult := runArgparseCommand(t, append([]string{binary, "skill", family}, test.args...))

				// Then exit status, stdout, and stderr are the golden's byte for byte (first taken
				// as the Python script's answer).
				checkSkillAnswer(t, "", repositoryRoot(), append([]string{"skill", family}, test.args...), skillProcessResult(goResult))
			})
		}
	}
}

func TestSkillArgparseDoubleDashPassesOptionLikePositionals(t *testing.T) {
	goldenRoot(t)
	// Given option-looking filenames containing valid inputs in an isolated working directory.
	binary := recordedCRW(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "-h"), []byte("run_mode: loop\nobservation_path: blocked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "--help"), []byte(`{"observation":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		family string
		args   []string
	}{
		{"hook-probe", []string{"decide", "--", "--help"}},
		{"start-policy", []string{"check", "--", "-h"}},
	}
	for _, test := range tests {
		t.Run(test.family, func(t *testing.T) {
			// When -- terminates option parsing.
			goResult := runArgparseCommandIn(t, dir, append([]string{binary, "skill", test.family}, test.args...))

			// Then the option-looking positional reaches normal execution, as the golden holds.
			checkSkillAnswer(t, "", dir, append([]string{"skill", test.family}, test.args...), skillProcessResult(goResult))
		})
	}
}

func runArgparseCommand(t *testing.T, command []string) hookProbeResult {
	t.Helper()
	return runArgparseCommandIn(t, repositoryRoot(), command)
}

func runArgparseCommandIn(t *testing.T, dir string, command []string) hookProbeResult {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = oracleEnv("TMPDIR=/var/tmp")
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
	return hookProbeResult{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}
