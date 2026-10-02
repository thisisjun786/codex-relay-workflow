package adapter

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
)

// processIdentity is the start ticks and state of pid as /proc names them: what the fixtures that
// publish a worker write for the test process they stand in for.
func processIdentity(pid int64) (int64, string, error) {
	ticks, ok := service.StartTicks(int(pid)).(int64)
	if !ok {
		return 0, "", fmt.Errorf("process %d has no readable start ticks", pid)
	}
	return ticks, service.ProcessState(int(pid)), nil
}
