package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// startedThread starts a thread through the backend, for the tests that read turns directly.
func startedThread(t *testing.T, e *env, role string) engine.ThreadInfo {
	t.Helper()
	spec := e.policy.Roles[role]
	cwd := filepath.Join(e.root, "direct-"+role)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_direct", Role: role, Pair: spec.Pair, Sandbox: spec.Sandbox, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// runTurn runs one turn directly and returns its outcome.
func runTurn(t *testing.T, e *env, info engine.ThreadInfo, role string, k int, text string) (engine.TurnOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	op := fmt.Sprintf("c_direct/t%d", k)
	return e.backend.RunTurn(ctx, engine.TurnSpec{CallID: "c_direct", Op: op, K: k, ThreadID: info.ID, Role: role, Pair: e.policy.Roles[role].Pair, Input: "crw-flow-op: " + op + "\n\n" + text})
}

// With every notification dropped, a turn is still ended: the backend reads the turn it recorded by its id, not the latest
// turn of the thread, and a repair turn gets its own result.
func TestPullFallbackReadsExactTurn(t *testing.T) {
	e := newEnv(t)
	e.threads.DropNotifications(true)
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: fmt.Sprintf(`{"verdict":"turn-%d"}`, r.N)}
	})
	info := startedThread(t, e, "explorer")
	first, err := runTurn(t, e, info, "explorer", 1, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := runTurn(t, e, info, "explorer", 2, "two")
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != `{"verdict":"turn-1"}` || second.Text != `{"verdict":"turn-2"}` || first.TurnID == second.TurnID {
		t.Fatalf("outcomes %+v / %+v", first, second)
	}
	// the older turn, read again by its recorded id while a newer one exists, is still its own
	again, err := e.backend.ReadTurn(context.Background(), info.ID, first.TurnID, "c_direct/t1")
	if err != nil || again.Text != first.Text || again.TurnID != first.TurnID {
		t.Fatalf("reading %s: %+v %v", first.TurnID, again, err)
	}
	// the same through the engine: the repair turn of a call reads its own turn
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.N == 1 {
			return fakehost.TurnPlan{Message: "not json"}
		}
		return fakehost.TurnPlan{Message: fmt.Sprintf(`{"verdict":"turn-%d"}`, r.N)}
	})
	res := e.run(`const S = ` + verdictSchema + `
const r = await agent.call('x', { agentType: 'explorer', schema: S, label: 'x' })
return [r.status, r.output.verdict, r.meta.repairUsed, r.meta.turnIds.length]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[completed turn-2 true 2]" {
		t.Fatalf("%v", got)
	}
	// a turn that is still running is not an answer
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Hang: true} })
	running, err := e.client.Call(context.Background(), "turn/start", map[string]any{"threadId": info.ID, "input": []any{map[string]any{"type": "text", "text": "slow"}}})
	if err != nil {
		t.Fatal(err)
	}
	var started struct{ Turn struct{ ID string } }
	if err := json.Unmarshal(running, &started); err != nil {
		t.Fatal(err)
	}
	if _, err := e.backend.ReadTurn(context.Background(), info.ID, started.Turn.ID, ""); !errors.Is(err, agent.ErrTurnInProgress) {
		t.Errorf("a running turn read as %v", err)
	}
}

// The pull never hydrates history with includeTurns: it pages thread/turns/list with itemsView "full" and, for a turn the
// list gave only as a summary, reads thread/items/list by thread and turn.
func TestPullFallbackRequestsFullItems(t *testing.T) {
	e := newEnv(t)
	e.threads.DropNotifications(true)
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: passMessage, SummaryOnly: r.N == 2}
	})
	info := startedThread(t, e, "explorer")
	full, err := runTurn(t, e, info, "explorer", 1, "one")
	if err != nil || full.Text != passMessage {
		t.Fatalf("%+v %v", full, err)
	}
	summary, err := runTurn(t, e, info, "explorer", 2, "two")
	if err != nil || summary.Text != passMessage {
		t.Fatalf("%+v %v", summary, err)
	}
	lists := e.requests("thread/turns/list")
	if len(lists) == 0 {
		t.Fatalf("the turns were not read")
	}
	for _, l := range lists {
		if l.Params["itemsView"] != "full" || l.Params["threadId"] != info.ID {
			t.Errorf("thread/turns/list %v", l.Params)
		}
	}
	items := e.requests("thread/items/list")
	if len(items) == 0 {
		t.Fatalf("the summary turn's items were not read with thread/items/list")
	}
	for _, i := range items {
		if i.Params["threadId"] != info.ID || i.Params["turnId"] != summary.TurnID {
			t.Errorf("thread/items/list %v, want thread %s turn %s", i.Params, info.ID, summary.TurnID)
		}
	}
	for _, r := range e.requests("thread/read") {
		if r.Params["includeTurns"] != false {
			t.Errorf("thread/read %v hydrates history", r.Params)
		}
	}
}

// A turn whose items cannot be read in full is unreadable - not a turn without a message. The call keeps the intent it
// wrote, the engine sends no repair turn, and an empty message that was read in full is still a (schema-invalid) answer.
func TestSummaryItemsViewIsUnreadableNotAbsent(t *testing.T) {
	e := newEnv(t)
	e.threads.DropNotifications(true)
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: passMessage, SummaryOnly: true, ItemsListError: map[string]any{"code": -32600, "message": "items are not available"}}
	})
	info := startedThread(t, e, "explorer")
	_, err := runTurn(t, e, info, "explorer", 1, "one")
	var unreadable *agent.UnreadableTurnError
	if !errors.As(err, &unreadable) {
		t.Fatalf("error %v, want an unreadable turn", err)
	}
	var transport *engine.TransportError
	if !errors.As(err, &transport) {
		t.Errorf("an unreadable turn is not an answer: %T", err)
	}
	if unreadable.ThreadID != info.ID || unreadable.TurnID == "" {
		t.Errorf("%+v", unreadable)
	}
	// through the engine: no repair, the call stays open
	res := e.run(`const S = ` + verdictSchema + `
const r = await agent.call('x', { agentType: 'explorer', schema: S, label: 'x' })
return [r.status, r.reason, r.meta.repairUsed]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[unknown dispatch_unknown false]" {
		t.Fatalf("%v", got)
	}
	rows := kinds(e.rows("x"))
	if rows[len(rows)-1] != engine.EvTurnStarted {
		t.Errorf("rows %v: turn_started must stay the last row", rows)
	}
	if n := len(e.requests("turn/start")); n != 2 { // the direct turn and the call's first turn - and no repair
		t.Errorf("%d turn/start requests", n)
	}
	// a message that was read in full and is empty is absent only in the sense that it is a (bad) answer
	e2 := newEnv(t)
	e2.threads.DropNotifications(true)
	e2.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: ""} })
	info2 := startedThread(t, e2, "explorer")
	out, err := runTurn(t, e2, info2, "explorer", 1, "one")
	if err != nil || out.Text != "" {
		t.Errorf("an empty message read in full: %+v %v", out, err)
	}
}

