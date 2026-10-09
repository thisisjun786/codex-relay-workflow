package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

// lostResumeRPC answers every call as mcpRPC does except thread/resume, whose answer is lost after the
// request went out.
type lostResumeRPC struct{ *mcpRPC }

func (r lostResumeRPC) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := r.mcpRPC.Call(ctx, method, params)
	if method == "thread/resume" {
		return nil, errors.New("connection lost after the frame went out")
	}
	return raw, err
}

func lostResumeAdapter(t *testing.T, rpc bridge.RPC, policy bridge.ExecutionPolicy) *Adapter {
	t.Helper()
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "go.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: rpc, Ledger: l, Policy: policy})
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// CRW-1000 (review P2): a resume that carried the limit and whose answer was lost is outcome_unknown,
// and its receipt still says what went out: the limit under requested and unobservable. The record was
// written only after thread/resume returned, so the one send whose effect is unknown was the one that
// recorded nothing.
func TestAResumeWhoseAnswerWasLostStillRecordsTheLimitItSent(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(map[bool]string{false: "with-pair", true: "settings-free"}[free], func(t *testing.T) {
			record := childRecord(free, "")
			rpc := &mcpRPC{}
			a := lostResumeAdapter(t, lostResumeRPC{rpc}, autoCompactChildPolicy(t, record))
			receipt := sendRecord(t, a, "send-lost-resume", record)
			if receipt["status"] != "outcome_unknown" {
				t.Fatalf("receipt=%v", receipt)
			}
			if got := resumeConfig(rpc)[settings.AutoCompactTokenLimitKey]; got != int64(550000) {
				t.Fatalf("the resume did not carry the limit: %v", resumeConfig(rpc))
			}
			if value, present := autoCompactSentValue(t, receipt); !present || value != 550000 || !autoCompactMarkedUnobservable(t, receipt) {
				t.Fatalf("the receipt of the resume whose answer was lost does not record the limit it sent: %v", receipt)
			}
			if verified := autoCompactStrings(autoCompactSettings(t, receipt)["verified"]); len(verified) != 0 {
				t.Fatalf("the limit was recorded as verified: %v", verified)
			}
			stored, err := a.GetOperation(context.Background(), "send-lost-resume")
			if err != nil {
				t.Fatal(err)
			}
			if value, present := autoCompactSentValue(t, plain(stored).(map[string]any)); !present || value != 550000 {
				t.Fatalf("the stored receipt lost the limit: %v", stored)
			}
		})
	}
}

// CRW-1000 (review P3): the actual snapshot of the settings observation is taken after the MCP servers
// were read back, so it carries mcpServers beside the resume's own settings.
func TestTheSettingsObservationOfAResumeCarriesTheServersReadBack(t *testing.T) {
	record := childRecord(false, "ui-qa")
	rpc := &mcpRPC{configured: []string{"node_repl", "oracle"}, applied: true}
	a := mcpAdapter(t, rpc, autoCompactProfilePolicy(t, record))
	receipt := sendRecord(t, a, "send-servers-in-actual", record)
	if receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v", receipt)
	}
	actual, _ := autoCompactSettings(t, receipt)["actual"].(map[string]any)
	if _, present := actual["mcpServers"]; !present {
		t.Fatalf("the actual snapshot lacks mcpServers although the servers were read back: %v", actual)
	}
}
