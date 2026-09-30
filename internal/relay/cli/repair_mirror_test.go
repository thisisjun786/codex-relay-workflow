package cli_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// schemaMeta is every schema_meta row of the store in state, read through a read-only connection.
func schemaMeta(t *testing.T, state string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT key, value FROM schema_meta")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		meta[key] = value
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return meta
}

// takeoverAnswer is one `takeover` action's answer: the status object, or the refusal record.
func takeoverAnswer(t *testing.T, answer run) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(answer.stdout), &object); err != nil {
		t.Fatalf("takeover answered no JSON object (exit %d): %q %q", answer.code, answer.stdout, answer.stderr)
	}
	return object
}

// Backlog line 6: the torn publication "initial stamp committed, mirror absent" (cutover.md
// Record, Torn publications) has one way out, the explicit controller recovery, and the todo-42
// runbook relies on it should Step 0.4 stop between the fence's schema_meta COMMIT and its
// takeover.json publication. Whichever runtime committed the stamp: `takeover status` reports
// the state (jsonStale, the stamp's owner at epoch 1) rather than failing on the missing
// mirror; `takeover repair-mirror` publishes exactly the mirror the initializer would have
// published, derived only from schema_meta, and writes nothing to schema_meta; afterwards the
// store is the one the initializer leaves: the owner's writer is admitted in its runtime, the
// other runtime answers as it answers any store the other owns, and both doctors report no
// ownership detail.
func Test30RepairMirrorPublishesTheStampedMirror(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	for _, tc := range []struct{ creator, socket string }{
		{"python", filepath.Join(home, "app-server.sock")},
		{"go", filepath.Join(home, "app-server.sock")},
		// A store no writable opener has bound yet has no scope, and its mirror no socket.
		{"python", ""},
		{"go", ""},
	} {
		creator, socketed := tc.creator, tc.socket != ""
		name := creator
		if !socketed {
			name += " socketless"
		}
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(home, strings.ReplaceAll(name, " ", "-"))
			routing := []string{"--state", state}
			if socketed {
				routing = append(routing, "--socket", tc.socket)
			}
			with := func(argv ...string) []string { return append(append([]string{}, routing...), argv...) }
			relay := map[string]func(argv ...string) run{
				"python": func(argv ...string) run { return fence(t, argv...) },
				"go":     func(argv ...string) run { return binaryRun(t, alias, argv...) },
			}
			if created := relay[creator](with("store-challenge", "--write")...); created.code != 0 {
				t.Fatalf("%s creates the store: %+v", creator, created)
			}
			path := filepath.Join(state, "relay.sqlite3")
			published, err := ownership.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			if (published.AppServerSocket != nil) != socketed || (published.ScopeKey != nil) != socketed {
				t.Fatalf("the initializer's socket and scope: %+v", published)
			}
			// The crash between the stamp's COMMIT and the mirror's publication.
			if err = os.Remove(filepath.Join(state, "takeover.json")); err != nil {
				t.Fatal(err)
			}
			meta := schemaMeta(t, state)
			status := binaryRun(t, alias, with("takeover", "status")...)
			if got := takeoverAnswer(t, status); status.code != 0 || got["owner"] != creator || got["epoch"] != float64(1) || got["takeoverId"] != "" || got["phase"] != "active" || got["jsonStale"] != true {
				t.Fatalf("status of the torn stamp: %+v", status)
			}
			repaired := binaryRun(t, alias, with("takeover", "repair-mirror")...)
			if got := takeoverAnswer(t, repaired); repaired.code != 0 || got["owner"] != creator || got["epoch"] != float64(1) || got["phase"] != "active" || got["jsonStale"] != false {
				t.Fatalf("repair-mirror: %+v", repaired)
			}
			record, err := ownership.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			record.UpdatedAt, published.UpdatedAt = "", ""
			if !reflect.DeepEqual(record, published) {
				t.Fatalf("the repaired mirror is not the initializer's:\n%+v\n%+v", record, published)
			}
			if after := schemaMeta(t, state); !reflect.DeepEqual(after, meta) {
				t.Fatalf("repair-mirror wrote schema_meta:\n%v\n%v", meta, after)
			}
			for runtime, doctor := range relay {
				if block := ownershipBlock(t, doctor("--state", state, "--json", "doctor").stdout); !strings.Contains(block, `"detail": null`) || !strings.Contains(block, `"phase": "active"`) {
					t.Fatalf("%s doctor after the repair: %s", runtime, block)
				}
			}
			for runtime, write := range relay {
				answer := write(with("store-challenge", "--write")...)
				if (runtime == creator) != (answer.code == 0) || runtime != creator && !strings.Contains(answer.stdout, "store_owned_by_other") {
					t.Fatalf("%s writer on the repaired %s store: %+v", runtime, creator, answer)
				}
			}
			// Nothing is left to repair: a second repair refuses and changes nothing.
			again := binaryRun(t, alias, with("takeover", "repair-mirror")...)
			if got := takeoverAnswer(t, again); again.code != 2 || got["reason"] != "store_owned_by_other" {
				t.Fatalf("repair-mirror over a published mirror: %+v", again)
			}
		})
	}
}

