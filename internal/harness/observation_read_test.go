package harness

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The reader's cases over records the Go writer (RecordInvocation) leaves; the oracle's own recordings
// replay in internal/runtime/doctor/harness_hooks_test.go, where testdata/harness/hooks/oracle.json lives.

const readerManifest = `{"name":"crw","version":"1.2.3"}`

func readerQuery(home, plugin string) ObservationQuery {
	return ObservationQuery{PluginRoot: plugin, CodexHome: home, SessionID: "s1", NowMS: time.Now().UnixMilli() + 1000, MaxAgeMS: HookObservationMaxAgeMS}
}

func recordFor(t *testing.T, env map[string]string, raw, event string) {
	t.Helper()
	if !RecordInvocation(raw, "pabcd-state", event, lookup(env)) {
		t.Fatalf("%s not recorded", event)
	}
}

func TestReadHookObservationsReadsWhatRecordInvocationWrites(t *testing.T) {
	env, home, plugin := hookEnv(t)
	recordFor(t, env, `{"session_id":"s1"}`, "stop")
	recordFor(t, env, `{"session_id":"s1"}`, "session-start")
	recordFor(t, env, `{"session_id":"s1","agent_id":"child-a","agent_type":"worker"}`, "stop")
	got := ReadHookObservations(readerQuery(home, plugin))
	if got.Reason != "" || got.Ignored != 0 || len(got.Observations) != 2 || got.Observations[0].Event != "session-start" || got.Observations[1].Event != "stop" {
		t.Fatalf("root: %+v", got)
	}
	o := got.Observations[0]
	if o.SessionID != "s1" || o.AgentID != nil || o.Component != "pabcd-state" || o.Entrypoint != Entrypoint || o.Outcome != "invoked" || len(o.ObservedAt) != len("2006-01-02T15:04:05.000Z") {
		t.Errorf("projection: %+v", o)
	}
	child, other := "child-a", "child-b"
	q := readerQuery(home, plugin)
	q.AgentID = &child
	if got := ReadHookObservations(q); len(got.Observations) != 1 || got.Observations[0].AgentID == nil || *got.Observations[0].AgentID != child {
		t.Errorf("child: %+v", got)
	}
	q.AgentID = &other
	if got := ReadHookObservations(q); len(got.Observations) != 0 || got.Reason != "no invocation records" {
		t.Errorf("another actor: %+v", got)
	}
	q = readerQuery(home, plugin)
	q.SessionID = "s2"
	if got := ReadHookObservations(q); len(got.Observations) != 0 || got.Reason != "no invocation records" {
		t.Errorf("another session: %+v", got)
	}
}

