//go:build agysmoke

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// TestSmokeRealAgy runs crw review once on a small diff with the real agy and its account, model and host-wide lock (only the ledger and the output directory are
// temporary). Built only with the agysmoke tag, to be run by hand once: go test -tags agysmoke -run TestSmokeRealAgy -v ./internal/review/command
func TestSmokeRealAgy(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"sum.go": sumSource("_, x := range xs", "x")})
	head := r.commit(map[string]string{"sum.go": sumSource("i := 1; i < len(xs); i++", "xs[i]")}) // skips the first number
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	var out, errOut bytes.Buffer
	code := Run(ctx, []string{"--repo", r.dir, "--base", base, "--head", head, "--issue", "CRW-506", "--out", filepath.Join(tmp, "out"), "--state-dir", filepath.Join(tmp, "state")}, &out, &errOut)
	t.Logf("exit %d\nsummary %s\nstderr %s", code, out.String(), errOut.String())
	var s Summary
	if err := json.Unmarshal(out.Bytes(), &s); err != nil || code != 0 {
		t.Fatalf("crw review did not finish: %v", err)
	}
	data, err := os.ReadFile(s.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	a, err := review.ParseArtifact(data, head)
	if err != nil {
		t.Fatalf("the artifact does not validate: %v", err)
	}
	t.Logf("artifact: status=%s reason=%q model=%s agyVersion=%s reviewers=%+v findings=%d dropped=%d started=%s finished=%s", a.Status, a.Reason, a.Model, a.AgyVersion, a.Reviewers, len(a.Findings), len(a.Dropped), a.StartedAt, a.FinishedAt)
	for _, c := range a.Calls {
		t.Logf("call %s reviewer=%d chunk=%d %s/%s served=%q elapsed=%dms tokens=%+v", c.Stage, c.Reviewer, c.Chunk, c.Class, c.Reason, c.Model, c.ElapsedMillis, c.Tokens)
	}
	if a.Status == review.StatusUnavailable {
		t.Fatalf("no reviewer returned a result: %s", a.Reason)
	}
}

func sumSource(loop, term string) string {
	return "package sum\n\n// Sum adds the numbers.\nfunc Sum(xs []int) int {\n\tt := 0\n\tfor " + loop + " {\n\t\tt += " + term + "\n\t}\n\treturn t\n}\n"
}
