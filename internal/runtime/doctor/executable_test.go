package doctor_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// A script is judged by what it runs, not by its #! line alone: the exec polyglot (a line of
// three single quotes and exec after #!/bin/sh) that pip and uv write when the interpreter's
// path is over 127 bytes or contains a space re-executes itself under Python; any script
// inside a venv belongs to that venv; and a shell wrapper that execs a Python interpreter or
// console script runs Python. A wrapper that execs a native binary does not; one that execs a
// word it cannot expand, or hands a native program text it may run, is unreadable.
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
	write(t, filepath.Join(root, "bin", "via-exec"), "#!/bin/sh\n# run the relay\nexec \""+filepath.Join(venv, "bin", "longshebang")+"\" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-name"), "#!/usr/bin/env bash\ntrue && python3 -m relay \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "native"), "\x7fELF\x02\x01\x01\x00", 0o755)
	write(t, filepath.Join(root, "bin", "via-native"), "#!/bin/sh -e\nexec "+filepath.Join(root, "bin", "native")+" --x \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-variable"), "#!/bin/sh\nexec \"$RELAY_HOME/bin/relay\" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "message"), "#!/bin/sh\n"+filepath.Join(root, "bin", "native")+" \"then run setup.py\"\n", 0o755)
	classifier := doctor.Classifier{Expand: doctor.Expander{Path: "/usr/bin:/bin"}}
	for path, want := range map[string]string{
		filepath.Join(venv, "bin", "longshebang"):          doctor.KindPythonScript,
		filepath.Join(root, "elsewhere", "longshebang"):    doctor.KindPythonScript,
		filepath.Join(root, "elsewhere", "graalpy-script"): doctor.KindPythonScript,
		filepath.Join(venv, "bin", "activate-and-run"):     doctor.KindPythonScript,
		filepath.Join(root, "bin", "via-exec"):             doctor.KindPythonScript,
		filepath.Join(root, "bin", "via-name"):             doctor.KindPythonScript,
		filepath.Join(root, "bin", "via-native"):           doctor.KindScript,
		filepath.Join(root, "bin", "via-variable"):         doctor.KindUnreadable,
		filepath.Join(root, "bin", "message"):              doctor.KindUnreadable,
	} {
		e := classifier.Classify(path, "")
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
// true &&, a bare command PATH resolves to a venv console script makes it Python, and a command
// outside the grammar (cd, set, an assignment) leaves it unreadable (finding 14: a wrapper that
// hands a runner "$PY", or a variable it does not set, is not judged clean).
func TestAShellWrapperIsJudgedAtEveryCommandItRuns(t *testing.T) {
	root := t.TempDir()
	venv := filepath.Join(root, "venv")
	write(t, filepath.Join(venv, "pyvenv.cfg"), "home = /usr/bin\n", 0o644)
	link(t, "/bin/sh", filepath.Join(venv, "bin", "python3"))
	write(t, filepath.Join(venv, "bin", "codex-session-relay"), "#!"+filepath.Join(venv, "bin", "python3")+"\n", 0o755)
	bin := filepath.Join(root, "bin")
	link(t, filepath.Join(venv, "bin", "codex-session-relay"), filepath.Join(bin, "relay"))
	write(t, filepath.Join(bin, "runner"), "\x7fELF\x02\x01\x01\x00runner", 0o755)
	native := filepath.Join(root, "native")
	write(t, native, "\x7fELF\x02\x01\x01\x00", 0o755)
	wrappers := filepath.Join(root, "wrappers")
	for name, body := range map[string]string{
		"exec-gone":     "exec gone-relay \"$@\"",
		"runs-gone":     "true && gone-relay",
		"runs-relay":    "set -e\nrelay \"$@\"",
		"runs-native":   "true && exec " + native,
		"changes-dir":   "cd /tmp && exec " + native,
		"assigns":       "PY=\"${CRW_PYTHON:-/usr/bin/python3}\"\nexec runner /tmp/x \"$PY\" -m codex_session_relay \"$@\"",
		"runner-varies": "exec runner /tmp/x \"$CRW_PYTHON\" -m codex_session_relay \"$@\"",
	} {
		write(t, filepath.Join(wrappers, name), "#!/bin/sh\n"+body+"\n", 0o755)
	}
	classifier := doctor.Classifier{Expand: doctor.Expander{Path: bin + ":/usr/bin:/bin"}}
	for name, want := range map[string]string{
		"exec-gone":     "gone-relay",
		"runs-gone":     "gone-relay",
		"runs-relay":    doctor.KindPythonScript,
		"runs-native":   doctor.KindScript,
		"changes-dir":   `"cd"`,
		"assigns":       "an assignment",
		"runner-varies": "$CRW_PYTHON",
	} {
		e := classifier.Classify(filepath.Join(wrappers, name), "")
		if strings.HasPrefix(want, "python") || want == doctor.KindScript {
			if e.Kind != want {
				t.Errorf("%s: kind %s (%s), want %s", name, e.Kind, e.Detail, want)
			}
		} else if e.Kind != doctor.KindUnreadable || !strings.Contains(e.Detail, want) {
			t.Errorf("%s: kind %s (%s), want unreadable naming %s", name, e.Kind, e.Detail, want)
		}
	}
}

// realPython is a copy of the host's Python interpreter under name in dir: a native image whose
// name says nothing.
func realPython(t *testing.T, dir, name string) string {
	t.Helper()
	found, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH to copy")
	}
	source, err := os.Open(found)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(dir, name)
	mkdir(t, dir)
	target, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := io.Copy(target, source); err != nil {
		t.Fatal(err)
	}
	return path
}

