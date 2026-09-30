package skill

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The parity tests in this package compare the Go commands with what the Python reference
// scripts answered. Those answers are recorded (internal/testsupport/pyoracle): a test reads them
// back, and only CRW_PYTHON_ORACLE=record or check runs Python again. Every answer a test and
// its subtests ask for is kept in one recording named after the top-level test, which therefore
// calls pythonOracleRoot before its subtests ask.

var pythonOracleRoots sync.Map // top-level test name -> testing.TB

// pythonOracleRoot makes t, a top-level test, the owner of the recording its subtests' Python
// answers are kept in.
func pythonOracleRoot(t *testing.T) {
	t.Helper()
	pythonOracleRoots.Store(t.Name(), testing.TB(t))
	t.Cleanup(func() { pythonOracleRoots.Delete(t.Name()) })
}

// oracleScope is a subtest seen as its top-level test for naming and keeping the recording,
// while failures still land on the subtest that asked.
type oracleScope struct {
	testing.TB
	root testing.TB
}

func (s oracleScope) Name() string                      { return s.root.Name() }
func (s oracleScope) Cleanup(f func())                  { s.root.Cleanup(f) }
func (s oracleScope) Errorf(format string, args ...any) { s.root.Errorf(format, args...) }

// pythonOracle answers the testing.TB that holds t's recording and the key prefix that names t
// inside it: t itself and "" for a top-level test, the top-level test and the subtest path
// otherwise.
func pythonOracle(t testing.TB) (testing.TB, string) {
	t.Helper()
	top, sub, found := strings.Cut(t.Name(), "/")
	if !found {
		return t, ""
	}
	root, ok := pythonOracleRoots.Load(top)
	if !ok {
		t.Fatalf("%s asks Python from a subtest; call pythonOracleRoot(t) at the start of %s", t.Name(), top)
	}
	return oracleScope{TB: t, root: root.(testing.TB)}, sub
}

func oracleKey(sub, key string) string {
	switch {
	case sub == "":
		return key
	case key == "":
		return sub
	}
	return sub + " | " + key
}

// pythonSubstitutions stores the run-specific paths an answer can carry as placeholders: the
// checkout as <ROOT> and each temporary directory the command names, in its arguments or as its
// working directory, as <TMP>, <TMP2>, ... It also answers the command's arguments, after the
// interpreter, spelled with those placeholders, which names the question in the recording.
func pythonSubstitutions(command *exec.Cmd) ([]pyoracle.Option, string) {
	root := repositoryRoot()
	pairs := [][2]string{{root, "<ROOT>"}}
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	seen := map[string]bool{}
	for _, value := range append([]string{command.Dir}, command.Args...) {
		value = strings.ReplaceAll(value, root, "")
		for {
			at := strings.Index(value, tmp)
			if at < 0 {
				break
			}
			rest := value[at+len(tmp):]
			base, _, _ := strings.Cut(rest, string(filepath.Separator))
			value = rest
			if base == "" || seen[base] {
				continue
			}
			seen[base] = true
			placeholder := "<TMP>"
			if len(seen) > 1 {
				placeholder = fmt.Sprintf("<TMP%d>", len(seen))
			}
			pairs = append(pairs, [2]string{tmp + base, placeholder})
		}
	}
	options := make([]pyoracle.Option, 0, len(pairs))
	var args []string
	if len(command.Args) > 1 {
		args = append(args, command.Args[1:]...)
	}
	question := strconv.Quote(strings.Join(args, " "))
	for _, pair := range pairs {
		options = append(options, pyoracle.Substitute(pair[0], pair[1]))
		question = strings.ReplaceAll(question, pair[0], pair[1])
	}
	return options, question
}

