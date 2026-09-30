package install_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/exercise"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	expected "github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// A policy file whose name holds a byte that is not UTF-8 is recorded as runtime_install.py's
// bridgerecord records it - the path as os.fsdecode spells it, the byte as its surrogate escape
// written "\udc80", and the digest over the file itself - byte for byte the record
// bridgerecord.document and json.dumps write for the same input (the golden, which began as that
// record with the home and the fake App Server's directory spelled as placeholders). The
// packaged launcher, which fs-encodes the path back, then starts the Go bridge under that policy.
func TestRegisterMCPRecordsANonUTF8PolicyPathAsPythonDoes(t *testing.T) {
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	policy := filepath.Join(h.home, "pol\x80icy.json")
	write(t, policy, policyText)
	sum := sha256.Sum256([]byte(policyText))
	digest := hex.EncodeToString(sum[:])
	homeOnly := []string{"HOME=" + h.home}
	var stdout, stderr strings.Builder
	if code := install.Main(context.Background(), []string{"register-mcp", "--owner", "plugin", "--execution-policy", policy,
		"--bridge-arg=--socket", "--bridge-arg=" + h.fake.SocketPath, "--bridge-arg=--state-dir", "--bridge-arg=" + filepath.Join(h.home, "bridge-ledger")}, scope.Env(homeOnly), &stdout, &stderr); code != install.OK {
		t.Fatalf("register-mcp: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	got := readFile(t, recordPath)
	if !strings.Contains(got, `"path": "`+h.home+`/pol\udc80icy.json"`) || strings.ContainsRune(got, '�') {
		t.Fatalf("the policy path is not recorded as its surrogate escape:\n%s", got)
	}
	expected.Check(t, "bridge record", []byte(got), expected.Substitute(h.home, "<HOME>"), expected.Substitute(filepath.Dir(h.fake.SocketPath), "<SOCKET-DIR>"))

	// The native crw-bridge.sh the package declares, whose Go launcher fs-encodes the path with
	// the same reading.FSEncode. (The legacy crw_bridge_mcp.py, run by the host's python3, was
	// started here too until todo 44 removed the Python launchers' tests.)
	for _, launcher := range bridgeLaunchers {
		dir, argv := launcher.place(t, h)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		started := exercise.ArgvIn(ctx, dir, argv, scope.Env(homeOnly))
		cancel()
		if started.Err != nil {
			t.Fatalf("%s did not start the bridge: %v\n%s", launcher.name, started.Err, started.Stderr)
		}
		summary := golden.Obj(record.Get(golden.Obj(started.Connection), "executionPolicy"))
		if record.Get(summary, "mode") != "allowlist" || record.Get(summary, "digest") != digest {
			t.Fatalf("%s: the bridge runs without the recorded policy: %s", launcher.name, golden.Canon(summary))
		}
	}
}
