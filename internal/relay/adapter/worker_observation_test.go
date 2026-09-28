package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

func Test28_WorkerObservationMatchesPython(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	scope := filepath.Join(root, "scopes")
	socket := filepath.Join(root, "socket")
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loc, err := s.Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(scope, 0700); err != nil {
		t.Fatal(err)
	}
	observer := WorkerObservation{State: state, Socket: socket, Scope: scope, Authority: "isolated", Installation: root}
	pid := int64(os.Getpid())
	ticks, _, err := processIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(root + "\x00" + state))
	installation := fmt.Sprintf("%x", digest[:8])
	record := map[string]any{"pid": pid, "startTicks": ticks, "bootId": strings.TrimSpace(string(boot)), "token": "test-token", "installationId": installation, "stateDir": state, "socketPath": socket, "storeId": loc.StoreID, "scopeRoot": scope, "scopeAuthority": "isolated"}
	identity := map[string]any{}
	for _, key := range []string{"token", "pid", "startTicks", "installationId", "stateDir", "socketPath", "storeId", "scopeRoot", "scopeAuthority"} {
		identity[key] = record[key]
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	identity["dbDevice"] = stat.Dev
	identity["dbInode"] = stat.Ino
	worker := map[string]any{"pid": pid, "startTicks": ticks, "bootId": record["bootId"]}
	policy := map[string]any{"state": "declared", "digest": "policy-digest", "roles": map[string]any{}, "detail": nil}
	receipt := map[string]any{"schemaVersion": 1, "worker": worker, "service": identity, "policy": policy, "observedAt": "2026-09-27T00:00:00Z"}
	socketHash := sha256.Sum256([]byte(socket))
	scopeHash := sha256.Sum256([]byte(scope))
	key := fmt.Sprintf("isolated-%x-%x", scopeHash[:4], socketHash[:8])
	write := func(path string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(state, "daemon.json"), record)
	write(filepath.Join(scope, key+".json"), record)
	write(filepath.Join(state, "worker-policy.json"), receipt)
	for _, path := range []string{filepath.Join(state, "daemon.lock"), filepath.Join(scope, key+".lock")} {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	check := func() {
		gotPolicy, reason := observer.Read(context.Background())
		var reasonValue any
		if reason != "" {
			reasonValue = reason
		}
		got := map[string]any{"policy": gotPolicy, "reason": reasonValue}
		spec := map[string]any{"state": state, "socket": socket, "scope": scope, "storeId": loc.StoreID, "installationId": installation}
		raw, _ := json.Marshal(spec)
		repo, _ := filepath.Abs("../../..")
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/worker_capture.py"))
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(raw)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("oracle %v %s", err, out)
		}
		var want any
		if err := json.Unmarshal(out, &want); err != nil {
			t.Fatal(err)
		}
		expected, _ := json.Marshal(want)
		actual, _ := json.Marshal(got)
		if !bytes.Equal(actual, expected) {
			t.Fatalf("Go %s Python %s", actual, expected)
		}
	}
	check()
	// Python explicitly treats these files and locks as same-user cooperative evidence. A writer
	// that can replace all of them and hold both locks may publish any policy; Go must not imply a
	// stronger authentication boundary than the oracle does.
	policy["forgedBySameUID"] = true
	write(filepath.Join(state, "worker-policy.json"), receipt)
	check()
	delete(policy, "forgedBySameUID")
	for _, value := range []any{nil, true, "1", 2} {
		receipt["schemaVersion"] = value
		write(filepath.Join(state, "worker-policy.json"), receipt)
		check()
	}
	receipt["schemaVersion"] = 1
	worker["startTicks"] = ticks + 1
	write(filepath.Join(state, "worker-policy.json"), receipt)
	check()
	worker["startTicks"] = ticks
	record["token"] = "changed"
	write(filepath.Join(state, "daemon.json"), record)
	write(filepath.Join(state, "worker-policy.json"), receipt)
	check()
}
