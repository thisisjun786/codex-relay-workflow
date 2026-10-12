package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
)

// fakeRuns is the Runs page's source over rows held in memory: the runs, and the rows of each, which a test appends to while a
// stream is open.
type fakeRuns struct {
	mu    sync.Mutex
	infos []flowcli.RunInfo
	rows  map[string][]flowcli.Row
	state map[string]string
}

func newFakeRuns() *fakeRuns {
	return &fakeRuns{rows: map[string][]flowcli.Row{}, state: map[string]string{}}
}

func (f *fakeRuns) add(run, state string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos = append(f.infos, flowcli.RunInfo{RunRecord: flowcli.RunRecord{RunID: run, Workflow: "project-lane", Owner: "thread-owner"}, State: state, Calls: n})
	f.state[run] = state
	for i := 1; i <= n; i++ {
		f.rows[run] = append(f.rows[run], flowcli.Row{Seq: i, Kind: "call", Event: "completed", CallID: fmt.Sprintf("c%d", i)})
	}
}

func (f *fakeRuns) push(run string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.rows[run]) + 1
	f.rows[run] = append(f.rows[run], flowcli.Row{Seq: n, Kind: "flow", Event: "phase"})
}

func (f *fakeRuns) finish(run string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[run] = flowcli.StateCompleted
}

func (f *fakeRuns) Infos(context.Context) ([]flowcli.RunInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]flowcli.RunInfo(nil), f.infos...), nil
}

func (f *fakeRuns) Detail(_ context.Context, run string) (flowcli.RunDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.infos {
		if i.RunID == run {
			return flowcli.RunDetail{RunInfo: i, Phases: []flowcli.PhaseNode{{Title: "build", Agents: []flowcli.AgentNode{{CallID: "c1", Role: "executor", Node: "n1", Status: "completed", ThreadID: "thr-1", Tokens: 12}, {CallID: "c2", Role: "executor", Node: "n2", Status: "outcome_unknown", RecoveryHold: true}}}},
				Nodes: []flowcli.NodeInfo{{Node: "n1", Commits: []string{"abc"}, Threads: []string{"thr-1"}, Tokens: 12}, {Node: "n2", RecoveryHold: true}}, Questions: []flowcli.Question{}}, nil
		}
	}
	return flowcli.RunDetail{}, fmt.Errorf("%w: %s", flowcli.ErrNoRun, run)
}

