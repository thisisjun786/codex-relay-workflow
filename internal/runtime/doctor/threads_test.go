package doctor_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// fakeThread is one thread the fake App Server lists (status "notLoaded" unless set), with its
// newest turn (none when turn is nil).
type fakeThread struct {
	id, name, status string
	archived         bool
	turn             map[string]any
}

// turn is a Turn as codex-cli 0.154.0 reports it: startedAt and completedAt are Unix seconds
// or nil.
func turn(id, status string, started, completed any) map[string]any {
	return map[string]any{"id": id, "status": status, "startedAt": started, "completedAt": completed, "items": []any{}}
}

// fakeAppServer is a fake App Server for h's Codex home listing threads perPage at a time (cursors
// "p<offset>" per listing), each thread's newest turn answered by thread/turns/list.
func fakeAppServer(t *testing.T, h *host, perPage int, threads ...fakeThread) *fakehost.Server {
	t.Helper()
	srv := fakehost.Start(t)
	srv.Respond("initialize", fakehost.Reply{Result: map[string]any{"userAgent": "fake Codex/0.154.0", "codexHome": h.codex, "platformOs": "linux"}})
	srv.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params struct {
			Archived bool    `json:"archived"`
			Cursor   *string `json:"cursor"`
		}
		_ = json.Unmarshal(raw, &params)
		var matching []fakeThread
		for _, one := range threads {
			if one.archived == params.Archived {
				matching = append(matching, one)
			}
		}
		start := 0
		if params.Cursor != nil {
			start, _ = strconv.Atoi(strings.TrimPrefix(*params.Cursor, "p"))
		}
		end := min(start+perPage, len(matching))
		data := []any{}
		for _, one := range matching[start:end] {
			status := one.status
			if status == "" {
				status = "notLoaded"
			}
			data = append(data, map[string]any{"id": one.id, "name": one.name, "status": map[string]any{"type": status},
				"createdAt": int64(1790000000), "updatedAt": int64(1790000100), "turns": []any{}})
		}
		var next any
		if end < len(matching) {
			next = "p" + strconv.Itoa(end)
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": next}}
	})
	srv.Handle("thread/turns/list", func(raw json.RawMessage) fakehost.Reply {
		var params struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(raw, &params)
		for _, one := range threads {
			if one.id == params.ThreadID {
				data := []any{}
				if one.turn != nil {
					data = append(data, one.turn)
				}
				return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
			}
		}
		return fakehost.Reply{Error: &fakehost.RPCError{Code: -32600, Message: "thread not found"}}
	})
	return srv
}

func (h *host) scanSocket(t *testing.T, socket string) record.Object {
	t.Helper()
	return doctor.RetentionScan(context.Background(), doctor.RetentionOptions{Env: h.env, Now: func() time.Time { return scanNow }, ScopeRegistry: filepath.Join(h.home, "scopes"), Socket: socket})
}

// cacheVersion installs the plugin wiring from source as a cached version now, and returns its
// directory and a time before it was installed.
func (h *host) cacheVersion(t *testing.T, version, source string) (string, time.Time) {
	t.Helper()
	before := time.Now().Add(-time.Second)
	cache := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", version)
	for _, name := range []string{"mcp.json", "hooks/stop-recording-completion.json", "crw-bridge.sh"} {
		raw, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(cache, "wiring", filepath.FromSlash(name)), string(raw), 0o644)
	}
	return cache, before
}

// nativeHost is a host whose pointer selects a Go runtime and whose one cached version is the
// shipped native wiring.
func (h *host) nativeHost(t *testing.T) (string, time.Time) {
	t.Helper()
	dir, _ := h.goRuntime(t, "bin-0.4.1-aaaaaaaaaaaa")
	link(t, dir, h.current())
	return h.cacheVersion(t, "0.4.1+native", filepath.Join(golden.Root(), "plugins", "crw", "wiring"))
}

// rowReferences is every row-7 reference, keyed by thread id.
func rowReferences(report record.Object) map[string]record.Object {
	out := map[string]record.Object{}
	for _, raw := range golden.List(record.Get(report, "pythonReferences")) {
		if r := golden.Obj(raw); record.Get(r, "row") == int64(7) {
			out[record.Text(r, "threadId")] = r
		}
	}
	return out
}

func stringList(v any) []string {
	var out []string
	for _, raw := range golden.List(v) {
		out = append(out, raw.(string))
	}
	return out
}

