package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
)

// Discovery given no --socket scopes the state directory by the default App Server socket, the
// one the bridge defaults to, so a command run without --socket selects the store a relay
// service started on that socket serves (docs/port/decisions.md section 73).
func TestDiscoveryWithoutASocketIsScopedByTheDefaultSocket(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	root := t.TempDir()
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	// ownState gives a subtest a home and a state root (XDG_STATE_HOME) of its own, with
	// CODEX_HOME unset. The home's own state root is live to the fixture opener, so stores are
	// made under XDG_STATE_HOME (makeStoreIn).
	ownState := func(t *testing.T) (home, base string) {
		t.Helper()
		dir := t.TempDir()
		home = filepath.Join(dir, "home")
		t.Setenv("HOME", home)
		t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
		t.Setenv("CODEX_HOME", "")
		must(t, os.Unsetenv("CODEX_HOME"))
		return home, filepath.Join(dir, "state", "codex-session-relay")
	}
	scopeOf := func(t *testing.T, socket string) string {
		t.Helper()
		canonical, err := canonicalSocket(socket)
		must(t, err)
		return socketHash(canonical)
	}

	t.Run("no socket gives the default socket's scope", func(t *testing.T) {
		home, base := ownState(t)
		socket := filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")
		named, err := DefaultSocket()
		if err != nil || named != socket {
			t.Fatalf("DefaultSocket() = %q, %v; want %q", named, err, socket)
		}
		got, err := DiscoverStateDir("")
		must(t, err)
		scope := scopeOf(t, socket)
		if got.Path != filepath.Join(base, scope) || got.SocketScope != scope || got.Source != "xdg" {
			t.Fatalf("selection %+v; want scope %s under %s", got, scope, base)
		}
		if want := "; scoped by the default Codex App Server socket " + socket; !strings.HasSuffix(got.Detail, want) {
			t.Fatalf("detail %q does not end with %q", got.Detail, want)
		}
		// The selection carries that socket for validation (selection.Refusal), and only then.
		if got.DefaultSocket != socket {
			t.Fatalf("DefaultSocket %q, want %q", got.DefaultSocket, socket)
		}
		// The directory a service started with --socket <the default socket> discovers.
		explicit, err := DiscoverStateDir(socket)
		must(t, err)
		if explicit.Path != got.Path || explicit.SocketScope != got.SocketScope || explicit.DefaultSocket != "" {
			t.Fatalf("--socket %s selects %+v, no socket %+v", socket, explicit, got)
		}
		if _, err := os.Stat(got.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("discovery created %s: %v", got.Path, err)
		}
	})

	t.Run("CODEX_HOME is honoured", func(t *testing.T) {
		_, base := ownState(t)
		codexHome := filepath.Join(root, "codex-elsewhere")
		t.Setenv("CODEX_HOME", codexHome)
		socket := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
		got, err := DiscoverStateDir("")
		must(t, err)
		if got.Path != filepath.Join(base, scopeOf(t, socket)) || !strings.HasSuffix(got.Detail, " "+socket) {
			t.Fatalf("selection %+v; want the scope of %s", got, socket)
		}
		// A CODEX_HOME through a symbolic link is the socket it resolves to, as --socket is.
		link := filepath.Join(root, "codex-link")
		must(t, os.MkdirAll(codexHome, 0o700))
		must(t, os.Symlink(codexHome, link))
		t.Setenv("CODEX_HOME", link)
		linked, err := DiscoverStateDir("")
		must(t, err)
		if linked.SocketScope != got.SocketScope {
			t.Fatalf("CODEX_HOME through a link scoped %+v, not %+v", linked, got)
		}
		// An empty CODEX_HOME is no CODEX_HOME: the home's .codex.
		t.Setenv("CODEX_HOME", "")
		empty, err := DiscoverStateDir("")
		must(t, err)
		home, _ := os.LookupEnv("HOME")
		if want := scopeOf(t, filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")); empty.SocketScope != want {
			t.Fatalf("empty CODEX_HOME scoped %q, want %q", empty.SocketScope, want)
		}
	})

	t.Run("the legacy default directory is kept only when it alone holds a store", func(t *testing.T) {
		home, base := ownState(t)
		socket := filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")
		scope := scopeOf(t, socket)
		legacy := filepath.Join(base, LegacyDefaultScope)

		// Nothing anywhere: the default socket's directory, where a writer would create one.
		fresh, err := DiscoverStateDir("")
		must(t, err)
		if fresh.Path != filepath.Join(base, scope) {
			t.Fatalf("fresh selection %+v", fresh)
		}

		// Only "default" holds a store: it is kept, and its recording no socket is not held
		// against it as it would be beside a store discovery could name.
		makeStoreIn(t, root, base, LegacyDefaultScope, "")
		kept, err := DiscoverStateDir("")
		must(t, err)
		if kept.Path != legacy || kept.SocketScope != LegacyDefaultScope || kept.DefaultSocket != "" || len(kept.Unidentified) != 0 || len(kept.Ambiguous) != 0 ||
			!strings.Contains(kept.Detail, "; kept the legacy default directory") {
			t.Fatalf("legacy selection %+v", kept)
		}
		// --socket names the same socket, but only a selection made without one keeps "default":
		// discovery for an explicit socket reports it as a store that records no socket.
		explicit, err := DiscoverStateDir(socket)
		must(t, err)
		if explicit.Path != filepath.Join(base, scope) || !slices.Equal(explicit.Unidentified, []string{legacy}) {
			t.Fatalf("--socket selection %+v", explicit)
		}

		// Another store records the default socket: that store is the default socket's, and
		// "default" no longer holds the only store.
		makeStoreIn(t, root, base, "adopted", socket)
		adopted, err := DiscoverStateDir("")
		must(t, err)
		if adopted.Path != filepath.Join(base, "adopted") || adopted.SocketScope != "adopted" || !strings.Contains(adopted.Detail, "adopted the store already recorded for this socket") {
			t.Fatalf("adoption %+v", adopted)
		}
		// Two of them: refused as ambiguous, never settled by "default".
		makeStoreIn(t, root, base, "adopted-twice", socket)
		contested, err := DiscoverStateDir("")
		must(t, err)
		if contested.Path != filepath.Join(base, scope) || !slices.Equal(contested.Ambiguous, []string{filepath.Join(base, "adopted"), filepath.Join(base, "adopted-twice")}) {
			t.Fatalf("contested %+v", contested)
		}

		// The default socket's own directory holds a store: it wins over every other.
		makeStoreIn(t, root, base, scope, socket)
		canonical, err := DiscoverStateDir("")
		must(t, err)
		if canonical.Path != filepath.Join(base, scope) || len(canonical.Ambiguous) != 0 || strings.Contains(canonical.Detail, "kept") {
			t.Fatalf("canonical %+v", canonical)
		}
	})

	t.Run("a socket and the explicit selectors are unchanged", func(t *testing.T) {
		home, _ := ownState(t)
		t.Setenv("XDG_STATE_HOME", "")
		base := filepath.Join(home, ".local", "state", "codex-session-relay")
		socket := filepath.Join(root, "run", "given.sock")
		given, err := ResolveStateDir("", socket)
		must(t, err)
		if given.Path != filepath.Join(base, scopeOf(t, socket)) || given.Source != "home" || given.Detail != "default under "+filepath.Join(home, ".local", "state") {
			t.Fatalf("--socket selection %+v", given)
		}
		// CODEX_HOME names the default socket only; it does not move a given socket's directory.
		t.Setenv("CODEX_HOME", filepath.Join(root, "codex-other"))
		moved, err := ResolveStateDir("", socket)
		must(t, err)
		if moved.Path != given.Path || moved.Detail != given.Detail {
			t.Fatalf("CODEX_HOME moved a --socket selection: %+v then %+v", given, moved)
		}
		xdg := filepath.Join(root, "xdg")
		t.Setenv("XDG_STATE_HOME", xdg)
		byXDG, err := ResolveStateDir("", "")
		must(t, err)
		if byXDG.Source != "xdg" || filepath.Dir(byXDG.Path) != filepath.Join(xdg, "codex-session-relay") || !strings.HasPrefix(byXDG.Detail, "XDG_STATE_HOME="+xdg+"; scoped by the default Codex App Server socket ") {
			t.Fatalf("xdg selection %+v", byXDG)
		}
		t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(root, "env"))
		byEnv, err := ResolveStateDir("", "")
		must(t, err)
		if byEnv.Source != "env" || byEnv.Path != filepath.Join(root, "env") || byEnv.SocketScope != "" || byEnv.DefaultSocket != "" || byEnv.Detail != "CODEX_SESSION_RELAY_STATE="+filepath.Join(root, "env") {
			t.Fatalf("env selection %+v", byEnv)
		}
		byFlag, err := ResolveStateDir(filepath.Join(root, "flag"), socket)
		must(t, err)
		if byFlag.Source != "flag" || byFlag.Path != filepath.Join(root, "flag") || byFlag.SocketScope != "" || byFlag.DefaultSocket != "" {
			t.Fatalf("flag selection %+v", byFlag)
		}
	})
}