// A turn is identified by its userMessage's clientId first and by the first input line second.
func TestClientUserMessageIdMatchedFirst(t *testing.T) {
	item := func(clientID any, text string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"type": "userMessage", "id": "u", "clientId": clientID, "content": []any{map[string]any{"type": "text", "text": text, "text_elements": []any{}}}})
		return raw
	}
	cases := []struct {
		name       string
		items      []json.RawMessage
		op, source string
		wantOK     bool
	}{
		{"the clientId wins over a different first line", []json.RawMessage{item("c_a/t1", "crw-flow-op: c_b/t1\n\npacket")}, "c_a/t1", agent.MarkerClientID, true},
		{"the first line when the clientId is null", []json.RawMessage{item(nil, "crw-flow-op: c_b/t2\n\npacket")}, "c_b/t2", agent.MarkerFirstLine, true},
		{"the first line when the clientId is empty", []json.RawMessage{item("", "crw-flow-op: c_b/t3\n\npacket")}, "c_b/t3", agent.MarkerFirstLine, true},
		{"no marker at all", []json.RawMessage{item(nil, "just a packet")}, "", "", false},
		{"no user message", []json.RawMessage{json.RawMessage(`{"type":"agentMessage","id":"m","text":"x"}`)}, "", "", false},
	}
	for _, c := range cases {
		op, source, ok := agent.MarkerOf(c.items)
		if op != c.op || source != c.source || ok != c.wantOK {
			t.Errorf("%s: (%q, %q, %v), want (%q, %q, %v)", c.name, op, source, ok, c.op, c.source, c.wantOK)
		}
	}

	// the same rule on turns read from the host
	e := newEnv(t)
	e.threads.DropNotifications(true)
	info := startedThread(t, e, "explorer")
	send := func(params map[string]any) string {
		raw, err := e.client.Call(context.Background(), "turn/start", params)
		if err != nil {
			t.Fatal(err)
		}
		var started struct{ Turn struct{ ID string } }
		if err := json.Unmarshal(raw, &started); err != nil {
			t.Fatal(err)
		}
		return started.Turn.ID
	}
	text := func(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }
	byClient := send(map[string]any{"threadId": info.ID, "input": text("crw-flow-op: c_other/t1\n\nx"), "clientUserMessageId": "c_x/t1"})
	byLine := send(map[string]any{"threadId": info.ID, "input": text("crw-flow-op: c_y/t1\n\nx")})
	// both turns are in progress or ended by now; wait until the host lists them as ended
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, e1 := e.backend.ReadTurn(context.Background(), info.ID, byClient, "")
		_, e2 := e.backend.ReadTurn(context.Background(), info.ID, byLine, "")
		if e1 == nil && e2 == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turns did not end: %v %v", e1, e2)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := e.backend.ReadTurn(context.Background(), info.ID, byClient, "c_x/t1"); err != nil {
		t.Errorf("the clientId names the operation: %v", err)
	}
	var foreign *agent.ForeignTurnError
	if _, err := e.backend.ReadTurn(context.Background(), info.ID, byClient, "c_other/t1"); !errors.As(err, &foreign) || foreign.Got != "c_x/t1" || foreign.Source != agent.MarkerClientID {
		t.Errorf("the first line must not override the clientId: %v", err)
	}
	if _, err := e.backend.ReadTurn(context.Background(), info.ID, byLine, "c_y/t1"); err != nil {
		t.Errorf("the first line names the operation when there is no clientId: %v", err)
	}
	if _, err := e.backend.ReadTurn(context.Background(), info.ID, byLine, "c_x/t1"); !errors.As(err, &foreign) || foreign.Source != agent.MarkerFirstLine {
		t.Errorf("a turn of another operation: %v", err)
	}
	// RunTurn sends the same operation in both places
	out, err := runTurn(t, e, info, "explorer", 7, "seven")
	if err != nil {
		t.Fatal(err)
	}
	last := e.requests("turn/start")
	sent := last[len(last)-1]
	if sent.Params["clientUserMessageId"] != "c_direct/t7" || firstLine(inputText(t, sent)) != "crw-flow-op: c_direct/t7" || out.TurnID == "" {
		t.Errorf("%v", sent.Params)
	}
}