// With nothing answering at the socket, row 7 read nothing: it stays unscanned with the reason,
// and the scan is not clear. The default socket is the bridge's under the Codex home, which in
// this fixture does not exist. An App Server serving another Codex home is not asked either.
func TestRetentionScanLeavesThreadsUnscannedWithoutAnAppServer(t *testing.T) {
	h := newHost(t)
	h.nativeHost(t)
	report := h.scan(t)
	socket := filepath.Join(h.codex, "app-server-control", "app-server-control.sock")
	if record.Get(report, "appServerSocket") != socket {
		t.Fatalf("default socket %v", record.Get(report, "appServerSocket"))
	}
	row := surfaceRow(t, report, 7)
	if record.Get(row, "scanned") != false || !strings.Contains(record.Text(row, "detail"), "could not be reached") || record.Get(report, "clear") != false {
		t.Fatalf("row 7 %s clear %v", golden.Canon(row), record.Get(report, "clear"))
	}
	if !listed(stringList(record.Get(report, "unscanned")), "row 7: ", socket) || len(rowReferences(report)) != 0 {
		t.Fatalf("unscanned %s", golden.Canon(record.Get(report, "unscanned")))
	}

	other := fakeAppServer(t, h, 10, fakeThread{id: "t-running", turn: turn("u1", "inProgress", int64(1), nil)})
	other.Respond("initialize", fakehost.Reply{Result: map[string]any{"userAgent": "fake Codex/0.154.0", "codexHome": filepath.Join(h.home, "other-codex")}})
	report = h.scanSocket(t, other.SocketPath)
	row = surfaceRow(t, report, 7)
	if record.Get(row, "scanned") != false || !strings.Contains(record.Text(row, "detail"), "serves the Codex home") || len(rowReferences(report)) != 0 {
		t.Fatalf("row 7 against another Codex home %s", golden.Canon(row))
	}
	if other.Count("thread/list") != 0 {
		t.Fatal("the scan listed the threads of an App Server serving another Codex home")
	}
}

// A turn that may still be running and started before the cached version was installed was
// given its hook commands by a version replaced since: a reference, with the thread's id, name
// and the timestamps compared. A running turn here is inProgress; one running in another App
// Server process, or whose process died, reads interrupted with no completedAt; an archived
// thread is listed too. So is a running turn that records no start. A turn that ended
// (completed, failed, interrupted with a completedAt) or a thread with no turn is not, however
// old: its next turn resolves the version installed then.
func TestRetentionScanReportsARunningTurnOlderThanThePayload(t *testing.T) {
	h := newHost(t)
	cache, before := h.nativeHost(t)
	old := before.Add(-time.Hour).Unix()
	srv := fakeAppServer(t, h, 10,
		fakeThread{id: "t-loaded", name: "running here", status: "active", turn: turn("u-loaded", "inProgress", old, nil)},
		fakeThread{id: "t-elsewhere", turn: turn("u-elsewhere", "interrupted", old, nil)},
		fakeThread{id: "t-archived", archived: true, turn: turn("u-archived", "interrupted", old, nil)},
		fakeThread{id: "t-undated", turn: turn("u-undated", "inProgress", nil, nil)},
		fakeThread{id: "t-done", turn: turn("u-done", "completed", old, old+5)},
		fakeThread{id: "t-failed", turn: turn("u-failed", "failed", old, old+5)},
		fakeThread{id: "t-stopped", turn: turn("u-stopped", "interrupted", old, old+5)},
		fakeThread{id: "t-empty"},
	)
	report := h.scanSocket(t, srv.SocketPath)
	refs := rowReferences(report)
	if len(refs) != 4 || refs["t-loaded"] == nil || refs["t-elsewhere"] == nil || refs["t-archived"] == nil || refs["t-undated"] == nil {
		t.Fatalf("row 7 references %s", golden.Canon(record.Get(report, "pythonReferences")))
	}
	loaded := refs["t-loaded"]
	for key, want := range map[string]any{
		"source": srv.SocketPath, "field": "newestTurn", "value": filepath.Dir(cache), "kind": "missing", "python": true,
		"name": "running here", "threadStatus": "active", "archived": false, "turnId": "u-loaded", "turnStatus": "inProgress",
		"turnStartedAt": time.Unix(old, 0).UTC().Format("2006-01-02T15:04:05Z"), "turnCompletedAt": nil,
		"threadCreatedAt": "2026-09-21T14:13:20Z", "threadUpdatedAt": "2026-09-21T14:15:00Z", "versionDirectory": nil,
	} {
		if got := record.Get(loaded, key); got != want {
			t.Errorf("%s = %v, want %v (%s)", key, got, want, golden.Canon(loaded))
		}
	}
	if detail := record.Text(loaded, "detail"); !strings.Contains(detail, "replaced since") || !strings.Contains(detail, "the earliest at") {
		t.Errorf("detail %q", detail)
	}
	if detail := record.Text(refs["t-elsewhere"], "detail"); !strings.Contains(detail, "another App Server process") {
		t.Errorf("detail %q", detail)
	}
	if record.Get(refs["t-archived"], "archived") != true {
		t.Errorf("archived %s", golden.Canon(refs["t-archived"]))
	}
	if record.Get(refs["t-undated"], "kind") != "unreadable" || !strings.Contains(record.Text(refs["t-undated"], "detail"), "no startedAt") {
		t.Errorf("undated %s", golden.Canon(refs["t-undated"]))
	}
	row := surfaceRow(t, report, 7)
	if record.Get(row, "scanned") != true || record.Get(row, "examined") != int64(8) || record.Get(report, "clear") != false {
		t.Fatalf("row 7 %s clear %v", golden.Canon(row), record.Get(report, "clear"))
	}
}

