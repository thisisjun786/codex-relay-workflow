package adapter

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

func daemonFactory(ctx context.Context, services dispatch.Services, s *store.Store) (*daemon.Daemon, error) {
	selection, err := store.ResolveStateDir("", services.SocketPath)
	if err != nil {
		return nil, err
	}
	clock := delivery.CommandClock
	if clock == nil {
		clock = delivery.SystemClock{}
	}
	a, err := Open(services.SocketPath, selection.Path, Options{Store: s, Clock: clock, Policy: registry.EnvironmentRolePolicy().BridgePolicy()})
	if err != nil {
		return nil, err
	}
	channel := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: services.Program, Socket: services.SocketPath}
	channel.SettingsLoader = func(ctx context.Context, task string) (*delivery.TaskSettings, error) {
		r := &registry.Registry{Store: s, Now: clock.ISO, Policy: registry.EnvironmentRolePolicy()}
		settings, free, err := r.AuthorizedSettings(ctx, task)
		if err != nil {
			return nil, err
		}
		return &delivery.TaskSettings{Data: delivery.Obj(settings.Data), SettingsFreeResume: free}, nil
	}
	d := daemon.New(s, a, clock, channel)
	d.Faults = &faults.Ledger{Store: s, Clock: clock}
	d.Notices = &faults.NoticeDeliverer{Ledger: d.Faults, Channel: supervisor.NoticeChannel{Channel: channel, Ledger: d.Faults, Host: a}, Owner: "relay-daemon"}
	installation, err := faults.ExecutableInstallation()
	if err != nil {
		return nil, errors.Join(err, a.Close())
	}
	hostRecord, err := faults.HostRecordPath()
	if err != nil {
		return nil, errors.Join(err, a.Close())
	}
	d.Sweeper = &faults.Sweeper{Store: s, MaxAttempts: d.Delivery.Policy.MaxAttempts, Selection: services.Selection, Now: clock.ISO, SupersessionReason: d.Delivery.SupersessionReason,
		Installation: installation, HostRecordPath: hostRecord}
	return d, nil
}
