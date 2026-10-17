package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func commandPrefixScript(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "integrate.sh")
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "sh '" + script + "'"
}

func mergeAllCommand(t *testing.T) string {
	return commandPrefixScript(t, "set -eu\ngit merge --no-ff --no-edit -m command-merge \"$1\"\n")
}

func failOnFileCommand(t *testing.T, file string) string {
	return commandPrefixScript(t, "set -eu\nif git cat-file -e \"$CRW_INTEGRATE_HEAD:"+file+"\" 2>/dev/null; then exit 9; fi\ngit merge --no-ff --no-edit -m command-merge \"$1\"\n")
}

func verifiedIntents(t *testing.T, k *batchKit) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, row := range k.stageRows() {
		if row.Stage != "intent" || row.NodeID != "" {
			continue
		}
		var detail map[string]string
		if json.Unmarshal([]byte(row.Detail), &detail) != nil || detail["verified_head"] == "" {
			continue
		}
		detail["_batch"] = row.BatchID
		out = append(out, detail)
	}
	return out
}

func intentCriteria(t *testing.T, detail map[string]string) map[string]bool {
	t.Helper()
	criteria := map[string]bool{}
	var raw map[string]string
	if err := json.Unmarshal([]byte(detail["criteria"]), &raw); err != nil {
		t.Fatalf("the intent's criteria do not decode: %v", err)
	}
	for node := range raw {
		criteria[node] = true
	}
	return criteria
}

func TestReviewSuccessfulCommandPrefixCanBeRecovered(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	commandConfig(t, k.repo.path, "true", failOnFileCommand(t, "b.txt"))
	in := k.batchIn()
	in.IntegrationRef = "dev"
	ctx := context.Background()
	if _, err := k.sched.IntegrateBatch(ctx, in, IntegrationDeps(stubVerifier(writeStubVerifier(t)))); err == nil {
		t.Fatal("expected b failure")
	}
	tip := k.repo.git("rev-parse", "dev")
	if tip == k.base {
		t.Fatal("the command's verified move was undone")
	}
	if ok, err := commitIsAncestor(ctx, k.repo.path, k.heads["a"], tip); err != nil || !ok {
		t.Fatalf("the retained tip does not contain a: %v %v", ok, err)
	}
	if ok, err := commitIsAncestor(ctx, k.repo.path, k.heads["b"], tip); err != nil || ok {
		t.Fatalf("the retained tip already contains b: %v %v", ok, err)
	}
	if n := countStage(k.stageRows(), "marked"); n != 0 {
		t.Fatalf("a failed batch marked %d candidates", n)
	}
	intents := verifiedIntents(t, k)
	if len(intents) != 1 || intents[0]["verified_head"] != tip {
		t.Fatalf("verified-head rows %v; want exactly the retained tip %s", intents, tip)
	}
	criteria := intentCriteria(t, intents[0])
	if !criteria["a"] || criteria["b"] {
		t.Fatalf("the prefix's criteria %v; want a covered and b absent", criteria)
	}
	receipt, err := DecodeVerificationRecord([]byte(intents[0]["verification_record"]))
	if err != nil || receipt.Result != "pass" || receipt.HeadCommit != tip {
		t.Fatalf("the retained receipt does not prove %s: %v %+v", tip, err, receipt)
	}
	in.Nodes = []string{"a"}
	res, err := k.sched.IntegrateBatch(ctx, in, IntegrationDeps(stubVerifier(writeStubVerifier(t))))
	if err != nil || len(res.MarkedEvents) != 1 || len(res.ContainedUnverified) != 0 {
		t.Fatalf("verified prefix cannot resume: contained=%+v, marked=%v, err=%v", res.ContainedUnverified, res.MarkedEvents, err)
	}
	if got := k.repo.git("rev-parse", "dev"); got != tip {
		t.Fatalf("the resume moved the target: %s -> %s", tip, got)
	}
	raw, _, err := k.sched.VerifiedMoveRecord(ctx, "dev", tip)
	if err != nil {
		t.Fatalf("the reconciled move has no receipt: %v", err)
	}
	proof, err := DecodeVerificationRecord(raw)
	if err != nil || proof.Result != "pass" || proof.HeadCommit != tip {
		t.Fatalf("the receipt of %s: %v %+v", tip, err, proof)
	}
}

