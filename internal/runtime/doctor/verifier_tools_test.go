package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// records the first one. A root is judged by content, not place (CRW-1579): a working root whose link resolves into another
// directory is offered too.
func TestDoctorNamesAGoRootCandidate(t *testing.T) {
	h := newHost(t)
	empty := filepath.Join(h.home, "empty-path")
	mkdir(t, empty)
	h.env = h.env.With("PATH", empty)
	root := filepath.Join(h.home, ".local", "go", "go")
	write(t, filepath.Join(root, "bin", "go"), "#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n", 0o755)
	write(t, filepath.Join(h.home, ".config", "crw", "config.json"), `{}`, 0o600)
	elsewhere, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(elsewhere, "go-sdk")
	write(t, filepath.Join(linked, "bin", "go"), "#!/bin/sh\necho 'go version go1.28.0 linux/amd64'\n", 0o755)
	link(t, linked, filepath.Join(h.home, "sdk", "go1.28.0"))

	tools := golden.Obj(record.Get(doctor.Diagnose(context.Background(), h.options()), "verifierTools"))
	candidates := golden.Canon(record.Get(tools, "goRootCandidates"))
	if !strings.Contains(candidates, root) || !strings.Contains(candidates, "go1.27.1") {
		t.Fatalf("the Go root below the home is not a candidate:\n%s", golden.Canon(tools))
	}
	if !strings.Contains(candidates, filepath.Join(h.home, "sdk", "go1.28.0")) || !strings.Contains(candidates, "go1.28.0") {
		t.Errorf("a working root whose link resolves into another directory is not a candidate:\n%s", candidates)
	}
	if !strings.Contains(golden.Canon(record.Get(tools, "warnings")), "crw config verifier --go-root ") {
		t.Errorf("the warnings do not name the command that records a candidate:\n%s", golden.Canon(tools))
	}
}

// CRW-1579 (U-A5-2): the probe's home comes from the one place (tmploc). With a relative TMPDIR the home is refused with
// tmploc's reason, and the verifierTools entry reports it in its warnings. Before, the home was made relative to the working
// directory when ./rel existed, and the go ran there with a relative HOME; now nothing is made below the working directory.
func TestVerifierToolsReport_probeHomeError(t *testing.T) {
	h := newHost(t)
	root := filepath.Join(h.home, "go")
	write(t, filepath.Join(root, "bin", "go"), "#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n", 0o755)
	write(t, filepath.Join(h.home, ".config", "crw", "config.json"), `{"verifier":{"go_root":"`+root+`"}}`, 0o600)
	cwd := t.TempDir()
	mkdir(t, filepath.Join(cwd, "rel"))
	t.Chdir(cwd)
	t.Setenv("TMPDIR", "rel")

	tools := doctor.VerifierToolsReport(context.Background(), h.env)
	warnings := golden.Canon(record.Get(tools, "warnings"))
	if !strings.Contains(warnings, "tmpdir_not_absolute") || !strings.Contains(warnings, `TMPDIR=\"rel\"`) {
		t.Errorf("the probe's home error is not in the warnings:\n%s", golden.Canon(tools))
	}
	if record.Get(tools, "goVersion") != nil {
		t.Errorf("a go probed without a home has a version:\n%s", golden.Canon(tools))
	}
	if entries, err := os.ReadDir(filepath.Join(cwd, "rel")); err != nil || len(entries) != 0 {
		t.Errorf("the probe made something below the working directory: %v %v", entries, err)
	}

	// no root configured and no go on PATH, a root at a usual location: only the search for candidates probes it, and its
	// warning gives the same reason instead of "none was found"
	empty := filepath.Join(h.home, "empty-path")
	mkdir(t, empty)
	write(t, filepath.Join(h.home, ".local", "go", "go", "bin", "go"), "#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n", 0o755)
	write(t, filepath.Join(h.home, ".config", "crw", "config.json"), `{}`, 0o600)
	tools = doctor.VerifierToolsReport(context.Background(), h.env.With("PATH", empty))
	warnings = golden.Canon(record.Get(tools, "warnings"))
	if !strings.Contains(warnings, "could not be probed") || !strings.Contains(warnings, "tmpdir_not_absolute") ||
		strings.Contains(warnings, "no Go toolchain root was found") {
		t.Errorf("the search's home error is not in the warnings:\n%s", golden.Canon(tools))
	}
}
