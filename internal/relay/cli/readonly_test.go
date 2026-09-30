package cli_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// matrixPython runs testdata/readonly_matrix.py with the real Python relay package and its
// tests importable, never writing bytecode into the tree.
func matrixPython(t *testing.T, args ...string) string {
	t.Helper()
	root := repositoryRoot(t)
	command := exec.Command("uv", append([]string{"run", "--no-sync", "--project", root, "python",
		filepath.Join(root, "internal", "relay", "cli", "testdata", "readonly_matrix.py")}, args...)...)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1",
		"PYTHONPATH="+filepath.Join(root, "packages", "codex-session-relay"))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("readonly_matrix.py %v: %v\n%s", args, err, stderr.String())
	}
	return stdout.String()
}

// restamp flips the owner of a stopped store in both halves, schema_meta and takeover.json,
// with the real Python fence's own identity and encoding (test_fence.py stamp).
func restamp(t *testing.T, state, owner string) {
	t.Helper()
	matrixPython(t, "stamp", filepath.Join(state, "relay.sqlite3"), owner, "active", "1", "")
}

// execute is one answer of a program: its exit status and output. Safe off the test goroutine.
func execute(program string, argv ...string) (run, error) {
	command := exec.Command(program, argv...)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	code := 0
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return run{}, fmt.Errorf("%s %v: %w", program, argv, err)
		}
		code = exit.ExitCode()
	}
	return run{code, stdout.String(), stderr.String()}, nil
}

// binaryRun is one answer of the built crw binary invoked as codex-session-relay.
func binaryRun(t *testing.T, alias string, argv ...string) run {
	t.Helper()
	answer, err := execute(alias, argv...)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

var (
	isoInstant   = regexp.MustCompile(`"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?\+00:00)"`)
	installation = regexp.MustCompile(`"installationId": "[0-9a-f]+"`)
)

// comparable masks what two processes a moment apart legitimately answer differently: the
// wall-clock instant each one observed, and the installation identity of each program.
func comparable(command, stdout string) (string, error) {
	now := time.Now()
	masked := isoInstant.ReplaceAllStringFunc(stdout, func(quoted string) string {
		instant, err := time.Parse("2006-01-02T15:04:05.999999-07:00", strings.Trim(quoted, `"`))
		if err == nil && instant.Sub(now).Abs() < 24*time.Hour {
			return `"<now>"`
		}
		return quoted
	})
	masked = installation.ReplaceAllString(masked, `"installationId": "<installation>"`)
	if command != "doctor" {
		return masked, nil
	}
	// Ownership and access are each runtime's own diagnosis, not domain data
	// (test_fence_readonly.py), and runtime names the answering implementation.
	var report map[string]json.RawMessage
	if err := json.Unmarshal([]byte(masked), &report); err != nil {
		return "", fmt.Errorf("doctor: %w\n%s", err, masked)
	}
	for _, key := range []string{"ownership", "access", "actorReachability", "accessReceipt", "runtime"} {
		delete(report, key)
	}
	encoded, err := json.Marshal(report)
	return string(encoded), err
}

func copyStore(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, entry.Name()), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// storeFiles is every file of S except SQLite's own coordination sidecars, which a mode=ro
// reader may create (cutover.md, Read-only clients), with the digest of its bytes.
func storeFiles(dir string) (map[string][32]byte, error) {
	files := map[string][32]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(entry.Name(), "-shm") || strings.HasSuffix(entry.Name(), "-wal") && info.Size() == 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		files[entry.Name()] = sha256.Sum256(raw)
	}
	return files, nil
}

