//go:build dev

package stopevents

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The records these tests judge are written by the real `crw hook`, built once per test binary,
// answering Stops from the isolated Codex 0.154.0 run recorded in contract/golden/stop_event_r1.json:
// three Stops of one turn, the second and third with byte-identical payloads. A guard peer on the
// host's control.sock stands in for the relay and answers every request it is asked.

var (
	buildDir  string
	crwBinary = sync.OnceValues(func() (string, error) {
		out := filepath.Join(buildDir, "crw")
		build := exec.Command("go", "build", "-buildvcs=false", "-o", out, "./cmd/crw")
		build.Dir = repositoryRoot()
		if output, err := build.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go build ./cmd/crw: %w\n%s", err, output)
		}
		return out, nil
	})
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "crw-stopevents-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buildDir = dir
	code := m.Run()
	if err := testsupport.RemoveTempTree(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func repositoryRoot() string {
	root, _ := filepath.Abs("../../..")
	return root
}

// stopEventsScript is the Python judge these tests' recordings were taken from, and
// stopEventsFixture the Stop events they answer, both named before any test changes the working
// directory.
var (
	stopEventsScript  = filepath.Join(repositoryRoot(), "scripts", "stop_events.py")
	stopEventsFixture = filepath.Join(repositoryRoot(), "contract", "golden", "stop_event_r1.json")
)

// cpython314 is python3 when it is CPython 3.14, the interpreter the judge's parity is claimed
// against (docs/live-trial.md), or "" when there is no such interpreter to compare with.
func cpython314() string {
	python, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	out, err := exec.Command(python, "-c", "import sys; print(sys.implementation.name, *sys.version_info[:2])").Output()
	if err != nil || strings.TrimSpace(string(out)) != "cpython 3 14" {
		return ""
	}
	return python
}

// pythonReading is the exit status and output scripts/stop_events.py gave for args, run in h's
// root under HOME=home by CPython 3.14. It is replayed from this test's recording
// (internal/testsupport/pyoracle); CRW_PYTHON_ORACLE=record or check runs the script again, and
// only on a checkout that still has it and an interpreter that is CPython 3.14. missing is the
// run-specific name a row reads as an absent directory.
func pythonReading(t *testing.T, key string, h *host, missing, home string, args []string) (int, string) {
	t.Helper()
	var answer struct {
		Code   int    `json:"code"`
		Stdout string `json:"stdout"`
	}
	pyoracle.JSON(t, key, &answer, func() (any, error) {
		python := cpython314()
		if python == "" {
			return nil, errors.New("python3 is not CPython 3.14, the interpreter the judge's parity is claimed against")
		}
		cmd := exec.Command(python, append([]string{stopEventsScript}, args...)...)
		cmd.Dir, cmd.Env = h.root, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "PYTHONDONTWRITEBYTECODE=1"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				return nil, err
			}
			code = exit.ExitCode()
		}
		return map[string]any{"code": code, "stdout": stdout.String()}, nil
	}, pyoracle.Substitute(h.root, "$ROOT"), pyoracle.Substitute(missing, "$MISSING"))
	return answer.Code, answer.Stdout
}

type stopFixture struct {
	TranscriptLines []string `json:"transcriptLines"`
	Stops           []struct {
		Payload     map[string]any `json:"payload"`
		LinesAtStop int            `json:"linesAtStop"`
	} `json:"stops"`
}

