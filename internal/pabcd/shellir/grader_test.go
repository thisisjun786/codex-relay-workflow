package shellir

import "testing"

// TestGraderReproductionsRefused holds the shapes an independent review reproduced against the reader: a redirection on
// a compound command, a cd that a loop body makes, an unquoted expansion that splits, a shell builtin behind an external
// program, and parallel. Each is judged on what the shell would run, never on the reading the walk first gave.
func TestGraderReproductionsRefused(t *testing.T) {
	t.Run("compound redirection is a write", func(t *testing.T) {
		for _, cmd := range []string{"{ :; } > /home/u/.codex/memories/n.md", "( : ) > /home/u/.codex/memories/n.md"} {
			r, err := Analyze(cmd, "/work")
			if err != nil {
				t.Fatalf("%q: %v", cmd, err)
			}
			found := false
			for _, e := range r.Execs {
				for _, rd := range e.Redirs {
					if rd.Op == ">" && rd.Target.Value == "/home/u/.codex/memories/n.md" {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("%q: the redirection of the compound command is not in the records: %+v", cmd, r.Execs)
			}
		}
	})
	t.Run("a cd in a loop body is unknown after the loop", func(t *testing.T) {
		r, err := Analyze("while false; do cd /tmp; done; rm -rf ../repo", "/tmp/wt/slot/repo")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range r.Execs {
			if e.Name == "rm" && e.Dir.Known {
				t.Errorf("rm runs in a known directory %q after a loop that changed it", e.Dir.Path)
			}
		}
	})
	t.Run("an unquoted expansion that splits is not one program", func(t *testing.T) {
		r, err := Analyze("bash -c 'CMD=\"rm -rf ../repo\"; $CMD'", "/tmp/wt/slot/repo")
		if err != nil && !isUnreadable(err) {
			t.Fatal(err)
		}
		for _, e := range r.Execs {
			if e.Program.Known && e.Program.Value == "rm -rf ../repo" {
				t.Errorf("the split value is one known program %q", e.Program.Value)
			}
		}
		r, err = Analyze("X=\"rm -rf ../repo\"; $X", "/work")
		if err != nil && !isUnreadable(err) {
			t.Fatal(err)
		}
		for _, e := range r.Execs {
			if e.Program.Known && e.Program.Value != "" {
				t.Errorf("an unquoted expansion with blanks is a known program %q", e.Program.Value)
			}
		}
	})
	t.Run("a shell builtin behind an external program is refused", func(t *testing.T) {
		for _, cmd := range []string{"env cd /tmp; rm -rf ../repo", "nice cd /tmp", "nohup export A=1"} {
			if _, err := Analyze(cmd, "/tmp/wt/slot/repo"); !isUnreadable(err) {
				t.Errorf("%q: err %v, want unreadable", cmd, err)
			}
		}
	})
	t.Run("parallel runs its operands", func(t *testing.T) {
		if _, err := Analyze("parallel rm -rf {} ::: /tmp/wt/slot/repo", "/work"); !isUnreadable(err) {
			t.Errorf("parallel: err %v, want unreadable", err)
		}
	})
}