// unstampedStore is a relay database with the full schema and no ownership stamp, the store a
// relay that predates the fence left: no ownership key, no mirror and no write gate beside it.
// It is written with VACUUM INTO from a store made elsewhere, so no lock file is ever removed.
func unstampedStore(t *testing.T) string {
	t.Helper()
	source, err := fixtureOpen(t.Context(), filepath.Join(stateDir(t), "relay.sqlite3"), "")
	must(t, err)
	_, err = source.DB.Exec("DELETE FROM schema_meta WHERE key IN ('writer_protocol','owner','owner_epoch','takeover_id','rollback_allowed','python_compatibility_build')")
	must(t, err)
	target := filepath.Join(stateDir(t), "relay.sqlite3")
	_, err = source.DB.Exec("VACUUM INTO ?", target)
	must(t, err)
	must(t, source.Close())
	entries, err := os.ReadDir(filepath.Dir(target))
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("an unstamped store's directory holds %v", entries)
	}
	return target
}

// A store with no ownership stamp is refused in plain words, the reason and every other field
// unchanged, by the doctor's write probe, a writable open and a registration hold.
func TestAnUnstampedStoreIsRefusedInPlainWords(t *testing.T) {
	t.Parallel()
	path := unstampedStore(t)
	want := "store_owned_by_other: " + UnstampedStoreDetail

	probed := Probe(t.Context(), StateSelection{Path: filepath.Dir(path)})
	if !probed.Access.DBReadable || probed.Access.DBWritable || probed.Access.Detail != "database write probe failed: "+want {
		t.Fatalf("probe access %+v", probed.Access)
	}

	_, err := Open(t.Context(), path, "")
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" || refused.Detail != UnstampedStoreDetail {
		t.Fatalf("writable open: %v", err)
	}
	// The stamp's own refusal stays reachable beneath the words.
	if !strings.Contains(errors.Unwrap(err).Error(), "durable ownership missing") {
		t.Fatalf("writable open's cause: %v", errors.Unwrap(err))
	}

	err = RegistrationHold(t.Context(), path, func(*sql.Conn, string) error {
		t.Fatal("a hold was taken on an unstamped store")
		return nil
	})
	if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" || refused.Detail != UnstampedStoreDetail {
		t.Fatalf("registration hold: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("a refusal changed the store's directory: %v", entries)
	}
}

