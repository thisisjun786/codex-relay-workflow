package ops

import (
	"strings"
	"testing"
)

// CRW-1579 (U-A5-8): Snapshot makes its private index directory through tmploc, which refuses a
// relative TMPDIR. os.MkdirTemp("") would have made the directory below the working directory.
func TestTmplocMkdirTemp_relativeTMPDIRIsRefused_atAProductSite(t *testing.T) {
	repo, _ := newRepo(t)
	t.Setenv("TMPDIR", "relative/tmp")
	_, err := Snapshot(ctx, ExecGit("git"), repo)
	if err == nil {
		t.Fatalf("Snapshot with TMPDIR=relative/tmp succeeded; want the tmploc refusal")
	}
	if !strings.Contains(err.Error(), "tmpdir_not_absolute") {
		t.Fatalf("Snapshot error = %v; want tmpdir_not_absolute", err)
	}
}
