package doctor_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
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

	// a section the verifier cannot use is ignored with a warning (final verification d4ed9281): the report is read, the verifier's
	// reading is the PATH fallback, and the warning names the setting
	write(t, config, `{"verifier":{"go_root":"~/.local/go/go"}}`, 0o600)
	tools = golden.Obj(record.Get(h.diagnose(t), "verifierTools"))
	crwDev = golden.Obj(record.Get(tools, "crwDev"))
	warnings = golden.Canon(record.Get(tools, "warnings"))
	if record.Get(tools, "config") != config || record.Get(crwDev, "mode") != "path" || !strings.Contains(warnings, "ignored") || !strings.Contains(warnings, "verifier.go_root") {
		t.Errorf("a malformed section:\n%s", golden.Canon(tools))
	}
}

// CRW-1319 (evaluation a48fe518 d3): with no Go root configured the doctor looks for one (the root of the go on PATH and the usual
// install locations, such as $HOME/.local/go/go), lists what it found with each version, and its warning names the command that
// records the first one. The test's home is below TMPDIR, which may be below /tmp, so the probe's temporary-directory rule points at
// a directory of the test's own, and a root that resolves below it is not offered.
func TestDoctorNamesAGoRootCandidate(t *testing.T) {
	h := newHost(t)
	empty := filepath.Join(h.home, "empty-path")
	mkdir(t, empty)
	h.env = h.env.With("PATH", empty)
	root := filepath.Join(h.home, ".local", "go", "go")
	write(t, filepath.Join(root, "bin", "go"), "#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n", 0o755)
	write(t, filepath.Join(h.home, ".config", "crw", "config.json"), `{}`, 0o600)
	cleared, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(cleared, "go-sdk")
	write(t, filepath.Join(gone, "bin", "go"), "#!/bin/sh\necho 'go version go1.28.0 linux/amd64'\n", 0o755)
	link(t, gone, filepath.Join(h.home, "sdk", "go1.28.0"))

	options := h.options()
	options.VerifierProbe = crwconfig.ProbeOptions{TemporaryDirs: []string{cleared}}
	tools := golden.Obj(record.Get(doctor.Diagnose(context.Background(), options), "verifierTools"))
	candidates := golden.Canon(record.Get(tools, "goRootCandidates"))
	if !strings.Contains(candidates, root) || !strings.Contains(candidates, "go1.27.1") {
		t.Fatalf("the Go root below the home is not a candidate:\n%s", golden.Canon(tools))
	}
	if strings.Contains(candidates, "go1.28.0") {
		t.Errorf("a root that resolves below the temporary directory is a candidate:\n%s", candidates)
	}
	if !strings.Contains(golden.Canon(record.Get(tools, "warnings")), "crw config verifier --go-root ") {
		t.Errorf("the warnings do not name the command that records a candidate:\n%s", golden.Canon(tools))
	}
}
