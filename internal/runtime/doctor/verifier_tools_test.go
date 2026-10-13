package doctor_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// CRW-1319: the doctor report shows the verifier's tools as the run reads them: the Go toolchain it runs (where it is, where that
// came from, its version) and whether crw-dev is runnable. A missing or broken tool is a warning, never a fault of the report.
func TestDoctorReportsTheVerifierTools(t *testing.T) {
	h := newHost(t)
	empty := filepath.Join(h.home, "empty-path")
	mkdir(t, empty)
	h.env = h.env.With("PATH", empty)
	root := filepath.Join(h.home, "go")
	write(t, filepath.Join(root, "bin", "go"), "#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n", 0o755)
	config := filepath.Join(h.home, ".config", "crw", "config.json")
	write(t, config, `{"verifier":{"go_root":"`+root+`"}}`, 0o600)

	tools := golden.Obj(record.Get(h.diagnose(t), "verifierTools"))
	if tools == nil {
		t.Fatalf("the doctor report has no verifierTools entry")
	}
	crwDev := golden.Obj(record.Get(tools, "crwDev"))
	if record.Get(tools, "config") != config || record.Get(tools, "go") != filepath.Join(root, "bin", "go") || record.Get(tools, "goSource") != "config" ||
		record.Get(tools, "goVersion") != "go1.27.1" || record.Get(crwDev, "mode") != "tree" || record.Get(crwDev, "runnable") != true ||
		golden.Canon(record.Get(tools, "warnings")) != "[]" {
		t.Errorf("a configured Go root:\n%s", golden.Canon(tools))
	}

	write(t, config, `{"verifier":{"go_root":"`+root+`","crw_dev":"`+filepath.Join(h.home, "no-crw-dev")+`"}}`, 0o600)
	tools = golden.Obj(record.Get(h.diagnose(t), "verifierTools"))
	crwDev = golden.Obj(record.Get(tools, "crwDev"))
	if record.Get(crwDev, "mode") != "binary" || record.Get(crwDev, "runnable") != false || !strings.Contains(golden.Canon(record.Get(tools, "warnings")), "crw-dev") {
		t.Errorf("a crw-dev that does not run:\n%s", golden.Canon(tools))
	}

	write(t, config, `{}`, 0o600)
	tools = golden.Obj(record.Get(h.diagnose(t), "verifierTools"))
	crwDev = golden.Obj(record.Get(tools, "crwDev"))
	warnings := golden.Canon(record.Get(tools, "warnings"))
	if record.Get(tools, "go") != nil || record.Get(crwDev, "mode") != "path" || record.Get(crwDev, "runnable") != false ||
		!strings.Contains(warnings, "verifier.go_root") || !strings.Contains(warnings, "go") {
		t.Errorf("nothing configured, nothing on PATH:\n%s", golden.Canon(tools))
	}
}
