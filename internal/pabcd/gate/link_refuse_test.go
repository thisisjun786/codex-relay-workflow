package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest whose evidence root, or whose state directory, is a symbolic link to another directory is refused with a refusal
// that names the root, even when the directory it leads to holds a valid-looking receipt and manifest; the whole receipt is
// refused as well. The same receipt under a real root parses (TestValidatedManifestRechecksLinkedVerdictAndIdentityBytes).
func TestManifestRefusesALinkedEvidenceRoot(t *testing.T) {
	for _, link := range []string{stateDir, filepath.Join(stateDir, "evidence")} {
		t.Run(link, func(t *testing.T) {
			cwd, outside := workspace(t), t.TempDir()
			f := manifestFixture(t, cwd)
			moved := filepath.Join(outside, "moved")
			must(t, os.Rename(filepath.Join(cwd, link), moved))
			must(t, os.Symlink(moved, filepath.Join(cwd, link)))
			root := filepath.Join(cwd, stateDir, "evidence")
			_, err := parseArtifactManifest(f.receipt["artifactManifest"], filepath.Join(cwd, f.rel), cwd)
			if err == nil || !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), filepath.Join(cwd, link)) {
				t.Fatalf("err = %v, want a refusal naming the linked %s under the root %s", err, link, root)
			}
			if got, err := ParseSourceBoundReceipt(f.rel, cwd, ReceiptQA); got != nil || err == nil {
				t.Fatalf("a receipt under a linked root parsed: %+v", got)
			}
		})
	}
}
