package role

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"io"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// DispatchHost is the existing App Server read method, never a creation surface.
type DispatchHost interface {
	Call(context.Context, string, map[string]any) (json.RawMessage, error)
}

// The correction every created refusal ends with, in the two halves a refusal can use on its own. Their concatenation is the
// text a recorded refusal of the dispatch CLI fixtures pins, so it does not change.
const (
	createdCheckCopy       = "; copy agentId from the native spawn result and ensure the native thread database is readable"
	createdCheckClose      = "; close an old wrong-ID report with outcome stopped, executionState stopped and reconciliation"
	createdCheckCorrection = createdCheckCopy + createdCheckClose
)

// CheckedDispatch is the product boundary. RunDispatch stays the parity ledger.
// Its report checks run after domain validation, inside the same lock, before
// atomic publication, and a refusal writes nothing: the created report needs the
// attempt's issuance and a child tied to it (createdCheckReporter), a reviewer's
// complete needs that tie (createdCheckComplete), a failed or task_failed handoff
// of a recorded child needs its observed end (dispatchHandoffGate), and the stopped
// report closes or cleans up (createdCheckStop).
func CheckedDispatch(ctx context.Context, cwd string, input any, env host.LookupEnv, h DispatchHost) (DispatchResult, error) {
	return dispatchPinnedChecked(ctx, cwd, input, env, h, nil)
}

// dispatchPinnedChecked is CheckedDispatch with the after callback of the pinned walk (see dispatchPinnedDir).
func dispatchPinnedChecked(ctx context.Context, cwd string, input any, env host.LookupEnv, h DispatchHost, after func(string)) (DispatchResult, error) {
	if env == nil {
		env = os.LookupEnv
	}
	b, err := dispatchRecord(input)
	if err != nil {
		return DispatchResult{}, err
	}
	if dispatchIs(b["action"], "report") && dispatchIs(b["outcome"], "stopped") {
		return createdCheckStop(ctx, cwd, b, env, h, after)
	}
	if dispatchIs(b["action"], "report") && (dispatchIs(b["outcome"], "failed") || dispatchIs(b["outcome"], "task_failed")) {
		gate := dispatchHandoffGate(ctx, env, h)
		return dispatchPinnedRunHeld(cwd, input, env, nil, after, false, func(_ *dispatchPinnedDir, _ string, d *Dispatch, b map[string]any) (DispatchResult, error) {
			r, err := dispatchReport(d, b, gate)
			if err == nil && dispatchIs(d.Status, "stopped") {
				r = dispatchPolicyStopCleanup(d, r)
			}
			return r, err
		})
	}
	if dispatchIs(b["action"], "report") && dispatchIs(b["outcome"], "complete") {
		return dispatchPinnedRunHeld(cwd, input, env, nil, after, false, createdCheckComplete)
	}
	if !dispatchIs(b["action"], "report") || !dispatchIs(b["outcome"], "created") {
		return RunDispatch(cwd, input, env)
	}
	return dispatchPinnedRunHeld(cwd, input, env, nil, after, true, createdCheckReporter(ctx, env, h))
}

