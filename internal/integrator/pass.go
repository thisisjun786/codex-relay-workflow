package integrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Report is what one pass reports.
type Report struct {
	// Result is idle, stopped, locked, nothing-to-integrate, error, or the run's own result
	// (pushed, nothing-merged, verification-failed(rc=N), dev-moved:<sha>, push-failed).
	Result  string   `json:"result"`
	Exit    int      `json:"exit"`
	Error   string   `json:"error,omitempty"`
	Summary *Summary `json:"summary,omitempty"`
	// Moved is where each pending file went, as "<name> -> <dir>: <reason>".
	Moved   []string `json:"moved,omitempty"`
	Ignored []string `json:"ignored,omitempty"`
	// Retry describes the retry step the pass took after a failed verification.
	Retry string `json:"retry,omitempty"`
}

// Pass runs one pass of the queue: the shell runner's loop body. A stop file ends it before it
// reads the queue; an empty queue is idle; a lock another run holds is refused (ExitUsage)
// without reading the queue.
func (r *Runner) Pass(ctx context.Context) Report {
	if fileExists(r.qpath(fileStop)) {
		r.logf("stop file, exiting")
		return Report{Result: "stopped", Exit: ExitPushed}
	}
	if err := r.ensureQueueDirs(); err != nil {
		return Report{Result: "error", Exit: ExitUsage, Error: err.Error()}
	}
	entries, ignored, err := r.listPending()
	if err != nil {
		return Report{Result: "error", Exit: ExitUsage, Error: err.Error()}
	}
	if len(entries) == 0 {
		return Report{Result: "idle", Exit: ExitPushed, Ignored: ignored}
	}
	unlock, err := acquireLock(r.Cfg.LockFile)
	if err != nil {
		if errors.Is(err, ErrLockHeld) {
			return Report{Result: "locked", Exit: ExitUsage, Error: err.Error(), Ignored: ignored}
		}
		return Report{Result: "error", Exit: ExitUsage, Error: err.Error(), Ignored: ignored}
	}
	defer unlock()

	rep := Report{Ignored: ignored}
	// The queue is read again under the lock: another run may have emptied it meanwhile.
	entries, _, err = r.listPending()
	if err != nil || len(entries) == 0 {
		rep.Result, rep.Exit = "idle", ExitPushed
		return rep
	}
	solo := r.soloLeft() > 0
	var batch []Entry
	for _, e := range entries {
		if !e.HeadOK {
			r.logf("%s %q is not a full head SHA: blocked", e.Key, e.Head)
			rep.moved(r, e, dirBlocked, "head is not a 40-hex commit")
			continue
		}
		if note, ok := r.gate(ctx, e); !ok {
			r.logf("%s %s gate not PASS: %s", e.Key, e.Head, note)
			rep.moved(r, e, dirBlocked, "gate not PASS: "+note)
			continue
		}
		batch = append(batch, e)
		if solo {
			break
		}
	}
	if len(batch) == 0 {
		rep.Result, rep.Exit = "nothing-to-integrate", ExitNothingMerged
		return rep
	}

	tag := r.Cfg.TagPrefix + r.Now().Format("0102150405")
	specs := make([]string, len(batch))
	for i, e := range batch {
		specs[i] = e.Spec()
	}
	r.logf("%s start %s", tag, strings.Join(specs, " "))
	out := r.integrate(ctx, tag, batch)
	if out.Err != nil {
		r.logf("%s rc=%d %v", tag, out.Exit, out.Err)
		rep.Result, rep.Exit, rep.Error = "error", out.Exit, out.Err.Error()
		return rep
	}
	rep.Summary, rep.Exit, rep.Result = out.Summary, out.Exit, out.Summary.Result
	data, _ := jsonLine(out.Summary)
	r.logf("%s rc=%d %s", tag, out.Exit, data)

	if out.Exit == ExitPushed || out.Exit == ExitNothingMerged {
		if out.Exit == ExitPushed {
			for _, e := range out.merged {
				rep.moved(r, e, dirDone, "merged")
			}
		}
		for _, l := range out.left {
			// A head that is already in the branch is done; any other left-out head goes back to
			// its owner to refresh.
			if l.Reason == reasonAlreadyInDev {
				rep.moved(r, l.Entry, dirDone, l.Reason)
			} else {
				rep.moved(r, l.Entry, dirConflict, l.Reason)
			}
		}
	}
	r.afterRun(batch, out, solo, &rep)
	return rep
}