// A running turn that started after the cached native version was installed was given that
// version's hook commands, which row 5 clears: no reference, and a host with nothing else
// retained reads clear.
func TestRetentionScanClearsARunningTurnNewerThanThePayload(t *testing.T) {
	h := newHost(t)
	h.nativeHost(t)
	later := time.Now().Add(time.Minute).Unix()
	srv := fakeAppServer(t, h, 10,
		fakeThread{id: "t-new", status: "active", turn: turn("u-new", "inProgress", later, nil)},
		fakeThread{id: "t-new-elsewhere", turn: turn("u-new-elsewhere", "interrupted", later, nil)},
	)
	report := h.scanSocket(t, srv.SocketPath)
	if refs := golden.List(record.Get(report, "pythonReferences")); len(refs) != 0 {
		t.Fatalf("python references %s", golden.Canon(refs))
	}
	row := surfaceRow(t, report, 7)
	if record.Get(row, "scanned") != true || record.Get(row, "examined") != int64(2) || !strings.Contains(record.Text(row, "detail"), "2 with a newest turn that may still be running") {
		t.Fatalf("row 7 %s", golden.Canon(row))
	}
	if record.Get(report, "clear") != true {
		t.Fatalf("clear %v: unscanned %s unreadable %s holds %s", record.Get(report, "clear"), golden.Canon(record.Get(report, "unscanned")),
			golden.Canon(record.Get(report, "unreadable")), golden.Canon(record.Get(report, "liveHolds")))
	}
}

// A running turn given the hook commands of a cached version whose wiring row 5 reports as
// Python (the pre-native bootstrap) is a reference naming that version, even after it started.
func TestRetentionScanReportsATurnGivenAPythonVersion(t *testing.T) {
	h := newHost(t)
	h.pythonFirstOnPath(t)
	dir, _ := h.goRuntime(t, "bin-0.4.1-aaaaaaaaaaaa")
	link(t, dir, h.current())
	cache, _ := h.cacheVersion(t, "0.4.0+test", filepath.Join(golden.Root(), "internal", "pluginwiring", "testdata", "pre-native-wiring"))
	srv := fakeAppServer(t, h, 10, fakeThread{id: "t-python", name: "held", turn: turn("u-python", "inProgress", time.Now().Add(time.Minute).Unix(), nil)})
	report := h.scanSocket(t, srv.SocketPath)
	ref := rowReferences(report)["t-python"]
	if ref == nil || record.Get(ref, "value") != cache || record.Get(ref, "kind") != "directory" || record.Get(ref, "versionDirectory") != cache ||
		record.Get(ref, "versionInstalledAt") == nil || !strings.Contains(record.Text(ref, "detail"), "reports one of its hook commands as Python") {
		t.Fatalf("row 7 reference %s", golden.Canon(ref))
	}
}

