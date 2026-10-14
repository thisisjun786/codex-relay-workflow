package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The fake crw-dev of TestCILocalIntegrationVerifiesTheMergedTree: this test binary, started by a script with labFakeCrwDev set, answers
// `crw-dev ci local --commit H --base B --record R --runner local` the way the real one does for the lab: the sealed record of the commit
// H in its working directory (the merged tree), written to R, and one line of its arguments and directory appended to the log.
const (
	labFakeCrwDev     = "CRW_LAB_FAKE_CRW_DEV"
	labFakeCrwDevLog  = "CRW_LAB_FAKE_CRW_DEV_LOG"
	labFakeCrwDevRepo = "CRW_LAB_FAKE_CRW_DEV_REPO"
)

// fakeCrwDevMain is the fake crw-dev's main: it answers 0 with the record written, or 1 with the reason.
func fakeCrwDevMain() int {
	args := os.Args[1:]
	dir, _ := os.Getwd()
	at, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	files, _ := filepath.Glob(filepath.Join(dir, "node-*.txt"))
	for i, f := range files {
		files[i] = filepath.Base(f)
	}
	line, _ := json.Marshal(map[string]any{"args": args, "dir": dir, "head": strings.TrimSpace(string(at)), "files": files})
	if f, err := os.OpenFile(os.Getenv(labFakeCrwDevLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	if len(args) < 2 || args[0] != "ci" || args[1] != "local" || flag("--commit") == "" || flag("--record") == "" {
		fmt.Fprintf(os.Stderr, "fake crw-dev: not a ci local verification: %q\n", args)
		return 2
	}
	ctx := context.Background()
	head, base := flag("--commit"), flag("--base")
	keys, err := dagsched.CommitVerificationKeys(ctx, dir, head)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake crw-dev:", err)
		return 1
	}
	tree, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", head+"^{tree}").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake crw-dev:", err)
		return 1
	}
	raw, err := dagsched.SealVerificationRecord(dagsched.VerificationRecord{Runner: "local", Repository: os.Getenv(labFakeCrwDevRepo), BaseCommit: base, HeadCommit: head,
		TreeHash: strings.TrimSpace(string(tree)), CiDigest: keys.CiDigest, Tools: keys.Pins, Pins: keys.Pins, PinMismatch: []string{}, Dependencies: keys.Dependencies,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", Jobs: []json.RawMessage{}})
	if err == nil {
		err = os.WriteFile(flag("--record"), raw, 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake crw-dev:", err)
		return 1
	}
	return 0
}

// TestCILocalIntegrationVerifiesTheMergedTree (CRW-1285, the verification gap of CRW-1277): the shipped integration verifier
// CILocalIntegration, which the lab otherwise replaces, verifies each integration batch's merged tree: the configured crw-dev (here a
// fake one, a process of its own started through the shared dagsched.CommandVerifier) is run in the merged tree with `ci local --commit
// <merged head> --base <target tip> --record <the batch's record> --runner local`, its record lands the nodes, and the note of a crw-dev
// whose steps take go from PATH is written. A crw-dev that cannot start is a host error that names the setting.
func TestCILocalIntegrationVerifiesTheMergedTree(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "crw-dev.jsonl")
	crwDev := filepath.Join(t.TempDir(), "crw-dev")
	script := fmt.Sprintf("#!/bin/sh\n%s=1 %s=%q %s=%q exec %q \"$@\"\n", labFakeCrwDev, labFakeCrwDevLog, log, labFakeCrwDevRepo, l.repo, self)
	if err := os.WriteFile(crwDev, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var notes bytes.Buffer
	tools := crwconfig.VerifierTools{CrwDev: crwDev, Config: "/lab/config.json"}
	OpsFor = func(ctx context.Context, req LaneRequest) (LaneWorld, error) {
		return BuildLane(ctx, req, LaneOptions{Verifier: labVerifier{l}, IntegrationVerify: CILocalIntegration(tools, &notes), SkillsDigest: "sha256:lab-skills", Reporter: &labReporter{l: l},
			Relay: func(r *SchedRelay) ops.Relay { return &labRelay{SchedRelay: r, l: l} }})
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	v := l.closed(l.runToEnd(spec.RunID))
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the integration verifier never ran crw-dev: %v", err)
	}
	type call struct {
		Args  []string `json:"args"`
		Dir   string   `json:"dir"`
		Head  string   `json:"head"`
		Files []string `json:"files"`
	}
	var calls []call
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var c call
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	if len(calls) != 2 {
		t.Fatalf("%d verifications of merged trees, want one per node: %s", len(calls), raw)
	}
	for i, c := range calls {
		a := c.Args
		if len(a) != 10 || a[0] != "ci" || a[1] != "local" || a[2] != "--commit" || a[4] != "--base" || a[6] != "--record" || a[8] != "--runner" || a[9] != "local" {
			t.Errorf("call %d: crw-dev %q", i, a)
			continue
		}
		head := a[3]
		if out, err := exec.Command("git", "-C", l.repo, "merge-base", "--is-ancestor", head, "integration").CombinedOutput(); err != nil {
			t.Errorf("call %d verified %s, which did not land: %s", i, head, out)
		}
		// the directory is the merged tree: the commit it verified is checked out there, with the work of the nodes merged so far
		if c.Head != head {
			t.Errorf("call %d ran in %s at %s, not at the merged head %s", i, c.Dir, c.Head, head)
		}
		for _, key := range []string{"A-1", "A-2"}[:i+1] {
			if !slices.Contains(c.Files, "node-"+key+".txt") {
				t.Errorf("call %d: the merged tree %s has no work of %s: %v", i, c.Dir, key, c.Files)
			}
		}
	}
	if calls[1].Args[3] != v.Tip {
		t.Errorf("the last verified head %s is not the tip %s the milestone closed over", calls[1].Args[3], v.Tip)
	}
	if !strings.Contains(notes.String(), "[crw note] integration verification: verifier.go_root is not set in /lab/config.json") {
		t.Errorf("the note of a crw-dev whose steps take go from PATH: %q", notes.String())
	}

	// a crw-dev that cannot start is a host error naming the setting, not a failed verification
	err = CILocalIntegration(crwconfig.VerifierTools{CrwDev: filepath.Join(t.TempDir(), "no-crw-dev"), Config: "/lab/config.json"}, nil)(ctx, l.repo,
		[]string{"CRW_VERIFY_HEAD=h", "CRW_VERIFY_BASE=b", "CRW_VERIFY_RECORD=" + filepath.Join(t.TempDir(), "r.json")})
	var host *dagsched.IntegrationVerifierHostError
	if !errors.As(err, &host) || !strings.Contains(host.Detail, "could not start") || !strings.Contains(host.Detail, "verifier.crw_dev in /lab/config.json") {
		t.Errorf("a crw-dev that cannot start: %v", err)
	}
	// a tree with no cmd/crw-dev to run under a configured Go root is a host error too
	err = CILocalIntegration(crwconfig.VerifierTools{GoRoot: "/opt/go", Config: "/lab/config.json"}, nil)(ctx, t.TempDir(), nil)
	if !errors.As(err, &host) || !strings.Contains(host.Detail, "has no cmd/crw-dev") {
		t.Errorf("a tree with no crw-dev: %v", err)
	}
}
