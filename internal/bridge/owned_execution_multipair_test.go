package bridge

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-454: a role that lists several pairs leaves the choice between them to the task, so a request
// that matches one of them is authorized but is not what the policy pinned. An unloaded thread
// resumed with such a pair could have it moved back from a pair the user chose since, so the send is
// withheld, as it is for a supervisor; a role pinned to one pair is still sent to.
func Test_a_pair_of_a_role_that_lists_several_is_not_sent_to_a_thread_the_host_has_not_loaded(t *testing.T) {
	const sol = "gpt-6.1-sol"
	policy := `{"roles":{"parent":{"model":"` + parentModel + `","reasoningEffort":"` + parentEffort + `"},"child":{"pairs":[{"model":"` + pyModel + `","reasoningEffort":"` + pyEffort + `"},{"model":"` + sol + `","reasoningEffort":"xhigh"}]}}}`
	unloaded := func(host *fakehost.Server, model, effort string) {
		host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
		resume := startReply(t.TempDir())
		resume.Result["model"], resume.Result["reasoningEffort"] = model, effort
		host.Respond("thread/resume", resume)
		host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	}
	for _, pair := range [][2]string{{pyModel, pyEffort}, {sol, "xhigh"}} {
		b, host := policyBridge(t, policy)
		unloaded(host, pair[0], pair[1])
		input := SendMessage{RequestID: "child-send", ThreadID: "thread-1", Message: "work", Role: "child", Expected: map[string]any{"model": pair[0], "reasoning_effort": pair[1]}}
		receipt, err := b.SendMessageToThread(context.Background(), input)
		if err != nil || receipt["status"] != "failed" || pyjson.Map(receipt["rpcError"])["code"] != "unverified_pair_for_unloaded_thread" || host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 {
			t.Fatalf("%v: receipt=%v err=%v", pair, receipt, err)
		}
	}
	b, host := policyBridge(t, policy)
	unloaded(host, parentModel, parentEffort)
	input := SendMessage{RequestID: "parent-send", ThreadID: "thread-1", Message: "work", Role: "parent", Expected: map[string]any{"model": parentModel, "reasoning_effort": parentEffort}}
	if receipt, err := b.SendMessageToThread(context.Background(), input); err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 {
		t.Fatalf("a one-pair role is no longer sent to: receipt=%v err=%v", receipt, err)
	}
}
