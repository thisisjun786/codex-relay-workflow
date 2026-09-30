package faults

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// Python's answers.
//
// These tests compare Go with what the Python reference implementation answered for the same
// input. That implementation leaves the repository (todo 44), so each answer is recorded once
// under testdata/python-oracle and read back (internal/testsupport/pyoracle). Each capture
// closure is the live-Python code that produced the answer; it runs only when
// CRW_PYTHON_ORACLE records or checks. What only live Python reads - a Python store's seed
// rows, its takeover - is prepared inside those closures or only while Python is live, so a
// replay runs no Python. Go's side of every comparison runs live in every mode.

// pyQuestions counts the questions each running test has asked Python.
var pyQuestions = struct {
	sync.Mutex
	asked map[testing.TB]int
}{asked: map[testing.TB]int{}}

// pyKey names the next question t asks Python: its number in the test's fixed sequence of
// questions, a label for the reader, and a digest of the question's words, so a question that
// changes finds no recorded answer rather than another question's.
func pyKey(t testing.TB, label string, words []string) string {
	pyQuestions.Lock()
	n, seen := pyQuestions.asked[t]
	pyQuestions.asked[t] = n + 1
	pyQuestions.Unlock()
	if !seen {
		t.Cleanup(func() {
			pyQuestions.Lock()
			delete(pyQuestions.asked, t)
			pyQuestions.Unlock()
		})
	}
	digest := sha256.Sum256([]byte(strings.Join(words, "\x00")))
	return fmt.Sprintf("%03d %s %x", n+1, label, digest[:4])
}

// pyPaths are the run-specific paths a question and its answer may carry, as pairs of the
// actual path and the placeholder a recording stores instead. A longer path comes before a
// prefix of it.
type pyPaths []string

// pyRunPaths are the paths every live-Python run of t may name: the disposable home it runs in,
// the directory holding t's temporary directories (t.TempDir), the Python package's directory,
// which Python reports as the relay's installation (relayPackageLocation), and this checkout.
func pyRunPaths(t testing.TB, home string) pyPaths {
	return pyPaths{home, "<home>", testTemp(t), "<test-tmp>", relayPackageLocation, "<relay-package>", f1Root(), "<repo>"}
}

// testTemps holds the directory each running test's t.TempDir directories are made in.
var testTemps = struct {
	sync.Mutex
	dir map[testing.TB]string
}{dir: map[testing.TB]string{}}

// testTemp is the directory t.TempDir makes t's temporary directories in.
func testTemp(t testing.TB) string {
	testTemps.Lock()
	dir, ok := testTemps.dir[t]
	testTemps.Unlock()
	if ok {
		return dir
	}
	dir = filepath.Dir(t.TempDir())
	testTemps.Lock()
	testTemps.dir[t] = dir
	testTemps.Unlock()
	t.Cleanup(func() {
		testTemps.Lock()
		delete(testTemps.dir, t)
		testTemps.Unlock()
	})
	return dir
}

func (p pyPaths) options() []pyoracle.Option {
	var opts []pyoracle.Option
	for i := 0; i+1 < len(p); i += 2 {
		opts = append(opts, pyoracle.Substitute(p[i], p[i+1]))
	}
	return opts
}

func (p pyPaths) neutral(words []string) []string {
	var pairs []string
	for i := 0; i+1 < len(p); i += 2 {
		if p[i] != "" {
			pairs = append(pairs, p[i], p[i+1])
		}
	}
	replacer := strings.NewReplacer(pairs...)
	out := make([]string, len(words))
	for i, word := range words {
		out[i] = replacer.Replace(word)
	}
	return out
}

// pyAnswer is the bytes live Python answered to question (its words, such as an argv) for t;
// capture asks it.
func pyAnswer(t testing.TB, label string, question []string, paths pyPaths, capture func() ([]byte, error)) []byte {
	t.Helper()
	label = paths.neutral([]string{label})[0]
	if runes := []rune(label); len(runes) > 72 {
		label = string(runes[:72]) + "…"
	}
	return pyoracle.Answer(t, pyKey(t, label, paths.neutral(question)), capture, paths.options()...)
}

