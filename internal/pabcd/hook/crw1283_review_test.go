package hook

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The forms below were found by the security review of the CRW-1283 relaxations (opusverify-e4c54cc3-r1): each was refused before
// the relaxation and must stay refused by the guard whose target it reaches (Sion decision 5). No test runs the command.

// crw1283Targets is the protected command of each guard: a memory write, a GitHub post and the removal of the managed checkout.
type crw1283Target struct {
	name, command string
	refused       func(command string) bool
	dir           string
}

func crw1283Targets(t *testing.T) []crw1283Target {
	t.Helper()
	rig := newDelRig(t)
	cwd, root, env := gateScene(t)
	return []crw1283Target{
		{"memory", "tee " + root + "/note.md", func(command string) bool {
			return HandleMemoryWriteGate(gateBash(t, cwd, command), env) != ""
		}, cwd},
		{"github", "gh pr comment 1 --body x", func(command string) bool {
			_, denied := githubPostJudgeText(command, cwd)
			return denied
		}, cwd},
		{"worktree", "rm -rf " + rig.checkout, func(command string) bool { return rig.verdict(command).Deny }, rig.checkout},
	}
}

func crw1283Copy(t *testing.T, source, destination string) {
	t.Helper()
	in, err := os.Open(source)
	if err != nil {
		t.Skipf("no %s on this host: %v", source, err)
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReview1283PositionalMutation: a script that changes its positional parameters before it runs "$@" does not run the
// arguments it was given, so the reader must not read "$@" as those arguments.
func TestReview1283PositionalMutation(t *testing.T) {
	cases := []struct {
		name, body, args string // %s is the protected command
	}{
		{"set_--", "set -- %s\n\"$@\"\n", "true"},
		{"set_operands", "set %s\n\"$@\"\n", "true"},
		{"set_-e_--", "set -e -- %s\n\"$@\"\n", "true"},
		{"set_-o_operands", "set -o errexit %s\n\"$@\"\n", "true"},
		{"shift", "shift\n\"$@\"\n", "true %s"},
		{"shift_2", "shift 2\n\"$@\"\n", "true true %s"},
		{"shift_exec", "shift\nexec \"$@\"\n", "true %s"},
		{"conditional_shift", "if test -n \"$1\"; then shift; fi\n\"$@\"\n", "true %s"},
		{"and_shift", "test -n \"$1\" && shift\n\"$@\"\n", "true %s"},
		{"loop_shift", "while test \"$1\" = true; do shift; done\n\"$@\"\n", "true %s"},
		{"case_shift", "case \"$1\" in true) shift ;; esac\n\"$@\"\n", "true %s"},
		{"eval_set", "eval 'set -- %s'\n\"$@\"\n", "true"},
		{"eval_shift", "eval shift\n\"$@\"\n", "true %s"},
		{"command_shift", "command shift\n\"$@\"\n", "true %s"},
		{"builtin_set", "builtin set -- %s\n\"$@\"\n", "true"},
		{"source_shift", ". ./shift.sh\n\"$@\"\n", "true %s"},
		{"trap_shift", "trap shift DEBUG\n\"$@\"\n", "true %s"},
		{"function_set", "f() { set -- %s; }\nf\n\"$@\"\n", "true"},
		{"for_shift", "for i in 1; do shift; done\n\"$@\"\n", "true %s"},
		{"until_shift", "until test \"$1\" != true; do shift; done\n\"$@\"\n", "true %s"},
		{"group_shift", "{ shift; }\n\"$@\"\n", "true %s"},
		{"or_shift", "shift || exit 1\n\"$@\"\n", "true %s"},
		{"set_dash", "set - %s\n\"$@\"\n", "true"},
		{"alias_set", "shopt -s expand_aliases\nalias s='set --'\ns %s\n\"$@\"\n", "true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, target := range crw1283Targets(t) {
				crw1283Script(t, target.dir, "shift.sh", "shift\n")
				body := strings.ReplaceAll(c.body, "%s", target.command)
				args := strings.ReplaceAll(c.args, "%s", target.command)
				script := crw1283Script(t, target.dir, "mutation.sh", "#!/bin/bash\n"+body)
				for _, command := range []string{"bash " + script + " " + args, script + " " + args} {
					if !target.refused(command) {
						t.Errorf("%s guard ALLOWED %q after positional mutation (body %q)", target.name, command, body)
					}
				}
			}
		})
	}
	// Plan C (Sion 10-13): the reader no longer follows a forwarded "$@", so a wrapper that sets options alone is refused too.
	for _, body := range []string{"set -euo pipefail\n\"$@\"\n", "set -u\n\"$@\"\n", "set +x\n\"$@\"\n"} {
		for _, target := range crw1283Targets(t) {
			script := crw1283Script(t, target.dir, "options.sh", "#!/bin/bash\n"+body)
			if !target.refused("bash " + script + " go version") {
				t.Errorf("%s guard allowed a \"$@\" wrapper after an option-only set (body %q)", target.name, body)
			}
			if !target.refused("bash " + script + " " + target.command) {
				t.Errorf("%s guard ALLOWED the forwarded protected command after an option-only set (body %q)", target.name, body)
			}
		}
	}
}

