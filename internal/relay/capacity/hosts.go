package capacity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const HostHeartbeatInterval = 30 * time.Second
const HostFreshness = 90 * time.Second

// HostPool owns placement in the controller store. Workers never open this DB.
type HostPool struct {
	Store *store.Store
	Now   func() time.Time
}

func (p *HostPool) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
func hostJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

type HostObservation = worker.Observation

type HostStatus struct {
	Host         crwconfig.ExecutionHost `json:"host"`
	Capacity     crwconfig.HostCapacity  `json:"capacity"`
	Enabled      bool                    `json:"enabled"`
	Checked      bool                    `json:"checked"`
	Fresh        bool                    `json:"fresh"`
	Observation  HostObservation         `json:"observation"`
	ReceivedAt   string                  `json:"received_at"`
	HeldSessions int                     `json:"held_sessions"`
	HeldHeavy    int                     `json:"held_heavy"`
	HeldCPU      int                     `json:"held_cpu"`
}

func (p *HostPool) event(ctx context.Context, id, kind string, v any) error {
	_, err := p.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO executor_host_events(host_id,kind,detail_json,recorded_at) VALUES (?,?,?,?)", id, kind, hostJSON(v), p.now().Format(time.RFC3339Nano))
	return err
}
func (p *HostPool) Add(ctx context.Context, h crwconfig.ExecutionHost) error {
	if err := h.Validate(); err != nil {
		return err
	}
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var old string
		err := p.Store.Querier(ctx).QueryRowContext(ctx, "SELECT config_json FROM executor_hosts WHERE host_id=?", h.ID).Scan(&old)
		if err == nil {
			if old == hostJSON(h) {
				return nil
			}
			return errors.New("host identity already enrolled with another profile")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = p.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO executor_hosts(host_id,config_json) VALUES (?,?)", h.ID, hostJSON(h)); err != nil {
			return err
		}
		return p.event(ctx, h.ID, "added", h)
	})
}
func (p *HostPool) Status(ctx context.Context, id string) (HostStatus, error) {
	var s HostStatus
	var config, observation string
	q := p.Store.Querier(ctx)
	err := q.QueryRowContext(ctx, "SELECT config_json,enabled,session_limit,test_slots,test_cpu_budget,observation_json,received_at,checked FROM executor_hosts WHERE host_id=?", id).Scan(&config, &s.Enabled, &s.Capacity.Sessions, &s.Capacity.TestSlots, &s.Capacity.TestCPU, &observation, &s.ReceivedAt, &s.Checked)
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal([]byte(config), &s.Host); err != nil {
		return s, err
	}
	if err = json.Unmarshal([]byte(observation), &s.Observation); err != nil {
		return s, err
	}
	at, err := time.Parse(time.RFC3339Nano, s.ReceivedAt)
	s.Fresh = err == nil && !p.now().Before(at) && p.now().Sub(at) <= HostFreshness
	err = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM executor_assignments WHERE host_id=? AND state NOT IN ('terminated','not_started')", id).Scan(&s.HeldSessions)
	if err != nil {
		return s, err
	}
	err = q.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(p.cpu),0) FROM executor_heavy_permits p JOIN executor_assignments a ON a.execution_id=p.execution_id WHERE a.host_id=? AND p.state='held'", id).Scan(&s.HeldHeavy, &s.HeldCPU)
	return s, err
}
func (p *HostPool) List(ctx context.Context) ([]HostStatus, error) {
	rows, err := p.Store.Querier(ctx).QueryContext(ctx, "SELECT host_id FROM executor_hosts ORDER BY host_id")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	out := []HostStatus{}
	for _, id := range ids {
		s, err := p.Status(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
func (p *HostPool) Check(ctx context.Context, o HostObservation) error {
	return p.observe(ctx, o, true)
}
func (p *HostPool) Heartbeat(ctx context.Context, o HostObservation) error {
	return p.observe(ctx, o, false)
}
func (p *HostPool) observe(ctx context.Context, o HostObservation, check bool) error {
	if o.Incarnation == "" || o.BootID == "" || o.Sequence < 1 || o.EffectiveCPU < 1 || o.QueuedHeavy < 0 || o.AvailableBytes < 0 {
		return errors.New("incomplete host observation")
	}
	for _, v := range []float64{o.SwapUsedPercent, o.PressureSomeAvg60, o.PressureFullAvg10} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return errors.New("invalid host pressure measurement")
		}
	}
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		s, err := p.Status(ctx, o.HostID)
		if err != nil {
			return err
		}
		all, err := p.List(ctx)
		if err != nil {
			return err
		}
		for _, other := range all {
			if other.Host.ID != o.HostID && other.Observation.BootID == o.BootID {
				return errors.New("machine boot identity already belongs to another enrolled host")
			}
		}
		q := p.Store.Querier(ctx)
		changed := s.Observation.Incarnation != o.Incarnation
		if !changed && (o.Sequence <= s.Observation.Sequence || o.BootID != s.Observation.BootID) {
			return errors.New("replayed observation or changed boot without a new incarnation")
		}
		if changed {
			var n int
			if err = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM executor_host_incarnations WHERE host_id=? AND incarnation=?", o.HostID, o.Incarnation).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return errors.New("retired worker incarnation")
			}
			if _, err = q.ExecContext(ctx, "INSERT INTO executor_host_incarnations VALUES (?,?,?)", o.HostID, o.Incarnation, o.BootID); err != nil {
				return err
			}
		}
		matches := o.ToolchainProfile == s.Host.ToolchainProfile && o.LaunchProfile == s.Host.LaunchProfile && o.CapabilityRevision == s.Host.CapabilityRevision && o.Supervised && o.Measured
		checked := (s.Checked && !changed || check) && matches
		enabled := s.Enabled && !changed && checked && s.Capacity.Validate(o.EffectiveCPU) == nil
		if _, err = q.ExecContext(ctx, "UPDATE executor_hosts SET enabled=?,checked=?,observation_json=?,received_at=? WHERE host_id=?", enabled, checked, hostJSON(o), p.now().Format(time.RFC3339Nano), o.HostID); err != nil {
			return err
		}
		kind := "heartbeat"
		if check {
			kind = "checked"
		}
		return p.event(ctx, o.HostID, kind, o)
	})
}
func (p *HostPool) Enable(ctx context.Context, id string, c crwconfig.HostCapacity) error {
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		s, err := p.Status(ctx, id)
		if err != nil {
			return err
		}
		var installing int
		if err := p.Store.Querier(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM executor_toolchain_plans WHERE host_id=? AND state='applying'", id).Scan(&installing); err != nil {
			return err
		}
		if installing != 0 {
			return errors.New("toolchain application still requires readback")
		}
		if !s.Checked || !s.Fresh {
			return errors.New("host requires a fresh matching check")
		}
		if err = c.Validate(s.Observation.EffectiveCPU); err != nil {
			return err
		}
		if s.HeldSessions != 0 || s.HeldHeavy != 0 {
			return errors.New("capacity changes require drain and termination readback")
		}
		if _, err = p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_hosts SET enabled=1,session_limit=?,test_slots=?,test_cpu_budget=? WHERE host_id=?", c.Sessions, c.TestSlots, c.TestCPU, id); err != nil {
			return err
		}
		return p.event(ctx, id, "enabled", c)
	})
}
func (p *HostPool) Drain(ctx context.Context, id string) error {
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := p.Status(ctx, id); err != nil {
			return err
		}
		if _, err := p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_hosts SET enabled=0 WHERE host_id=?", id); err != nil {
			return err
		}
		return p.event(ctx, id, "drained", nil)
	})
}
func eligibleHost(s HostStatus, t executor.Task) error {
	if !s.Enabled || !s.Checked || !s.Fresh {
		return errors.New("host disabled, unchecked or stale")
	}
	o, h := s.Observation, s.Host
	if !o.Measured || o.AvailableBytes < 15<<30 || o.SwapUsedPercent > 100 || o.PressureSomeAvg60 > 10 || o.PressureFullAvg10 > 5 {
		return errors.New("host memory unmeasured or over existing thresholds")
	}
	if h.Repositories[t.Repository.ID] == "" || t.Repository.Forge != "github" {
		return errors.New("repository not enrolled under its canonical forge identity")
	}
	b := t.Executor
	if h.Kind != b.Kind || h.CapabilityRevision != b.CapabilityRevision || (b.LaunchProfileDigest != "" && h.LaunchProfile != b.LaunchProfileDigest) || (b.ToolchainProfileDigest != "" && h.ToolchainProfile != b.ToolchainProfileDigest) {
		return errors.New("executor profile mismatch")
	}
	if b.HostID != "" && (b.HostID != h.ID || b.Incarnation != o.Incarnation) {
		return errors.New("assignment incarnation mismatch")
	}
	if s.HeldSessions >= s.Capacity.Sessions {
		return errors.New("host session ceiling reached")
	}
	for _, c := range t.Checks {
		if c.Resources.CPU > s.Capacity.TestCPU {
			return errors.New("check CPU request cannot fit host budget")
		}
	}
	return nil
}

