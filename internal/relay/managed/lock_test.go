package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
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

func TestLockProject_StartsShareAndAWriterExcludes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	first, err := LockProject(ctx, path, "P1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LockProject(ctx, path, "P1")
	if err != nil {
		t.Fatalf("a second start of the project waited for the first: %v", err)
	}
	if release, held := holdProject(t, path, "P1"); held {
		release()
		t.Fatal("a writer took the lock while starts held it")
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	if release, held := holdProject(t, path, "P1"); held {
		release()
		t.Fatal("a writer took the lock while a start still held it")
	}
	if err := second(); err != nil {
		t.Fatal(err)
	}
	release, held := holdProject(t, path, "P1")
	if !held {
		t.Fatal("the lock was not released with its holders")
	}
	// Another project's lock is another file.
	other, err := LockProject(ctx, path, "P2")
	if err != nil {
		t.Fatalf("a writer of P1 held up P2: %v", err)
	}
	_ = other()
	release()
}

func TestLockProject_WaitsForAWriterAndEndsWithItsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	release, held := holdProject(t, path, "P1")
	if !held {
		t.Fatal("writer did not get the lock")
	}
	t.Run("granted when the writer lets go", func(t *testing.T) {
		got := make(chan error, 1)
		go func() {
			unlock, err := LockProject(context.Background(), path, "P1")
			if err == nil {
				err = unlock()
			}
			got <- err
		}()
		select {
		case err := <-got:
			t.Fatalf("a start took the lock a writer held: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		release()
		if err := recv(t, got, "the start to get the lock"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ends with the context", func(t *testing.T) {
		release, held := holdProject(t, path, "P1")
		if !held {
			t.Fatal("writer did not get the lock")
		}
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := LockProject(ctx, path, "P1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waited past its context: %v", err)
		}
	})
	t.Run("a cancelled context takes nothing even when the lock is free", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := LockProject(ctx, path, "P-free"); !errors.Is(err, context.Canceled) {
			t.Fatalf("took the lock under a cancelled context: %v", err)
		}
	})
	t.Run("a holder that never lets go ends the wait with LockWaitExpired", func(t *testing.T) {
		release, held := holdProject(t, path, "P1")
		if !held {
			t.Fatal("writer did not get the lock")
		}
		defer release()
		saved := ownership.LockWait
		ownership.LockWait = 200 * time.Millisecond
		defer func() { ownership.LockWait = saved }()
		_, err := LockProject(context.Background(), path, "P1")
		var expired *ownership.LockWaitExpired
		if !errors.As(err, &expired) || expired.What != "the managed-start project lock" {
			t.Fatalf("wait did not end with LockWaitExpired: %v", err)
		}
	})
}

// The sidecar is an owned regular file beside the store, a different file from every request lock, and
// the lock for one project key does not touch a request lock of the same text.
func TestLockProject_SidecarIsOwnedAndApartFromRequestLocks(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "relay.sqlite3")
	unlock, err := LockProject(context.Background(), path, "P1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	sidecar := protocolLockPath(path, "P1")
	if filepath.Dir(sidecar) != state {
		t.Fatalf("sidecar %s is not beside the store", sidecar)
	}
	info, err := os.Lstat(sidecar)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("sidecar: %v %v", info, err)
	}
	request, err := Lock(path, "P1")
	if err != nil {
		t.Fatalf("a request lock of the same text collided with the project lock: %v", err)
	}
	_ = request()
}
