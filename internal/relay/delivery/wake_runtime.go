package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// WakeConnection is the borrowed transport a send used, with the client generation that held it.
type WakeConnection struct {
	Conn   net.Conn
	ID     uint64
	Socket string
}

// WakeConnectionSource is an adapter that exposes the connection it currently sends on.
type WakeConnectionSource interface {
	WakeConnection() WakeConnection
}

// ObserveWakeConfiguration independently reads the endpoint now; it does not
// attribute a previous send to the endpoint currently found at that path.
func ObserveWakeConfiguration(ctx context.Context, s *store.Store, socket, task, disposition string, workerPID int, settings *TaskSettings) (*WakeExpectation, error) {
	return observeWakeConfiguration(ctx, s, task, disposition, settings, func() (WakeIdentity, error) {
		return wakeRuntime(ctx, socket, workerPID)
	})
}

// ObserveWakeConnection reads the peer actually held by the sending adapter.
// Unknown or mismatched connections never fall back to dialing the named path.
func ObserveWakeConnection(ctx context.Context, s *store.Store, socket, task, disposition string, workerPID int, settings *TaskSettings, connection WakeConnection) (*WakeExpectation, error) {
	if connection.Conn == nil || connection.ID == 0 || connection.Socket == "" {
		return nil, fmt.Errorf("sending connection is unmeasured")
	}
	named, err := store.CanonicalSocket(socket)
	if err != nil {
		return nil, err
	}
	actual, err := store.CanonicalSocket(connection.Socket)
	if err != nil {
		return nil, err
	}
	if named != actual {
		return nil, fmt.Errorf("sending connection belongs to another socket")
	}
	return observeWakeConfiguration(ctx, s, task, disposition, settings, func() (WakeIdentity, error) {
		return wakeRuntime(ctx, actual, workerPID, connection.Conn)
	})
}

func observeWakeConfiguration(ctx context.Context, s *store.Store, task, disposition string, settings *TaskSettings, observe func() (WakeIdentity, error)) (*WakeExpectation, error) {
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
	identity, err := observe()
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
