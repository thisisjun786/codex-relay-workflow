package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// multipairPolicy is a policy whose child role lists two pairs and whose parent role is pinned to one.
const multipairSol = "gpt-6.1-sol"

var multipairPolicy = `{"roles":{"parent":{"model":"` + parentModel + `","reasoningEffort":"` + parentEffort + `"},"child":{"pairs":[{"model":"` + pyModel + `","reasoningEffort":"` + pyEffort + `"},{"model":"` + multipairSol + `","reasoningEffort":"xhigh"}]}}}`

// unloadedReporting answers a thread the host has not loaded, whose resume reports the given pair ("" leaves the field out).
func unloadedReporting(t *testing.T, host *fakehost.Server, model, effort string) {
	t.Helper()
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	resume := startReply(t.TempDir())
	delete(resume.Result, "model")
	delete(resume.Result, "reasoningEffort")
	if model != "" {
		resume.Result["model"] = model
	}
	if effort != "" {
		resume.Result["reasoningEffort"] = effort
	}
	host.Respond("thread/resume", resume)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
}

// resumeCarriesNoPair fails unless the one thread/resume the host received names neither a model nor an effort.
func resumeCarriesNoPair(t *testing.T, host *fakehost.Server) {
	t.Helper()
	if host.Count("thread/resume") != 1 {
		t.Fatalf("%d resumes; want one", host.Count("thread/resume"))
	}
	params := hostParams(t, host, "thread/resume")
	config := pyjson.Map(params["config"])
	if _, ok := params["model"]; ok || config["model_reasoning_effort"] != nil {
		t.Fatalf("the resume transmitted a pair: %v", params)
	}
}

// CRW-454, CRW-1311: a role that lists several pairs leaves the choice between them to the task, so a request that matches one of them is
// authorized (role_pair) but is not what the policy pinned. A thread the host has not loaded is resumed without transmitting the pair, as relay
// delivery resumes it, and the turn starts only when the model and effort the host reports for the resumed thread equal the request's: the
// request cannot move the thread back from a pair the user chose since. A role pinned to one pair is still sent to with its pair.
func Test_a_pair_of_a_role_that_lists_several_is_sent_to_an_unloaded_thread_only_when_the_host_reports_it(t *testing.T) {
	for _, pair := range [][2]string{{pyModel, pyEffort}, {multipairSol, "xhigh"}} {
		b, host := policyBridge(t, multipairPolicy)
		unloadedReporting(t, host, pair[0], pair[1])
		input := SendMessage{RequestID: "child-send", ThreadID: "thread-1", Message: "work", Role: "child", Expected: map[string]any{"model": pair[0], "reasoning_effort": pair[1]}}
		receipt, err := b.SendMessageToThread(context.Background(), input)
		if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 {
			t.Fatalf("%v: the host reports the request's pair: receipt=%v err=%v", pair, receipt, err)
		}
		resumeCarriesNoPair(t, host)
		check := pyjson.Map(receipt["pairCheck"])
		if check["match"] != true || pyjson.Map(check["reported"])["model"] != pair[0] || pyjson.Map(check["reported"])["reasoningEffort"] != pair[1] ||
			pyjson.Map(check["requested"])["model"] != pair[0] || check["transmitted"] != false {
			t.Fatalf("%v: pairCheck %v", pair, check)
		}
	}
	b, host := policyBridge(t, multipairPolicy)
	unloadedReporting(t, host, parentModel, parentEffort)
	input := SendMessage{RequestID: "parent-send", ThreadID: "thread-1", Message: "work", Role: "parent", Expected: map[string]any{"model": parentModel, "reasoning_effort": parentEffort}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 || receipt["pairCheck"] != nil {
		t.Fatalf("a one-pair role is no longer sent to as before: receipt=%v err=%v", receipt, err)
	}
	if params := hostParams(t, host, "thread/resume"); params["model"] != parentModel {
		t.Fatalf("a one-pair role's resume no longer carries its pair: %v", params)
	}
}

// CRW-454, CRW-1311: a pair of a role that lists several is not sent to a thread the host has not loaded unless the host reports it: when
// the host reports another pair for the resumed thread, or reports none, no turn is started and the send is refused as
// unverified_pair_for_unloaded_thread naming both pairs; the receipt records what the host reported. (Before CRW-1311 the send was refused
// before any resume; the resume now carries no pair, so it cannot move the thread.)
func Test_a_pair_of_a_role_that_lists_several_is_not_sent_to_a_thread_the_host_has_not_loaded(t *testing.T) {
	for name, reported := range map[string][2]string{
		"the role's other pair": {multipairSol, "xhigh"},
		"another effort":        {pyModel, "low"},
		"no pair in the answer": {"", ""},
		"a model and no effort": {pyModel, ""},
	} {
		t.Run(name, func(t *testing.T) {
			b, host := policyBridge(t, multipairPolicy)
			unloadedReporting(t, host, reported[0], reported[1])
			input := SendMessage{RequestID: "child-send", ThreadID: "thread-1", Message: "work", Role: "child", Expected: map[string]any{"model": pyModel, "reasoning_effort": pyEffort}}
			receipt, err := b.SendMessageToThread(context.Background(), input)
			rpc := pyjson.Map(receipt["rpcError"])
			if err != nil || receipt["status"] != "failed" || rpc["code"] != "unverified_pair_for_unloaded_thread" || host.Count("turn/start") != 0 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			resumeCarriesNoPair(t, host)
			message := pyjson.Text(rpc["message"])
			for _, want := range []string{pyModel + "/" + pyEffort, "no turn was started"} {
				if !strings.Contains(message, want) {
					t.Errorf("the refusal %q does not name %q", message, want)
				}
			}
			if reported[0] != "" && !strings.Contains(message, reported[0]+"/") {
				t.Errorf("the refusal %q does not name the reported model %q", message, reported[0])
			}
			check := pyjson.Map(receipt["pairCheck"])
			if check["match"] != false || pyjson.Map(check["reported"])["model"] != nullableText(reported[0]) || pyjson.Map(check["reported"])["reasoningEffort"] != nullableText(reported[1]) {
				t.Fatalf("pairCheck %v; want the host's report %v", check, reported)
			}
		})
	}
}