// A #! interpreter is resolved and classified like any reference (finding 12): a link to Python
// under another name, a copy of the Python image under another name, and a #! script that is
// itself Python make the script Python. An interpreter that is neither a shell this scan reads,
// env nor Python (perl, busybox), and a file with no #! that is not native (a shell runs it as
// a shell script), are unreadable (finding 13). A free-threaded or debug interpreter is found
// by its image and by its name (finding 16).
func TestAScriptIsJudgedByTheInterpreterItsHashBangResolvesTo(t *testing.T) {
	root := t.TempDir()
	image := realPython(t, filepath.Join(root, "native"), "interp")
	link(t, image, filepath.Join(root, "native", "py"))
	write(t, filepath.Join(root, "native", "busybox"), "\x7fELF\x02\x01\x01\x00busybox", 0o755)
	write(t, filepath.Join(root, "native", "perl"), "\x7fELF\x02\x01\x01\x00perl", 0o755)
	scripts := filepath.Join(root, "scripts")
	for name, body := range map[string]string{
		"linked":       "#!" + filepath.Join(root, "native", "py") + "\nimport sys\n",
		"copied":       "#!" + image + "\nimport sys\n",
		"nested":       "#!" + filepath.Join(scripts, "linked") + "\n",
		"env-copied":   "#!/usr/bin/env interp\n",
		"perl":         "#!" + filepath.Join(root, "native", "perl") + "\nexec 'python3','-m','codex_session_relay';\n",
		"busybox":      "#!" + filepath.Join(root, "native", "busybox") + " sh\npython3 -m codex_session_relay hook\n",
		"no-hash-bang": "python3 -m codex_session_relay hook\n",
		"env-option":   "#!/usr/bin/env -S python3 -u\n",
		"gone":         "#!" + filepath.Join(root, "native", "nothing") + "\n",
	} {
		write(t, filepath.Join(scripts, name), body, 0o755)
	}
	classifier := doctor.Classifier{Expand: doctor.Expander{Path: filepath.Join(root, "native") + ":/usr/bin:/bin"}}
	for name, want := range map[string]string{
		"linked":       doctor.KindPythonScript,
		"copied":       doctor.KindPythonScript,
		"nested":       doctor.KindPythonScript,
		"env-copied":   doctor.KindPythonScript,
		"perl":         doctor.KindUnreadable,
		"busybox":      doctor.KindUnreadable,
		"no-hash-bang": doctor.KindOther,
		"env-option":   doctor.KindUnreadable,
		"gone":         doctor.KindScript,
	} {
		if e := classifier.Classify(filepath.Join(scripts, name), ""); e.Kind != want || e.Python != (want == doctor.KindPythonScript) {
			t.Errorf("%s: kind %s python %v (%s), want %s", name, e.Kind, e.Python, e.Detail, want)
		}
	}
	threaded := realPython(t, filepath.Join(root, "threaded"), "python3.13t")
	for _, path := range []string{image, threaded, filepath.Join(root, "missing", "python3.13t"), filepath.Join(root, "missing", "python3.12-dbg")} {
		if e := classifier.Classify(path, ""); !e.Python {
			t.Errorf("%s: kind %s (%s), not Python", path, e.Kind, e.Detail)
		}
	}
}
