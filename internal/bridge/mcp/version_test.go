package mcp

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// The bridge's --version and its App Server clientInfo.version are the Python package's
// version, read from the real `codex-thread-bridge --version`, so a later bump of one side
// fails here instead of drifting silently.
func TestVersion_is_the_python_bridge_package_version(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	command := exec.Command("uv", "run", "--no-sync", "--project", root, "codex-thread-bridge", "--version")
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	python, err := command.Output()
	if err != nil {
		t.Fatalf("the Python reference is required: %v", err)
	}
	_, env := isolated(t)
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"--version"}, env, io.NopCloser(strings.NewReader("")), nopWriteCloser{&stdout}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if stdout.String() != string(python) || strings.TrimSpace(string(python)) != appserver.BridgeVersion {
		t.Fatalf("go %q, python %q, clientInfo %q", stdout.String(), python, appserver.BridgeVersion)
	}
}
