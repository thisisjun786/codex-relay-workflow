package manage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// CRW-1049 pins the named temporary file path of improve collect --out, the form the bundle is written in
// when the unnamed form is unavailable: every step that can fail after the file exists, the errno that must not
// fall back, a platform without the unnamed form, and improve run reaching the same path.

// improveNamedStepSeam makes one step of the temporary file write fail with err (an empty step fails none) and
// records every step the write reached, in order.
func improveNamedStepSeam(t *testing.T, step string, err error) *[]string {
	t.Helper()
	seen := new([]string)
	previous := improveTemporaryStep
	improveTemporaryStep = func(name string) error {
		*seen = append(*seen, name)
		if name == step {
			return err
		}
		return nil
	}
	t.Cleanup(func() { improveTemporaryStep = previous })
	return seen
}

// improveNamedRun runs improve collect --out against a fresh state, with the unnamed form refused as the kernel
// would, and answers the exit status, stderr, the output file and the output directory.
func improveNamedRun(t *testing.T, refuse error) (code int, stderr, out, outDir string, calls *int) {
	t.Helper()
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	improveInputRelayConfig(t, s)
	outDir = improveReview799OutDir(t, s)
	out = filepath.Join(outDir, "bundle.json")
	calls = improveForceUnnamed(t, refuse)
	code, _, stderr = improveTestRun(t, s, "--out", out)
	return code, stderr, out, outDir, calls
}

// C1.1: a write, sync, chmod, close or rename that fails on the named temporary file is a refusal that names its
// cause, writes no destination and leaves no improve-bundle-* file in the output directory.
func TestImproveNamedTemporaryEveryStepFailureLeavesNothing(t *testing.T) {
	for _, step := range []string{"write", "sync", "chmod", "close", "rename"} {
		t.Run(step, func(t *testing.T) {
			cause := errors.New("simulated " + step + " failure")
			seen := improveNamedStepSeam(t, step, cause)
			code, stderr, out, outDir, calls := improveNamedRun(t, unix.EOPNOTSUPP)
			if *calls == 0 || !slices.Contains(*seen, step) {
				t.Fatalf("the named path was not taken: unnamed attempts %d, steps %v", *calls, *seen)
			}
			if code == 0 || !strings.Contains(stderr, cause.Error()) {
				t.Fatalf("a failed %s: exit %d, stderr %q, want a refusal naming %q", step, code, stderr, cause)
			}
			if _, err := os.Stat(out); err == nil {
				t.Errorf("a failed %s wrote the destination", step)
			}
			if left := improveTestTemporaryNames(t, outDir); len(left) != 0 {
				t.Errorf("a failed %s left %v", step, left)
			}
		})
	}
}

// C1.1 control: the steps are reached in order and, with none failing, the named file becomes the bundle.
func TestImproveNamedTemporaryStepsRunInOrderWhenNoneFails(t *testing.T) {
	seen := improveNamedStepSeam(t, "", nil)
	code, stderr, out, outDir, _ := improveNamedRun(t, unix.EOPNOTSUPP)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if want := []string{"write", "sync", "chmod", "close", "rename"}; !slices.Equal(*seen, want) {
		t.Errorf("the steps were %v, want %v", *seen, want)
	}
	if bundle := improveTestReadBundle(t, out); bundle.Schema != improveBundleSchema {
		t.Errorf("the named file did not become the bundle: %+v", bundle)
	}
	if left := improveTestTemporaryNames(t, outDir); len(left) != 0 {
		t.Errorf("the run left %v", left)
	}
}

// C1.2: an unnamed creation that fails with EACCES would also stop a named file, so it is refused and not retried
// as a named one; ENOSPC is the contrast the existing test holds.
func TestImproveUnnamedTemporaryPermissionDeniedIsNotFallenBack(t *testing.T) {
	for _, errno := range []unix.Errno{unix.EACCES, unix.ENOSPC} {
		t.Run(errno.Error(), func(t *testing.T) {
			seen := improveNamedStepSeam(t, "", nil)
			code, stderr, out, outDir, calls := improveNamedRun(t, errno)
			if code == 0 {
				t.Fatalf("a creation refused with %v succeeded: stderr %q", errno, stderr)
			}
			if *calls != 1 || len(*seen) != 0 {
				t.Errorf("the run tried the unnamed form %d times and reached the named steps %v, want one attempt and none", *calls, *seen)
			}
			if _, err := os.Stat(out); err == nil {
				t.Errorf("the refused run wrote the destination")
			}
			if left := improveTestTemporaryNames(t, outDir); len(left) != 0 {
				t.Errorf("the refused run left %v", left)
			}
		})
	}
}

// C1.3: the answer of improveCreateTemporary on a platform without the unnamed form is the unsupported
// error, and collect --out then succeeds with the named form.
func TestImproveUnnamedTemporaryPlatformAnswerWritesTheNamedForm(t *testing.T) {
	platform := fmt.Errorf("%w: the unnamed temporary file is unavailable on this platform", errImproveNoUnnamedName)
	if !improveUnnamedUnsupported(platform) {
		t.Fatal("the platform answer is not recognised as the unsupported form")
	}
	seen := improveNamedStepSeam(t, "", nil)
	code, stderr, out, outDir, calls := improveNamedRun(t, platform)
	if code != 0 {
		t.Fatalf("collect --out on a platform without the unnamed form: exit %d, stderr %q", code, stderr)
	}
	if *calls == 0 || !slices.Contains(*seen, "rename") {
		t.Errorf("the named form was not used: unnamed attempts %d, steps %v", *calls, *seen)
	}
	if bundle := improveTestReadBundle(t, out); bundle.Schema != improveBundleSchema {
		t.Errorf("the bundle is not written: %+v", bundle)
	}
	if left := improveTestTemporaryNames(t, outDir); len(left) != 0 {
		t.Errorf("the run left %v", left)
	}
}

// C1.4: improve run calls the same collect path, so with the unnamed form refused it still leaves its bundle,
// drafts and roadmap, and the bundle was written through the named steps.
func TestImproveRunSucceedsWithTheUnnamedFormRefused(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveRoadmapTestSeedRepeatedFriction(t, db) })
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	calls := improveForceUnnamed(t, unix.EOPNOTSUPP)
	seen := improveNamedStepSeam(t, "", nil)
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	code, out, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2")
	if code != 0 {
		t.Fatalf("improve run with the unnamed form refused: exit %d, stderr %s", code, errOut)
	}
	if *calls != 1 || !slices.Contains(*seen, "rename") {
		t.Errorf("improve run did not write its bundle through the named path: unnamed attempts %d, steps %v", *calls, *seen)
	}
	if path := strings.TrimSpace(out); !strings.HasPrefix(filepath.Base(path), "roadmap-") {
		t.Errorf("the run printed %q, want a roadmap path", out)
	}
	if got := improveRoadmapTestBundles(t, manageState, "M2"); len(got) != 1 {
		t.Errorf("the ref directory holds %v, want one bundle", got)
	}
	if left := improveTestTemporaryNames(t, filepath.Join(manageState, "improve", "M2")); len(left) != 0 {
		t.Errorf("the run left %v", left)
	}
}
