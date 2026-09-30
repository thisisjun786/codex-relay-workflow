package cli_test

import (
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// restamp flips the owner of a stopped store in both halves, schema_meta and takeover.json,
// as the real Python fence's own stamp does (test_fence.py stamp; stampStore).
func restamp(t *testing.T, state, owner string) {
	t.Helper()
	stampStore(t, filepath.Join(state, "relay.sqlite3"), owner, "active", 1, "")
}

// readOnlyFixture is test_fence_readonly.py's populated store with its READ_FORMS, as the
// Python relay's testdata/readonly_matrix.py built it under a directory (the named fixture),
// written under dir.
func readOnlyFixture(t *testing.T, name, dir string) (db string, forms []json.RawMessage) {
	t.Helper()
	var built struct {
		DB    string            `json:"db"`
		Forms []json.RawMessage `json:"forms"`
	}
	output := fixtureTree(t, name, dir)
	if err := json.Unmarshal([]byte(output), &built); err != nil {
		t.Fatal(err)
	}
	return built.DB, built.Forms
}

// execute is one answer of a program: its exit status and output. Safe off the test goroutine.
func execute(program string, argv ...string) (run, error) {
	return executeWith(nil, program, argv...)
}

// executeWith is execute with extra environment entries, which win over the inherited ones.
func executeWith(extra []string, program string, argv ...string) (run, error) {
	command := exec.Command(program, argv...)
	command.Env = append(append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1"), extra...)
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
	// observedInstant is a field a read form stamps with the moment it ran: merge-evidence's
	// observation window and base verification, and the reporting forms' observation.
	observedInstant = regexp.MustCompile(`"(startedAt|finishedAt|baseVerifiedAt|observedAt)": "\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?\+00:00"`)
)

// comparable masks what two runs of a read form legitimately answer differently: the wall-clock
// instant each one observed, and the installation identity of the program. The fields a read
// form stamps with the moment it ran are masked by name, however old the golden, and any other
// instant within a day of now is masked as the moment of this run.
func comparable(command, stdout string) (string, error) {
	now := time.Now()
	masked := observedInstant.ReplaceAllString(stdout, `"$1": "<now>"`)
	masked = isoInstant.ReplaceAllStringFunc(masked, func(quoted string) string {
		instant, err := time.Parse("2006-01-02T15:04:05.999999-07:00", strings.Trim(quoted, `"`))
		if err == nil && instant.Sub(now).Abs() < 24*time.Hour {
			return `"<now>"`
		}
		return quoted
	})
	masked = installation.ReplaceAllString(masked, `"installationId": "<installation>"`)
	// The golden was read from another file with the same rows.
	masked = identityNeutral(masked)
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
		if info.Mode()&os.ModeSymlink != 0 {
			// A link is the name it holds, never the file it names (which may not exist).
			target, err := os.Readlink(filepath.Join(dir, entry.Name()))
			if err != nil {
				return nil, err
			}
			files[entry.Name()] = sha256.Sum256([]byte("symlink:" + target))
			continue
		}
		if !info.Mode().IsRegular() {
			// A directory or a FIFO is its kind: reading a FIFO would wait for a writer.
			files[entry.Name()] = sha256.Sum256([]byte("kind:" + info.Mode().Type().String()))
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

// unauthenticatedGH is a PATH entry whose gh answers as the gh on the host that recorded the
// goldens did: unauthenticated, exit 4, with the help that names gh auth login. The only gh call
// a read form makes (merge-evidence's first read) fails there, and the golden quotes that
// message, so every run meets the same gh rather than its host's, whose message depends on where
// it runs (a GitHub Actions runner's names GH_TOKEN for workflows) and which, authenticated,
// would reach the network.
func unauthenticatedGH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'To get started with GitHub CLI, please run:  gh auth login\\nAlternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token.\\n' >&2\nexit 4\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// Every read form of test_fence_readonly.py's READ_FORMS, through the built binary, answers
// what its golden holds (the Python CLI's answer, at first) for the same copy of one populated
// store, whoever owns it and whatever phase it is in (cli.py:185-205, cutover.md Read-only
// clients). Where Go may not write - another owner, a contended write gate - the Go reads change
// no byte of the store. A Go store's mirror phase is no longer an input (decision 56).
func TestReadOnlyForms_match_python_in_every_ownership_state(t *testing.T) {
	home := tempHome(t)
	recordedGH := unauthenticatedGH(t)
	_, alias := packageBinary(t)
	var built struct {
		DB    string
		Forms []json.RawMessage
	}
	built.DB, built.Forms = readOnlyFixture(t, "readonly-matrix.json", filepath.Join(home, "fixture"))
	// The trees the Python owner left where its read persisted housekeeping, by state and form.
	var left map[string]treeImage
	fixtureJSON(t, "readonly-matrix-python-left.json", &left, [2]string{"<home>", home}, [2]string{"<repo>", repositoryRootPath()})
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
	// The states are independent copies, so they run side by side; inside one state the forms
	// run in order, each on the store the previous one left (the Python owner's, where it
	// persisted housekeeping, from the fixture).
	failures := make([][]string, len(states))
	// Each state's compared answers, by key, checked against the goldens once every state ran.
	answers := make([]map[string]map[string]any, len(states))
	var wait sync.WaitGroup
	for i, state := range states {
		if state.phase == "starting" {
			// A Go store's mirror phase is no longer an input (decision 56); the state keeps
			// its place, since each state's directory, which the goldens name, is lettered
			// by it.
			continue
		}
		dir := filepath.Join(home, "states", string(rune('a'+i)))
		copyStore(t, filepath.Dir(built.DB), dir)
		epoch, _ := strconv.ParseInt(state.epoch, 10, 64)
		stampStore(t, filepath.Join(dir, "relay.sqlite3"), state.owner, state.phase, epoch, state.takeover)
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
		answers[i] = map[string]map[string]any{}
		go func() {
			defer wait.Done()
			fail := func(format string, args ...any) {
				failures[i] = append(failures[i], state.name+": "+fmt.Sprintf(format, args...))
			}
			for index, f := range forms {
				argv := append([]string{"--state", dir, f.command}, f.options...)
				before, err := storeFiles(dir)
				if err != nil {
					fail("%v", err)
					return
				}
				got, err := executeWith([]string{recordedGH}, alias, argv...)
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
				// Keyed by state and form: the states run side by side.
				key := fmt.Sprintf("%s/%02d %s", state.name, index, f.command)
				// The owner Python read the store next, persisting housekeeping the next form
				// reads: where it changed the store, the tree it left is put in place.
				if tree, changed := left[key]; changed && state.owner == "python" {
					if err = pythonLeft(t, dir, tree); err != nil {
						fail("%v", err)
						return
					}
				}
				text, err := comparable(f.command, got.stdout)
				if err != nil {
					fail("%s %v: %v", f.command, f.options, err)
				}
				answers[i][key] = map[string]any{"code": got.code, "stdout": text}
			}
		}()
	}
	wait.Wait()
	for _, state := range failures {
		for _, failure := range state {
			t.Error(failure)
		}
	}
	for _, state := range answers {
		for _, key := range slices.Sorted(maps.Keys(state)) {
			expectJSON(t, key, state[key], home)
		}
	}
}

// pythonLeft puts in dir the tree the Python owner left there (a store it changed), rehomed.
func pythonLeft(t *testing.T, dir string, tree treeImage) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	if err = materializeTree(dir, tree); err != nil {
		return err
	}
	testsupport.Rehome(t, filepath.Join(dir, "relay.sqlite3"))
	return nil
}

// A read-only command never creates, initializes or binds a store (decision 30, cutover.md
// Record, Read-only clients): against an absent S every read form leaves nothing behind, and
// the forms that read the store answer exactly as the Python fence did (the goldens), exit 2
// {"error": "refused", "reason": "store_absent", "detail": "no relay store exists at <D>; a
// read-only command never creates one"}.
func TestReadOnlyForms_never_create_an_absent_store(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	for _, argv := range [][]string{
		{"status"}, {"show", "--event", "absent"}, {"store-identity"}, {"store-challenge", "--read", "absent"},
		{"fault-show"}, {"fault-next"}, {"fault-policy", "--product", "example"}, {"sync-status"},
		{"assignment-find", "--issue", "REL-1"}, {"route-show"}, {"capacity-show"}, {"fault-notifications"},
		{"--socket", filepath.Join(home, "app.sock"), "status"}, {"service", "status"}, {"doctor"},
	} {
		state := filepath.Join(home, "absent-"+strings.ReplaceAll(strings.Join(argv, "-"), "/", "_"))
		full := append([]string{"--state", state}, argv...)
		key := answerKey(t, full)
		got := binaryRun(t, alias, full...)
		if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
			entries, _ := os.ReadDir(state)
			t.Errorf("%v created %s (%v): %s", argv, state, entries, got.stdout)
		}
		if argv[0] == "doctor" || argv[0] == "service" {
			continue
		}
		want := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_absent\",\n  \"detail\": \"no relay store exists at " + state + "/relay.sqlite3; a read-only command never creates one\"\n}\n"
		expectRun(t, key, got.code, got.stdout, append([]string{home}, full...)...)
		if got.code != 2 || got.stdout != want {
			t.Errorf("%v: go exit %d\n%s", argv, got.code, got.stdout)
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

// D is the file every opener opens, Path.resolve()'s, so a D link naming no file, beside no
// mirror and no gate, is an absent store in both runtimes: every read form refuses it as
// store_absent, byte for byte the live fence's answer, and leaves the link alone in S; a writer
// and a service command's start preflight take it for the absent store a writable opener
// creates, through the link (decision 30), answering alike and leaving the same names in S.
func TestReadOnlyForms_take_a_dangling_database_link_for_an_absent_store(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	app := filepath.Join(home, "app.sock")
	names := func(state string) []string {
		entries, err := os.ReadDir(state)
		if err != nil {
			t.Fatal(err)
		}
		var listed []string
		for _, entry := range entries {
			listed = append(listed, entry.Name())
		}
		return listed
	}
	relay := func(runtime, state string, argv ...string) (run, []string) {
		answer := binaryRun(t, alias, argv...)
		return answer, names(state)
	}
	dangling := func(name string) string {
		state := filepath.Join(home, name)
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(state, "nowhere.sqlite3"), filepath.Join(state, "relay.sqlite3")); err != nil {
			t.Fatal(err)
		}
		return state
	}
	for i, argv := range [][]string{
		{"status"}, {"show", "--event", "absent"}, {"store-identity"}, {"store-challenge", "--read", "absent"},
		{"fault-show"}, {"fault-next"}, {"sync-status"}, {"route-show"}, {"--socket", app, "status"},
	} {
		answers := map[string]run{}
		runtimes := []string{"go"}
		var key string
		for _, runtime := range runtimes {
			state := dangling(fmt.Sprintf("%s-read-%d", runtime, i))
			full := append([]string{"--state", state}, argv...)
			if runtime == "go" {
				key = goldenKey(t, "fence "+keyLabel(full...))
			}
			answer, listed := relay(runtime, state, full...)
			answer.stdout = strings.ReplaceAll(answer.stdout, state, "<S>")
			answers[runtime] = answer
			if !slices.Equal(listed, []string{"relay.sqlite3"}) {
				t.Errorf("%s %v on a dangling D link changed S: %v", runtime, argv, listed)
			}
		}
		want := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_absent\",\n  \"detail\": \"no relay store exists at <S>/relay.sqlite3; a read-only command never creates one\"\n}\n"
		got := answers["go"]
		expectRun(t, key, got.code, got.stdout, home)
		if got.code != 2 || got.stdout != want {
			t.Errorf("%v: go exit %d\n%s", argv, got.code, got.stdout)
		}
	}
	for i, argv := range [][]string{{"store-challenge", "--write"}, {"--socket", app, "service", "disable"}} {
		listed := map[string][]string{}
		runtimes := []string{"go"}
		var key string
		for _, runtime := range runtimes {
			state := dangling(fmt.Sprintf("%s-write-%d", runtime, i))
			full := append([]string{"--state", state}, argv...)
			if runtime == "go" {
				key = goldenKey(t, "fence "+keyLabel(full...))
			}
			answer, left := relay(runtime, state, full...)
			if answer.code != 0 {
				t.Errorf("%s %v on a dangling D link: exit %d\n%s", runtime, argv, answer.code, answer.stdout)
			}
			listed[runtime] = left
		}
		// The names a writer leaves in S beside the link it wrote through.
		expectJSON(t, key, listed["go"], home)
	}
}

// A partial store (a write gate or an ownership mirror without D) is refused, never read or
// repaired (decision 30): every form, read or write, answers the refusal the fence's writer
// admission gives the same state (partial store: write-gate.lock without a database for a gate
// alone, validate's missing or unsupported writer protocol beside a mirror), reason
// store_owned_by_other with exit 2 rather than a host error that invites a retry, byte for byte
// the live fence's answer on a twin S, and leaves S and the scope registry exactly as it found
// them: the service commands and the daemon refuse it before any daemon.lock, daemon.json,
// service.json or scope claim. D is the file every opener opens, Path.resolve()'s, so a D link
// naming no file beside a gate is a gate without a database. A takeover.json link naming no
// file reads as no record but is there, so beside no D it is partial too, gate or none, and no
// writer initializes a store over it. A gate that is not a regular file (a FIFO, whose
// read-only open would wait for a writer) is nobody's creation: the start preflight's creator
// probe opens it without waiting and refuses it at once. A daemon without --socket meets
// check_start first, as cli.py main runs it: refused in check_start's words where a mirror holds
// a record, else asked for its --socket (exit 4), alike in both runtimes.
func TestReadOnlyForms_refuse_a_partial_store_as_a_writer_does(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	source := filepath.Join(home, "source")
	pythonCreates(t, source)
	mirror, err := os.ReadFile(filepath.Join(source, "takeover.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(home, "app.sock")
	scopes := os.Getenv("CODEX_SESSION_RELAY_SCOPE_DIR")
	usage := "{\n  \"error\": \"usage\",\n  \"detail\": \"this command needs --socket to reach the host\"\n}\n"
	for _, partial := range []struct {
		name       string
		files      map[string]string
		dangling   bool
		mirrorLink bool
		fifoGate   bool
		detail     string
	}{
		{"gate only", map[string]string{"write-gate.lock": ""}, false, false, false, "partial store: write-gate.lock without a database"},
		{"mirror only", map[string]string{"takeover.json": string(mirror)}, false, false, false, "missing or unsupported writer protocol"},
		{"gate and mirror", map[string]string{"write-gate.lock": "", "takeover.json": string(mirror)}, false, false, false, "missing or unsupported writer protocol"},
		{"gate beside a dangling link", map[string]string{"write-gate.lock": ""}, true, false, false, "partial store: write-gate.lock without a database"},
		{"dangling mirror link", nil, false, true, false, "partial store: write-gate.lock without a database"},
		{"gate beside a dangling mirror link", map[string]string{"write-gate.lock": ""}, false, true, false, "partial store: write-gate.lock without a database"},
		{"fifo gate", nil, false, false, true, "partial store: write-gate.lock without a database"},
	} {
		states := map[string]string{}
		runtimes := []string{"go"}
		for _, runtime := range runtimes {
			state := filepath.Join(home, runtime+"-"+strings.ReplaceAll(partial.name, " ", "-"))
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, content := range partial.files {
				if err := os.WriteFile(filepath.Join(state, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if partial.dangling {
				if err := os.Symlink(filepath.Join(state, "nowhere.sqlite3"), filepath.Join(state, "relay.sqlite3")); err != nil {
					t.Fatal(err)
				}
			}
			if partial.mirrorLink {
				if err := os.Symlink(filepath.Join(state, "nowhere.json"), filepath.Join(state, "takeover.json")); err != nil {
					t.Fatal(err)
				}
			}
			if partial.fifoGate {
				if err := syscall.Mkfifo(filepath.Join(state, "write-gate.lock"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			states[runtime] = state
		}
		refused := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"" + partial.detail + "\"\n}\n"
		for _, argv := range [][]string{
			{"daemon", "--allow-isolated-scope", "--max-ticks", "0"},
			{"status"}, {"show", "--event", "absent"}, {"store-identity"}, {"store-challenge", "--read", "absent"},
			{"fault-show"}, {"fault-next"}, {"sync-status"}, {"route-show"}, {"--socket", app, "status"},
			{"store-challenge", "--write"}, {"--socket", app, "store-challenge", "--write"},
			{"--socket", app, "daemon", "--allow-isolated-scope", "--max-ticks", "0"},
			{"--socket", app, "service", "enable"}, {"--socket", app, "service", "disable"},
			{"--socket", app, "service", "stop"}, {"--socket", app, "service", "declare", "--forget-execution-policy"},
		} {
			key := goldenKey(t, "fence "+keyLabel(append([]string{"--state", states["go"]}, argv...)...))
			answers := map[string]run{}
			for runtime, state := range states {
				before, err := storeFiles(state)
				if err != nil {
					t.Fatal(err)
				}
				answers[runtime] = binaryRun(t, alias, append([]string{"--state", state}, argv...)...)
				if after, err := storeFiles(state); err != nil || !maps.Equal(before, after) {
					t.Errorf("%s %s %v changed S (%v): %v -> %v", runtime, partial.name, argv, err, before, after)
				}
				if claims, err := os.ReadDir(scopes); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s %s %v touched the scope registry: %v %v", runtime, partial.name, argv, claims, err)
				}
			}
			goAnswer := answers["go"]
			goAnswer.stdout = strings.NewReplacer(states["go"], "<S>", alias, "<relay>").Replace(goAnswer.stdout)
			expectRun(t, key, goAnswer.code, goAnswer.stdout, home)
			code, want := 2, refused
			if argv[0] == "daemon" && partial.files["takeover.json"] == "" {
				code, want = 4, usage
			}
			if goAnswer.code != code || goAnswer.stdout != want {
				t.Errorf("%s %v: go exit %d\n%s", partial.name, argv, goAnswer.code, goAnswer.stdout)
			}
		}
	}
}

// A gate another opener holds EX beside no D and no mirror is a first opener still creating the
// store (decision 30), not a partial store, and no store to act on yet either: the start
// preflight of a service command or a socketed daemon waits for it in both runtimes, polling
// within the creation bound (store.CreationWait, ownership.py CREATION_WAIT_SECONDS), then
// judges the store again from the start, before it touches S or the scope registry. A holder
// that keeps the gate past the bound is refused in one wording and exit by both. One that lets
// the gate go with D still absent (a creator that died) leaves a partial store: the waiting
// commands refuse it in the fence writer's words, and so does the next one. The fence's own run
// of these steps (the same waits, timings and answers, checked against the same words) left
// with the Python runtime (todo 44); the Go run is held to those words here.
func TestServiceAndDaemon_wait_for_a_creation_in_progress_as_the_fence_does(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	programs := map[string]string{"go": alias}
	partial := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"partial store: write-gate.lock without a database\"\n}\n"
	expired := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"store creation in progress: write-gate.lock still held after 0.5s; retry\"\n}\n"
	// The bound, lowered to half a second for one command in this process.
	bound := store.CreationWait
	t.Cleanup(func() { store.CreationWait = bound })
	shortened := map[string]func(argv ...string) run{
		"go": func(argv ...string) run {
			store.CreationWait = 500 * time.Millisecond
			defer func() { store.CreationWait = bound }()
			var stdout, stderr bytes.Buffer
			code := cli.ExecuteAs(context.Background(), "codex-session-relay", argv, &stdout, &stderr)
			return run{code, stdout.String(), stderr.String()}
		},
	}
	scopes := filepath.Join(home, "scopes")
	untouched := func(runtime, what, state string, before map[string][32]byte) {
		t.Helper()
		if after, err := storeFiles(state); err != nil || !maps.Equal(before, after) {
			t.Errorf("%s %s changed S (%v): %v -> %v", runtime, what, err, before, after)
		}
		if claims, err := os.ReadDir(scopes); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s %s touched the scope registry: %v %v", runtime, what, claims, err)
		}
	}
	for _, runtime := range []string{"go"} {
		program := programs[runtime]
		state := filepath.Join(home, runtime)
		app := filepath.Join(home, runtime+".sock")
		if err := os.Mkdir(state, 0o700); err != nil {
			t.Fatal(err)
		}
		gate, err := os.OpenFile(filepath.Join(state, "write-gate.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err = syscall.Flock(int(gate.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		before, err := storeFiles(state)
		if err != nil {
			t.Fatal(err)
		}
		forms := [][]string{{"service", "disable"}, {"daemon", "--allow-isolated-scope", "--max-ticks", "0"}}
		// A creator that never finishes: refused once the bound has passed, nothing written.
		for _, argv := range forms {
			started := time.Now()
			answer := shortened[runtime](append([]string{"--state", state, "--socket", app}, argv...)...)
			if waited := time.Since(started); answer.code != 2 || answer.stdout != expired || waited < 500*time.Millisecond {
				t.Errorf("%s %v past the creation bound (after %s): exit %d\n%s%s", runtime, argv, waited, answer.code, answer.stdout, answer.stderr)
			}
			untouched(runtime, fmt.Sprint(argv), state, before)
		}
		// service status and the read-only forms never probe the gate: they answer at once, well
		// inside the default bound, whatever they answer.
		for _, argv := range [][]string{{"service", "status"}, {"status"}} {
			started := time.Now()
			answer, err := execute(program, append([]string{"--state", state, "--socket", app}, argv...)...)
			if err != nil {
				t.Fatal(err)
			}
			if waited := time.Since(started); waited > 10*time.Second {
				t.Errorf("%s %v waited %s beside a held gate: exit %d\n%s", runtime, argv, waited, answer.code, answer.stdout)
			}
			untouched(runtime, fmt.Sprint(argv), state, before)
		}
		// A creator that dies while they wait: the gate it leaves is a partial store.
		type pending struct {
			argv []string
			done chan run
		}
		var waiting []pending
		for _, argv := range forms {
			p := pending{argv, make(chan run, 1)}
			go func() {
				answer, err := execute(program, append([]string{"--state", state, "--socket", app}, p.argv...)...)
				if err != nil {
					answer = run{-1, "", err.Error()}
				}
				p.done <- answer
			}()
			waiting = append(waiting, p)
		}
		time.Sleep(time.Second)
		for _, p := range waiting {
			select {
			case answer := <-p.done:
				t.Errorf("%s %v did not wait for the creation: exit %d\n%s", runtime, p.argv, answer.code, answer.stdout)
				p.done <- answer
			default:
			}
		}
		if err = gate.Close(); err != nil {
			t.Fatal(err)
		}
		for _, p := range waiting {
			select {
			case answer := <-p.done:
				if answer.code != 2 || answer.stdout != partial {
					t.Errorf("%s %v once its creator died: exit %d\n%s%s", runtime, p.argv, answer.code, answer.stdout, answer.stderr)
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("%s %v still waiting after the gate was let go", runtime, p.argv)
			}
		}
		untouched(runtime, "waiting on a creator that died", state, before)
		again, err := execute(program, "--state", state, "--socket", app, "service", "disable")
		if err != nil {
			t.Fatal(err)
		}
		if again.code != 2 || again.stdout != partial {
			t.Errorf("%s service disable on an abandoned gate: exit %d\n%s", runtime, again.code, again.stdout)
		}
		untouched(runtime, "service disable on an abandoned gate", state, before)
	}
}

// The live-state guard refuses only under test isolation (decisions.md 46), and a read it
// refuses is reported, never read as an unreadable store. A Stop whose receipt lives under the
// live state root is answered with the refusal while CRW_REFUSE_LIVE_STATE=1 is exported, and
// with a verdict when it is not, as the product answers it. The store is Go's, so no daemon's
// absence refuses the Stop first: the Go CLI evaluates its own store in-process, and the other
// runtime's it routes to that owner.
func TestGuardEvaluate_reports_the_live_state_refusal(t *testing.T) {
	t.Setenv("CRW_REFUSE_LIVE_STATE", "1")
	// The Stop's markers are filed under a digest of the workspace path: a home at a fixed path
	// keeps the fixture's markers where this run looks for them.
	home := isolateHome(t, fixedTree(t, t.Name()))
	_, alias := packageBinary(t)
	state := filepath.Join(home, ".local", "state", "codex-session-relay", "scope")
	var guard struct{ Root, Stop, Now string }
	built := fixtureTree(t, "guard-live-state.json", home)
	if err := json.Unmarshal([]byte(built), &guard); err != nil {
		t.Fatal(err)
	}
	restamp(t, state, "go")
	argv := []string{"--state", state, "guard-evaluate", "--marker-root", guard.Root, "--stop-input", guard.Stop,
		"--mode", "observe", "--now", guard.Now, "--no-record"}
	refused := binaryRun(t, alias, argv...)
	want := "{\n  \"error\": \"host\",\n  \"detail\": \"store: live state refused under CRW_REFUSE_LIVE_STATE=1 (test isolation)\"\n}\n"
	if refused.code != 3 || refused.stdout != want {
		t.Fatalf("exit %d\n%s", refused.code, refused.stdout)
	}
	t.Setenv("CRW_REFUSE_LIVE_STATE", "")
	allowed := binaryRun(t, alias, argv...)
	if allowed.code != 0 || !strings.Contains(allowed.stdout, `"decision": `) || strings.Contains(allowed.stdout, "receipts") {
		t.Fatalf("exit %d\n%s", allowed.code, allowed.stdout)
	}
}

// packet-check reads the store read-only for the receiver's standing, and a read the live-state
// guard refuses under test isolation is reported as the refusal, never answered as if the store
// could not be opened (cutover.md, The live-state guard). Without the refusal, as the product
// runs, it is the golden's answer (Python's, at first).
func TestPacketCheck_reports_the_live_state_refusal(t *testing.T) {
	t.Setenv("CRW_REFUSE_LIVE_STATE", "1")
	home := tempHome(t)
	_, alias := packageBinary(t)
	var built struct {
		DB    string
		Forms []json.RawMessage
	}
	built.DB, built.Forms = readOnlyFixture(t, "readonly-packet-check.json", filepath.Join(home, "fixture"))
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
	want := "{\n  \"error\": \"host\",\n  \"detail\": \"store: live state refused under CRW_REFUSE_LIVE_STATE=1 (test isolation)\"\n}\n"
	if refused.code != 3 || refused.stdout != want {
		t.Fatalf("exit %d\n%s", refused.code, refused.stdout)
	}
	t.Setenv("CRW_REFUSE_LIVE_STATE", "")
	allowed := binaryRun(t, alias, argv...)
	expectRun(t, "packet-check", allowed.code, allowed.stdout, argv...)
	if allowed.code != 0 {
		t.Fatalf("exit %d\n%s", allowed.code, allowed.stdout)
	}
}

// A write form is never served read-only: under another owner both runtimes refuse it with
// the fence's refused envelope, detail and exit 2 (cli.main answers every RelayError so),
// including the fault commands and the write forms of the conditional read commands, and Go
// changes no byte of the store it refused.
func TestWriteForms_refuse_a_foreign_store_as_python_does(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	pythonOwned := filepath.Join(home, "python-owned")
	pythonCreates(t, pythonOwned)
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
		key := goldenKey(t, "python "+keyLabel(argv...))
		got := binaryRun(t, alias, append([]string{"--state", pythonOwned}, argv...)...)
		// The fence's own words for the other owner (ownership.py validate), byte for byte.
		expectRun(t, key, got.code, got.stdout, append([]string{"--state", pythonOwned}, argv...)...)
		if got.code != 2 || !strings.Contains(got.stdout, `"detail": "the relay store belongs to another runtime"`) {
			t.Errorf("%v: go %d %s", argv, got.code, got.stdout)
		}
		if after, err := storeFiles(pythonOwned); err != nil || !maps.Equal(before, after) {
			t.Errorf("%v changed the foreign store (%v)", argv, err)
		}
	}
}
