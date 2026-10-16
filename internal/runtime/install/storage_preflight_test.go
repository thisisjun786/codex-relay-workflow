package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// CRW-958 round 2: install and update run the storage check of the environment they were given
// before they change anything. A structural fault of a root the configuration names refuses the
// run with nothing unpacked and no host record written; a warning is reported in the steps and the
// run goes on.
func TestInstallRunsTheStoragePreflightFirst(t *testing.T) {
	h := newHost(t)
	plain := filepath.Join(h.home, "plain")
	write(t, plain, "")
	config := filepath.Join(h.home, "crw.json")
	write(t, config, `{"paths":{"build_cache":"`+plain+`"}}`)
	o := h.options()
	o.Env = append(o.Env, "CRW_CONFIG="+config)
	result, code := install.Install(context.Background(), o, "install", install.Source{From: archive(t, "0.9.0", "")})
	if code != install.Refused || !strings.Contains(text(at(result, "refused")), "not_directory") || at(result, "steps") != nil {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if _, err := os.Stat(h.record); err == nil {
		t.Error("the host record was written by a refused preflight")
	}
	if entries, _ := filepath.Glob(filepath.Join(h.dest, "bin-*")); len(entries) != 0 {
		t.Errorf("something was unpacked: %v", entries)
	}

	// a warning alone (two uses share one directory) is reported and the install goes on
	shared := filepath.Join(h.home, "shared")
	write(t, config, `{"paths":{"data_root":"`+shared+`","tools_root":"`+shared+`"}}`)
	result, code = install.Install(context.Background(), o, "install", install.Source{From: archive(t, "0.9.0", "")})
	if code != install.OK || !strings.Contains(golden.Canon(at(result, "steps")), "shared_path") {
		t.Fatalf("a warning: exit %d\n%s", code, golden.Canon(result))
	}
}

// CRW-1577: a configuration without a tracking section is a warning of the install, never a refusal:
// the top-level warnings and the storage step report it with the repair command naming this file, and
// the file itself is left alone — the advisory cannot configure tracking automatically.
func TestInstallWarnsWhenTrackingIsMissing(t *testing.T) {
	h := newHost(t)
	config := filepath.Join(h.home, "crw.json")
	write(t, config, `{}`)
	o := h.options()
	o.Env = append(o.Env, "CRW_CONFIG="+config)
	result, code := install.Install(context.Background(), o, "install", install.Source{From: archive(t, "0.9.0", "")})
	if code != install.OK || at(result, "applied") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	warnings := golden.Canon(at(result, "warnings"))
	for _, want := range []string{"tracking_missing", `"level":"warning"`, "crw config tracking --repo OWNER/NAME --config ", config} {
		if !strings.Contains(warnings, want) {
			t.Errorf("the top-level warnings lack %q:\n%s", want, golden.Canon(result))
		}
	}
	if steps := golden.Canon(at(result, "steps")); !strings.Contains(steps, "tracking_missing") {
		t.Errorf("the storage step does not report tracking_missing:\n%s", steps)
	}
	if raw, err := os.ReadFile(config); err != nil || strings.TrimSpace(string(raw)) != "{}" {
		t.Errorf("the configuration was written by the advisory: %q %v", raw, err)
	}
}

// CRW-1577: `crw install` prints the one repair line of a missing tracking section to stderr while
// the JSON answer keeps it in warnings; a configured section warns nothing.
func TestInstallCLIPrintsTrackingRepairWarning(t *testing.T) {
	h := newHost(t)
	config := filepath.Join(h.home, "crw.json")
	write(t, config, `{}`)
	args := func(h *host) []string {
		return []string{"install", "--from", archive(t, "0.9.0", ""), "--issue", "CRW-1577", "--codex-home", h.codex, "--record", h.record,
			"--socket", h.fake.SocketPath, "--state", h.relayState}
	}
	code, out, errOut := h.main(t, append(h.env, "CRW_CONFIG="+config), args(h)...)
	if code != install.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if lines := strings.Count(errOut, "tracking_missing"); lines != 1 || !strings.Contains(errOut, "crw config tracking --repo OWNER/NAME --config "+config) {
		t.Errorf("stderr = %q, want one tracking_missing warning with the repair command naming the file", errOut)
	}
	if !strings.Contains(out, "tracking_missing") {
		t.Errorf("the JSON answer has no warnings:\n%s", out)
	}

	h2 := newHost(t)
	configured := filepath.Join(h2.home, "crw.json")
	write(t, configured, `{"tracking":{"kind":"github","repo":"o/work"}}`)
	code, out, errOut = h2.main(t, append(h2.env, "CRW_CONFIG="+configured), args(h2)...)
	if code != install.OK || strings.Contains(errOut, "tracking_missing") || !strings.Contains(out, "applied") {
		t.Errorf("a configured file: exit %d stderr %q\n%s", code, errOut, out)
	}
}
