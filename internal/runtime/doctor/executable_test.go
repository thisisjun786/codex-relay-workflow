package doctor_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// A script is judged by what it runs, not by its #! line alone: a shell wrapper that execs a
// native binary runs that binary (a script); one that execs a word it cannot expand, hands a
// native program text it may run, or re-executes itself under another interpreter (the shell
// launcher pip and uv write when an interpreter's path is too long for #! or contains a space) is
// unreadable.
func TestClassifyJudgesAScriptByWhatItRuns(t *testing.T) {
	root := t.TempDir()
	link(t, "/bin/sh", filepath.Join(root, "long path", "bin", "python3"))
	polyglot := "#!/bin/sh\n'''exec' \"" + filepath.Join(root, "long path", "bin", "python3") + "\" \"$0\" \"$@\"\n' '''\nimport sys\n"
	write(t, filepath.Join(root, "elsewhere", "longshebang"), polyglot, 0o755)
	write(t, filepath.Join(root, "bin", "native"), "\x7fELF\x02\x01\x01\x00", 0o755)
	write(t, filepath.Join(root, "bin", "via-exec"), "#!/bin/sh\n# run the relay\nexec \""+filepath.Join(root, "elsewhere", "longshebang")+"\" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-native"), "#!/bin/sh -e\nexec "+filepath.Join(root, "bin", "native")+" --x \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "via-variable"), "#!/bin/sh\nexec \"$RELAY_HOME/bin/relay\" \"$@\"\n", 0o755)
	write(t, filepath.Join(root, "bin", "message"), "#!/bin/sh\n"+filepath.Join(root, "bin", "native")+" \"then run setup.py\"\n", 0o755)
	classifier := doctor.Classifier{Expand: doctor.Expander{Path: "/usr/bin:/bin"}}
	for path, want := range map[string]string{
		filepath.Join(root, "elsewhere", "longshebang"): doctor.KindUnreadable,
		filepath.Join(root, "bin", "via-exec"):          doctor.KindUnreadable,
		filepath.Join(root, "bin", "via-native"):        doctor.KindScript,
		filepath.Join(root, "bin", "via-variable"):      doctor.KindUnreadable,
		filepath.Join(root, "bin", "message"):           doctor.KindUnreadable,
	} {
		if e := classifier.Classify(path, ""); e.Kind != want {
			t.Errorf("%s: kind %s (%s), want %s", filepath.Base(path), e.Kind, e.Detail, want)
		}
	}
}

// A shell wrapper is judged at every command it runs, not only at an exec target: a bare
// command no PATH directory holds leaves it unreadable whether it is exec'd or run after
// true &&, a bare command PATH resolves to a script is judged as that script, and a command
// outside the grammar (cd, set, an assignment) leaves it unreadable (finding 14: a wrapper that
// hands a runner "$PY", or a variable it does not set, is not judged clean).
func TestAShellWrapperIsJudgedAtEveryCommandItRuns(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	link(t, "/bin/sh", filepath.Join(scripts, "bin", "python3"))
	write(t, filepath.Join(scripts, "bin", "codex-session-relay"), "#!"+filepath.Join(scripts, "bin", "python3")+"\n", 0o755)
	bin := filepath.Join(root, "bin")
	link(t, filepath.Join(scripts, "bin", "codex-session-relay"), filepath.Join(bin, "relay"))
	write(t, filepath.Join(bin, "runner"), "\x7fELF\x02\x01\x01\x00runner", 0o755)
	native := filepath.Join(root, "native")
	write(t, native, "\x7fELF\x02\x01\x01\x00", 0o755)
	wrappers := filepath.Join(root, "wrappers")
	for name, body := range map[string]string{
		"exec-gone":     "exec gone-relay \"$@\"",
		"runs-gone":     "true && gone-relay",
		"runs-relay":    "relay \"$@\"",
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
		"runs-relay":    doctor.KindScript,
		"runs-native":   doctor.KindScript,
		"changes-dir":   `"cd"`,
		"assigns":       "an assignment",
		"runner-varies": "$CRW_PYTHON",
	} {
		e := classifier.Classify(filepath.Join(wrappers, name), "")
		if want == doctor.KindScript {
			if e.Kind != want {
				t.Errorf("%s: kind %s (%s), want %s", name, e.Kind, e.Detail, want)
			}
		} else if e.Kind != doctor.KindUnreadable || !strings.Contains(e.Detail, want) {
			t.Errorf("%s: kind %s (%s), want unreadable naming %s", name, e.Kind, e.Detail, want)
		}
	}
}

// A #! interpreter is resolved and classified like any reference (finding 12): a shell this
// reading reads (sh, bash, dash) and env are followed; any other interpreter, found directly,
// through a link, through env or through another #! script (perl, busybox, a Python), is
// unreadable, and so is a file with no #! that is not native (a shell runs it as a shell script,
// finding 13) and an env given an option. An interpreter that does not exist runs nothing.
func TestAScriptIsJudgedByTheInterpreterItsHashBangResolvesTo(t *testing.T) {
	root := t.TempDir()
	image := filepath.Join(root, "native", "interp")
	write(t, image, "\x7fELF\x02\x01\x01\x00interp", 0o755)
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
		"linked":       doctor.KindUnreadable,
		"copied":       doctor.KindUnreadable,
		"nested":       doctor.KindUnreadable,
		"env-copied":   doctor.KindUnreadable,
		"perl":         doctor.KindUnreadable,
		"busybox":      doctor.KindUnreadable,
		"no-hash-bang": doctor.KindOther,
		"env-option":   doctor.KindUnreadable,
		"gone":         doctor.KindScript,
	} {
		if e := classifier.Classify(filepath.Join(scripts, name), ""); e.Kind != want {
			t.Errorf("%s: kind %s (%s), want %s", name, e.Kind, e.Detail, want)
		}
	}
}
