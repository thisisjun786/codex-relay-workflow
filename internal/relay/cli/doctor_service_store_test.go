package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
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