// createdCheckReporter is the created report at the checked boundary. The ledger's own validation runs first; then, before
// anything is written and without a host call until the record's own checks pass: a toolUseId the report names must be the
// native call the attempt was issued to, and the attempt must have been issued by the spawn hook, unless the report takes the
// explicit reconciliation path for a child spawned without it. Then the session's other records must not hold the id, the
// host must witness a subagent of this session, and the child is tied to the issued call (createdCheckTie). The accepted
// report writes the attempt's receipt; a report of a child whose receipt is already spawn-result keeps that receipt.
func createdCheckReporter(ctx context.Context, env host.LookupEnv, h DispatchHost) dispatchReporter {
	return func(dir *dispatchPinnedDir, name string, d *Dispatch, b map[string]any) (DispatchResult, error) {
		a := &d.Attempts[len(d.Attempts)-1]
		issued, tool, prior := a.SpawnIssued, a.ToolUseID, a.Receipt
		if _, err := dispatchReport(d, b, nil); err != nil {
			return DispatchResult{}, err
		}
		agent := *a.AgentID
		if v, present := b["toolUseId"]; present {
			if s, ok := v.(string); !ok || tool == nil || *tool != s {
				return DispatchResult{}, errors.New("toolUseId is not the native call this attempt was issued to" + createdCheckCopy)
			}
		}
		issuance := DispatchIssuance{Recorded: issued, ToolUseID: tool}
		if !issued {
			if _, present := b["reconciliation"]; !present {
				return DispatchResult{}, errors.New("created requires this attempt's managed spawn issuance: spawn with the claimed marker so the spawn hook issues it; " +
					"a child spawned without the hook is recorded only with reconciliation evidence, and it cannot satisfy independent review")
			}
			s, err := dispatchSmallText(b["reconciliation"], "reconciliation evidence")
			if err != nil {
				return DispatchResult{}, err
			}
			issuance.Reconciliation = &s
		}
		if err := createdArchivedReplay(dir, name, d.SessionID, a.ID, agent); err != nil {
			return DispatchResult{}, err
		}
		if prior != nil && prior.Correlation == "spawn-result" && prior.Child.AgentID == agent {
			// The same child reported again: the evidence that tied it to the issued call stays as it was recorded, whatever the
			// host shows now. Only the caller's own claim is refreshed.
			prior.ObservedModel = a.ObservedModel
			a.Receipt = prior
			return dispatchResult(d, "wait", ""), nil
		}
		identity, err := createdCheckRead(ctx, env, h, agent)
		if err != nil {
			return DispatchResult{}, errors.New("host could not verify created agent" + createdCheckCorrection)
		}
		if identity.ID != agent || identity.Parent != d.SessionID || !identity.Subagent {
			return DispatchResult{}, errors.New("agentId is not a real subagent thread parented by this session" + createdCheckCorrection)
		}
		witness, native := "app-server", identity
		if h == nil {
			witness = "native-thread-database"
		} else if native, err = createdCheckNative(ctx, env, agent); err != nil || native.ID != agent || native.Parent != d.SessionID {
			native = createdCheckIdentity{}
		}
		correlation := "unissued"
		if issued {
			if correlation, err = createdCheckTie(ctx, env, d, a, agent, native.FirstMessage); err != nil {
				return DispatchResult{}, err
			}
		}
		settings := DispatchHostSettings{Source: "unobservable"}
		for _, v := range []struct {
			value string
			dst   **string
		}{{native.Model, &settings.Model}, {native.Effort, &settings.Effort}} {
			if v.value != "" {
				value := v.value
				*v.dst, settings.Source = &value, "native-thread-database"
			}
		}
		a.Receipt = &DispatchReceipt{Candidate: a.Candidate, Issuance: issuance, Child: DispatchChild{AgentID: agent, Parent: identity.Parent, Witness: witness},
			Correlation: correlation, ObservedModel: a.ObservedModel, Host: settings}
		reason := ""
		switch correlation {
		case "unverified":
			reason = "the host does not show the issued spawn's result naming this child, so it cannot satisfy independent review; report created again once it does"
		case "unissued":
			reason = "recorded without issuance on caller reconciliation; this child cannot satisfy independent review"
		}
		return dispatchResult(d, "wait", reason), nil
	}
}

