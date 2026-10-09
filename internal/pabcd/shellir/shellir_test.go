package shellir

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func names(r Result) []string {
	var out []string
	for _, e := range r.Execs {
		out = append(out, e.Name)
	}
	return out
}

func isUnreadable(err error) bool {
	var u *Unreadable
	return errors.As(err, &u)
}

// TestCommandVerdicts pins the verdict and the program names of representative
// commands. Each row is one shape the reader must read or must refuse.
func TestCommandVerdicts(t *testing.T) {
	cases := []struct {
		name       string
		cmd        string
		unreadable bool
		want       []string
	}{
		{"plain command", "gh pr comment 1 --body hi", false, []string{"gh"}},
		{"literal variable", "MSG=lit; gh pr comment --body \"$MSG\"", false, []string{"", "gh"}},
		{"unknown argument is still readable", "echo \"$X\"", false, []string{"echo"}},
		{"unknown program", "$CMD x", true, nil},
		{"bash -c literal", "bash -c 'gh pr comment'", false, []string{"bash", "gh"}},
		{"bash -c unknown string", "bash -c \"$X\"", true, nil},
		{"python -c literal", "python3 -c 'print(1)'", false, []string{"python3"}},
		{"python -c unknown", "python3 -c \"$X\"", true, nil},
		{"here-document into shell", "bash <<EOF\necho hi\nEOF\n", false, []string{"bash", "echo"}},
		{"pipe into shell", "echo hi | sh", true, nil},
		{"eval of computed text", "eval \"$(x)\"", true, nil},
		{"cd then rm", "cd /tmp && rm x", false, []string{"cd", "rm"}},
		{"cd in one branch", "if x; then cd /a; fi; rm y", false, []string{"x", "cd", "rm"}},
		{"env -S literal", "env -S 'gh pr'", false, []string{"env", "gh"}},
		{"env -S with expansion", "env -S 'a $X'", true, nil},
		{"PATH assignment", "PATH=/x gh", true, nil},
		{"code environment name", "BASH_ENV=/tmp/x bash -c 'echo'", true, nil},
		{"git alias key", "git -c alias.x=!sh status", true, nil},
		{"git plain subcommand", "git status", false, []string{"git"}},
		{"git unlisted subcommand", "git frob", true, nil},
		{"npm script shell", "npm --script-shell=/bin/sh install", true, nil},
		{"zsh repeat", "repeat 2 echo", true, nil},
		{"function body walked", "f() { echo hi; }; f", false, []string{"echo", "f", "echo"}},
		{"function shadows gh", "gh() { :; }", true, nil},
		{"command substitution", "x=$(gh pr view)", false, []string{"gh", ""}},
		{"find -exec", "find . -exec rm {} \\;", false, []string{"find", "rm"}},
		{"find -exec without terminator", "find . -exec rm {}", true, nil},
		{"sudo login shell", "sudo -i", true, nil},
		{"timeout wrapper", "timeout 5 gh x", false, []string{"timeout", "gh"}},
		{"nice wrapper", "nice -n 5 gh", false, []string{"nice", "gh"}},
		{"sed inline", "sed -n 1p f", false, []string{"sed"}},
		{"source file", "source ./x.sh", false, []string{"source", "source"}},
		{"parse error", "echo \"", true, nil},
		{"su without -c", "su root", true, nil},
		{"su -c literal", "su -c 'gh pr'", false, []string{"su", "gh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Analyze(tc.cmd, "/work")
			if tc.unreadable {
				if !isUnreadable(err) {
					t.Fatalf("Analyze(%q) error = %v, want Unreadable", tc.cmd, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Analyze(%q) error = %v", tc.cmd, err)
			}
			if got := names(r); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Analyze(%q) names = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestCdTrackingAndScriptFile(t *testing.T) {
	r, err := Analyze("cd /tmp && rm x", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Execs[len(r.Execs)-1].Dir; got.Path != "/tmp" || !got.Known {
		t.Fatalf("rm dir = %+v, want known /tmp", got)
	}
	r, err = Analyze("if x; then cd /a; fi; rm y", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Execs[len(r.Execs)-1].Dir; got.Known {
		t.Fatalf("rm after a conditional cd has known dir %+v", got)
	}
	r, err = Analyze("bash ./job.sh", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Execs) != 2 || r.Execs[1].Kind != KindScriptFile || r.Execs[1].Script.Value != "./job.sh" {
		t.Fatalf("bash script file records = %+v", r.Execs)
	}
	if _, err := Analyze("cd x && bash ./job.sh", ""); !isUnreadable(err) {
		t.Fatalf("script file in unknown directory: err = %v, want Unreadable", err)
	}
}

// TestEnvChdirMovesTheProgramDirectory: env -C and env --chdir move the directory of the program env runs, and the shell's own
// directory stays. An operand that leaves the known path makes the program's directory unknown, as cd does.
func TestEnvChdirMovesTheProgramDirectory(t *testing.T) {
	r, err := Analyze("env -C sub rm x; rm y", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Execs) != 3 || r.Execs[1].Dir.Path != "/work/sub" || !r.Execs[1].Dir.Known || r.Execs[2].Dir.Path != "/work" || !r.Execs[2].Dir.Known {
		t.Fatalf("execs = %+v, want rm in /work/sub and then rm in /work", r.Execs)
	}
	r, err = Analyze("env --chdir=/tmp rm x", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Execs) != 2 || r.Execs[1].Dir.Path != "/tmp" || !r.Execs[1].Dir.Known {
		t.Fatalf("env --chdir=/tmp rm execs = %+v, want rm in known /tmp", r.Execs)
	}
	r, err = Analyze("env -C ../x rm y", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Execs) != 2 || r.Execs[1].Dir.Known {
		t.Fatalf("env -C ../x rm execs = %+v, want rm in an unknown directory", r.Execs)
	}
	for _, tc := range []struct {
		name    string
		cmd     string
		findDir string
	}{
		{"env inside find", `find . -name x -exec env -C sub rm {} +; rm y`, "/work"},
		{"find inside env", `env -C sub find . -name x -exec rm {} +; rm y`, "/work/sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Analyze(tc.cmd, "/work")
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Execs) != 4 || r.Execs[2].Name != "rm" || r.Execs[3].Name != "rm" {
				t.Fatalf("execs = %+v, want wrapper, wrapper, rm, rm", r.Execs)
			}
			wrapped := r.Execs[2]
			if !wrapped.Dir.Known || wrapped.Dir.Path != "/work/sub" {
				t.Errorf("wrapped rm dir = %+v, want known /work/sub", wrapped.Dir)
			}
			feed := wrapped.Ctx.Feed
			if feed == nil || feed.Wrapper != "find" || !feed.Dir.Known || feed.Dir.Path != tc.findDir || len(feed.Starts) != 1 || !feed.Starts[0].Known || feed.Starts[0].Value != "." {
				t.Errorf("wrapped rm feed = %+v, want find from %s starting at .", feed, tc.findDir)
			}
			plain := r.Execs[3]
			if !plain.Dir.Known || plain.Dir.Path != "/work" || plain.Ctx.Feed != nil {
				t.Errorf("plain rm = %+v, want known /work without a feed", plain)
			}
		})
	}
}

func TestInlineProgramBytes(t *testing.T) {
	r, err := Analyze("python3 -c 'print(\"a\\n\")'", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if r.Execs[0].Inline == nil || r.Execs[0].Inline.Source.Value != "print(\"a\\n\")" {
		t.Fatalf("inline = %+v", r.Execs[0].Inline)
	}
	r, err = Analyze("sed -e 1p -e 2d f", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if r.Execs[0].Inline == nil || r.Execs[0].Inline.Source.Value != "1p\n2d" {
		t.Fatalf("sed inline = %+v", r.Execs[0].Inline)
	}
}

func TestHereDocumentAndHerestring(t *testing.T) {
	r, err := Analyze("cat <<-EOF\n\tbody\n\tEOF\n", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Execs[0].Redirs[0].Target; got.Value != "body\n" {
		t.Fatalf("<<- body = %q, want %q", got.Value, "body\n")
	}
	r, err = Analyze("cat <<< 'x'", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Execs[0].Redirs[0].Target; got.Value != "x\n" {
		t.Fatalf("here-string = %q, want %q", got.Value, "x\n")
	}
	r, err = Analyze("cat <<EOF\n$HOME\nEOF\n", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Execs[0].Redirs[0].Target; got.Known {
		t.Fatalf("unquoted here-document with expansion is known: %+v", got)
	}
}

func TestCommandSizeBoundary(t *testing.T) {
	ok := "true" + strings.Repeat(" ", MaxCommandBytes-len("true"))
	if _, err := Analyze(ok, ""); err != nil {
		t.Fatalf("command at the limit: %v", err)
	}
	over := ok + " "
	if _, err := Analyze(over, ""); !isUnreadable(err) {
		t.Fatalf("command over the limit: err = %v, want Unreadable", err)
	}
}

func TestNestingBoundary(t *testing.T) {
	nest := func(depth int) string {
		return strings.Repeat("( ", depth) + "true" + strings.Repeat(" )", depth)
	}
	if _, err := Analyze(nest(MaxNestingDepth), ""); err != nil {
		t.Fatalf("nesting at the limit: %v", err)
	}
	if _, err := Analyze(nest(MaxNestingDepth+1), ""); !isUnreadable(err) {
		t.Fatalf("nesting over the limit: err = %v, want Unreadable", err)
	}
}

// TestWordsMatchBash evaluates static words with the layer and with bash.
// Every word the layer reads as known must print the same bytes under bash.
func TestWordsMatchBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	words := []string{
		"'a b'", "\"x\\$y\"", "$'\\x41'", "a\\ b", "\"a\"'b'", "\\\"", "$'\\t'", "''",
		"\"a\\\\b\"", "'it'\"'\"'s'", "$'\\101\\n'", "a\\nb", "\"\\\\\"", "x\"$\"y",
	}
	for _, word := range words {
		r, err := Analyze("cmd "+word, "/work")
		if err != nil {
			continue
		}
		got := r.Execs[0].Args[0]
		if !got.Known {
			continue
		}
		out, err := exec.Command(bash, "-c", "printf '%s' "+word).Output()
		if err != nil {
			t.Fatalf("bash %s: %v", word, err)
		}
		if string(out) != got.Value {
			t.Errorf("word %s: layer %q, bash %q", word, got.Value, string(out))
		}
	}
}
