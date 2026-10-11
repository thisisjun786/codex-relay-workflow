package appserver_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
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
	raiseAs(t, host, client, thread, methods, false)
}

// raiseAs is raise with the choice of the request params: the older tests' shape (a threadId on every
// request) or the one the generated schema defines (SchemaParams).
func raiseAs(t *testing.T, host *fakehost.Server, client *appserver.Client, thread string, methods []string, schemaParams bool) {
	t.Helper()
	host.Script("thread/read", fakehost.Reply{Result: map[string]any{"ok": true}, ServerRequests: methods, SchemaParams: schemaParams}, fakehost.Reply{Result: map[string]any{"ok": true}})
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

// CRW-1193. The two legacy approvals name their thread in conversationId, with no threadId: that is how
// codex raises them. The responder reads the conversationId as the thread for its ownership question and
// for the record of its denial; a client without a responder attributes them as it always did.
func TestFlowResponderDeniesOwnedLegacyApprovalsByConversationId(t *testing.T) {
	legacy := []string{"execCommandApproval", "applyPatchApproval"}
	t.Run("owned conversation is denied and recorded on its thread", func(t *testing.T) {
		host := fakehost.Start(t)
		client, err := appserver.Dial(bounded(t), host.SocketPath, withFlowResponder("flow-1"))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		mark := client.RequestMark()

		raiseAs(t, host, client, "flow-1", legacy, true)

		for _, r := range host.ServerRequests() {
			var members map[string]any
			if err := json.Unmarshal(r.Params, &members); err != nil || members["threadId"] != nil || members["conversationId"] != "flow-1" {
				t.Fatalf("%s is not raised as the schema defines it: %s", r.Method, r.Params)
			}
		}
		report := client.RequestsSince(mark, "flow-1")
		if len(report.ThisThread) != 2 || report.DeniedByResponderForThisThread != 2 || report.ApprovalsLeftForThisThread != 0 || len(report.Unattributed) != 0 {
			t.Fatalf("%+v", report)
		}
		for i, r := range report.ThisThread {
			if r.Method != legacy[i] || !r.Approval || r.Answered != appserver.AnsweredByResponder || r.ThreadID != "flow-1" {
				t.Fatalf("record %+v", r)
			}
		}
		answers := decodeAnswers(t, host)
		if len(answers) != 2 {
			t.Fatalf("answers = %s", host.Answers())
		}
		for i, a := range answers {
			if a.Result == nil || !denies(legacy[i], a.Result) {
				t.Fatalf("answer %d to %s does not deny: %s", i, legacy[i], host.Answers()[i].Raw)
			}
		}
	})

	t.Run("a conversation nobody owns keeps the default path", func(t *testing.T) {
		outcomeOf := func(options ...appserver.Option) (appserver.RequestReport, int) {
			host := fakehost.Start(t)
			client, err := appserver.Dial(bounded(t), host.SocketPath, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			mark := client.RequestMark()
			raiseAs(t, host, client, "desktop-1", legacy, true)
			report := client.RequestsSince(mark, "desktop-1")
			if len(report.ThisThread) != 0 || len(report.Unattributed) != 2 {
				t.Fatalf("legacy approvals of a conversation are attributed by threadId only: %+v", report)
			}
			for i := range report.Unattributed {
				report.Unattributed[i].At = 0
			}
			return report, len(host.Answers())
		}
		plain, plainAnswers := outcomeOf()
		withResponder, responderAnswers := outcomeOf(withFlowResponder("flow-1"))
		if plainAnswers != 0 || responderAnswers != 0 || !reflect.DeepEqual(plain, withResponder) {
			t.Fatalf("answers %d/%d\n plain     %+v\n responder %+v", plainAnswers, responderAnswers, plain, withResponder)
		}
		for _, r := range plain.Unattributed {
			if r.Answered != "left_for_thread_approver" {
				t.Fatalf("record %+v", r)
			}
		}
	})

	t.Run("a client without a responder leaves an owned-looking conversation alone", func(t *testing.T) {
		host := fakehost.Start(t)
		client, err := appserver.Dial(bounded(t), host.SocketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		mark := client.RequestMark()
		raiseAs(t, host, client, "flow-1", legacy, true)
		if report := client.RequestsSince(mark, "flow-1"); len(report.ThisThread) != 0 || len(host.Answers()) != 0 {
			t.Fatalf("%+v", report)
		}
	})
}

// CRW-1193. The note of a report says what happened to the requests it lists: a report that holds a
// request the responder denied does not claim that nothing was denied; every other report keeps the
// default note.
func TestRequestsReportNoteFollowsTheResponder(t *testing.T) {
	host := fakehost.Start(t)
	client, err := appserver.Dial(bounded(t), host.SocketPath, withFlowResponder("flow-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mark := client.RequestMark()
	raise(t, host, client, "flow-1", []string{"item/commandExecution/requestApproval"})
	raise(t, host, client, "desktop-1", []string{"item/commandExecution/requestApproval"})

	denied := client.RequestsSince(mark, "flow-1")
	left := client.RequestsSince(mark, "desktop-1")
	empty := client.RequestsSince(client.RequestMark(), "flow-1")

	if denied.DeniedByResponderForThisThread != 1 || strings.Contains(denied.Note, "Nothing here was granted or denied") || !strings.Contains(denied.Note, "denied") {
		t.Fatalf("denied report note: %q", denied.Note)
	}
	if left.Note != empty.Note || !strings.Contains(left.Note, "Nothing here was granted or denied") {
		t.Fatalf("default note changed:\n left  %q\n empty %q", left.Note, empty.Note)
	}
	// A client without a responder reports the default note, byte for byte.
	plainHost := fakehost.Start(t)
	plain, err := appserver.Dial(bounded(t), plainHost.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	plainMark := plain.RequestMark()
	raise(t, plainHost, plain, "flow-1", []string{"item/commandExecution/requestApproval"})
	if got := plain.RequestsSince(plainMark, "flow-1").Note; got != left.Note {
		t.Fatalf("plain note %q != default %q", got, left.Note)
	}
}

// CRW-1193. The responder answers by refusing and records a refusal. A custom answer callback that
// returns anything else (a grant, a user's answer, a tool result) is not sent and not recorded as a
// denial: the request takes the default handling, so the record never says "nothing was granted" about
// a request that was granted.
func TestFlowResponderRecordsOnlyDenials(t *testing.T) {
	grant := func(req appserver.ServerRequest) (appserver.ServerAnswer, bool) {
		switch req.Method {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
			return appserver.ServerAnswer{Result: map[string]any{"decision": "accept"}}, true
		case "mcpServer/elicitation/request":
			return appserver.ServerAnswer{Result: map[string]any{"action": "accept"}}, true
		case "item/permissions/requestApproval":
			return appserver.ServerAnswer{Result: map[string]any{"permissions": map[string]any{"network": map[string]any{"enabled": true}}, "scope": "turn"}}, true
		case "execCommandApproval", "applyPatchApproval":
			return appserver.ServerAnswer{Result: map[string]any{"decision": "approved"}}, true
		case "item/tool/requestUserInput":
			return appserver.ServerAnswer{Result: map[string]any{"answers": map[string]any{}}}, true
		}
		return appserver.ServerAnswer{Result: map[string]any{"ok": true}}, true
	}
	t.Run("a granting answer is not sent and not recorded as a denial", func(t *testing.T) {
		host := fakehost.Start(t)
		client, err := appserver.Dial(bounded(t), host.SocketPath, appserver.WithServerRequestResponder(func(string) bool { return true }, grant))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		mark := client.RequestMark()
		methods := append(slices.Clone(fakehost.ApprovalMethods[:]), "item/tool/call")
		raiseAs(t, host, client, "flow-1", methods, false)

		report := client.RequestsSince(mark, "flow-1")
		if report.DeniedByResponderForThisThread != 0 || report.ApprovalsLeftForThisThread != 7 || report.RefusedForThisThread != 1 {
			t.Fatalf("%+v", report)
		}
		if strings.Contains(report.Note, "nothing was granted") {
			t.Fatalf("the report claims a denial: %q", report.Note)
		}
		for _, a := range decodeAnswers(t, host) {
			if a.Result != nil {
				t.Fatalf("the responder sent a non-denial result: %v", a.Result)
			}
		}
	})
	t.Run("the protocol's own denial forms and an error are denials", func(t *testing.T) {
		host := fakehost.Start(t)
		answer := func(req appserver.ServerRequest) (appserver.ServerAnswer, bool) {
			switch req.Method {
			case "item/commandExecution/requestApproval":
				return appserver.ServerAnswer{Result: map[string]any{"decision": "cancel"}}, true
			case "mcpServer/elicitation/request":
				return appserver.ServerAnswer{Result: map[string]any{"action": "cancel"}}, true
			case "execCommandApproval":
				return appserver.ServerAnswer{Result: map[string]any{"decision": "abort"}}, true
			case "applyPatchApproval":
				return appserver.ServerAnswer{Result: map[string]any{"decision": map[string]any{"denied": map[string]any{"rejection": "no"}}}}, true
			case "item/tool/call":
				return appserver.ServerAnswer{Error: &appserver.ServerError{Code: -32601, Message: "no"}}, true
			}
			return appserver.ServerAnswer{}, false
		}
		client, err := appserver.Dial(bounded(t), host.SocketPath, appserver.WithServerRequestResponder(func(string) bool { return true }, answer))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		mark := client.RequestMark()
		raiseAs(t, host, client, "flow-1", []string{"item/commandExecution/requestApproval", "mcpServer/elicitation/request", "execCommandApproval", "applyPatchApproval", "item/tool/call"}, true)
		report := client.RequestsSince(mark, "flow-1")
		if report.DeniedByResponderForThisThread != 5 || report.ApprovalsLeftForThisThread != 0 || report.RefusedForThisThread != 0 {
			t.Fatalf("%+v", report)
		}
	})
}