// Select runs inside the caller's assignment transaction. Ratios use integer
// cross-products. Its full observation is retained with the committed assignment.
func (p *HostPool) Select(ctx context.Context, t executor.Task) (HostStatus, error) {
	all, err := p.List(ctx)
	if err != nil {
		return HostStatus{}, err
	}
	var candidates []HostStatus
	for _, s := range all {
		if eligibleHost(s, t) == nil {
			candidates = append(candidates, s)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		x, y := a.HeldSessions*b.Capacity.Sessions, b.HeldSessions*a.Capacity.Sessions
		if x != y {
			return x < y
		}
		if a.Observation.QueuedHeavy != b.Observation.QueuedHeavy {
			return a.Observation.QueuedHeavy < b.Observation.QueuedHeavy
		}
		return a.Host.ID < b.Host.ID
	})
	if len(candidates) == 0 {
		return HostStatus{}, errors.New("no compatible fresh execution host with session headroom")
	}
	return candidates[0], nil
}

type Assignment struct {
	Admission executor.Admission `json:"admission"`
	Grant     executor.Grant     `json:"grant"`
	SlotID    string             `json:"slot_id"`
	State     string             `json:"state"`
	Proof     string             `json:"proof"`
	Placement HostStatus         `json:"placement"`
}

func (p *HostPool) Assignment(ctx context.Context, id string) (Assignment, error) {
	var a Assignment
	var raw, g, placement string
	err := p.Store.Querier(ctx).QueryRowContext(ctx, "SELECT admission_json,grant_json,slot_id,state,proof,placement_json FROM executor_assignments WHERE execution_id=?", id).Scan(&raw, &g, &a.SlotID, &a.State, &a.Proof, &placement)
	if err != nil {
		return a, err
	}
	for _, v := range []struct {
		raw string
		out any
	}{{raw, &a.Admission}, {g, &a.Grant}, {placement, &a.Placement}} {
		if err = json.Unmarshal([]byte(v.raw), v.out); err != nil {
			return a, err
		}
	}
	return a, nil
}
func (p *HostPool) Assign(ctx context.Context, a executor.Admission, g executor.Grant, reserve func(context.Context) (string, error)) (out Assignment, err error) {
	if err = executor.ValidateAdmission(a); err != nil {
		return out, err
	}
	if err = executor.ValidateGrant(a, g, p.now()); err != nil {
		return out, err
	}
	err = p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		old, e := p.Assignment(ctx, a.Task.Identity.ExecutionID)
		if e == nil {
			if executor.ReferenceOf(old.Admission) != executor.ReferenceOf(a) || old.Grant != g {
				return errors.New("assignment replay changed immutable identity or grant")
			}
			out = old
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		slot, err := reserve(ctx)
		if err != nil {
			return err
		}
		if slot == "" {
			return errors.New("assignment has no global reservation")
		}
		s, err := p.Status(ctx, a.Task.Executor.HostID)
		if err != nil {
			return err
		}
		if err = eligibleHost(s, a.Task); err != nil {
			return err
		}
		id := a.Task.Identity
		_, err = p.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO executor_assignments(execution_id,plan_id,node_id,host_id,incarnation,task_digest,admission_json,grant_json,slot_id,state,placement_json,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,'assigned',?,?)", id.ExecutionID, id.Plan, id.Node, s.Host.ID, s.Observation.Incarnation, a.TaskDigest, hostJSON(a), hostJSON(g), slot, hostJSON(s), p.now().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		out = Assignment{Admission: a, Grant: g, SlotID: slot, State: "assigned", Placement: s}
		return p.event(ctx, s.Host.ID, "assigned", out)
	})
	return out, err
}
func (p *HostPool) reference(ctx context.Context, r executor.Reference) (Assignment, error) {
	a, err := p.Assignment(ctx, r.Identity.ExecutionID)
	if err != nil {
		return a, err
	}
	if executor.ReferenceOf(a.Admission) != r {
		return a, errors.New("stale or foreign execution reference")
	}
	return a, nil
}

