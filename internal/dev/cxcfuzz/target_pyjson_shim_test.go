//go:build dev

package cxcfuzz

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The pyjson shim's contract (CRW-1066): whatever path it takes to ask Python, the answer for one request is the
// answer of a fresh `python3 -m json.tool --json-lines` process fed that request's text -- the exit status,
// stdout and stderr, each with its one trailing newline stripped -- and the shim reuses its Python processes
// instead of starting one per request. These tests drive the committed shim through the real worker pool, with a
// python3 on PATH that logs every process it starts, so a spawn is counted rather than assumed.

// pyjsonRig is a worker pool over the committed pyjson shim whose python3 is a logging wrapper: every python3
// process the worker starts appends its pid to a log before it runs the real interpreter.
type pyjsonRig struct {
	t    *testing.T
	pool *Pool
	env  []string
	log  string
	real string
}

func requirePython(t *testing.T) string {
	t.Helper()
	requireNode(t)
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 is not on PATH: %v", err)
	}
	return path
}

// newPyjsonRig starts nothing: the first Call starts the worker. The wrapper is a shell script that records
// $$ -- after exec the interpreter keeps that pid -- and then runs the real python3 with the same arguments.
func newPyjsonRig(t *testing.T, timeout time.Duration) *pyjsonRig {
	t.Helper()
	real := requirePython(t)
	dir := t.TempDir()
	rig := &pyjsonRig{t: t, log: filepath.Join(dir, "spawns.log"), real: real}
	script := "#!/bin/sh\necho $$ >> '" + rig.log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PATH=") {
			rig.env = append(rig.env, entry)
		}
	}
	rig.env = append(rig.env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	target := pyjsonTarget()
	pool, err := NewPool(target.Oracle, 1, timeout, DefaultStartupTimeout, rig.env)
	if err != nil {
		t.Fatal(err)
	}
	rig.pool = pool
	t.Cleanup(func() {
		rig.reap()
		_ = pool.Close()
		rig.reap()
	})
	return rig
}

// pids are the python3 processes the worker has started so far, in start order.
func (r *pyjsonRig) pids() []int {
	raw, err := os.ReadFile(r.log)
	if err != nil {
		return nil
	}
	var out []int
	for _, field := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(field); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func (r *pyjsonRig) spawns() int { return len(r.pids()) }

// jsonTool is whether pid is a live json.tool process the wrapper started. A pid the log names that was reused by an
// unrelated process, or has ended, is not one.
func jsonTool(pid int) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.Contains(string(out), "json.tool")
}

