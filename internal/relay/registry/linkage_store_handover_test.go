package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The store seat's handover (CRW-792). The seat CRW-450 lets the supervisor bind is the one
// scope no Linear level owns, and until now linkage-handover derived its scope kind from the role
// alone, so it looked for an initiative named "store" and refused unregistered_scope. These drive
// the built linkage-handover line, so the argparse spec, the handler and the registry are exercised
// together, and reuse the harness in linkage_store_test.go.

// seatLine answers one codex-session-relay line with its exit status, stdout AND stderr: a parser
// refusal writes its usage text to stderr (exit 2), a handler refusal answers JSON on stdout, and
// the two must not be confused.
func (s *storeSeat) seatLine(argv ...string) (int, string, string) {
	s.t.Helper()
	var stdout, stderr bytes.Buffer
	line := append([]string{"--state", s.state}, argv...)
	code := dispatch.Execute(context.Background(), "codex-session-relay", line, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// handover is one linkage-handover for the store seat; scopeKind "" leaves the option off.
func (s *storeSeat) handover(role, scope, scopeKind, expect, task string) (int, string, string) {
	s.t.Helper()
	argv := []string{"linkage-handover", "--role", role, "--scope", scope,
		"--expect-task", expect, "--task", task, "--host", "host",
		"--evidence", "the seat changes hands", "--actor", "test"}
	if scopeKind != "" {
		argv = append(argv, "--scope-kind", scopeKind)
	}
	return s.seatLine(argv...)
}

// mustHandover is handover with a successful answer decoded.
func (s *storeSeat) mustHandover(role, scope, scopeKind, expect, task string) map[string]any {
	s.t.Helper()
	code, out, errOut := s.handover(role, scope, scopeKind, expect, task)
	if code != 0 {
		s.t.Fatalf("linkage-handover %s %s --scope-kind %q -> %s: exit %d stdout %q stderr %q", role, scope, scopeKind, task, code, out, errOut)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		s.t.Fatalf("handover answer is not JSON: %v (%q)", err, out)
	}
	return answer
}

// mustRefuseHandover is a handover line that has to be refused with reason and nothing else.
func (s *storeSeat) mustRefuseHandover(reason, role, scope, scopeKind, expect, task string) {
	s.t.Helper()
	code, out, errOut := s.handover(role, scope, scopeKind, expect, task)
	if code != 2 {
		s.t.Fatalf("linkage-handover %s %s --scope-kind %q -> %s: exit %d stdout %q stderr %q, want exit 2", role, scope, scopeKind, task, code, out, errOut)
	}
	if got := s.refusedReason(out); got != reason {
		s.t.Fatalf("linkage-handover %s %s --scope-kind %q -> %s: refused %q, want %q (%q)", role, scope, scopeKind, task, got, reason, out)
	}
}

// mustRefuseParser is a line the argparse spec refuses before any handler: exit 2, no stdout, and
// the usage text on stderr.
func (s *storeSeat) mustRefuseParser(argv ...string) {
	s.t.Helper()
	code, out, errOut := s.seatLine(argv...)
	if code != 2 {
		s.t.Fatalf("%v: exit %d stdout %q stderr %q, want exit 2", argv, code, out, errOut)
	}
	if out != "" {
		s.t.Fatalf("%v: a parser refusal answers nothing on stdout, got %q", argv, out)
	}
	if errOut == "" {
		s.t.Fatalf("%v: a parser refusal says why on stderr", argv)
	}
}

// seatOwner reads the live owner of the store seat, the way capacity's checkDeclarer reads it for
// the store scope (internal/relay/capacity/capacity.go, the store branch): a live supervisor
// binding held by the task. It returns "" when the task holds no live seat.
func (s *storeSeat) seatOwner(task string) string {
	s.t.Helper()
	held, err := store.Open(context.Background(), filepath.Join(s.state, "relay.sqlite3"), "")
	if err != nil {
		s.t.Fatal(err)
	}
	defer held.Close()
	var owner string
	err = held.Querier(context.Background()).QueryRowContext(context.Background(),
		"SELECT task_id FROM scope_bindings WHERE task_id = ? AND role = 'supervisor'"+
			"  AND status IN ('active','paused') AND superseded_by IS NULL", task).Scan(&owner)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return ""
		}
		s.t.Fatal(err)
	}
	return owner
}

// TestStoreSeatHandoverMovesTheStoreSeat: the seat the management session holds changes hands with
// --scope-kind store, and the row the store scope is read from follows the move. Before CRW-792 the
// line was an unknown argument (exit 2, "unrecognized arguments: --scope-kind store" on stderr).
func TestStoreSeatHandoverMovesTheStoreSeat(t *testing.T) {
	s := newStoreSeat(t)
	s.mustBind("01management", storeSeatKind)
	answer := s.mustHandover(storeSeatRole, storeSeatKey, storeSeatKind, "01management", "01successor")
	if answer["role"] != storeSeatRole || answer["scopeKind"] != storeSeatKind || answer["scopeKey"] != storeSeatKey {
		t.Fatalf("handover answered %v", answer)
	}
	if answer["taskId"] != "01successor" {
		t.Fatalf("handover left %v holding the seat, want 01successor", answer["taskId"])
	}
	if owner := s.seatOwner("01successor"); owner != "01successor" {
		t.Fatalf("the successor holds no live store seat (read %q)", owner)
	}
	if owner := s.seatOwner("01management"); owner != "" {
		t.Fatalf("the outgoing owner still holds a live store seat (%q)", owner)
	}
}

