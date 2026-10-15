// Package cli is the run surface of the flow engine: `crw flow start`, `ps`, `show`, `watch`, `answer`, `pause`, `cancel`,
// `status-line`, `digest`, the run process they start, `crw flow audit` (CRW-1206), the audit of the reviews behind a run's
// acceptances, and the commands that reach up from a run (CRW-1204): `report-up`, `reports`, `report-ack` and
// `report-delivered`, with the upward hook the binary installs into a run (UpwardFactory). The engine runs a script; this package
// is what starts a run as a process of its own, finds it again in the relay store, follows and controls it, and applies an answer
// to a wait of it.
//
// A run is a process that lives for one run and loses nothing when it dies (docs/relay/dag-scheduler.md): its journal is in the
// relay store (internal/flow/journal), its run lock is a file lock under <state>/flow/<run>/, and `crw flow resume` and
// `crw flow recover` take a run whose lock is free from the journal again. The commands that read (`ps`, `show`, `watch`) open
// the store read-only and never create it. The commands that write a run's journal (`answer`, and the run process itself) hold
// the run lock, so the journal has one writer at a time. An answer to a run a process holds (CRW-1492) asks that process to stop at
// its next boundary (SIGUSR2: Control.Drain, the turns in flight end and the next requests are held), waits --wait for the run lock
// and appends under it once the process let the run go; otherwise it answers run_draining (the stop is asked: a process an older crw
// runs drops the signal and goes on), and the same answer run again once the run is paused applies and resumes it.
//
// A process ends a run with a finished row, or, when the operator paused it (`crw flow pause`, CRW-1487) or an answer drained it
// (CRW-1492), with a run/paused row and no finished row: the process exits 0, nothing is told finished, `ps` and `show` read the
// run paused while no process holds it, a follow of it ends, and `crw flow resume` (or, for a drained run, the answer it stopped
// for) goes on with it. A pause of the run's own limits (a deadline, a token budget, a quota) finishes the run completed with its
// calls held.
//
// The reference of the commands is plugins/crw/skills/crw-canon/references/flow.md.
package cli
