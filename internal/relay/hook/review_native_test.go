package hook

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// review is one run of review_parity.py's group against the native hook: the native hook runs
// through the group's scenario and its steps, in order (each pair's stdout and snapshot, each CLI
// outcome, each lone snapshot), are the group's golden, which began as the steps of the script's
// Python side (review_parity.py python).
type review struct {
	t     *testing.T
	base  string
	steps []any
}

// reviewPython runs review_parity.py group id.
func reviewPython(t *testing.T, id string) {
	t.Helper()
	base := hookHome(t, 5)
	r := &review{t: t, base: base}
	switch id {
	case "D1", "D2":
		r.paths(id)
	case "D3":
		r.constants()
	case "D4":
		r.budgets()
	case "D5":
		r.dialErrors()
	case "D7":
		for _, mask := range []int{0o002, 0o022} {
			h := r.setup(strconv.Itoa(mask))
			r.pair(h, h.payload, nil, mask, true, true)
			t.Logf("%o directory and file modes equal", mask)
		}
	case "D9":
		h := r.setup("large")
		h.payload = append(h.payload, Field{Key: "padding", Value: strings.Repeat("x", 5<<20)})
		stdin := filepath.Join(h.home, "prefilled-stdin.json")
		writeTest(t, stdin, []byte(evidence.Dumps(h.payload, false, false, true)))
		r.pairInput(h, prefilledStdin(stdin), nil, 0o022, false, true)
		t.Log("5 MiB prefilled stdin payload equal")
	case "D10":
		r.utf8()
	default:
		t.Fatal(id)
	}
	// A snapshot that lists directories names the journal's day directory, the run's date: the
	// golden spells it journal/<DAY>, so it holds on any day.
	steps := journalDay.ReplaceAllString(evidence.DumpsIndent(r.steps, 2, true, true), "journal/<DAY>")
	golden.Check(t, id, []byte(steps+"\n"), golden.Substitute(base, "<BASE>"), golden.Substitute(testRoot, "<REPO>"))
}

// reviewHome is review_parity.py setup(name): a home, its settings, a transcript and the Stop.
type reviewHome struct {
	home    string
	cfg     Object
	payload Object
	env     map[string]string
}

func (r *review) setup(name string) reviewHome {
	t := r.t
	home := filepath.Join(r.base, name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(home, "relay"), []byte("#!/bin/sh\nexit 0\n"))
	cfg := Object{{Key: "configVersion", Value: int64(1)}, {Key: "mode", Value: "observe"}, {Key: "relayExecutable", Value: filepath.Join(home, "relay")},
		{Key: "markerRoot", Value: filepath.Join(home, "markers")}, {Key: "dbPath", Value: filepath.Join(home, "state/relay.sqlite3")},
		{Key: "timeoutSeconds", Value: int64(5)}, {Key: "journalRoot", Value: filepath.Join(home, "journal")}}
	writeTest(t, filepath.Join(home, ConfigName), []byte(evidence.Dumps(cfg, false, false, true)))
	transcript := filepath.Join(home, "transcript.jsonl")
	writeTest(t, transcript, []byte(`{"type": "event_msg", "payload": {"type": "task_started", "turn_id": "t"}}`+"\n"+
		`{"type": "event_msg", "payload": {"type": "item_completed", "turn_id": "t", "thread_id": "s", "item": {"type": "AgentMessage", "id": "i", "content": [{"type": "Text", "text": "DONE"}]}}}`+"\n"))
	payload := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false},
		{Key: "last_assistant_message", Value: "DONE"}, {Key: "transcript_path", Value: transcript}}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		env[key] = value
	}
	for key, value := range map[string]string{"HOME": home, "CODEX_HOME": home, "XDG_STATE_HOME": filepath.Join(home, "xdg"),
		"CODEX_SESSION_RELAY_STATE": filepath.Join(home, "state"), "CRW_COMPLETION_HOOK_CONFIG": ""} {
		env[key] = value
	}
	return reviewHome{home: home, cfg: cfg, payload: payload, env: env}
}

func (h reviewHome) environ() []string {
	out := make([]string, 0, len(h.env))
	for key, value := range h.env {
		out = append(out, key+"="+value)
	}
	return out
}

func (h reviewHome) writeSettings(t *testing.T) {
	writeTest(t, filepath.Join(h.home, ConfigName), []byte(evidence.Dumps(h.cfg, false, false, true)))
}

// prefilledStdin is a Stop read from a file whose bytes and EOF exist before the hook starts.
type prefilledStdin string

