package install_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/exercise"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// pythonBridgeRecord is the record scripts/crw_runtime/bridgerecord.document builds for these
// fields and a policy at the raw path policy (os.fsdecode of its bytes), as json.dumps(indent=2,
// sort_keys=True) writes it. Python's answer is recorded (pyoracle), with each run-specific
// directory in dirs spelled as its placeholder.
func pythonBridgeRecord(t *testing.T, fields record.Object, policy, digest string, dirs ...pyoracle.Option) string {
	t.Helper()
	given, err := json.Marshal(map[string]any{"command": record.Get(fields, "bridgeExecutable"), "args": record.Get(fields, "args"),
		"name": record.Get(fields, "serverName"), "issue": record.Get(fields, "installedBy"), "pathHex": hex.EncodeToString([]byte(policy)), "digest": digest})
	if err != nil {
		t.Fatal(err)
	}
	script := `import json, os, sys
sys.path.insert(0, sys.argv[1])
from crw_runtime import bridgerecord
given = json.load(sys.stdin)
document = bridgerecord.document(command=given["command"], arguments=given["args"], name=given["name"], issue=given["issue"],
    execution_policy={"path": os.fsdecode(bytes.fromhex(given["pathHex"])), "digest": given["digest"]})
sys.stdout.write(json.dumps(document, indent=2, sort_keys=True) + "\n")
`
	out := pyoracle.Answer(t, "bridgerecord.document", func() ([]byte, error) {
		cmd := exec.Command(hostPython(), "-c", script, filepath.Join(golden.Root(), "scripts"))
		cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		cmd.Stdin = bytes.NewReader(given)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("bridgerecord.document: %v\n%s", err, stderr.String())
		}
		return out, nil
	}, dirs...)
	return string(out)
}

// A policy file whose name holds a byte that is not UTF-8 is recorded as runtime_install.py's
// bridgerecord records it - the path as os.fsdecode spells it, the byte as its surrogate escape
// written "\udc80", and the digest over the file itself - byte for byte the record
// bridgerecord.document and json.dumps write for the same input. Both packaged launchers, which
// fs-encode the path back, then start the Go bridge under that policy.
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
	decoded, err := reading.Decode([]byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if want := pythonBridgeRecord(t, decoded.(record.Object), policy, digest,
		pyoracle.Substitute(h.home, "<HOME>"), pyoracle.Substitute(filepath.Dir(h.fake.SocketPath), "<SOCKET-DIR>")); got != want {
		t.Fatalf("record:\n%s\nPython's bridgerecord:\n%s", got, want)
	}

	// Both launchers: the native crw-bridge.sh the package declares, whose Go launcher
	// fs-encodes the path with the same reading.FSEncode, and the legacy crw_bridge_mcp.py where
	// this host has a python3 to run it (runnablePython3).
	for _, launcher := range bridgeLaunchers {
		if launcher.python && runnablePython3() == "" {
			t.Logf("%s: not started, no python3 that runs on PATH", launcher.name)
			continue
		}
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
