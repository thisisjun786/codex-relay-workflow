package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
)

// One package serves the run surface and `crw flow audit` (the merge of CRW-1205 with CRW-1206): the binary dispatches it once,
// installs the store and host the audit opens, and `crw flow help` lists both.
func TestFlowDispatchesRunSurfaceAndAuditOnce(t *testing.T) {
	ctx := context.Background()
	var out, errOut strings.Builder
	prev := flowcli.AuditEnv
	flowcli.AuditEnv = nil
	t.Cleanup(func() { flowcli.AuditEnv = prev })

	dir := t.TempDir()
	code := run(ctx, "crw", []string{"flow", "audit", "--run", "r", "--state", filepath.Join(dir, "state"), "--socket", filepath.Join(dir, "s.sock")}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "there is no relay store") {
		t.Errorf("crw flow audit on a missing store: exit %d, stderr %q (the binary must install AuditEnv and reach the audit)", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(ctx, "crw", []string{"flow", "ps", "--state", filepath.Join(dir, "state"), "--socket", filepath.Join(dir, "s.sock")}, &out, &errOut); strings.Contains(errOut.String(), "invalid choice") {
		t.Errorf("crw flow ps is not served by the run surface: exit %d, stderr %q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(ctx, "crw", []string{"flow", "help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "crw flow audit (--run RUN") ||
		!strings.Contains(out.String(), "crw flow start WORKFLOW") || strings.Count(out.String(), "crw flow audit (--run RUN") != 1 {
		t.Errorf("crw flow help: %d %q", code, out.String())
	}
}
