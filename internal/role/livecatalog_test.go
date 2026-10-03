package role

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func liveOptions(t *testing.T) CatalogOptions {
	t.Helper()
	root := t.TempDir()
	return CatalogOptions{Environ: []string{"HOME=" + root, "CODEX_HOME=" + filepath.Join(root, "codex"), "CRW_HOME=" + filepath.Join(root, "crw"), "PATH=" + filepath.Join(root, "bin"), "TMPDIR=" + root}, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
}

func liveRead(t *testing.T, r *CatalogReader, o CatalogOptions) LiveCatalog {
	t.Helper()
	c, err := r.ReadCatalog(o)
	check(t, err)
	return c
}

func livePath(o CatalogOptions) string {
	p, _ := StorePath(catalogEnv(o.Environ))
	return filepath.Join(filepath.Dir(p), "model-catalog.json")
}

func liveRoster(id string) string {
	return `[{"namespaced":` + strconv.Quote(id) + `,"native":true,"reasoningEfforts":["low","high"]},{"disabled":true},{"initialSelectionPending":true},{"namespaced":"empty","reasoningEfforts":[]},{"namespaced":"unknown","reasoningEfforts":null}]`
}

// Inputs and expectations were recorded from the pinned live-catalog.ts parser.
func TestLiveCatalogParser(t *testing.T) {
	for _, c := range []struct{ input, want, err string }{
		{`{}`, ``, "invalid OCX catalog"}, {`[{}]`, ``, "invalid OCX model id"},
		{`[null]`, ``, "invalid OCX model row"}, {`[[]]`, ``, "invalid OCX model row"}, {`[1]`, ``, "invalid OCX model row"},
		{`[]`, `[]`, ""}, {`[{"disabled":true}]`, `[]`, ""}, {`[{"initialSelectionPending":true}]`, `[]`, ""},
		{`[{"id":"x"}]`, ``, "invalid OCX model id"}, {`[{"namespaced":"","id":"fallback","native":true}]`, ``, "invalid OCX model id"},
		{`[{"namespaced":"x","id":"ignored","native":true},{"namespaced":"x","displayName":"duplicate"}]`, `[{"id":"x","source":"native","label":"x","reasoningEfforts":null}]`, ""},
		{`[{"namespaced":" x ","displayName":" Label ","reasoningEfforts":[" high ","",{"effort":"low"}," high ",null]}]`, `[{"id":" x ","source":"ocx","label":" Label  ( x )","reasoningEfforts":[" high ","low"]}]`, ""},
		{`[{"namespaced":"x","reasoningEfforts":[]},{"namespaced":"y","reasoningEfforts":null},{"namespaced":"z"}]`, `[{"id":"x","source":"ocx","label":"x","reasoningEfforts":[]},{"id":"y","source":"ocx","label":"y","reasoningEfforts":null},{"id":"z","source":"ocx","label":"z","reasoningEfforts":null}]`, ""},
		{`[{"namespaced":"x","disabled":"true","initialSelectionPending":1}]`, `[{"id":"x","source":"ocx","label":"x","reasoningEfforts":null}]`, ""},
	} {
		t.Run(c.input, func(t *testing.T) {
			got, err := ParseOcxModels(c.input)
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("got %v, want %s", err, c.err)
				}
				return
			}
			check(t, err)
			catalogJSON(t, got, json.RawMessage(c.want))
		})
	}
	if _, err := ParseOcxModels("not JSON"); err == nil {
		t.Fatal("accepted malformed JSON")
	}
}

func TestLiveCatalogCache(t *testing.T) {
	o := liveOptions(t)
	now, calls, current := o.Now(), 0, "first"
	o.Now = func() time.Time { return now }
	o.RunOcx = func([]string) (string, error) { calls++; return liveRoster(current), nil }
	var r CatalogReader
	if c := liveRead(t, &r, o); c.Entries[0].ID != "first" || c.Status != "fresh" {
		t.Fatal(c)
	}
	liveRead(t, &r, o)
	if calls != 1 {
		t.Fatal(calls)
	}
	cmd := exec.Command(must(os.Executable()), "-test.run=^TestLiveCatalogProcess$")
	cmd.Env = append(os.Environ(), "CRW_LIVE_TEST_MODE=cache", "CRW_LIVE_TEST_ENV="+string(must(json.Marshal(o.Environ))))
	var child LiveCatalog
	check(t, json.Unmarshal(must(cmd.Output()), &child))
	if child.Status != "fresh" || child.Entries[0].ID != "first" {
		t.Fatal(child)
	}
	current, o.ForceRefresh = "second", true
	if c := liveRead(t, &r, o); c.Entries[0].ID != "second" {
		t.Fatal(c)
	}
	o.ForceRefresh = false
	now = now.Add(29999 * time.Millisecond)
	liveRead(t, &r, o)
	if calls != 2 {
		t.Fatal("refreshed before TTL", calls)
	}
	now = now.Add(time.Millisecond)
	o.RunOcx = func([]string) (string, error) { return "", errors.New("secret stderr") }
	stale := liveRead(t, &r, o)
	if stale.Status != "stale" || stale.Entries[0].ID != "second" || strings.Contains(stale.Message, "secret") {
		t.Fatal(stale)
	}
	o.ForceRefresh = true
	o.RunOcx = func([]string) (string, error) { return "[]", nil }
	if c := liveRead(t, &r, o); c.Status != "fresh" || len(c.Entries) != 0 {
		t.Fatal(c)
	}
	o.ForceRefresh = false
	if c := liveRead(t, &r, o); len(c.Entries) != 0 {
		t.Fatal(c)
	}
	check(t, os.Remove(livePath(o)))
	o.RunOcx = func([]string) (string, error) { return "not JSON", nil }
	if c := liveRead(t, &r, o); c.Status != "unavailable" || len(c.Entries) != 0 || c.FetchedAt != nil {
		t.Fatal(c)
	}
}

