package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

const wakeProbeProtocol = "native:flow-start-wake-probe/1"
const wakeProbeBound = 60 * time.Second
const wakeProbeBootstrap = "This is a disposable CRW wake capability probe. Reply exactly READY, then stop. Do not use tools, inspect files, change settings, or perform project work."
const wakeProbeMessage = "This disposable CRW wake capability probe has reached a new turn. Reply exactly READY and stop. Do not run readback commands, use tools, or perform project work."

type wakeProbeSettings struct {
	CWD          string         `json:"cwd"`
	Model        string         `json:"model"`
	Effort       string         `json:"reasoningEffort"`
	Approval     string         `json:"approvalPolicy"`
	Sandbox      map[string]any `json:"sandbox"`
	Roots        []string       `json:"runtimeWorkspaceRoots"`
	Environments []any          `json:"environments"`
	MCPProfile   string         `json:"mcpProfile"`
}

type wakeProbeRun struct {
	c           *command
	st          *store.Store
	state       string
	socket      string
	owner       string
	want        *delivery.WakeExpectation
	id          string
	thread      string
	bootstrap   string
	message     string
	host        *adapter.Adapter
	registry    *registry.Registry
	bound       bool
	archiveSafe bool
	journal     *journal.Journal
}

func (c *command) probeMissingWake(r *Reader, socket, owner string, want *delivery.WakeExpectation) (retErr error) {
	evidence, _, err := delivery.FindWakeEvidence(c.ctx, r.St, want, c.now())
	if err != nil || evidence != nil {
		return err
	}
	if socket == "" {
		socket, err = store.DefaultSocket()
		if err != nil {
			return err
		}
	}
	write := c.Writer
	if write == nil {
		write = OpenWriter
	}
	st, state, err := write(c.ctx, r.State, socket)
	if err != nil {
		return fmt.Errorf("wake probe store: %w", err)
	}
	defer st.Close()
	id, err := NewRunID()
	if err != nil {
		return err
	}
	p := &wakeProbeRun{c: c, st: st, state: state, socket: socket, owner: owner, want: want, id: id}
	defer func() {
		retErr = errors.Join(retErr, p.finish(retErr))
		if retErr != nil {
			retErr = fmt.Errorf("wake probe: %w", retErr)
		}
	}()
	ctx, cancel := context.WithTimeout(c.ctx, wakeProbeBound)
	defer cancel()
	return p.run(ctx)
}

