package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The store seat: the management session is registered as the relay's store-scope supervisor,
// the one scope no Linear level owns. These drive the built linkage-bind line, so the argparse
// spec, the handler and the registry are exercised together.
const (
	storeSeatRole = "supervisor"
	storeSeatKind = "store"
	storeSeatKey  = "store"
)

// storeSeat is one test's store and its own command line.
type storeSeat struct {
	t     *testing.T
	state string
}

func newStoreSeat(t *testing.T) *storeSeat {
	t.Helper()
	return &storeSeat{t: t, state: filepath.Join(t.TempDir(), "state")}
}

// run answers one codex-session-relay line: its exit status and stdout, the two things a caller
// sees (usage goes to stderr).
func (s *storeSeat) run(argv ...string) (int, string) {
	s.t.Helper()
	var stdout, stderr bytes.Buffer
	line := append([]string{"--state", s.state}, argv...)
	code := dispatch.Execute(context.Background(), "codex-session-relay", line, &stdout, &stderr)
	return code, stdout.String()
}

// bind is one linkage-bind for the store seat; scopeKind "" leaves the option off.
func (s *storeSeat) bind(task, scopeKind string) (int, string) {
	s.t.Helper()
	argv := []string{"linkage-bind", "--role", storeSeatRole, "--scope", storeSeatKey, "--task", task, "--host", "host"}
	if scopeKind != "" {
		argv = append(argv, "--scope-kind", scopeKind)
	}
	return s.run(argv...)
}

// mustBind is bind with a successful answer decoded.
func (s *storeSeat) mustBind(task, scopeKind string) map[string]any {
	s.t.Helper()
	code, out := s.bind(task, scopeKind)
	if code != 0 {
		s.t.Fatalf("bind %s --scope-kind %q: exit %d stdout %q", task, scopeKind, code, out)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		s.t.Fatalf("binding answer is not JSON: %v (%q)", err, out)
	}
	return answer
}

// refusedReason is the reason a refused line printed, or "" when it did not refuse.
func (s *storeSeat) refusedReason(out string) string {
	s.t.Helper()
	var answer map[string]any
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return ""
	}
	reason, _ := answer["reason"].(string)
	return reason
}

// mustRefuse is a line that has to be refused with reason, and nothing else.
func (s *storeSeat) mustRefuse(reason string, argv ...string) {
	s.t.Helper()
	code, out := s.run(argv...)
	if code != 2 {
		s.t.Fatalf("%v: exit %d stdout %q, want exit 2", argv, code, out)
	}
	if got := s.refusedReason(out); got != reason {
		s.t.Fatalf("%v: refused %q, want %q (%q)", argv, got, reason, out)
	}
}

// TestStoreSeatBindsTheStoreScopeToTheSupervisor: --scope-kind store claims the store scope for
// the supervisor, one key, under the identity bindingID("supervisor","store","store",task).
func TestStoreSeatBindsTheStoreScopeToTheSupervisor(t *testing.T) {
	s := newStoreSeat(t)
	answer := s.mustBind("01management", storeSeatKind)
	if answer["role"] != storeSeatRole || answer["scopeKind"] != storeSeatKind || answer["scopeKey"] != storeSeatKey {
		t.Fatalf("binding %v", answer)
	}
	sum := sha256.Sum256([]byte(storeSeatRole + "|" + storeSeatKind + "|" + storeSeatKey + "|01management"))
	if want := "bnd-" + hex.EncodeToString(sum[:])[:32]; answer["bindingId"] != want {
		t.Fatalf("bindingId %v, want %s", answer["bindingId"], want)
	}
	// The same claim again converges on the same row instead of opening a second owner.
	if again := s.mustBind("01management", storeSeatKind); again["bindingId"] != answer["bindingId"] {
		t.Fatalf("re-bind gave %v, want %v", again["bindingId"], answer["bindingId"])
	}
}

// TestStoreSeatRefusesASecondOwner: one store scope, one live supervisor.
func TestStoreSeatRefusesASecondOwner(t *testing.T) {
	s := newStoreSeat(t)
	s.mustBind("01management", storeSeatKind)
	s.mustRefuse("duplicate_scope_owner",
		"linkage-bind", "--role", storeSeatRole, "--scope-kind", storeSeatKind, "--scope", storeSeatKey, "--task", "01other", "--host", "host")
}

