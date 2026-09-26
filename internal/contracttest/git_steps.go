package contracttest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	_ "modernc.org/sqlite"
)

func (r *gitBridgeRun) seedLegacy(step map[string]any) error {
	input := r.arguments(asObject(step["arguments"]))
	params := map[string]any{"source_repository": input.Source, "starting_revision": input.Revision, "destination": input.Destination, "worktree_mode": input.Mode, "sandbox": input.Sandbox, "expected_sandbox_policy": input.Policy, "prompt": nil, "title": nil, "model": nil, "reasoning_effort": nil, "app_server_project_id": nil}
	fingerprint, err := ledger.Fingerprint("create_worktree_thread", params)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(r.root, "state", "operations.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	raw, err := json.Marshal(step["receipt"])
	if err != nil {
		return err
	}
	_, err = db.ExecContext(context.Background(), "INSERT INTO operations VALUES (?,?,?)", input.RequestID, fingerprint, raw)
	return err
}

func (r *gitBridgeRun) concurrentLaunch(results []any, index int, raw any) {
	input := r.args
	input.RequestID = stringValue(raw)
	receipt, err := r.b.CreateWorktreeThread(context.Background(), input)
	if err != nil {
		results[index] = map[string]any{"exception": "ValueError", "message": err.Error()}
	} else {
		results[index] = receipt
	}
}

func (r *gitBridgeRun) concurrent(step map[string]any) error {
	ids, ok := step["request_ids"].([]any)
	if !ok {
		return fmt.Errorf("concurrent request_ids missing")
	}
	results := make([]any, len(ids))
	paused, release := make(chan struct{}), make(chan struct{})
	r.t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	r.host.Script("thread/start", fakehost.Reply{Result: r.startReply(), Paused: paused, Release: release})
	var group sync.WaitGroup
	group.Go(func() { r.concurrentLaunch(results, 0, ids[0]) })
	select {
	case <-paused:
		r.threads["thread-1"] = asObject(r.startReply()["thread"])
	case <-time.After(5 * time.Second):
		return fmt.Errorf("first concurrent launch never reached host")
	}
	for i := 1; i < len(ids); i++ {
		group.Go(func() { r.concurrentLaunch(results, i, ids[i]) })
	}
	close(release)
	group.Wait()
	r.receipts = append(r.receipts, results...)
	r.record()
	return nil
}
