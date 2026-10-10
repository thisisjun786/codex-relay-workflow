package spawn

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1121 (verification round 1): deliveries of one root event that race mint one grant.

// Two deliveries of one root event that both pass the replay lookup before either mints: one grant, one answer.
func TestSpawnHookConcurrentDeliveriesOfOneEventMintOneGrant(t *testing.T) {
	rig := spawnReapplyRig(t, "")
	payload := spawnReapplyPayload(rig.ws, "same-call", `{"agent_type":"explorer","message":"CRW-SUBSPAWN-ALLOWED coordinate"}`)
	read := spawnHookSettings
	var entered sync.WaitGroup
	entered.Add(2)
	ready := make(chan struct{})
	spawnHookSettings = func(env host.LookupEnv) role.SettingsSnapshot {
		entered.Done()
		<-ready
		return read(env)
	}
	t.Cleanup(func() { spawnHookSettings = read })
	answers := make([]string, 2)
	var done sync.WaitGroup
	for i := range answers {
		done.Add(1)
		go func() { defer done.Done(); answers[i] = RunSpawnAttachHook(payload, rig.env) }()
	}
	entered.Wait()
	close(ready)
	done.Wait()
	grants, _ := filepath.Glob(filepath.Join(rig.tmp, "*", "*", "*.json"))
	if answers[0] != answers[1] || len(grants) != 1 || !strings.Contains(answers[0], "[CRW-SUBSPAWN-GRANT:") {
		t.Fatalf("the event minted %d grants; the answers are equal: %v\n%.300q\n%.300q", len(grants), answers[0] == answers[1], answers[0], answers[1])
	}
}
