package shellir

import "testing"

// TestEnvChdirRepeatedOperandIsUnreadable (CRW-910 evaluation of 39c72966, d1): a second env directory operand is refused, since the
// reader cannot tell which directory the program runs in. Accepting it let "env -C sub -C . rm -rf ../repo" delete the checkout.
func TestEnvChdirRepeatedOperandIsUnreadable(t *testing.T) {
	for _, cmd := range []string{"env -C sub -C . rm -rf ../repo", "env --chdir=sub --chdir=. rm x", "env -C sub --chdir . rm x"} {
		if _, err := Analyze(cmd, "/work"); !isUnreadable(err) {
			t.Errorf("%s: err = %v, want Unreadable", cmd, err)
		}
	}
}

// TestEnvChdirReplacedOperandIsUnknownDir (CRW-910 evaluation of 39c72966, d2): find's {} is the path each run of the program starts
// in, so an operand holding it leaves the program's directory unknown, as cd does with the same operand.
func TestEnvChdirReplacedOperandIsUnknownDir(t *testing.T) {
	r, err := Analyze(`find /memory -maxdepth 0 -exec env -C '{}' rm x \;`, "/work")
	if err != nil {
		t.Fatal(err)
	}
	rms := 0
	for _, e := range r.Execs {
		if e.Name == "rm" {
			rms++
			if e.Dir.Known {
				t.Errorf("rm after env -C {} has known dir %+v, want unknown", e.Dir)
			}
		}
	}
	if rms != 1 {
		t.Fatalf("execs = %+v, want one rm", r.Execs)
	}
}

// TestEnvChdirRedirectsOpenInOuterDirectory (CRW-910 evaluation of 39c72966, d3): the shell opens a redirection before env changes
// directory, so a relative file of it is in the directory the command runs in, not the one env moves to.
func TestEnvChdirRedirectsOpenInOuterDirectory(t *testing.T) {
	r, err := Analyze("env -C /tmp ls > out.txt", "/work")
	if err != nil {
		t.Fatal(err)
	}
	ls := r.Execs[len(r.Execs)-1]
	if len(ls.Redirs) != 1 || !ls.Redirs[0].Target.Known || ls.Redirs[0].Target.Value != "/work/out.txt" {
		t.Errorf("ls redirs = %+v, want the file /work/out.txt", ls.Redirs)
	}
	if ls.Dir.Path != "/tmp" || !ls.Dir.Known {
		t.Errorf("ls dir = %+v, want known /tmp", ls.Dir)
	}
	r, err = Analyze("cd x && env -C /tmp ls > out.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	ls = r.Execs[len(r.Execs)-1]
	if len(ls.Redirs) != 1 || ls.Redirs[0].Target.Known {
		t.Errorf("ls redirs in an unknown outer directory = %+v, want an unknown file", ls.Redirs)
	}
}
