package hook

import (
	"strings"
	"testing"
)

// The finding of the final Opus verification of f2ef7770 (opusverify-final-f2ef7770-f2): zsh builtins that assign a parameter by
// name also assign a positional parameter when the name is 1, 2, ... or argv (print -v 1 X, print -v argv X, zformat, zparseopts -D,
// getln 1). A later "$@" is then not the arguments the script was given, in straight-line text and in a loop. Each form was refused
// before the relaxations (c007bd50 refused every "$@"). No test runs the command.

// TestReview1283ZshParameterBuiltins: a zsh builtin that assigns or removes positional parameters ends the arguments the reader
// follows, in every guard.
func TestReview1283ZshParameterBuiltins(t *testing.T) {
	cases := []struct {
		name, body, args string // %s is the protected command
	}{
		{"print_v_1", "print -v 1 env\n\"$@\"\n", "true %s"},
		{"print_rv_1", "print -rv 1 env\n\"$@\"\n", "true %s"},
		{"print_r_v_1", "print -r -v 1 env\n\"$@\"\n", "true %s"},
		{"print_v_2", "print -v 2 env\n\"$@\"\n", "true true %s"},
		{"print_v_argv", "print -v argv %s\n\"$@\"\n", "true"},
		{"print_v_unknown_name", "print -v \"$N\" env\n\"$@\"\n", "true %s"},
		{"print_unknown_option", "print $O env\n\"$@\"\n", "true %s"},
		{"command_print", "command print -v 1 env\n\"$@\"\n", "true %s"},
		{"builtin_print", "builtin print -v 1 env\n\"$@\"\n", "true %s"},
		{"noglob_print", "noglob print -v 1 env\n\"$@\"\n", "true %s"},
		{"noglob_shift", "noglob shift\n\"$@\"\n", "true %s"},
		{"noglob_set", "noglob set -- %s\n\"$@\"\n", "true"},
		{"nocorrect_shift", "nocorrect shift\n\"$@\"\n", "true %s"},
		{"precommand_dash_shift", "- shift\n\"$@\"\n", "true %s"},
		{"loop_noglob_shift", "while :; do nice \"$@\"; noglob shift; done\n", "true %s"},
		{"pipeline_last_print", "echo x | print -v 1 env\n\"$@\"\n", "true %s"},
		{"pipeline_last_read", "echo env | read 1\n\"$@\"\n", "true %s"},
		{"pipeline_last_zformat", "true | zformat -f 1 '%a' a:env\n\"$@\"\n", "true %s"},
		{"pipeline_last_shift", "true | shift\n\"$@\"\n", "true %s"},
		{"pipeline_last_set", "true | set -- %s\n\"$@\"\n", "true"},
		{"loop_pipeline_last_read", "while :; do nice \"$@\"; echo env | read 1; done\n", "true %s"},
		{"loop_pipeline_last_shift", "while :; do nice \"$@\"; true | shift; done\n", "true %s"},
		{"time_print", "time print -v 1 env\n\"$@\"\n", "true %s"},
		{"negated_print", "! print -v 1 env\n\"$@\"\n", "true %s"},
		{"and_list_print", "true && print -v 1 env\n\"$@\"\n", "true %s"},
		{"group_print", "{ print -v 1 env; }\n\"$@\"\n", "true %s"},
		{"case_print", "case x in x) print -v 1 env ;; esac\n\"$@\"\n", "true %s"},
		{"functions_print", "print -v 1 env\nexec \"$@\"\n", "true %s"},
		{"function_print", "f() { print -v 1 env; }\nf\n\"$@\"\n", "true %s"},
		{"print_in_if", "if :; then print -v 1 env; fi\n\"$@\"\n", "true %s"},
		{"zformat_f", "zformat -f 1 '%a' a:env\n\"$@\"\n", "true %s"},
		{"zformat_a_argv", "zformat -a argv : env:x\n\"$@\"\n", "true"},
		{"zparseopts_D", "zparseopts -D n:=o\n\"$@\"\n", "-n 5 %s"},
		{"zparseopts_a_argv", "zparseopts -a argv n:=o\n\"$@\"\n", "-n 5 %s"},
		{"getln", "pushln env\ngetln 1\n\"$@\"\n", "true %s"},
		{"vared", "vared 1 <<<env\n\"$@\"\n", "true %s"},
		{"zregexparse", "zregexparse x y z\n\"$@\"\n", "true %s"},
		{"compadd", "compadd -O argv env\n\"$@\"\n", "true %s"},
		{"loop_print_v", "while :; do nice \"$@\"; print -v 1 env; done\n", "true %s"},
		{"loop_print_rv_argv", "while :; do nice \"$@\"; print -rv argv env; done\n", "true"},
		{"loop_print_before_forward", "while :; do print -v 1 env; nice \"$@\"; done\n", "true %s"},
		{"loop_print_after_variable", "while :; do x=1; nice \"$@\"; print -v 1 env; done\n", "true %s"},
		{"loop_print_in_if", "while :; do nice \"$@\"; if :; then print -v 1 env; fi; done\n", "true %s"},
		{"loop_print_function", "f() { print -v 1 env; }\nwhile :; do nice \"$@\"; f; done\n", "true %s"},
		{"loop_command_print", "until false; do ionice -c3 \"$@\"; command print -v 1 env; done\n", "true %s"},
		{"loop_print_condition", "while nice \"$@\" && print -v 1 env; do :; done\n", "true %s"},
		{"loop_zformat", "while :; do timeout 5 \"$@\"; zformat -f 1 '%a' a:env; done\n", "true %s"},
		{"loop_zparseopts", "while :; do env \"$@\"; zparseopts -D n:=o; done\n", "true %s"},
		{"loop_getln", "for i in 1 2; do nice \"$@\"; getln 1; done\n", "true %s"},
		{"loop_vared", "while :; do nice \"$@\"; vared 1; done\n", "true %s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, target := range crw1283Targets(t) {
				body := strings.ReplaceAll(c.body, "%s", target.command)
				args := strings.ReplaceAll(c.args, "%s", target.command)
				script := crw1283Script(t, target.dir, "zshparam.sh", "#!/bin/bash\n"+body)
				commands := []string{"bash " + script + " " + args, "zsh " + script + " " + args}
				if target.name == "github" {
					// the memory and worktree guards do not read a script run by its path unless it forwards "$@" (the open item
					// CRW-1287, pinned by the differential fuzz)
					commands = append(commands, script+" "+args)
				}
				for _, command := range commands {
					if !target.refused(command) {
						t.Errorf("%s guard ALLOWED %q after a zsh builtin that assigns a positional parameter (body %q)", target.name, command, body)
					}
				}
			}
		})
	}
}

