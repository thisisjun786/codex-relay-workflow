package manage

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// upgradeGoodTree is the tree the fake forge reports for upgradeGoodCommit, and the tree the default
// verification record names. upgradeOtherTree is a tree the release does not contain.
const (
	upgradeGoodTree  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	upgradeOtherTree = "cccccccccccccccccccccccccccccccccccccccc"
	upgradeGoodBase  = "dddddddddddddddddddddddddddddddddddddddd"
	// upgradeRecordName is the file the harness writes the record to, inside the test home.
	upgradeRecordName = "verification-record.json"
)

// upgradeSealedRecord is a verification-record/1 whose digest is correct for its members.
func upgradeSealedRecord(t *testing.T, tree, result string) []byte {
	t.Helper()
	raw, err := dagsched.SealVerificationRecord(dagsched.VerificationRecord{
		Runner: "test", Repository: "owner/repo", BaseCommit: upgradeGoodBase, HeadCommit: upgradeGoodCommit,
		TreeHash: tree, CiDigest: "sha256:0", Tools: map[string]string{}, Pins: map[string]string{},
		Dependencies: map[string]string{}, OS: "linux", Arch: "amd64", Result: result,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// upgradeResealed is the good record with some members changed and its digest fixed, as a writer that produced that shape would have sealed it.
func upgradeResealed(t *testing.T, edit func(members map[string]any)) []byte {
	t.Helper()
	var members map[string]any
	if err := json.Unmarshal(upgradeSealedRecord(t, upgradeGoodTree, dagsched.VerificationRecordResultPass), &members); err != nil {
		t.Fatal(err)
	}
	edit(members)
	delete(members, "digest")
	body, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := dagsched.VerificationRecordDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	members["digest"] = digest
	out, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// upgradeRecordFor is the record file a harness gives its run: the explicit bytes when the test set
// them, else a sealed record for the tree and result the options name (pass for upgradeGoodTree).
func upgradeRecordFor(t *testing.T, opts upgradeHarnessOptions) []byte {
	t.Helper()
	if opts.recordRaw != nil {
		return opts.recordRaw
	}
	tree := opts.recordTree
	if tree == "" {
		tree = upgradeGoodTree
	}
	result := opts.recordResult
	if result == "" {
		result = dagsched.VerificationRecordResultPass
	}
	return upgradeSealedRecord(t, tree, result)
}

// upgradeHasFlag reports whether the arguments already name the flag, so the harness adds none.
func upgradeHasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

// upgradeAssertRefusal runs the command, requires the refusal exit and reason, and requires that no
// install or service call followed it.
func upgradeAssertRefusal(t *testing.T, h *upgradeEnv, args []string, reason string) {
	t.Helper()
	if code := h.run(args...); code != upgradeExitRefused {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitRefused, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != reason {
		t.Errorf("reason %q, want %q", got, reason)
	}
	joined := strings.Join(append(h.crwCalls(), h.ghCallLines()...), " ")
	for _, needle := range []string{"install", "service"} {
		if strings.Contains(joined, needle) {
			t.Errorf("a call after the refusal carried %q: %s", needle, joined)
		}
	}
}

// C11: runtime-upgrade installs a commit only when a verification-record/1 for its tree is a PASS. A
// missing record, a record of another tree and a record that is not a PASS each stop the run with its
// own reason before any install or service call.
func TestUpgradeRefusesMissingVerificationRecord(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, noRecord: true})
	upgradeAssertRefusal(t, h, []string{"--release-dir", h.release}, upgradeReasonVerifyMissing)
	t.Run("a path that does not exist", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, noRecord: true})
		absent := filepath.Join(h.home, "absent.json")
		upgradeAssertRefusal(t, h, []string{"--release-dir", h.release, "--verification", absent}, upgradeReasonVerifyMissing)
	})
}

func TestUpgradeRefusesVerificationRecordForAnotherTree(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, recordTree: upgradeOtherTree})
	upgradeAssertRefusal(t, h, []string{"--release-dir", h.release}, upgradeReasonVerifyOtherTree)
}