// createdCheckTie decides whether the child agent of an issued attempt is the child the issued native call created. Two things
// the host shows refuse it on their own: a first message that carries no marker of this attempt, and a child the host already
// showed with the marker when the hook issued the attempt (PriorChildren). The marker itself is written by the caller into the
// call's message, so it names the attempt but is not the call's result: a child made before the issuance or by another call
// (the hook off) can carry it too. The tie is the host's own result of the issued call, the completed spawn item of its tool
// use id in the parent's rollout (createdCheckSpawnResult): naming this child ties it ("spawn-result"); a failed call or one
// that returned another child refuses the report; no readable result leaves the child "unverified", which cannot satisfy an
// independent review.
func createdCheckTie(ctx context.Context, env host.LookupEnv, d *Dispatch, a *DispatchAttempt, agent, first string) (string, error) {
	if first != "" {
		if m := managedSpawnMarker(first); m == nil || m[1] != d.ID || m[2] != a.ID {
			return "", errors.New("agentId is not the child the issued spawn created: its first message carries no marker of this attempt" + createdCheckCorrection)
		}
	}
	if slices.Contains(a.PriorChildren, agent) {
		return "", errors.New("agentId is not the child the issued spawn created: the host already showed it with this attempt's marker when the spawn was issued" + createdCheckCorrection)
	}
	if a.ToolUseID == nil || *a.ToolUseID == "" {
		return "unverified", nil
	}
	result, err := createdCheckSpawnResult(ctx, env, d.SessionID, *a.ToolUseID)
	switch {
	case err != nil || !result.Seen:
		return "unverified", nil
	case result.Status == "completed" && slices.Contains(result.Children, agent):
		return "spawn-result", nil
	case result.Status == "completed":
		return "", errors.New("agentId is not the child the issued spawn created: the host's result of the issued call names another child" + createdCheckCorrection)
	case result.Status == "failed":
		return "", errors.New("agentId is not the child the issued spawn created: the host's result of the issued call shows it failed" + createdCheckCorrection)
	}
	return "unverified", nil
}

// createdSpawnResult is the host's own result of one spawn call: whether the parent's rollout holds the call's completed
// item, the status it ended with and the child threads it returned.
type createdSpawnResult struct {
	Seen     bool
	Status   string
	Children []string
}

