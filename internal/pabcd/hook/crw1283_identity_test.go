package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two P0 defects of the evaluation of deb45bd1 (/state/crw/mgmt/pre-eval/crw1283-deb45bd1.json), both introduced by the PATH
// relaxation: a directory name and a file header decided what a program is (d1), and a script's shebang interpreter that an
// assigned PATH selects was never read (d2). Each form was refused before the relaxation (c007bd50) and stays refused (Sion
// decision 5). No test runs the command.

// TestReview1283SystemDirectoryNameDoesNotIdentifyAProgram (d1): a program an assigned PATH finds in /usr/bin, /bin or /usr/local/bin
// with a binary header is not the program its name says: the PATH the shell starts with must run the same file for that name.
// /usr/bin/static-sh (busybox, a shell under a name the reader does not know) stands for the evaluation's /usr/local/bin/check.
func TestReview1283SystemDirectoryNameDoesNotIdentifyAProgram(t *testing.T) {
	if info, err := os.Stat("/usr/bin/static-sh"); err != nil || !info.Mode().IsRegular() {
		t.Skip("no /usr/bin/static-sh on this host")
	}
	empty := cleanTempDir(t)
	t.Setenv("PATH", empty)
	for _, target := range crw1283Targets(t) {
		linkDir := filepath.Join(target.dir, "usr", "local", "bin")
		if err := os.MkdirAll(linkDir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(linkDir, "check"))
		if err := os.Symlink("/usr/bin/static-sh", filepath.Join(linkDir, "check")); err != nil {
			t.Fatal(err)
		}
		crw1283Script(t, target.dir, "body.sh", target.command+"\n")
		for _, form := range []string{
			"PATH=/usr/bin:/bin static-sh -c '{CMD}'",
			"PATH=/usr/local/bin:/usr/bin:/bin static-sh -c '{CMD}'",
			"export PATH=/usr/bin:/bin; static-sh -c '{CMD}'",
			"env PATH=/usr/bin:/bin static-sh -c '{CMD}'",
			"PATH=/usr/bin:/bin nice static-sh -c '{CMD}'",
			"PATH=/usr/bin:/bin static-sh {DIR}/body.sh",
			"PATH={DIR}/usr/local/bin:/usr/bin:/bin check -c '{CMD}'",
		} {
			command := strings.NewReplacer("{DIR}", target.dir, "{CMD}", target.command).Replace(form)
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a system-directory program the starting PATH does not run: %s", target.name, command)
			}
		}
	}
	// The PATH the shell starts with runs the same file: the assignment does not change which program runs.
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, target := range crw1283Targets(t) {
		for _, control := range []string{"PATH=/usr/bin:/bin printf x", "PATH=/usr/bin:/bin ls /tmp", "export PATH=/usr/bin:/bin; env true", "PATH=/usr/bin:/bin nice true"} {
			if target.refused(control) {
				t.Errorf("%s guard refused a system program the starting PATH runs: %s", target.name, control)
			}
		}
	}
}

// TestReview1283ShebangInterpreterOnAssignedPath (d2): a script's #! line names the interpreter through env, and the assigned PATH
// selects the file that env runs. The interpreter is resolved on that PATH and must be the file the starting PATH runs for its
// name; any other is refused, whatever the script's own body reads.
func TestReview1283ShebangInterpreterOnAssignedPath(t *testing.T) {
	for _, target := range crw1283Targets(t) {
		evil := filepath.Join(target.dir, "p")
		if err := os.MkdirAll(evil, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"check": "#!/usr/bin/env bash\nprintf ok\n", "split": "#!/usr/bin/env -S bash -e\nprintf ok\n",
			"py": "#!/usr/bin/env python3\nprint(1)\n", "bash": "#!/bin/sh\n" + target.command + "\n", "python3": "#!/bin/sh\n" + target.command + "\n"} {
			crw1283Script(t, evil, name, body)
		}
		for _, form := range []string{
			"PATH={P}:/usr/bin:/bin check",
			"export PATH={P}:/usr/bin:/bin; check",
			"env PATH={P}:/usr/bin:/bin check",
			"PATH={P}:/usr/bin:/bin nice check",
			"PATH={P}:/usr/bin:/bin {P}/check",
			"PATH={P}:/usr/bin:/bin split",
			"PATH={P}:/usr/bin:/bin py",
			"export PATH={P}:/usr/bin:/bin; {P}/check",
		} {
			command := strings.ReplaceAll(form, "{P}", evil)
			if !target.refused(command) {
				t.Errorf("%s guard ALLOWED a script whose env interpreter the assigned PATH selects: %s", target.name, command)
			}
		}
		// The assigned PATH holds no interpreter of that name before the system directories: the script is read as before.
		quiet := filepath.Join(target.dir, "q")
		if err := os.MkdirAll(quiet, 0o755); err != nil {
			t.Fatal(err)
		}
		crw1283Script(t, quiet, "check", "#!/usr/bin/env bash\nset -eu\nprintf ok\n")
		for _, control := range []string{"PATH={Q}:/usr/bin:/bin check", "export PATH={Q}:/usr/bin:/bin; {Q}/check", "PATH={Q}:/usr/bin:/bin nice check"} {
			command := strings.ReplaceAll(control, "{Q}", quiet)
			if target.refused(command) {
				t.Errorf("%s guard refused a script whose env interpreter is the one the starting PATH runs: %s", target.name, command)
			}
		}
	}
}
