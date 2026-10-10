//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"testing"
)

// CRW-958: a campaign without --out writes below the scratch root the configuration resolves, not
// below a path of one host: the default follows the XDG cache, and a configuration moves it.
func TestDefaultOutRootFollowsTheScratchRoot(t *testing.T) {
	home := t.TempDir()
	env := func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}
	got, err := DefaultOutRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cache", "crw", "scratch", "fuzz"); got != want {
		t.Errorf("DefaultOutRoot = %q, want %q", got, want)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	moved := filepath.Join(t.TempDir(), "moved-scratch")
	if err := os.WriteFile(config, []byte(`{"paths":{"scratch_root":"`+moved+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	withConfig := func(key string) string {
		if key == "CRW_CONFIG" {
			return config
		}
		return env(key)
	}
	got, err = DefaultOutRoot(withConfig)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(moved, "fuzz"); got != want {
		t.Errorf("DefaultOutRoot with scratch_root configured = %q, want %q", got, want)
	}
}