func (rep *Report) moved(r *Runner, e Entry, dir, reason string) {
	if r.moveEntry(e, dir) {
		rep.Moved = append(rep.Moved, fmt.Sprintf("%s -> %s: %s", e.Name, dir, reason))
	}
}

// afterRun is the retry policy after a run: a failed batch is retried once as the same batch
// (load flakes), then each of its heads runs alone for as many runs as the batch had members
// (not a bisection), and a head that fails alone is retried once before it is blocked.
func (r *Runner) afterRun(batch []Entry, out Outcome, wasSolo bool, rep *Report) {
	if out.Exit == ExitVerifyFailed {
		switch {
		case len(batch) > 1 && !fileExists(r.batchMarker(batch)):
			if err := r.touch(r.batchMarker(batch)); err != nil {
				r.logf("retry marker: %v", err)
			}
			rep.Retry = "same batch once more"
			r.logf("batch verification failed: one retry of the same batch (%s)", batchKey(batch))
		case len(batch) > 1:
			r.setSoloLeft(len(batch))
			rep.Retry = fmt.Sprintf("one at a time for the next %d runs", len(batch))
			r.logf("batch verification failed: one at a time for the next %d runs", len(batch))
		default:
			e := batch[0]
			marker := r.qpath(dirRetried, e.Name)
			if prev, _ := readHead(marker); prev != e.Head {
				if err := os.MkdirAll(r.qpath(dirRetried), 0o775); err == nil {
					err = os.WriteFile(marker, []byte(e.Head+"\n"), 0o664)
					if err != nil {
						r.logf("retry marker: %v", err)
					}
				}
				rep.Retry = "alone, one retry of the same head"
				r.logf("%s alone failed verification: one retry of %s", e.Key, e.Head)
			} else {
				rep.moved(r, e, dirBlocked, "alone failed verification twice")
				rep.Retry = "alone, blocked"
				r.logf("%s alone failed verification: blocked", e.Key)
			}
		}
	} else if out.Exit == ExitPushed && wasSolo && r.pendingCount() == 0 {
		r.setSoloLeft(0)
	}
	// Solo mode covers only the members of the failed batch, then batching resumes.
	if left := r.soloLeft(); left > 0 && len(batch) == 1 {
		left--
		r.setSoloLeft(left)
		if left == 0 {
			r.logf("solo pass over: batching again")
		}
	}
}

// gate re-checks the pre-merge gate of one head: the configured command's last output line
// starts with PASS and its status is 0. The second result is false for anything else, with the
// line (or the reason it could not run) as the first.
func (r *Runner) gate(ctx context.Context, e Entry) (string, bool) {
	var out bytes.Buffer
	argv := expandAll(r.Cfg.GateCommand, "{issue}", e.Key, "{head}", e.Head)
	rc, err := r.Exec(ctx, Cmd{Argv: argv, Dir: r.Cfg.Checkout, Env: r.gitEnv(), Stdout: &out, Stderr: &out})
	last := lastLine(out.String())
	if err != nil {
		return "gate command: " + err.Error(), false
	}
	if rc != 0 || !strings.HasPrefix(last, "PASS") {
		if last == "" {
			last = fmt.Sprintf("status %d, no output", rc)
		}
		return last, false
	}
	return last, true
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n \t"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Watch runs a pass, waits the configured interval, and repeats until the stop file appears or
// ctx ends. report receives every pass that did something.
func (r *Runner) Watch(ctx context.Context, report func(Report)) error {
	for {
		rep := r.Pass(ctx)
		if rep.Result == "stopped" {
			report(rep)
			return nil
		}
		if rep.Result != "idle" {
			report(rep)
		}
		if err := r.Sleep(ctx, r.Cfg.Interval()); err != nil {
			return err
		}
	}
}