// TestReview1283ExternalBuiltinName: env, exec and the other external launchers run the program the PATH finds, never the shell's
// builtin of that name, so a builtin name behind them is resolved through the assigned PATH and the resolved file is read.
func TestReview1283ExternalBuiltinName(t *testing.T) {
	launchers := []string{
		"env PATH={DIR}:/usr/bin:/bin printf",
		"PATH={DIR}:/usr/bin:/bin exec printf",
		"PATH={DIR}:/usr/bin:/bin env printf",
		"PATH={DIR}:/usr/bin:/bin nice printf",
		"PATH={DIR}:/usr/bin:/bin timeout 5 printf",
		"PATH={DIR}:/usr/bin:/bin nohup printf",
		"PATH={DIR}:/usr/bin:/bin xargs printf </dev/null",
		"PATH={DIR}:/usr/bin:/bin find /dev/null -exec printf {} +",
		"env PATH={DIR}:/usr/bin:/bin true",
		"env PATH={DIR}:/usr/bin:/bin test -n x",
		"export PATH={DIR}:/usr/bin:/bin; env echo x",
		// A builtin the text disables is searched on the PATH as an external program is.
		"enable -n printf; PATH={DIR}:/usr/bin:/bin printf",
		"PATH={DIR}:/usr/bin:/bin; enable -n printf; printf",
		"disable printf; PATH={DIR}:/usr/bin:/bin printf",
		". {DIR}/defs.sh; PATH={DIR}:/usr/bin:/bin printf",
		"eval 'enable -n printf'; PATH={DIR}:/usr/bin:/bin printf",
	}
	for _, target := range crw1283Targets(t) {
		for _, name := range []string{"printf", "true", "test", "echo"} {
			crw1283Script(t, target.dir, name, "#!/bin/sh\n"+target.command+"\n")
		}
		crw1283Script(t, target.dir, "defs.sh", "enable -n printf\n")
		for _, launcher := range launchers {
			command := strings.ReplaceAll(launcher, "{DIR}", target.dir)
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED external builtin-named program: %s", target.name, command)
			}
		}
		// printf is a builtin in bash, zsh and dash and a program the PATH finds in a strict POSIX shell or one without the builtin:
		// a file of that name on the assigned PATH is read (review b2ae50e0, Sion decision 5). A PATH whose printf is the system's
		// is refused too since split (1) (Sion 10-13), as at c007bd50, until CRW-1292.
		for _, shadowed := range []string{"PATH={DIR}:/usr/bin:/bin printf ok", "PATH={DIR}:/usr/bin:/bin command printf ok"} {
			command := strings.ReplaceAll(shadowed, "{DIR}", target.dir)
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a builtin name the assigned PATH shadows: %s", target.name, command)
			}
		}
		for _, control := range []string{"PATH=/usr/bin:/bin printf ok", "PATH=/usr/bin:/bin command printf ok", "env PATH=/usr/bin:/bin printf ok"} {
			if !target.refused(control) {
				t.Errorf("%s guard ALLOWED a PATH assignment (refused as at c007bd50 until CRW-1292): %s", target.name, control)
			}
		}
	}
}

// TestReview1283PathBinaryAlias: a binary that an assigned PATH finds outside the system directories cannot be identified by its
// name (a copy of a shell named check runs -c text), so the relaxation is withdrawn for it and the command is refused.
func TestReview1283PathBinaryAlias(t *testing.T) {
	for _, target := range crw1283Targets(t) {
		crw1283Copy(t, "/usr/bin/dash", filepath.Join(target.dir, "check"))
		_ = os.Remove(filepath.Join(target.dir, "linked"))
		if err := os.Symlink("/usr/bin/dash", filepath.Join(target.dir, "linked")); err != nil {
			t.Fatal(err)
		}
		crw1283Script(t, target.dir, "body.sh", target.command+"\n")
		for _, form := range []string{
			"PATH={DIR} check -c '{CMD}'",
			"PATH={DIR}:/usr/bin:/bin check -c '{CMD}'",
			"export PATH={DIR}:/usr/bin:/bin; check -c '{CMD}'",
			"env PATH={DIR}:/usr/bin:/bin check -c '{CMD}'",
			"PATH={DIR}:/usr/bin:/bin linked -c '{CMD}'",
			"PATH={DIR}:/usr/bin:/bin check {DIR}/body.sh",
			"PATH={DIR}:/usr/bin:/bin check <{DIR}/body.sh",
			"PATH={DIR}:/usr/bin:/bin nice check -c '{CMD}'",
		} {
			command := strings.NewReplacer("{DIR}", target.dir, "{CMD}", target.command).Replace(form)
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a PATH-selected renamed binary: %s", target.name, command)
			}
		}
	}
}

