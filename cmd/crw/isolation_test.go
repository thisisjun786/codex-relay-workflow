package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	home, e := os.MkdirTemp("/dev/shm", "crw-home-")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	oldHome := os.Getenv("HOME")
	env := map[string]string{"HOME": home, "XDG_STATE_HOME": home + "/state", "XDG_CONFIG_HOME": home + "/config", "CODEX_HOME": home + "/codex", "CODEX_SESSION_RELAY_STATE": "", "CODEX_SESSION_RELAY_STATE_DIR": ""}
	if os.Getenv("GOPATH") == "" {
		env["GOPATH"] = filepath.Join(oldHome, "go")
	}
	if os.Getenv("GOCACHE") == "" {
		env["GOCACHE"] = filepath.Join(oldHome, ".cache/go-build")
	}
	for k, v := range env {
		if e = os.Setenv(k, v); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}
	code := m.Run()
	if e = os.RemoveAll(home); e != nil {
		fmt.Fprintln(os.Stderr, e)
		code = 1
	}
	os.Exit(code)
}
