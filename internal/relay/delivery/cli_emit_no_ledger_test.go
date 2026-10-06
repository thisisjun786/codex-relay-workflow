package delivery

import (
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// CRW-680: emit's existence check reads the turn without creating an operations ledger, and the
// turn-id form check runs before the store is opened, so a refused malformed turn id records no
// socket_path. Every CRW-675 answer stays the same.

// crw680Ledgers is every operations ledger under root: the store directory and the socket's state
// directory both live under the side's home.
func crw680Ledgers(t *testing.T, root string) []string {
	t.Helper()
	found := []string{}
	mustDo(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "operations-") {
			found = append(found, path)
		}
		return nil
	}))
	sort.Strings(found)
	return found
}

// crw680Listed is a fakehost answering one listing that holds crw675Turn.
func crw680Listed(t *testing.T) *fakehost.Server {
	t.Helper()
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": crw675Turn, "status": "inProgress", "startedAt": 1700000001}}, "nextCursor": nil}})
	return host
}

func TestCRW680_a_refused_emit_without_socket_creates_no_operations_ledger(t *testing.T) {
	t.Parallel()
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}})
	side, rid := crw675Side(t, host.SocketPath)
	before := crw680Ledgers(t, side.home)
	refused, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 2 || refused["reason"] != "unassigned_turn" {
		t.Fatalf("a turn the exhausted listing does not hold: %d %v", code, refused)
	}
	if after := crw680Ledgers(t, side.home); !reflect.DeepEqual(before, after) {
		t.Fatalf("the existence check created an operations ledger: %v -> %v", before, after)
	}
}

func TestCRW680_a_staged_emit_without_socket_creates_no_operations_ledger(t *testing.T) {
	t.Parallel()
	side, rid := crw675Side(t, crw680Listed(t).SocketPath)
	before := crw680Ledgers(t, side.home)
	accepted, code := runJSON(t, side, crw675Emit(rid, crw675Turn)...)
	if code != 0 || accepted["stage"] != "staged" {
		t.Fatalf("a listed turn stages as before: %d %v", code, accepted)
	}
	if after := crw680Ledgers(t, side.home); !reflect.DeepEqual(before, after) {
		t.Fatalf("the existence check created an operations ledger: %v -> %v", before, after)
	}
}

func TestCRW680_a_refused_malformed_turn_id_records_no_socket_path(t *testing.T) {
	t.Parallel()
	side, rid := crw675Side(t, "")
	socketX := filepath.Join(t.TempDir(), "host-x.sock")
	refused, code := runJSON(t, side, append([]string{"--socket", socketX}, crw675Emit(rid, crw675Typo)...)...)
	if code != 2 || refused["reason"] != "unassigned_turn" {
		t.Fatalf("a malformed turn id under --socket: %d %v", code, refused)
	}
	if recorded := strings.Trim(sqliteDump(t, side, "SELECT value FROM schema_meta WHERE key='socket_path'"), "[]\"\n "); recorded != "" {
		t.Fatalf("the refused emit recorded socket_path %q, want none", recorded)
	}
	// The store is still unbound, so a following valid emit with another socket is not refused
	// for a socket mismatch.
	accepted, code := runJSON(t, side, append([]string{"--socket", crw680Listed(t).SocketPath}, crw675Emit(rid, crw675Turn)...)...)
	if code != 0 || accepted["stage"] != "staged" {
		t.Fatalf("a valid emit with another socket: %d %v", code, accepted)
	}
}
