package role

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

// nativeHome writes a Codex home below dir whose native model list holds the given IDs and returns the
// home directory.
func nativeHome(t *testing.T, dir string, ids ...string) string {
	t.Helper()
	codex := filepath.Join(dir, ".codex")
	check(t, os.MkdirAll(codex, 0700))
	models := make([]map[string]any, len(ids))
	for i, id := range ids {
		models[i] = map[string]any{"id": id}
	}
	check(t, os.WriteFile(filepath.Join(codex, "models_cache.json"), must(json.Marshal(map[string]any{"models": models})), 0600))
	return dir
}

// sharedHomeOptions is two sessions of one CRW_HOME that differ only in HOME, CODEX_HOME unset: the
// native catalog each of them reads is the one below its own HOME. No ocx exists on PATH, so the
// reader falls back to the native catalog.
func sharedHomeOptions(t *testing.T, crw, home string) CatalogOptions {
	t.Helper()
	return CatalogOptions{
		Environ: []string{"HOME=" + home, "CRW_HOME=" + crw, "PATH=" + filepath.Join(crw, "bin"), "TMPDIR=" + crw},
		Now:     func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
}

// CRW-1132 (A5-07): the cache and the pending requests are keyed by the native home and catalog the
// reader resolves, not by the raw CODEX_HOME text, so a HOME that selects another native catalog is
// another source.
func TestLiveCatalogKeyFollowsTheResolvedNativeHome(t *testing.T) {
	root := t.TempDir()
	crw := filepath.Join(root, "crw")
	one, two := nativeHome(t, filepath.Join(root, "one"), "home-one"), nativeHome(t, filepath.Join(root, "two"), "home-two")
	a, b := sharedHomeOptions(t, crw, one), sharedHomeOptions(t, crw, two)
	var r CatalogReader
	if c := liveRead(t, &r, a); c.Status != "fresh" || len(c.Entries) != 1 || c.Entries[0].ID != "home-one" {
		t.Fatalf("first home: %+v", c)
	}
	if c := liveRead(t, &r, b); c.Status != "fresh" || len(c.Entries) != 1 || c.Entries[0].ID != "home-two" {
		t.Fatalf("a changed HOME returned the other home's list as fresh: %+v", c)
	}
	if c := liveRead(t, &r, a); len(c.Entries) != 1 || c.Entries[0].ID != "home-one" {
		t.Fatalf("back to the first home: %+v", c)
	}
}

// The pending request of one source is never joined by a read of another source.
func TestLiveCatalogPendingDoesNotMergeAcrossHomes(t *testing.T) {
	root := t.TempDir()
	crw := filepath.Join(root, "crw")
	one, two := nativeHome(t, filepath.Join(root, "one"), "home-one"), nativeHome(t, filepath.Join(root, "two"), "home-two")
	synctest.Test(t, func(t *testing.T) {
		var r CatalogReader
		release := make(chan struct{})
		calls := 0
		run := func([]string) (string, error) { calls++; <-release; return "", os.ErrNotExist }
		a, b := sharedHomeOptions(t, crw, one), sharedHomeOptions(t, crw, two)
		a.RunOcx, b.RunOcx = run, run
		a.ForceRefresh, b.ForceRefresh = true, true
		results := make(chan LiveCatalog, 2)
		go func() { results <- liveRead(t, &r, a) }()
		synctest.Wait()
		go func() { results <- liveRead(t, &r, b) }()
		synctest.Wait()
		failed := calls != 2
		if failed {
			t.Errorf("a read of another home joined the pending request: %d discovery runs", calls)
		}
		close(release)
		if failed {
			return
		}
		got := map[string]bool{}
		for range 2 {
			c := <-results
			got[c.Entries[0].ID] = true
		}
		if !reflect.DeepEqual(got, map[string]bool{"home-one": true, "home-two": true}) {
			t.Fatalf("each read must answer its own home: %v", got)
		}
	})
}

// A HOME that changes nothing the reader resolves keeps the cache: with CODEX_HOME, the catalog path and
// OPENCODEX_HOME all named, HOME selects nothing.
func TestLiveCatalogKeyKeepsTheCacheWhenHomeSelectsNothing(t *testing.T) {
	o := liveOptions(t)
	o.Environ = append(o.Environ, "OPENCODEX_HOME="+t.TempDir())
	calls := 0
	o.RunOcx = func([]string) (string, error) { calls++; return "[]", nil }
	var r CatalogReader
	liveRead(t, &r, o)
	other := o
	other.Environ = append(append([]string{}, o.Environ...), "HOME="+t.TempDir())
	liveRead(t, &r, other)
	if calls != 1 {
		t.Fatalf("an unrelated HOME refreshed the cache: %d discovery runs", calls)
	}
}

// The OCX the reader runs is part of its identity: with PATH text unchanged, another executable found on
// it is another source.
func TestLiveCatalogKeyFollowsTheResolvedOcx(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, dir := range []string{first, second} {
		check(t, os.MkdirAll(dir, 0700))
		check(t, os.WriteFile(filepath.Join(dir, "ocx"), []byte("#!/bin/sh\n"), 0700))
	}
	env := catalogEnv([]string{"PATH=" + first + ":" + second, "HOME=" + root, "CODEX_HOME=" + filepath.Join(root, "codex"), "OPENCODEX_HOME=" + filepath.Join(root, "ocx-home")})
	before := sourceKey(env)
	check(t, os.Remove(filepath.Join(first, "ocx")))
	if sourceKey(env) == before {
		t.Fatal("the same PATH text now finds another ocx executable but keeps its key")
	}
}

// A relative native catalog path or Codex home is read relative to the process directory, so the key
// names the file the reader opens, not the relative text: the same text in two directories is two sources.
func TestLiveCatalogKeyFollowsTheAbsoluteNativeSource(t *testing.T) {
	for _, c := range []struct {
		name string
		env  func(root string) []string
		file func(dir string) string
	}{
		{"catalog-path", func(root string) []string {
			return []string{"CODEX_MODELS_CACHE_PATH=models.json", "CODEX_HOME=" + filepath.Join(root, "codex"), "OPENCODEX_HOME=" + filepath.Join(root, "ocx-home")}
		}, func(dir string) string { return filepath.Join(dir, "models.json") }},
		{"codex-home", func(root string) []string {
			return []string{"CODEX_HOME=codex", "OPENCODEX_HOME=" + filepath.Join(root, "ocx-home")}
		}, func(dir string) string { return filepath.Join(dir, "codex", "models_cache.json") }},
		{"opencodex-home", func(root string) []string {
			return []string{"OPENCODEX_HOME=ocx", "CODEX_HOME=" + filepath.Join(root, "codex")}
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			crw := filepath.Join(root, "crw")
			dirs := []string{filepath.Join(root, "one"), filepath.Join(root, "two")}
			for i, dir := range dirs {
				check(t, os.MkdirAll(dir, 0700))
				if c.file != nil {
					check(t, os.MkdirAll(filepath.Dir(c.file(dir)), 0700))
					check(t, os.WriteFile(c.file(dir), must(json.Marshal(map[string]any{"models": []any{map[string]any{"id": []string{"home-one", "home-two"}[i]}}})), 0600))
				}
			}
			environ := append([]string{"HOME=" + root, "CRW_HOME=" + crw, "PATH=" + filepath.Join(crw, "bin"), "TMPDIR=" + crw}, c.env(root)...)
			keys := make([]string, 2)
			for i, dir := range dirs {
				t.Chdir(dir)
				keys[i] = sourceKey(catalogEnv(environ))
			}
			if keys[0] == keys[1] {
				t.Fatal("the same relative text in two directories shares a key")
			}
			if c.file == nil {
				return
			}
			var r CatalogReader
			o := CatalogOptions{Environ: environ, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
			for i, dir := range dirs {
				t.Chdir(dir)
				if got := liveRead(t, &r, o); got.Status != "fresh" || len(got.Entries) != 1 || got.Entries[0].ID != []string{"home-one", "home-two"}[i] {
					t.Fatalf("directory %d answered %+v", i, got)
				}
			}
		})
	}
}

// A cache a writer could not have written is not fresh: the reader discovers again instead.
func TestLiveCatalogRejectsCachesAWriterCannotProduce(t *testing.T) {
	for _, c := range []struct{ name, patch string }{
		{"numeric-fetchedAt", `{"fetchedAt":1767225600000}`},
		{"date-only-fetchedAt", `{"fetchedAt":"2026-01-01"}`},
		{"offset-fetchedAt", `{"fetchedAt":"2026-01-01T00:00:00.000+00:00"}`},
		{"no-millis-fetchedAt", `{"fetchedAt":"2026-01-01T00:00:00Z"}`},
		{"state-null", `{"state":null}`},
		{"state-number", `{"state":7}`},
		{"state-of-the-other-source", `{"state":"native-catalog"}`},
		{"state-unavailable", `{"state":"unavailable"}`},
		{"state-unsupported", `{"state":"unsupported-ocx-catalog"}`},
		{"blank-id", `{"entries":[{"id":" ","source":"ocx","label":"x","reasoningEfforts":null}]}`},
		{"empty-id", `{"entries":[{"id":"","source":"ocx","label":"x","reasoningEfforts":null}]}`},
		{"blank-label", `{"entries":[{"id":"x","source":"ocx","label":" \t","reasoningEfforts":null}]}`},
		{"empty-label", `{"entries":[{"id":"x","source":"ocx","label":"","reasoningEfforts":null}]}`},
		{"duplicate-id", `{"entries":[{"id":"x","source":"ocx","label":"x","reasoningEfforts":null},{"id":"x","source":"ocx","label":"y","reasoningEfforts":null}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := liveOptions(t)
			path := livePath(o)
			base := map[string]any{"state": "ocx-active", "entries": []any{map[string]any{"id": "x", "source": "ocx", "label": "x", "reasoningEfforts": nil}}, "status": "fresh", "source": "ocx", "fetchedAt": "2026-01-01T00:00:00.000Z"}
			var patch map[string]any
			check(t, json.Unmarshal([]byte(c.patch), &patch))
			for k, v := range patch {
				base[k] = v
			}
			check(t, os.MkdirAll(filepath.Dir(path), 0700))
			check(t, os.WriteFile(path, must(json.Marshal(map[string]any{"key": sourceKey(catalogEnv(o.Environ)), "catalog": base})), 0600))
			calls := 0
			o.RunOcx = func([]string) (string, error) { calls++; return liveRoster("discovered"), nil }
			var r CatalogReader
			got := liveRead(t, &r, o)
			if calls != 1 || got.Status != "fresh" || got.Entries[0].ID != "discovered" {
				t.Fatalf("the invalid cache was returned as fresh (%d runs): %+v", calls, got)
			}
			// The refresh replaced the cache with one a writer produced, which now serves within the TTL.
			again := liveRead(t, &r, o)
			if calls != 1 || again.Entries[0].ID != "discovered" {
				t.Fatalf("the refreshed cache was not reused (%d runs): %+v", calls, again)
			}
		})
	}
}

// A cache whose members a newer writer extended keeps them in the fresh answer, and a failed refresh still
// shows the last successful list as stale, which routes nothing.
func TestLiveCatalogKeepsExtensionFieldsAndShowsStaleWithoutAuthority(t *testing.T) {
	o := liveOptions(t)
	path := livePath(o)
	base := map[string]any{"state": "native-catalog", "entries": []any{map[string]any{"id": "x", "source": "native", "label": "x", "reasoningEfforts": []any{"high"}, "vendor": "v"}}, "status": "fresh", "source": "native", "fetchedAt": "2026-01-01T00:00:00.000Z", "extra": map[string]any{"n": 7}}
	check(t, os.MkdirAll(filepath.Dir(path), 0700))
	check(t, os.WriteFile(path, must(json.Marshal(map[string]any{"key": sourceKey(catalogEnv(o.Environ)), "catalog": base})), 0600))
	o.RunOcx = func([]string) (string, error) { t.Fatal("a valid cache was refreshed"); return "", nil }
	var r CatalogReader
	catalogJSON(t, liveRead(t, &r, o), must(json.Marshal(base)))
	o.ForceRefresh = true
	o.RunOcx = func([]string) (string, error) { return "", errors.New("down") }
	stale := liveRead(t, &r, o)
	if stale.Status != "stale" || len(stale.Entries) != 1 || stale.Entries[0].ID != "x" {
		t.Fatalf("stale list not shown: %+v", stale)
	}
	if CatalogIsAuthoritative(stale, o.Now(), catalogEnv(o.Environ)) {
		t.Fatal("a stale list proved routing")
	}
}