// live are the logged python3 processes that still run.
func (r *pyjsonRig) live() []int {
	var out []int
	for _, pid := range r.pids() {
		if jsonTool(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// signal sends sig to every live logged python3 and returns how many it reached.
func (r *pyjsonRig) signal(sig syscall.Signal) int {
	n := 0
	for _, pid := range r.live() {
		if syscall.Kill(pid, sig) == nil {
			n++
		}
	}
	return n
}

// reap makes sure no logged python3 outlives the test, a stopped one included.
func (r *pyjsonRig) reap() {
	r.signal(syscall.SIGCONT)
	r.signal(syscall.SIGKILL)
}

// call sends one request the way the campaign does and returns the decoded answer.
func (r *pyjsonRig) call(wire string) (pyjsonAnswer, error) {
	raw, err := r.pool.Call(wire, "")
	if err != nil {
		return pyjsonAnswer{}, err
	}
	var answer pyjsonAnswer
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		return pyjsonAnswer{}, fmt.Errorf("the answer %.200s is not an exit/stdout/stderr object: %w", raw, err)
	}
	return answer, nil
}

type pyjsonAnswer struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// pyjsonWire is a request line's input for text and the json.tool options, as the generator draws them.
func pyjsonWire(req pyjsonRequest) string {
	raw, err := json.Marshal(struct {
		Text        string `json:"text"`
		Indent      int    `json:"indent"`
		Compact     bool   `json:"compact"`
		SortKeys    bool   `json:"sortKeys"`
		EnsureAscii bool   `json:"ensureAscii"`
	}{req.Text, req.Indent, req.Compact, req.SortKeys, req.EnsureAscii})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// pyjsonReference is the contract's right-hand side: one fresh python3 -m json.tool --json-lines per request, the
// flags the shim documents, the text on stdin. The flag mapping is written out here, not read from the shim.
func pyjsonReference(python string, req pyjsonRequest) (pyjsonAnswer, error) {
	argv := []string{"-m", "json.tool", "--json-lines"}
	switch {
	case req.Compact:
		argv = append(argv, "--compact")
	case req.Indent == 0:
		argv = append(argv, "--no-indent")
	case req.Indent != 4:
		argv = append(argv, "--indent", strconv.Itoa(req.Indent))
	}
	if req.SortKeys {
		argv = append(argv, "--sort-keys")
	}
	if !req.EnsureAscii {
		argv = append(argv, "--no-ensure-ascii")
	}
	cmd := exec.Command(python, argv...)
	cmd.Stdin = strings.NewReader(req.Text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		var failed *exec.ExitError
		if !errors.As(err, &failed) {
			return pyjsonAnswer{}, err
		}
		exit = failed.ExitCode()
	}
	return pyjsonAnswer{Exit: exit, Stdout: strings.TrimSuffix(stdout.String(), "\n"), Stderr: strings.TrimSuffix(stderr.String(), "\n")}, nil
}

// pyjsonContractRequests are the requests the equivalence contract covers: the committed pyjson cases, the
// boundary documents under every option, hand-written invalid, multi-line and newline-ended texts, deep nesting on
// both sides of Python's recursion limit, and a generated sample of at least 1,000 documents.
func pyjsonContractRequests(t *testing.T) []pyjsonRequest {
	t.Helper()
	var out []pyjsonRequest
	cases, err := LoadCases(filepath.Join("testdata", "pyjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		input, err := decode(c.Input)
		if err != nil {
			t.Fatalf("case %s: %v", c.Name, err)
		}
		out = append(out, pyjsonRead(input))
	}
	defaults := pyjsonRequest{Indent: 4, EnsureAscii: true}
	options := []func(pyjsonRequest) pyjsonRequest{
		func(r pyjsonRequest) pyjsonRequest { return r },
		func(r pyjsonRequest) pyjsonRequest { r.Compact = true; return r },
		func(r pyjsonRequest) pyjsonRequest { r.Indent = 0; return r },
		func(r pyjsonRequest) pyjsonRequest { r.Indent = 2; r.SortKeys = true; return r },
		func(r pyjsonRequest) pyjsonRequest { r.Indent = 8; r.EnsureAscii = false; return r },
		func(r pyjsonRequest) pyjsonRequest { r.SortKeys = true; r.EnsureAscii = false; return r },
	}
	extra := []string{
		"", "\n", "\n\n", " ", "1", "1\n", "1\n2", "1\n2\n", "1\r\n2", "1\r2", "1\r", `{"a": `, `{"a": 1`, `{"a": 1}` + "\n" + `{`,
		"{\n\"a\": 1\n}", "[1,\n2]", `"unterminated`, `[1,]`, `{"a" 1}`, `{'a': 1}`, `nul`, `tru`, `-`, `01`, `1.`, `.5`, `+1`,
		`"\x"`, `"` + "\t" + `"`, "\ufeff1", `[1] [2]`, `{"b": 1, "a": {"d": 2, "c": [3, {"f": 4, "e": 5}]}}`,
		`"é ☃ 😀"`, `{"é": "☃"}`, `1E400`, `-1e400`, `[NaN, Infinity, -Infinity]`, `1e5`, `1.0`, `123456789012345678901234567890`,
		`"\ud800"`, `"\udc00"`, `"\ud800\ud800"`, `{"\ud800": 1}`,
		pyjsonDeepDocument(1), pyjsonDeepDocument(300), pyjsonDeepDocument(990), pyjsonDeepDocument(995), pyjsonDeepDocument(997),
		pyjsonDeepDocument(998), pyjsonDeepDocument(1000), pyjsonDeepDocument(1003), pyjsonDeepDocument(1200),
	}
	for _, text := range append(append([]string{}, pyjsonDocuments...), extra...) {
		for _, option := range options {
			base := defaults
			base.Text = text
			out = append(out, option(base))
		}
	}
	rng := rand.New(rand.NewSource(1066))
	for i := 0; i < 1000; i++ {
		out = append(out, pyjsonRead(pyjsonGenerate(rng, rng.Int())))
	}
	return out
}

// referenceAnswers computes the contract's expected answers on a few goroutines, in request order.
func referenceAnswers(t *testing.T, python string, requests []pyjsonRequest) []pyjsonAnswer {
	t.Helper()
	answers := make([]pyjsonAnswer, len(requests))
	errs := make([]error, len(requests))
	var next int64 = -1
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= len(requests) {
					return
				}
				answers[i], errs[i] = pyjsonReference(python, requests[i])
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("the reference run of request %d: %v", i, err)
		}
	}
	return answers
}

// The shim answers every request exactly as a fresh python3 -m json.tool --json-lines process does -- exit
// status, stdout and stderr -- for the committed cases, the boundary documents under every option and a generated
// sample of at least 1,000 documents (invalid documents, lone surrogate escapes, deep nesting around Python's
// recursion limit, and the --compact, --indent, --sort-keys and --no-ensure-ascii flags), and it does so while
// starting Python for fewer than half of the requests (CRW-1066 c1).
func TestPyjsonShimMatchesAFreshJsonToolForEveryRequest(t *testing.T) {
	rig := newPyjsonRig(t, 60*time.Second)
	requests := pyjsonContractRequests(t)
	if len(requests) < 1000 {
		t.Fatalf("the contract covers %d requests, want at least 1000", len(requests))
	}
	want := referenceAnswers(t, rig.real, requests)
	var invalid, deep, surrogate, flagged int
	for i, req := range requests {
		got, err := rig.call(pyjsonWire(req))
		if err != nil {
			t.Fatalf("request %d (%q %+v): %v", i, req.Text, req, err)
		}
		if got != want[i] {
			t.Errorf("request %d text %.80q indent %d compact %v sortKeys %v ensureAscii %v:\n shim      exit %d stdout %.200q stderr %.200q\n reference exit %d stdout %.200q stderr %.200q",
				i, req.Text, req.Indent, req.Compact, req.SortKeys, req.EnsureAscii, got.Exit, got.Stdout, got.Stderr, want[i].Exit, want[i].Stdout, want[i].Stderr)
		}
		if want[i].Exit != 0 {
			invalid++
		}
		if pyjsonNesting(req.Text) >= 990 {
			deep++
		}
		if strings.Contains(req.Text, `\ud`) {
			surrogate++
		}
		if req.Compact || req.SortKeys || !req.EnsureAscii || req.Indent != 4 {
			flagged++
		}
	}
	if invalid < 200 || deep < 100 || surrogate < 50 || flagged < 500 {
		t.Errorf("the contract sample is too thin: %d invalid, %d deep, %d surrogate, %d flagged of %d", invalid, deep, surrogate, flagged, len(requests))
	}
	if spawns := rig.spawns(); spawns*2 >= len(requests) {
		t.Errorf("the shim started python3 %d times for %d requests; reuse means fewer than half", spawns, len(requests))
	}
	t.Logf("%d requests, %d python3 processes started, %d invalid, %d deep, %d surrogate, %d flagged", len(requests), rig.spawns(), invalid, deep, surrogate, flagged)
}

// Valid documents under one option set are answered by one reused Python process, not one per request.
func TestPyjsonShimReusesOnePythonForValidDocuments(t *testing.T) {
	rig := newPyjsonRig(t, 30*time.Second)
	for i := 0; i < 60; i++ {
		req := pyjsonRequest{Text: fmt.Sprintf(`{"n": %d, "s": "v%d"}`, i, i), Indent: 4, EnsureAscii: true}
		got, err := rig.call(pyjsonWire(req))
		if err != nil {
			t.Fatal(err)
		}
		want, err := pyjsonReference(rig.real, req)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("request %d: %+v, want %+v", i, got, want)
		}
	}
	if spawns := rig.spawns(); spawns > 2 {
		t.Fatalf("60 valid documents under one option set started python3 %d times, want at most 2", spawns)
	}
}

// A refused document ends the Python process that read it; the next request is answered all the same, by a process
// started for it, and a refusal is never taken for a process failure.
func TestPyjsonShimAnswersAfterARefusedDocument(t *testing.T) {
	rig := newPyjsonRig(t, 30*time.Second)
	sequence := []string{`{"a": 1}`, `{"a": `, `{"a": 2}`, `[1,]`, `{"a": 3}`, ``, `[4]`}
	for i, text := range sequence {
		req := pyjsonRequest{Text: text, Indent: 4, EnsureAscii: true}
		got, err := rig.call(pyjsonWire(req))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		want, err := pyjsonReference(rig.real, req)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("request %d %q: %+v, want %+v", i, text, got, want)
		}
	}
}

