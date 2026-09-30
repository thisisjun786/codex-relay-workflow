package cli_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

func TestKindModuleNestedImportErrorMatchesPython(t *testing.T) {
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "--json", "--kind-module", "codex_session_relay.not_real", "status"}
	key := goldenKey(t, "codex-session-relay "+keyLabel(args...))
	var goOut, goErr bytes.Buffer
	goCode := cli.Execute(context.Background(), args, &goOut, &goErr)
	expectRunErr(t, key, goCode, goOut.String(), goErr.String(), append([]string{home}, args...)...)
}