// The listing is read to its end: every page of thread/list (the non-archived and the archived
// listing each), each cursor passed back as given, every source kind asked for, from the state
// DB, 100 at a time; a running turn on the last page is found.
func TestRetentionScanPagesThroughTheThreads(t *testing.T) {
	h := newHost(t)
	_, before := h.nativeHost(t)
	old := before.Add(-time.Hour).Unix()
	srv := fakeAppServer(t, h, 1,
		fakeThread{id: "t-1", turn: turn("u-1", "completed", old, old+1)},
		fakeThread{id: "t-2", turn: turn("u-2", "completed", old, old+1)},
		fakeThread{id: "t-3", turn: turn("u-3", "inProgress", old, nil)},
		fakeThread{id: "t-a1", archived: true, turn: turn("u-a1", "completed", old, old+1)},
		fakeThread{id: "t-a2", archived: true, turn: turn("u-a2", "interrupted", old, nil)},
	)
	report := h.scanSocket(t, srv.SocketPath)
	refs := rowReferences(report)
	if len(refs) != 2 || refs["t-3"] == nil || refs["t-a2"] == nil {
		t.Fatalf("row 7 references %s", golden.Canon(record.Get(report, "pythonReferences")))
	}
	var cursors []string
	for _, request := range srv.Requests() {
		if request.Method != "thread/list" {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		kinds, _ := params["sourceKinds"].([]any)
		if params["limit"] != float64(100) || params["useStateDbOnly"] != true || len(kinds) != 10 || !slices.Contains(kinds, any("subAgent")) || !slices.Contains(kinds, any("exec")) {
			t.Fatalf("thread/list params %s", request.Params)
		}
		cursor, _ := params["cursor"].(string)
		cursors = append(cursors, strconv.FormatBool(params["archived"].(bool))+":"+cursor)
	}
	if want := []string{"false:", "false:p1", "false:p2", "true:", "true:p1"}; !slices.Equal(cursors, want) {
		t.Fatalf("listing pages %v, want %v", cursors, want)
	}
	if row := surfaceRow(t, report, 7); record.Get(row, "scanned") != true || record.Get(row, "examined") != int64(5) || srv.Count("thread/turns/list") != 5 {
		t.Fatalf("row 7 %s, turns read %d", golden.Canon(row), srv.Count("thread/turns/list"))
	}
}

// A listing or a turn that cannot be read is never read as no thread: a page that fails, a
// cursor that comes back again and a thread whose newest turn cannot be read are unreadable, the
// row is unscanned, and what was read before them is still judged.
func TestRetentionScanDoesNotReadAFailedListingAsNoThreads(t *testing.T) {
	h := newHost(t)
	_, before := h.nativeHost(t)
	old := before.Add(-time.Hour).Unix()
	threads := []fakeThread{
		{id: "t-1", turn: turn("u-1", "inProgress", old, nil)},
		{id: "t-2", turn: turn("u-2", "completed", old, old+1)},
	}

	failing := fakeAppServer(t, h, 1, threads...)
	failing.Script("thread/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "t-1", "status": map[string]any{"type": "notLoaded"}}}, "nextCursor": "p1"}},
		fakehost.Reply{Error: &fakehost.RPCError{Code: -32603, Message: "state db unavailable"}})
	report := h.scanSocket(t, failing.SocketPath)
	if got := rowEntries(unreadable(report), 7); len(got) != 1 || !listed(got, "thread/list (archived false)", "state db unavailable", "from this page on are unknown") {
		t.Fatalf("unreadable %v", got)
	}
	if record.Get(surfaceRow(t, report, 7), "scanned") != false || rowReferences(report)["t-1"] == nil {
		t.Fatalf("row 7 %s references %s", golden.Canon(surfaceRow(t, report, 7)), golden.Canon(record.Get(report, "pythonReferences")))
	}

	looping := fakeAppServer(t, h, 1, threads...)
	page := fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": "again"}}
	looping.Script("thread/list", page, page)
	report = h.scanSocket(t, looping.SocketPath)
	if got := rowEntries(unreadable(report), 7); len(got) != 1 || !listed(got, `the cursor "again" came back again`) || record.Get(surfaceRow(t, report, 7), "scanned") != false {
		t.Fatalf("unreadable %v", got)
	}

	unreadableTurn := fakeAppServer(t, h, 10, append(threads, fakeThread{id: "t-gone"})...)
	unreadableTurn.Script("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{turn("u-1", "inProgress", old, nil)}}},
		fakehost.Reply{Result: map[string]any{"data": []any{turn("u-2", "completed", old, old+1)}}},
		fakehost.Reply{Error: &fakehost.RPCError{Code: -32600, Message: "thread not found"}})
	report = h.scanSocket(t, unreadableTurn.SocketPath)
	if got := rowEntries(unreadable(report), 7); len(got) != 1 || !listed(got, "thread t-gone", "thread not found") || record.Get(surfaceRow(t, report, 7), "scanned") != false {
		t.Fatalf("unreadable %v", got)
	}
}

// A cached version whose directory cannot be read has no install time, so no running turn can be
// placed against the version it was given: each is a reference, even one that started after
// every version was installed.
func TestRetentionScanCannotPlaceATurnAgainstAnUndatedVersion(t *testing.T) {
	skipAsRoot(t)
	h := newHost(t)
	cache, _ := h.nativeHost(t)
	root := filepath.Dir(cache)
	if err := os.Chmod(root, 0o644); err != nil { // listable, but its entries cannot be looked up
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	srv := fakeAppServer(t, h, 10, fakeThread{id: "t-new", status: "active", turn: turn("u-new", "inProgress", time.Now().Add(time.Minute).Unix(), nil)})
	report := h.scanSocket(t, srv.SocketPath)
	ref := rowReferences(report)["t-new"]
	if ref == nil || record.Get(ref, "kind") != "unreadable" || !strings.Contains(record.Text(ref, "detail"), "install time of a cached plugin version could not be read") {
		t.Fatalf("row 7 reference %s", golden.Canon(record.Get(report, "pythonReferences")))
	}
	if record.Get(surfaceRow(t, report, 5), "scanned") != false || record.Get(report, "clear") != false {
		t.Fatalf("row 5 %s", golden.Canon(surfaceRow(t, report, 5)))
	}
}
