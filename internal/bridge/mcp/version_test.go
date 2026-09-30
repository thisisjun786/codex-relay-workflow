package mcp

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
)

// The bridge's --version and its App Server clientInfo.version are the bridge component's
// version in the compatibility definition (internal/runtime/definition, the only copy since todo
// 44 removed scripts/crw_runtime/components.json), so a later bump of one side fails here
// instead of drifting silently. Until todo 44 this read the Python package's real
// `codex-thread-bridge --version`.
func TestVersion_is_the_bridge_components_definition_version(t *testing.T) {
	bridge, ok := definition.Of(definition.Bridge)
	if !ok || bridge.Version == "" {
		t.Fatalf("no %s component in the definition", definition.Bridge)
	}
	_, env := isolated(t)
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"--version"}, env, io.NopCloser(strings.NewReader("")), nopWriteCloser{&stdout}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if stdout.String() != bridge.Version+"\n" || appserver.BridgeVersion != bridge.Version {
		t.Fatalf("go %q, definition %q, clientInfo %q", stdout.String(), bridge.Version, appserver.BridgeVersion)
	}
}
