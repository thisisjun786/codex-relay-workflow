package delivery

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-675: emit refuses unassigned_turn, writing nothing, when --turn-id does not have the Codex id
// form of a Codex-form --turn-thread, and — without --socket — when a read-only ReadTurn on the App
// Server socket the store recorded finds the turn absent from an exhausted listing. Every other
// case stages exactly as it did: a listed turn, an unreachable host, a HostUnavailable page budget,
// and a store that records no socket. A thread that is not a Codex id (every older fixture here) is
// not checked. The refusal name, the output shape and the exit code are unchanged.
const (
	crw675Child  = "01a10ea8-6513-7e32-b4d9-70b889d15dd2"
	crw675Anchor = "01a10ea8-6513-7e32-b4d9-70b889d15dd1"
	crw675Turn   = "01a10ea8-dcee-7d42-be24-cc53cbc76607"
	// crw675Typo is crw675Turn with one hyphen moved, the shape of the id a child mistyped by hand.
	crw675Typo = "01a10ea8-dcee-7d42-be24-cc53cbc-76607"
)

// crw675Side is a side whose registered child and generation anchor are Codex-form ids, so the form
// check and the existence check run where the seed's "01child-task" would not. socket binds the
// store to that App Server socket when it is not empty; an empty socket leaves the store recording
// none, as the seed does.
func crw675Side(t *testing.T, socket string) (*cliSide, string) {
	t.Helper()
	side := newSide(t, filepath.Join(t.TempDir(), "work"))
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(side.state, "relay.sqlite3"), socket)
	mustDo(t, err)
	_, err = s.Querier(ctx).ExecContext(ctx, "UPDATE relationships SET child_task_id = ?", crw675Child)
	mustDo(t, err)
	_, err = s.Querier(ctx).ExecContext(ctx, "UPDATE generations SET dispatch_turn_id = ?", crw675Anchor)
	mustDo(t, err)
	mustDo(t, s.Close())
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	return side, rid
}

// crw675Emit is the child's emit on a continuation turn: the turn is not the generation's anchor,
// so the claim admits it exactly as a Loop's later turn is admitted.
func crw675Emit(rid, turn string) []string {
	return []string{"emit", "--relationship", rid, "--generation", "1", "--outcome", "blocked_needs_input",
		"--turn-thread", crw675Child, "--turn-id", turn,
		"--continues-anchor", crw675Anchor, "--continuation-actor", crw675Child, "--continuation-reason", "a later turn of this execution"}
}

// crw675Wrote is what a refusal must not have written: no receipt event and no refusal row, because
// the check runs before the receipt intake is reached.
func crw675Wrote(t *testing.T, side *cliSide) string {
	t.Helper()
	return sqliteDump(t, side, "SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM refusals)")
}

func TestCRW675_emit_refuses_a_turn_id_that_is_not_the_threads_form(t *testing.T) {
	t.Parallel()
	side, rid := crw675Side(t, "")
	before := crw675Wrote(t, side)
	// Red before the change: the claim admits the mistyped id and the receipt stages, because
	// nothing read the turn id's form.
	refused, code := runJSON(t, side, crw675Emit(rid, crw675Typo)...)
	if code != 2 || refused["reason"] != "unassigned_turn" {
		t.Fatalf("a turn id that is not the thread's form: %d %v", code, refused)
	}
	detail, _ := refused["detail"].(string)
	for _, want := range []string{"Codex id form", "command output"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, detail)
		}
	}
	if after := crw675Wrote(t, side); after != before {
		t.Fatalf("a refused emit wrote %s, want %s", after, before)
	}
	// The form the thread requires is accepted, and with no recorded socket the receipt stages as
	// it always did.
	accepted, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 0 || accepted["stage"] != "staged" || accepted["receipt"].(map[string]any)["outcome"] != "blocked_needs_input" {
		t.Fatalf("the same emit with a Codex-form turn id: %d %v", code, accepted)
	}
}

// With --socket the form check still runs first: the emit is refused before the host is reached, so
// a socket that leads nowhere changes nothing about the refusal.
func TestCRW675_the_form_check_applies_with_a_socket_too(t *testing.T) {
	t.Parallel()
	side, rid := crw675Side(t, "")
	args := append([]string{"--socket", filepath.Join(t.TempDir(), "no-such-host.sock")}, crw675Emit(rid, crw675Typo)...)
	refused, code := runJSON(t, side, args...)
	if code != 2 || refused["reason"] != "unassigned_turn" {
		t.Fatalf("a mistyped turn id under --socket: %d %v", code, refused)
	}
	if detail, _ := refused["detail"].(string); !strings.Contains(detail, "Codex id form") {
		t.Fatalf("the refusal is not the form check's:\n%s", detail)
	}
}

func TestCRW675_emit_refuses_a_turn_the_recorded_host_does_not_hold(t *testing.T) {
	t.Parallel()
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}})
	side, rid := crw675Side(t, host.SocketPath)
	before := crw675Wrote(t, side)
	// Red before the change: the emit never read the turn, so a turn that does not exist staged.
	refused, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 2 || refused["reason"] != "unassigned_turn" {
		t.Fatalf("a turn the exhausted listing does not hold: %d %v", code, refused)
	}
	detail, _ := refused["detail"].(string)
	for _, want := range []string{"does not exist on", "command output"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, detail)
		}
	}
	if after := crw675Wrote(t, side); after != before {
		t.Fatalf("a refused emit wrote %s, want %s", after, before)
	}
}

func TestCRW675_emit_stages_a_turn_the_recorded_host_holds(t *testing.T) {
	t.Parallel()
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": crw675Turn, "status": "inProgress", "startedAt": 1700000001}}, "nextCursor": nil}})
	side, rid := crw675Side(t, host.SocketPath)
	accepted, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 0 || accepted["stage"] != "staged" || accepted["terminalProof"] != "claimed" || accepted["observedTurnStatus"] != "inProgress" {
		t.Fatalf("a listed turn stages as before: %d %v", code, accepted)
	}
}

func TestCRW675_emit_stages_when_the_recorded_host_cannot_be_read(t *testing.T) {
	t.Parallel()
	side, rid := crw675Side(t, filepath.Join(t.TempDir(), "no-such-host.sock"))
	accepted, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 0 || accepted["stage"] != "staged" {
		t.Fatalf("an unreachable host stages as before: %d %v", code, accepted)
	}
}

func TestCRW675_emit_stages_when_the_recorded_host_exhausts_its_page_budget(t *testing.T) {
	t.Parallel()
	host := fakehost.Start(t)
	// Every page carries a cursor, so the listing is never exhausted and ReadTurn answers
	// HostUnavailable: a page budget that ran out is not evidence of absence.
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": "more"}})
	side, rid := crw675Side(t, host.SocketPath)
	accepted, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 0 || accepted["stage"] != "staged" {
		t.Fatalf("a HostUnavailable read stages as before: %d %v", code, accepted)
	}
}

func TestCRW675_emit_stages_when_the_store_records_no_socket(t *testing.T) {
	t.Parallel()
	side, rid := crw675Side(t, "")
	accepted, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 0 || accepted["stage"] != "staged" {
		t.Fatalf("a store that records no socket stages as before: %d %v", code, accepted)
	}
}
