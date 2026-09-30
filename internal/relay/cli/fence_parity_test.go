package cli_test

import (
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// fence runs the live Python fence's console script (the worktree's .venv) exactly as given:
// unlike python(), it hands no store over first, so it sees each state as a host would.
func fence(t *testing.T, argv ...string) run {
	t.Helper()
	answer, err := execute(filepath.Join(repositoryRoot(t), ".venv", "bin", "codex-session-relay"), argv...)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// pythonCreates makes the store at state with the fence's absent-store initializer. That takes
// a writer form: a read-only form never creates a store (cutover.md Record).
func pythonCreates(t *testing.T, home, state string) {
	t.Helper()
	if created := python(t, home, "--state", state, "store-challenge", "--write"); created.code != 0 {
		t.Fatalf("python store-challenge --write: %+v", created)
	}
}

func records(t *testing.T, state string) (ownership.Record, ownership.Stamp) {
	t.Helper()
	path := filepath.Join(state, "relay.sqlite3")
	r, err := ownership.ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ownership.SnapshotMeta(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

// Socket binding (cutover.md Record, decision 30): crw-run's documented flow - a socketless
// `--state S register`, then `--state S --socket K deliver` - works on either runtime. Each
// runtime binds its own unbound store at the first socketed writable open, both halves
// agree afterwards, the other runtime admits that binding after a takeover, and every
// answer on the way is the other runtime's answer byte for byte.
func TestSocketBinding_follows_the_fence_on_the_socketless_first_flow(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	app, other := filepath.Join(home, "app.sock"), filepath.Join(home, "other.sock")
	states := map[string]string{"go": filepath.Join(home, "go"), "python": filepath.Join(home, "python")}
	runtimes := map[string]func(argv ...string) run{
		"go":     func(argv ...string) run { return binaryRun(t, alias, argv...) },
		"python": func(argv ...string) run { return fence(t, argv...) },
	}
	// One step on both runtimes, each on its own store: the same exit and, once the state
	// directory and the program are spelled alike, the same stdout.
	step := func(want int, argv ...string) {
		t.Helper()
		answers := map[string]run{}
		for runtime, relay := range runtimes {
			answer := relay(append([]string{"--state", states[runtime]}, argv...)...)
			if answer.code != want {
				t.Fatalf("%s %v: exit %d\n%s%s", runtime, argv, answer.code, answer.stdout, answer.stderr)
			}
			answer.stdout = strings.NewReplacer(states[runtime], "<S>", alias, "<relay>",
				filepath.Join(repositoryRoot(t), ".venv", "bin", "codex-session-relay"), "<relay>").Replace(answer.stdout)
			answers[runtime] = answer
		}
		if argv[len(argv)-1] == "--write" && want == 0 {
			return // a fresh challenge nonce each
		}
		if answers["go"].stdout != answers["python"].stdout {
			t.Fatalf("%v:\ngo:\n%s\npython:\n%s", argv, answers["go"].stdout, answers["python"].stdout)
		}
	}
	unbound := func(bound bool) {
		t.Helper()
		for runtime, state := range states {
			r, s := records(t, state)
			if bound != (r.AppServerSocket != nil) || bound != (s.SocketPath != "") {
				t.Fatalf("%s: bound=%v record %+v socket_path %q", runtime, bound, r, s.SocketPath)
			}
		}
	}
	step(0, "store-challenge", "--write")
	unbound(false)
	step(0, "--socket", app, "status") // a read-only command never binds
	unbound(false)
	step(0, "--socket", app, "store-challenge", "--write")
	unbound(true)
	var scopeKeys []string
	for runtime, state := range states {
		r, s := records(t, state)
		if *r.AppServerSocket != app || s.SocketPath != app || r.ScopeKey == nil || r.Owner != runtime || r.Epoch != 1 || r.Phase != "active" || r.Transition != nil {
			t.Fatalf("%s binding: %+v %+v", runtime, r, s)
		}
		scopeKeys = append(scopeKeys, *r.ScopeKey)
	}
	if scopeKeys[0] != scopeKeys[1] {
		t.Fatalf("the runtimes record different scope keys for one socket: %v", scopeKeys)
	}
	step(0, "store-challenge", "--write")
	step(0, "--socket", app, "store-challenge", "--write")
	// Another socket: both refuse, with the same answer, and change nothing.
	before := map[string]map[string][32]byte{}
	for runtime, state := range states {
		files, err := storeFiles(state)
		if err != nil {
			t.Fatal(err)
		}
		before[runtime] = files
	}
	step(2, "--socket", other, "store-challenge", "--write")
	for runtime, state := range states {
		if files, err := storeFiles(state); err != nil || !maps.Equal(before[runtime], files) {
			t.Fatalf("%s: a refused socket changed the store (%v)", runtime, err)
		}
	}
	// Each runtime admits the other's binding after a completed takeover.
	testsupport.HandOver(t, filepath.Join(states["go"], "relay.sqlite3"), "python")
	testsupport.HandOver(t, filepath.Join(states["python"], "relay.sqlite3"), "go")
	states["go"], states["python"] = states["python"], states["go"]
	step(0, "--socket", app, "store-challenge", "--write")
}

// Neither runtime binds a store the other owns: Go refuses a socketed write on the fence's
// unbound store exactly as the fence refuses one on Go's, and neither changes a byte.
func TestSocketBinding_never_binds_the_other_runtimes_store(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	app := filepath.Join(home, "app.sock")
	pythonOwned, goOwned := filepath.Join(home, "python-owned"), filepath.Join(home, "go-owned")
	if created := fence(t, "--state", pythonOwned, "store-challenge", "--write"); created.code != 0 {
		t.Fatalf("python: %+v", created)
	}
	if created := binaryRun(t, alias, "--state", goOwned, "store-challenge", "--write"); created.code != 0 {
		t.Fatalf("go: %+v", created)
	}
	beforePython, err := storeFiles(pythonOwned)
	if err != nil {
		t.Fatal(err)
	}
	beforeGo, err := storeFiles(goOwned)
	if err != nil {
		t.Fatal(err)
	}
	got := binaryRun(t, alias, "--state", pythonOwned, "--socket", app, "store-challenge", "--write")
	py := fence(t, "--state", goOwned, "--socket", app, "store-challenge", "--write")
	want := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"the relay store belongs to another runtime\"\n}\n"
	if got.code != 2 || py.code != 2 || got.stdout != py.stdout || got.stdout != want {
		t.Fatalf("go exit %d\n%s\npython exit %d\n%s", got.code, got.stdout, py.code, py.stdout)
	}
	if after, err := storeFiles(pythonOwned); err != nil || !maps.Equal(beforePython, after) {
		t.Fatalf("go changed the python store (%v)", err)
	}
	if after, err := storeFiles(goOwned); err != nil || !maps.Equal(beforeGo, after) {
		t.Fatalf("python changed the go store (%v)", err)
	}
}

// A torn binding (socket_path committed, the mirror still unbound) is refused by every opener
// but the one passing that socket, which completes it, in both runtimes alike.
func TestSocketBinding_completes_a_torn_binding_only_for_its_socket(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	app := filepath.Join(home, "app.sock")
	for runtime, relay := range map[string]func(argv ...string) run{
		"go":     func(argv ...string) run { return binaryRun(t, alias, argv...) },
		"python": func(argv ...string) run { return fence(t, argv...) },
	} {
		state := filepath.Join(home, runtime)
		if created := relay("--state", state, "store-challenge", "--write"); created.code != 0 {
			t.Fatalf("%s: %+v", runtime, created)
		}
		tear(t, state, app)
		if refused := relay("--state", state, "store-challenge", "--write"); refused.code != 2 || !strings.Contains(refused.stdout, "scope without socket") {
			t.Fatalf("%s: socketless open of a torn binding: %+v", runtime, refused)
		}
		if done := relay("--state", state, "--socket", app, "store-challenge", "--write"); done.code != 0 {
			t.Fatalf("%s: completing opener: %+v", runtime, done)
		}
		if r, s := records(t, state); r.AppServerSocket == nil || *r.AppServerSocket != app || s.SocketPath != app {
			t.Fatalf("%s: not completed: %+v", runtime, r)
		}
		if again := relay("--state", state, "store-challenge", "--write"); again.code != 0 {
			t.Fatalf("%s: socketless open after completion: %+v", runtime, again)
		}
		// The daemon's start preflight (check_start) takes its socket, so a daemon completes a
		// torn binding too. Each runtime binds its own socket: one socket is one scope.
		state, socket := filepath.Join(home, runtime+"-daemon"), filepath.Join(home, runtime+".sock")
		if created := relay("--state", state, "store-challenge", "--write"); created.code != 0 {
			t.Fatalf("%s: %+v", runtime, created)
		}
		tear(t, state, socket)
		if done := relay("--state", state, "--socket", socket, "daemon", "--max-ticks", "0", "--allow-isolated-scope"); done.code != 0 {
			t.Fatalf("%s: daemon on a torn binding: %+v", runtime, done)
		}
		if r, s := records(t, state); r.AppServerSocket == nil || *r.AppServerSocket != socket || s.SocketPath != socket {
			t.Fatalf("%s: daemon did not complete the binding: %+v", runtime, r)
		}
	}
}

// tear commits schema_meta.socket_path without publishing the mirror: the state a crash
// between a socket binding's commit and its publication leaves.
func tear(t *testing.T, state, socket string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(state, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO schema_meta VALUES ('socket_path', ?)", socket); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}

// The torn state "initial stamp committed, mirror absent" (cutover.md Record): doctor's
// ownership block keeps the six keys and names it "takeover record missing", as the fence's
// ownership.report does, whichever runtime created the store and whichever diagnoses it.
func TestDoctor_reports_a_stamp_without_its_mirror_as_the_fence_does(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	for _, creator := range []string{"python", "go"} {
		state := filepath.Join(home, creator)
		var created run
		if creator == "python" {
			created = fence(t, "--state", state, "store-challenge", "--write")
		} else {
			created = binaryRun(t, alias, "--state", state, "store-challenge", "--write")
		}
		if created.code != 0 {
			t.Fatalf("%s: %+v", creator, created)
		}
		if err := os.Remove(filepath.Join(state, "takeover.json")); err != nil {
			t.Fatal(err)
		}
		got := binaryRun(t, alias, "--state", state, "--json", "doctor")
		py := fence(t, "--state", state, "--json", "doctor")
		goBlock, pyBlock := ownershipBlock(t, got.stdout), ownershipBlock(t, py.stdout)
		if got.code != py.code || goBlock != pyBlock || !strings.Contains(goBlock, `"detail": "takeover record missing"`) || !strings.Contains(goBlock, `"owner": "`+creator+`",`) {
			t.Fatalf("%s-created store, exit go=%d python=%d\ngo:%s\npython:%s", creator, got.code, py.code, goBlock, pyBlock)
		}
		// Writers refuse the torn stamp; nothing repairs it.
		for runtime, answer := range map[string]run{
			"go":     binaryRun(t, alias, "--state", state, "store-challenge", "--write"),
			"python": fence(t, "--state", state, "store-challenge", "--write"),
		} {
			if answer.code != 2 {
				t.Fatalf("%s writer on the torn stamp: %+v", runtime, answer)
			}
		}
		if _, err := os.Stat(filepath.Join(state, "takeover.json")); err == nil {
			t.Fatal("the torn stamp was repaired")
		}
	}
}

// The mirror's scopeKey is the key the validating process's own scope-registry authority gives
// its socket (cutover.md Record, record.go Validate, ownership.py validate). A store each
// runtime bound under one CODEX_SESSION_RELAY_SCOPE_DIR is refused to a writer of either
// runtime under another, with the same answer and nothing changed, before its owner is judged
// (so the other runtime's store too); a reader still reads it, and under the binding authority
// its owner writes it again.
func TestScopeKey_from_another_lock_authority_is_refused_as_the_fence_refuses_it(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	app := filepath.Join(home, "app.sock")
	states := map[string]string{"go": filepath.Join(home, "go"), "python": filepath.Join(home, "python")}
	fenceProgram := filepath.Join(repositoryRoot(t), ".venv", "bin", "codex-session-relay")
	relay := func(runtime, state string, argv ...string) run {
		argv = append([]string{"--state", state}, argv...)
		if runtime == "go" {
			return binaryRun(t, alias, argv...)
		}
		return fence(t, argv...)
	}
	bound := map[string]string{}
	for creator, state := range states {
		if created := relay(creator, state, "--socket", app, "store-challenge", "--write"); created.code != 0 {
			t.Fatalf("%s creates the bound store: %+v", creator, created)
		}
		r, _ := records(t, state)
		if r.ScopeKey == nil || !strings.HasPrefix(*r.ScopeKey, "isolated-") || r.Owner != creator {
			t.Fatalf("%s: %+v", creator, r)
		}
		bound[creator] = *r.ScopeKey
	}
	if bound["go"] != bound["python"] {
		t.Fatalf("the runtimes bound one socket under one authority to different keys: %v", bound)
	}
	scopes := os.Getenv("CODEX_SESSION_RELAY_SCOPE_DIR")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(home, "other-scopes"))
	want := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"scope key disagrees with lock authority\"\n}\n"
	for _, runtime := range []string{"go", "python"} {
		for creator, state := range states {
			for _, argv := range [][]string{{"--socket", app, "store-challenge", "--write"}, {"store-challenge", "--write"}} {
				before, err := storeFiles(state)
				if err != nil {
					t.Fatal(err)
				}
				answer := relay(runtime, state, argv...)
				if answer.code != 2 || answer.stdout != want {
					t.Errorf("%s %v on the %s-owned store: exit %d\n%s", runtime, argv, creator, answer.code, strings.ReplaceAll(answer.stdout, fenceProgram, "<relay>"))
				}
				if after, err := storeFiles(state); err != nil || !maps.Equal(before, after) {
					t.Errorf("%s %v changed the %s-owned store (%v)", runtime, argv, creator, err)
				}
			}
			if read := relay(runtime, state, "store-identity"); read.code != 0 {
				t.Errorf("%s reader of the %s-owned store: %+v", runtime, creator, read)
			}
		}
	}
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", scopes)
	for creator, state := range states {
		if answer := relay(creator, state, "--socket", app, "store-challenge", "--write"); answer.code != 0 {
			t.Errorf("%s under the binding authority: %+v", creator, answer)
		}
	}
}
