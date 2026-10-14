package cli

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"os"
	"path/filepath"
)

// UpwardFactory is journal.UpwardFactory of the shipped binary: the run hands the plan summary, its escalations and its completion digest
// to the hook of this package (report.RunHook), which works on the relay store the run's journal is in and tells the orchestrator thread
// with `crw manage send-parent`.
//
// state and socket are the relay the run's store was selected by: the notices name it, so the pump and `crw manage send-parent` work on
// that store and not on the one the manage configuration names.
//
// A run meant to start nodes that finished completed and started none (Blocked over the run's record, the reading `crw flow show` prints)
// is reported to the initiative lead as project_blocked, through the writer `crw flow report-up` and the milestone report work through.
func UpwardFactory(st *store.Store, j *journal.Journal, runID, state, socket string) engine.UpwardHook {
	program, _ := os.Executable()
	rows := report.JournalAppender{Journal: j, RunID: runID, Store: st}
	// the reports, the escalations and the completion digest go up through the supervisor channel (CRW-1380); the plan summary notice,
	// which the channel has no kind for, still goes through crw manage send-parent
	sender := report.RelaySender{Store: st, Fallback: report.ManageSender{Program: program, State: state, Socket: socket}}
	reports := &report.Writer{Store: st, Rows: rows, Resolver: report.StoreResolver{Store: st}, Sender: sender, ArtifactDir: filepath.Join(state, "flow", runID, "reports")}
	return &report.RunHook{Store: st, Sched: &dagsched.Scheduler{Store: st}, Journal: j, Rows: rows, Sender: sender, Run: runID, Reports: reports, Blocked: RunBlocked}
}

// RunBlocked is Blocked over the run record report.RunHook reads (flow_runs): the workflow and the plan the run's arguments name decide
// whether the run is meant to start nodes (StartsNodes).
func RunBlocked(run report.RunInfo, events []engine.Event) (reasons []string, blocked bool) {
	return Blocked(RunRecord{RunID: run.RunID, Workflow: run.Workflow, PlanID: run.PlanID, PlanRevision: run.PlanRevision, Owner: run.Actor, StartedAt: run.StartedAt}, events)
}
