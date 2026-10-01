//go:build linux

package adapter

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func Test28_MSC_5_StableReadAndMutations(t *testing.T) {
	shareGoldens(t)
	for _, kind := range []string{"quiet", "rename", "write", "mapped"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "quiet.txt")
			if err := os.WriteFile(file, []byte("original content"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "quiet" {
				hashCapture(t, file, []string{root}, false)
				return
			}
			h, err := OpenAuthorized(file, []string{root}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			var mutate func() error
			switch kind {
			case "rename":
				mutate = func() error { return os.Rename(file, filepath.Join(root, "moved.txt")) }
			case "write":
				mutate = func() error { return os.WriteFile(file, []byte("different content entirely"), 0600) }
			case "mapped":
				writable, err := os.OpenFile(file, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer writable.Close()
				mapping, err := unix.Mmap(int(writable.Fd()), 0, len("original content"), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
				if err != nil {
					t.Fatal(err)
				}
				defer unix.Munmap(mapping)
				mutate = func() error {
					mapping[0] = 'B'
					if err := unix.Msync(mapping, unix.MS_SYNC); err != nil {
						return err
					}
					return h.Rebaseline()
				}
			}
			_, _, err = HashAuthorized(h, mutate)
			var refusal *ScopeError
			if !errors.As(err, &refusal) {
				t.Fatalf("mutation accepted: %v", err)
			}
			expected := "artifact_mutated_during_read"
			if kind == "rename" {
				expected = "path_relocated"
			}
			if refusal.Reason != expected {
				t.Fatal(err)
			}
			if kind == "rename" {
				if err := os.Rename(filepath.Join(root, "moved.txt"), file); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(file, []byte("original content"), 0600); err != nil {
				t.Fatal(err)
			}
			scopeCapture(t, map[string]any{"op": "mutation", "kind": kind, "path": file, "roots": []string{root}, "moved": filepath.Join(root, "moved.txt")}, map[string]any{"error": refusal.Error()})
		})
	}
}
func Test28_MSC_6_ReadLeaseAndRealWriterBreak(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "lease.txt")
	if err := os.WriteFile(file, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	hashCapture(t, file, []string{root}, false)
	writable, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	hashCapture(t, file, []string{root}, true)
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	hashCapture(t, file, []string{root}, true)
	h, err := OpenAuthorized(file, []string{root}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	if h.Binding.Mode != LeaseEnforced {
		t.Fatalf("filesystem did not grant lease: %s", h.Binding.Detail)
	}
	// Subscribe before the writer opens; O_NONBLOCK asks the kernel to request a
	// break without parking the test behind the kernel's lease-break timeout.
	broken := make(chan os.Signal, 1)
	signal.Notify(broken, syscall.SIGIO)
	defer signal.Stop(broken)
	writer := make(chan error, 1)
	go func() {
		fd, err := unix.Open(file, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			err = unix.Close(fd)
		}
		writer <- err
	}()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	select {
	case <-broken:
	case <-timeout.C:
		t.Fatal("kernel did not report lease break")
	}
	select {
	case err := <-writer:
		if !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatalf("writer open: %v", err)
		}
	case <-timeout.C:
		t.Fatal("writer did not return")
	}
	var refusal *ScopeError
	if err := h.VerifyStable(); !errors.As(err, &refusal) || refusal.Reason != "artifact_lease_broken" {
		t.Fatalf("lease break: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	scopeCapture(t, map[string]any{"op": "leaseBreak", "path": file, "roots": []string{root}}, map[string]any{"error": refusal.Error()})
	if !AtLeast(LeaseEnforced, BestEffortDetection) || AtLeast(BestEffortDetection, LeaseEnforced) {
		t.Fatal("tier ordering")
	}
}
