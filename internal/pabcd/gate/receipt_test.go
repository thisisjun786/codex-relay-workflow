package gate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

// The oracle's source-receipt.test.ts, test for test: adversarial, because a receipt is a hand-written file claiming tests passed.
const stateDir = ".crw"

var identity = map[string]any{"kind": "resolved", "commitSha": "abc1234", "dirty": false, "capturedAt": "2026-01-01T00:00:00.000Z"}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// workspace is a directory with an empty evidence root.
func workspace(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(cwd, stateDir, "evidence"), 0o755))
	return cwd
}

// writeFile writes body (a string as it is, anything else as JSON) at path, making its directory.
func writeFile(t *testing.T, path string, body any) {
	t.Helper()
	data, isText := body.(string)
	if !isText {
		encoded, err := json.Marshal(body)
		must(t, err)
		data = string(encoded)
	}
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(data), 0o644))
}

// writeReceipt writes a receipt into the evidence root and returns its path relative to cwd.
func writeReceipt(t *testing.T, cwd, name string, body any) string {
	t.Helper()
	rel := filepath.Join(stateDir, "evidence", name)
	writeFile(t, filepath.Join(cwd, rel), body)
	return rel
}

// refused asserts that the receipt at rel is refused with a ReceiptError that holds want.
func refused(t *testing.T, cwd, rel string, kind ReceiptKind, want string) {
	t.Helper()
	got, err := ParseSourceBoundReceipt(rel, cwd, kind)
	var refusal ReceiptError
	if got != nil || !errors.As(err, &refusal) || !strings.Contains(refusal.Message, want) {
		t.Fatalf("got %+v, %v; want a refusal holding %q", got, err, want)
	}
}

// fixture is a QA receipt over a desktop verdict and its artifact identity (whose sha256 digests are the constants below).
type fixture struct {
	rel, dir string
	receipt  map[string]any
}

func manifestFixture(t *testing.T, cwd string) fixture {
	t.Helper()
	dir := filepath.Join(cwd, stateDir, "evidence", "qa", "D-PACKAGE")
	writeFile(t, filepath.Join(dir, "verdict.json"), `{"scenario":"D-PACKAGE","desktopArtifact":true,"criterionIds":["c-3"],"artifactRefs":["artifact-identity.json"]}`)
	writeFile(t, filepath.Join(dir, "artifact-identity.json"), `{"version":1}`)
	entry := func(kind, digest string) any {
		return map[string]any{"path": "qa/D-PACKAGE/" + kind + ".json", "kind": kind, "sha256": digest, "criterionIds": []any{"c-3"}}
	}
	receipt := map[string]any{"kind": "qa", "sourceIdentity": identity, "createdAt": identity["capturedAt"], "artifactManifest": []any{
		entry("verdict", "5f8c7f5898d582e7f17d1fc2d6137d25629e6694f93ab19031579f90de820ea5"),
		entry("artifact-identity", "2430f1a2ad2982d0067885488a4c89e21ad1d7c83b115ba8f1b20acc88dfaea8")}}
	return fixture{writeReceipt(t, cwd, "qa-receipt.json", receipt), dir, receipt}
}

func (f fixture) entry(i int) map[string]any {
	return f.receipt["artifactManifest"].([]any)[i].(map[string]any)
}

func (f fixture) save(t *testing.T, cwd string) { writeReceipt(t, cwd, "qa-receipt.json", f.receipt) }

func TestValidatedManifestRechecksLinkedVerdictAndIdentityBytes(t *testing.T) {
	cwd := workspace(t)
	f := manifestFixture(t, cwd)
	parsed, err := ParseSourceBoundReceipt(f.rel, cwd, ReceiptQA)
	if err != nil || len(parsed.ArtifactManifest) != 2 || strings.Join(parsed.ArtifactManifest[1].CriterionIDs, ",") != "c-3" {
		t.Fatalf("%+v, %v", parsed, err)
	}
	writeFile(t, filepath.Join(f.dir, "artifact-identity.json"), "changed")
	refused(t, cwd, f.rel, ReceiptQA, "digest does not match")
}

