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