// The thread, read and list answers of the fake follow the schema codex-cli 0.154.0 generates, and no Thread carries
// developerInstructions (the 0.154 Thread has no such member, so it can never be read back).
func TestFakehostThreadMatchesSchema(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "bridge", "appserver", "testdata", "schema-0.154", "v2")
	resolve := func(name string) *jsonschema.Resolved {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r, err := s.Resolve(nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r
	}
	e := newEnv(t)
	ctx := context.Background()
	call := func(method string, params map[string]any) map[string]any {
		raw, err := e.client.Call(ctx, method, params)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	cwd := filepath.Join(e.root, "work")
	start := call("thread/start", map[string]any{"cwd": cwd, "model": "m", "sandbox": "workspace-write", "approvalPolicy": "never", "ephemeral": false,
		"config": map[string]any{"model_reasoning_effort": "high"}, "developerInstructions": "role prompt"})
	id := start["thread"].(map[string]any)["id"].(string)
	// a turn, so the fork and the reads have history
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: passMessage} })
	started := call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "go"}}})
	turn := started["turn"].(map[string]any)["id"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for e.threads.All()[0].Turns[0].Status != "completed" {
		if time.Now().After(deadline) {
			t.Fatal("the turn did not end")
		}
		time.Sleep(5 * time.Millisecond)
	}
	read := call("thread/read", map[string]any{"threadId": id, "includeTurns": false})
	list := call("thread/list", map[string]any{"cwd": cwd})
	fork := call("thread/fork", map[string]any{"threadId": id, "lastTurnId": turn, "cwd": filepath.Join(e.root, "fork"), "ephemeral": false})
	resume := call("thread/resume", map[string]any{"threadId": id, "excludeTurns": true})
	turns := call("thread/turns/list", map[string]any{"threadId": id, "itemsView": "full"})
	items := call("thread/items/list", map[string]any{"threadId": id, "turnId": turn})
	for name, c := range map[string]struct {
		file   string
		answer map[string]any
	}{
		"thread/start":      {"ThreadStartResponse.json", start},
		"thread/read":       {"ThreadReadResponse.json", read},
		"thread/list":       {"ThreadListResponse.json", list},
		"thread/fork":       {"ThreadForkResponse.json", fork},
		"thread/resume":     {"ThreadResumeResponse.json", resume},
		"thread/turns/list": {"ThreadTurnsListResponse.json", turns},
		"thread/items/list": {"ThreadItemsListResponse.json", items},
	} {
		if err := resolve(c.file).Validate(c.answer); err != nil {
			t.Errorf("%s does not match %s: %v", name, c.file, err)
		}
	}
	// the answers reconcile depends on: a seeded thread, an archived one, a filtered and paged listing, an active thread and
	// the interrupt that ends its turn
	forkID := fork["thread"].(map[string]any)["id"].(string)
	seeded := e.threads.Seed(fakehost.SeedThread{Cwd: cwd, Model: "m", Effort: "high", ForkedFromID: id, ParentThreadID: "",
		Turns: []fakehost.SeedTurn{{ClientID: "x/t1", Text: "crw-flow-op: x/t1\n\nwork"}}})
	archive := call("thread/archive", map[string]any{"threadId": forkID})
	e.threads.ListPageSize(1)
	paged := call("thread/list", map[string]any{"cwd": cwd, "sourceKinds": []any{"appServer"}, "limit": 50})
	archivedOnly := call("thread/list", map[string]any{"cwd": filepath.Join(e.root, "fork"), "archived": true})
	seededRead := call("thread/read", map[string]any{"threadId": seeded, "includeTurns": false})
	seededTurns := call("thread/turns/list", map[string]any{"threadId": seeded, "itemsView": "full"})
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Hang: true} })
	running := call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "hang"}}})
	active := call("thread/read", map[string]any{"threadId": id, "includeTurns": false})
	interrupted := call("turn/interrupt", map[string]any{"threadId": id, "turnId": running["turn"].(map[string]any)["id"]})
	for name, c := range map[string]struct {
		file   string
		answer map[string]any
	}{
		"thread/archive":                {"ThreadArchiveResponse.json", archive},
		"turn/interrupt":                {"TurnInterruptResponse.json", interrupted},
		"thread/list (paged, filtered)": {"ThreadListResponse.json", paged},
		"thread/list (archived)":        {"ThreadListResponse.json", archivedOnly},
		"thread/read (seeded)":          {"ThreadReadResponse.json", seededRead},
		"thread/turns/list (seeded)":    {"ThreadTurnsListResponse.json", seededTurns},
		"thread/read (active)":          {"ThreadReadResponse.json", active},
	} {
		if err := resolve(c.file).Validate(c.answer); err != nil {
			t.Errorf("%s does not match %s: %v", name, c.file, err)
		}
	}
	if status := active["thread"].(map[string]any)["status"].(map[string]any)["type"]; status != "active" {
		t.Errorf("a thread with a turn in progress reads %v", status)
	}
	if n := len(paged["data"].([]any)); n != 1 || paged["nextCursor"] == nil {
		t.Errorf("the page holds %d threads with next cursor %v: the page size is capped", n, paged["nextCursor"])
	}
	if got := archivedOnly["data"].([]any); len(got) != 1 {
		t.Errorf("the archived listing holds %d threads", len(got))
	}
	e.threads.ListPageSize(0)
	// the validator is live: a Thread of the wrong shape does not pass
	bad := map[string]any{"thread": map[string]any{"id": 7}}
	if err := resolve("ThreadReadResponse.json").Validate(bad); err == nil {
		t.Errorf("a malformed thread passed the schema")
	}
	// no Thread the fake returns carries developerInstructions, though the fake kept what thread/start was given
	threads := []map[string]any{start["thread"].(map[string]any), read["thread"].(map[string]any), fork["thread"].(map[string]any), resume["thread"].(map[string]any)}
	for _, entry := range list["data"].([]any) {
		threads = append(threads, entry.(map[string]any))
	}
	for i, thread := range threads {
		if _, has := thread["developerInstructions"]; has {
			t.Errorf("thread %d carries developerInstructions", i)
		}
	}
	if v, _ := e.threads.Thread(id); v.DeveloperInstructions != "role prompt" {
		t.Errorf("the fake dropped what thread/start was given: %q", v.DeveloperInstructions)
	}
	if fork["thread"].(map[string]any)["forkedFromId"] != id || len(list["data"].([]any)) != 1 {
		t.Errorf("fork source %v, list %d", fork["thread"].(map[string]any)["forkedFromId"], len(list["data"].([]any)))
	}
	if got := read["thread"].(map[string]any); got["model"] != "m" || got["reasoningEffort"] != "high" || got["cwd"] != cwd {
		t.Errorf("thread/read %v", got)
	}
	// a Thread whose pair is null is valid too
	e.threads.Readback(func(fakehost.ThreadView) (*string, *string) { return nil, nil })
	if err := resolve("ThreadReadResponse.json").Validate(call("thread/read", map[string]any{"threadId": id, "includeTurns": false})); err != nil {
		t.Errorf("a thread with a null pair: %v", err)
	}
}

