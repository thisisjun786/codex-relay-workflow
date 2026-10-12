package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plan C of CRW-1283 (Sion 10-13, decided answers 2 and 4 rewritten): the reader no longer follows the arguments a script forwards
// with "$@", and, since split (1), every PATH assignment is refused as at c007bd50 (the literal PATH reading is CRW-1292). The forms below are the findings of the final Opus verification of 93890d32 (opusverify-final-93890d32-f3):
// each was allowed at 93890d32 and was refused before the relaxations (c007bd50 refused every "$@" and every PATH assignment). No test
// runs the command.

func planCScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	return crw1283Script(t, dir, name, body)
}

// TestPlanC1283PrintOptionArgumentForwarder: zsh's print -f, -C, -x and -X take the next word as their argument, so -v after them still
// assigns a positional parameter, and zsh's read name?prompt assigns $1; the forwarding script that uses them is refused by every
// guard whose target the forwarded arguments may carry, run by a shell or by its path.
func TestPlanC1283PrintOptionArgumentForwarder(t *testing.T) {
	cases := []struct {
		name, body, args string // %s is the protected command
	}{
		{"print_f_v_1", "print -f {F} -v 1 env\n\"$@\"\n", "true %s"},
		{"print_f_v_argv", "print -f {F} -v argv %s\n\"$@\"\n", "true"},
		{"print_C_v_1", "print -C 1 -v 1 env\n\"$@\"\n", "true %s"},
		{"print_x_v_1", "print -x 4 -v 1 env\n\"$@\"\n", "true %s"},
		{"print_X_v_argv", "print -X 4 -v argv %s\n\"$@\"\n", "true"},
		{"loop_print_f_v_1", "while :; do nice \"$@\"; print -f {F} -v 1 env; done\n", "true %s"},
		{"pipeline_last_print_C_v_1", "true | print -C 1 -v 1 env\n\"$@\"\n", "true %s"},
		{"loop_read_prompt", "while :; do nice \"$@\"; read 1\\?p <<<env; done\n", "true %s"},
		{"pipeline_last_read_prompt", "echo env | read 1\\?p\n\"$@\"\n", "true %s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, target := range crw1283Targets(t) {
				body := strings.ReplaceAll(strings.ReplaceAll(c.body, "%s", target.command), "{F}", "%s")
				args := strings.ReplaceAll(c.args, "%s", target.command)
				script := crw1283Script(t, target.dir, "printopt.sh", "#!/bin/bash\n"+body)
				for _, command := range []string{"bash " + script + " " + args, "zsh " + script + " " + args, script + " " + args} {
					if !target.refused(command) {
						t.Errorf("%s guard ALLOWED %q (body %q)", target.name, command, body)
					}
				}
			}
		})
	}
}

// TestPlanC1283PathAssignmentOutOfOrder: a literal PATH assignment that the walk does not follow in order (after the program it
// affects in a loop, the last element of a pipeline that zsh runs in the current shell, a DEBUG trap, a function zsh or a trap runs,
// a sourced file) changes which file a later program name runs; the evil fixture's go runs the guard's target. Each is refused, and
// the in-order assignment that selects the evil go is refused because the file it selects is read.
func TestPlanC1283PathAssignmentOutOfOrder(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, target := range crw1283Targets(t) {
		evil := filepath.Join(target.dir, "evil")
		planCScript(t, target.dir, "evil/go", "#!/bin/sh\n"+target.command+"\n")
		path := evil + ":/usr/bin:/bin"
		crw1283Script(t, target.dir, "p.sh", "PATH="+path+"\n")
		crw1283Script(t, target.dir, "q.sh", "export PATH="+path+"\n")
		for _, command := range []string{
			"while :; do go version; export PATH=" + path + "; done",
			"until false; do go version; PATH=" + path + "; done",
			"for i in 1 2; do go version; PATH=" + path + "; done",
			"while go version; do PATH=" + path + "; done",
			"true | export PATH=" + path + "; go version",
			"true | PATH=" + path + "; go version",
			"trap 'PATH=" + path + "' DEBUG; go version",
			"f() { export PATH=" + path + "; }; trap f DEBUG; go version",
			"TRAPDEBUG() { PATH=" + path + "; }; go version",
			"chpwd() { PATH=" + path + "; }; cd /tmp; go version",
			". " + target.dir + "/p.sh; go version",
			"source " + target.dir + "/q.sh; go version",
			"eval 'export PATH=" + path + "'; go version",
			"PATH=" + path + " go version",
			"export PATH=" + path + "; go version",
		} {
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED %q", target.name, command)
			}
		}
	}
}

// TestPlanC1283PathAssignmentNeedsAKnownStartingPath: without a PATH in the hook's environment the reader cannot tell what an
// assigned PATH replaces, so the assignment is refused as before the relaxations, even with a literal value and a harmless program.
// Split (1) (Sion 10-13): with a known starting PATH it is refused too, as at c007bd50, until CRW-1292 reads a literal PATH.
func TestPlanC1283PathAssignmentNeedsAKnownStartingPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, target := range crw1283Targets(t) {
		planCScript(t, target.dir, "ben/check", "#!/bin/sh\n:\n")
		command := "PATH=" + filepath.Join(target.dir, "ben") + " check"
		if !target.refused(command) {
			t.Errorf("%s guard ALLOWED a literal PATH assignment with a known starting PATH (refused as at c007bd50 until CRW-1292): %s", target.name, command)
		}
		os.Unsetenv("PATH")
		if !target.refused(command) {
			t.Errorf("%s guard ALLOWED a PATH assignment while the starting PATH is not known: %s", target.name, command)
		}
		os.Setenv("PATH", "/usr/bin:/bin")
	}
}

// TestPlanC1283PathValueMustBeLiteral: a PATH value built from an expansion ($PATH included) is refused as before the relaxations.
func TestPlanC1283PathValueMustBeLiteral(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, target := range crw1283Targets(t) {
		ben := filepath.Join(target.dir, "ben")
		planCScript(t, target.dir, "ben/check", "#!/bin/sh\n:\n")
		for _, command := range []string{"D=" + ben + "; PATH=$D check", "PATH=" + ben + ":$PATH check", "export PATH=\"" + ben + ":$PATH\"; check",
			"env PATH=" + ben + ":$PATH check", "X=1 export PATH=" + ben + ":$PATH; check", "PATH=$UNKNOWN /usr/bin/true", "PATH+=:" + ben + " check"} {
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a PATH value that is not literal: %s", target.name, command)
			}
		}
	}
}
