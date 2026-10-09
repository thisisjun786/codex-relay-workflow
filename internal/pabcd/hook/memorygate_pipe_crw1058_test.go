package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1058: a literal printf piped into a shell is the program the shell runs. The write that program makes is judged as a
// write, so without a grant the gate blocks it, and with one the write passes and the grant is spent.
func TestMemoryGatePipeProgramWriteIsJudged(t *testing.T) {
	cwd, root, env := gateScene(t)
	payload := gateBash(t, cwd, "printf 'echo x > "+root+"/a' | bash")
	if reason := gateDeny(t, HandleMemoryWriteGate(payload, env)); !strings.Contains(reason, "Blocked a write") {
		t.Errorf("the write the pipe carries is not judged as a write: %s", reason)
	}
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	if out := HandleMemoryWriteGate(payload, env); out != "" {
		t.Errorf("with a grant the write must pass: %s", out)
	}
	if out := HandleMemoryWriteGate(payload, env); !strings.Contains(out, "MEMORY-WRITE-GATE") {
		t.Errorf("the grant must be spent after one write: %q", out)
	}
}
