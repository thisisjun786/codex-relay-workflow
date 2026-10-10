package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// CRW-1256: after the full CXC removal (cutover S9.3b) `switch cxc` means only the emergency stop of the
// ported hooks (CRW-201, CRW-1228). The help and the status text must not present it as a return to CXC
// or as replacing CXC-owned role files.
func TestInstallSwitchHelpDescribesCXCAsTheEmergencyStop(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := install.Main(context.Background(), []string{"switch", "--help"}, nil, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("switch --help: exit %d stderr=%q", code, stderr.String())
	}
	help := stdout.String()
	for _, banned := range []string{"return to CXC", "CXC-owned"} {
		if strings.Contains(help, banned) {
			t.Errorf("switch --help still contains %q:\n%s", banned, help)
		}
	}
	for _, want := range []string{"emergency", "hooks"} {
		if !strings.Contains(help, want) {
			t.Errorf("switch --help lacks %q:\n%s", want, help)
		}
	}
}

// The conflict note names `switch cxc` as the emergency stop, not as a way back to CXC.
func TestInstallSwitchStatusConflictNoteNamesCXCAsEmergencyStop(t *testing.T) {
	h := newSwitchHome(t, switchConfig)
	// A damaged switch.json reads as on, so with both plugins enabled it is a conflict.
	if err := os.MkdirAll(filepath.Dir(hookswitch.Path(h.home)), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookswitch.Path(h.home), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := h.status()
	if s.State != "conflict" {
		t.Fatalf("status = %+v, want a conflict", s)
	}
	var note string
	for _, n := range s.Notes {
		if strings.Contains(n, "crw install switch cxc") {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("no conflict note names `crw install switch cxc`: %+v", s.Notes)
	}
	if strings.Contains(note, "return to CXC") || !strings.Contains(note, "emergency") {
		t.Fatalf("conflict note = %q, want the emergency stop wording", note)
	}
}
