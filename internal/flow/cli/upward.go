package cli

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"os"
)

// UpwardFactory is journal.UpwardFactory of the shipped binary: the run hands the plan summary, its escalations and its completion digest
// to the hook of this package (report.RunHook), which works on the relay store the run's journal is in and tells the orchestrator thread
// with `crw manage send-parent`.
//
// state and socket are the relay the run's store was selected by: the notices name it, so the pump and `crw manage send-parent` work on
// that store and not on the one the manage configuration names.
func UpwardFactory(st *store.Store, j *journal.Journal, runID, state, socket string) engine.UpwardHook {
	program, _ := os.Executable()
	return &report.RunHook{Store: st, Sched: &dagsched.Scheduler{Store: st}, Journal: j, Rows: report.JournalAppender{Journal: j, RunID: runID, Store: st}, Sender: report.ManageSender{Program: program, State: state, Socket: socket}, Run: runID}
}
