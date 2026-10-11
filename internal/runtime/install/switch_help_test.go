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
	// The restore puts a recorded `enabled = true` of a remaining CXC plugin back (CRW-1256 evaluation d1),
	// so the help must not promise that switch cxc never turns CXC on.
	if strings.Contains(help, "does not re-activate CXC") {
		t.Errorf("switch --help promises that switch cxc does not re-activate CXC, but the restore can turn a remaining CXC plugin on:\n%s", help)
	}
	for _, want := range []string{"emergency", "hooks", "when one is missing", "is left as it is", "only what the switch recorded", "never installs CXC", "comes back on", "recorded as enabled = false", "with no recorded entry"} {
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
	// The CXC plugin is on in this home and no switch recorded its line, so switch cxc leaves it on.
	if strings.Contains(out, "does not re-activate CXC") || !strings.Contains(out, "no CXC plugin line is recorded, so the CXC plugin stays on") {
		t.Fatalf("the conflict note must say that with no recorded line the CXC plugin stays on:\n%s", out)
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

// CRW-1256 evaluation d1: on a home that still has a CXC plugin entry (enabled = true), `switch crw` records
// the line and turns the plugin off, and `switch cxc` puts the recorded line back, so CXC is on again. The
// human text of that round trip must say so and must not say that CXC is not active.
func TestInstallSwitchLegacyRoundTripTextSaysTheCXCPluginIsOnAgain(t *testing.T) {
	h := newSwitchHome(t, switchConfig)
	if code, _, errOut := h.run("crw"); code != 0 {
		t.Fatalf("switch crw: exit %d stderr=%q", code, errOut)
	}
	code, out, errOut := h.run("cxc")
	if code != 0 || errOut != "" {
		t.Fatalf("switch cxc: exit %d stderr=%q", code, errOut)
	}
	if s := h.status(); s.State != "cxc" || s.Switch.Active != "cxc" || !s.Plugins["codexclaw"].Enabled {
		t.Fatalf("status after the round trip = %+v, want state cxc with the CXC plugin enabled", s)
	}
	for _, text := range []string{out, h.mustRun("status")} {
		if strings.Contains(text, "does not mean CXC is active") || strings.Contains(text, "does not re-activate CXC") {
			t.Fatalf("the text says CXC is not active while the CXC plugin is enabled:\n%s", text)
		}
		if !strings.Contains(text, "emergency stop") || !strings.Contains(text, "hooks are off") || !strings.Contains(text, "The CXC plugin codexclaw@codexclaw is enabled") {
			t.Fatalf("the text lacks the emergency stop with the enabled CXC plugin:\n%s", text)
		}
	}
}

func (h *switchHome) mustRun(args ...string) string {
	h.t.Helper()
	code, out, errOut := h.run(args...)
	if code != 0 || errOut != "" {
		h.t.Fatalf("switch %v: exit %d stderr=%q", args, code, errOut)
	}
	return out
}

// CRW-1256 evaluation (fix round 3): `switch cxc` puts back the CXC plugin line recorded before `switch crw`
// (configguard RunSwitch, RestoreTableKeyExact), so in a conflict the CXC plugin stays on only when no line
// is recorded or the recorded line leaves it on; a recorded enabled = false turns it off. The human conflict
// note must say which, and the real `switch cxc` must then do what the note said.
func TestInstallSwitchConflictNoteFollowsTheRecordedCXCLine(t *testing.T) {
	const cxcOn = "[plugins.\"crw@crw\"]\nenabled=true\n\n[plugins.\"codexclaw@codexclaw\"]\nenabled=true\n"
	const cxcOff = "[plugins.\"crw@crw\"]\nenabled=true\n\n[plugins.\"codexclaw@codexclaw\"]\nenabled=false\n"
	for name, tc := range map[string]struct {
		before  string // config.toml at `switch crw`; "" means no switch ran (no record)
		note    string // what the human conflict note says
		cxcOnAt bool   // the CXC plugin after the real `switch cxc`
	}{
		"recorded enabled=true":  {cxcOn, "puts back the recorded CXC plugin line `enabled=true`, so the CXC plugin stays on", true},
		"recorded enabled=false": {cxcOff, "puts back the recorded CXC plugin line `enabled=false`, so the CXC plugin is turned off", false},
		"no record":              {"", "no CXC plugin line is recorded, so the CXC plugin stays on", true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSwitchHome(t, cxcOn)
			if tc.before != "" {
				if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(tc.before), 0o600); err != nil {
					t.Fatal(err)
				}
				h.mustRun("crw")
			} else {
				// No switch ran: a damaged switch.json reads as crw (on).
				if err := os.MkdirAll(filepath.Dir(hookswitch.Path(h.home)), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(hookswitch.Path(h.home), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The CXC plugin is turned on by hand since: a conflict.
			if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(cxcOn), 0o600); err != nil {
				t.Fatal(err)
			}
			if s := h.status(); s.State != "conflict" || (tc.before != "") != s.Record.Present {
				t.Fatalf("status = %+v, want a conflict (record present %v)", s, tc.before != "")
			}
			out := h.mustRun("status")
			if !strings.Contains(out, tc.note) {
				t.Errorf("the conflict note lacks %q:\n%s", tc.note, out)
			}
			if !tc.cxcOnAt && strings.Contains(out, "stays on") {
				t.Errorf("the conflict note says the CXC plugin stays on, but switch cxc turns it off:\n%s", out)
			}
			h.mustRun("cxc")
			if s := h.status(); s.Plugins["codexclaw"].Enabled != tc.cxcOnAt || s.Switch.Active != "cxc" {
				t.Fatalf("after switch cxc: CXC plugin enabled %v active %q, want enabled %v active cxc\n%s", s.Plugins["codexclaw"].Enabled, s.Switch.Active, tc.cxcOnAt, h.config())
			}
		})
	}
}
