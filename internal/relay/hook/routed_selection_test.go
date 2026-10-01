package hook

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

type routedFixture struct {
	OwnerEnv                          map[string]string
	State, Socket, Root, Program, Now string
	Stop                              json.RawMessage
}

// asOwner puts this test process, which serves the Go owner, in the owner's configuration:
// every relay variable the owner has, and none it lacks (presence alone changes recovery lines).
func asOwner(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range []string{"HOME", "CODEX_HOME", "XDG_STATE_HOME", "CODEX_SESSION_RELAY_MARKER_ROOT", "CODEX_SESSION_RELAY_STATE"} {
		if value, ok := env[key]; ok {
			t.Setenv(key, value)
			continue
		}
		previous, had := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, previous)
			}
		})
	}
}

// goOwner serves the Go owner's control.sock handler at state, one connection at a time, and
// counts what it answered.
func goOwner(t *testing.T, ctx context.Context, state string) *atomic.Int32 {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(state, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	served := &atomic.Int32{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			served.Add(1)
			if err = HandleControl(ctx, conn, state); err != nil {
				t.Error(err)
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return served
}

func decoded(t *testing.T, raw []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return value
}

// Test33RoutedSelectionRefusals is PR #185 thread 4127894191 (decision 24): a routed Stop
// carries the selection inputs, socketPath and program, so the owner refuses it as the CLI's
// local fallback refuses it under the owner's configuration. The hook's own environment sees one
// store for its socket and routes the Stop; the owner's sees two. The fixture is
// routed_selection.py prepare's (testdata/fixtures/routed-selection-<name>). The Go hook client
// (RequestGuard, what crw hook sends) asks the Go owner, whose refusal is the golden, which began
// as the Python CLI's local fallback's under the owner's configuration. The directions that
// crossed runtimes (the Python client to the Go owner, the Go client to the retained Python
// owner) ran until todo 44: rollback to Python closed at todo 43 (rollback_allowed=0), and the
// Python runtime leaves in todo 44.
func Test33RoutedSelectionRefusals(t *testing.T) {
	for _, name := range []string{"override", "ambiguous"} {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t, 5)
			_, names := layFixture(t, "routed-selection-"+name, home, "app.sock")
			raw, err := os.ReadFile(filepath.Join(home, "fixture.json"))
			if err != nil {
				t.Fatal(err)
			}
			var f routedFixture
			if err = json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			asOwner(t, f.OwnerEnv)
			served := goOwner(t, ctx, f.State)
			conn, err := net.Dial("unix", filepath.Join(f.State, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			payload, err := decodeObject(f.Stop)
			if err != nil {
				t.Fatal(err)
			}
			answer, err := RequestGuard(ctx, conn, payload, GuardOptions{Root: f.Root, Now: f.Now, Mode: Hold, SocketPath: f.Socket, Program: f.Program})
			if err != nil {
				t.Fatal(err)
			}
			if served.Load() != 1 {
				t.Fatalf("the Stop was not routed to the Go owner: %d", served.Load())
			}
			golden.Check(t, "answer", names.spell([]byte(evidence.DumpsIndent(answer, 2, true, true)+"\n")), golden.Substitute(home, "<ROOT>"), golden.Substitute(testRoot, "<REPO>"))
			if held, _ := filepath.Glob(filepath.Join(f.Root, "*", "*", "hook", "*", "*", "*.json")); len(held) != 0 {
				t.Fatalf("a refusal recorded an observation: %v", held)
			}
		})
	}
}

// Test33OwnerEvaluatesOnlyItsOwnLocations is PR #185 thread 4127894432 for the Go owner, every
// refusal byte for byte the retained Python owner's (control.py owner_paths) until todo 44: a
// request naming another marker root or another store is answered as a host error and nothing
// is written there; the owner's own root and store, under any spelling that is the same file,
// are evaluated with the owner's own paths. The fixture is routed_selection.py paths'.
func Test33OwnerEvaluatesOnlyItsOwnLocations(t *testing.T) {
	home := hookHome(t, 5)
	layFixture(t, "routed-selection-paths", home)
	raw, err := os.ReadFile(filepath.Join(home, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Stop json.RawMessage
		Now  string
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	own, elsewhere, state := filepath.Join(home, "markers"), filepath.Join(home, "elsewhere"), filepath.Join(home, "state")
	ownDB, otherDB := filepath.Join(state, "relay.sqlite3"), filepath.Join(home, "other", "relay.sqlite3")
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", own)
	request := func(root, db string) []byte {
		params, _ := json.Marshal(map[string]any{"markerRoot": root, "stopInput": f.Stop, "mode": "observe", "dbPath": db, "now": f.Now, "noRecord": false, "deadline": "2999-01-01T00:00:00Z"})
		return []byte(`{"protocol":1,"method":"guard-evaluate","params":` + string(params) + "}\n")
	}
	ask := func(frame []byte) []byte {
		conn, err := net.Dial("unix", filepath.Join(state, "control.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err = conn.Write(frame); err != nil {
			t.Fatal(err)
		}
		answer, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		return answer
	}
	snapshot := func() string {
		var listing strings.Builder
		for _, root := range []string{own, elsewhere, filepath.Dir(otherDB)} {
			_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					content, _ := os.ReadFile(path)
					listing.WriteString(path + "\x00" + string(content) + "\x00")
				}
				return nil
			})
		}
		return listing.String()
	}
	rootDetail := "control.sock evaluates Stops only under this owner's marker root " + own + "; the request named "
	storeDetail := "control.sock reads only this owner's store " + ownDB + "; the request named "
	refusals := []struct {
		root, db, named, detail string
	}{
		{elsewhere, ownDB, elsewhere, rootDetail},
		{"markers", ownDB, "markers", rootDetail},
		{own, otherDB, otherDB, storeDetail},
		{own, "state/relay.sqlite3", "state/relay.sqlite3", storeDetail},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := snapshot()
	served := goOwner(t, ctx, state)
	for _, c := range refusals {
		frame := ask(request(c.root, c.db))
		want := evidence.Dumps(Object{{Key: "error", Value: "host"}, {Key: "detail", Value: c.detail + c.named}}, false, false, true) + "\n"
		if string(frame) != want {
			t.Fatalf("Go owner answered %q, want %q", frame, want)
		}
	}
	if after := snapshot(); after != before {
		t.Fatal("a refused request wrote under a marker root or beside a store")
	}
	for i, frame := range [][]byte{ask(request(own, ownDB)), ask(request(own+"/", filepath.Join(home, "link", "relay.sqlite3")))} {
		answer := decoded(t, frame).(map[string]any)
		if answer["state"] != "receipt_missing" || answer["recordedAs"] != "hook/s/t/"+string(rune('0'+i)) {
			t.Fatalf("the owner's own locations: %v", answer)
		}
	}
	if served.Load() != int32(len(refusals)+2) {
		t.Fatal(served.Load())
	}
	if recorded, _ := filepath.Glob(filepath.Join(own, "*", "*", "hook", "s", "t", "*.json")); len(recorded) != 2 {
		t.Fatal(recorded)
	}
}
