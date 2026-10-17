package ops

import (
	"fmt"
	"strings"
	"testing"
)

// CRW-1668: only blobs absent from the base's complete history are new.
// Use real Git histories so a deleted tip entry is not mistaken for an absent blob.
func TestScanBlobsExcludesBaseHistory(t *testing.T) {
	oldBlob := strings.Repeat("h", (2<<20)+1)
	newBlob := strings.Repeat("n", (2<<20)+1)
	for _, tc := range []struct {
		name        string
		history     string
		restorePath string
		addNew      bool
		allowNew    bool
		wantPath    string
	}{
		{name: "restore ancestor blob", history: "base", restorePath: "old.bin"},
		{name: "restore under another path", history: "base", restorePath: "renamed.bin"},
		{name: "restore second parent blob", history: "merge", restorePath: "old.bin"},
		{name: "new blob", addNew: true, wantPath: "new.bin"},
		{name: "old blob does not excuse new blob", history: "base", restorePath: "old.bin", addNew: true, wantPath: "new.bin"},
		{name: "unrelated history is not base history", history: "unrelated", restorePath: "old.bin", wantPath: "old.bin"},
		{name: "allowlisted new blob", addNew: true, allowNew: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := newRepo(t)
			commit := func(message string) string {
				t.Helper()
				gitIn(t, repo, "add", "-A")
				gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
				return gitIn(t, repo, "rev-parse", "HEAD")
			}
			if tc.history != "" {
				if tc.history != "base" {
					gitIn(t, repo, "checkout", "-q", "-b", "blob-history")
				}
				write(t, repo, "old.bin", oldBlob, 0o644)
				commit("historical blob")
				switch tc.history {
				case "base":
					gitIn(t, repo, "rm", "--", "old.bin")
					commit("remove blob before base")
				case "merge", "unrelated":
					gitIn(t, repo, "checkout", "-q", "main")
					if tc.history == "merge" {
						gitIn(t, repo, "merge", "--no-ff", "--no-commit", "blob-history")
						gitIn(t, repo, "rm", "-f", "--", "old.bin")
						commit("merge history without its blob in the tree")
					}
				}
			}
			base := gitIn(t, repo, "rev-parse", "HEAD")
			if tc.restorePath != "" {
				write(t, repo, tc.restorePath, oldBlob, 0o644)
			}
			if tc.addNew {
				write(t, repo, "new.bin", newBlob, 0o644)
			}
			if tc.allowNew {
				write(t, repo, ".large-blob-allowlist.json", fmt.Sprintf(`{"entries":[{"path":"new.bin","max_bytes":%d,"reason":"test fixture"}]}`, len(newBlob)), 0o644)
			}
			head := commit("candidate")
			g := &ghVerify{run: (&fakeGH{}).run()}
			err := g.scanBlobs(t.Context(), repo, base, head)
			if tc.wantPath == "" {
				if err != nil {
					t.Fatalf("blob already reachable from base or explicitly allowed was refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "over 2 MiB") || !strings.Contains(err.Error(), tc.wantPath) {
				t.Fatalf("new blob at %q must be refused, got %v", tc.wantPath, err)
			}
		})
	}
}
