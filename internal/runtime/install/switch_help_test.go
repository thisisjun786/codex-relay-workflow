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
	for _, want := range []string{"emergency", "hooks", "when one is missing", "is left as it is", "only what the switch recorded"} {
		if !strings.Contains(help, want) {
			t.Errorf("switch --help lacks %q:\n%s", want, help)
		}
	}
}

// The JSON document keeps the conflict note it had before CRW-1256 (the output is not part of the
// wording change); the human status text carries the emergency-stop explanation.
func TestInstallSwitchStatusConflictKeepsJSONNoteAndExplainsEmergencyStopInText(t *testing.T) {
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
	const jsonNote = "both the CRW hooks and the CXC plugin are on: run `crw install switch crw` (CXC off) or `crw install switch cxc`"
	var found bool
	for _, n := range s.Notes {
		if n == jsonNote {
			found = true
		}
		if strings.Contains(n, "emergency") {
			t.Errorf("JSON note %q changed wording", n)
		}
	}
	if !found {
		t.Fatalf("JSON notes lack the unchanged conflict note: %+v", s.Notes)
	}
	code, out, errOut := h.run("status")
	if code != 0 || errOut != "" {
		t.Fatalf("status: exit %d stderr=%q", code, errOut)
	}
	if strings.Contains(out, "return to CXC") || !strings.Contains(out, "emergency stop") || !strings.Contains(out, "hooks off") {
		t.Fatalf("human status lacks the emergency-stop explanation:\n%s", out)
	}
}

// With the CXC plugin gone, active=cxc is the common emergency state; the human status says what it means
// and the JSON values stay as they were.
func TestInstallSwitchStatusActiveCXCExplainsHooksOffInText(t *testing.T) {
	h := newSwitchHome(t, "[plugins.\"crw@crw\"]\nenabled=true\n")
	if code, _, errOut := h.run("cxc"); code != 0 {
		t.Fatalf("switch cxc: exit %d stderr=%q", code, errOut)
	}
	s := h.status()
	if s.State != "off" || s.Switch.Active != "cxc" {
		t.Fatalf("status = state %q active %q, want off / cxc", s.State, s.Switch.Active)
	}
	for _, n := range s.Notes {
		if strings.Contains(n, "emergency") {
			t.Errorf("JSON note %q gained wording", n)
		}
	}
	code, out, errOut := h.run("status")
	if code != 0 || errOut != "" {
		t.Fatalf("status: exit %d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "active=cxc") || !strings.Contains(out, "emergency stop") || !strings.Contains(out, "hooks are off") || strings.Contains(out, "return to CXC") {
		t.Fatalf("human status does not explain active=cxc as hooks off:\n%s", out)
	}
	// The switch result text ends in the same status.
	code, out, _ = h.run("cxc")
	if code != 0 || !strings.Contains(out, "emergency stop") {
		t.Fatalf("switch cxc text lacks the explanation (exit %d):\n%s", code, out)
	}
}
