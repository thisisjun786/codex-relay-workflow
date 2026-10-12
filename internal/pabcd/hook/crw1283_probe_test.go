package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The forms below were found by the second security review of the CRW-1283 relaxations (opusverify-b2ae50e0-r1, probes of
// /scratch/crw/verify-opus-1283/r1b): each was refused by every guard named in its row before the relaxations (base c007bd50) and
// was allowed on b2ae50e0. Each stays refused (Sion decision 5). No test runs the command.

// crw1283Probe is one form: files written into the target's directory, and the command. {CMD} is the protected command of the
// target, {PROG} its program word and {REST} its other words, {DIR} the target's directory. guards names the targets that refused
// the form before the relaxations; the others are named in a comment where they are not.
type crw1283Probe struct {
	name    string
	files   map[string]string
	command string
	guards  []string
}

var crw1283All = []string{"memory", "github", "worktree"}

// crw1283GitHub is the GitHub guard alone: the memory and worktree guards do not read the body of a script run by its path from
// the command itself unless the body forwards "$@" (shellIRDirectForwarder), before and after the relaxations alike; the "$@" of
// these forms is inside a quoted string, so those guards allowed them on c007bd50 as well (an open item of the handoff).
var crw1283GitHub = []string{"github"}

var crw1283Probes = []crw1283Probe{
	// "$@" read by a text that runs later (trap) or by a shell that starts with other positional parameters (-s, a descriptor
	// alias, a sourced file given operands, a zsh positional assignment).
	{"trap-shift", map[string]string{"t.sh": "trap '\"$@\"' EXIT\nshift\n"}, "bash {DIR}/t.sh true {CMD}", crw1283All},
	{"trap-set", map[string]string{"t.sh": "trap '\"$@\"' EXIT\nset -- {CMD}\n"}, "bash {DIR}/t.sh true", crw1283All},
	{"trap-shift-direct", map[string]string{"t.sh": "#!/bin/bash\ntrap '\"$@\"' EXIT\nshift\n"}, "{DIR}/t.sh true {CMD}", crw1283GitHub},
	{"trap-return-func", map[string]string{"t.sh": "trap '\"$@\"' EXIT\nset -- {CMD}\n"}, "{DIR}/t.sh true", crw1283GitHub},
	{"bash-c-args-trap", nil, "bash -c 'trap '\\''\"$@\"'\\'' EXIT; shift' z true {CMD}", crw1283All},
	{"stdin-s", map[string]string{"t.sh": "bash -s {CMD} <<'EOF'\n\"$@\"\nEOF\n"}, "bash {DIR}/t.sh true", crw1283All},
	{"stdin-s-direct", map[string]string{"t.sh": "#!/bin/bash\nbash -s {CMD} <<'EOF'\n\"$@\"\nEOF\n"}, "{DIR}/t.sh true", crw1283GitHub},
	{"dash-s-args", map[string]string{"t.sh": "sh -s {CMD} <<'EOF'\n\"$@\"\nEOF\n"}, "sh {DIR}/t.sh true", crw1283All},
	{"fd-alias", map[string]string{"t.sh": "bash /dev/fd/3 {CMD} 3<<'EOF'\n\"$@\"\nEOF\n"}, "bash {DIR}/t.sh true", crw1283All},
	{"dash-dot-args", map[string]string{"outer.sh": ". {DIR}/inner.sh true\n", "inner.sh": "\"$@\"\n"}, "sh {DIR}/outer.sh {CMD}", crw1283All},
	{"zsh-posassign", map[string]string{"t.sh": "1={PROG}\n\"$@\"\n"}, "zsh {DIR}/t.sh true {REST}", crw1283All},
	{"zsh-posassign-c", nil, "zsh -c '1={PROG}; \"$@\"' z true {REST}", crw1283All},
	// Found while fixing the forms above: operands xargs appends at run time, zsh's argv array and a positional loop variable, and a
	// shell given its program on standard input. The memory and worktree guards allowed xargs-feed-direct on c007bd50 too (a script
	// run by its path is not read there); a forwarding body is read now, and the operands xargs appends leave its "$@" unreadable.
	{"xargs-feed-script", map[string]string{"slot.sh": "#!/bin/bash\n\"$@\"\n"}, "echo {CMD} | xargs bash {DIR}/slot.sh", crw1283All},
	{"xargs-feed-direct", map[string]string{"slot.sh": "#!/bin/bash\n\"$@\"\n"}, "echo {CMD} | xargs {DIR}/slot.sh", crw1283All},
	{"zsh-argv-array", nil, "zsh -c 'argv=({CMD}); \"$@\"' z true", crw1283All},
	{"zsh-for-positional", nil, "zsh -c 'for 1 in {PROG}; do \"$@\"; done' z true {REST}", crw1283All},
	{"herestring-stdin", nil, "bash -s {CMD} <<<'\"$@\"'", crw1283All},
	// A name that is a builtin or keyword in one shell and a program the PATH finds in another.
	{"prefix-time", map[string]string{"time": "#!/bin/sh\n{CMD}\n"}, "PATH={DIR}:/usr/bin:/bin time true", crw1283All},
	{"export-time-keyword", map[string]string{"time": "#!/bin/sh\n{CMD}\n"}, "export PATH={DIR}:/usr/bin:/bin; time true", crw1283All},
	{"export-chdir", map[string]string{"chdir": "#!/bin/sh\n{CMD}\n"}, "export PATH={DIR}:/usr/bin:/bin; chdir /tmp", crw1283All},
	{"export-printf", map[string]string{"printf": "#!/bin/sh\n{CMD}\n"}, "export PATH={DIR}:/usr/bin:/bin; printf x", crw1283All},
	{"dash-let", map[string]string{"let": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin; let x=1'", crw1283All},
	{"dash-pushd", map[string]string{"pushd": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin; pushd /tmp'", crw1283All},
	{"dash-time", map[string]string{"time": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin; time true'", crw1283All},
	{"dash-declare", map[string]string{"declare": "#!/bin/sh\n{CMD}\n"}, "sh -c 'PATH={DIR}:/usr/bin:/bin; export PATH; declare x=1'", crw1283All},
	{"dash-mapfile", map[string]string{"mapfile": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin; mapfile x </dev/null'", crw1283All},
	{"zsh-readarray", map[string]string{"readarray": "#!/bin/sh\n{CMD}\n"}, "zsh -c 'export PATH={DIR}:/usr/bin:/bin; readarray x </dev/null'", crw1283All},
	// Found while fixing the forms above: the other words bash parses as keywords that dash searches on the PATH ((( runs nested
	// subshells there, so the word inside is the program).
	{"dash-test-clause", map[string]string{"[[": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin; [[ -n x ]]'", crw1283All},
	{"dash-arithmetic", map[string]string{"check": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin; ((check))'", crw1283All},
	{"dash-coproc", map[string]string{"coproc": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin; coproc true'", crw1283All},
	{"dash-function", map[string]string{"function": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin\nfunction f {\ntrue\n}'", crw1283All},
	{"dash-select", map[string]string{"select": "#!/bin/sh\n{CMD}\n"}, "sh -c 'export PATH={DIR}:/usr/bin:/bin\nselect x in a\ndo break; done' </dev/null", crw1283All},
	// A script the assigned PATH finds under an interpreter's name: the reader reads the interpreter's inline program, not the file.
	{"python3-named-script", map[string]string{"python3": "#!/bin/sh\n{CMD}\n"}, "PATH={DIR}:/usr/bin:/bin python3 -c 'print(1)'", crw1283All},
	// The GitHub guard refused python3 -m before and after; the memory and worktree guards did not before the relaxation either way.
	{"python3-m-script", map[string]string{"python3": "#!/bin/sh\n{CMD}\n"}, "PATH={DIR}:/usr/bin:/bin python3 -m json.tool /dev/null", crw1283All},
	{"perl-e-script", map[string]string{"perl": "#!/bin/sh\n{CMD}\n"}, "PATH={DIR}:/usr/bin:/bin perl -e 1", crw1283All},
	{"node-e-script", map[string]string{"node": "#!/bin/sh\n{CMD}\n"}, "PATH={DIR}:/usr/bin:/bin node -e 1", crw1283All},
	{"awk-script", map[string]string{"awk": "#!/bin/sh\n{CMD}\n"}, "PATH={DIR}:/usr/bin:/bin awk 'BEGIN{}'", crw1283All},
}

// crw1283ProbeCommand writes the probe's files into the target's directory and returns the command for that target.
func crw1283ProbeCommand(t *testing.T, p crw1283Probe, target crw1283Target) string {
	t.Helper()
	fields := strings.Fields(target.command)
	r := strings.NewReplacer("{CMD}", target.command, "{PROG}", fields[0], "{REST}", strings.Join(fields[1:], " "), "{DIR}", target.dir)
	for name, body := range p.files {
		file := filepath.Join(target.dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(r.Replace(body)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r.Replace(p.command)
}

// TestReview1283ProbeForms: every form the second review found allowed on b2ae50e0 is refused by each guard that refused it before
// the CRW-1283 relaxations.
func TestReview1283ProbeForms(t *testing.T) {
	t.Setenv("HOME", cleanTempDir(t))
	for _, p := range crw1283Probes {
		t.Run(p.name, func(t *testing.T) {
			for _, target := range crw1283Targets(t) {
				if !strings.Contains(" "+strings.Join(p.guards, " ")+" ", " "+target.name+" ") {
					continue
				}
				command := crw1283ProbeCommand(t, p, target)
				if !target.refused(command) {
					t.Errorf("%s guard ALLOWED %s: %s", target.name, p.name, command)
				}
			}
		})
	}
}

// TestReview1283ProbeControls: the relaxations the probes narrow still admit what CRW-1283 asks for: a forwarding wrapper, a
// PATH assignment in front of a program the PATH finds in a system directory, and a builtin every shell runs itself.
func TestReview1283ProbeControls(t *testing.T) {
	t.Setenv("HOME", cleanTempDir(t))
	for _, target := range crw1283Targets(t) {
		slot := crw1283Script(t, target.dir, "slot.sh", "#!/bin/bash\nset -u\nexec ionice -c3 nice \"$@\"\n")
		for _, command := range []string{
			"bash " + slot + " go version",
			slot + " go version",
			"export PATH=" + target.dir + ":/usr/bin:/bin; cd /tmp; true",
			"PATH=/usr/bin:/bin printf x",
			"export PATH=/usr/bin:/bin; printf x; echo y; test -n z",
			"bash -c '\"$@\"' z go version",
		} {
			if target.refused(command) {
				t.Errorf("%s guard refused a control: %s", target.name, command)
			}
		}
		for _, command := range []string{"bash " + slot + " " + target.command, slot + " " + target.command} {
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED the forwarded protected command: %s", target.name, command)
			}
		}
	}
}

// TestGithubPostRefusalSaysWhetherItPosts: a refusal of the GitHub guard never says that the command names no GitHub post (Sion
// decision 1): when the reader cannot read whether the command posts, the refusal says so.
func TestGithubPostRefusalSaysWhetherItPosts(t *testing.T) {
	cwd, _, _ := gateScene(t)
	for _, command := range []string{`bash -c "$UNKNOWN"`, `ionice -c3 nice "$@"`, `$UNKNOWN version`, `ionice -c3 nice $X go version`} {
		out := HandleGitHubPostGuard(gateBash(t, cwd, command))
		if out == "" {
			t.Errorf("GitHub guard allowed an unreadable command: %s", command)
			continue
		}
		if strings.Contains(out, "names no GitHub post") {
			t.Errorf("GitHub guard refusal says the command names no GitHub post: %s => %s", command, out)
		}
		if !strings.Contains(out, "cannot tell whether the command posts") {
			t.Errorf("GitHub guard refusal does not say the reader cannot tell whether the command posts: %s => %s", command, out)
		}
	}
}