func TestLiveCatalogCoalesces(t *testing.T) {
	o := liveOptions(t)
	synctest.Test(t, func(t *testing.T) {
		var r CatalogReader
		calls := 0
		release := make(chan struct{})
		o.ForceRefresh = true
		o.RunOcx = func([]string) (string, error) { calls++; <-release; return "[]", nil }
		results := make(chan LiveCatalog, 2)
		go func() { results <- liveRead(t, &r, o) }()
		synctest.Wait()
		go func() { results <- liveRead(t, &r, o) }()
		synctest.Wait()
		if calls != 1 {
			t.Fatal("did not join pending request", calls)
		}
		close(release)
		a, b := <-results, <-results
		if !reflect.DeepEqual(a, b) {
			t.Fatal(a, b)
		}
		liveRead(t, &r, o)
		if calls != 2 {
			t.Fatal("pending request not retired", calls)
		}
	})
}

func TestLiveCatalogNativeFallback(t *testing.T) {
	o := liveOptions(t)
	env := catalogEnv(o.Environ)
	codex, _ := env("CODEX_HOME")
	check(t, os.MkdirAll(codex, 0700))
	check(t, os.WriteFile(filepath.Join(codex, "config.toml"), []byte("model_catalog_json = 'custom.json' # selected\n[profile]\nmodel_catalog_json = 'wrong.json'\n"), 0600))
	check(t, os.WriteFile(filepath.Join(codex, "custom.json"), []byte(`{"models":[{"slug":"gpt-future","supported_reasoning_levels":[{"effort":"high"}]},{"slug":"hidden","visibility":"hide"}]}`), 0600))
	check(t, os.WriteFile(filepath.Join(codex, "models_cache.json"), []byte(`{"models":["explicit"]}`), 0600))
	var r CatalogReader
	c := liveRead(t, &r, o) // No ocx exists in this test's PATH.
	if c.Source != ModelNative || c.Entries[0].ID != "gpt-future" || !reflect.DeepEqual(*c.Entries[0].ReasoningEfforts, []string{"high"}) {
		t.Fatal(c)
	}
	o.Environ = append(o.Environ, "CODEX_MODELS_CACHE_PATH="+filepath.Join(codex, "models_cache.json"))
	if c := liveRead(t, &r, o); c.Entries[0].ID != "explicit" {
		t.Fatal(c)
	}
	o.ForceRefresh = true
	o.RunOcx = func([]string) (string, error) { return "", errors.New("OCX failed") }
	c = liveRead(t, &r, o)
	if c.Status != "stale" || c.Source != ModelNative || !strings.HasPrefix(c.Message, "OCX model discovery failed.") {
		t.Fatal(c)
	}
	check(t, os.Remove(livePath(o)))
	if c := liveRead(t, &r, o); c.Source != ModelOcx || c.Status != "unavailable" {
		t.Fatal(c)
	}
	o.RunOcx = func([]string) (string, error) { return "", os.ErrNotExist }
	o.ReadNative = func(_ host.LookupEnv) []CatalogEntry { return []CatalogEntry{} }
	if c := liveRead(t, &r, o); c.Status != "fresh" || c.State != CatalogNative || len(c.Entries) != 0 {
		t.Fatal(c)
	}
	check(t, os.Remove(livePath(o)))
	o.ReadNative = func(_ host.LookupEnv) []CatalogEntry { return nil }
	if c := liveRead(t, &r, o); c.Status != "unavailable" || c.Source != ModelNative {
		t.Fatal(c)
	}
}