// Every read form of test_fence_readonly.py's READ_FORMS, through the built binary, answers
// what the real Python CLI answers for the same copy of one populated store, whoever owns it
// and whatever phase it is in (cli.py:185-205, cutover.md Read-only clients). Where Go may
// not write - another owner, draining, starting without a permit, a contended write gate -
// the Go reads change no byte of the store.
func TestReadOnlyForms_match_python_in_every_ownership_state(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	root := repositoryRoot(t)
	var built struct {
		DB    string            `json:"db"`
		Forms []json.RawMessage `json:"forms"`
	}
	if err := json.Unmarshal([]byte(matrixPython(t, "fixture", filepath.Join(home, "fixture"))), &built); err != nil {
		t.Fatal(err)
	}
	type form struct {
		command string
		options []string
	}
	var forms []form
	var names []string
	for _, raw := range built.Forms {
		var pair []json.RawMessage
		var f form
		if json.Unmarshal(raw, &pair) != nil || len(pair) != 2 || json.Unmarshal(pair[0], &f.command) != nil || json.Unmarshal(pair[1], &f.options) != nil {
			t.Fatalf("form %s", raw)
		}
		forms = append(forms, f)
		names = append(names, f.command)
	}
	if len(forms) < 40 || !slices.Contains(names, "store-challenge") || !slices.Contains(names, "fault-next") {
		t.Fatalf("READ_FORMS did not arrive whole: %v", names)
	}
	states := []struct {
		name, owner, phase, epoch, takeover string
		gate                                bool
	}{
		{"python active", "python", "active", "1", "", false},
		{"go active", "go", "active", "1", "", false},
		{"python draining", "python", "draining", "1", "", false},
		{"go starting", "go", "starting", "2", "t-start", false},
		{"go active, write gate held exclusively", "go", "active", "1", "", true},
	}
	// The states are independent copies, so they run side by side; inside one state every
	// form runs through Go and then through Python, so both read the same store even where a
	// writer persists housekeeping between forms.
	failures := make([][]string, len(states))
	var wait sync.WaitGroup
	for i, state := range states {
		dir := filepath.Join(home, "states", string(rune('a'+i)))
		copyStore(t, filepath.Dir(built.DB), dir)
		matrixPython(t, "stamp", filepath.Join(dir, "relay.sqlite3"), state.owner, state.phase, state.epoch, state.takeover)
		if state.gate {
			gate, err := os.OpenFile(filepath.Join(dir, "write-gate.lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			if err := syscall.Flock(int(gate.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
		}
		goWrites := state.owner == "go" && state.phase == "active" && !state.gate
		wait.Add(1)
		go func() {
			defer wait.Done()
			fail := func(format string, args ...any) {
				failures[i] = append(failures[i], state.name+": "+fmt.Sprintf(format, args...))
			}
			for _, f := range forms {
				argv := append([]string{"--state", dir, f.command}, f.options...)
				before, err := storeFiles(dir)
				if err != nil {
					fail("%v", err)
					return
				}
				got, err := execute(alias, argv...)
				if err != nil {
					fail("%v", err)
					return
				}
				// Only the owner persists housekeeping: fault-next expires the lapsed claim on
				// the store Go writes, and projects it (readonly_queue_state) on any other.
				mayWrite := goWrites
				if after, err := storeFiles(dir); err != nil || !mayWrite && !maps.Equal(before, after) {
					fail("%s changed a store it may not write (%v):\nbefore %v\nafter  %v", f.command, err, before, after)
				}
				py, err := execute("uv", append([]string{"run", "--no-sync", "--project", root, "codex-session-relay"}, argv...)...)
				if err != nil {
					fail("%v", err)
					return
				}
				pyText, pyErr := comparable(f.command, py.stdout)
				goText, goErr := comparable(f.command, got.stdout)
				if py.code != got.code || pyErr != nil || goErr != nil || pyText != goText {
					fail("%s %v: exit python=%d go=%d\npython:\n%s\ngo:\n%s", f.command, f.options, py.code, got.code, py.stdout, got.stdout)
				}
			}
		}()
	}
	wait.Wait()
	for _, state := range failures {
		for _, failure := range state {
			t.Error(failure)
		}
	}
}

// A read-only command never creates, initializes or binds a store (decision 30, cutover.md
// Record, Read-only clients): against an absent S every read form leaves nothing behind, and
// the forms that read the store answer exactly as the live Python fence does, exit 2
// {"error": "refused", "reason": "store_absent", "detail": "no relay store exists at <D>; a
// read-only command never creates one"}.
func TestReadOnlyForms_never_create_an_absent_store(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	for _, argv := range [][]string{
		{"status"}, {"show", "--event", "absent"}, {"store-identity"}, {"store-challenge", "--read", "absent"},
		{"fault-show"}, {"fault-next"}, {"fault-policy", "--product", "example"}, {"sync-status"},
		{"assignment-find", "--issue", "REL-1"}, {"route-show"}, {"capacity-show"}, {"fault-notifications"},
		{"--socket", filepath.Join(home, "app.sock"), "status"}, {"service", "status"}, {"doctor"},
	} {
		state := filepath.Join(home, "absent-"+strings.ReplaceAll(strings.Join(argv, "-"), "/", "_"))
		got := binaryRun(t, alias, append([]string{"--state", state}, argv...)...)
		py := python(t, home, append([]string{"--state", state}, argv...)...)
		for runtime, answer := range map[string]run{"go": got, "python": py} {
			if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
				entries, _ := os.ReadDir(state)
				t.Errorf("%s %v created %s (%v): %s", runtime, argv, state, entries, answer.stdout)
			}
		}
		if argv[0] == "doctor" || argv[0] == "service" {
			continue
		}
		want := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_absent\",\n  \"detail\": \"no relay store exists at " + state + "/relay.sqlite3; a read-only command never creates one\"\n}\n"
		if got.code != py.code || got.stdout != py.stdout || got.code != 2 || got.stdout != want {
			t.Errorf("%v: go exit %d\n%s\npython exit %d\n%s", argv, got.code, got.stdout, py.code, py.stdout)
		}
	}
	// A write form still creates the absent store, as owner=go epoch 1 (decision 30).
	state := filepath.Join(home, "absent-write")
	if got := binaryRun(t, alias, "--state", state, "store-challenge", "--write"); got.code != 0 {
		t.Fatalf("write form: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
}

// A partial store (a write gate or an ownership mirror without D) is refused, never read or
// repaired (decision 30): every form, read or write, answers the refusal the fence's writer
// admission gives the same state (partial store: write-gate.lock without a database for a gate
// alone, validate's missing or unsupported writer protocol beside a mirror), reason
// store_owned_by_other with exit 2 rather than a host error that invites a retry, byte for byte
// the live fence's answer on a twin S, and leaves S exactly as it found it.
func TestReadOnlyForms_refuse_a_partial_store_as_a_writer_does(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	source := filepath.Join(home, "source")
	pythonCreates(t, home, source)
	mirror, err := os.ReadFile(filepath.Join(source, "takeover.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(home, "app.sock")
	fenceProgram := filepath.Join(repositoryRoot(t), ".venv", "bin", "codex-session-relay")
	for _, partial := range []struct {
		name   string
		files  map[string]string
		detail string
	}{
		{"gate only", map[string]string{"write-gate.lock": ""}, "partial store: write-gate.lock without a database"},
		{"mirror only", map[string]string{"takeover.json": string(mirror)}, "missing or unsupported writer protocol"},
		{"gate and mirror", map[string]string{"write-gate.lock": "", "takeover.json": string(mirror)}, "missing or unsupported writer protocol"},
	} {
		states := map[string]string{}
		for _, runtime := range []string{"go", "python"} {
			state := filepath.Join(home, runtime+"-"+strings.ReplaceAll(partial.name, " ", "-"))
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, content := range partial.files {
				if err := os.WriteFile(filepath.Join(state, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			states[runtime] = state
		}
		want := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"" + partial.detail + "\"\n}\n"
		for _, argv := range [][]string{
			{"status"}, {"show", "--event", "absent"}, {"store-identity"}, {"store-challenge", "--read", "absent"},
			{"fault-show"}, {"fault-next"}, {"sync-status"}, {"route-show"}, {"--socket", app, "status"},
			{"store-challenge", "--write"}, {"--socket", app, "store-challenge", "--write"},
			{"--socket", app, "daemon", "--allow-isolated-scope"},
		} {
			answers := map[string]run{}
			for runtime, state := range states {
				before, err := storeFiles(state)
				if err != nil {
					t.Fatal(err)
				}
				if runtime == "go" {
					answers[runtime] = binaryRun(t, alias, append([]string{"--state", state}, argv...)...)
				} else {
					answers[runtime] = fence(t, append([]string{"--state", state}, argv...)...)
				}
				// The fence's daemon takes its own daemon.lock in S before its store is refused.
				if runtime == "python" && slices.Contains(argv, "daemon") {
					continue
				}
				if after, err := storeFiles(state); err != nil || !maps.Equal(before, after) {
					t.Errorf("%s %s %v changed S (%v): %v -> %v", runtime, partial.name, argv, err, before, after)
				}
			}
			goAnswer, pyAnswer := answers["go"], answers["python"]
			goAnswer.stdout = strings.NewReplacer(states["go"], "<S>", alias, "<relay>").Replace(goAnswer.stdout)
			pyAnswer.stdout = strings.NewReplacer(states["python"], "<S>", fenceProgram, "<relay>").Replace(pyAnswer.stdout)
			if goAnswer.code != pyAnswer.code || goAnswer.stdout != pyAnswer.stdout || goAnswer.code != 2 || goAnswer.stdout != want {
				t.Errorf("%s %v: go exit %d\n%s\npython exit %d\n%s", partial.name, argv, goAnswer.code, goAnswer.stdout, pyAnswer.code, pyAnswer.stdout)
			}
		}
	}
}

// The live-state guard stays until todo 43 (decisions D4): a read it refuses is reported,
// never read as an unreadable store. A Stop whose receipt lives under the live state root is
// answered with the refusal unless CRW_ALLOW_LIVE_STATE=1 is exported, and with a verdict
// when it is. The store is Go's, so no daemon's absence refuses the Stop first: the Go CLI
// evaluates its own store in-process, and the other runtime's it routes to that owner.
func TestGuardEvaluate_reports_the_live_state_refusal(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	state := filepath.Join(home, ".local", "state", "codex-session-relay", "scope")
	var guard struct{ Root, Stop, Now string }
	if err := json.Unmarshal([]byte(matrixPython(t, "guard", filepath.Join(home, "guard"), filepath.Join(state, "relay.sqlite3"))), &guard); err != nil {
		t.Fatal(err)
	}
	restamp(t, state, "go")
	argv := []string{"--state", state, "guard-evaluate", "--marker-root", guard.Root, "--stop-input", guard.Stop,
		"--mode", "observe", "--now", guard.Now, "--no-record"}
	refused := binaryRun(t, alias, argv...)
	want := "{\n  \"error\": \"host\",\n  \"detail\": \"store: live state requires CRW_ALLOW_LIVE_STATE=1\"\n}\n"
	if refused.code != 3 || refused.stdout != want {
		t.Fatalf("exit %d\n%s", refused.code, refused.stdout)
	}
	t.Setenv("CRW_ALLOW_LIVE_STATE", "1")
	allowed := binaryRun(t, alias, argv...)
	if allowed.code != 0 || !strings.Contains(allowed.stdout, `"decision": `) || strings.Contains(allowed.stdout, "receipts") {
		t.Fatalf("exit %d\n%s", allowed.code, allowed.stdout)
	}
}

// packet-check reads the store read-only for the receiver's standing, and a read the live-state
// guard refuses is reported as the refusal, never answered as if the store could not be opened
// (cutover.md, The live-state guard). With the guard lifted it is Python's answer.
func TestPacketCheck_reports_the_live_state_refusal(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	root := repositoryRoot(t)
	var built struct {
		DB    string            `json:"db"`
		Forms []json.RawMessage `json:"forms"`
	}
	if err := json.Unmarshal([]byte(matrixPython(t, "fixture", filepath.Join(home, "fixture"))), &built); err != nil {
		t.Fatal(err)
	}
	var options []string
	for _, raw := range built.Forms {
		var pair []json.RawMessage
		var command string
		if json.Unmarshal(raw, &pair) == nil && len(pair) == 2 && json.Unmarshal(pair[0], &command) == nil && command == "packet-check" {
			if err := json.Unmarshal(pair[1], &options); err != nil {
				t.Fatal(err)
			}
		}
	}
	packet := ""
	for i := 0; i+1 < len(options); i++ {
		if options[i] == "--packet" {
			packet = options[i+1]
		}
	}
	if packet == "" {
		t.Fatalf("READ_FORMS carries no packet-check packet: %v", options)
	}
	live := filepath.Join(home, "xdg-state", "codex-session-relay", "scope")
	copyStore(t, filepath.Dir(built.DB), live)
	restamp(t, live, "python")
	// The store reading (--receiver), as the packet's recipient; READ_FORMS' own form supplies
	// the record and reads no store.
	argv := []string{"--state", live, "packet-check", "--packet", packet, "--receiver", "01supervisor-task"}
	refused := binaryRun(t, alias, argv...)
	want := "{\n  \"error\": \"host\",\n  \"detail\": \"store: live state requires CRW_ALLOW_LIVE_STATE=1\"\n}\n"
	if refused.code != 3 || refused.stdout != want {
		t.Fatalf("exit %d\n%s", refused.code, refused.stdout)
	}
	t.Setenv("CRW_ALLOW_LIVE_STATE", "1")
	allowed := binaryRun(t, alias, argv...)
	py, err := execute("uv", append([]string{"run", "--no-sync", "--project", root, "codex-session-relay"}, argv...)...)
	if err != nil {
		t.Fatal(err)
	}
	if allowed.code != 0 || py.code != 0 || allowed.stdout != py.stdout {
		t.Fatalf("exit go=%d python=%d\ngo:\n%s\npython:\n%s", allowed.code, py.code, allowed.stdout, py.stdout)
	}
}

// A write form is never served read-only: under another owner both runtimes refuse it with
// the fence's refused envelope, detail and exit 2 (cli.main answers every RelayError so),
// including the fault commands and the write forms of the conditional read commands, and Go
// changes no byte of the store it refused.
func TestWriteForms_refuse_a_foreign_store_as_python_does(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	pythonOwned := filepath.Join(home, "python-owned")
	pythonCreates(t, home, pythonOwned)
	goOwned := filepath.Join(home, "go-owned")
	copyStore(t, pythonOwned, goOwned)
	restamp(t, goOwned, "go")
	for _, argv := range [][]string{
		{"fault-policy", "--product", "example", "--fault-class", "delivery_refused", "--severity", "degraded", "--threshold", "2", "--reason", "r"},
		{"fault-limit", "--product", "example", "--kind", "append_comment", "--max-count", "3", "--window", "60"},
		{"fault-target", "--product", "example", "--workspace", home, "--team", "T"},
		{"store-challenge", "--write"},
	} {
		before, err := storeFiles(pythonOwned)
		if err != nil {
			t.Fatal(err)
		}
		got := binaryRun(t, alias, append([]string{"--state", pythonOwned}, argv...)...)
		py, err := execute("uv", append([]string{"run", "--no-sync", "--project", repositoryRoot(t), "codex-session-relay", "--state", goOwned}, argv...)...)
		if err != nil {
			t.Fatal(err)
		}
		// The fence's own words for the other owner (ownership.py validate), byte for byte.
		if got.code != 2 || py.code != 2 || got.stdout != py.stdout || !strings.Contains(got.stdout, `"detail": "the relay store belongs to another runtime"`) {
			t.Errorf("%v: go %d %s\npython %d %s", argv, got.code, got.stdout, py.code, py.stdout)
		}
		if after, err := storeFiles(pythonOwned); err != nil || !maps.Equal(before, after) {
			t.Errorf("%v changed the foreign store (%v)", argv, err)
		}
	}
}