// invoke is the native half of review_parity.py invoke(): the hook in home under umask mask,
// which must exit 0 with nothing on stderr; it answers stdout.
func (r *review) invoke(h reviewHome, payload any, args []string, mask int) string {
	t := r.t
	t.Helper()
	command := exec.Command("/bin/sh", append([]string{"-c", fmt.Sprintf("umask %03o && exec \"$0\" \"$@\"", mask), binary(t), "hook"}, args...)...)
	command.Dir = h.home
	command.Env = h.environ()
	switch p := payload.(type) {
	case prefilledStdin:
		stdin, err := os.Open(string(p))
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		command.Stdin = stdin
	case Object:
		command.Stdin = strings.NewReader(evidence.Dumps(p, false, false, true))
	default:
		t.Fatalf("payload %T", payload)
	}
	o := runOutcome(t, command)
	if o.Code != 0 || o.Stderr != "" {
		t.Fatalf("hook %q: %+v", args, o)
	}
	return o.Stdout
}

// peer is review_parity.py peer(): a control.sock owner at path answering every guard request
// with response (nil: reading until the hook disconnects) until the returned stop is called.
func (r *review) peer(path string, response Object) (stop func()) {
	t := r.t
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				done <- nil
				return
			}
			if err = serveReviewPeer(conn, response); err != nil {
				done <- err
				return
			}
		}
	}()
	return func() {
		_ = listener.Close()
		_ = os.Remove(path)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("peer completion not signalled")
		}
	}
}

func serveReviewPeer(conn net.Conn, response Object) error {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if len(line) == 0 {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if _, err = decodeObject(line); err != nil {
		return err
	}
	if response == nil {
		rest, err := io.ReadAll(reader)
		if err != nil || len(rest) != 0 {
			return fmt.Errorf("after the request: %q %v", rest, err)
		}
		return nil
	}
	_, err = io.WriteString(conn, evidence.Dumps(response, false, false, true)+"\n")
	return err
}

var reviewRelease = Object{{Key: "decision", Value: "release"}, {Key: "state", Value: "unmanaged"}, {Key: "hook_output", Value: Object{}}}

// snapshot is review_parity.py snapshot(): the journal and claim files under home, without the
// process id and times, a journal row's name spelled <ROW> and the row itself named "row";
// with directories, each directory's mode too.
func (r *review) snapshot(home string, directories bool) Object {
	t := r.t
	t.Helper()
	var normalize func(any) any
	normalize = func(v any) any {
		switch value := v.(type) {
		case Object:
			out := Object{}
			for _, field := range value {
				if !slices.Contains([]string{"at", "claimedAt", "elapsedMs", "guardElapsedMs", "identityScanMs", "pid"}, field.Key) {
					out = append(out, Field{Key: field.Key, Value: normalize(field.Value)})
				}
			}
			return out
		case []any:
			out := make([]any, len(value))
			for i, item := range value {
				out[i] = normalize(item)
			}
			return out
		case string:
			return journalRowName.ReplaceAllString(value, "<ROW>")
		}
		return v
	}
	records := Object{}
	for _, root := range []string{filepath.Join(home, "journal"), filepath.Join(home, "crw-completion-hook")} {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(home, path)
			if d.IsDir() {
				if directories {
					records = append(records, Field{Key: rel, Value: int64(info.Mode().Perm())})
				}
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			value, err := Decode(raw)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if _, err := strconv.Atoi(filepath.Base(filepath.Dir(path))); err == nil {
				rel = "row"
			}
			records = append(records, Field{Key: rel, Value: Object{{Key: "value", Value: normalize(value)}, {Key: "mode", Value: int64(info.Mode().Perm())}}})
			return nil
		})
	}
	return records
}

// journalDay is the journal's day directory (YYYYMMDD of the run), which a snapshot that lists
// directories names; it is the run's date, not anything the hook decides.
var journalDay = regexp.MustCompile(`journal/[0-9]{8}`)