func TestLiveCatalogValidation(t *testing.T) {
	for _, c := range []struct {
		name, patch string
		valid       bool
	}{
		{"fresh", `{}`, true}, {"future1000", `{"fetchedAt":"2026-01-01T00:00:01.000Z"}`, true},
		{"future1001", `{"fetchedAt":"2026-01-01T00:00:01.001Z"}`, false}, {"date-only", `{"fetchedAt":"2026-01-01"}`, true},
		{"bad-date", `{"fetchedAt":"invalid"}`, false}, {"bad-source", `{"source":"other"}`, false}, {"stale", `{"status":"stale"}`, false},
		{"null-entries", `{"entries":null}`, false}, {"empty", `{"entries":[]}`, true},
		{"blank-id", `{"entries":[{"id":" ","source":"ocx","label":"","reasoningEfforts":[""]}]}`, true},
		{"missing-efforts", `{"entries":[{"id":"x","source":"ocx","label":"x"}]}`, false},
		{"invalid-efforts", `{"entries":[{"id":"x","source":"ocx","label":"x","reasoningEfforts":[1]}]}`, false},
		{"missing-state", `{"state":null,"extra":7}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := liveOptions(t)
			env := catalogEnv(o.Environ)
			path := livePath(o)
			base := map[string]any{"state": "ocx-active", "entries": []any{map[string]any{"id": "x", "source": "ocx", "label": "x", "reasoningEfforts": nil}}, "status": "fresh", "source": "ocx", "fetchedAt": "2026-01-01T00:00:00.000Z"}
			var patch map[string]any
			check(t, json.Unmarshal([]byte(c.patch), &patch))
			for k, v := range patch {
				base[k] = v
			}
			if c.name == "missing-state" {
				delete(base, "state")
			}
			check(t, os.MkdirAll(filepath.Dir(path), 0700))
			check(t, os.WriteFile(path, must(json.Marshal(map[string]any{"key": sourceKey(env), "catalog": base})), 0600))
			calls := 0
			o.RunOcx = func([]string) (string, error) { calls++; return "", errors.New("failure") }
			var r CatalogReader
			got := liveRead(t, &r, o)
			if c.valid {
				if calls != 0 {
					t.Fatal("rejected cache", got)
				}
				catalogJSON(t, got, must(json.Marshal(base)))
			} else if calls != 1 || got.Status != "unavailable" {
				t.Fatal("accepted invalid cache", got, calls)
			}
		})
	}
}

func TestLiveCatalogSourceIdentityAndClock(t *testing.T) {
	o := liveOptions(t)
	calls := 0
	o.RunOcx = func([]string) (string, error) { calls++; return "[]", nil }
	var r CatalogReader
	liveRead(t, &r, o)
	for _, key := range []string{"CODEX_HOME", "CODEX_MODELS_CACHE_PATH", "PATH", "OPENCODEX_HOME"} {
		changed := o
		changed.Environ = append(append([]string{}, o.Environ...), key+"=changed")
		liveRead(t, &r, changed)
	}
	if calls != 5 {
		t.Fatal(calls)
	}
	if sourceKey(catalogEnv([]string{"PATH=", "Path=other"})) != sourceKey(catalogEnv([]string{"PATH="})) {
		t.Fatal("empty PATH fell through")
	}
	if sourceKey(catalogEnv([]string{"Path=other"})) == sourceKey(catalogEnv([]string{"PATH="})) {
		t.Fatal("Path not used")
	}
	o.ForceRefresh = false
	ticks := 0
	now := o.Now()
	o.Now = func() time.Time { ticks++; return now.Add(time.Duration(ticks) * time.Millisecond) }
	liveRead(t, &r, o)
	// Last cache has a different source key: validation now(), then successful-fetch now().
	if ticks != 2 {
		t.Fatal("clock call order", ticks)
	}
	liveRead(t, &r, o)
	if ticks != 4 {
		t.Fatal("TTL clock calls", ticks)
	}
}

func TestLiveCatalogPersistence(t *testing.T) {
	o := liveOptions(t)
	o.RunOcx = func([]string) (string, error) { return "[]", nil }
	var r CatalogReader
	c := liveRead(t, &r, o)
	path := livePath(o)
	old := must(os.ReadFile(path))
	if !strings.HasSuffix(string(old), "\n") || must(os.Stat(path)).Mode().Perm() != 0600 || must(os.Stat(filepath.Dir(path))).Mode().Perm() != 0700 {
		t.Fatal("cache modes/format")
	}
	if persistCatalog(path, sourceKey(catalogEnv(o.Environ)), c, func(string, string) error { return errors.New("rename failed") }) {
		t.Fatal("reported save")
	}
	if !reflect.DeepEqual(old, must(os.ReadFile(path))) {
		t.Fatal("previous cache lost")
	}
	if entries := must(os.ReadDir(filepath.Dir(path))); len(entries) != 1 {
		t.Fatal("temp leaked", entries)
	}
	if runtime.GOOS != "windows" {
		target := filepath.Join(filepath.Dir(path), "target")
		check(t, os.WriteFile(target, []byte("keep"), 0600))
		check(t, os.Remove(path))
		check(t, os.Symlink(target, path))
		o.ForceRefresh = true
		liveRead(t, &r, o)
		if must(os.Lstat(path)).Mode()&os.ModeSymlink != 0 || string(must(os.ReadFile(target))) != "keep" {
			t.Fatal("did not replace link only")
		}
	}
	check(t, os.Remove(path))
	check(t, os.Mkdir(path, 0700))
	o.ForceRefresh = true
	if c := liveRead(t, &r, o); c.Status != "fresh" || c.Message != "Model list loaded; its cache could not be saved." {
		t.Fatal(c)
	}
}

func TestLiveCatalogSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable")
	}
	o := liveOptions(t)
	bin, _ := catalogEnv(o.Environ)("PATH")
	check(t, os.MkdirAll(bin, 0700))
	program := "#!/bin/sh\nexec \"$CRW_LIVE_TEST_EXE\" -test.run=^TestLiveCatalogProcess$ -- \"$@\"\n"
	check(t, os.WriteFile(filepath.Join(bin, "ocx"), []byte(program), 0700))
	for _, mode := range []string{"ok", "exact", "stdout", "stderr", "exit", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "pid")
			env := append(append([]string{}, o.Environ...), "CRW_LIVE_TEST_MODE="+mode, "CRW_LIVE_TEST_EXE="+must(os.Executable()), "CRW_LIVE_TEST_PID="+pidfile)
			start := time.Now()
			out, err := RunOcxModels(env)
			if mode == "ok" || mode == "exact" {
				check(t, err)
				if mode == "ok" && out != "[]" || mode == "exact" && len(out) != 4*1024*1024 {
					t.Fatal(len(out))
				}
			} else if err == nil {
				t.Fatal("subprocess bound/failure accepted")
			}
			if mode == "timeout" && (time.Since(start) < 11*time.Second || time.Since(start) >= 18*time.Second) {
				t.Fatal("timeout outside oracle bounds")
			}
			pid := must(strconv.Atoi(strings.TrimSpace(string(must(os.ReadFile(pidfile))))))
			if syscall.Kill(pid, 0) != syscall.ESRCH {
				t.Fatal("own fake process not reaped", pid)
			}
		})
	}
	first := t.TempDir()
	check(t, os.WriteFile(filepath.Join(first, "ocx"), []byte("denied"), 0600))
	base := append(o.Environ, "CRW_LIVE_TEST_MODE=ok", "CRW_LIVE_TEST_EXE="+must(os.Executable()), "CRW_LIVE_TEST_PID="+filepath.Join(t.TempDir(), "pid"))
	if _, err := RunOcxModels(append(append([]string{}, base...), "PATH="+first)); !errors.Is(err, os.ErrPermission) {
		t.Fatal("EACCES became missing", err)
	}
	if out, err := RunOcxModels(append(append([]string{}, base...), "PATH="+first+":"+bin)); err != nil || out != "[]" {
		t.Fatal(out, err)
	}
}

// Re-executes only this helper; the shell wrapper execs it, so the recorded pid belongs to the runner.
func TestLiveCatalogProcess(t *testing.T) {
	mode := os.Getenv("CRW_LIVE_TEST_MODE")
	if mode == "" {
		return
	}
	if mode == "cache" {
		var env []string
		check(t, json.Unmarshal([]byte(os.Getenv("CRW_LIVE_TEST_ENV")), &env))
		var r CatalogReader
		c := liveRead(t, &r, CatalogOptions{Environ: env, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }, RunOcx: func([]string) (string, error) { return "", errors.New("cache not reused") }})
		fmt.Print(string(must(Stringify(c, ""))))
		os.Exit(0)
	}
	check(t, os.WriteFile(os.Getenv("CRW_LIVE_TEST_PID"), []byte(strconv.Itoa(os.Getpid())), 0600))
	if !reflect.DeepEqual(os.Args[len(os.Args)-3:], []string{"models", "live", "--json"}) {
		os.Exit(2)
	}
	switch mode {
	case "ok":
		fmt.Print("[]")
	case "exact":
		fmt.Print(strings.Repeat(" ", 4*1024*1024))
	case "stdout":
		fmt.Print(strings.Repeat("x", 5*1024*1024))
	case "stderr":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 5*1024*1024))
	case "exit":
		os.Exit(7)
	case "timeout":
		for {
			time.Sleep(time.Hour)
		} // Simulates a hung owned process, not test synchronization.
	}
	os.Exit(0)
}
