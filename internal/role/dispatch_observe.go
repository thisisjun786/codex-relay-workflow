package role

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// dispatchObservation is one read-only look at a recorded child: who the host says it is, the status of its thread and how
// its newest turn stands. It is what the stopped close and the fallback handoff share. It is a snapshot taken now and never a
// reservation: the child may start another turn the moment after it is read.
type dispatchObservation struct {
	// Foreign is set when the host answered for the id with a thread that is not a subagent of this session.
	Foreign bool
	// Status is the thread status the App Server reported ("" when it did not answer for the child).
	Status string
	// Newest is the status of the child's newest turn: the App Server's turn list (completed, interrupted, failed,
	// inProgress, or another value it reports), or the host's rollout of the child, which shows only an ended turn
	// (completed or interrupted). "" when neither showed a turn.
	Newest string
	// Source names where Newest was read: "app-server", "native-rollout" or "".
	Source string
}

// dispatchTerminalTurn reports whether a newest turn status is one that has ended.
func dispatchTerminalTurn(status string) bool {
	return slices.Contains([]string{"completed", "interrupted", "failed"}, status)
}

// dispatchObserve reads the recorded child agent of session under one bound. The identity comes from the App Server when
// there is a host and it answers, else from the native thread database. The newest turn comes from the App Server's turn
// list when it knows the child, and otherwise from the child's rollout, the host's own record of the child's turns, which a
// native helper the App Server cannot see still writes. The bridge's derivation is used: the thread status and the newest
// turn are two facts, and neither stands for the other. activeTurns asks for the newest turn of a thread the App Server reports
// active too; the stopped close, which refuses an active child whatever its turns say, does not ask.
func dispatchObserve(ctx context.Context, env host.LookupEnv, h DispatchHost, session, agent string, activeTurns bool) dispatchObservation {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	own := func(id createdCheckIdentity) bool { return id.ID == agent && id.Parent == session && id.Subagent }
	var o dispatchObservation
	if h != nil {
		if identity, err := createdCheckRead(ctx, env, h, agent); err == nil {
			if !own(identity) {
				o.Foreign = true
				return o
			}
			o.Status = identity.Status
			if o.Status == "active" && !activeTurns {
				return o
			}
			if o.Newest = createdRuntimeStatus(ctx, h, agent); o.Newest != "" {
				o.Source = "app-server"
				return o
			}
		}
	}
	native, err := createdCheckNative(ctx, env, agent)
	if err != nil {
		return o
	}
	if !own(native) {
		// The App Server answered for its own child but the database disagrees: the observation is contradictory, and the
		// turn the App Server could not show stays unseen. Without an App Server answer the database decides.
		o.Foreign = o.Status == ""
		return o
	}
	if native.RolloutPath != "" {
		if o.Newest = dispatchRolloutNewest(native.RolloutPath); o.Newest != "" {
			o.Source = "native-rollout"
		}
	}
	return o
}

// dispatchRolloutBeforeOpen is called between the look at a rollout path and its open; a test replaces the path there.
var dispatchRolloutBeforeOpen func()