// createdCheckSpawnResult reads the host's result of the native spawn call with tool use id call out of the rollout of the
// session, whose path the native thread database holds. The host writes each collaboration tool call it runs as an item of
// type CollabAgentToolCall whose id is the call's tool use id (the id the spawn hook received and recorded), and its
// item_completed event names the threads the call created (receiver_thread_ids). The caller cannot write that event; a line
// that does not parse is not a result.
func createdCheckSpawnResult(ctx context.Context, env host.LookupEnv, session, call string) (createdSpawnResult, error) {
	path := ""
	err := createdCheckWithDB(ctx, env, func(conn *sql.Conn, columns map[string]bool) error {
		if !columns["rollout_path"] {
			return nil
		}
		err := conn.QueryRowContext(ctx, "SELECT COALESCE(CAST(rollout_path AS TEXT), '') FROM threads WHERE id = ?", session).Scan(&path)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil || path == "" {
		return createdSpawnResult{}, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return createdSpawnResult{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return createdSpawnResult{}, err
	}
	defer f.Close()
	quoted, err := json.Marshal(call)
	if err != nil {
		return createdSpawnResult{}, err
	}
	var out createdSpawnResult
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, quoted) {
			var e struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Item struct {
						Type      string   `json:"type"`
						ID        string   `json:"id"`
						Tool      string   `json:"tool"`
						Status    string   `json:"status"`
						Sender    string   `json:"sender_thread_id"`
						Receivers []string `json:"receiver_thread_ids"`
					} `json:"item"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &e) == nil && e.Type == "event_msg" && e.Payload.Type == "item_completed" {
				if it := e.Payload.Item; it.Type == "CollabAgentToolCall" && it.Tool == "spawn_agent" && it.ID == call && it.Sender == session {
					out = createdSpawnResult{Seen: true, Status: it.Status, Children: it.Receivers}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return createdSpawnResult{}, err
			}
			return out, nil
		}
	}
}

// createdCheckComplete is the complete report at the checked boundary: an independent review is complete only through a child
// tied to its issued spawn (receipt correlation spawn-result). Every other check is the ledger's.
func createdCheckComplete(_ *dispatchPinnedDir, _ string, d *Dispatch, b map[string]any) (DispatchResult, error) {
	a := d.Attempts[len(d.Attempts)-1]
	if d.Role == Reviewer && (a.Receipt == nil || a.Receipt.Correlation != "spawn-result") {
		return DispatchResult{}, errors.New("this child is not tied to its issued spawn, so it cannot satisfy independent review; " +
			"report created again once the host shows the issued spawn's result, or close it with outcome stopped")
	}
	return dispatchReport(d, b, nil)
}

// dispatchPolicyStopCleanup marks the outstanding cleanup of a child a policy stop left recorded: the dispatch is stopped by
// the provider decision, which keeps its code and never hands on, while the attempt still names a child that may be running.
// The stopped report accounts for the child later (dispatchCleanupRecord).
func dispatchPolicyStopCleanup(d *Dispatch, r DispatchResult) DispatchResult {
	a := &d.Attempts[len(d.Attempts)-1]
	if a.AgentID == nil || *a.AgentID == "" || dispatchIs(a.Status, "failed") || dispatchIs(a.Status, "complete") {
		return r
	}
	a.Cleanup = &DispatchCleanup{Status: "pending", Note: "the policy stopped the dispatch; the recorded child is not yet accounted for"}
	r.Attempts = d.Attempts
	r.Reason += "; the recorded child's cleanup is outstanding: once it has ended, report outcome stopped with its agentId, executionState stopped and reconciliation"
	return r
}

// dispatchCleanupRecord records the cleanup evidence of a stopped dispatch's child on its attempt, which the caller then saves;
// the dispatch stays stopped and nothing is handed on. A child seen to have ended (newest is a terminal turn) is cleaned up:
// the attempt is failed with the reconciliation, the original code and task failure are kept, and its id is free. Otherwise
// the evidence is recorded as unconfirmed and the id stays held. It returns the reason of the answer.
func dispatchCleanupRecord(a *DispatchAttempt, evidence, newest, source string) string {
	if !dispatchTerminalTurn(newest) {
		a.Cleanup = &DispatchCleanup{Status: "unconfirmed", Evidence: evidence, Note: "the child's end was not observed"}
		return "dispatch stays stopped; cleanup evidence recorded, but the child's end was not confirmed, so its id stays held; report again once it can be observed"
	}
	a.Status, a.Reconciliation = "failed", &evidence
	a.Cleanup = &DispatchCleanup{Status: "confirmed", Evidence: evidence, Newest: newest, Source: source, Note: dispatchTerminationNote}
	return "dispatch stays stopped; child cleanup recorded and its id released"
}

// createdArchivedReplay refuses an agent id that another attempt of the session already holds. The
// host archives a child when it finishes, but it stays a real child of the session, so only the
// ledger can tell that it was spawned for an earlier attempt. The attempt that reports its own id
// again is not another attempt. A record that cannot be read as a regular dispatch file fails the
// check, because it may be the one that holds the id. The directory is listed and every record read
// through the pinned directory the report holds the lock in. An attempt the stopped close closed does not hold its id any
// more, though its record keeps it as evidence. The caller holds the session lock from the listing to its own write.
func createdArchivedReplay(dir *dispatchPinnedDir, final, session, attempt, agent string) error {
	entries, err := fs.ReadDir(dir.root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		if !entry.Type().IsRegular() {
			return errors.New("dispatch record " + name + " is unusable: not a regular file")
		}
		dir.point("sibling")
		d, err := dispatchPinnedRead(dir, name, session, name[:len(name)-len(".json")])
		if err != nil {
			return errors.New("dispatch record " + name + " is unusable: " + err.Error())
		}
		for i, a := range d.Attempts {
			if a.AgentID != nil && *a.AgentID == agent && (name != final || a.ID != attempt) && !createdCheckClosed(&d, i) {
				return errors.New("agentId was already reported for dispatch " + d.ID + " attempt " + a.ID + createdCheckCopy + createdCheckHeld(&d, i))
			}
		}
	}
	return nil
}

// createdCheckClosed reports whether attempt i is the one the stopped close closed: the last attempt of a stopped dispatch,
// failed with a reconciliation. Nothing else writes that combination.
func createdCheckClosed(d *Dispatch, i int) bool {
	a := d.Attempts[i]
	return i == len(d.Attempts)-1 && dispatchIs(d.Status, "stopped") && dispatchIs(a.Status, "failed") && a.Reconciliation != nil
}

// createdCheckHeld ends a refusal with the way out of it: the stopped close frees the id of the last attempt of a dispatch
// that is still active, and the cleanup report of a dispatch a policy stopped frees it once the child is seen to have ended;
// any other holder keeps it for good, and the caller needs a child of its own.
func createdCheckHeld(d *Dispatch, i int) string {
	if i == len(d.Attempts)-1 && dispatchIs(d.Status, "active") {
		return createdCheckClose + "; once dispatch " + d.ID + " is closed that way its agentId is free"
	}
	if i == len(d.Attempts)-1 && dispatchIs(d.Status, "stopped") {
		return "; dispatch " + d.ID + " was stopped by policy with its child unaccounted for: once the child has ended, a report with outcome stopped, executionState stopped and reconciliation records its cleanup and frees the agentId"
	}
	return "; its agentId stays reserved, so spawn a new child for this attempt"
}

type createdCheckIdentity struct {
	ID, Parent, Status string
	Subagent           bool
	// RolloutPath, FirstMessage, Model and Effort are the native database's rollout_path, first_user_message, model and
	// reasoning_effort of the thread, "" when the column is absent or empty. Only the native read fills them.
	RolloutPath, FirstMessage, Model, Effort string
}

func createdCheckRead(ctx context.Context, env host.LookupEnv, h DispatchHost, agent string) (createdCheckIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if h == nil {
		return createdCheckNative(ctx, env, agent)
	}
	raw, err := h.Call(ctx, "thread/read", map[string]any{"threadId": agent, "includeTurns": false})
	if err != nil {
		return createdCheckIdentity{}, err
	}
	var reply struct {
		Thread struct {
			ID         string `json:"id"`
			Parent     string `json:"parentThreadId"`
			SourceName string `json:"threadSource"`
			Status     struct {
				Type string `json:"type"`
			} `json:"status"`
			Source struct {
				SubAgent struct {
					Spawn struct {
						Parent string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subAgent"`
			} `json:"source"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return createdCheckIdentity{}, err
	}
	t := reply.Thread
	return createdCheckIdentity{ID: t.ID, Parent: t.Parent, Status: t.Status.Type, Subagent: t.SourceName == "subagent" || t.SourceName == "" && t.Source.SubAgent.Spawn.Parent == t.Parent && t.Parent != ""}, nil
}

// createdRuntimeStatus is the status of the child's newest turn, read through the host's turn list the way the bridge and the
// child cleanup read it (newest first, one entry). It is "" when the list cannot be read or holds no turn.
func createdRuntimeStatus(ctx context.Context, h DispatchHost, agent string) string {
	raw, err := h.Call(ctx, "thread/turns/list", map[string]any{"threadId": agent, "limit": 1, "itemsView": "summary"})
	if err != nil {
		return ""
	}
	var page struct {
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &page) != nil || len(page.Data) == 0 {
		return ""
	}
	return page.Data[0].Status
}

// Closing reduces authority only. Caller reconciliation is recorded, never
// presented as a host-authenticated termination receipt; the old ID is retained in the record and no longer held.
// The host is asked twice about the recorded child, under one bound: its identity and status (the active refusal), then its
// newest turn, which must not be in progress. The thread/read answer's turns field decides nothing, because a host that
// leaves it out would turn the turn check off without a sign except the note of the close. A close that could not see the
// newest turn end goes through and says so.
func createdCheckStop(ctx context.Context, cwd string, b map[string]any, env host.LookupEnv, h DispatchHost, after func(string)) (out DispatchResult, err error) {
	status := map[string]any{"action": "status", "sessionId": b["sessionId"], "dispatchId": b["dispatchId"]}
	if _, err := RunDispatch(cwd, status, env); err != nil {
		return DispatchResult{}, err
	}
	root, err := dispatchRoot(cwd)
	if err != nil {
		return DispatchResult{}, err
	}
	session, _ := b["sessionId"].(string)
	id, _ := b["dispatchId"].(string)
	dir, err := dispatchDirectory(root, session, after)
	if err != nil {
		return DispatchResult{}, err
	}
	defer dir.Close()
	releaseSession, err := dir.lock(dispatchSessionLock)
	if err != nil {
		return DispatchResult{}, err
	}
	defer func() { err = errors.Join(err, releaseSession()) }()
	name := id + ".json"
	release, err := dir.lock(name)
	if err != nil {
		return DispatchResult{}, err
	}
	defer func() { err = errors.Join(err, release()) }()
	d, err := dispatchPinnedRead(dir, name, session, id)
	if err != nil {
		return DispatchResult{}, err
	}
	a := &d.Attempts[len(d.Attempts)-1]
	if !dispatchIs(b["attemptId"], a.ID) {
		return DispatchResult{}, errors.New("stale or missing attemptId; inspect status")
	}
	cleanup := dispatchIs(d.Status, "stopped") && a.AgentID != nil && !createdCheckClosed(&d, len(d.Attempts)-1)
	if !dispatchIs(d.Status, "active") && !cleanup {
		return dispatchResult(&d, nil, ""), nil
	}
	if !dispatchIs(b["executionState"], "stopped") || a.AgentID == nil || !dispatchIs(b["agentId"], *a.AgentID) {
		return DispatchResult{}, errors.New("recorded child must be stopped and identified")
	}
	reconciliation, err := dispatchSmallText(b["reconciliation"], "reconciliation evidence")
	if err != nil {
		return DispatchResult{}, err
	}
	o := dispatchObserve(ctx, env, h, session, *a.AgentID, false)
	if !o.Foreign && o.Status == "active" {
		return DispatchResult{}, errors.New("recorded child is active; stop it before closing")
	}
	newest := ""
	if !o.Foreign {
		newest = o.Newest
	}
	if newest == "inProgress" {
		return DispatchResult{}, errors.New("recorded child has a turn in progress; stop it before closing")
	}
	if cleanup {
		reason := dispatchCleanupRecord(a, reconciliation, newest, o.Source)
		if err := dispatchSave(dir, name, &d, nil); err != nil {
			return DispatchResult{}, err
		}
		return dispatchResult(&d, "stop", reason), nil
	}
	a.Reconciliation = &reconciliation
	a.Code = nil
	a.TaskFailure = nil
	a.Status = "failed"
	d.Status = "stopped"
	if err := dispatchSave(dir, name, &d, nil); err != nil {
		return DispatchResult{}, err
	}
	reason := "dispatch closed; caller reconciliation recorded, recorded identity retained"
	if !dispatchTerminalTurn(newest) {
		reason += "; runtime not confirmed"
	}
	return dispatchResult(&d, "stop", reason), nil
}

// The host writes thread_spawn into threads.source when it creates a subagent.
// Read that witness without networking: role is also imported by offline relay
// packages. Existing host imports already register the pure-Go SQLite driver.
func createdCheckNative(ctx context.Context, env host.LookupEnv, agent string) (createdCheckIdentity, error) {
	var row createdCheckIdentity
	err := createdCheckWithDB(ctx, env, func(conn *sql.Conn, columns map[string]bool) error {
		id, source, r, err := createdCheckScan(ctx, conn, columns, "WHERE id = ?", agent)
		if err != nil {
			return err
		}
		// The archive flag is a lifecycle fact, not part of the identity: the host archives a child
		// when it finishes, and the spawn marker below still proves who created it.
		if id != agent {
			return errors.New("host thread identity is unavailable")
		}
		row, err = createdCheckParent(id, source, r)
		return err
	})
	return row, err
}

// createdCheckMarked lists the subagents of session whose first message carries the dispatch marker of attempt in dispatch.
func createdCheckMarked(ctx context.Context, env host.LookupEnv, session, dispatch, attempt string) ([]createdCheckIdentity, error) {
	var out []createdCheckIdentity
	err := createdCheckWithDB(ctx, env, func(conn *sql.Conn, columns map[string]bool) error {
		if !columns["first_user_message"] {
			return nil
		}
		rows, err := conn.QueryContext(ctx, "SELECT id, source FROM threads WHERE first_user_message LIKE ?", "%[CRW-DISPATCH:%")
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id, source string
			if err := rows.Scan(&id, &source); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			id, source, r, err := createdCheckScan(ctx, conn, columns, "WHERE id = ?", id)
			if err != nil {
				return err
			}
			row, err := createdCheckParent(id, source, r)
			if err != nil || row.Parent != session {
				continue
			}
			if m := managedSpawnMarker(row.FirstMessage); m != nil && m[1] == dispatch && m[2] == attempt {
				out = append(out, row)
			}
		}
		return nil
	})
	return out, err
}

// errCreatedNoDatabase says the host has no thread database yet, so it shows no thread at all.
var errCreatedNoDatabase = errors.New("host thread database is missing")

// createdCheckWithDB opens the newest native thread database read-only, without networking, and runs fn on one connection.
func createdCheckWithDB(ctx context.Context, env host.LookupEnv, fn func(*sql.Conn, map[string]bool) error) error {
	home, err := host.CodexSQLiteHome(env)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(home)
	if errors.Is(err, fs.ErrNotExist) {
		return errCreatedNoDatabase
	}
	if err != nil {
		return err
	}
	pattern := regexp.MustCompile(`^state_([0-9]+)\.sqlite$`)
	name := ""
	var highest *big.Int
	for _, entry := range entries {
		match := pattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		version, _ := new(big.Int).SetString(match[1], 10)
		if highest == nil || version.Cmp(highest) > 0 || version.Cmp(highest) == 0 && entry.Name() < name {
			name, highest = entry.Name(), version
		}
	}
	if name == "" {
		return errCreatedNoDatabase
	}
	path := filepath.Join(home, name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("host thread database must be a regular file")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return err
	}
	columns, err := createdCheckColumns(ctx, conn)
	if err != nil {
		return err
	}
	return fn(conn, columns)
}

// createdCheckScan reads one threads row selected by where. Optional columns are read when the host's schema has them; a
// missing one reads as "".
func createdCheckScan(ctx context.Context, conn *sql.Conn, columns map[string]bool, where, arg string) (id, source string, row createdCheckIdentity, err error) {
	optional := func(name string) string {
		if columns[name] {
			return "COALESCE(CAST(" + name + " AS TEXT), '')"
		}
		return "''"
	}
	query := "SELECT id, source, " + optional("rollout_path") + ", " + optional("first_user_message") + ", " + optional("model") + ", " + optional("reasoning_effort") + " FROM threads " + where
	if err = conn.QueryRowContext(ctx, query, arg).Scan(&id, &source, &row.RolloutPath, &row.FirstMessage, &row.Model, &row.Effort); err != nil {
		return "", "", createdCheckIdentity{}, err
	}
	return id, source, row, nil
}

// createdCheckParent reads the parent thread out of a threads row's source and completes the identity.
func createdCheckParent(id, source string, row createdCheckIdentity) (createdCheckIdentity, error) {
	var marker struct {
		Subagent struct {
			Spawn struct {
				Parent string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal([]byte(source), &marker); err != nil {
		return createdCheckIdentity{}, err
	}
	parent := marker.Subagent.Spawn.Parent
	row.ID, row.Parent, row.Subagent = id, parent, parent != ""
	return row, nil
}

// createdCheckColumns lists the columns of the native threads table, whose schema grows with the host's versions.
func createdCheckColumns(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, "SELECT name FROM pragma_table_info('threads')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}