func loadFixture(t *testing.T) stopFixture {
	t.Helper()
	raw, err := os.ReadFile(stopEventsFixture)
	if err != nil {
		t.Fatal(err)
	}
	var document stopFixture
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// distinctThird is the fixture with its third Stop's answer, and the payloads reporting it,
// changed to "DONE.", so each of the three Stops is an event of its own.
func distinctThird(t *testing.T) stopFixture {
	document := loadFixture(t)
	last := document.Stops[4].LinesAtStop - 1
	var line map[string]any
	if err := json.Unmarshal([]byte(document.TranscriptLines[last]), &line); err != nil {
		t.Fatal(err)
	}
	line["payload"].(map[string]any)["item"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "DONE."
	raw, _ := json.Marshal(line)
	document.TranscriptLines[last] = string(raw)
	for _, index := range []int{4, 5} {
		document.Stops[index].Payload["last_assistant_message"] = "DONE."
	}
	return document
}

// host is a temporary Codex home whose settings journal every invocation, and the guard peer
// answering its control.sock.
type host struct {
	t                                                         *testing.T
	root, codex, journal, state, transcript, settings, ledger string
	calls                                                     atomic.Int32
	dieFirst                                                  bool
	first                                                     chan *os.Process
}

func newHost(t *testing.T, decision string) *host {
	t.Helper()
	// Short, because control.sock has to fit a sockaddr_un.
	root, err := os.MkdirTemp("", "se")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	h := &host{t: t, root: root, codex: root + "/codex", journal: root + "/journal", state: root + "/state", transcript: root + "/rollout.jsonl", first: make(chan *os.Process, 1)}
	h.settings = h.codex + "/" + hook.ConfigName
	h.ledger = filepath.Join(append([]string{h.codex}, hook.HostLedgerParts...)...)
	for _, dir := range []string{h.codex, h.state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.writeSettings(h.settings, nil)
	verdict := `{"decision":"release","state":"unmanaged","hook_output":{}}`
	if decision == hook.Block {
		verdict = `{"decision":"block","state":"declared","hook_output":{"decision":"block","reason":"verify the child","continue":true}}`
	}
	listener, err := net.Listen("unix", h.state+"/control.sock")
	if err != nil {
		t.Fatal(err)
	}
	var served sync.WaitGroup
	t.Cleanup(func() { _ = listener.Close(); served.Wait() })
	served.Add(1)
	go func() {
		defer served.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			served.Add(1)
			go func() {
				defer served.Done()
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil || len(bytes.TrimSpace(line)) == 0 {
					return // a duplicate dials and asks nothing
				}
				if h.calls.Add(1) == 1 && h.dieFirst {
					_ = (<-h.first).Kill() // the owner dies before it answers
					return
				}
				_, _ = conn.Write([]byte(verdict + "\n"))
			}()
		}
	}()
	return h
}

func (h *host) writeSettings(path string, change func(map[string]any)) {
	document := map[string]any{"configVersion": 1, "event": "Stop", "relayExecutable": h.root + "/relay", "markerRoot": h.root + "/marker", "dbPath": nil, "mode": "observe", "timeoutSeconds": 5, "journalRoot": h.journal, "journalPolicy": hook.EveryInvocation, "installedBy": "CRW-212", "isolationAssertedBy": nil}
	if change != nil {
		change(document)
	}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// ownRoot is another registration of this host whose settings name their own journal root.
func (h *host) ownRoot(name string) (journal, settings string) {
	journal, settings = h.root+"/"+name, h.root+"/"+name+".json"
	h.writeSettings(settings, func(d map[string]any) { d["journalRoot"] = journal })
	return journal, settings
}

// at writes the transcript as it stood at this Stop and returns the Stop's payload.
func (h *host) at(document stopFixture, index int) []byte {
	stop := document.Stops[index]
	var b strings.Builder
	for _, line := range document.TranscriptLines[:stop.LinesAtStop] {
		b.WriteString(line + "\n")
	}
	if err := os.WriteFile(h.transcript, []byte(b.String()), 0o600); err != nil {
		h.t.Fatal(err)
	}
	payload := map[string]any{}
	for k, v := range stop.Payload {
		payload[k] = v
	}
	payload["transcript_path"], payload["cwd"] = h.transcript, h.root
	raw, _ := json.Marshal(payload)
	return raw
}

// run is one registration answering one Stop, as the host starts it.
func (h *host) run(settings string, payload []byte) string {
	h.t.Helper()
	bin, err := crwBinary()
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(bin, "hook", settings)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.root, "CODEX_HOME=" + h.codex, "XDG_STATE_HOME=" + h.root + "/xdg", "CODEX_SESSION_RELAY_STATE=" + h.state, "TMPDIR=" + os.TempDir(), testsupport.RefuseLiveStateEnv + "=1"}
	cmd.Stdin = bytes.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	select {
	case h.first <- cmd.Process:
	default:
	}
	_ = cmd.Wait()
	return out.String()
}

// twice is one Stop answered by the host's two registrations: one accepts, one is its duplicate.
func (h *host) twice(payload []byte) {
	h.run(h.settings, payload)
	h.run(h.settings, payload)
}

var looseStop = []byte(`{"session_id": "s", "turn_id": "t", "stop_hook_active": false, "last_assistant_message": "done"}`)

var rowName = regexp.MustCompile(`^[0-9a-f]{32}\.json$`)

func (h *host) rowPaths(journal string) []string {
	var found []string
	days, _ := filepath.Glob(journal + "/[0-9]*")
	for _, day := range days {
		entries, _ := os.ReadDir(day)
		for _, entry := range entries {
			if rowName.MatchString(entry.Name()) {
				found = append(found, day+"/"+entry.Name())
			}
		}
	}
	return found
}

// records is one event's records by kind: the host file, the claim, the outcome, and the
// accepted and duplicate rows.
func (h *host) records() map[string]string {
	h.t.Helper()
	found := map[string]string{}
	hosts, _ := filepath.Glob(h.ledger + "/*.json")
	outcomes, _ := filepath.Glob(h.journal + "/accepted/*" + hook.OutcomeSuffix)
	claims, _ := filepath.Glob(h.journal + "/accepted/*.json")
	claims = slices.DeleteFunc(claims, func(p string) bool { return strings.HasSuffix(p, hook.OutcomeSuffix) })
	if len(hosts) != 1 || len(outcomes) != 1 || len(claims) != 1 {
		h.t.Fatalf("not one event's records: hosts %v claims %v outcomes %v", hosts, claims, outcomes)
	}
	found["host"], found["claim"], found["outcome"] = hosts[0], claims[0], outcomes[0]
	for _, p := range h.rowPaths(h.journal) {
		found[readObject(h.t, p)["acceptance"].(string)] = p
	}
	return found
}

// oneEvent is one Stop through both registrations of one root.
func oneEvent(t *testing.T) (*host, map[string]string) {
	h := newHost(t, hook.Release)
	h.twice(h.at(loadFixture(t), 0))
	return h, h.records()
}

func readObject(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// change rewrites a record in its writer's own bytes after edit.
func change(t *testing.T, path string, edit func(hook.Object) hook.Object) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := hook.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, hook.RecordBytes(edit(body.(hook.Object))), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setAt sets (or with drop, removes) the field a path names inside a record.
func setAt(o hook.Object, path []string, value any, drop bool) hook.Object {
	out := hook.Object{}
	found := false
	for _, f := range o {
		if f.Key != path[0] {
			out = append(out, f)
			continue
		}
		found = true
		if len(path) > 1 {
			out = append(out, contract.Field{Key: f.Key, Value: setAt(f.Value.(hook.Object), path[1:], value, drop)})
		} else if !drop {
			out = append(out, contract.Field{Key: f.Key, Value: value})
		}
	}
	if !found && !drop && len(path) == 1 {
		out = append(out, contract.Field{Key: path[0], Value: value})
	}
	return out
}

func set(path string, value any) func(hook.Object) hook.Object {
	return func(o hook.Object) hook.Object { return setAt(o, strings.Split(path, "."), value, false) }
}

func pop(path string) func(hook.Object) hook.Object {
	return func(o hook.Object) hook.Object { return setAt(o, strings.Split(path, "."), nil, true) }
}

func valueAt(t *testing.T, file, path string) any {
	var v any = readObject(t, file)
	for _, part := range strings.Split(path, ".") {
		v = v.(map[string]any)[part]
	}
	return v
}

// verify is the command as an operator runs it: its exit status and the reading it printed.
func verify(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	var out, errs bytes.Buffer
	code := Run(args, &out, &errs)
	var answer map[string]any
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("stop-events %q printed no reading (exit %d): %v\n%s", args, code, err, errs.String())
	}
	return code, answer
}

func roots(journals ...string) []string {
	var args []string
	for _, j := range journals {
		args = append(args, "--journal-root", j)
	}
	return args
}

func expectVerdict(t *testing.T, code int, answer map[string]any, wantCode int, verdict string) {
	t.Helper()
	if code != wantCode || answer["verdict"] != verdict {
		t.Fatalf("reading (%d, %v), want (%d, %s): %v", code, answer["verdict"], wantCode, verdict, answer)
	}
}

func listed(answer map[string]any, name string) []any { return answer[name].([]any) }

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func jsonMarshal(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// evidenceDumps is a record's content in the writer's separators without its key order.
func evidenceDumps(o hook.Object) string { return evidence.Dumps(o, false, false, true) }