// pythonProcess answers what the Python command exited with and printed. The answer is kept in
// t's recording under the command's arguments, and under key too when one test asks the same
// arguments more than once. The command runs only in record and check mode. Python's traceback
// frames are not kept: they name the interpreter's own files, and every comparison here excludes
// them (decision 29a); the exception line that ends a traceback is kept.
func pythonProcess(t testing.TB, key string, command *exec.Cmd) skillProcessResult {
	t.Helper()
	scope, sub := pythonOracle(t)
	options, question := pythonSubstitutions(command)
	key = oracleKey(oracleKey(sub, key), question)
	var live *skillProcessResult
	run := func() skillProcessResult {
		if live == nil {
			result := captureSkillProcess(t, command)
			result.stderr = stripPythonTraceback(result.stderr)
			live = &result
		}
		return *live
	}
	exit := pyoracle.Answer(scope, oracleKey(key, "exit"), func() ([]byte, error) {
		return []byte(strconv.Itoa(run().exit)), nil
	}, options...)
	stdout := pyoracle.Answer(scope, oracleKey(key, "stdout"), func() ([]byte, error) { return []byte(run().stdout), nil }, options...)
	stderr := pyoracle.Answer(scope, oracleKey(key, "stderr"), func() ([]byte, error) { return []byte(run().stderr), nil }, options...)
	code, err := strconv.Atoi(string(exit))
	if err != nil {
		t.Fatalf("recorded exit status %q: %v", exit, err)
	}
	return skillProcessResult{exit: code, stdout: string(stdout), stderr: string(stderr)}
}

// pythonOutput answers what a Python program that must succeed printed on stdout, kept in t's
// recording under key. The command runs only in record and check mode.
func pythonOutput(t testing.TB, key string, command *exec.Cmd) []byte {
	t.Helper()
	scope, sub := pythonOracle(t)
	options, _ := pythonSubstitutions(command)
	return pyoracle.Answer(scope, oracleKey(sub, key), func() ([]byte, error) {
		result := captureSkillProcess(t, command)
		if result.exit != 0 {
			return nil, fmt.Errorf("python exited %d: %s", result.exit, result.stderr)
		}
		return []byte(result.stdout), nil
	}, options...)
}

// pythonInputs extracts testdata/python-inputs.tar.gz into a directory of t's and answers it: the
// decision, title and host fixtures and hook-contract.md as they stood when the Python answers
// were recorded (decisions/, titles/, host/, hook-contract.md). A parity test gives both sides
// these rather than the live plugin files, which keep changing after no Python is left to answer
// for them again.
func pythonInputs(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	archive, err := os.Open(filepath.Join("testdata", "python-inputs.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	unzipped, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	entries := tar.NewReader(unzipped)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0o700)
		case tar.TypeReg:
			var data []byte
			if data, err = io.ReadAll(entries); err == nil {
				err = os.WriteFile(path, data, 0o600)
			}
		default:
			err = fmt.Errorf("%s: unexpected entry type %c", header.Name, header.Typeflag)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// frozenReplayArgs points a replay at inputs, a pythonInputs directory, for every path it would
// otherwise take from the live plugin: --fixtures, --contract and --host-fixtures for hook-probe,
// --fixtures for parent-title. Other commands, and arguments ending option parsing, are answered
// unchanged.
func frozenReplayArgs(inputs, family string, args []string) []string {
	if len(args) == 0 || args[0] != "replay" || slices.Contains(args, "--") {
		return args
	}
	var paths [][2]string
	switch family {
	case "hook-probe":
		paths = [][2]string{{"--fixtures", "decisions"}, {"--contract", "hook-contract.md"}, {"--host-fixtures", "host"}}
	case "parent-title":
		paths = [][2]string{{"--fixtures", "titles"}}
	default:
		return args
	}
	out := slices.Clone(args)
	for _, path := range paths {
		given := slices.ContainsFunc(args, func(arg string) bool { return arg == path[0] || strings.HasPrefix(arg, path[0]+"=") })
		if !given {
			out = append(out, path[0], filepath.Join(inputs, path[1]))
		}
	}
	return out
}
