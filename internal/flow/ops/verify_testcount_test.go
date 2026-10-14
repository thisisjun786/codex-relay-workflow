package ops

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// TestVerificationWithNoTestRunWarnsAndKeepsItsResult (CRW-1328): the verifier reads the count of test packages the record says ran
// (testPackagesRun, additive in verification-record/1): a record without it is unknown, never zero. A verification that ran no test is
// recorded on the crw.verify row with the warning no_tests_run, and the final gate warns of it in its outcome; the pass is the record's
// and the gate's state is unchanged.
func TestVerificationWithNoTestRunWarnsAndKeepsItsResult(t *testing.T) {
	tree := toolsTree(t)
	for name, tc := range map[string]struct {
		record string
		want   *int
	}{
		"none ran": {`{"schema":"verification-record/1","result":"pass","digest":"sha256:ff","jobs":[],"testPackagesRun":0}`, ptrInt(0)},
		"three":    {`{"schema":"verification-record/1","result":"pass","digest":"sha256:ff","jobs":[],"testPackagesRun":3}`, ptrInt(3)},
		"unknown":  {toolsPassRecord, nil},
	} {
		run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (int, []byte, error) {
			return 0, nil, os.WriteFile(args[slices.Index(args, "--record")+1], []byte(tc.record), 0o644)
		}
		v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, RecordDir: t.TempDir(), Run: run}
		out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: strings.Repeat("a", 40), Worktree: tree})
		if err != nil || !out.Pass || (out.TestPackagesRun == nil) != (tc.want == nil) || (tc.want != nil && *out.TestPackagesRun != *tc.want) {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}

	// the crw.verify row
	w := newWorld(t)
	ver := fakeVerifier{out: VerifyOutcome{Pass: true, Command: CILocalCommand, Digest: "sha256:ab", TestPackagesRun: ptrInt(0)}}
	w.lane.d.Verifier = &ver
	var res VerifyResult
	_ = json.Unmarshal(w.must("verify", "A", strings.Repeat("a", 40)), &res)
	row := w.j.Events()[len(w.j.Events())-1]
	if !res.Pass || row.Data["warning"] != WarnNoTestsRun || row.Data["test_packages_run"] != float64(0) || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], WarnNoTestsRun) {
		t.Errorf("a verification that ran no test: %v (%+v)", row.Data, res)
	}
	ver.out.TestPackagesRun = nil
	res = VerifyResult{}
	_ = json.Unmarshal(w.must("verify", "A", strings.Repeat("b", 40)), &res)
	if row := w.j.Events()[len(w.j.Events())-1]; row.Data["warning"] != nil || row.Data["test_packages_run"] != nil || len(res.Warnings) != 0 {
		t.Errorf("an unknown count warns: %v (%+v)", row.Data, res)
	}

	// the final gate over a head whose verification ran no test passes, and says so
	cw := newCloseWorld(t, Milestone{})
	cw.verifier.testPackages = ptrInt(0)
	got := cw.close()
	if got["state"] != "closed" {
		t.Fatalf("the close: %v", got)
	}
	gates := cw.gates()
	g := gates[len(gates)-1]
	if g.State != GatePass || len(g.Warnings) != 1 || !strings.Contains(g.Warnings[0], WarnNoTestsRun) || g.Evidence[0].Warning != WarnNoTestsRun || !g.Evidence[0].Pass {
		t.Errorf("the gate over a verification that ran no test: %+v", g)
	}
	cw2 := newCloseWorld(t, Milestone{})
	cw2.verifier.testPackages = ptrInt(4)
	cw2.close()
	if gates := cw2.gates(); gates[len(gates)-1].State != GatePass || len(gates[len(gates)-1].Warnings) != 0 || gates[len(gates)-1].Evidence[0].Warning != "" {
		t.Errorf("a gate over tests that ran warns: %+v", gates[len(gates)-1])
	}
}

func ptrInt(n int) *int { return &n }
