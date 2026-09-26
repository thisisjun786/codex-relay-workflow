package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStateSelection_python_precedence_properties(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Run("test_explicit_override_wins", func(t *testing.T) {
		// Python: state_dir() with only CODEX_SESSION_RELAY_STATE set answers that directory.
		t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(root, "env"))
		got, err := ResolveStateDir("", "")
		if err != nil || got.Path != filepath.Join(root, "env") || got.Source != "env" {
			t.Fatalf("selection %+v %v", got, err)
		}
		// And a socket does not move an explicitly overridden directory.
		scoped, err := ResolveStateDir("", "/run/one.sock")
		if err != nil || scoped.Path != filepath.Join(root, "env") {
			t.Fatalf("socket-scoped override %+v %v", scoped, err)
		}
	})
	t.Run("test_a_socket_gets_its_own_endpoint_directory", func(t *testing.T) {
		a, e := DiscoverStateDir("/run/one.sock")
		if e != nil {
			t.Fatal(e)
		}
		b, e := DiscoverStateDir("/run/two.sock")
		if e != nil || a.Path == b.Path || !strings.Contains(a.Path, "codex-session-relay") {
			t.Fatalf("selections %+v %+v %v", a, b, e)
		}
	})
	t.Run("test_each_rule_wins_in_order_and_says_so", func(t *testing.T) {
		home, e := ResolveStateDir("", "/run/x.sock")
		if e != nil || home.Source != "home" || !strings.HasPrefix(home.Path, root) {
			t.Fatalf("home %+v %v", home, e)
		}
		t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
		xdg, e := ResolveStateDir("", "/run/x.sock")
		if e != nil || xdg.Source != "xdg" || !strings.Contains(xdg.Detail, "XDG_STATE_HOME=") || xdg.SocketScope != home.SocketScope {
			t.Fatalf("xdg %+v %v", xdg, e)
		}
		t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(root, "env"))
		env, e := ResolveStateDir("", "/run/x.sock")
		if e != nil || env.Source != "env" || !strings.Contains(env.Detail, "CODEX_SESSION_RELAY_STATE=") {
			t.Fatalf("env %+v %v", env, e)
		}
		flag, e := ResolveStateDir(filepath.Join(root, "flag"), "/run/x.sock")
		if e != nil || flag.Source != "flag" || !strings.Contains(flag.Detail, "--state ") || filepath.Base(flag.DBPath()) != "relay.sqlite3" {
			t.Fatalf("flag %+v %v", flag, e)
		}
	})
	t.Run("test_a_relative_flag_resolves_to_the_same_store_as_its_absolute_form", func(t *testing.T) {
		target := filepath.Join(root, "rel")
		created, e := Open(context.Background(), filepath.Join(target, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		if e := created.Close(); e != nil {
			t.Fatal(e)
		}
		t.Chdir(root)
		relativeSelection, e := ResolveStateDir("rel", "")
		if e != nil {
			t.Fatal(e)
		}
		relative := Probe(context.Background(), relativeSelection)
		absoluteSelection, e := ResolveStateDir(target, "")
		if e != nil {
			t.Fatal(e)
		}
		absolute := Probe(context.Background(), absoluteSelection)
		if relative.Store.StoreID == "" || relative.Store.StoreID != absolute.Store.StoreID || relative.Store.Inode == 0 || relative.Store.Inode != absolute.Store.Inode {
			t.Fatalf("relative %+v absolute %+v", relative.Store, absolute.Store)
		}
	})
}

func TestStateSelection_python_discovery_properties(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	base := filepath.Join(root, "state", "codex-session-relay")
	makeStore := func(t *testing.T, name, socket string) { t.Helper(); makeStoreIn(t, root, base, name, socket) }
	// ownBase gives one subtest a state root of its own, so it passes alone with -run.
	ownBase := func(t *testing.T) string {
		t.Helper()
		state := t.TempDir()
		t.Setenv("XDG_STATE_HOME", state)
		return filepath.Join(state, "codex-session-relay")
	}
	t.Run("test_a_store_already_in_use_keeps_its_directory_after_the_hash_changed", func(t *testing.T) {
		socket := filepath.Join(root, "run", "app.sock")
		canonical, e := canonicalSocket(socket)
		if e != nil {
			t.Fatal(e)
		}
		legacy := socketHash("run/app.sock")
		makeStore(t, legacy, socket)
		t.Chdir(root)
		got, e := DiscoverStateDir("run/app.sock")
		if e != nil || got.Path != filepath.Join(base, legacy) || legacy == socketHash(canonical) || !strings.Contains(got.Detail, "already using") {
			t.Fatalf("legacy %+v %v", got, e)
		}
		// An empty canonical DIRECTORY is not a canonical store and must not hide the real one.
		if e := os.MkdirAll(filepath.Join(base, socketHash(canonical)), 0o700); e != nil {
			t.Fatal(e)
		}
		again, e := DiscoverStateDir("run/app.sock")
		if e != nil || again.Path != filepath.Join(base, legacy) {
			t.Fatalf("empty canonical directory hid the store: %+v %v", again, e)
		}
	})
	t.Run("test_a_store_is_found_by_the_socket_it_recorded_not_by_its_hash", func(t *testing.T) {
		socket := filepath.Join(root, "adopt.sock")
		makeStore(t, "old-named-store", socket)
		got, e := DiscoverStateDir(socket)
		if e != nil || got.Path != filepath.Join(base, "old-named-store") || !strings.Contains(got.Detail, "adopted") {
			t.Fatalf("adoption %+v %v", got, e)
		}
	})
	t.Run("test_two_stores_claiming_one_socket_are_not_silently_chosen_between", func(t *testing.T) {
		socket := filepath.Join(root, "contested.sock")
		makeStore(t, "contender-one", socket)
		makeStore(t, "contender-two", socket)
		got, e := DiscoverStateDir(socket)
		both := []string{filepath.Join(base, "contender-one"), filepath.Join(base, "contender-two")}
		if e != nil || !slices.Equal(got.Ambiguous, both) || slices.Contains(both, got.Path) {
			t.Fatalf("ambiguity %+v %v", got, e)
		}
	})
	t.Run("test_one_store_claiming_a_socket_is_still_adopted", func(t *testing.T) {
		socket := filepath.Join(root, "sole.sock")
		makeStore(t, "sole-store", socket)
		got, e := DiscoverStateDir(socket)
		if e != nil || got.Path != filepath.Join(base, "sole-store") {
			t.Fatalf("adoption %+v %v", got, e)
		}
	})
	t.Run("test_two_stores_claiming_one_socket_do_not_produce_a_third", func(t *testing.T) {
		base := ownBase(t)
		socket := filepath.Join(root, "contested.sock")
		makeStoreIn(t, root, base, "contender-one", socket)
		makeStoreIn(t, root, base, "contender-two", socket)
		got, e := DiscoverStateDir(socket)
		both := []string{filepath.Join(base, "contender-one"), filepath.Join(base, "contender-two")}
		if e != nil || !slices.Equal(got.Ambiguous, both) || slices.Contains(both, got.Path) {
			t.Fatalf("ambiguity %+v %v", got, e)
		}
		if _, e := os.Stat(got.DBPath()); !os.IsNotExist(e) {
			t.Fatalf("third store exists: %v", e)
		}
	})
	t.Run("test_an_ordinary_selection_carries_no_ambiguity", func(t *testing.T) {
		got, e := DiscoverStateDir(filepath.Join(root, "quiet.sock"))
		if e != nil || len(got.Ambiguous) != 0 {
			t.Fatalf("selection %+v %v", got, e)
		}
		flagged, e := ResolveStateDir(filepath.Join(root, "flag"), "")
		if e != nil || len(flagged.Ambiguous) != 0 {
			t.Fatalf("flag selection %+v %v", flagged, e)
		}
	})
	t.Run("test_a_store_with_no_recorded_socket_is_never_adopted_on_a_guess", func(t *testing.T) {
		makeStore(t, "stranger", "")
		unknown := filepath.Join(root, "unknown.sock")
		canonical, e := canonicalSocket(unknown)
		if e != nil {
			t.Fatal(e)
		}
		got, e := DiscoverStateDir(unknown)
		if e != nil || got.SocketScope != socketHash(canonical) || got.Path == filepath.Join(base, "stranger") || !slices.Equal(got.Unidentified, []string{filepath.Join(base, "stranger")}) {
			t.Fatalf("stranger %+v %v", got, e)
		}
	})
	t.Run("test_a_store_recording_no_socket_blocks_creating_one_beside_it", func(t *testing.T) {
		base := ownBase(t)
		makeStoreIn(t, root, base, "stranger", "")
		got, e := DiscoverStateDir(filepath.Join(root, "new.sock"))
		if e != nil || !slices.Equal(got.Unidentified, []string{filepath.Join(base, "stranger")}) || len(got.Ambiguous) != 0 {
			t.Fatalf("unidentified %+v %v", got, e)
		}
		if _, e := os.Stat(got.DBPath()); !os.IsNotExist(e) {
			t.Fatalf("created store: %v", e)
		}
	})
	t.Run("test_an_existing_canonical_store_settles_the_question_already", func(t *testing.T) {
		socket := filepath.Join(root, "canonical.sock")
		canonical, e := canonicalSocket(socket)
		if e != nil {
			t.Fatal(e)
		}
		makeStore(t, socketHash(canonical), socket)
		got, e := DiscoverStateDir(socket)
		if e != nil || got.Path != filepath.Join(base, socketHash(canonical)) || len(got.Unidentified) != 0 {
			t.Fatalf("canonical %+v %v", got, e)
		}
	})
	t.Run("test_a_fresh_socket_uses_the_canonical_directory", func(t *testing.T) {
		socket := filepath.Join(root, "fresh.sock")
		canonical, e := canonicalSocket(socket)
		if e != nil {
			t.Fatal(e)
		}
		got, e := DiscoverStateDir(socket)
		if e != nil || got.SocketScope != socketHash(canonical) {
			t.Fatalf("fresh %+v %v", got, e)
		}
	})
	t.Run("test_two_spellings_of_one_socket_choose_the_same_default_store", func(t *testing.T) {
		socket := filepath.Join(root, "alias-target.sock")
		alias := filepath.Join(root, "alias.sock")
		if e := os.WriteFile(socket, nil, 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(socket, alias); e != nil {
			t.Fatal(e)
		}
		a, e := DiscoverStateDir(socket)
		if e != nil {
			t.Fatal(e)
		}
		b, e := DiscoverStateDir(alias)
		if e != nil || a.Path != b.Path {
			t.Fatalf("alias %+v %+v %v", a, b, e)
		}
	})
}

// makeStoreIn seeds a discovery fixture store at base/name. Open's live-state guard stays
// active, so the seed temporarily designates another isolated root under root.
func makeStoreIn(t *testing.T, root, base, name, socket string) {
	t.Helper()
	previous := os.Getenv("XDG_STATE_HOME")
	if err := os.Setenv("XDG_STATE_HOME", filepath.Join(root, "guard-root")); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("XDG_STATE_HOME", previous); err != nil {
			t.Error(err)
		}
	}()
	s, e := Open(context.Background(), filepath.Join(base, name, "relay.sqlite3"), socket)
	if e != nil {
		t.Fatal(e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
}