// The repair refuses every state but the exact torn stamp and leaves it as it found it: a
// mirror that is present (malformed or intact) is not absent, a stamp past its initial epoch or
// naming a takeover is not the initializer's, a directory holding only the write gate stays the
// partial store decision D0 refuses, and a legacy store is never fenced by a repair.
func Test30RepairMirrorRefusesAnythingButTheTornStamp(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	socket := filepath.Join(home, "app-server.sock")
	durable := func(statement string) func(t *testing.T, state string) {
		return func(t *testing.T, state string) {
			db, err := sql.Open("sqlite", filepath.Join(state, "relay.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		torn    bool                             // the mirror is removed before prepare runs
		prepare func(t *testing.T, state string) // nil: the store as created
		detail  string                           // what the store_owned_by_other refusal names; "" for another failure
	}{
		{"intact mirror", false, nil, "the takeover record exists"},
		{"malformed mirror", true, func(t *testing.T, state string) {
			if err := os.WriteFile(filepath.Join(state, "takeover.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "decode mirror"},
		{"epoch 2", true, durable("UPDATE schema_meta SET value='2' WHERE key='owner_epoch'"), "past its initial publication (owner_epoch 2,"},
		{"a takeover id", true, durable("UPDATE schema_meta SET value='0123456789abcdef0123456789abcdef' WHERE key='takeover_id'"), `past its initial publication (owner_epoch 1, takeover_id "0123456789abcdef0123456789abcdef")`},
		{"a missing ownership key", true, durable("DELETE FROM schema_meta WHERE key='rollback_allowed'"), "read mirror"},
		{"legacy", true, durable("DELETE FROM schema_meta WHERE key IN ('writer_protocol','owner','owner_epoch','takeover_id','rollback_allowed','python_compatibility_build')"), "read mirror"},
		// Physical identity needs the database: the command fails before any lock.
		{"gate only", true, func(t *testing.T, state string) {
			for _, name := range []string{"relay.sqlite3", "relay.sqlite3-wal", "relay.sqlite3-shm"} {
				if err := os.Remove(filepath.Join(state, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(home, strings.ReplaceAll(tc.name, " ", "-"))
			if created := binaryRun(t, alias, "--state", state, "--socket", socket, "store-challenge", "--write"); created.code != 0 {
				t.Fatalf("go creates the store: %+v", created)
			}
			if tc.torn {
				if err := os.Remove(filepath.Join(state, "takeover.json")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.prepare != nil {
				tc.prepare(t, state)
			}
			before := stateFiles(t, state)
			repaired := binaryRun(t, alias, "--state", state, "--socket", socket, "takeover", "repair-mirror")
			if repaired.code == 0 {
				t.Fatalf("repair-mirror accepted %s: %+v", tc.name, repaired)
			}
			if tc.detail != "" {
				if got := takeoverAnswer(t, repaired); repaired.code != 2 || got["reason"] != "store_owned_by_other" || !strings.Contains(fmt.Sprint(got["detail"]), tc.detail) {
					t.Fatalf("repair-mirror over %s: %+v", tc.name, repaired)
				}
			}
			if after := stateFiles(t, state); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused repair changed the store:\n%v\n%v", before, after)
			}
		})
	}
}

// stateFiles is the content of the store's database and mirror, "" for one that is absent.
func stateFiles(t *testing.T, state string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"relay.sqlite3", "relay.sqlite3-wal", "takeover.json"} {
		raw, err := os.ReadFile(filepath.Join(state, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		files[name] = string(raw)
	}
	return files
}
