package managed

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func Test27_MST_LockHeldByHelperProcessRefused(t *testing.T) {
	state := t.TempDir()
	storePath := filepath.Join(state, "relay.sqlite3")
	cmd := exec.Command(os.Args[0], "-test.run=^Test27_MST_LockHelper$")
	ready, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "CRW_LOCK_HELPER_STORE="+storePath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	signal := make([]byte, 1)
	if _, err := ready.Read(signal); err != nil || signal[0] != '!' {
		t.Fatalf("helper did not acquire flock: %v %q", err, signal)
	}
	_, err = Lock(storePath, "req-1")
	if err == nil || !strings.Contains(err.Error(), "already being advanced") {
		t.Fatalf("held helper lock accepted: %v", err)
	}
}
func Test27_MST_LockHelper(t *testing.T) {
	storePath := os.Getenv("CRW_LOCK_HELPER_STORE")
	if storePath == "" {
		return
	}
	release, err := Lock(storePath, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	if _, err := os.Stdout.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	_, _ = os.Stdin.Read(signal[:])
}
func Test27_MST_LockSidecarAndOwnedFile(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "relay.sqlite3")
	release, err := Lock(path, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("req-1"))
	sidecar := filepath.Join(state, "managed-start-"+hex.EncodeToString(sum[:])+".lock")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatal(err)
	}
	_, err = Lock(path, "req-1")
	if err == nil || !strings.Contains(err.Error(), "already being advanced") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, err := Lock(path, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = again()
}
