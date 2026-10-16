package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1570 D3/D4 (F6): a run's land-head verification (the node's crw.verify, whose crw-dev inherits the run process's environment) and
// its integration batch verification (CILocalIntegration, whose crw-dev gets a fresh TMPDIR of its own) both work below the TMPDIR the
// run's start recorded, whatever TMPDIR the shell that runs the process has, and however long the recorded one is.

// tmpSeeingVerifier is the lab's node verifier that also runs a command the way ops.CILocalVerifier runs crw-dev (ops.ExecCommand with
// the process environment inherited) and keeps the TMPDIR that command saw.
type tmpSeeingVerifier struct {
	labVerifier
	mu   *sync.Mutex
	seen *[]string
}

func (v tmpSeeingVerifier) Verify(ctx context.Context, req ops.VerifyRequest) (ops.VerifyOutcome, error) {
	code, out, err := ops.ExecCommand(ctx, "", []string{}, "sh", "-c", `printf %s "$TMPDIR"`)
	if err != nil || code != 0 {
		return ops.VerifyOutcome{}, errors.Join(err, errors.New(string(out)))
	}
	v.mu.Lock()
	*v.seen = append(*v.seen, string(out))
	v.mu.Unlock()
	return v.labVerifier.Verify(ctx, req)
}

// dirNames are the names in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// longDir makes a directory below a new t.TempDir() whose path is over 100 bytes long.
func longDir(t *testing.T) string {
	t.Helper()
	short := t.TempDir()
	long := filepath.Join(short, strings.Repeat("l", max(1, 101-len(short))))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	if len(long) <= 100 {
		t.Fatalf("%s is %d bytes, not over 100", long, len(long))
	}
	return long
}

// TestLandHeadVerifyAndBatchVerifyShareTheRunsTMPDIR (F6, not parallel: the run's body applies the recorded env to this process): a
// project-lane run started with TMPDIR A runs its process in a shell with another TMPDIR B, or none; the node's crw.verify runs its
// command with TMPDIR = A, the integration batch's crw-dev gets a fresh 0700 directory directly below A (the physical path), removed
// after, the node lands, and nothing is left below B. A short A and one over 100 bytes long give the same outcome.
func TestLandHeadVerifyAndBatchVerifyShareTheRunsTMPDIR(t *testing.T) {
	for _, caller := range []string{"B", "unset"} {
		for _, length := range []string{"short", "over 100 bytes"} {
			t.Run("caller TMPDIR "+caller+", "+length+" A", func(t *testing.T) {
				l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
				a := l.w.tmp
				if length != "short" {
					a = longDir(t)
					l.w.tmp = a
				}
				log := filepath.Join(t.TempDir(), "crw-dev.jsonl")
				tools := crwconfig.VerifierTools{CrwDev: labCrwDevScript(t, log, l.repo), Config: "/lab/config.json"}
				var notes bytes.Buffer
				var mu sync.Mutex
				var seen []string
				OpsFor = func(ctx context.Context, req LaneRequest) (LaneWorld, error) {
					return BuildLane(ctx, req, LaneOptions{Verifier: tmpSeeingVerifier{labVerifier: labVerifier{l}, mu: &mu, seen: &seen}, IntegrationVerify: CILocalIntegration(tools, &notes),
						SkillsDigest: "sha256:lab-skills", Reporter: &labReporter{l: l}, UnitCheck: l.uc.seam(), Crosscheck: l.cc.seam(),
						Relay: func(r *SchedRelay) ops.Relay { return &labRelay{SchedRelay: r, l: l} }})
				}
				l.td.ApplyEnv = applyRunEnv
				spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
				if spec.Env["TMPDIR"] != a {
					t.Fatalf("the start recorded env %v, want TMPDIR %s", spec.Env, a)
				}
				b := t.TempDir()
				t.Setenv("TMPDIR", b)
				if caller == "unset" {
					if err := os.Unsetenv("TMPDIR"); err != nil {
						t.Fatal(err)
					}
				}
				bBefore := dirNames(t, b)
				l.closed(l.runToEnd(spec.RunID))
				if v, ok := os.LookupEnv("TMPDIR"); (caller == "B" && v != b) || (caller == "unset" && ok) {
					t.Errorf("the run process left this process's TMPDIR %q (%v), want the caller's %s", v, ok, caller)
				}
				mu.Lock()
				got := slices.Clone(seen)
				mu.Unlock()
				if len(got) == 0 {
					t.Fatal("the node was never verified")
				}
				for i, v := range got {
					if v != a {
						t.Errorf("verification %d ran its command with TMPDIR %q, want the run's %s", i, v, a)
					}
				}
				raw, err := os.ReadFile(log)
				if err != nil {
					t.Fatalf("the integration verifier never ran crw-dev: %v", err)
				}
				resolved, err := filepath.EvalSymlinks(a)
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
					var c fakeCrwDevCall
					if err := json.Unmarshal([]byte(line), &c); err != nil {
						t.Fatal(err)
					}
					tmp, _ := envValue(c.Env, "TMPDIR")
					if c.TmpPhysical == "" || filepath.Dir(c.TmpPhysical) != resolved || !integrationTmpName.MatchString(filepath.Base(c.TmpPhysical)) || c.TmpMode != "-rwx------" {
						t.Errorf("the batch's crw-dev had TMPDIR %q (physically %q, mode %q), want a fresh 0700 crw-<8 hex> directly below the run's %s", tmp, c.TmpPhysical, c.TmpMode, resolved)
					}
					if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("the batch's TMPDIR %s is still there: %v", tmp, err)
					}
				}
				if after := dirNames(t, b); !slices.Equal(after, bBefore) {
					t.Errorf("the run left %v below the caller's TMPDIR %s (before %v)", after, b, bBefore)
				}
				if strings.Contains(notes.String(), "not below the caller's") {
					t.Errorf("the batch's TMPDIR was moved: %s", notes.String())
				}
			})
		}
	}
}