// TestStoreSeatRefusesATaskThatAlreadyHoldsASupervisorSeat: one task holds one supervisor seat,
// in either order (I-151).
func TestStoreSeatRefusesATaskThatAlreadyHoldsASupervisorSeat(t *testing.T) {
	initiative := []string{"linkage-bind", "--role", storeSeatRole, "--scope", "INIT-1", "--task", "01management", "--host", "host"}

	alreadySupervises := newStoreSeat(t)
	if code, out := alreadySupervises.run(initiative...); code != 0 {
		t.Fatalf("initiative seat: exit %d stdout %q", code, out)
	}
	alreadySupervises.mustRefuse("role_already_bound",
		"linkage-bind", "--role", storeSeatRole, "--scope-kind", storeSeatKind, "--scope", storeSeatKey, "--task", "01management", "--host", "host")

	alreadySeated := newStoreSeat(t)
	alreadySeated.mustBind("01management", storeSeatKind)
	alreadySeated.mustRefuse("role_already_bound", initiative...)
}

// TestStoreSeatRefusesTheStoreKindForAnotherRoleOrKey: the store seat is the supervisor's alone
// and its key is "store"; anything else is a named refusal, not a second scope.
func TestStoreSeatRefusesTheStoreKindForAnotherRoleOrKey(t *testing.T) {
	s := newStoreSeat(t)
	for _, argv := range [][]string{
		{"linkage-bind", "--role", "parent", "--scope-kind", storeSeatKind, "--scope", storeSeatKey, "--task", "01p", "--host", "host"},
		{"linkage-bind", "--role", "child", "--scope-kind", storeSeatKind, "--scope", storeSeatKey, "--task", "01c", "--host", "host"},
		{"linkage-bind", "--role", storeSeatRole, "--scope-kind", storeSeatKind, "--scope", "INIT-1", "--task", "01s", "--host", "host"},
	} {
		s.mustRefuse("scope_role_mismatch", argv...)
	}
}

// TestStoreSeatRefusesASeatWhoseKeyCollidesWithAnotherLevel: an initiative whose key happens to
// be "store" is a different scope from the store seat, and the one-supervisor-seat rule counts
// it: the second claim is refused whichever order the two are made in.
func TestStoreSeatRefusesASeatWhoseKeyCollidesWithAnotherLevel(t *testing.T) {
	initiativeStore := []string{"linkage-bind", "--role", storeSeatRole, "--scope", storeSeatKey, "--task", "01management", "--host", "host"}
	storeSeat := []string{"linkage-bind", "--role", storeSeatRole, "--scope-kind", storeSeatKind, "--scope", storeSeatKey, "--task", "01management", "--host", "host"}

	byInitiative := newStoreSeat(t)
	if code, out := byInitiative.run(initiativeStore...); code != 0 {
		t.Fatalf("initiative named %q: exit %d stdout %q", storeSeatKey, code, out)
	}
	byInitiative.mustRefuse("role_already_bound", storeSeat...)

	bySeat := newStoreSeat(t)
	bySeat.mustBind("01management", storeSeatKind)
	bySeat.mustRefuse("role_already_bound", initiativeStore...)
}

// TestStoreSeatLeavesTheRoleOwnLevelAlone: without --scope-kind every role binds the level it
// always did.
func TestStoreSeatLeavesTheRoleOwnLevelAlone(t *testing.T) {
	s := newStoreSeat(t)
	for _, c := range []struct{ role, scope, kind string }{
		{"supervisor", "INIT-1", "initiative"},
		{"parent", "PROJ-1", "project"},
		{"child", "ISS-1", "issue"},
	} {
		code, out := s.run("linkage-bind", "--role", c.role, "--scope", c.scope, "--task", "01"+c.role, "--host", "host")
		if code != 0 {
			t.Fatalf("%s: exit %d stdout %q", c.role, code, out)
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(out), &answer); err != nil {
			t.Fatal(err)
		}
		if answer["scopeKind"] != c.kind {
			t.Fatalf("%s bound scopeKind %v, want %s", c.role, answer["scopeKind"], c.kind)
		}
	}
}

// TestStoreSeatMakesNoExecutionEdge: the seat is not an initiative execution binding, so it
// writes no edge to a project.
func TestStoreSeatMakesNoExecutionEdge(t *testing.T) {
	s := newStoreSeat(t)
	s.mustBind("01management", storeSeatKind)
	held, err := store.Open(context.Background(), filepath.Join(s.state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	rows, err := held.All(context.Background(), "SELECT link_id FROM scope_links")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the store seat wrote %d scope_links rows", len(rows))
	}
}
