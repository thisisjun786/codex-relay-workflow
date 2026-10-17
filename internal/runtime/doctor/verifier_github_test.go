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

// CRW-1594: the verifierTools entry names the verifier's kind and, for github-actions, its repository and
// workflow; the local kind reports the remote fields as absent and changes nothing else.
func TestDoctorReportsTheVerifierKind(t *testing.T) {
	h := newHost(t)
	bin := filepath.Join(h.home, "bin")
	mkdir(t, bin)
	write(t, filepath.Join(bin, "git"), "#!/bin/sh\necho 'git version 2.43.0'\n", 0o755)
	write(t, filepath.Join(bin, "gitleaks"), "#!/bin/sh\necho 'gitleaks version 8.18.0'\n", 0o755)
	write(t, filepath.Join(bin, "gh"), `#!/bin/sh
case "$1 $2" in
  "auth status") exit 0 ;;
  "repo view") echo '{"viewerPermission":"WRITE"}' ;;
  "api --paginate") echo '{"workflows":[{"id":7,"name":"CRW CI","path":".github/workflows/ci.yml","state":"active"}]}' ;;
esac
exit 0
`, 0o755)
	h.env = h.env.With("PATH", bin)
	config := filepath.Join(h.home, ".config", "crw", "config.json")
	write(t, config, `{"verifier":{"kind":"github-actions","repository":"owner/repo","workflow":"CRW CI"}}`, 0o600)

	tools := doctor.VerifierToolsReport(context.Background(), h.env)
	if record.Get(tools, "kind") != "github-actions" || record.Get(tools, "repository") != "owner/repo" ||
		record.Get(tools, "workflow") != "CRW CI" {
		t.Errorf("the remote fields:\n%s", golden.Canon(tools))
	}
	crwDev := golden.Obj(record.Get(tools, "crwDev"))
	if record.Get(crwDev, "runnable") != true {
		t.Errorf("the remote probe is not runnable:\n%s", golden.Canon(tools))
	}

	write(t, config, `{}`, 0o600)
	tools = doctor.VerifierToolsReport(context.Background(), h.env)
	if record.Get(tools, "kind") != "local" || record.Get(tools, "repository") != nil || record.Get(tools, "workflow") != nil {
		t.Errorf("the local fields:\n%s", golden.Canon(tools))
	}
	if _, err := os.Stat(filepath.Join(h.home, "touched")); err == nil {
		t.Error("the probe wrote into the home")
	}
}

// CRW-1594: VerifierToolsReport of a remote whose gh cannot authenticate warns, does not fault.
func TestDoctorReportsARemoteThatCannotAuthenticate(t *testing.T) {
	h := newHost(t)
	bin := filepath.Join(h.home, "bin")
	mkdir(t, bin)
	write(t, filepath.Join(bin, "git"), "#!/bin/sh\necho 'git version 2.43.0'\n", 0o755)
	write(t, filepath.Join(bin, "gitleaks"), "#!/bin/sh\necho ok\n", 0o755)
	write(t, filepath.Join(bin, "gh"), "#!/bin/sh\nexit 1\n", 0o755)
	h.env = h.env.With("PATH", bin)
	write(t, filepath.Join(h.home, ".config", "crw", "config.json"),
		`{"verifier":{"kind":"github-actions","repository":"owner/repo"}}`, 0o600)

	tools := doctor.VerifierToolsReport(context.Background(), h.env)
	crwDev := golden.Obj(record.Get(tools, "crwDev"))
	if record.Get(tools, "kind") != "github-actions" || record.Get(crwDev, "runnable") != false {
		t.Errorf("the remote report:\n%s", golden.Canon(tools))
	}
	if warnings := golden.Canon(record.Get(tools, "warnings")); !strings.Contains(warnings, "auth") && !strings.Contains(warnings, "logged") {
		t.Errorf("the login failure is not a warning:\n%s", warnings)
	}
}
