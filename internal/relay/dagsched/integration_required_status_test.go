package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (k *batchKit) integrateBatch(t *testing.T) IntegrationBatchResult {
	t.Helper()
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func intentDetail(t *testing.T, k *batchKit, head string) map[string]string {
	t.Helper()
	for _, r := range k.stageRows() {
		if r.Stage != "intent" || r.NodeID != "" {
			continue
		}
		var detail map[string]string
		if err := json.Unmarshal([]byte(r.Detail), &detail); err != nil {
			t.Fatal(err)
		}
		if detail["verified_head"] == head {
			return detail
		}
	}
	t.Fatalf("no intent row for %s", head)
	return nil
}

func TestRequiredStatusMoveRecord(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	res := k.integrateBatch(t)
	detail := intentDetail(t, k, res.NewHead)
	if detail["verification_record"] == "" || detail["verification_base"] == "" {
		t.Fatalf("the intent carries no receipt: %v", detail)
	}
	raw, base, err := k.sched.VerifiedMoveRecord(context.Background(), "dev-int", res.NewHead)
	if err != nil {
		t.Fatalf("VerifiedMoveRecord: %v", err)
	}
	if string(raw) != detail["verification_record"] {
		t.Errorf("the returned bytes are not the intent's record bytes")
	}
	record, err := DecodeVerificationRecord(raw)
	if err != nil {
		t.Fatalf("the retained bytes are not a record: %v", err)
	}
	if record.HeadCommit != res.NewHead || base != record.BaseCommit || base != k.base {
		t.Errorf("record head %q base %q, want head %q base %q", record.HeadCommit, base, res.NewHead, k.base)
	}
	if digest, err := VerificationRecordDigest(raw); err != nil || digest != record.Digest {
		t.Errorf("the retained bytes' canonical digest does not verify")
	}
	for _, bad := range []struct{ ref, commit string }{
		{"dev-int", k.base},
		{"another-ref", res.NewHead},
		{"dev-int", strings.Repeat("0", 40)},
	} {
		if raw, _, err := k.sched.VerifiedMoveRecord(context.Background(), bad.ref, bad.commit); err == nil || raw != nil {
			t.Errorf("VerifiedMoveRecord(%s, %s) = %v, want a refusal", bad.ref, bad.commit, err)
		}
	}
}

func TestRequiredStatusMoveRecordMissingProof(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	res := k.integrateBatch(t)
	ctx := context.Background()
	batch := registry.CoordinationID("dib", "g", "test", "no-record")
	detail := map[string]string{
		"verified_head":       res.NewHead,
		"verification_digest": res.VerificationDigest,
		"integration_ref":     "dev-int",
		"base":                k.base,
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range [][2]string{{"intent", string(raw)}, {"ref_moved", res.NewHead}} {
		if err := store.RecordIntegrationStage(ctx, k.sched.Store, store.IntegrationStageRow{
			StageID: registry.CoordinationID("dis", batch, st[0], "", st[1]), BatchID: batch, PlanID: "g",
			Stage: st[0], Detail: st[1], RecordedBy: "parent", RecordedAt: registry.SystemISO(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := k.sched.VerifiedMoveRecord(ctx, "dev-int", res.NewHead)
	if err == nil || got != nil {
		t.Error("a digest-only intent answered a record")
	}
}

func TestRequiredStatusBeforePushOrdering(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	res := k.integrateBatch(t)
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	ctx := context.Background()
	var atCallback string
	result, err := PushIntegrationWithStatus(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil, nil,
		func(ctx context.Context, checkout, remoteName, remoteRef, integrationRef, local string) error {
			atCallback = remoteBranch(t, remote, remoteRef)
			return nil
		})
	if err != nil || result.Outcome != PushPushed {
		t.Fatalf("push = %+v, %v", result, err)
	}
	if atCallback != "" && atCallback != res.OldHead {
		t.Errorf("at the callback the remote held %q, want the old head or nothing", atCallback)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Errorf("remote dev = %s, want %s", got, res.NewHead)
	}
}

func TestRequiredStatusBeforePushRefusal(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	k.integrateBatch(t)
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	ctx := context.Background()
	if _, err := PushIntegrationWithStatus(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil, nil,
		func(context.Context, string, string, string, string, string) error {
			return errors.New("the status write failed")
		}); err == nil || !strings.Contains(err.Error(), "required integration status") {
		t.Fatalf("a refused callback pushed anyway: %v", err)
	}
	if got := remoteBranch(t, remote, "dev"); got != "" {
		t.Errorf("a refused callback still pushed: remote dev = %s", got)
	}
}

func TestRequiredStatusBeforePushUpToDate(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	res := k.integrateBatch(t)
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	ctx := context.Background()
	k.repo.git("push", "origin", res.NewHead+":refs/heads/dev")
	called := false
	result, err := PushIntegrationWithStatus(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil, nil,
		func(context.Context, string, string, string, string, string) error {
			called = true
			return nil
		})
	if err != nil || result.Outcome != PushUpToDate {
		t.Fatalf("push = %+v, %v", result, err)
	}
	if called {
		t.Error("an up-to-date remote still ran the status callback")
	}
}