// TestReview1283PathGitAlias: git found through an assigned PATH is read as git is everywhere else, so an alias that runs a shell
// command is the command it names; a copy of git outside the system directories is not identified at all.
func TestReview1283PathGitAlias(t *testing.T) {
	for _, target := range crw1283Targets(t) {
		copyDir := filepath.Join(target.dir, "copy")
		scriptDir := filepath.Join(target.dir, "script")
		for _, d := range []string{copyDir, scriptDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		crw1283Copy(t, "/usr/bin/git", filepath.Join(copyDir, "git"))
		crw1283Script(t, scriptDir, "git", "#!/bin/sh\nexec /usr/bin/git \"$@\"\n")
		for _, dir := range []string{copyDir, scriptDir, "/usr/bin"} {
			for _, form := range []string{
				"PATH={DIR}:/usr/bin:/bin git -c 'alias.review=!{CMD}' review",
				"env PATH={DIR}:/usr/bin:/bin git -c 'alias.review=!{CMD}' review",
			} {
				command := strings.NewReplacer("{DIR}", dir, "{CMD}", target.command).Replace(form)
				if !target.refused(command) {
					t.Errorf("%s guard ALLOWED a git alias behind an assigned PATH: %s", target.name, command)
				}
			}
		}
	}
}

// TestReview1283PathPrefixKeepsShellState: a PATH assignment in front of a builtin lasts for that command only, but what the builtin
// does to the shell (its directory, its variables) stays; a builtin whose prefix assignments outlast it in some shells is refused.
func TestReview1283PathPrefixKeepsShellState(t *testing.T) {
	rig := newDelRig(t)
	for _, command := range []string{
		"PATH=/usr/bin:/bin cd " + filepath.Dir(rig.checkout) + "; rm -rf repo",
		"X=/tmp/x; PATH=/usr/bin:/bin printf -v X %s " + rig.checkout + "; rm -rf \"$X\"",
		"X=/tmp/x; PATH=/usr/bin:/bin read X <" + rig.checkout + "/.git; rm -rf \"$X\"",
	} {
		if !rig.verdict(command).Deny {
			t.Errorf("worktree guard ALLOWED a removal after a PATH-prefixed builtin: %s", command)
		}
	}
	for _, target := range crw1283Targets(t) {
		crw1283Script(t, target.dir, "printf", "#!/bin/sh\n"+target.command+"\n")
		for _, form := range []string{
			"PATH={DIR}:/usr/bin:/bin export X=1; env printf",
			"PATH={DIR}:/usr/bin:/bin :; env printf",
			"PATH={DIR}:/usr/bin:/bin eval true; env printf",
			"PATH={DIR}:/usr/bin:/bin exec 3>/dev/null; env printf",
			"f() { :; }; PATH={DIR}:/usr/bin:/bin f; env printf",
		} {
			command := strings.ReplaceAll(form, "{DIR}", target.dir)
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a PATH assignment that may outlast its builtin: %s", target.name, command)
			}
		}
	}
}

// TestReview1283PathBinaryOfTheStartingPath: a binary the assignment puts in front of the one the PATH the shell starts with runs is
// refused. The toolchain case (PATH=<go>/bin:... go version, the starting PATH runs the same file) is refused too since split (1)
// (Sion 10-13), as at c007bd50, until CRW-1292 reads a literal PATH.
func TestReview1283PathBinaryOfTheStartingPath(t *testing.T) {
	base := cleanTempDir(t)
	toolchain := filepath.Join(base, "toolchain")
	shadow := filepath.Join(base, "shadow")
	for _, d := range []string{toolchain, shadow} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	crw1283Copy(t, "/usr/bin/true", filepath.Join(toolchain, "tool"))
	crw1283Copy(t, "/usr/bin/dash", filepath.Join(shadow, "tool"))
	t.Setenv("PATH", toolchain+":/usr/bin:/bin")
	for _, target := range crw1283Targets(t) {
		for _, command := range []string{"PATH=" + toolchain + ":/usr/bin:/bin tool version", "export PATH=" + toolchain + ":/usr/bin:/bin; tool version"} {
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a PATH assignment (refused as at c007bd50 until CRW-1292): %s", target.name, command)
			}
		}
		for _, command := range []string{
			"PATH=" + shadow + ":/usr/bin:/bin tool -c '" + target.command + "'",
			"PATH=" + shadow + ":" + toolchain + ":/usr/bin:/bin tool -c '" + target.command + "'",
		} {
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a binary the assigned PATH puts in front of the starting one: %s", target.name, command)
			}
		}
	}
}
