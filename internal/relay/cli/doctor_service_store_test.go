package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// writeScopeClaim records socket's scope claim as a relay service started on it registers one:
// the state directory it serves, the socket as it was given, and this process as the holder.
func writeScopeClaim(t *testing.T, socket, state string) string {
	t.Helper()
	scope, err := service.ResolveScope()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(scope.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	raw, err := json.Marshal(map[string]any{
		"pid": pid, "startTicks": service.StartTicks(pid), "bootId": service.BootID(), "storeId": "served-store",
		"stateDir": state, "socketPath": socket, "scopeKey": scope.Key(socket), "scopeAuthority": scope.Authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scope.Root, scope.Key(socket)+".json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The doctor names the store the relay service registered for the selection's socket serves,
// when discovery chose another directory, so a reader that selected the wrong store can correct
// itself (decision 73). Without --socket that socket is the default App Server socket. The key
// is absent wherever it has nothing to say.
func TestDoctorNamesTheServiceStoreDiscoveryDidNotSelect(t *testing.T) {
	home := tempHome(t)
	socket := filepath.Join(home, "codex-home", "app-server-control", "app-server-control.sock")
	served := filepath.Join(home, "served")
	claim := writeScopeClaim(t, socket, served)

	answer := golang(t, home, "doctor")
	if answer.code != 0 {
		t.Fatal(answer)
	}
	report := decode(t, answer.stdout)
	selected := obj(report["stateSelection"])
	if selected["source"] != "xdg" || !strings.Contains(selected["detail"].(string), "; scoped by the default Codex App Server socket "+socket) {
		t.Fatalf("selection %v", selected)
	}
	got := obj(report["serviceStore"])
	if got["stateDirectory"] != served || got["socketPath"] != socket || got["storeId"] != "served-store" || got["live"] != true || got["scopeRecord"] != claim {
		t.Fatalf("serviceStore %v", got)
	}
	recover, _ := got["recover"].([]any)
	if len(recover) != 2 || recover[0] != "codex-session-relay --state="+served+" --socket="+socket+" doctor" {
		t.Fatalf("recover %v", got["recover"])
	}
	// Additive and trailing: every key the report had keeps its place.
	if strings.LastIndex(answer.stdout, `"serviceStore"`) < strings.LastIndex(answer.stdout, `"runtime"`) {
		t.Fatalf("serviceStore is not the trailing key:\n%s", answer.stdout)
	}
	// The same reading through --socket, for the service's own socket.
	bySocket := decode(t, golang(t, home, "--socket", socket, "doctor").stdout)
	if obj(bySocket["serviceStore"])["stateDirectory"] != served {
		t.Fatalf("--socket serviceStore %v", bySocket["serviceStore"])
	}

	absent := func(t *testing.T, argv ...string) {
		t.Helper()
		answer := golang(t, home, argv...)
		if _, present := decode(t, answer.stdout)["serviceStore"]; present || answer.code != 0 {
			t.Fatalf("%v: serviceStore reported: %s", argv, answer.stdout)
		}
	}
	// An explicit selection is not second-guessed.
	absent(t, "--state", filepath.Join(home, "elsewhere"), "doctor")
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "elsewhere"))
	absent(t, "doctor")
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	// Another socket's claim says nothing about this one.
	absent(t, "--socket", filepath.Join(home, "other.sock"), "doctor")
	// The claim names the directory discovery selected: nothing to correct.
	writeScopeClaim(t, socket, selected["path"].(string))
	absent(t, "doctor")
}

// A selection discovery made without --socket was scoped by the default App Server socket, so a
// store in that directory recording another socket is refused as it is under an explicit
// --socket, for a writing and a read-only command alike; one recording the default socket or
// none, and a store in the legacy default directory, are admitted as before (decision 73).
// Nothing connects to the default socket: a command that needs the App Server still needs
// --socket.
func TestANoSocketSelectionIsHeldToTheDefaultSocket(t *testing.T) {
	for _, c := range []struct {
		name    string
		other   bool
		record  bool
		legacy  bool
		refused bool
	}{
		{name: "a store recording another socket is refused", other: true, record: true, refused: true},
		{name: "a store recording the default socket is admitted", record: true},
		{name: "a store recording no socket is admitted"},
		{name: "the legacy default directory is admitted whatever it records", other: true, record: true, legacy: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := tempHome(t)
			withHome(t, home)
			socket := filepath.Join(home, "codex-home", "app-server-control", "app-server-control.sock")
			scope, err := store.SocketScope(socket)
			if err != nil {
				t.Fatal(err)
			}
			if c.legacy {
				scope = store.LegacyDefaultScope
			}
			dir := filepath.Join(home, ".local", "state", "codex-session-relay", scope)
			recorded := ""
			if c.record {
				recorded = socket
			}
			if c.other {
				recorded = filepath.Join(home, "other.sock")
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			testsupport.Create(t, filepath.Join(dir, "relay.sqlite3"), recorded, "go")
			artifacts := filepath.Join(home, "artifacts")
			if err := os.MkdirAll(artifacts, 0o700); err != nil {
				t.Fatal(err)
			}

			register := golang(t, home, "register", "--parent-task", "parent", "--parent-host", "host", "--child-task", "child", "--child-host", "host",
				"--issue", "REL-default", "--artifact-root", artifacts, "--allowed-recipient", "parent", "--dispatch-request-id", "dispatch-default", "--dispatch-turn-id", "turn-default")
			status := golang(t, home, "status")
			for _, answer := range []run{register, status} {
				if !c.refused {
					if answer.code != 0 {
						t.Fatalf("admitted store refused: %+v", answer)
					}
					continue
				}
				refusal := decode(t, answer.stdout)
				wantRecorded, _ := store.CanonicalSocket(recorded)
				wantRequested, _ := store.CanonicalSocket(socket)
				if answer.code != 2 || refusal["reason"] != "state_directory_serves_another_socket" || refusal["recordedSocket"] != wantRecorded ||
					refusal["requestedSocket"] != wantRequested || refusal["stateDirectory"] != dir {
					t.Fatalf("want state_directory_serves_another_socket (exit 2): %+v", answer)
				}
			}
			// What reached the store, read through an explicit --state, which is never checked
			// against the default socket.
			contents := obj(decode(t, golang(t, home, "--state", dir, "doctor").stdout)["contents"])
			want := float64(1)
			if c.refused {
				want = 0
			}
			if contents["relationships"] != want {
				t.Fatalf("relationships %v, want %v", contents["relationships"], want)
			}
			// The default socket is never a --socket: a host command still asks for one (usage,
			// exit 4), after the selection refusal where there is one.
			wantDeliver := 4
			if c.refused {
				wantDeliver = 2
			}
			if deliver := golang(t, home, "deliver"); deliver.code != wantDeliver {
				t.Fatalf("deliver without --socket: %+v", deliver)
			}
		})
	}
}
