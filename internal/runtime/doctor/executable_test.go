package doctor_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// A script is judged by what it runs, not by its #! line alone: the exec polyglot (a line of
// three single quotes and exec after #!/bin/sh) that pip and uv write when the interpreter's
// path is over 127 bytes or contains a space re-executes itself under Python; any script
// inside a venv belongs to that venv; and a
// shell wrapper that execs a Python interpreter or console script runs Python. A wrapper that
// execs a native binary does not, and one that execs a path it cannot expand is unreadable.
func TestClassifyJudgesAScriptByWhatItRuns(t *testing.T) {
	root := t.TempDir()
	venv := filepath.Join(root, "a venv with a space")
	write(t, filepath.Join(venv, "pyvenv.cfg"), "home = /usr/bin\n", 0o644)
	link(t, "/bin/sh", filepath.Join(venv, "bin", "python3"))
	polyglot := "#!/bin/sh\n'''exec' \"" + filepath.Join(venv, "bin", "python3") + "\" \"$0\" \"$@\"\n' '''\nimport sys\n"
	write(t, filepath.Join(venv, "bin", "longshebang"), polyglot, 0o755)
	write(t, filepath.Join(root, "elsewhere", "longshebang"), polyglot, 0o755)
	// GraalPy's interpreter is not named python*: only the exec polyglot form says it is Python.
	write(t, filepath.Join(root, "elsewhere", "graalpy-script"), "#!/bin/sh\n'''exec' '/opt/long path/bin/graalpy' \"$0\" \"$@\"\n' '''\n", 0o755)
	write(t, filepath.Join(venv, "bin", "activate-and-run"), "#!/bin/sh\necho hi\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-exec"), "#!/bin/sh\n# run the relay\nset -e\nexec \""+filepath.Join(venv, "bin", "longshebang")+"\" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-name"), "#!/usr/bin/env bash\ncd /tmp && python3 -m relay \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "native"), "\x7fELF\x02\x01\x01\x00", 0o755)
	write(t, filepath.Join(root, "bin", "via-native"), "#!/bin/sh\nexec "+filepath.Join(root, "bin", "native")+" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-variable"), "#!/bin/sh\nexec \"$RELAY_HOME/bin/relay\" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-variable-py"), "#!/bin/sh\nexec \"$APP/relay.py\" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "message"), "#!/bin/sh\necho \"then run setup.py\"\nexec "+filepath.Join(root, "bin", "native")+"\n", 0o755)
	for path, want := range map[string]string{
		filepath.Join(venv, "bin", "longshebang"):          doctor.KindPythonScript,
		filepath.Join(root, "elsewhere", "longshebang"):    doctor.KindPythonScript,
		filepath.Join(root, "elsewhere", "graalpy-script"): doctor.KindPythonScript,
		filepath.Join(venv, "bin", "activate-and-run"):     doctor.KindPythonScript,
		filepath.Join(root, "bin", "via-exec"):             doctor.KindPythonScript,
		filepath.Join(root, "bin", "via-name"):             doctor.KindPythonScript,
		filepath.Join(root, "bin", "via-native"):           doctor.KindScript,
		filepath.Join(root, "bin", "via-variable"):         doctor.KindUnreadable,
		filepath.Join(root, "bin", "via-variable-py"):      doctor.KindPythonScript,
		filepath.Join(root, "bin", "message"):              doctor.KindScript,
	} {
		e := doctor.Classify(path, "", "")
		if e.Kind != want || e.Python != (want == doctor.KindPythonScript) {
			t.Errorf("%s: kind %s python %v (%s), want %s", filepath.Base(path), e.Kind, e.Python, e.Detail, want)
		}
	}
}

// The scan finds the same launchers from hook commands: a venv entry point written in the
// exec polyglot form, and one outside any venv whose interpreter (GraalPy) is not named python*.
func TestRetentionScanFindsAPolyglotConsoleScript(t *testing.T) {
	h := newHost(t)
	venv := filepath.Join(h.home, "venv")
	write(t, filepath.Join(venv, "pyvenv.cfg"), "home = /usr/bin\n", 0o644)
	link(t, "/bin/sh", filepath.Join(venv, "bin", "python3"))
	script := filepath.Join(venv, "bin", "longshebang")
	write(t, script, "#!/bin/sh\n'''exec' \""+filepath.Join(venv, "bin", "python3")+"\" \"$0\" \"$@\"\n' '''\n", 0o755)
	graal := filepath.Join(h.home, "tools", "bin", "relay")
	write(t, graal, "#!/bin/sh\n'''exec' '/opt/long path/bin/graalpy' \"$0\" \"$@\"\n' '''\n", 0o755)
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "`+script+` hook", "timeout": 10}, {"type": "command", "command": "`+graal+` hook", "timeout": 10}]}]}}`, 0o600)
	want := "9:hooks.Stop[0].hooks[0].command:" + script + "|9:hooks.Stop[0].hooks[1].command:" + graal
	if got := references(h.scan(t)); strings.Join(got, "|") != want {
		t.Fatalf("python references %v", got)
	}
}

// A shell wrapper is judged at every command it runs, not only at an exec target: a bare
// command no PATH directory holds leaves it unreadable whether it is exec'd or run after
// cd ... &&, a bare command PATH resolves to a venv console script makes it Python, and a
// builtin (cd, set) runs nothing.
func TestAShellWrapperIsJudgedAtEveryCommandItRuns(t *testing.T) {
	root := t.TempDir()
	venv := filepath.Join(root, "venv")
	write(t, filepath.Join(venv, "pyvenv.cfg"), "home = /usr/bin\n", 0o644)
	link(t, "/bin/sh", filepath.Join(venv, "bin", "python3"))
	write(t, filepath.Join(venv, "bin", "codex-session-relay"), "#!"+filepath.Join(venv, "bin", "python3")+"\n", 0o755)
	bin := filepath.Join(root, "bin")
	link(t, filepath.Join(venv, "bin", "codex-session-relay"), filepath.Join(bin, "relay"))
	native := filepath.Join(root, "native")
	write(t, native, "\x7fELF\x02\x01\x01\x00", 0o755)
	wrappers := filepath.Join(root, "wrappers")
	write(t, filepath.Join(wrappers, "exec-gone"), "#!/bin/sh\nexec gone-relay \"$@\"\n", 0o755)
	write(t, filepath.Join(wrappers, "runs-gone"), "#!/bin/sh\ncd /tmp && gone-relay\n", 0o755)
	write(t, filepath.Join(wrappers, "runs-relay"), "#!/bin/sh\nset -e\nrelay \"$@\"\n", 0o755)
	write(t, filepath.Join(wrappers, "runs-builtin"), "#!/bin/sh\ncd /tmp && exec "+native+"\n", 0o755)
	classifier := doctor.Classifier{Expand: doctor.Expander{Path: bin + ":/usr/bin:/bin"}}
	for name, want := range map[string]string{
		"exec-gone":    doctor.KindUnreadable,
		"runs-gone":    doctor.KindUnreadable,
		"runs-relay":   doctor.KindPythonScript,
		"runs-builtin": doctor.KindScript,
	} {
		e := classifier.Classify(filepath.Join(wrappers, name), "")
		if e.Kind != want || (want == doctor.KindUnreadable && !strings.Contains(e.Detail, "gone-relay")) {
			t.Errorf("%s: kind %s (%s), want %s", name, e.Kind, e.Detail, want)
		}
	}
}
