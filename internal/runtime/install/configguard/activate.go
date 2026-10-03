package configguard

import "fmt"

type ActivateDeps struct {
	Run                   CodexRunner
	CodexHome, ConfigPath string
	Now                   func() string
}

func Activate(ActivateDeps) (*InstallManifest, error) {
	return nil, fmt.Errorf("activation not implemented")
}
func PreserveMultiAgentV2Table(pre, post string, enabled ...bool) (string, bool) { return "", false }