// ClaimStart commits dispatch intent before transport. Any retry must Inspect;
// even a controller crash between this commit and sending Start remains unknown.
func (p *HostPool) ClaimStart(ctx context.Context, r executor.Reference) (bool, error) {
	var send bool
	err := p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		a, err := p.reference(ctx, r)
		if err != nil {
			return err
		}
		if a.State != "assigned" {
			return nil
		}
		if err = executor.ValidateGrant(a.Admission, a.Grant, p.now()); err != nil {
			return err
		}
		if _, err = p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_assignments SET state='start_unknown' WHERE execution_id=?", r.Identity.ExecutionID); err != nil {
			return err
		}
		send = true
		return p.event(ctx, r.Executor.HostID, "start_intent", r)
	})
	return send, err
}
func (p *HostPool) ObserveExecution(ctx context.Context, r executor.Reference, i executor.Inspection) error {
	if err := executor.ValidateInspection(i); err != nil {
		return err
	}
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		a, err := p.reference(ctx, r)
		if err != nil {
			return err
		}
		if a.State == "terminated" || a.State == "not_started" {
			return nil
		}
		if i.State == "unknown" {
			return nil
		}
		if a.State == "cancel_unknown" && i.State == "running" {
			return nil
		}
		state := i.State
		if _, err = p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_assignments SET state=?,proof=? WHERE execution_id=?", state, i.Proof, r.Identity.ExecutionID); err != nil {
			return err
		}
		if state == "terminated" || state == "not_started" {
			if _, err = p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_heavy_permits SET state='terminated',proof=? WHERE execution_id=? AND state='held'", i.Proof, r.Identity.ExecutionID); err != nil {
				return err
			}
		}
		return p.event(ctx, r.Executor.HostID, "execution_observed", i)
	})
}
func (p *HostPool) CancelIntent(ctx context.Context, r executor.Reference) error {
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		a, err := p.reference(ctx, r)
		if err != nil {
			return err
		}
		if a.State == "terminated" || a.State == "not_started" {
			return nil
		}
		_, err = p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_assignments SET state='cancel_unknown' WHERE execution_id=?", r.Identity.ExecutionID)
		return err
	})
}

