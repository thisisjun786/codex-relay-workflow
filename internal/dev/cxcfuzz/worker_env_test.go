//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordEnvShim is a worker that answers each request with the homes it started under, so a test reads the
// environment the worker was launched with. It answers the start-up handshake the same way.
const recordEnvShim = `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9][0-9]*\).*/\1/')
  printf '{"id":%s,"output":{"home":"%s","codexHome":"%s","crwHome":"%s","codexclawHome":"%s","tmp":"%s","xdgConfig":"%s"}}\n' "$id" "$HOME" "$CODEX_HOME" "$CRW_HOME" "$CODEXCLAW_HOME" "$TMPDIR" "$XDG_CONFIG_HOME"
done
`

// writeRecordEnvShim puts the environment-recording worker under a test directory.
func writeRecordEnvShim(t *testing.T) string {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "record-env.sh")
	if err := os.WriteFile(shim, []byte(recordEnvShim), 0o755); err != nil {
		t.Fatal(err)
	}
	return shim
}

// A worker's environment is the harness's own start-up root, never the caller's homes: the caller here names
// real-looking homes, and the worker, including its start-up handshake, must start under a root the pool made
// (CRW-978 c4, CRW-971 item 1).
func TestWorkerEnvironmentIsTheHarnessRootNotTheCallers(t *testing.T) {
	caller := []string{
		"HOME=/caller/real/home", "CODEX_HOME=/caller/real/home/.codex", "CRW_HOME=/caller/real/crw-home",
		"CODEXCLAW_HOME=/caller/real/codexclaw-home", "TMPDIR=/caller/real/tmp", "XDG_CONFIG_HOME=/caller/real/xdg",
		"PATH=" + os.Getenv("PATH"),
	}
	pool, err := NewPool(Oracle{Command: "sh", Shim: writeRecordEnvShim(t)}, 1, 5*time.Second, 10*time.Second, caller)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	// The empty root is the start-up handshake's: nothing in the request changes the worker's own environment.
	reply, err := pool.Call(`null`, "")
	if err != nil {
		t.Fatal(err)
	}
	var seen map[string]string
	if err := json.Unmarshal([]byte(reply), &seen); err != nil {
		t.Fatalf("the reply %q is not the recorded environment: %v", reply, err)
	}
	if len(seen) != 6 {
		t.Fatalf("the worker recorded %d homes, want 7", len(seen))
	}
	for name, value := range seen {
		if strings.HasPrefix(value, "/caller/") || !strings.Contains(value, "cxcfuzz-worker-") {
			t.Errorf("%s = %q, want a path under the harness start-up root", name, value)
		}
	}
}

// Control: a worker started with none of the home variables set still starts and answers as before.
func TestWorkerStartsWithNoHomeVariablesSet(t *testing.T) {
	pool, err := NewPool(Oracle{Command: "sh", Shim: writeRecordEnvShim(t)}, 1, 5*time.Second, 10*time.Second, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	if _, err := pool.Call(`null`, ""); err != nil {
		t.Fatalf("a worker with no home variables did not answer: %v", err)
	}
}