// Every CodexErrorInfo variant the pinned schema lists is classified by the engine's table, and a name it does not list is
// held host_infra_unknown.
func TestCodexErrorInfoTableMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "bridge", "appserver", "testdata", "schema-0.154", "ServerNotification.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Definitions map[string]struct {
			OneOf []struct {
				Enum       []string                   `json:"enum"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"oneOf"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	var variants []string
	for _, branch := range bundle.Definitions["CodexErrorInfo"].OneOf {
		variants = append(variants, branch.Enum...)
		for name := range branch.Properties {
			variants = append(variants, name)
		}
	}
	if len(variants) < 15 {
		t.Fatalf("variants %v", variants)
	}
	for _, v := range variants {
		if d := engine.ClassifyHostFailure(v); d.Reason == "host_infra_unknown" {
			t.Errorf("the schema variant %s is not in the engine's host failure table", v)
		}
	}
	if d := engine.ClassifyHostFailure("aVariantThePinnedSchemaLacks"); d.Status != engine.StatusHeld || d.Reason != "host_infra_unknown" {
		t.Errorf("%+v", d)
	}
}

// The notification bus carries every turn of a thread, not only its first: the client drops the subscription of a thread it
// started when the first turn ends, so the backend holds it. With the pull fallback an hour away, a call whose repair and
// fix turns come over the bus finishes at once and the host is never asked to unsubscribe.
func TestThreadStaysSubscribedAcrossTurns(t *testing.T) {
	e := newEnv(t, withPoll(time.Hour))
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.N == 1 {
			return fakehost.TurnPlan{Message: "not json"}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	res := e.run(`const S = ` + verdictSchema + `
const a = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
const fix = await agent.call('fix', { resume: a, schema: S, label: 'fix' })
const again = await agent.call('fix again', { resume: fix, schema: S, label: 'again' })
return [a.status, a.meta.repairUsed, fix.status, again.status]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[completed true completed completed]" {
		t.Fatalf("%v", got)
	}
	if n := len(e.requests("turn/start")); n != 4 {
		t.Fatalf("%d turns", n)
	}
	if n := len(e.requests("thread/turns/list")); n != 0 {
		t.Errorf("%d turn reads: every turn should have been heard on the bus", n)
	}
	if got := e.threads.Unsubscribes(); len(got) != 0 {
		t.Errorf("the host was asked to unsubscribe %v while the backend held the threads", got)
	}
	e.backend.Close()
}
