package sync

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
)

// packetPolicy is cli.py main's _apply_launch_policy(_declaring_service(services), os.environ)
// for the store-backed receive check, then the role policy that process would snapshot: the
// launch declaration of the state directory the check reads, through the service's one
// reader. A conflict or an unreadable declaration is refused; nothing is written.
func packetPolicy(state, environment string) (registry.RolePolicy, error) {
	resolution := service.ResolveLaunchPolicyAt(state, environment)
	if refusal := service.LaunchRefusal(resolution); refusal != nil {
		return registry.RolePolicy{}, &cli.PayloadExit{Payload: refusal, Code: 2}
	}
	value, recorded, err := service.LaunchVariable(resolution)
	if errors.Is(err, service.ErrEmbeddedNUL) {
		return registry.RolePolicy{}, &cli.HostError{Class: "ValueError", Detail: err.Error()}
	}
	if recorded {
		environment = value
	}
	return registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: environment}), nil
}
