package hook

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// MemoryWriteAttempt says why a PreToolUse call counts as a memory write.
type MemoryWriteAttempt struct {
	Surface string // "tool", "edit" or "shell"; empty when the call is no memory write
	Target  string
}

// HandleMemoryWriteGate is the PreToolUse leg of MEMORY-WRITE-GATE-01.
func HandleMemoryWriteGate(raw string, env host.LookupEnv) string {
	return memoryGateHandle(raw, env, state.WriteState)
}

func memoryGateHandle(raw string, env host.LookupEnv, write func(string, state.State) error) string {
	return ""
}

func memoryGateClassify(tool string, input any, cwd string, env host.LookupEnv) MemoryWriteAttempt {
	return MemoryWriteAttempt{}
}

func memoryGatePatchTargets(patch string) []string { return nil }

type memoryGateEnv struct{}

func newMemoryGateEnv(env host.LookupEnv) memoryGateEnv { return memoryGateEnv{} }

func (memoryGateEnv) abs(raw, cwd string) (clean, joined string) { return "", "" }
