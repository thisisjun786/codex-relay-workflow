package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
)

// lastIntegrateRunner is the runner the last integrateEnvRun built.
var lastIntegrateRunner *integrator.Runner

// integrateEnvRun runs one integrate pass over an empty queue with the configuration document doc
// and returns the environment the runner was built with.
func integrateEnvRun(t *testing.T, dir, doc string) []string {
	t.Helper()
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", config)
	var built *integrator.Runner
	previous := integrateRunner
	integrateRunner = func(cfg integrator.Config) *integrator.Runner {
		built = previous(cfg)
		return built
	}
	t.Cleanup(func() { integrateRunner = previous })
	var stderr bytes.Buffer
	if code := Run(context.Background(), []string{"integrate"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("integrate = %d\n%s", code, stderr.String())
	}
	if built == nil {
		t.Fatal("no runner was built")
	}
	lastIntegrateRunner = built
	return built.Env
}

// CRW-958 (merge with the integrator lane): the record-version and verification commands the
// integration queue starts run the go tool, so they get the configured tool roots and the
// configuration file; with nothing configured the runner keeps the process environment.
func TestStorageIntegrateRunnerCarriesTheToolRoots(t *testing.T) {
	var section map[string]any
	if err := json.Unmarshal([]byte(integrateTestSection(t.TempDir())), &section); err != nil {
		t.Fatal(err)
	}
	t.Run("configured", func(t *testing.T) {
		home, _, _ := coreTempHome(t)
		dir := t.TempDir()
		json.Unmarshal([]byte(integrateTestSection(dir)), &section)
		want := map[string]string{
			"GOCACHE":    filepath.Join(home, "disk", "bc"),
			"GOMODCACHE": filepath.Join(home, "disk", "mc"),
			"GOTMPDIR":   filepath.Join(home, "disk", "work"),
		}
		doc := storageDoc(t, map[string]string{"build_cache": want["GOCACHE"], "module_cache": want["GOMODCACHE"], "build_work": want["GOTMPDIR"]},
			map[string]any{"integrate": section})
		env := integrateEnvRun(t, dir, doc)
		got := storageEnvLines(strings.Join(env, "\n"))
		storageCheckEnv(t, "integrate runner", got, want)
		if got["CRW_CONFIG"] != filepath.Join(dir, "config.json") {
			t.Errorf("CRW_CONFIG = %q, want the configuration file", got["CRW_CONFIG"])
		}
		// Building the environment makes no directory (integrate --dry-run changes nothing); the
		// runner's Prepare seam makes build_work right before a tool command starts.
		if _, err := os.Stat(want["GOTMPDIR"]); !os.IsNotExist(err) {
			t.Errorf("build_work exists before any tool command is about to start: %v", err)
		}
		if lastIntegrateRunner == nil || lastIntegrateRunner.Prepare == nil {
			t.Fatal("the runner has no Prepare seam")
		}
		if err := lastIntegrateRunner.Prepare(); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(want["GOTMPDIR"]); err != nil || !info.IsDir() {
			t.Errorf("build_work was not made by Prepare: %v", err)
		}
	})
	t.Run("nothing configured", func(t *testing.T) {
		coreTempHome(t)
		dir := t.TempDir()
		json.Unmarshal([]byte(integrateTestSection(dir)), &section)
		doc := storageDoc(t, map[string]string{}, map[string]any{"integrate": section})
		if env := integrateEnvRun(t, dir, doc); env != nil {
			for _, line := range env {
				if strings.HasPrefix(line, "GOCACHE=") || strings.HasPrefix(line, "GOMODCACHE=") || strings.HasPrefix(line, "GOTMPDIR=") {
					if os.Getenv(strings.SplitN(line, "=", 2)[0]) != strings.SplitN(line, "=", 2)[1] {
						t.Errorf("an unset root put %q into the environment", line)
					}
				}
			}
		}
	})
}
