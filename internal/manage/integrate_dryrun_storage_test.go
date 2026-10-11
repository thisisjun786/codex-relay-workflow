package manage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dev side promises --dry-run creates no directories. A configured tool
// work root must not be prepared when no gate/build command is going to run.
func TestMergeVerifyIntegrateDryRunDoesNotCreateBuildWork(t *testing.T) {
	home, _, _ := coreTempHome(t)
	dir := t.TempDir()
	var section map[string]any
	if err := json.Unmarshal([]byte(integrateTestSection(dir)), &section); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "new-disk", "build-work")
	doc := storageDoc(t, map[string]string{"build_work": work}, map[string]any{"integrate": section})
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", path)
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("before dry-run: %v", err)
	}
	var out, errOut bytes.Buffer
	code := Run(t.Context(), []string{"integrate", "--dry-run"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("dry-run exit %d stdout %s stderr %s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("dry-run created configured build_work %s (stat error %v)", work, err)
	}
}

// CRW-958: a real pass makes the configured build_work right before the gate, record-version and
// verification commands start, so the go tool finds GOTMPDIR; an idle pass starts no tool and makes
// nothing.
func TestIntegrateRunnerPreparesBuildWorkOnlyForARealRun(t *testing.T) {
	home, _, _ := coreTempHome(t)
	dir := t.TempDir()
	var section map[string]any
	if err := json.Unmarshal([]byte(integrateTestSection(dir)), &section); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "new-disk", "build-work")
	doc := storageDoc(t, map[string]string{"build_work": work}, map[string]any{"integrate": section})
	integrateEnvRun(t, dir, doc) // an idle pass over an empty queue
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("an idle pass made build_work: %v", err)
	}
	if err := lastIntegrateRunner.Prepare(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Fatalf("Prepare did not make build_work: %v", err)
	}
}
