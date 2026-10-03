package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"golang.org/x/sys/unix"
)

func publishWorker(t *testing.T, state, socket, scope, installationRoot string) {
	t.Helper()
	s, err := openStore(context.Background(), filepath.Join(state, "relay.sqlite3"), socket)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := s.Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scope, 0700); err != nil {
		t.Fatal(err)
	}
	pid := int64(os.Getpid())
	ticks, _, err := processIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(installationRoot + "\x00" + state))
	record := map[string]any{"pid": pid, "startTicks": ticks, "bootId": strings.TrimSpace(string(boot)), "token": "test-worker-token", "installationId": fmt.Sprintf("%x", sum[:8]), "stateDir": state, "socketPath": socket, "storeId": loc.StoreID, "scopeRoot": scope, "scopeAuthority": "isolated"}
	run := map[string]any{}
	for _, key := range []string{"token", "pid", "startTicks", "installationId", "stateDir", "socketPath", "storeId", "scopeRoot", "scopeAuthority"} {
		run[key] = record[key]
	}
	info, err := os.Stat(filepath.Join(state, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	run["dbDevice"] = stat.Dev
	run["dbInode"] = stat.Ino
	receipt := map[string]any{"schemaVersion": 1, "worker": map[string]any{"pid": pid, "startTicks": ticks, "bootId": record["bootId"]}, "service": run, "policy": plain(registry.EnvironmentRolePolicy().Summary()), "observedAt": "2023-11-14T22:13:20.000000+00:00"}
	write := func(path string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256.Sum256([]byte(socket))
	salt := sha256.Sum256([]byte(scope))
	key := fmt.Sprintf("isolated-%x-%x", salt[:4], hash[:8])
	write(filepath.Join(state, "daemon.json"), record)
	write(filepath.Join(state, "worker-policy.json"), receipt)
	write(filepath.Join(scope, key+".json"), record)
	for _, path := range []string{filepath.Join(state, "daemon.lock"), filepath.Join(scope, key+".lock")} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		})
	}
}
