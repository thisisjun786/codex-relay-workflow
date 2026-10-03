package role

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// SpawnResolution is the stub of the red step.
type SpawnResolution struct {
	Role           RoleName    `json:"role"`
	Model          *string     `json:"model"`
	UsesMainModel  bool        `json:"usesMainModel"`
	Effort         *EffortName `json:"effort"`
	PromptOverride *string     `json:"promptOverride"`
}

func ResolveSpawnConfig(env host.LookupEnv, role RoleName) (SpawnResolution, error) {
	return SpawnResolution{}, nil
}

func IsTrackedProjectConfig(cwd string) bool { return false }

func ProjectConfigTrustToken(cwd string) (string, bool) { return "", false }
