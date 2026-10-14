package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1374 d4: the daemon's fault sweep makes no network call, and so no git call, even over a store that holds a recorded integration
// target with an observed head, the one thing dag-integration-target-check would read the remote for. The proof is behavioural: a git
// stand-in first on PATH records every call and fails it, the sweep the daemon runs (the one daemonFactory builds) records its faults
// over that store, and the stand-in is never called. The same stand-in is then reached by the check itself, so the silence above is the
// sweep's and not a stand-in that nothing can reach. This replaces the source-string proof of CRW-1304 (TestCheckTargetIsNotADaemonStep,
// which stays as a second, weaker guard).
func TestTheDaemonSweepMakesNoGitOrRemoteCall(t *testing.T) {
	ctx := context.Background()
	sweeper, ledger, s := daemonSweepWorld(t, ctx, "")
	work := t.TempDir()
	for _, query := range []string{
		"INSERT INTO dag_integration_targets (target_id, kind, checkout, integration_ref, remote, remote_ref, plan_id, observed_head, source, recorded_by, recorded_at)" +
			" VALUES ('dit-target', 'target', '" + work + "', 'dev-int', 'origin', 'dev', 'g', '', 'record', 'parent', '2026-10-14T00:00:00+00:00')",
		"INSERT INTO dag_integration_targets (target_id, kind, checkout, integration_ref, remote, remote_ref, plan_id, observed_head, source, recorded_by, recorded_at)" +
			" VALUES ('dit-observed', 'observed', '" + work + "', 'dev-int', 'origin', 'dev', 'g', '" + strings.Repeat("a", 40) + "', 'check', 'parent', '2026-10-14T00:00:01+00:00')",
	} {
		if _, err := s.Q(ctx).ExecContext(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	bin, called := t.TempDir(), filepath.Join(t.TempDir(), "git-calls")
	script := "#!/bin/sh\necho \"git $*\" >> '" + called + "'\necho 'the git stand-in must not be called by a daemon sweep' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	batch, err := sweeper.Sweep(ctx, "crw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sweeper.RecordAll(ctx, ledger, batch); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(called); err == nil {
		t.Fatalf("the daemon sweep called git:\n%s", raw)
	}
	if len(batch.Observations) == 0 {
		t.Fatal("the sweep observed nothing, so it proves nothing about what it does not call")
	}

	// The control: the check reads the same target through the same stand-in, so the stand-in is reachable.
	readings, err := (&dagsched.Scheduler{Store: s}).CheckRecordedTargets(ctx, "", "")
	if err != nil || len(readings) != 1 || readings[0].Status != dagsched.TargetUnreadable {
		t.Fatalf("the check over the stand-in: %+v, %v; want one unreadable reading", readings, err)
	}
	raw, err := os.ReadFile(called)
	if err != nil || !strings.Contains(string(raw), "ls-remote") {
		t.Fatalf("the check did not reach the stand-in (%v): %q", err, raw)
	}
}