func TestUpgradeRefusesVerificationRecordThatIsNotPass(t *testing.T) {
	tampered := strings.Replace(string(upgradeSealedRecord(t, upgradeGoodTree, dagsched.VerificationRecordResultPass)),
		upgradeGoodTree, upgradeOtherTree, 1)
	for name, opts := range map[string]upgradeHarnessOptions{
		"a fail result":     {recordResult: "fail"},
		"a malformed file":  {recordRaw: []byte("{not json")},
		"a digest mismatch": {recordRaw: []byte(tampered)},
		// CRW-1026: a record without its pins, or whose pins contradict its tools, is not reusable
		"no pins member": {recordRaw: upgradeResealed(t, func(m map[string]any) { delete(m, "pins") })},
		"pins null":      {recordRaw: upgradeResealed(t, func(m map[string]any) { m["pins"] = nil })},
		"pin against tool": {recordRaw: upgradeResealed(t, func(m map[string]any) {
			m["tools"], m["pins"] = map[string]any{"go": "1.27.1"}, map[string]any{"go": "1.26.0"}
		})},
	} {
		t.Run(name, func(t *testing.T) {
			opts.gh = upgradeGhPaths(upgradeGoodCommit)
			opts.pointer = true
			h := upgradeHarness(t, opts)
			upgradeAssertRefusal(t, h, []string{"--release-dir", h.release}, upgradeReasonVerifyNotPass)
		})
	}
}

// The green case: a PASS record for the installed commit's tree lets the run proceed to install.
func TestUpgradeProceedsOnAPassRecordForTheTree(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
		pointer: true, produceRuntime: true, pointAtIt: true})
	if code := h.run("--release-dir", h.release); code != 0 {
		t.Fatalf("exit %d; the record is %+v", code, h.recordOf(t))
	}
	if !strings.Contains(strings.Join(h.crwCalls(), " "), "install") {
		t.Errorf("a PASS record did not reach the install: %q", h.callLines())
	}
}

// CRW-1026 (d1): the upgrade judges a record's pins against what the release commit declares, read from the forge at that
// commit. A record whose tools and pins agree with each other but differ from the declaration, or that has no pins for a
// tool the commit declares, is refused; a declaration the forge cannot answer refuses too, never waives the pins.
func TestUpgradeJudgesRecordPinsAgainstTheReleaseDeclaration(t *testing.T) {
	declared := map[string]string{
		"go.mod":                   "module example.com/m\n\ngo 1.27\n\ntoolchain go1.27.1\n\nrequire (\n\thonnef.co/go/tools v0.8.1\n)\n",
		"scripts/ci/secrets.sh":    "#!/usr/bin/env bash\nscan_version=8.30.1\n",
		".github/workflows/ci.yml": "jobs:\n  build:\n    steps:\n      - with:\n          node-version: '24.20.0'\n        uses: actions/setup-node@v4\n",
	}
	paths := func() map[string]upgradeGhAnswer {
		gh := upgradeGhPaths(upgradeGoodCommit)
		for file, body := range declared {
			gh[upgradeContentsPath(upgradeGoodCommit, file)] = upgradeGhAnswer{Body: body}
		}
		return gh
	}
	record := func(tools, pins map[string]any) []byte {
		return upgradeResealed(t, func(m map[string]any) { m["tools"], m["pins"] = tools, pins })
	}
	right := map[string]any{"go": "1.27.1", "staticcheck": "0.8.1", "gitleaks": "8.30.1", "node": "24.20.0"}
	wrong := map[string]any{"go": "1.19.0", "staticcheck": "0.8.1", "gitleaks": "8.30.1", "node": "24.20.0"}
	for name, tc := range map[string]struct {
		raw []byte
		gh  func() map[string]upgradeGhAnswer
	}{
		"empty tools and pins":               {raw: upgradeSealedRecord(t, upgradeGoodTree, dagsched.VerificationRecordResultPass), gh: paths},
		"a tool missing from tools and pins": {raw: record(map[string]any{"go": "1.27.1"}, map[string]any{"go": "1.27.1"}), gh: paths},
		"tools and pins agree and differ":    {raw: record(wrong, wrong), gh: paths},
		"a node pin missing":                 {raw: record(map[string]any{"go": "1.27.1", "staticcheck": "0.8.1", "gitleaks": "8.30.1"}, map[string]any{"go": "1.27.1", "staticcheck": "0.8.1", "gitleaks": "8.30.1"}), gh: paths},
		"a declaration the forge cannot answer": {raw: record(right, right), gh: func() map[string]upgradeGhAnswer {
			gh := paths()
			gh[upgradeContentsPath(upgradeGoodCommit, "go.mod")] = upgradeGhAnswer{Stderr: "gh: Bad Gateway (HTTP 502)", Exit: 1}
			return gh
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := upgradeHarness(t, upgradeHarnessOptions{gh: tc.gh(), pointer: true, recordRaw: tc.raw})
			upgradeAssertRefusal(t, h, []string{"--release-dir", h.release}, upgradeReasonVerifyNotPass)
		})
	}
	t.Run("the record that equals the declaration passes", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: paths(), pointer: true, produceRuntime: true, pointAtIt: true, recordRaw: record(right, right)})
		if code := h.run("--release-dir", h.release); code != 0 {
			t.Fatalf("exit %d; the record is %+v", code, h.recordOf(t))
		}
	})
}