type HeavyPermit struct {
	ID          string `json:"id"`
	ExecutionID string `json:"execution_id"`
	CommandID   string `json:"command_id"`
	CPU         int    `json:"cpu"`
	State       string `json:"state"`
}

func (p *HostPool) AcquireHeavy(ctx context.Context, r executor.Reference, command string, cpu int) (out HeavyPermit, err error) {
	if command == "" || cpu < 1 {
		return out, errors.New("heavy command needs identity and positive CPU budget")
	}
	err = p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		a, err := p.reference(ctx, r)
		if err != nil {
			return err
		}
		if a.State != "running" {
			return errors.New("execution cannot start a heavy command")
		}
		if err := executor.ValidateGrant(a.Admission, a.Grant, p.now()); err != nil {
			return err
		}
		q := p.Store.Querier(ctx)
		id := executor.Digest([]byte(r.Identity.ExecutionID + "\x00" + command))
		e := q.QueryRowContext(ctx, "SELECT permit_id,execution_id,command_id,cpu,state FROM executor_heavy_permits WHERE permit_id=?", id).Scan(&out.ID, &out.ExecutionID, &out.CommandID, &out.CPU, &out.State)
		if e == nil {
			if out.CPU != cpu || out.State != "held" {
				return errors.New("heavy command identity already consumed or changed")
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		s, err := p.Status(ctx, r.Executor.HostID)
		if err != nil {
			return err
		}
		if !s.Fresh || !s.Checked || s.Observation.Incarnation != r.Executor.Incarnation {
			return errors.New("worker observation stale or changed")
		}
		if cpu > s.Capacity.TestCPU {
			return fmt.Errorf("CPU request %d cannot fit host budget %d", cpu, s.Capacity.TestCPU)
		}
		if s.HeldHeavy >= s.Capacity.TestSlots || s.HeldCPU+cpu > s.Capacity.TestCPU {
			return errors.New("heavy capacity exhausted")
		}
		out = HeavyPermit{ID: id, ExecutionID: r.Identity.ExecutionID, CommandID: command, CPU: cpu, State: "held"}
		_, err = q.ExecContext(ctx, "INSERT INTO executor_heavy_permits(permit_id,execution_id,command_id,cpu,state) VALUES (?,?,?,?,'held')", id, r.Identity.ExecutionID, command, cpu)
		return err
	})
	return out, err
}
func (p *HostPool) EndHeavy(ctx context.Context, r executor.Reference, id, proof string) error {
	if proof == "" {
		return errors.New("whole command group termination proof required")
	}
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := p.reference(ctx, r); err != nil {
			return err
		}
		res, err := p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_heavy_permits SET state='terminated',proof=? WHERE permit_id=? AND execution_id=?", proof, id, r.Identity.ExecutionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return errors.New("unknown heavy permit")
		}
		return err
	})
}

