package doctor

import "github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"

// This is the red-first stub: the replay test and the recorder land before the port, so the
// failures are behavioural (missing checks) rather than a compile error. The next commit replaces
// every function here with the CXC v0.2.40 port.

// HarnessPabcdCheck is the pabcd session-state check (doctor.ts:166-181).
func HarnessPabcdCheck(projectRoot string) HarnessCheck { return HarnessCheck{} }

// HarnessManifestTargetChecks is the hooks / mcp-targets pair (doctor.ts:228-254).
func HarnessManifestTargetChecks(pluginRoot string) []HarnessCheck { return nil }

// HarnessInstalledRootCheck is the STALE-ROOT-01 check (doctor.ts:401-445).
func HarnessInstalledRootCheck(pluginRoot string, options HarnessOptions) HarnessCheck {
	return HarnessCheck{}
}

func harnessInstallCodexHome(codexHome string, lookup record.Environ, passwdHome func() (string, error)) (string, error) {
	return "", nil
}

func harnessInstallInstalledRootCheck(pluginRoot string, resolve func() (string, error), readFile func(string) ([]byte, error)) HarnessCheck {
	return HarnessCheck{}
}
