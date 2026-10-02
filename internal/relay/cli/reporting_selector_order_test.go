package cli_test

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// selectorFixture is a temporary home whose default scope directory holds one store: the one a
// command given no --state discovers from the default App Server socket (decision 73). Nothing
// here refers to the real default scope.
type selectorFixture struct {
	home, socket, dir string
}

// newSelectorFixture records another socket in that store, or the default socket itself.
func newSelectorFixture(t *testing.T, recordsDefaultSocket bool) selectorFixture {
	t.Helper()
	home := tempHome(t)
	withHome(t, home)
	socket := filepath.Join(home, "codex-home", "app-server-control", "app-server-control.sock")
	scope, err := store.SocketScope(socket)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local", "state", "codex-session-relay", scope)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(home, "other.sock")
	if recordsDefaultSocket {
		recorded = socket
	}
	db := filepath.Join(dir, "relay.sqlite3")
	testsupport.Create(t, db, recorded, "go")

	// What the fixture is, so a mistake here fails here and not in a behavior assertion.
	wantRecorded, _ := store.CanonicalSocket(recorded)
	requested, _ := store.CanonicalSocket(socket)
	if got := store.StoreSocket(db); got != wantRecorded || (got == requested) != recordsDefaultSocket {
		t.Fatalf("the store records %q, want %q (the default socket is %q)", got, wantRecorded, requested)
	}
	selected := obj(decode(t, golang(t, home, "doctor").stdout)["stateSelection"])
	if selected["path"] != dir {
		t.Fatalf("default discovery selects %v, want the fixture's %s", selected["path"], dir)
	}
	return selectorFixture{home: home, socket: socket, dir: dir}
}

// show is a reporting-show line whose own arguments are all valid, after the root options.
func (f selectorFixture) show(root ...string) []string {
	return append(append([]string{}, root...), "reporting-show",
		"--marker-root", filepath.Join(f.home, "markers"), "--workspace", filepath.Join(f.home, "workspace"),
		"--assignment", strings.Repeat("a", 64), "--session", "session-1", "--turn", "turn-1")
}

// derive is a reporting-derive line whose own arguments are all valid, after the root options.
func (f selectorFixture) derive(root ...string) []string {
	return append(append([]string{}, root...), "reporting-derive", "--relationship", "r", "--turn", "t")
}

// The reporting forms judge the root options they require or forbid before the selected store's
// recorded socket is compared (CRW-264): an invalid line is the usage error whatever socket the
// store recorded, and answers the same against a store that recorded the socket asked for. The
// store is read-only here, so nothing in its directory changes either way.
func TestReportingSelectorsAreValidatedBeforeTheRecordedSocket(t *testing.T) {
	for _, held := range []struct {
		name           string
		recordsDefault bool
	}{
		{"a store that recorded another socket", false},
		{"a store that recorded the socket asked for", true},
	} {
		t.Run(held.name, func(t *testing.T) {
			f := newSelectorFixture(t, held.recordsDefault)
			for _, c := range []struct {
				name   string
				argv   []string
				detail string
			}{
				{"reporting-show without a root --state", f.show(), "reporting-show requires explicit --state"},
				{"reporting-derive given a root --socket", f.derive("--socket", f.socket), "reporting-derive does not take --socket"},
				{"reporting-show with --state given a root --socket", f.show("--state", f.dir, "--socket", f.socket), "reporting-show does not take --socket"},
			} {
				t.Run(c.name, func(t *testing.T) {
					before := dirFiles(t, f.dir)
					answer := golang(t, f.home, c.argv...)
					got := decode(t, answer.stdout)
					if answer.code != 4 || got["error"] != "usage" || got["detail"] != c.detail || got["reason"] != nil {
						t.Fatalf("want the usage error %q (exit 4): %+v", c.detail, answer)
					}
					if after := dirFiles(t, f.dir); !maps.Equal(before, after) {
						t.Fatalf("the refused line wrote into the store directory:\nbefore %v\nafter  %v", before, after)
					}
				})
			}
		})
	}
}

// A line that is invalid in its selectors and also names a --kind-module the relay cannot import
// answers the selector usage error, which no longer waits behind the store's checks. A line whose
// selectors are valid keeps answering the module error, exit 3 for an empty or relative name and
// exit 4 for an unknown one.
func TestReportingSelectorsPrecedeKindModuleErrors(t *testing.T) {
	f := newSelectorFixture(t, true)
	modules := []struct {
		name, module string
		code         int
		detail       string
	}{
		{"an unknown module", "nosuch", 4, "is not a module this relay knows"},
		{"an empty module", "", 3, "is not an absolute module name"},
		{"a relative module", ".relative", 3, "is not an absolute module name"},
	}
	for _, m := range modules {
		t.Run(m.name, func(t *testing.T) {
			invalid := golang(t, f.home, f.derive("--kind-module", m.module, "--socket", f.socket)...)
			if got := decode(t, invalid.stdout); invalid.code != 4 || got["error"] != "usage" || got["detail"] != "reporting-derive does not take --socket" {
				t.Fatalf("an invalid selector with %s: want the selector usage error (exit 4): %+v", m.name, invalid)
			}
			for _, valid := range [][]string{f.derive("--kind-module", m.module), f.show("--kind-module", m.module, "--state", f.dir)} {
				answer := golang(t, f.home, valid...)
				if answer.code != m.code || !strings.Contains(answer.stdout, m.detail) {
					t.Fatalf("%s on a valid line: want exit %d naming %q: %+v", m.name, m.code, m.detail, answer)
				}
			}
		})
	}
}
