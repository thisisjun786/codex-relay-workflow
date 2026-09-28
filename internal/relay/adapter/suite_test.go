package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var suiteDirectory string
var suiteBinary string
var suiteAlias string

// Build before TestMain changes HOME so the toolchain inherits the caller's
// caches and module environment, on both developer workstations and hosted CI.
// Every binary scenario uses this one clock-injected build; production defaults
// are unchanged because ordinary builds do not set the link-time clock seam.
func buildSuiteBinary(root string) error {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		return err
	}
	suiteBinary = filepath.Join(root, "crw")
	suiteAlias = filepath.Join(root, "codex-session-relay")
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return err
	}
	command := exec.Command(goBinary, "build", "-buildvcs=false", "-ldflags=-X github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter.testClock=1700000000", "-o", suiteBinary, "./cmd/crw")
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("build shared crw: %w\n%s", err, output)
	}
	return os.Symlink(suiteBinary, suiteAlias)
}

type deliverySeed struct{ Event, Work string }

var seedOnce sync.Once
var seeded deliverySeed
var seedError error

// Python constructs the relational seed once, retaining its immutable artifact
// path. Each scenario gets independent SQLite copies, never a shared writer.
func copyDeliverySeed(t *testing.T, destination string) deliverySeed {
	t.Helper()
	seedOnce.Do(func() {
		root := filepath.Join(suiteDirectory, "oracle-seed")
		if err := os.Mkdir(root, 0700); err != nil {
			seedError = err
			return
		}
		repo, err := filepath.Abs("../../..")
		if err != nil {
			seedError = err
			return
		}
		command := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/settlement_capture.py"), root, "seed")
		command.Dir = repo
		output, err := command.CombinedOutput()
		if err != nil {
			seedError = fmt.Errorf("Python seed: %w\n%s", err, output)
			return
		}
		if err := json.Unmarshal(output, &seeded); err != nil {
			seedError = err
			return
		}
		seeded.Work = filepath.Join(root, "work")
	})
	if seedError != nil {
		t.Fatal(seedError)
	}
	data, err := os.ReadFile(filepath.Join(suiteDirectory, "oracle-seed", "python.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.sqlite3", "python.sqlite3"} {
		if err := os.WriteFile(filepath.Join(destination, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return seeded
}
