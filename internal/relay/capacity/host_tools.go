package capacity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
	"time"
)

type ToolPlan struct {
	ID       string                   `json:"id"`
	HostID   string                   `json:"host_id"`
	Manifest worker.Toolchain         `json:"manifest"`
	State    string                   `json:"state"`
	Readback worker.ToolchainReadback `json:"readback"`
}

func (p *HostPool) PlanTools(ctx context.Context, id string, m worker.Toolchain) (out ToolPlan, err error) {
	// Platform compatibility is checked by the target worker at apply/readback.
	if len(m.Files) == 0 || m.OS == "" || m.Arch == "" {
		return out, errors.New("pinned platform toolchain required")
	}
	out = ToolPlan{ID: executor.Digest([]byte(id + "\x00" + m.Digest())), HostID: id, Manifest: m, State: "planned"}
	err = p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := p.Status(ctx, id); err != nil {
			return err
		}
		_, err := p.Store.Querier(ctx).ExecContext(ctx, "INSERT OR IGNORE INTO executor_toolchain_plans(plan_id,host_id,manifest_json,profile_digest,state,recorded_at) VALUES (?,?,?,?,'planned',?)", out.ID, id, hostJSON(m), m.Digest(), p.now().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return out, err
	}
	return p.ToolPlan(ctx, out.ID)
}
func (p *HostPool) ToolPlan(ctx context.Context, id string) (out ToolPlan, err error) {
	var raw, readback string
	out.ID = id
	err = p.Store.Querier(ctx).QueryRowContext(ctx, "SELECT host_id,manifest_json,state,readback_json FROM executor_toolchain_plans WHERE plan_id=?", id).Scan(&out.HostID, &raw, &out.State, &readback)
	if err != nil {
		return
	}
	if err = json.Unmarshal([]byte(raw), &out.Manifest); err == nil {
		err = json.Unmarshal([]byte(readback), &out.Readback)
	}
	return
}
func (p *HostPool) ApplyTools(ctx context.Context, id string, apply func(context.Context, ToolPlan) (worker.ToolchainReadback, error)) (out ToolPlan, err error) {
	err = p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		out, err = p.ToolPlan(ctx, id)
		if err != nil {
			return err
		}
		s, err := p.Status(ctx, out.HostID)
		if err != nil {
			return err
		}
		if s.Enabled || s.HeldSessions != 0 || s.HeldHeavy != 0 {
			return errors.New("toolchain apply requires a drained host with termination readback")
		}
		_, err = p.Store.Querier(ctx).ExecContext(ctx, "UPDATE executor_toolchain_plans SET state='applying' WHERE plan_id=?", id)
		return err
	})
	if err != nil {
		return out, err
	}
	readback, err := apply(ctx, out)
	if err != nil {
		return out, err
	}
	if readback.Digest != out.Manifest.Digest() || hostJSON(readback.Files) != hostJSON(out.Manifest.Files) {
		return out, errors.New("toolchain readback does not match approved plan")
	}
	err = p.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		s, err := p.Status(ctx, out.HostID)
		if err != nil {
			return err
		}
		if s.Enabled || s.HeldSessions != 0 || s.HeldHeavy != 0 {
			return errors.New("host changed during toolchain apply")
		}
		s.Host.ToolchainProfile = readback.Digest
		q := p.Store.Querier(ctx)
		if _, err = q.ExecContext(ctx, "UPDATE executor_hosts SET config_json=?,checked=0 WHERE host_id=?", hostJSON(s.Host), out.HostID); err != nil {
			return err
		}
		if _, err = q.ExecContext(ctx, "UPDATE executor_toolchain_plans SET state='applied',readback_json=? WHERE plan_id=?", hostJSON(readback), id); err != nil {
			return err
		}
		return p.event(ctx, out.HostID, "toolchain_applied", readback)
	})
	if err != nil {
		return out, err
	}
	return p.ToolPlan(ctx, id)
}
