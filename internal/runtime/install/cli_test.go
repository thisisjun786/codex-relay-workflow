package install_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/migrate"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// `crw install register-mcp --re-register-policy` is the re-registration path of CRW-868. It is
// dispatched before the create path's owner contract, it takes the file from --execution-policy, and
// a policy it was not given is a usage error rather than a refusal; the frozen install usage line
// still names register-mcp and only the commands it named before.
func TestInstallReRegisterPolicyCommand(t *testing.T) {
	h := newHost(t)
	env := append(append(scope.Env{}, h.env...), "CRW_HOME="+filepath.Join(h.home, "crw-home"))
	policy := filepath.Join(h.home, "policy.json")
	write(t, policy, policyText)

	// No policy named: usage, and the record is never read.
	var stdout, stderr strings.Builder
	if code := install.Main(context.Background(), []string{"register-mcp", "--re-register-policy"}, env, &stdout, &stderr); code != install.Usage ||
		!strings.Contains(stdout.String(), `"outcome": "CONFLICT"`) || !strings.Contains(stdout.String(), "--execution-policy") {
		t.Fatalf("no policy: exit %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	// A policy named and no record yet: the re-registration path answers record_absent and writes
	// nothing.
	stdout.Reset()
	stderr.Reset()
	code := install.Main(context.Background(), []string{"register-mcp", "--re-register-policy", "--execution-policy", policy}, env, &stdout, &stderr)
	if code != install.Refused || !strings.Contains(stdout.String(), `"outcome": "record_absent"`) || stderr.Len() != 0 {
		t.Fatalf("no record: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}

	// The frozen usage line is unchanged by the new flag.
	stdout.Reset()
	if code := install.Main(context.Background(), []string{"help"}, env, &stdout, &strings.Builder{}); code != 0 {
		t.Fatalf("install help: exit %d", code)
	}
	want := "usage: crw install {install,update,rollback,remove,status,register-mcp,hook,register-service} ...\n"
	if stdout.String() != want {
		t.Fatalf("install usage = %q, want %q", stdout.String(), want)
	}
}

// The install command row of `crw install migrate-state`: it is dispatched before the generic
// install options, it has its own help, and the frozen install usage line is unchanged.
func TestInstallMigrateStateCommand(t *testing.T) {
	found := false
	for _, command := range install.Commands {
		if command == "migrate-state" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Commands = %v, want migrate-state", install.Commands)
	}
	var stdout, stderr strings.Builder
	if code := install.Main(context.Background(), []string{"migrate-state", "--help"}, nil, &stdout, &stderr); code != 0 || stdout.String() != migrate.UsageText || stderr.Len() != 0 {
		t.Fatalf("help: exit %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	// The frozen install usage names exactly the commands it named before this one arrived.
	stdout.Reset()
	if code := install.Main(context.Background(), []string{"help"}, nil, &stdout, &strings.Builder{}); code != 0 {
		t.Fatalf("install help: exit %d", code)
	}
	want := "usage: crw install {install,update,rollback,remove,status,register-mcp,hook,register-service} ...\n"
	if stdout.String() != want {
		t.Fatalf("install usage = %q, want %q", stdout.String(), want)
	}
	// Its own invalid arguments are usage errors, and the installer's invalid-choice list names it.
	stdout.Reset()
	stderr.Reset()
	if code := install.Main(context.Background(), []string{"migrate-state", "--scope", "bogus"}, nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unknown scope") {
		t.Fatalf("invalid scope: exit %d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := install.Main(context.Background(), []string{"unpack"}, nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "migrate-state") {
		t.Fatalf("invalid choice: exit %d stderr=%q", code, stderr.String())
	}
}