func TestLegacyQAReceiptWithoutManifestRemainsReadable(t *testing.T) {
	cwd := workspace(t)
	rel := writeReceipt(t, cwd, "legacy.json", map[string]any{"kind": "qa", "sourceIdentity": identity, "createdAt": identity["capturedAt"]})
	parsed, err := ParseSourceBoundReceipt(rel, cwd, ReceiptQA)
	if err != nil || parsed.ArtifactManifest != nil {
		t.Fatalf("%+v, %v", parsed, err)
	}
}

func TestManifestRejectsMalformedPathsHashesKindsAndCriterionMetadata(t *testing.T) {
	for _, c := range []struct {
		change func(f fixture)
		want   string // the oracle's refusal, which the test file asserts only as a refusal
	}{
		{func(f fixture) { f.entry(0)["path"] = "../outside/verdict.json" }, "path must be a relative path without .."},
		{func(f fixture) { f.entry(0)["sha256"] = "BAD" }, "sha256 must be a lowercase SHA-256 digest"},
		{func(f fixture) { f.entry(0)["kind"] = "artifact-identity" }, "kind does not match path basename"},
		{func(f fixture) { f.entry(1)["criterionIds"] = []any{"c-4"} }, "criterionIds do not match verdict"},
		{func(f fixture) { f.entry(1)["criterionIds"] = []any{"c-3", "c-3"} }, "criterionIds must be unique c-N IDs"},
		{func(f fixture) { f.entry(1)["path"] = "qa/D-PACKAGE/missing/artifact-identity.json" }, "cannot be read"},
		{func(f fixture) { f.receipt["artifactManifest"] = f.receipt["artifactManifest"].([]any)[:1] }, "desktop verdict qa/D-PACKAGE/verdict.json has no identity"},
		{func(f fixture) { f.receipt["artifactManifest"] = f.receipt["artifactManifest"].([]any)[1:] }, "needs one same-directory verdict"},
	} {
		cwd := workspace(t)
		f := manifestFixture(t, cwd)
		c.change(f)
		f.save(t, cwd)
		refused(t, cwd, f.rel, ReceiptQA, c.want)
	}
}

func TestManifestRefusesADirectSymlinkAndAnEscapingParent(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "artifact-identity.json"), `{"version":1}`)
	cwd := workspace(t)
	f := manifestFixture(t, cwd)
	must(t, os.Remove(filepath.Join(f.dir, "artifact-identity.json")))
	must(t, os.Symlink(filepath.Join(outside, "artifact-identity.json"), filepath.Join(f.dir, "artifact-identity.json")))
	refused(t, cwd, f.rel, ReceiptQA, "symlink")

	cwd = workspace(t)
	f = manifestFixture(t, cwd)
	must(t, os.Symlink(outside, filepath.Join(f.dir, "linked")))
	f.entry(1)["path"] = "qa/D-PACKAGE/linked/artifact-identity.json"
	f.save(t, cwd)
	refused(t, cwd, f.rel, ReceiptQA, "outside the evidence root")
}

func TestAWellFormedTestReceiptParses(t *testing.T) {
	cwd := workspace(t)
	rel := writeReceipt(t, cwd, "t.json", map[string]any{"kind": "test", "sourceIdentity": identity, "command": "npm test", "exitCode": 0, "createdAt": "2026-01-01T00:00:00.000Z"})
	got, err := ParseSourceBoundReceipt(rel, cwd, ReceiptTest)
	if err != nil || got.Kind != ReceiptTest || got.SourceIdentity.CommitSha != "abc1234" || got.Command == nil || *got.Command != "npm test" {
		t.Fatalf("%+v, %v", got, err)
	}
}

