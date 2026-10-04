package role

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"os"
	"path/filepath"
	"time"
)

// DispatchHost is the existing App Server read method, never a creation surface.
type DispatchHost interface {
	Call(context.Context, string, map[string]any) (json.RawMessage, error)
}

const createdCheckCorrection = "; copy agentId from the native spawn result and ensure the App Server is available; close an old wrong-ID report with outcome stopped, executionState stopped and reconciliation"

// CheckedDispatch is the product boundary. RunDispatch stays the parity ledger.
// The created check runs after domain validation, inside the same lock, before
// atomic publication. On refusal dispatchSave removes its owned temporary file.
func CheckedDispatch(ctx context.Context, cwd string, input any, env host.LookupEnv, h DispatchHost) (DispatchResult, error) {
	if env == nil {
		env = os.LookupEnv
	}
	b, err := dispatchRecord(input)
	if err != nil {
		return DispatchResult{}, err
	}
	if dispatchIs(b["action"], "report") && dispatchIs(b["outcome"], "stopped") {
		return createdCheckStop(ctx, cwd, b, env, h)
	}
	if !dispatchIs(b["action"], "report") || !dispatchIs(b["outcome"], "created") {
		return RunDispatch(cwd, input, env)
	}
	return dispatchRun(cwd, input, env, func(temp, final string) error {
		session, _ := b["sessionId"].(string)
		agent, _ := b["agentId"].(string)
		identity, err := createdCheckRead(ctx, env, h, agent)
		if err != nil {
			return errors.New("host could not verify created agent" + createdCheckCorrection)
		}
		if identity.ID != agent || identity.Parent != session || !identity.Subagent {
			return errors.New("agentId is not a real subagent thread parented by this session" + createdCheckCorrection)
		}
		return crwdir.Rename(temp, final)
	})
}

type createdCheckIdentity struct {
	ID, Parent, Status string
	Subagent           bool
}

func createdCheckRead(ctx context.Context, env host.LookupEnv, h DispatchHost, agent string) (createdCheckIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if h == nil {
		home, err := host.Home(env)
		if err != nil {
			return createdCheckIdentity{}, err
		}
		client := appserver.New(filepath.Join(ResolveNativeRoleHome(env, home), "app-server-control", "app-server-control.sock"), appserver.DefaultBounds)
		defer client.Close()
		h = client
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

// Closing reduces authority only. Caller reconciliation is recorded, never
// presented as a host-authenticated termination receipt; the old ID is retained.
func createdCheckStop(ctx context.Context, cwd string, b map[string]any, env host.LookupEnv, h DispatchHost) (DispatchResult, error) {
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
	dir, err := dispatchDirectory(root, session)
	if err != nil {
		return DispatchResult{}, err
	}
	path := filepath.Join(dir, id+".json")
	lock := path + ".lock"
	if err := os.Mkdir(lock, 0700); err != nil {
		return DispatchResult{}, err
	}
	defer os.RemoveAll(lock)
	d, err := dispatchRead(path, session, id)
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
	identity, readErr := createdCheckRead(ctx, env, h, *a.AgentID)
	if readErr == nil && identity.ID == *a.AgentID && identity.Parent == session && identity.Subagent && identity.Status == "active" {
		return DispatchResult{}, errors.New("recorded child is active; stop it before closing")
	}
	a.Reconciliation = &reconciliation
	a.Code = nil
	a.TaskFailure = nil
	a.Status = "failed"
	d.Status = "stopped"
	if err := dispatchSave(path, &d, crwdir.Rename); err != nil {
		return DispatchResult{}, err
	}
	return dispatchResult(&d, "stop", "dispatch closed; caller reconciliation recorded, recorded identity retained"), nil
}
