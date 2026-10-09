package shellir

import "testing"

// TestShellFamilyProgramPositions (CRW-894): ash, mksh, hush and the busybox applets sh, ash and hush are shells wherever bash and
// sh are: a program read from the pipe, a here-string or a here-document is read or refused, a -c string is walked, and a
// shell started by a -c string, eval, su -c or su --command= inherits the pipe.
func TestShellFamilyProgramPositions(t *testing.T) {
	for _, sh := range []string{"ash", "mksh", "hush", "pdksh", "oksh", "posh", "yash", "rbash", "busybox ash", "busybox sh", "busybox hush", "/bin/busybox ash"} {
		for _, c := range []struct {
			cmd        string
			unreadable bool
		}{
			{"printf x | " + sh, false}, // CRW-1058 reads the literal pipe program
			{"printf x | exec -a x " + sh, false},
			{"cat program | " + sh, true},
			{"cat program | exec -a x " + sh, true},
			{"printf x | " + sh + " </dev/stdin", true},
			{"printf x | " + sh + " -c '" + sh + "'", true},
			{"printf x | eval '" + sh + "'", true},
			{"printf x | su -c '" + sh + "'", true},
			{"printf x | su --command='" + sh + "'", true},
			{"printf x | if false; then :; elif " + sh + "; then :; fi", true},
			{"printf x | while " + sh + "; do :; done", true},
			{"printf x | until " + sh + "; do :; done", true},
			{sh + " <<'EOF'\n" + "echo hi\nEOF", false},
			{sh + " <<< 'echo hi'", false},
			{sh + " -c 'echo hi'", false},
			{"printf x | " + sh + " -c 'echo hi'", false},
			{"printf x | " + sh + " script.sh", false},
		} {
			_, err := Analyze(c.cmd, "/work")
			if got := err != nil; got != c.unreadable {
				t.Errorf("%q: unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
			}
		}
	}
}

// TestShellFamilyBodiesAreWalked: the text a family shell runs is walked, so a command inside it is an Exec of the layer.
func TestShellFamilyBodiesAreWalked(t *testing.T) {
	for _, cmd := range []string{
		"ash -c 'rm -rf x'", "mksh -c 'rm -rf x'", "busybox ash -c 'rm -rf x'", "/bin/busybox hush -c 'rm -rf x'",
		"ash <<< 'rm -rf x'", "mksh <<'EOF'\nrm -rf x\nEOF",
	} {
		res, err := Analyze(cmd, "/work")
		if err != nil {
			t.Errorf("%q: %v", cmd, err)
			continue
		}
		found := false
		for _, e := range res.Execs {
			if e.Kind == KindCommand && e.Name == "rm" {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no rm command was read from the shell text", cmd)
		}
	}
}

// TestBareBusybox: a busybox without an applet is refused where it could read a program (a pipe, a here-document, a here-string),
// and passes where it only prints its usage.
func TestBareBusybox(t *testing.T) {
	for cmd, unreadable := range map[string]bool{
		"printf x | busybox":      true,
		"printf x | /bin/busybox": true,
		"busybox <<< x":           true,
		"busybox <<'EOF'\nx\nEOF": true,
		"busybox":                 false,
		"/bin/busybox":            false,
		"busybox </dev/null":      false,
		"printf x | busybox cat":  false,
		"printf x | busybox echo": false,
	} {
		_, err := Analyze(cmd, "/work")
		if got := err != nil; got != unreadable {
			t.Errorf("%q: unreadable=%v, want %v (%v)", cmd, got, unreadable, err)
		}
	}
}