// A reused Python process that is killed from outside is a dead-worker case, never a silent answer: the request that
// needed it fails with the dead-worker cause, the worker is replaced, and the next request is answered.
func TestPyjsonShimReportsAKilledReusedProcessAsADeadWorker(t *testing.T) {
	rig := newPyjsonRig(t, 30*time.Second)
	req := pyjsonRequest{Text: `{"a": 1}`, Indent: 4, EnsureAscii: true}
	if _, err := rig.call(pyjsonWire(req)); err != nil {
		t.Fatal(err)
	}
	if killed := rig.signal(syscall.SIGKILL); killed == 0 {
		t.Fatal("no reused json.tool process was running after the first request: the shim starts Python for each request and ends it")
	}
	req.Text = `{"a": 2}`
	_, err := rig.call(pyjsonWire(req))
	var failure CaseFailure
	if !errors.As(err, &failure) || failure.Cause != CauseDeadWorker {
		t.Fatalf("err = %v, want a dead-worker case failure", err)
	}
	req.Text = `{"a": 3}`
	got, err := rig.call(pyjsonWire(req))
	if err != nil {
		t.Fatalf("the replacement worker did not answer: %v", err)
	}
	want, err := pyjsonReference(rig.real, req)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("answer %+v, want %+v", got, want)
	}
}

