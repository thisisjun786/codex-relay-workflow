package install_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// The tests below are the findings of the CRW-201 pre-merge evaluation of 2f010b1b (d2, d3, d4).

// d2: status reads the activation of a plugin by the key's decoded name, as the switch edits it.
func TestInstallSwitchStatusReadsQuotedAndAlternateSpellings(t *testing.T) {
	for name, tc := range map[string]struct {
		config                        string
		crwEnabled, cxcPresent, cxcOn bool
		stateBefore, stateAfterSwitch string
	}{
		"quoted keys":   {"[plugins.\"crw@crw\"]\n\"enabled\" = true\n\n[plugins.\"codexclaw@codexclaw\"]\n\"enabled\" = true\n", true, true, true, "cxc", "crw"},
		"literal keys":  {"[plugins.\"crw@crw\"]\n'enabled' = true\n\n[plugins.\"codexclaw@codexclaw\"]\n'enabled'=true\n", true, true, true, "cxc", "crw"},
		"escaped key":   {"[plugins.\"crw@crw\"]\nenabled = true\n\n[plugins.\"codexclaw@codexclaw\"]\n\"en\\u0061bled\" = true\n", true, true, true, "cxc", "crw"},
		"literal table": {"[plugins.\"crw@crw\"]\nenabled = true\n\n[plugins.'codexclaw@codexclaw']\nenabled = true\n", true, true, true, "cxc", "crw"},
		"spaced table":  {"[plugins.\"crw@crw\"]\nenabled = true\n\n[ plugins . \"codexclaw@codexclaw\" ]\nenabled = true\n", true, true, true, "cxc", "crw"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSwitchHome(t, tc.config)
			s := h.status()
			if s.State != tc.stateBefore || !s.Plugins["codexclaw"].Present || !s.Plugins["codexclaw"].Enabled || !s.Plugins["crw"].Enabled {
				t.Fatalf("status before = %+v", s)
			}
			if code, _, errOut := h.run("crw"); code != 0 {
				t.Fatalf("switch crw: exit %d %q", code, errOut)
			}
			s = h.status()
			if s.State != tc.stateAfterSwitch || s.Plugins["codexclaw"].Enabled || !s.Plugins["codexclaw"].Present || !s.Plugins["crw"].Enabled {
				t.Fatalf("status after switch crw = %+v\nconfig:\n%s", s, h.config())
			}
			if code, _, errOut := h.run("cxc"); code != 0 {
				t.Fatalf("switch cxc: exit %d %q", code, errOut)
			}
			if s = h.status(); s.State != "cxc" || !s.Plugins["codexclaw"].Enabled {
				t.Fatalf("status after switch cxc = %+v", s)
			}
		})
	}
	t.Run("a quoted false crw key is not running", func(t *testing.T) {
		h := newSwitchHome(t, "[plugins.\"crw@crw\"]\n\"enabled\" = false\n\n[plugins.\"codexclaw@codexclaw\"]\nenabled = true\n")
		if code, _, errOut := h.run("crw"); code != 0 {
			t.Fatalf("switch crw: exit %d %q", code, errOut)
		}
		s := h.status()
		if s.Plugins["crw"].Enabled || s.State != "off" {
			t.Fatalf("status = %+v", s)
		}
	})
}

// d4: the installer's cancellation reaches `crw install switch`.
func TestInstallSwitchHonoursACancelledContext(t *testing.T) {
	h := newSwitchHome(t, switchConfig)
	before := h.config()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, action := range []string{"crw", "cxc"} {
		var stdout, stderr bytes.Buffer
		code := install.Main(ctx, []string{"switch", action}, h.env, &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "cancel") {
			t.Fatalf("switch %s: exit %d stdout=%q stderr=%q", action, code, stdout.String(), stderr.String())
		}
	}
	if h.config() != before {
		t.Fatalf("a cancelled switch changed config.toml:\n%s", h.config())
	}
	if s := h.status(); s.Switch.Active != "" || s.Record.Present {
		t.Fatalf("a cancelled switch left state: %+v", s)
	}
}
