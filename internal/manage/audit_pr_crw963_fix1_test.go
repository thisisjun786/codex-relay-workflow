package manage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// CRW-963 (verification fix round 1): the targets are resolved before --max cuts them, so a
// target whose relay resolution fails does not take the place a healthy one could have had.
func TestCRW963ResolveBeforeMax(t *testing.T) {
	w := newCRW963World(t)
	previous := auditPRRelay
	auditPRRelay = func(ctx context.Context, e *Env, cfg *Config, args ...string) ([]byte, error) {
		if len(args) > 2 && args[0] == "assignment-find" && args[2] == "CRW-12" {
			return nil, errors.New("bad assignment")
		}
		return previous(ctx, e, cfg, args...)
	}
	t.Cleanup(func() { auditPRRelay = previous })
	code, stderr := w.run(1)
	got := w.ledgerSubjects()
	if len(got) != 1 || got[0] != "pr-7" {
		t.Fatalf("resolution before --max expected pr-7, got %v; code=%d stderr=%s", got, code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, "#12") {
		t.Fatalf("the failed resolution is named and the run exits 1: code=%d stderr=%s", code, stderr)
	}
}