func (p *wakeProbeRun) run(ctx context.Context) error {
	if p.want == nil {
		return fmt.Errorf("current configuration is unmeasured")
	}
	switch p.want.Identity.PairSource {
	case "role", "recorded":
	default:
		return fmt.Errorf("no equivalent automatic probe for pair source %q", p.want.Identity.PairSource)
	}
	var settings wakeProbeSettings
	if err := json.Unmarshal(p.want.RequiredSettings, &settings); err != nil {
		return err
	}
	if settings.Approval != "never" || len(settings.Environments) != 0 {
		return fmt.Errorf("automatic probe requires approvalPolicy=never and no named environments")
	}
	kind, _ := settings.Sandbox["type"].(string)
	mode := map[string]string{"readOnly": "read-only", "workspaceWrite": "workspace-write", "dangerFullAccess": "danger-full-access"}[kind]
	if mode == "" {
		return fmt.Errorf("unsupported probe sandbox %q", kind)
	}
	switch p.want.Identity.Role {
	case "parent", "child", "supervisor":
	default:
		return fmt.Errorf("unsupported probe role %q", p.want.Identity.Role)
	}
	var err error
	// The service's launch policy, read once: the probe's host and its registry both check against
	// the policy a daemon launched on this service reads, not against this process's environment.
	launch := registry.LaunchRolePolicy(p.state)
	p.host, err = adapter.Open(p.socket, p.state, adapter.Options{Store: p.st, Policy: launch.BridgePolicy()})
	if err != nil {
		return err
	}
	receipt, err := p.host.Create(ctx, bridge.CreateThread{
		RequestID: "wake-probe-" + p.id, CWD: settings.CWD,
		Prompt: wakeProbeBootstrap, PromptRequired: true, Title: "Wake capability probe",
		Sandbox: mode, Model: settings.Model, Effort: settings.Effort,
		Role: p.want.Identity.Role, MCPProfile: settings.MCPProfile,
		Roots: settings.Roots, Policy: settings.Sandbox,
	})
	if err != nil {
		return fmt.Errorf("create recipient: %w", err)
	}
	thread, _ := receipt["threadId"].(string)
	turn, _ := receipt["turnId"].(string)
	if receipt["status"] != "accepted" || receipt["replayed"] == true || thread == "" || turn == "" || thread == p.owner {
		return fmt.Errorf("creation did not prove a separate probe recipient")
	}
	p.thread, p.bootstrap, p.archiveSafe = thread, turn, true
	for {
		observed, err := p.host.ReadTurn(ctx, thread, turn)
		if err != nil {
			return fmt.Errorf("read bootstrap: %w", err)
		}
		if observed != nil {
			switch observed.Status {
			case "completed":
				goto ready
			case "failed", "interrupted":
				return fmt.Errorf("probe bootstrap ended %v", observed.Status)
			}
		}
		if err := waitWakeProbe(ctx); err != nil {
			return err
		}
	}
ready:
	turns, err := probeTurnIDs(ctx, p.host, thread)
	if err != nil || len(turns) != 1 || turns[0] != turn {
		return fmt.Errorf("probe history is not its bootstrap alone: %v", err)
	}
	p.archiveSafe = false // an unknown archive/unarchive outcome is never repeated
	if _, err := p.host.HostCall(ctx, "thread/archive", map[string]any{"threadId": thread}); err != nil {
		return fmt.Errorf("unload probe: %w", err)
	}
	if _, err := p.host.HostCall(ctx, "thread/unarchive", map[string]any{"threadId": thread}); err != nil {
		return fmt.Errorf("restore unloaded probe: %w", err)
	}
	p.archiveSafe = true
	life, err := p.host.ReadThread(ctx, thread)
	if err != nil || life.RuntimeStatus != "notLoaded" {
		return fmt.Errorf("probe did not become notLoaded: %v (%v)", life.RuntimeStatus, err)
	}
	data, err := registry.DecodeJSON(string(p.want.RequiredSettings))
	if err != nil {
		return err
	}
	authorized, ok := data.(delivery.Obj)
	if !ok {
		return fmt.Errorf("probe settings are not an object")
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	p.registry = &registry.Registry{Store: p.st, Policy: launch}
	endpoint := registry.Endpoint{TaskID: thread, HostID: host, Cwd: sql.NullString{String: settings.CWD, Valid: true}}
	if _, err := p.registry.BindScope(ctx, p.want.Identity.Role, "wake-probe:"+p.id, endpoint); err != nil {
		return err
	}
	p.bound = true
	// Preserve the exact authorized record. RecordSettings validates its role
	// against the new binding; it must not invent a role/exception citation.
	if _, err := p.registry.RecordSettings(ctx, thread, authorized, "wake_probe", "", registry.Citation{}); err != nil {
		return err
	}
	current, err := currentWake(ctx, &Reader{St: p.st, State: p.state}, p.socket, thread)
	if err != nil {
		return err
	}
	if !sameWakeConfiguration(p.want, current) {
		return fmt.Errorf("probe does not match the current worker, role or required settings")
	}
	p.journal, err = journal.Open(ctx, p.st, p.id, journal.Identity{ActorTaskID: thread}, journal.Options{Immediate: true, Now: p.c.now})
	if err != nil {
		return err
	}
	p.journal.Append(engine.Event{RunID: p.id, Kind: engine.KindRun, Event: "started", Data: map[string]any{
		"workflow": "wake-probe", "script_digest": wakeProbeProtocol, "args_digest": sha256Hex([]byte(p.owner)),
		"owner_thread": p.owner, "resumable": false,
	}})
	if err := p.journal.Err(); err != nil {
		return err
	}
	channel := &supervisor.Channel{Store: p.st, Socket: p.socket}
	staged, err := channel.StageDirect(ctx, supervisor.DirectMessage{
		Kind: supervisor.KindSessionStepWaiting, Relationship: "run:" + p.id,
		Subject: supervisor.FlowSubject(p.id, "wake-probe"), Summary: "wake capability probe",
		Text: wakeProbeMessage, At: p.c.now().UTC().Format(time.RFC3339Nano),
	}, nil)
	if err != nil {
		return err
	}
	p.message, _ = staged["messageId"].(string)
	if p.message == "" {
		return fmt.Errorf("probe message was not staged")
	}
	// The live daemon does the send. Starting another sender here would prove
	// the CLI's process, not the service instance preflight is adjudicating.
	for {
		row, err := channel.Get(ctx, p.message)
		if err != nil {
			return err
		}
		switch row.State {
		case "queued", "sending":
		case delivery.Dispatched:
			evidence, reason, err := delivery.FindWakeEvidence(ctx, p.st, p.want, p.c.now())
			if err != nil {
				return err
			}
			if evidence == nil {
				return fmt.Errorf("probe receipt is unmeasured: %s", reason)
			}
			p.journal.Append(engine.Event{RunID: p.id, Kind: engine.KindFlow, Event: delivery.WakeObserved, Data: map[string]any{
				"journal_seq": evidence.Sequence, "request_id": evidence.RequestID, "turn_id": evidence.TurnID, "owner_thread": p.owner,
			}})
			return p.journal.Err()
		default:
			return fmt.Errorf("probe delivery ended %s", row.State)
		}
		if err := waitWakeProbe(ctx); err != nil {
			return err
		}
	}
}

func waitWakeProbe(ctx context.Context) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// A bounded complete history is required before altering the disposable thread.
func probeTurnIDs(ctx context.Context, host *adapter.Adapter, thread string) ([]string, error) {
	read, err := host.HostCall(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 3, "itemsView": "summary"})
	if err != nil {
		return nil, err
	}
	if next := read["nextCursor"]; next != nil && next != "" {
		return nil, fmt.Errorf("probe history exceeds one page")
	}
	rows, ok := read["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("probe history is unreadable")
	}
	ids := make([]string, 0, len(rows))
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("probe turn is unreadable")
		}
		id, _ := row["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("probe turn has no id")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func sameWakeConfiguration(a, b *delivery.WakeExpectation) bool {
	if a == nil || b == nil || a.Identity != b.Identity {
		return false
	}
	var left, right any
	return json.Unmarshal(a.RequiredSettings, &left) == nil &&
		json.Unmarshal(b.RequiredSettings, &right) == nil && reflect.DeepEqual(left, right)
}