// pyValue is pyAnswer for a value: capture's result is recorded as JSON and decoded into out as
// encoding/json decodes (numbers as float64 in an interface).
func pyValue(t testing.TB, label string, question []string, paths pyPaths, out any, capture func() (any, error)) {
	t.Helper()
	raw := pyAnswer(t, label, question, paths, func() ([]byte, error) {
		value, err := capture()
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	})
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("recorded Python answer %s: %v", raw, err)
	}
}

// pyRun is what one Python process answered: its exit status, its streams (stderr only where a
// test compares it) and, for a command given a state directory, whether that directory existed
// after it ran.
type pyRun struct {
	Code    int    `json:"code"`
	Stdout  string `json:"stdout"`
	Stderr  string `json:"stderr,omitempty"`
	Created bool   `json:"created,omitempty"`
}

// runPython runs cmd to its end. A non-zero exit is an answer; only a process that could not be
// run is an error.
func runPython(cmd *exec.Cmd, keepStderr bool) (pyRun, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := pyRun{Stdout: stdout.String()}
	if keepStderr {
		run.Stderr = stderr.String()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		run.Code = exit.ExitCode()
	} else if err != nil {
		return pyRun{}, fmt.Errorf("%v: %s", err, stderr.String())
	}
	return run, nil
}

// pyCLIRun asks live Python one relay command line with a disposable home, as the CLI oracles
// here run it: stdout and exit status, stderr when keepStderr, and whether state existed after it
// ran.
func pyCLIRun(t testing.TB, home, state string, args []string, keepStderr bool, env ...string) pyRun {
	t.Helper()
	var run pyRun
	pyValue(t, "relay "+strings.Join(args, " "), args, pyRunPaths(t, home), &run, func() (any, error) {
		cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay"}, args...)...)
		cmd.Dir = f1Root()
		cmd.Env = append(os.Environ(), env...)
		answer, err := runPython(cmd, keepStderr)
		if err != nil {
			return nil, err
		}
		if state != "" {
			answer.Created = !stateAbsent(t, state)
		}
		return answer, nil
	})
	return run
}

// pyHomeEnv is the isolated host environment the CLI oracles give live Python under home.
func pyHomeEnv(home string) []string {
	return []string{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state"), "XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "CODEX_HOME=" + filepath.Join(home, "codex"), "TMPDIR=/dev/shm"}
}

// Evidence digests.
//
// An occurrence's evidence_digest is the SHA-256 of its evidence as Python writes it compactly
// (faults.evidence_digest: sorted keys, no ASCII escaping, no whitespace). Evidence a sweep
// derives names the installation and the host record by path, so its digest changes from run to
// run as those paths do, which a path placeholder cannot follow. A recording therefore holds each
// digest Python computed that way as "<evidence-sha256:OCCURRENCE>", and a replay puts back the
// digest of the evidence that occurrence holds in the answer - Python's recorded evidence with
// this run's paths - computed as Python computes it: SHA-256 over the stored evidence text (sorted
// keys, no ASCII escaping) with its whitespace removed. A digest that is not that of its row's
// evidence is recorded as it is.

const evidencePlaceholder = "<evidence-sha256:"

// pyDecode decodes a recorded answer keeping every number as written.
func pyDecode(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%v: %s", err, raw)
	}
	return value, nil
}