func clearReview(t *testing.T, home string) {
	for _, name := range []string{"journal", "crw-completion-hook"} {
		if err := os.RemoveAll(filepath.Join(home, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// pair is the native half of review_parity.py pair(): the hook answering through a peer, its
// stdout and snapshot a step; it answers the snapshot.
func (r *review) pair(h reviewHome, payload Object, args []string, mask int, directories, withPeer bool) Object {
	r.t.Helper()
	return r.pairInput(h, payload, args, mask, directories, withPeer)
}

func (r *review) pairInput(h reviewHome, payload any, args []string, mask int, directories, withPeer bool) Object {
	t := r.t
	t.Helper()
	var stdout string
	if withPeer {
		stop := r.peer(filepath.Join(filepath.Dir(text(get(h.cfg, "dbPath"))), "control.sock"), reviewRelease)
		stdout = r.invoke(h, payload, args, mask)
		stop()
	} else {
		stdout = r.invoke(h, payload, args, mask)
	}
	got := r.snapshot(h.home, directories)
	r.steps = append(r.steps, Object{{Key: "kind", Value: "pair"}, {Key: "stdout", Value: stdout}, {Key: "snapshot", Value: got}})
	return got
}

// cli is the native relay CLI's outcome for args and input, a step.
func (r *review) cli(h reviewHome, args []string, input string) outcomeBytes {
	t := r.t
	t.Helper()
	command := exec.Command(binary(t), append([]string{"relay"}, args...)...)
	command.Env = h.environ()
	command.Stdin = strings.NewReader(input)
	got := runOutcome(t, command)
	r.steps = append(r.steps, Object{{Key: "kind", Value: "cli"}, {Key: "code", Value: int64(got.Code)}, {Key: "stdout", Value: got.Stdout}, {Key: "stderr", Value: got.Stderr}})
	return got
}

func (r *review) paths(group string) {
	t := r.t
	cases := []string{"env", "argv_over_env", "empty_env", "empty_arg"}
	if group == "D2" {
		cases = []string{"relative_arg", "relative_home", "lexical", "symlink"}
	}
	// The settings argument and CRW_COMPLETION_HOOK_CONFIG are retired (decision 66): the
	// cases that read settings through either are skipped.
	retired := map[string]bool{"env": true, "argv_over_env": true, "empty_arg": true, "relative_arg": true, "lexical": true, "symlink": true}
	for _, name := range cases {
		if retired[name] {
			t.Log(name, "retired")
			continue
		}
		h := r.setup(name)
		alt := filepath.Join(h.home, "alt")
		if err := os.Mkdir(alt, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(alt, "cfg.json")
		writeTest(t, path, []byte(evidence.Dumps(h.cfg, false, false, true)))
		var args []string
		if name == "env" || name == "argv_over_env" || name == "empty_arg" {
			h.env["CRW_COMPLETION_HOOK_CONFIG"] = path
			if err := os.Remove(filepath.Join(h.home, ConfigName)); err != nil {
				t.Fatal(err)
			}
		}
		switch name {
		case "argv_over_env":
			h.env["CRW_COMPLETION_HOOK_CONFIG"] = filepath.Join(h.home, "missing")
			args = []string{path}
		case "empty_arg":
			args = []string{""}
		case "relative_arg":
			args = []string{"alt/cfg.json"}
		case "relative_home":
			h.env["CODEX_HOME"] = "."
		case "lexical":
			args = []string{h.home + "/alt/../alt/./cfg.json"}
		case "symlink":
			if err := os.Symlink(path, filepath.Join(h.home, "link.json")); err != nil {
				t.Fatal(err)
			}
			args = []string{"link.json"}
		}
		r.pair(h, h.payload, args, 0o022, false, true)
		if group == "D2" {
			// With no peer the native pre-scan row is the one the reader exempts.
			clearReview(t, h.home)
			r.invoke(h, h.payload, args, 0o022)
			rows, err := filepath.Glob(filepath.Join(h.home, "journal", "*", "*.json"))
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			raw, err := os.ReadFile(rows[0])
			if err != nil {
				t.Fatal(err)
			}
			row, err := decodeObject(raw)
			if err != nil || !NativePrescanUnreachable(row) {
				t.Fatalf("%s: %v %s", name, err, raw)
			}
		}
		t.Log(name, "equal")
	}
}

func (r *review) constants() {
	t := r.t
	for _, c := range []struct {
		name  string
		value float64
	}{{"nan", math.NaN()}, {"positive", math.Inf(1)}, {"negative", math.Inf(-1)}} {
		h := r.setup(c.name)
		h.cfg = append(h.cfg, Field{Key: "extra", Value: []any{c.value, "NaN", Object{{Key: "Infinity", Value: c.value}}}})
		h.writeSettings(t)
		transcript := text(get(h.payload, "transcript_path"))
		raw, err := os.ReadFile(transcript)
		if err != nil {
			t.Fatal(err)
		}
		writeTest(t, transcript, []byte(strings.ReplaceAll(string(raw), `"payload": {`, `"extra": `+evidence.Dumps(c.value, false, false, true)+`, "payload": {`)))
		h.payload = append(h.payload, Field{Key: "extra", Value: c.value})
		result := r.pair(h, h.payload, nil, 0o022, false, true)
		if get(object(get(object(get(object(get(result, "row")), "value")), "eventIdentity")), "established") != true {
			t.Fatalf("%s: %s", c.name, evidence.Dumps(result, false, true, true))
		}
		for _, field := range []string{"session_id", "turn_id", "stop_hook_active"} {
			clearReview(t, h.home)
			altered := set(append(Object{}, h.payload...), field, c.value)
			result = r.pair(h, altered, nil, 0o022, false, true)
			if get(object(get(object(get(result, "row")), "value")), "adapterOutcome") != "guard_answered" {
				t.Fatalf("%s %s: %s", c.name, field, evidence.Dumps(result, false, true, true))
			}
		}
		// The same loader on guard stdin: full CLI bytes, including echoed constants.
		args := []string{"guard-evaluate", "--marker-root", filepath.Join(h.home, "markers"), "--now", "2026-01-01T00:00:00Z", "--no-record"}
		stop := Object{{Key: "session_id", Value: c.value}, {Key: "turn_id", Value: c.value}, {Key: "extra", Value: []any{c.value}}}
		r.cli(h, args, evidence.Dumps(stop, false, false, true))
		t.Log(c.name, "all loaders equal")
	}
}

func (r *review) budgets() {
	t := r.t
	for _, c := range []struct {
		name   string
		budget any
	}{{"7", int64(7)}, {"7.01", 7.01}, {"8", int64(8)}, {"8.99", 8.99}, {"9", int64(9)}} {
		h := r.setup(c.name)
		h.cfg = set(h.cfg, "owner", "plugin")
		h.cfg = set(h.cfg, "adapterEntryPoint", filepath.Join(h.home, "adapter"))
		h.cfg = set(h.cfg, "adapterInterpreter", filepath.Join(testRoot, ".venv", "bin", "python"))
		h.cfg = set(h.cfg, "timeoutSeconds", c.budget)
		h.writeSettings(t)
		result := r.pair(h, h.payload, nil, 0o022, false, true)
		if (len(result) > 0) != (c.name == "7") {
			t.Fatalf("budget %s: %s", c.name, evidence.Dumps(result, false, true, true))
		}
		t.Log(c.name, "equal")
	}
}

func (r *review) dialErrors() {
	t := r.t
	for _, number := range []syscall.Errno{syscall.EINVAL, syscall.ELOOP} {
		h := r.setup(strconv.Itoa(int(number)))
		state := filepath.Join(h.home, "state")
		if number == syscall.EINVAL {
			state = filepath.Join(h.home, strings.Repeat("x", 130))
		}
		if err := os.Mkdir(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if number == syscall.ELOOP {
			if err := os.Symlink("control.sock", filepath.Join(state, "control.sock")); err != nil {
				t.Fatal(err)
			}
		}
		h.cfg = set(h.cfg, "dbPath", filepath.Join(state, "relay.sqlite3"))
		h.writeSettings(t)
		r.invoke(h, h.payload, nil, 0o022)
		r.steps = append(r.steps, Object{{Key: "kind", Value: "snapshot"}, {Key: "snapshot", Value: r.snapshot(h.home, false)}})
	}
}

func (r *review) utf8() {
	t := r.t
	h := r.setup("utf8")
	args := []string{"guard-evaluate", "--marker-root", text(get(h.cfg, "markerRoot"))}
	h.env["PYTHONIOENCODING"] = "utf-8:strict"
	for _, raw := range []string{`{"a":"` + "\xff" + `"}`, `{"a":"` + "\xe2\x82" + `"}`, "\x80"} {
		if got := r.cli(h, args, raw); got.Code != 3 {
			t.Fatalf("%q: %+v", raw, got)
		}
	}
	// The settings environment never affects guard-evaluate's own inputs.
	h.env["CRW_COMPLETION_HOOK_CONFIG"] = filepath.Join(h.home, "missing")
	command := exec.Command(binary(t), append([]string{"relay"}, append(args, "--now", "2026-01-01T00:00:00Z")...)...)
	command.Env = h.environ()
	command.Stdin = strings.NewReader("{}")
	o := runOutcome(t, command)
	answer, err := decodeObject([]byte(o.Stdout))
	if o.Code != 0 || err != nil || get(answer, "state") != "unmanaged" {
		t.Fatalf("%+v %v", o, err)
	}
	t.Log("stdin host errors equal; guard ignores hook settings")
}
