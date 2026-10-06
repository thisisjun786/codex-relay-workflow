package install_test

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/migrate"
)

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