func TestReadHookObservationsRefusesWhatItCannotTrust(t *testing.T) {
	manifest := func(plugin string) string { return filepath.Join(plugin, ".codex-plugin", "plugin.json") }
	cases := map[string]func(t *testing.T, file, plugin string, q *ObservationQuery){
		"expired":       func(_ *testing.T, _, _ string, q *ObservationQuery) { q.NowMS += HookObservationMaxAgeMS },
		"in the future": func(_ *testing.T, _, _ string, q *ObservationQuery) { q.NowMS -= 3600 * 1000 },
		"another version": func(t *testing.T, _, p string, _ *ObservationQuery) {
			writeFile(t, manifest(p), `{"name":"crw","version":"2.0.0"}`)
		},
		"changed entrypoint": func(t *testing.T, _, p string, _ *ObservationQuery) {
			writeFile(t, manifest(p), `{"name":"crw","version":"1.2.3","hooks":[]}`)
		},
		"wrong slot": func(t *testing.T, f, _ string, _ *ObservationQuery) {
			if err := os.Rename(f, filepath.Join(filepath.Dir(f), sum("other")+".json")); err != nil {
				t.Fatal(err)
			}
		},
		"link": func(t *testing.T, f, _ string, _ *ObservationQuery) {
			data, _ := os.ReadFile(f)
			copyPath := filepath.Join(t.TempDir(), "copy.json")
			writeFile(t, copyPath, string(data))
			if os.Remove(f) != nil || os.Symlink(copyPath, f) != nil {
				t.Fatal("cannot link")
			}
		},
		"oversized": func(t *testing.T, f, _ string, _ *ObservationQuery) {
			data, _ := os.ReadFile(f)
			writeFile(t, f, string(data)+strings.Repeat(" ", 8192))
		},
		"fifo": func(t *testing.T, f, _ string, _ *ObservationQuery) {
			if os.Remove(f) != nil || syscall.Mkfifo(f, 0o600) != nil {
				t.Fatal("cannot make a pipe")
			}
		},
		"directory": func(t *testing.T, f, _ string, _ *ObservationQuery) {
			if os.Remove(f) != nil || os.Mkdir(f, 0o700) != nil {
				t.Fatal("cannot make a directory")
			}
		},
		"manifest directory linked out of the plugin": func(t *testing.T, _, p string, _ *ObservationQuery) {
			outside := filepath.Join(t.TempDir(), "moved")
			if os.Rename(filepath.Join(p, ".codex-plugin"), outside) != nil || os.Symlink(outside, filepath.Join(p, ".codex-plugin")) != nil {
				t.Fatal("cannot link the manifest directory")
			}
		},
		"another plugin root": func(t *testing.T, _, _ string, q *ObservationQuery) {
			q.PluginRoot = t.TempDir()
			if err := os.MkdirAll(filepath.Join(q.PluginRoot, ".codex-plugin"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, manifest(q.PluginRoot), readerManifest)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			env, home, plugin := hookEnv(t)
			recordFor(t, env, `{"session_id":"s1"}`, "stop")
			q := readerQuery(home, plugin)
			if got := ReadHookObservations(q); len(got.Observations) != 1 {
				t.Fatalf("the record is not read before the change: %+v", got)
			}
			change(t, records(t, home)[0], plugin, &q)
			if got := ReadHookObservations(q); len(got.Observations) != 0 || got.Ignored != 1 || got.Reason != "" {
				t.Errorf("%+v", got)
			}
		})
	}
}

func TestReadHookObservationsSaysWhyItFoundNothing(t *testing.T) {
	env, home, plugin := hookEnv(t)
	q := readerQuery(home, plugin)
	if got := ReadHookObservations(q); got.Reason != "no invocation records" || got.Observations == nil {
		t.Errorf("missing store: %+v", got)
	}
	recordFor(t, env, `{"session_id":"s1"}`, "stop")
	// The actor's directory is followed through a link, as readdirSync follows it; the records in it are plain files.
	dir := filepath.Dir(records(t, home)[0])
	moved := filepath.Join(t.TempDir(), "moved")
	if os.Rename(dir, moved) != nil || os.Symlink(moved, dir) != nil {
		t.Fatal("cannot link the directory")
	}
	if got := ReadHookObservations(q); len(got.Observations) != 1 {
		t.Errorf("linked directory: %+v", got)
	}
	store := filepath.Join(home, "crw")
	if os.RemoveAll(store) != nil {
		t.Fatal("cannot remove the store")
	}
	writeFile(t, store, "not a directory")
	if got := ReadHookObservations(q); got.Reason != "invocation store unreadable" {
		t.Errorf("store is a file: %+v", got)
	}
	blank, negative := "", q
	negative.MaxAgeMS = -1
	noSession, noManifest, noActor := q, q, q
	noSession.SessionID, noManifest.PluginRoot, noActor.AgentID = "", t.TempDir(), &blank
	for name, c := range map[string]struct {
		q    ObservationQuery
		want string
	}{
		"no session":                {noSession, "session/actor identity unavailable"},
		"blank actor":               {noActor, "session/actor identity unavailable"},
		"negative age":              {negative, "invalid freshness filter"},
		"plugin without a manifest": {noManifest, "payload or invocation store unreadable"},
	} {
		if got := ReadHookObservations(c.q); got.Reason != c.want || len(got.Observations) != 0 {
			t.Errorf("%s: %q, want %q", name, got.Reason, c.want)
		}
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