// TestReview1283ZshParameterBuiltinControls: print without the option that assigns a variable, and the other builtins that name no
// parameter, leave the arguments as they are: a wrapper that uses them still forwards what it was given, and the protected command it
// forwards stays refused.
func TestReview1283ZshParameterBuiltinControls(t *testing.T) {
	bodies := []string{
		"print hello\nnice \"$@\"\n",
		"print -r -- 'a b'\nnice \"$@\"\n",
		"print -n done\nnice \"$@\"\n",
		"echo hello\nnice \"$@\"\n",
		"while :; do nice \"$@\"; print hello; break; done\n",
		"while :; do print -r -- note; nice \"$@\"; break; done\n",
		"whence -v ls >/dev/null\nnice \"$@\"\n",
	}
	for _, body := range bodies {
		t.Run(strings.Fields(body)[0], func(t *testing.T) {
			for _, target := range crw1283Targets(t) {
				if target.name == "github" {
					continue
				}
				script := crw1283Script(t, target.dir, "zshctl.sh", "#!/bin/bash\n"+body)
				for _, command := range []string{"bash " + script + " go version", script + " go version"} {
					if target.refused(command) {
						t.Errorf("%s guard refused a wrapper that prints and forwards go version: %s (body %q)", target.name, command, body)
					}
				}
				for _, command := range []string{"bash " + script + " " + target.command, script + " " + target.command} {
					if !target.refused(command) {
						t.Errorf("%s guard ALLOWED the protected command forwarded by a printing wrapper: %s (body %q)", target.name, command, body)
					}
				}
			}
			gh, _, _ := gateScene(t)
			script := crw1283Script(t, gh, "zshctl.sh", "#!/bin/bash\n"+body)
			if site, denied := githubPostJudgeText("bash "+script+" go version", gh); denied {
				t.Errorf("github guard refused a printing wrapper that forwards go version: %+v (body %q)", site, body)
			}
			if _, denied := githubPostJudgeText("bash "+script+" gh pr comment 1 --body x", gh); !denied {
				t.Errorf("github guard ALLOWED a post forwarded by a printing wrapper (body %q)", body)
			}
		})
	}
}
