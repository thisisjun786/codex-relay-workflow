//go:build !windows

package role

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fifoConfig makes CODEX_HOME/config.toml a FIFO with no writer and returns its home. A blocking open
// of it never returns, so the cleanup opens it for writing to let a reader that is stuck there go.
func fifoConfig(t *testing.T, o CatalogOptions) {
	t.Helper()
	codex, _ := catalogEnv(o.Environ)("CODEX_HOME")
	check(t, os.MkdirAll(codex, 0700))
	config := filepath.Join(codex, "config.toml")
	check(t, syscall.Mkfifo(config, 0600))
	t.Cleanup(func() {
		if f, err := os.OpenFile(config, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})
}

// within runs f, which returns what is wrong with its answer (or ""), and fails the test when it has not
// returned after the limit. f never touches t: it may outlive the test when it waits.
func within(t *testing.T, what string, f func() string) {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- f() }()
	select {
	case bad := <-done:
		if bad != "" {
			t.Errorf("%s: %s", what, bad)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: it waits on the native configuration file", what)
	}
}

// CRW-1132 and CRW-1120: a working OCX answers whatever the native Codex configuration file is. The
// file is read only to choose the native catalog, so a configuration that is not a regular file (a
// FIFO nobody writes, or a link to one) must not make the OCX run, an OCX cache hit or a catalog
// request wait.
func TestLiveCatalogOcxDoesNotWaitOnTheNativeConfigFile(t *testing.T) {
	t.Run("RunOcxModels", func(t *testing.T) {
		o := liveOptions(t)
		root, _ := catalogEnv(o.Environ)("HOME")
		fakeOcx(t, filepath.Join(root, "bin"), "ocx-model")
		fifoConfig(t, o)
		within(t, "RunOcxModels", func() string {
			if out, err := RunOcxModels(o.Environ); err != nil || out == "" {
				return fmt.Sprintf("answered %q, %v", out, err)
			}
			return ""
		})
	})
	t.Run("ReadCatalog-and-cache-hit", func(t *testing.T) {
		o := liveOptions(t)
		root, _ := catalogEnv(o.Environ)("HOME")
		fakeOcx(t, filepath.Join(root, "bin"), "ocx-model")
		fifoConfig(t, o)
		var r CatalogReader
		for i, want := range []string{"fresh discovery", "cache hit"} {
			within(t, want, func() string {
				got, err := r.ReadCatalog(o)
				if err != nil || got.Status != "fresh" || got.Source != ModelOcx || len(got.Entries) != 1 || got.Entries[0].ID != "ocx-model" {
					return fmt.Sprintf("read %d answered %+v, %v", i, got, err)
				}
				return ""
			})
		}
	})
	t.Run("link-to-a-FIFO", func(t *testing.T) {
		o := liveOptions(t)
		root, _ := catalogEnv(o.Environ)("HOME")
		fakeOcx(t, filepath.Join(root, "bin"), "ocx-model")
		codex, _ := catalogEnv(o.Environ)("CODEX_HOME")
		check(t, os.MkdirAll(codex, 0700))
		fifo := filepath.Join(root, "fifo")
		check(t, syscall.Mkfifo(fifo, 0600))
		check(t, os.Symlink(fifo, filepath.Join(codex, "config.toml")))
		t.Cleanup(func() {
			if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
				f.Close()
			}
		})
		var r CatalogReader
		within(t, "ReadCatalog", func() string {
			if got, err := r.ReadCatalog(o); err != nil || got.Status != "fresh" || got.Source != ModelOcx {
				return fmt.Sprintf("answered %+v, %v", got, err)
			}
			return ""
		})
	})
	t.Run("native-fallback-and-selection", func(t *testing.T) {
		// Without a working OCX the configuration decides the native catalog. A configuration that is not
		// a regular file selects nothing: the answer is the unavailable one, not a wait.
		o := liveOptions(t)
		fifoConfig(t, o)
		var r CatalogReader
		within(t, "ReadCatalog", func() string {
			if got, err := r.ReadCatalog(o); err != nil || got.Status != "unavailable" || got.Source != ModelNative {
				return fmt.Sprintf("answered %+v, %v", got, err)
			}
			return ""
		})
		within(t, "NativeCatalogPath", func() string {
			if p := NativeCatalogPath(catalogEnv(o.Environ)); p != "" {
				return fmt.Sprintf("a FIFO configuration answered %q", p)
			}
			return ""
		})
	})
}