// The refusals that need one receipt file and one expectation each. Each name is the oracle test it ports.
func TestReceiptRefusals(t *testing.T) {
	bare := func(extra map[string]any) map[string]any {
		receipt := map[string]any{"sourceIdentity": identity, "createdAt": "x"}
		for k, v := range extra {
			receipt[k] = v
		}
		return receipt
	}
	for _, c := range []struct {
		name  string
		kind  ReceiptKind
		body  any    // written into the evidence root as r.json; nil writes nothing
		claim string // the path asked for, relative to cwd; "" is r.json
		want  string
	}{
		{"a test receipt cannot satisfy the QA slot", ReceiptQA, bare(map[string]any{"kind": "test"}), "", "kind mismatch"},
		{"a QA receipt cannot satisfy the test slot", ReceiptTest, bare(map[string]any{"kind": "qa"}), "", "kind mismatch"},
		{"a zero-byte receipt is rejected by the evidence-root guard", ReceiptTest, "", "", "evidence-root guard"},
		{"malformed JSON is rejected", ReceiptTest, "{not json", "", "not valid JSON"},
		{"a JSON array is not a receipt", ReceiptTest, []int{1, 2}, "", "JSON object"},
		{"a receipt without sourceIdentity is rejected", ReceiptTest, map[string]any{"kind": "test", "createdAt": "x"}, "", "sourceIdentity"},
		{"a malformed sourceIdentity is rejected", ReceiptTest, map[string]any{"kind": "test", "sourceIdentity": map[string]any{"kind": "nope"}, "createdAt": "x"}, "", "sourceIdentity"},
		{"an unknown kind is rejected", ReceiptTest, bare(map[string]any{"kind": "smoke"}), "", `"test" or "qa"`},
		{"a receipt outside the evidence root is rejected", ReceiptTest, nil, "outside.json", "evidence-root guard"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := workspace(t)
			if c.body != nil {
				writeReceipt(t, cwd, "r.json", c.body)
			}
			claim := c.claim
			if claim == "" {
				claim = filepath.Join(stateDir, "evidence", "r.json")
			} else {
				writeFile(t, filepath.Join(cwd, claim), bare(map[string]any{"kind": "test"}))
			}
			refused(t, cwd, claim, c.kind, c.want)
		})
	}
}

func TestAnEmptyPathIsRejected(t *testing.T) {
	refused(t, workspace(t), "", ReceiptTest, "empty")
}

func TestAReceiptReachedThroughALinkedDirectoryInsideTheEvidenceRootIsRejected(t *testing.T) {
	cwd := workspace(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "r.json"), map[string]any{"kind": "test", "sourceIdentity": identity, "createdAt": "x"})
	must(t, os.Symlink(outside, filepath.Join(cwd, stateDir, "evidence", "linked")))
	// Lexically inside the evidence root; the realpath still lands outside it.
	refused(t, cwd, filepath.Join(stateDir, "evidence", "linked", "r.json"), ReceiptTest, "evidence-root guard")
}

func TestASymlinkIntoTheEvidenceRootIsRejected(t *testing.T) {
	cwd := workspace(t)
	real := filepath.Join(cwd, "elsewhere.json")
	writeFile(t, real, map[string]any{"kind": "test", "sourceIdentity": identity, "createdAt": "x"})
	must(t, os.Symlink(real, filepath.Join(cwd, stateDir, "evidence", "link.json")))
	refused(t, cwd, filepath.Join(stateDir, "evidence", "link.json"), ReceiptTest, "evidence-root guard")
}

func TestADirectoryIsNotAReceipt(t *testing.T) {
	cwd := workspace(t)
	rel := filepath.Join(stateDir, "evidence", "dir.json")
	must(t, os.MkdirAll(filepath.Join(cwd, rel), 0o755))
	refused(t, cwd, rel, ReceiptTest, "evidence-root guard")
}
