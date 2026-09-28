package skill

import (
	"os"
	"strings"
	"testing"
)

// oracleEnv is the environment every live-Python oracle and its Go counterpart run in
// (decision 29b). Python picks its stdin error handler from the locale: under C, POSIX
// and the coercion targets C.UTF-8/UTF-8 (a CI runner's default) it decodes stdin with
// surrogateescape, under the UTF-8 locale of a supported host it refuses invalid bytes.
// PYTHONIOENCODING=utf-8:strict pins the host behaviour, and LC_ALL=C.UTF-8 pins the
// UTF-8 file encoding. The Go commands read bytes and never consult the locale.
func oracleEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "LANG" || key == "LANGUAGE" || strings.HasPrefix(key, "LC_") ||
			key == "PYTHONUTF8" || key == "PYTHONIOENCODING" || key == "PYTHONCOERCECLOCALE" {
			continue
		}
		env = append(env, kv)
	}
	return append(append(env, "LC_ALL=C.UTF-8", "PYTHONIOENCODING=utf-8:strict"), extra...)
}

const pythonTracebackHeader = "Traceback (most recent call last):\n"

// stripPythonTraceback removes only a leading Python traceback block: the
// header and the indented frame lines that follow it, up to the final
// exception line. Everything else, including that final line, is kept.
func stripPythonTraceback(stderr string) string {
	rest, found := strings.CutPrefix(stderr, pythonTracebackHeader)
	if !found {
		return stderr
	}
	for strings.HasPrefix(rest, "  ") {
		end := strings.IndexByte(rest, '\n')
		if end < 0 {
			return stderr
		}
		rest = rest[end+1:]
	}
	return rest
}

// skillProcessParity is the decision 29a contract: exit code and stdout are
// byte-identical, and stderr is byte-identical once Python's traceback frames
// are removed. Go stderr is never stripped, so fake Go frames still fail.
func skillProcessParity(python, gocli skillProcessResult) bool {
	return python.exit == gocli.exit && python.stdout == gocli.stdout &&
		stripPythonTraceback(python.stderr) == gocli.stderr
}

func TestSkillProcessParityComparator(t *testing.T) {
	traceback := pythonTracebackHeader +
		"  File \"/x/hook_probe.py\", line 1544, in <module>\n" +
		"    raise SystemExit(main())\n" +
		"                     ~~~~^^\n" +
		"AttributeError: 'NoneType' object has no attribute 'get'\n"
	python := skillProcessResult{exit: 1, stdout: "out\n", stderr: traceback}
	final := "AttributeError: 'NoneType' object has no attribute 'get'\n"
	cases := []struct {
		name  string
		gocli skillProcessResult
		want  bool
	}{
		{"frames only differ", skillProcessResult{1, "out\n", final}, true},
		{"different exit", skillProcessResult{2, "out\n", final}, false},
		{"different stdout", skillProcessResult{1, "other\n", final}, false},
		{"different final line", skillProcessResult{1, "out\n", "TypeError: 'NoneType' object has no attribute 'get'\n"}, false},
		{"missing final line", skillProcessResult{1, "out\n", ""}, false},
		{"go prints frames", skillProcessResult{1, "out\n", traceback}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// Given Python's uncaught exception, when Go differs as named, then parity reports it.
			if got := skillProcessParity(python, test.gocli); got != test.want {
				t.Fatalf("skillProcessParity = %v, want %v", got, test.want)
			}
		})
	}
	plain := skillProcessResult{2, "", "usage: x\n"}
	if skillProcessParity(plain, skillProcessResult{2, "", "usage: y\n"}) {
		t.Fatal("ordinary stderr difference was accepted")
	}
	if !skillProcessParity(plain, plain) {
		t.Fatal("identical ordinary result was rejected")
	}
}