// dispatchOpenRollout opens a rollout the host wrote, for reading it as evidence. The path is looked at without following a
// link, opened without following one and without blocking, and the descriptor that was opened must be the regular file that was
// looked at, as the ledger's own reader requires of a record: a path replaced in between, by a link to another rollout, another
// file or a named pipe, is refused instead of read as the child's.
func dispatchOpenRollout(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("rollout is not a regular file")
	}
	if dispatchRolloutBeforeOpen != nil {
		dispatchRolloutBeforeOpen()
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(info, opened)) {
		err = errors.New("rollout is not the file that was looked at")
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// dispatchRolloutNewest reads the child's rollout, which the host appends to, and returns how its newest started turn ended:
// "completed" (task_complete), "interrupted" (turn_aborted), or "" when no turn started, the newest one has not ended in the
// file, or a damaged or unfinished line leaves it unclear how the newest turn stands. A turn that has not ended in the file
// may still be running, or its process may be gone; the rollout cannot tell, so it is never read as in progress. Only regular
// files are read, line by line.
func dispatchRolloutNewest(path string) string {
	f, err := dispatchOpenRollout(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	newest, ended := "", ""
	for {
		line, err := r.ReadBytes('\n')
		// Every line the host wrote whole parses. A line that does not, terminated or not, is damage or a write in progress,
		// and nothing in it says which record it was: it may be the start or the end of a turn, so what was read before it no
		// longer shows how the newest turn stands, and the end stays unseen until a later turn starts and ends whole. A line
		// is classified by its decoded type, never by the text it contains.
		if len(bytes.TrimSpace(line)) > 0 {
			var event struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Turn string `json:"turn_id"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &event) != nil {
				newest, ended = "", ""
			} else if event.Type == "event_msg" {
				switch p := event.Payload; p.Type {
				case "task_started":
					newest, ended = p.Turn, ""
				case "task_complete", "turn_aborted":
					if newest != "" && p.Turn == newest {
						ended = map[string]string{"task_complete": "completed", "turn_aborted": "interrupted"}[p.Type]
					}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return ""
			}
			break
		}
	}
	return ended
}

// dispatchHandoffGate is the gate of a failed or task_failed report through the checked boundary: the recorded child must be
// seen to have ended before its attempt is handed on. A child of another parent, an active child and a newest turn in progress
// refuse; a contradiction or a child whose end could not be seen leaves the attempt to reconcile, its id kept. A seen end is
// recorded on the attempt as the observation it was.
func dispatchHandoffGate(ctx context.Context, env host.LookupEnv, h DispatchHost) dispatchGate {
	return func(d *Dispatch) (*DispatchResult, error) {
		a := &d.Attempts[len(d.Attempts)-1]
		o := dispatchObserve(ctx, env, h, d.SessionID, *a.AgentID, true)
		reconcile := func(reason string) (*DispatchResult, error) {
			a.Status = "reconcile"
			r := dispatchResult(d, "reconcile", reason)
			return &r, nil
		}
		switch {
		case o.Foreign:
			return nil, errors.New("recorded child is not a subagent of this session; the handoff cannot confirm it ended")
		case o.Newest == "inProgress":
			return nil, errors.New("recorded child has a turn in progress; stop it before handoff")
		case o.Status == "active" && dispatchTerminalTurn(o.Newest):
			return reconcile("the child's thread is active but its newest turn ended; observe it again before handoff")
		case o.Status == "active":
			return nil, errors.New("recorded child is active; stop it before handoff")
		case !dispatchTerminalTurn(o.Newest):
			return reconcile("the child's end could not be observed; confirm it stopped, then report again")
		}
		a.Termination = &DispatchTermination{Newest: o.Newest, ThreadStatus: o.Status, Source: o.Source, Note: dispatchTerminationNote}
		return nil, nil
	}
}

// dispatchTerminationNote says what a recorded termination is and is not.
const dispatchTerminationNote = "observed at report time; not a reservation of the workspace"

// DispatchTermination is the observation a handoff relied on: the newest turn's status, the thread status when the App Server
// gave one, and where the turn was read.
type DispatchTermination struct {
	Newest       string `json:"newestTurn"`
	ThreadStatus string `json:"threadStatus,omitempty"`
	Source       string `json:"source"`
	Note         string `json:"note"`
}

// DispatchCleanup is the outstanding cleanup of a child whose dispatch a policy stopped: "pending" when the stop left the
// child recorded and unaccounted for, "unconfirmed" once cleanup evidence was submitted but the child's end could not be
// seen (its id stays held), and "confirmed" once the child was seen to have ended (the attempt is failed and its id free).
// The dispatch stays stopped in every state: the policy ended the dispatch, the cleanup only accounts for the child.
type DispatchCleanup struct {
	Status   string `json:"status"`
	Evidence string `json:"evidence,omitempty"`
	Newest   string `json:"newestTurn,omitempty"`
	Source   string `json:"source,omitempty"`
	Note     string `json:"note,omitempty"`
}