// TestStoreSeatHandoverReturnsTheSeatToAPreviousOwner: handing the seat back takes the reactivation
// path, not a second insert, so a task may hold the seat again after it gave it up.
func TestStoreSeatHandoverReturnsTheSeatToAPreviousOwner(t *testing.T) {
	s := newStoreSeat(t)
	s.mustBind("01management", storeSeatKind)
	s.mustHandover(storeSeatRole, storeSeatKey, storeSeatKind, "01management", "01successor")
	answer := s.mustHandover(storeSeatRole, storeSeatKey, storeSeatKind, "01successor", "01management")
	if answer["taskId"] != "01management" {
		t.Fatalf("handing the seat back left %v holding it", answer["taskId"])
	}
	if owner := s.seatOwner("01management"); owner != "01management" {
		t.Fatalf("the first owner does not hold the seat again (read %q)", owner)
	}
	if owner := s.seatOwner("01successor"); owner != "" {
		t.Fatalf("the second owner still holds a live store seat (%q)", owner)
	}
}

// TestStoreSeatHandoverNamesTheScopeKindOnlyForTheStoreSeat: --scope-kind is the supervisor's and
// the store key's, and no other combination reaches the handler.
func TestStoreSeatHandoverNamesTheScopeKindOnlyForTheStoreSeat(t *testing.T) {
	s := newStoreSeat(t)
	s.mustRefuseHandover("scope_role_mismatch", "parent", storeSeatKey, storeSeatKind, "01p", "01q")
	s.mustRefuseHandover("scope_role_mismatch", storeSeatRole, "INIT-1", storeSeatKind, "01s", "01t")
	// --role's own choices refuse a child or an unknown role before the handler is reached.
	s.mustRefuseParser("linkage-handover", "--role", "child", "--scope-kind", storeSeatKind, "--scope", storeSeatKey,
		"--expect-task", "01c", "--task", "01d", "--host", "host", "--evidence", "e", "--actor", "a")
	s.mustRefuseParser("linkage-handover", "--role", "nobody", "--scope-kind", storeSeatKind, "--scope", storeSeatKey,
		"--expect-task", "01e", "--task", "01f", "--host", "host", "--evidence", "e", "--actor", "a")
}

// TestStoreSeatHandoverRefusesAnExpectationThatIsNotTheOwner: the outgoing owner confirms the seat
// it believes it holds, so a wrong --expect-task is refused rather than resolved.
func TestStoreSeatHandoverRefusesAnExpectationThatIsNotTheOwner(t *testing.T) {
	s := newStoreSeat(t)
	s.mustBind("01management", storeSeatKind)
	s.mustRefuseHandover("handover_unconfirmed", storeSeatRole, storeSeatKey, storeSeatKind, "01somebody-else", "01successor")
}

// TestStoreSeatHandoverRefusesAReplacementThatHoldsAnotherSupervisorSeat: one task holds one
// supervisor seat, so the seat cannot move to a task that already supervises an initiative.
func TestStoreSeatHandoverRefusesAReplacementThatHoldsAnotherSupervisorSeat(t *testing.T) {
	s := newStoreSeat(t)
	if code, out := s.run("linkage-bind", "--role", storeSeatRole, "--scope", "INIT-1", "--task", "01elsewhere", "--host", "host"); code != 0 {
		t.Fatalf("initiative seat: exit %d stdout %q", code, out)
	}
	s.mustBind("01management", storeSeatKind)
	s.mustRefuseHandover("role_already_bound", storeSeatRole, storeSeatKey, storeSeatKind, "01management", "01elsewhere")
}

// TestStoreSeatHandoverRefusesAnUnregisteredScope: a seat nobody holds has no owner to replace.
func TestStoreSeatHandoverRefusesAnUnregisteredScope(t *testing.T) {
	s := newStoreSeat(t)
	s.mustRefuseHandover("unregistered_scope", storeSeatRole, storeSeatKey, storeSeatKind, "01management", "01successor")
}

// TestStoreSeatHandoverMovesTheLimitDeclaration: after the seat moves, the store scope is read from
// the new owner's live binding and no longer from the old one, which is what capacity's
// check_declarer resolves for --scope-kind store.
func TestStoreSeatHandoverMovesTheLimitDeclaration(t *testing.T) {
	s := newStoreSeat(t)
	s.mustBind("01management", storeSeatKind)
	s.mustHandover(storeSeatRole, storeSeatKey, storeSeatKind, "01management", "01successor")
	if owner := s.seatOwner("01successor"); owner != "01successor" {
		t.Fatalf("the new owner's live store seat is %q, so limit-declare --scope-kind store cannot read it", owner)
	}
	if owner := s.seatOwner("01management"); owner != "" {
		t.Fatalf("the old owner's store seat is still live (%q), so limit-declare --scope-kind store would still accept it", owner)
	}
}

// TestStoreSeatHandoverLeavesTheRoleOwnLevelAlone: without --scope-kind every handover moves the
// level it always did, and the store key still names an initiative.
func TestStoreSeatHandoverLeavesTheRoleOwnLevelAlone(t *testing.T) {
	s := newStoreSeat(t)
	if code, out := s.run("linkage-bind", "--role", "parent", "--scope", "PROJ-1", "--task", "01parent-a", "--host", "host"); code != 0 {
		t.Fatalf("parent binding: exit %d stdout %q", code, out)
	}
	answer := s.mustHandover("parent", "PROJ-1", "", "01parent-a", "01parent-b")
	if answer["scopeKind"] != "project" {
		t.Fatalf("a parent handover moved %v, want the project scope", answer["scopeKind"])
	}
	// Without --scope-kind the store key is an initiative's key, as it always was.
	s.mustRefuseHandover("unregistered_scope", storeSeatRole, storeSeatKey, "", "01management", "01successor")
}
