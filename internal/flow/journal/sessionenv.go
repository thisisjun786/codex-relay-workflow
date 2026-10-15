package journal

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/sessionhost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// SessionEnvRequest is what the session environment of a run is made of (CRW-1316 S3, CRW-1348): the relay store the wakes are
// staged in, the App Server client the subagents are read through, the allocator of the run's directories, and the run's
// directory, owner thread and the directory it was started in.
type SessionEnvRequest struct {
	Store     *store.Store
	Client    *appserver.Client
	Allocator *agent.Allocator
	// StateDir is the relay state directory: the run's steps and inbox are under <StateDir>/flow/<run>.
	StateDir, RunID string
	// Owner is the run's owner thread (flow_runs.actor_task_id); OwnerCwd the directory the run was started in.
	Owner, OwnerCwd string
	// BoundDir is the flow-bound directory the marker search reads the hook's ledger from; empty is bound.Dir().
	BoundDir string
	// Journal is the run's journal: what the session host adapter warns about (a marker search that tied a subagent other than by
	// one ledger line) is a session_host_warning row of it (CRW-1372). Nil drops the warnings.
	Journal engine.Journal
}

// SessionEnv is the backend and the session configuration of a session run: SessionBackend over the allocator, the session host
// adapter of internal/flow/sessionhost over the client (FindByMarker reads the hook's ledger of the owner thread), the FileStepBus
// of the run's directory, the owner thread, the trace digests of the backend, and the wake that stages the run: supervisor message
// in the store.
func SessionEnv(req SessionEnvRequest) (*agent.SessionBackend, *engine.SessionConfig, error) {
	if req.Owner == "" {
		return nil, nil, fmt.Errorf("the session run %s names no owner thread", req.RunID)
	}
	backend, err := agent.NewSessionBackend(req.Allocator, req.OwnerCwd)
	if err != nil {
		return nil, nil, err
	}
	dir := req.BoundDir
	if dir == "" {
		if dir, err = bound.Dir(); err != nil {
			return nil, nil, err
		}
	}
	backend.BoundDir = dir
	host := agent.SessionHost{Host: sessionhost.New(req.Client, dir)}
	if j := req.Journal; j != nil {
		host.Warn = func(op string, warnings []string) {
			j.Append(engine.Event{RunID: req.RunID, Kind: engine.KindFlow, Event: EvSessionHostWarning, Data: map[string]any{"op": op, "warnings": warnings}})
		}
	}
	cfg := &engine.SessionConfig{
		Host:  host,
		Bus:   engine.NewFileStepBus(filepath.Join(req.StateDir, "flow", req.RunID)),
		Owner: req.Owner,
		Trace: backend.Trace,
		// the executor steps' evidence assignments are recorded under the directory the run was started in (CRW-1482)
		EvidenceCwd: req.OwnerCwd,
	}
	if req.Store != nil {
		cfg.Wake = StageWake(req.Store)
	}
	return backend, cfg, nil
}

// EvSessionHostWarning is the row of a session host warning (FindByMarker's no_ledger_line, ledger_op_differs, several_spawns,
// window_ended).
const EvSessionHostWarning = "session_host_warning"

// StageWake is the wake of a session run's owner thread (CRW-1316 §10): a session_step_waiting message through run:<id>, which
// the supervisor channel addresses to the run's owner (flow_runs.actor_task_id) and delivers the way it delivers every message: a
// new turn on an idle thread, a resume and a turn on a notLoaded one, after the turn on a busy one (CRW-1333, CRW-1334). The
// subject is <run>/<wake id>, so the same wake staged again is one message; a wake the relay already holds stages nothing, and
// StageWake answers engine.WakeNotStaged with the relay's reason, so the run records staged:false (CRW-1372).
func StageWake(st *store.Store) func(ctx context.Context, w engine.SessionWake) error {
	return func(ctx context.Context, w engine.SessionWake) error {
		steps := "a step"
		if len(w.Ops) != 1 {
			steps = fmt.Sprintf("%d steps", len(w.Ops))
		}
		summary := fmt.Sprintf("run %s has %s waiting for you", w.Run, steps)
		if w.Renotify > 0 {
			summary = fmt.Sprintf("run %s still has %s waiting (no subagent dispatched for %s)", w.Run, steps, w.Waiting.Round(1e9))
		}
		text := summary + ": " + strings.Join(w.Ops, ", ") + ".\n" +
			"Take them: crw flow next " + w.Run + " --json, then spawn_agent each spawn step with the arguments it names and run crw flow dispatched for it."
		answer, err := (&supervisor.Channel{Store: st}).StageDirect(ctx, supervisor.DirectMessage{Kind: supervisor.KindSessionStepWaiting, Relationship: "run:" + w.Run,
			Subject: supervisor.FlowSubject(w.Run, w.ID), Summary: summary, Text: text, Evidence: []string{"crw flow next " + w.Run + " --json"}}, nil)
		if err != nil {
			return err
		}
		if reason, _ := answer["reason"].(string); answer["staged"] != true && reason != "" {
			return engine.WakeNotStaged{Reason: reason}
		}
		return nil
	}
}
