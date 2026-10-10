package appserver_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

type answerFrame struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAnswers(t *testing.T, host *fakehost.Server) []answerFrame {
	t.Helper()
	var out []answerFrame
	for _, a := range host.Answers() {
		var f answerFrame
		if err := json.Unmarshal(a.Raw, &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

// raise makes the host raise methods about the thread named in a thread/read call, and fences the
// delivery of the client's answers with a second round trip.
func raise(t *testing.T, host *fakehost.Server, client *appserver.Client, thread string, methods []string) {
	t.Helper()
	host.Script("thread/read", fakehost.Reply{Result: map[string]any{"ok": true}, ServerRequests: methods}, fakehost.Reply{Result: map[string]any{"ok": true}})
	params := map[string]any{}
	if thread != "" {
		params["threadId"] = thread
	}
	if _, err := client.Call(bounded(t), "thread/read", params); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(bounded(t), "thread/read", map[string]any{"threadId": "fence"}); err != nil {
		t.Fatal(err)
	}
}

func withFlowResponder(owned ...string) appserver.Option {
	return appserver.WithServerRequestResponder(func(thread string) bool { return slices.Contains(owned, thread) }, appserver.DenyAnswer)
}

// CRW-1193. The flow backend's responder answers the approval-class requests of the threads the
// backend started, with a refusal it records; the same connection's other threads keep their
// approver.
func TestFlowResponderDeniesOnlyOwnedThreads(t *testing.T) {
	// Given: a client with the responder installed for thread "flow-1", and a host that raises all seven
	// approval-class requests about "flow-1" and then about "desktop-1" on the same connection.
	host := fakehost.Start(t)
	client, err := appserver.Dial(bounded(t), host.SocketPath, withFlowResponder("flow-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mark := client.RequestMark()
	methods := fakehost.ApprovalMethods[:]

	// When
	raise(t, host, client, "flow-1", methods)
	raise(t, host, client, "desktop-1", methods)

	// Then: the owned thread's seven requests were answered, as denials, and recorded as such.
	owned := client.RequestsSince(mark, "flow-1")
	if len(owned.ThisThread) != 7 || owned.DeniedByResponderForThisThread != 7 || owned.ApprovalsLeftForThisThread != 0 {
		t.Fatalf("owned thread: %+v", owned)
	}
	for _, r := range owned.ThisThread {
		if !r.Approval || r.Answered != appserver.AnsweredByResponder {
			t.Fatalf("record %+v", r)
		}
	}
	// The other thread's seven are untouched: recorded as before, answered by nobody here.
	other := client.RequestsSince(mark, "desktop-1")
	if len(other.ThisThread) != 7 || other.ApprovalsLeftForThisThread != 7 || other.DeniedByResponderForThisThread != 0 {
		t.Fatalf("other thread: %+v", other)
	}
	for _, r := range other.ThisThread {
		if r.Answered != "left_for_thread_approver" {
			t.Fatalf("record %+v", r)
		}
	}
	// The host saw exactly seven answers (the owned thread's), each a refusal of its own request.
	answers := decodeAnswers(t, host)
	if len(answers) != 7 {
		t.Fatalf("answers = %d, want 7: %s", len(answers), host.Answers())
	}
	raised := host.ServerRequests()
	for i, a := range answers {
		var id string
		if err := json.Unmarshal(a.ID, &id); err != nil || raised[i].Method != methods[i] || id != raised[i].ID {
			t.Fatalf("answer %d (%s) does not answer request %+v", i, a.ID, raised[i])
		}
		if a.Error == nil && a.Result == nil {
			t.Fatalf("answer %d is empty", i)
		}
		if a.Result != nil && !denies(raised[i].Method, a.Result) {
			t.Fatalf("answer %d to %s grants or is unknown: %v", i, raised[i].Method, a.Result)
		}
	}
	// A compatibility refusal of the default path is not made by the responder.
	if refusals := client.RefusalsSince(mark, "flow-1"); len(refusals) != 0 {
		t.Fatalf("responder answers are not default refusals: %+v", refusals)
	}
}

// denies states, per method, what an answer that refuses looks like.
func denies(method string, result map[string]any) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return result["decision"] == "decline"
	case "mcpServer/elicitation/request":
		return result["action"] == "decline"
	case "item/permissions/requestApproval":
		granted, _ := result["permissions"].(map[string]any)
		return len(granted) == 0
	case "execCommandApproval", "applyPatchApproval":
		decision, _ := result["decision"].(map[string]any)
		_, ok := decision["denied"]
		return ok
	}
	return false
}

// CRW-1193. A request the responder cannot attribute, or that is not about an owned thread, is
// recorded and answered exactly as a client without a responder does.
func TestUnattributedRequestKeepsDefault(t *testing.T) {
	type outcome struct {
		Methods  []string
		Records  []appserver.RequestRecord
		Answers  []string
		Refusals []appserver.RequestRecord
	}
	scenario := func(options ...appserver.Option) outcome {
		host := fakehost.Start(t)
		client, err := appserver.Dial(bounded(t), host.SocketPath, options...)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		mark := client.RequestMark()
		methods := append(slices.Clone(fakehost.ApprovalMethods[:]), "item/tool/call")
		raise(t, host, client, "", methods)          // no threadId at all
		raise(t, host, client, "flow-9", methods)    // a thread nobody owns
		raise(t, host, client, "desktop-1", methods) // another connection's thread
		var o outcome
		for _, r := range host.ServerRequests() {
			o.Methods = append(o.Methods, r.Method)
		}
		for _, thread := range []string{"", "flow-9", "desktop-1"} {
			report := client.RequestsSince(mark, thread)
			o.Records = append(o.Records, report.ThisThread...)
			o.Refusals = append(o.Refusals, client.RefusalsSince(mark, thread)...)
		}
		for _, a := range host.Answers() {
			o.Answers = append(o.Answers, string(a.Raw))
		}
		// The index and the time are the client's own bookkeeping; the rest is compared.
		for i := range o.Records {
			o.Records[i].At = 0
		}
		for i := range o.Refusals {
			o.Refusals[i].At = 0
		}
		return o
	}

	plain := scenario()
	withResponder := scenario(withFlowResponder("flow-1"))

	if len(plain.Records) != 24 || len(plain.Answers) != 3 {
		t.Fatalf("the default path changed: %d records, %d answers", len(plain.Records), len(plain.Answers))
	}
	// Answer ids differ between two hosts only by their counters, which are the same here: the
	// frames, the records and the refusal stream must be identical.
	if !reflect.DeepEqual(plain, withResponder) {
		t.Fatalf("responder changed unowned handling:\n plain     %+v\n responder %+v", plain, withResponder)
	}
}

// CRW-1193. A client without a responder is the shared default; ownership is judged per request, so
// a thread the backend stops owning goes back to its approver.
func TestFlowResponderOwnershipIsJudgedPerRequest(t *testing.T) {
	host := fakehost.Start(t)
	owned := map[string]bool{"flow-1": true}
	client, err := appserver.Dial(bounded(t), host.SocketPath, appserver.WithServerRequestResponder(func(thread string) bool { return owned[thread] }, appserver.DenyAnswer))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mark := client.RequestMark()
	raise(t, host, client, "flow-1", []string{"item/commandExecution/requestApproval"})
	delete(owned, "flow-1")
	raise(t, host, client, "flow-1", []string{"item/commandExecution/requestApproval"})
	report := client.RequestsSince(mark, "flow-1")
	if report.DeniedByResponderForThisThread != 1 || report.ApprovalsLeftForThisThread != 1 {
		t.Fatalf("%+v", report)
	}
}

// The responder may decline to answer a method; the default then applies to it.
func TestFlowResponderDeclinedMethodKeepsDefault(t *testing.T) {
	host := fakehost.Start(t)
	client, err := appserver.Dial(bounded(t), host.SocketPath, appserver.WithServerRequestResponder(
		func(string) bool { return true },
		func(req appserver.ServerRequest) (appserver.ServerAnswer, bool) {
			return appserver.ServerAnswer{}, false
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mark := client.RequestMark()
	raise(t, host, client, "flow-1", []string{"item/commandExecution/requestApproval", "item/tool/call"})
	report := client.RequestsSince(mark, "flow-1")
	if report.ApprovalsLeftForThisThread != 1 || report.RefusedForThisThread != 1 || report.DeniedByResponderForThisThread != 0 {
		t.Fatalf("%+v", report)
	}
}