func (p *wakeProbeRun) finish(failure error) error {
	hostCtx, cancelHost := context.WithTimeout(context.WithoutCancel(p.c.ctx), 10*time.Second)
	defer cancelHost()
	var cleanup error
	request := "wake-probe-" + p.id
	wakeTurn := ""
	if p.message != "" {
		channel := &supervisor.Channel{Store: p.st, Socket: p.socket}
		_, err := channel.Acknowledge(hostCtx, supervisor.AcknowledgeRequest{
			Recipient: p.thread, Actor: p.owner, Messages: []string{p.message},
		})
		cleanup = errors.Join(cleanup, err)
		if attempt, err := p.st.LatestSupervisorAttempt(hostCtx, p.message); err == nil {
			request, wakeTurn = attempt.RequestID, attempt.TurnID.String
		}
	}
	if p.host != nil {
		if p.archiveSafe && p.thread != "" && p.thread != p.owner {
			turns, err := probeTurnIDs(hostCtx, p.host, p.thread)
			safe := err == nil && len(turns) > 0 && len(turns) <= 2
			for _, turn := range turns {
				if turn != p.bootstrap && turn != wakeTurn {
					safe = false
				}
			}
			if safe {
				_, err = p.host.HostCall(hostCtx, "thread/archive", map[string]any{"threadId": p.thread})
				cleanup = errors.Join(cleanup, err)
			} else {
				cleanup = errors.Join(cleanup, fmt.Errorf("probe cleanup left unproved history intact: %v", err))
			}
		}
		cleanup = errors.Join(cleanup, p.host.Close())
	}
	// A host that never answers can spend hostCtx, but the release and failure evidence must still be written.
	storeCtx, cancelStore := context.WithTimeout(context.WithoutCancel(p.c.ctx), 10*time.Second)
	defer cancelStore()
	if p.bound {
		// BindScopeAs reuses an active binding without changing its status.
		// Retire only this disposable scope's original tenure; a handover or
		// another task's binding must survive cleanup unchanged.
		err := p.st.Transaction(storeCtx, func(tx context.Context, _ *sql.Conn) error {
			at := p.c.now().UTC().Format(time.RFC3339Nano)
			result, err := p.st.Q(tx).ExecContext(tx, `UPDATE scope_bindings SET status='archived', revision=revision+1, updated_at=?
WHERE role=? AND scope_key=? AND task_id=? AND status='active' AND revision=1 AND superseded_by IS NULL`,
				at, p.want.Identity.Role, "wake-probe:"+p.id, p.thread)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return fmt.Errorf("probe binding changed before cleanup: rows=%d: %v", n, err)
			}
			return p.registry.Journal(tx, "wake_probe_binding_retired", p.thread, map[string]any{"scope": "wake-probe:" + p.id}, at)
		})
		cleanup = errors.Join(cleanup, err)
	}
	if p.journal != nil {
		status, kind := engine.RunCompleted, ""
		if failure != nil {
			status, kind = engine.RunScriptError, "wake_probe"
		}
		data := map[string]any{"status": status, "error_kind": kind}
		if cleanup != nil {
			data["cleanup_error"] = cleanup.Error()
		}
		p.journal.Append(engine.Event{RunID: p.id, Kind: engine.KindRun, Event: "finished", Data: data})
		cleanup = errors.Join(cleanup, p.journal.Close())
	}
	if problem := errors.Join(failure, cleanup); problem != nil {
		attempted, state := "no", delivery.WithheldPreSend
		if p.message != "" {
			attempted, state = "unknown", delivery.HeldUncertain
		}
		attempt := delivery.WakeAttempt{
			RequestID: request, RecipientTaskID: p.thread, Probe: true, Reason: problem.Error(),
			DeliveryMethod: "turn/start", DeliveryState: state, SendAttempted: attempted, Unattended: true,
			ObservedAt: p.c.now().UTC().Format(time.RFC3339Nano),
		}
		if p.want != nil {
			attempt.RequiredSettings = p.want.RequiredSettings
		}
		// No actual sending identity is invented for a preparation/observation
		// failure. A daemon receipt, if any, remains separately in the journal.
		cleanup = errors.Join(cleanup, delivery.RecordWakeAttempt(storeCtx, p.st, attempt))
	}
	return cleanup
}
