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

// CRW-958: the oracle checkout has no place of its own on a host. CRW_CXC_ORACLE_ROOT names it
// directly; otherwise it is below the tools root the configuration resolves, by default below the XDG
// data directory of a clean home, and a configuration moves it. No host path is built in.
func TestOracleRootFollowsTheEnvironmentAndTheToolsRoot(t *testing.T) {
	home := t.TempDir()
	values := map[string]string{"HOME": home}
	env := func(key string) string { return values[key] }
	want := filepath.Join(home, ".local", "share", "crw", "tools", "cxc-v0.2.40", "plugins", "codexclaw", "components")
	if got := OracleRootFrom(env); got != want {
		t.Errorf("clean HOME: OracleRoot = %q, want %q", got, want)
	}
	moved := filepath.Join(t.TempDir(), "tools")
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte(`{"paths":{"tools_root":"`+moved+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	values["CRW_CONFIG"] = config
	if got, want := OracleRootFrom(env), filepath.Join(moved, "cxc-v0.2.40", "plugins", "codexclaw", "components"); got != want {
		t.Errorf("tools_root configured: OracleRoot = %q, want %q", got, want)
	}
	values[OracleRootEnv] = "/explicit/components"
	if got := OracleRootFrom(env); got != "/explicit/components" {
		t.Errorf("explicit input: OracleRoot = %q", got)
	}
	// a target built from the process environment takes the same root
	t.Setenv(OracleRootEnv, "/from/environment")
	if got := echoTarget().Oracle.Root; got != "/from/environment" {
		t.Errorf("echo target root = %q", got)
	}
}