// A store that carries a stamp keeps the gate's own words, and a store this runtime owns is
// probed writable: the plain words are for an unstamped store only.
func TestOnlyAnUnstampedStoreGetsThePlainWords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	owned := filepath.Join(stateDir(t), "relay.sqlite3")
	s, err := fixtureOpen(ctx, owned, "")
	must(t, err)
	must(t, s.Close())
	if probed := Probe(ctx, StateSelection{Path: filepath.Dir(owned)}); !probed.Access.DBWritable || probed.Access.Detail != "" {
		t.Fatalf("owned store access %+v", probed.Access)
	}
	if unstampedAt(ctx, owned) {
		t.Fatal("a stamped store read as unstamped")
	}
	// A stamp without its write gate (a copy of a fenced store) is not unstamped: the gate's
	// failure is named as before.
	copied := filepath.Join(stateDir(t), "relay.sqlite3")
	s, err = fixtureOpen(ctx, owned, "")
	must(t, err)
	_, err = s.DB.Exec("VACUUM INTO ?", copied)
	must(t, err)
	must(t, s.Close())
	if err := writeGateRefusal(os.ErrNotExist, unstampedAt(ctx, copied)); err.Error() != "store_owned_by_other: ownership refused: write gate: file does not exist" {
		t.Fatalf("a stamped store's missing gate: %v", err)
	}
}

// The relay's default socket is the one the bridge defaults to (internal/bridge/mcp Defaults),
// with CODEX_HOME unset, empty and set, and the home that stands in for an unset or empty
// CODEX_HOME read alike: HOME set, empty (the root), or absent (this user's passwd entry). The
// store a command given no --socket selects is the one a relay service started on the bridge's
// socket serves. Nothing here opens a path: both only compute it.
func TestTheDefaultSocketIsTheBridges(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	type variable struct {
		set   bool
		value string
	}
	for _, c := range []struct{ home, codexHome variable }{
		{variable{true, "/h"}, variable{false, ""}},
		{variable{true, "/h"}, variable{true, ""}},
		{variable{true, "/h"}, variable{true, "/c"}},
		{variable{true, "/h//"}, variable{true, ""}},
		{variable{true, ""}, variable{true, ""}},
		{variable{false, ""}, variable{true, ""}},
		{variable{false, ""}, variable{false, ""}},
	} {
		env := map[string]string{}
		for name, v := range map[string]variable{"HOME": c.home, "CODEX_HOME": c.codexHome} {
			t.Setenv(name, v.value)
			if v.set {
				env[name] = v.value
			} else {
				must(t, os.Unsetenv(name))
			}
		}
		relay, err := DefaultSocket()
		must(t, err)
		if bridge, _ := mcp.Defaults(env); bridge != relay {
			t.Fatalf("HOME %+v, CODEX_HOME %+v: the bridge defaults to %s, the relay to %s", c.home, c.codexHome, bridge, relay)
		}
	}
}
