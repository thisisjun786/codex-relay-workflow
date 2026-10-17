package delivery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ObserveWakeConfiguration reads the current endpoint and worker incarnation.
// The sender passes its own PID and frozen settings. A preflight supplies the PID
// independently verified by the service's worker-policy observation and nil settings.
// Neither caller derives current configuration from a previous wake journal row.
func ObserveWakeConfiguration(ctx context.Context, s *store.Store, socket, task, disposition string, workerPID int, settings *TaskSettings) (*WakeExpectation, error) {
	if settings == nil {
		var err error
		settings, err = AuthorizedSettings(ctx, s, task, nil)
		if err != nil {
			return nil, err
		}
	}
	raw := json.RawMessage(dumps(settings.Data))
	policy, _ := settings.Data.Get("approvalPolicy").(string)
	if _, ok := settingsObject(raw, policy); !ok {
		return nil, fmt.Errorf("required settings are incomplete")
	}
	identity, err := wakeRuntime(ctx, socket, workerPID)
	if err != nil {
		return nil, err
	}
	identity.ApprovalPolicy = policy
	identity.RolePolicy = registry.EnvironmentRolePolicy().Digest()
	identity.Role = settings.BoundRole
	identity.PairSource = "role"
	if settings.SettingsFreeResume {
		identity.PairSource = "recorded"
	}
	if exception, ok := settings.Data.Get("citedException").(string); ok && exception != "" {
		identity.PairSource = "exception:" + exception
	}
	identity.Disposition = disposition
	return &WakeExpectation{Identity: identity, RequiredSettings: raw}, nil
}
