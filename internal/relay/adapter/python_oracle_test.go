package adapter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The Python reference implementation's answers to this package's parity tests are replayed from
// testdata/python-oracle (internal/testsupport/pyoracle). With CRW_PYTHON_ORACLE=record or check
// the drivers under testdata run live against the Python implementation, as they always did.

// pyRepo is the repository root, where the Python drivers run.
func pyRepo(t testing.TB) string {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

var pyTests struct {
	sync.Mutex
	parents  map[testing.TB]string
	counters map[testing.TB]map[string]int
}

// pyTempParent is the directory every t.TempDir of the calling test lies in: the one path each
// of the test's own paths starts with, on every run.
func pyTempParent(t testing.TB) string {
	t.Helper()
	pyTests.Lock()
	defer pyTests.Unlock()
	if parent, ok := pyTests.parents[t]; ok {
		return parent
	}
	if pyTests.parents == nil {
		pyTests.parents = map[testing.TB]string{}
	}
	parent := filepath.Dir(t.TempDir())
	pyTests.parents[t] = parent
	t.Cleanup(func() {
		pyTests.Lock()
		defer pyTests.Unlock()
		delete(pyTests.parents, t)
	})
	return parent
}

// pyKey names the next question the calling test asks driver: the driver and how many times the
// test asked it before, since a test asks its questions in a fixed order.
func pyKey(t testing.TB, driver string) string {
	t.Helper()
	pyTests.Lock()
	defer pyTests.Unlock()
	if pyTests.counters == nil {
		pyTests.counters = map[testing.TB]map[string]int{}
	}
	asked, ok := pyTests.counters[t]
	if !ok {
		asked = map[string]int{}
		pyTests.counters[t] = asked
		t.Cleanup(func() {
			pyTests.Lock()
			defer pyTests.Unlock()
			delete(pyTests.counters, t)
		})
	}
	asked[driver]++
	return fmt.Sprintf("%s #%d", driver, asked[driver])
}

// pyOptions stores the run's own paths as placeholders: the caller's first (a longer path before
// any prefix of it), then the test's temporary directories, the suite directory (TMPDIR, the
// fake hosts' sockets and the built binaries) and the repository.
func pyOptions(t testing.TB, extra ...pyoracle.Option) []pyoracle.Option {
	t.Helper()
	options := append([]pyoracle.Option{}, extra...)
	for _, path := range []struct{ actual, placeholder string }{{pyTempParent(t), "<test-tmp>"}, {suiteDirectory, "<suite>"}, {pyRepo(t), "<repo>"}} {
		options = append(options, pyoracle.Substitute(path.actual, path.placeholder))
		if real, err := filepath.EvalSymlinks(path.actual); err == nil && real != path.actual {
			options = append(options, pyoracle.Substitute(real, path.placeholder))
		}
	}
	return options
}

// pyOutput is a driver's combined output, which it must end with a zero exit.
func pyOutput(t testing.TB, key string, command func() *exec.Cmd, options ...pyoracle.Option) []byte {
	t.Helper()
	return pyNormalized(t, key, command, nil, options...)
}

// pyNormalized is pyOutput passed through normalize before it is recorded, for an answer that
// carries a value the test does not compare and a rerun changes.
func pyNormalized(t testing.TB, key string, command func() *exec.Cmd, normalize func([]byte) ([]byte, error), options ...pyoracle.Option) []byte {
	t.Helper()
	return pyoracle.Answer(t, key, func() ([]byte, error) {
		cmd := command()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("%v\n%s", err, out)
		}
		if normalize != nil {
			return normalize(out)
		}
		return out, nil
	}, pyOptions(t, options...)...)
}

// pyDriver is pyOutput for a driver under testdata that reads its question on stdin.
func pyDriver(t testing.TB, driver string, input []byte, options ...pyoracle.Option) []byte {
	t.Helper()
	return pyDriverNormalized(t, driver, input, nil, options...)
}

// pyDriverNormalized is pyDriver with pyNormalized's normalize.
func pyDriverNormalized(t testing.TB, driver string, input []byte, normalize func([]byte) ([]byte, error), options ...pyoracle.Option) []byte {
	t.Helper()
	repo := pyRepo(t)
	return pyNormalized(t, pyKey(t, driver), func() *exec.Cmd {
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata", driver))
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(input)
		return cmd
	}, normalize, options...)
}

// asGoAnswers names a value that follows from the run's own paths, such as a digest over a
// manifest that lists the test's temporary files or an offset into a document that spells one:
// it cannot be the same on a rerun. Where Python answered what Go answers in the run that
// records, the recording holds the placeholder and a replay puts back what Go answers then; check
// mode compares the live Python with that Go value again. Anywhere else the value stays as
// Python answered it.
func asGoAnswers(goValue, placeholder string) pyoracle.Option {
	return pyoracle.Substitute(goValue, placeholder)
}

// pyExit is how a Python process ended: its exit code and what it wrote.
type pyExit struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr,omitempty"`
}

// pyProcess runs command and answers how it ended; only a process that could not run fails.
// combined puts stderr into Stdout, as CombinedOutput does.
func pyProcess(t testing.TB, key string, combined bool, command func() *exec.Cmd, options ...pyoracle.Option) pyExit {
	t.Helper()
	var answer pyExit
	pyoracle.JSON(t, key, &answer, func() (any, error) {
		cmd := command()
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if combined {
			cmd.Stderr = &stdout
		}
		err := cmd.Run()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			return nil, err
		}
		return pyExit{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}, pyOptions(t, options...)...)
	return answer
}

// sqliteValue is one column value of a table dump, typed as SQLite stores it: an integer or a
// text is plain JSON, a real and a blob are tagged.
func sqliteValue(value any) any {
	switch v := value.(type) {
	case float64:
		return map[string]any{"real": v}
	case []byte:
		return map[string]any{"blob": base64.StdEncoding.EncodeToString(v)}
	default:
		return v
	}
}

// sqliteArgument turns a sqliteValue decoded with json.Number back into what SQLite stores.
func sqliteArgument(value any) (any, error) {
	switch v := value.(type) {
	case nil, string:
		return v, nil
	case json.Number:
		return v.Int64()
	case map[string]any:
		if real, ok := v["real"].(json.Number); ok {
			return real.Float64()
		}
		if blob, ok := v["blob"].(string); ok {
			return base64.StdEncoding.DecodeString(blob)
		}
	}
	return nil, fmt.Errorf("not a table dump value: %#v", value)
}

// withoutWallClock is a normalize for a driver that leaves Python's wall clock running: every
// startedAt and updatedAt in its JSON answer, which the test does not compare, becomes
// <wall-clock>.
func withoutWallClock(raw []byte) ([]byte, error) {
	var answer any
	if err := decodeNumbers(raw, &answer); err != nil {
		return nil, fmt.Errorf("%w: %s", err, raw)
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				if _, ok := item.(json.Number); ok && (key == "startedAt" || key == "updatedAt") {
					v[key] = "<wall-clock>"
					continue
				}
				walk(item)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(answer)
	return encodeJSON(answer)
}

// encodeJSON is json.Marshal leaving '<', '>' and '&' as they are, so a placeholder reads as
// itself in a recording.
func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// decodeNumbers decodes raw keeping numbers as json.Number.
func decodeNumbers(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(out)
}

// quoted is a SQL identifier.
func quoted(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