// A reused Python process that stops answering is a timeout case under the pool's deadline, and ending the worker
// ends the Python process with it: nothing stopped is left running.
func TestPyjsonShimReportsAHungReusedProcessAsATimeoutAndLeavesNothingRunning(t *testing.T) {
	rig := newPyjsonRig(t, 1500*time.Millisecond)
	req := pyjsonRequest{Text: `{"a": 1}`, Indent: 4, EnsureAscii: true}
	if _, err := rig.call(pyjsonWire(req)); err != nil {
		t.Fatal(err)
	}
	if stopped := rig.signal(syscall.SIGSTOP); stopped == 0 {
		t.Fatal("no reused json.tool process was running after the first request: the shim starts Python for each request and ends it")
	}
	req.Text = `{"a": 2}`
	_, err := rig.call(pyjsonWire(req))
	if !errors.Is(err, Timeout{}) {
		t.Fatalf("err = %v, want a Timeout", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(rig.live()) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if left := rig.live(); len(left) > 0 {
		t.Fatalf("python3 processes %v outlive the worker that was killed for the timeout", left)
	}
}

// A campaign whose reused Python process dies in the middle of the run counts exactly that case as a dead worker,
// records it, replaces the worker and finishes the other cases, and the campaign fails (CRW-1066 c5).
func TestCampaignCountsAReusedProcessThatDiesMidCampaign(t *testing.T) {
	rig := newPyjsonRig(t, 30*time.Second)
	target := pyjsonTarget()
	var cases atomic.Int64
	killed := 0
	target.Generate = func(rng *rand.Rand, size int) any {
		return pyjson.Object{{Key: "text", Value: fmt.Sprintf(`{"n": %d}`, rng.Intn(1000000))}}
	}
	goSide := target.Go
	target.Go = func(input any, env Env) (any, error) {
		if cases.Add(1) == 6 {
			killed = rig.signal(syscall.SIGKILL)
		}
		return goSide(input, env)
	}
	out := t.TempDir()
	summary, err := Campaign(Config{Target: target, Cases: 12, Seed: 1066, SeedSet: true, Workers: 1, Out: out, Env: rig.env})
	if err != nil {
		t.Fatal(err)
	}
	if killed == 0 {
		t.Fatal("no reused json.tool process was running when the sixth case began: the shim starts Python for each request and ends it")
	}
	if summary.Cases != 12 || summary.DeadWorkers != 1 || summary.Same != 11 || summary.Timeouts != 0 {
		t.Fatalf("summary %+v, want 12 cases, 1 dead worker and 11 agreements", summary)
	}
	if !campaignFailed(summary) {
		t.Fatal("a campaign that lost a case to a dead worker did not fail")
	}
	records, err := filepath.Glob(filepath.Join(out, "failures", "*.json"))
	if err != nil || len(records) != 1 {
		t.Fatalf("failure records %v (%v), want one", records, err)
	}
	raw, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	var record Failure
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Cause != CauseDeadWorker || record.Case != 6 {
		t.Fatalf("failure record %+v, want the dead-worker record of case 6", record)
	}
}

// A lone surrogate that reaches the shim as a character of the text (a \ud800 escape in the request line itself, not
// in the document) is written to Python the way Node writes it, as U+FFFD, and the answer is what a fresh json.tool
// gives that text.
func TestPyjsonShimWritesALoneSurrogateOfTheTextAsAReplacementCharacter(t *testing.T) {
	rig := newPyjsonRig(t, 30*time.Second)
	for _, row := range []struct{ name, wireText, text string }{
		{"in a string", `"\"\ud800\""`, "\"\ufffd\""},
		{"alone", `"\udc00"`, "\ufffd"},
		{"in a key", `"{\"\ud800\": 1}"`, "{\"\ufffd\": 1}"},
	} {
		wire := `{"text":` + row.wireText + `,"indent":4,"compact":false,"sortKeys":false,"ensureAscii":false}`
		got, err := rig.call(wire)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		want, err := pyjsonReference(rig.real, pyjsonRequest{Text: row.text, Indent: 4})
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: %+v, want %+v", row.name, got, want)
		}
	}
}
