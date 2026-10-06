package role

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
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
// The created check runs after domain validation, inside the same lock, before
// atomic publication. On refusal dispatchSave removes its owned temporary file.
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
	if !dispatchIs(b["action"], "report") || !dispatchIs(b["outcome"], "created") {
		return RunDispatch(cwd, input, env)
	}
	return dispatchPinnedRunHeld(cwd, input, env, func(dir *dispatchPinnedDir, _, final string) error {
		session, _ := b["sessionId"].(string)
		agent, _ := b["agentId"].(string)
		attempt, _ := b["attemptId"].(string)
		if err := createdArchivedReplay(dir, final, session, attempt, agent); err != nil {
			return err
		}
		identity, err := createdCheckRead(ctx, env, h, agent)
		if err != nil {
			return errors.New("host could not verify created agent" + createdCheckCorrection)
		}
		if identity.ID != agent || identity.Parent != session || !identity.Subagent {
			return errors.New("agentId is not a real subagent thread parented by this session" + createdCheckCorrection)
		}
		return nil
	}, after, true)
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
// that is still active; any other holder keeps it for good, and the caller needs a child of its own.
func createdCheckHeld(d *Dispatch, i int) string {
	if i == len(d.Attempts)-1 && dispatchIs(d.Status, "active") {
		return createdCheckClose + "; once dispatch " + d.ID + " is closed that way its agentId is free"
	}
	return "; its agentId stays reserved, so spawn a new child for this attempt"
}

type createdCheckIdentity struct {
	ID, Parent, Status string
	Subagent           bool
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
	if !dispatchIs(d.Status, "active") {
		return dispatchResult(&d, nil, ""), nil
	}
	if !dispatchIs(b["executionState"], "stopped") || a.AgentID == nil || !dispatchIs(b["agentId"], *a.AgentID) {
		return DispatchResult{}, errors.New("recorded child must be stopped and identified")
	}
	reconciliation, err := dispatchSmallText(b["reconciliation"], "reconciliation evidence")
	if err != nil {
		return DispatchResult{}, err
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	identity, readErr := createdCheckRead(readCtx, env, h, *a.AgentID)
	own := readErr == nil && identity.ID == *a.AgentID && identity.Parent == session && identity.Subagent
	if own && identity.Status == "active" {
		return DispatchResult{}, errors.New("recorded child is active; stop it before closing")
	}
	newest := ""
	if own && h != nil {
		newest = createdRuntimeStatus(readCtx, h, *a.AgentID)
	}
	if newest == "inProgress" {
		return DispatchResult{}, errors.New("recorded child has a turn in progress; stop it before closing")
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
	if !slices.Contains([]string{"completed", "interrupted", "failed"}, newest) {
		reason += "; runtime not confirmed"
	}
	return dispatchResult(&d, "stop", reason), nil
}

// The host writes thread_spawn into threads.source when it creates a subagent.
// Read that witness without networking: role is also imported by offline relay
// packages. Existing host imports already register the pure-Go SQLite driver.
func createdCheckNative(ctx context.Context, env host.LookupEnv, agent string) (createdCheckIdentity, error) {
	home, err := host.CodexSQLiteHome(env)
	if err != nil {
		return createdCheckIdentity{}, err
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return createdCheckIdentity{}, err
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
		return createdCheckIdentity{}, errors.New("host thread database is missing")
	}
	path := filepath.Join(home, name)
	info, err := os.Lstat(path)
	if err != nil {
		return createdCheckIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		return createdCheckIdentity{}, errors.New("host thread database must be a regular file")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return createdCheckIdentity{}, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		return createdCheckIdentity{}, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return createdCheckIdentity{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return createdCheckIdentity{}, err
	}
	var id, source string
	if err := conn.QueryRowContext(ctx, "SELECT id, source FROM threads WHERE id = ?", agent).Scan(&id, &source); err != nil {
		return createdCheckIdentity{}, err
	}
	// The archive flag is a lifecycle fact, not part of the identity: the host archives a child
	// when it finishes, and the spawn marker below still proves who created it.
	if id != agent {
		return createdCheckIdentity{}, errors.New("host thread identity is unavailable")
	}
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
	return createdCheckIdentity{ID: id, Parent: parent, Subagent: parent != ""}, nil
}