// pyEncode encodes a value as a recording keeps it: numbers as written, markup unescaped.
func pyEncode(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// occurrenceEvidence walks value for occurrence rows, objects with a string occurrence_id and a
// string evidence, and calls visit with each row.
func occurrenceEvidence(value any, visit func(row map[string]any, id, evidence string)) {
	switch v := value.(type) {
	case map[string]any:
		id, idOK := v["occurrence_id"].(string)
		evidence, evidenceOK := v["evidence"].(string)
		if idOK && evidenceOK {
			visit(v, id, evidence)
		}
		for _, item := range v {
			occurrenceEvidence(item, visit)
		}
	case []any:
		for _, item := range v {
			occurrenceEvidence(item, visit)
		}
	}
}

// pythonEvidenceDigest is faults.evidence_digest of evidence as Python stored it.
func pythonEvidenceDigest(evidence string) (string, bool) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(evidence)); err != nil {
		return "", false
	}
	sum := sha256.Sum256(compact.Bytes())
	return fmt.Sprintf("%x", sum), true
}

// mapStrings is value with f applied to every string in it.
func mapStrings(value any, f func(string) string) any {
	switch v := value.(type) {
	case string:
		return f(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = mapStrings(item, f)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = mapStrings(item, f)
		}
		return out
	}
	return value
}

// recordEvidenceDigests is a live answer with each evidence digest its occurrence row's evidence
// yields replaced by that occurrence's placeholder, wherever the answer names it.
func recordEvidenceDigests(raw []byte) ([]byte, error) {
	value, err := pyDecode(raw)
	if err != nil {
		return nil, err
	}
	owner := map[string]string{}
	occurrenceEvidence(value, func(row map[string]any, id, evidence string) {
		digest, _ := row["evidence_digest"].(string)
		if computed, ok := pythonEvidenceDigest(evidence); ok && digest != "" && computed == digest {
			if current, seen := owner[digest]; !seen || id < current {
				owner[digest] = id
			}
		}
	})
	if len(owner) == 0 {
		return raw, nil
	}
	var pairs []string
	for digest, id := range owner {
		pairs = append(pairs, digest, evidencePlaceholder+id+">")
	}
	replacer := strings.NewReplacer(pairs...)
	return pyEncode(mapStrings(value, replacer.Replace))
}

// replayEvidenceDigests puts back into an answer the digest of each occurrence placeholder: that of
// the evidence the occurrence's row holds in the answer.
func replayEvidenceDigests(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte(evidencePlaceholder[1:])) {
		return raw, nil
	}
	value, err := pyDecode(raw)
	if err != nil {
		return nil, err
	}
	var pairs []string
	occurrenceEvidence(value, func(_ map[string]any, id, evidence string) {
		if digest, ok := pythonEvidenceDigest(evidence); ok {
			pairs = append(pairs, evidencePlaceholder+id+">", digest)
		}
	})
	replacer := strings.NewReplacer(pairs...)
	replayed := mapStrings(value, replacer.Replace)
	encoded, err := pyEncode(replayed)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(encoded, []byte(evidencePlaceholder)) {
		return nil, fmt.Errorf("an evidence digest placeholder names no occurrence of the answer: %s", encoded)
	}
	return encoded, nil
}

// seedCLI runs a command line on the Go store in dir, as cliCall does, at the fixed time now
// (seconds since the epoch): it seeds a store Python is then given a copy of, so the times
// Python's answer echoes are the same in every run.
func seedCLI(t *testing.T, now float64, dir string, args ...string) (int, map[string]any) {
	t.Helper()
	ctx := context.WithValue(context.Background(), f1InputsKey{}, f1Inputs{clock: &testClock{now: now}})
	var out, stderr bytes.Buffer
	code, handled := executeAsCLI(ctx, append([]string{"--state", dir}, args...), &out, &stderr)
	if !handled {
		t.Fatalf("not handled: %v", args)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("%s (stderr: %s): %v", out.String(), stderr.String(), err)
	}
	return code, payload
}

// Report the first differing field while preserving the complete comparison.
func noticeDifference(path string, want, got any) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(w))
		for key := range w {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if diff := noticeDifference(path+"."+key, w[key], g[key]); diff != "" {
				return diff
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			break
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s.length: Go %d Python %d", path, len(g), len(w))
		}
		for i := range w {
			if diff := noticeDifference(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); diff != "" {
				return diff
			}
		}
	}
	return fmt.Sprintf("%s: Go %v Python %v", path, got, want)
}
