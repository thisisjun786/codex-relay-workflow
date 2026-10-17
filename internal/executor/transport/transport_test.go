package transport

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"strings"
	"testing"
)

func TestSSHUsesFixedCommandAndPinnedHostKeys(t *testing.T) {
	p := Process{Host: crwconfig.ExecutionHost{Transport: "ssh", Endpoint: "approved-alias", KnownHosts: "/synthetic/known_hosts"}}
	cmd := p.Command(context.Background())
	line := strings.Join(cmd.Args, " ")
	if cmd.Args[len(cmd.Args)-1] != "crw worker serve" || !strings.Contains(line, "StrictHostKeyChecking=yes") || !strings.Contains(line, "BatchMode=yes") {
		t.Fatal(line)
	}
}
func TestFrameRejectsExtraUnknownAndOversizedInput(t *testing.T) {
	for _, raw := range []string{`{"schema":"crw-worker/1","unexpected":true}`, `{} {}`, strings.Repeat("x", MaxFrame+1)} {
		var r Request
		if err := Decode(strings.NewReader(raw), &r); err == nil {
			t.Fatal("bad frame accepted")
		}
	}
}