func (f *fakeRuns) Tail(ctx context.Context, run string, after, limit int, wait time.Duration) (flowcli.Page, error) {
	deadline := time.Now().Add(wait)
	for {
		f.mu.Lock()
		rows, known := f.rows[run]
		state := f.state[run]
		f.mu.Unlock()
		if !known {
			return flowcli.Page{}, fmt.Errorf("%w: %s", flowcli.ErrNoRun, run)
		}
		p := flowcli.Page{Run: run, After: after, Cursor: after, Rows: []flowcli.Row{}, State: state}
		for _, r := range rows {
			if r.Seq > after {
				if len(p.Rows) == limit {
					p.More = true
					break
				}
				p.Rows = append(p.Rows, r)
				p.Cursor = r.Seq
			}
		}
		if len(p.Rows) > 0 || time.Now().After(deadline) {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *fakeRuns) Close() error { return nil }

// useRuns replaces the source for a test.
func useRuns(t *testing.T, f *fakeRuns) {
	t.Helper()
	old := runsOpen
	runsOpen = func(context.Context) (runsSource, error) { return f, nil }
	t.Cleanup(func() { runsOpen = old })
}

// TestRunsAPIList: GET /api/runs lists the runs of the store with their state, GET /api/runs/<id> is the tree of one (phase, then
// agent, with the nodes' commits and recovery_hold), an unknown run is a 404 and a malformed id a 400, and a store that cannot be
// read is reported with its reason, never as an empty list. The routes are registered from this file's own init.
func TestRunsAPIList(t *testing.T) {
	f := newFakeRuns()
	f.add("run-a", "running", 3)
	f.add("run-b", "completed", 1)
	useRuns(t, f)
	for _, want := range []string{"GET /api/runs", "GET /api/runs/", "POST /api/runs/"} {
		method, path, _ := strings.Cut(want, " ")
		found := false
		for _, r := range Routes() {
			found = found || (r.Method == method && r.Path == path)
		}
		if !found {
			t.Errorf("%s is not registered", want)
		}
	}
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	rec := request(server, http.MethodGet, "/api/runs", guardHost, "", nil)
	var list struct {
		Runs    []flowcli.RunInfo `json:"runs"`
		Unknown string            `json:"unknown"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Runs) != 2 || list.Runs[0].RunID != "run-a" || list.Runs[0].State != "running" || list.Runs[1].State != "completed" || list.Unknown != "" {
		t.Fatalf("the list: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("headers %v", rec.Header())
	}
	rec = request(server, http.MethodGet, "/api/runs/run-a", guardHost, "", nil)
	var d flowcli.RunDetail
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &d) != nil || len(d.Phases) != 1 || !d.Phases[0].Agents[1].RecoveryHold || d.Nodes[0].Commits[0] != "abc" || d.Phases[0].Agents[0].ThreadID != "thr-1" {
		t.Fatalf("the tree: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(server, http.MethodGet, "/api/runs/run-none", guardHost, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown run: %d", rec.Code)
	}
	for _, bad := range []string{"/api/runs/..", "/api/runs/run%20a", "/api/runs/run:a"} {
		if rec := request(server, http.MethodGet, bad, guardHost, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := request(server, http.MethodGet, "/api/runs/run-a/pause", guardHost, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a GET of a write verb: %d", rec.Code)
	}
	// a store that cannot be read is not an empty list
	runsOpen = func(context.Context) (runsSource, error) { return nil, errors.New("there is no relay store at /x") }
	rec = request(server, http.MethodGet, "/api/runs", guardHost, "", nil)
	list.Runs, list.Unknown = nil, ""
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || list.Unknown != "there is no relay store at /x" || len(list.Runs) != 0 {
		t.Errorf("an unreadable store: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(server, http.MethodGet, "/api/runs/run-a", guardHost, "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("the tree of an unreadable store: %d", rec.Code)
	}
}

// sseEvent is one event of a stream.
type sseEvent struct{ ID, Event, Data string }

// readEvents reads events from a stream until n of the event kind have arrived, the stream ends, or the time is up.
func readEvents(t *testing.T, body *bufio.Reader, kind string, n int) []sseEvent {
	t.Helper()
	type result struct {
		events []sseEvent
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var got []sseEvent
		var cur sseEvent
		for len(got) < n || n < 0 {
			line, err := body.ReadString('\n')
			if err != nil {
				done <- result{got, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if cur.Event == kind {
					got = append(got, cur)
				}
				if cur.Event == "end" {
					done <- result{got, nil}
					return
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		done <- result{got, nil}
	}()
	select {
	case r := <-done:
		return r.events
	case <-time.After(10 * time.Second):
		t.Fatal("the stream gave no event in time")
		return nil
	}
}

// streamServer serves the package registry on a loopback port the guard accepts.
func streamServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{Port: ln.Addr().(*net.TCPAddr).Port, Token: guardToken, Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(server)
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, ln.Addr().String()
}

func openStream(t *testing.T, ts *httptest.Server, path string, lastID string) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return resp, bufio.NewReader(resp.Body), cancel
}

// TestRunsSSEResumeFromCursor: the event stream of a run gives the rows with their seq as the event id; a reconnect that presents the
// last id it saw (the header an EventSource sends by itself, or ?after=) goes on from the next row and repeats none, a row that
// arrives while the stream is open is delivered, the state is an event of its own, and the stream ends with an `end` event when the
// run is over.
func TestRunsSSEResumeFromCursor(t *testing.T) {
	f := newFakeRuns()
	f.add("run-s", "running", 6)
	useRuns(t, f)
	old, oldLimit := runsEventWait, runsEventLimit
	runsEventWait, runsEventLimit = 50*time.Millisecond, 4
	defer func() { runsEventWait, runsEventLimit = old, oldLimit }()
	ts, _ := streamServer(t)

	resp, body, cancel := openStream(t, ts, "/api/runs/run-s/events", "")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the stream: %d %v", resp.StatusCode, resp.Header)
	}
	first := readEvents(t, body, "row", 3)
	cancel()
	resp.Body.Close()
	if len(first) != 3 || first[0].ID != "1" || first[2].ID != "3" {
		t.Fatalf("the first connection: %+v", first)
	}
	var row flowcli.Row
	if err := json.Unmarshal([]byte(first[1].Data), &row); err != nil || row.Seq != 2 || row.CallID != "c2" {
		t.Errorf("the data of event 2: %s %v", first[1].Data, err)
	}

	// the reconnect presents the last id it saw: rows 4 to 6 once, then a row that arrives while it is open
	resp, body, cancel = openStream(t, ts, "/api/runs/run-s/events", "3")
	defer cancel()
	defer resp.Body.Close()
	again := readEvents(t, body, "row", 3)
	if len(again) != 3 || again[0].ID != "4" || again[1].ID != "5" || again[2].ID != "6" {
		t.Fatalf("the reconnect: %+v", again)
	}
	go func() {
		time.Sleep(80 * time.Millisecond)
		f.push("run-s")
		f.finish("run-s")
	}()
	rest := readEvents(t, body, "row", 1)
	if len(rest) != 1 || rest[0].ID != "7" {
		t.Fatalf("the row that arrived: %+v", rest)
	}
	// the stream ends when the run is over
	if end := readEvents(t, body, "end", -1); len(end) != 1 || !strings.Contains(end[0].Data, `"state":"completed"`) || !strings.Contains(end[0].Data, `"cursor":7`) {
		t.Errorf("the end of the stream: %+v", end)
	}

	// ?after= is the cursor too, and a cursor past the last row gives no row
	resp2, body2, cancel2 := openStream(t, ts, "/api/runs/run-s/events?after=5", "")
	defer cancel2()
	defer resp2.Body.Close()
	got := readEvents(t, body2, "row", 2)
	if len(got) != 2 || got[0].ID != "6" || got[1].ID != "7" {
		t.Errorf("?after=5: %+v", got)
	}
	// an unknown run is a 404 before any stream starts, a bad cursor a 400
	resp3, _, cancel3 := openStream(t, ts, "/api/runs/run-none/events", "")
	defer cancel3()
	resp3.Body.Close()
	resp4, _, cancel4 := openStream(t, ts, "/api/runs/run-s/events?after=x", "")
	defer cancel4()
	resp4.Body.Close()
	if resp3.StatusCode != http.StatusNotFound || resp4.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown run %d, bad cursor %d", resp3.StatusCode, resp4.StatusCode)
	}
}

// TestRunsControlGuarded: pause, cancel and answer are writes and pass the guard before they reach a command: a wrong Host, no
// token, a wrong token, a foreign Origin and a content type other than JSON are refused with nothing run; with the token they run
// the command of `crw flow` for the run, an exit 1 of the command is a 409 with its message, and an answer body is read strictly.
func TestRunsControlGuarded(t *testing.T) {
	f := newFakeRuns()
	f.add("run-c", "running", 1)
	useRuns(t, f)
	var calls [][]string
	exit := 0
	old := runsControl
	runsControl = func(_ context.Context, args []string) (int, string, string) {
		calls = append(calls, args)
		if exit == 0 {
			return 0, "pause requested of run run-c (process 7)\n", ""
		}
		return exit, "", "crw flow pause: run run-c is completed: nothing to pause\n"
	}
	defer func() { runsControl = old }()
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	json := map[string]string{"Content-Type": "application/json", tokenHeader: guardToken}
	refused := []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"a foreign Host", "evil.example:80", json},
		{"no token", guardHost, map[string]string{"Content-Type": "application/json"}},
		{"a wrong token", guardHost, map[string]string{"Content-Type": "application/json", tokenHeader: "nope"}},
		{"a foreign Origin", guardHost, map[string]string{"Content-Type": "application/json", tokenHeader: guardToken, "Origin": "http://evil.example"}},
		{"a text content type", guardHost, map[string]string{"Content-Type": "text/plain", tokenHeader: guardToken}},
	}
	for _, verb := range []string{"pause", "cancel", "answer"} {
		for _, tc := range refused {
			rec := request(server, http.MethodPost, "/api/runs/run-c/"+verb, tc.host, `{"decision":"d","question":"q","revision":1}`, tc.headers)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with %s: %d", verb, tc.name, rec.Code)
			}
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused request ran %v", calls)
	}

	rec := request(server, http.MethodPost, "/api/runs/run-c/pause", guardHost, `{}`, json)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), "pause requested") {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(server, http.MethodPost, "/api/runs/run-c/cancel", guardHost, `{}`, json); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	exit = 1
	if rec := request(server, http.MethodPost, "/api/runs/run-c/pause", guardHost, `{}`, json); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nothing to pause") {
		t.Errorf("a refusal: %d %s", rec.Code, rec.Body.String())
	}
	exit = 2
	if rec := request(server, http.MethodPost, "/api/runs/run-c/pause", guardHost, `{}`, json); rec.Code != http.StatusBadRequest {
		t.Errorf("a command it cannot use: %d", rec.Code)
	}
	exit = 0
	rec = request(server, http.MethodPost, "/api/runs/run-c/answer", guardHost, `{"decision":"ud-1","node":"n1","question":"esc_one","revision":2}`, json)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body.String())
	}
	last := calls[len(calls)-1]
	if got := strings.Join(last, " "); got != "answer --json --thread gui --decision ud-1 --run run-c --node n1 --question esc_one --revision 2" {
		t.Errorf("the command of the answer: %s", got)
	}
	for _, bad := range []string{`{}`, `{"decision":"d","question":"q","revision":0}`, `{"decision":"d","question":"q","revision":1,"run":"other"}`, `{"decision":"d","question":"q","revision":1} {}`, `nope`} {
		before := len(calls)
		if rec := request(server, http.MethodPost, "/api/runs/run-c/answer", guardHost, bad, json); rec.Code != http.StatusBadRequest || len(calls) != before {
			t.Errorf("answer body %s: %d, %d commands run", bad, rec.Code, len(calls)-before)
		}
	}
	if rec := request(server, http.MethodPost, "/api/runs/run-c/explode", guardHost, `{}`, json); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown verb: %d", rec.Code)
	}
	if fmt.Sprint(calls[0]) != "[pause run-c]" || fmt.Sprint(calls[1]) != "[cancel run-c]" {
		t.Errorf("the commands: %v", calls[:2])
	}
}
