package role

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

type NativeRoleName = RoleName

type RegistrationResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Updated bool   `json:"updated,omitempty"`
}

func NativeRoles() []NativeRoleName                                    { return nil }
func ResolveNativeRoleHome(env host.LookupEnv, userHome string) string { return "" }
func RegisterRole(role NativeRoleName, home ...string) (RegistrationResult, error) {
	return RegistrationResult{}, errors.New("native registration not implemented")
}
func RegisterExecutor(home ...string) (RegistrationResult, error) {
	return RegisterRole(Executor, home...)
}
func RegisterArchitect(home ...string) (RegistrationResult, error) {
	return RegisterRole(Architect, home...)
}

type registrationStep int

const (
	registrationFileSync registrationStep = iota
	registrationBackupDirSync
	registrationAfterBackup
	registrationBeforeRename
	registrationRoleDirSync
)

func registrationRegister(role NativeRoleName, homes []string, fail func(registrationStep) error) (RegistrationResult, error) {
	return RegisterRole(role, homes...)
}
