package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// reportApproval makes the resume of an unloaded thread report the given approval policy besides the pair unloadedReporting set.
func reportApproval(t *testing.T, host *fakehost.Server, model, effort, approval string) {
	t.Helper()
	unloadedReporting(t, host, model, effort)
	resume := startReply(t.TempDir())
	resume.Result["model"], resume.Result["reasoningEffort"], resume.Result["approvalPolicy"] = model, effort, approval
	host.Respond("thread/resume", resume)
}

// CRW-1398 item 4: a settings finding on the resume of an unloaded thread of a role with several pairs refuses the send before the pair is
// compared, and the failed receipt, and the record the ledger holds before a turn would start, still carry pairCheck: what the host reported
// for the resumed thread, whether it equals the request's or not.
func TestCRW1398AFailedReceiptOfAnUnloadedMultiPairSendCarriesPairCheck(t *testing.T) {
	for name, reported := range map[string][2]string{
		"the host reports the request's pair": {pyModel, pyEffort},
		"the host reports the role's other":   {multipairSol, "xhigh"},
	} {
		t.Run(name, func(t *testing.T) {
			b, host := policyBridge(t, multipairPolicy)
			reportApproval(t, host, reported[0], reported[1], "on-request") // the contract's approval policy is never
			input := SendMessage{RequestID: "child-send", ThreadID: "thread-1", Message: "work", Role: "child", Expected: map[string]any{"model": pyModel, "reasoning_effort": pyEffort}}
			receipt, err := b.SendMessageToThread(context.Background(), input)
			rpc := pyjson.Map(receipt["rpcError"])
			if err != nil || receipt["status"] != "failed" || rpc["code"] != settings.UnsupportedApproval || host.Count("turn/start") != 0 {
				t.Fatalf("receipt=%v err=%v; want the approval finding to refuse the send", receipt, err)
			}
			resumeCarriesNoPair(t, host)
			stored, err := b.Ledger.Get(context.Background(), "child-send")
			if err != nil {
				t.Fatal(err)
			}
			for where, r := range map[string]map[string]any{"the failed receipt": receipt, "the ledger record": stored} {
				check := pyjson.Map(r["pairCheck"])
				if check == nil || check["match"] != (reported == [2]string{pyModel, pyEffort}) || check["transmitted"] != false ||
					pyjson.Map(check["reported"])["model"] != reported[0] || pyjson.Map(check["reported"])["reasoningEffort"] != reported[1] ||
					pyjson.Map(check["requested"])["model"] != pyModel {
					t.Errorf("%s: pairCheck %v; want what the host reported %v", where, r["pairCheck"], reported)
				}
			}
		})
	}
}

// CRW-1398 item 4: the record the ledger holds when the turn starts (saved before turn/start) already carries pairCheck, so a send that dies
// between the resume and the turn leaves what the host reported.
func TestCRW1398ThePreTurnLedgerRecordCarriesPairCheck(t *testing.T) {
	b, host := policyBridge(t, multipairPolicy)
	unloadedReporting(t, host, pyModel, pyEffort)
	var held map[string]any
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		stored, err := b.Ledger.Get(context.Background(), "child-send")
		if err != nil {
			t.Errorf("the ledger record at turn/start: %v", err)
		}
		held = stored
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}}
	})
	input := SendMessage{RequestID: "child-send", ThreadID: "thread-1", Message: "work", Role: "child", Expected: map[string]any{"model": pyModel, "reasoning_effort": pyEffort}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if check := pyjson.Map(held["pairCheck"]); check == nil || check["match"] != true || pyjson.Map(check["reported"])["model"] != pyModel {
		t.Fatalf("the record held at turn/start: %v; want pairCheck with the host's report", held["pairCheck"])
	}
}