// A provider timeout or a flow closure is not process termination. This guard
// applies to the existing release API too, so callers cannot return a global
// slot while a remote execution may still be running.
func (c *Capacity) hostSlotReturnable(ctx context.Context, slot string) error {
	q := c.Store.Querier(ctx)
	var present int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='executor_assignments'").Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	var live int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM executor_assignments WHERE slot_id=? AND state NOT IN ('terminated','not_started')", slot).Scan(&live); err != nil {
		return err
	}
	if live != 0 {
		return errors.New("execution host termination is unproven; retain the global slot")
	}
	return nil
}

// RenewGrant changes only control authority, never the frozen task or resources.
// The scheduler calls it inside the transaction that verifies current ownership.
func (p *HostPool) RenewGrant(ctx context.Context, r executor.Reference, g executor.Grant) error {
	return p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		a, err := p.reference(ctx, r)
		if err != nil {
			return err
		}
		if err = executor.ValidateGrant(a.Admission, g, p.now()); err != nil {
			return err
		}
		if g.OwnerEpoch < a.Grant.OwnerEpoch || (g.OwnerEpoch == a.Grant.OwnerEpoch && g.Owner != a.Grant.Owner) || g.ExpiresAt.Before(a.Grant.ExpiresAt) {
			return errors.New("control grant regressed")
		}
		if _, err = p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_assignments SET grant_json=? WHERE execution_id=?", hostJSON(g), r.Identity.ExecutionID); err != nil {
			return err
		}
		return p.event(ctx, r.Executor.HostID, "grant_renewed", g)
	})
}