func TestCommandIntegrationRecoversLatestVerifiedPrefix(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b"}}, batchNode{name: "c", files: map[string]string{"c.txt": "c"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	k.acceptByCommit("c")
	commandConfig(t, k.repo.path, "true", failOnFileCommand(t, "c.txt"))
	in := k.batchIn()
	in.IntegrationRef = "dev"
	ctx := context.Background()
	if _, err := k.sched.IntegrateBatch(ctx, in, IntegrationDeps(stubVerifier(writeStubVerifier(t)))); err == nil {
		t.Fatal("expected c failure")
	}
	tip := k.repo.git("rev-parse", "dev")
	intermediate := k.repo.git("rev-parse", tip+"^1")
	intents := verifiedIntents(t, k)
	byHead := map[string]map[string]string{}
	for _, d := range intents {
		byHead[d["verified_head"]] = d
	}
	if len(byHead) != 2 || byHead[intermediate] == nil || byHead[tip] == nil {
		t.Fatalf("verified-head rows %v; want the intermediate %s and the latest %s", byHead, intermediate, tip)
	}
	latest := intentCriteria(t, byHead[tip])
	if !latest["a"] || !latest["b"] || latest["c"] {
		t.Fatalf("the latest prefix's criteria %v; want a,b covered and c absent", latest)
	}
	in.Nodes = []string{"a", "b"}
	res, err := k.sched.IntegrateBatch(ctx, in, IntegrationDeps(stubVerifier(writeStubVerifier(t))))
	if err != nil || len(res.MarkedEvents) != 2 || len(res.ContainedUnverified) != 0 {
		t.Fatalf("resume: marked=%v contained=%+v err=%v", res.MarkedEvents, res.ContainedUnverified, err)
	}
	if len(res.Reconciled) != 1 || len(res.Abandoned) != 0 {
		t.Fatalf("reconciled=%v abandoned=%v; want the old batch reconciled and none abandoned", res.Reconciled, res.Abandoned)
	}
	if got := k.repo.git("rev-parse", "dev"); got != tip {
		t.Fatalf("the resume moved the target: %s -> %s", tip, got)
	}
	if _, _, err := k.sched.VerifiedMoveRecord(ctx, "dev", tip); err != nil {
		t.Fatalf("the latest verified prefix has no receipt: %v", err)
	}
}

func TestCommandIntegrationDoesNotCertifyAFailedPrefix(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	commandConfig(t, k.repo.path, "test ! -e b.txt", mergeAllCommand(t))
	in := k.batchIn()
	in.IntegrationRef = "dev"
	ctx := context.Background()
	if _, err := k.sched.IntegrateBatch(ctx, in, IntegrationDeps(stubVerifier(writeStubVerifier(t)))); err == nil {
		t.Fatal("expected verification to fail on the b tree")
	}
	tip := k.repo.git("rev-parse", "dev")
	if tip == k.base {
		t.Fatal("the command's actual move was undone")
	}
	intermediate := k.repo.git("rev-parse", tip+"^1")
	intents := verifiedIntents(t, k)
	if len(intents) != 1 || intents[0]["verified_head"] != intermediate {
		t.Fatalf("verified-head rows %v; want only the intermediate %s", intents, intermediate)
	}
	if n := countStage(k.stageRows(), "marked"); n != 0 {
		t.Fatalf("a batch whose last tree failed marked %d candidates", n)
	}
	failedRecord := false
	for _, row := range k.stageRows() {
		var detail map[string]any
		if json.Unmarshal([]byte(row.Detail), &detail) != nil {
			continue
		}
		if raw, ok := detail["verification_record"].(string); ok {
			if rec, err := DecodeVerificationRecord([]byte(raw)); err == nil && rec.Result == "fail" && rec.HeadCommit == tip {
				failedRecord = true
			}
		}
	}
	if !failedRecord {
		t.Error("the failed tree's receipt was not retained")
	}
}