// TestBatchVerifyStaysBelowTheRecordedDir (not parallel: it applies a run's env to this process): an integration batch verification of a
// run whose recorded TMPDIR was removed after its process started is a host error that names that directory; crw-dev does not run and
// nothing is made elsewhere, below the caller's TMPDIR included.
func TestBatchVerifyStaysBelowTheRecordedDir(t *testing.T) {
	repo, head := labCommitRepo(t)
	log := filepath.Join(t.TempDir(), "crw-dev.jsonl")
	tools := crwconfig.VerifierTools{CrwDev: labCrwDevScript(t, log, repo), Config: "/lab/config.json"}
	a, b := filepath.Join(t.TempDir(), "run-tmp"), t.TempDir()
	if err := os.Mkdir(a, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", b)
	restore, err := applyRunEnv(map[string]string{"TMPDIR": a})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	bBefore := dirNames(t, b)
	record := filepath.Join(t.TempDir(), "record.json")
	var notes bytes.Buffer
	err = CILocalIntegration(tools, &notes)(context.Background(), repo, []string{"CRW_VERIFY_HEAD=" + head, "CRW_VERIFY_BASE=" + head, "CRW_VERIFY_RECORD=" + record})
	var host *dagsched.IntegrationVerifierHostError
	if !errors.As(err, &host) || !strings.Contains(host.Detail, a) {
		t.Errorf("the batch verification below a removed recorded TMPDIR %s: %v, want a host error naming it", a, err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("crw-dev ran: %v", err)
	}
	if after := dirNames(t, b); !slices.Equal(after, bBefore) {
		t.Errorf("the verification made %v below the caller's TMPDIR %s (before %v)", after, b, bBefore)
	}
	if notes.Len() != 0 && strings.Contains(notes.String(), "not below the caller's") {
		t.Errorf("the verification moved its TMPDIR: %s", notes.String())
	}
}
