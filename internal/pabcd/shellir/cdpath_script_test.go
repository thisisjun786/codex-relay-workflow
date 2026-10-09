package shellir

import "testing"

// TestAnalyzeScriptCdpath: a script run with CDPATH set is read with it set, and a record that runs a script or a program carries
// whether CDPATH may be set there (CRW-875).
func TestAnalyzeScriptCdpath(t *testing.T) {
	body := "cd sub\nbash post.sh\n"
	r, err := AnalyzeScript(body, "/work", false)
	if err != nil {
		t.Fatal(err)
	}
	if last := r.Execs[len(r.Execs)-1]; last.Kind != KindScriptFile || !last.Dir.Known || last.Dir.Path != "/work/sub" {
		t.Fatalf("without CDPATH: %+v, want the script from /work/sub", last)
	}
	if _, err := AnalyzeScript(body, "/work", true); err == nil {
		t.Fatalf("with CDPATH: the script run from a bare-name cd must be unreadable")
	}
	for _, c := range []struct {
		cmd    string
		cdpath bool
	}{
		{"CDPATH=/x bash entry.sh", true},
		{"export CDPATH=/x; bash entry.sh", true},
		{"env CDPATH=/x bash entry.sh", true},
		{"cdpath=(/x); source entry.sh", true},
		{"bash entry.sh", false},
	} {
		r, err := Analyze(c.cmd, "/work")
		if err != nil {
			t.Fatalf("%q: %v", c.cmd, err)
		}
		found := false
		for _, e := range r.Execs {
			if e.Kind == KindScriptFile {
				found = true
				if e.Cdpath != c.cdpath {
					t.Errorf("%q: script record Cdpath=%v, want %v", c.cmd, e.Cdpath, c.cdpath)
				}
			}
		}
		if !found {
			t.Errorf("%q: no script record", c.cmd)
		}
	}
}

// TestExecLines: a record carries the line of the statement that holds it in the text it was read from.
func TestExecLines(t *testing.T) {
	r, err := Analyze("# a\n\necho a\nif true; then\n  echo b\nfi\nls\n", "/work")
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, e := range r.Execs {
		got = append(got, e.Line)
	}
	want := []int{3, 4, 5, 7}
	if len(got) != len(want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines %v, want %v", got, want)
		}
	}
}
