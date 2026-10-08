package dagsched

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// TestPremergeCLIRecordFileEditedAfterAcceptChangesNothing (CRW-952 c3): the record file is read once, at accept. Editing
// it and then deleting it leaves the stored text and its digest unchanged, which is what every later judgment reads.
func TestPremergeCLIRecordFileEditedAfterAcceptChangesNothing(t *testing.T) {
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	raw := premergeRaw(t, premergeRecordOf(t, k))
	path := filepath.Join(t.TempDir(), "premerge.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m, code := premergeCLIRun(t, k, []byte("@"+path))
	premergeCLIExpect(t, m, code, "")
	digest, err := premerge.Digest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"edited":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+k.fixture.path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored, storedDigest string
	if err := db.QueryRow("SELECT record_json, record_digest FROM dag_acceptance_premerge").Scan(&stored, &storedDigest); err != nil {
		t.Fatal(err)
	}
	if stored != string(raw) || storedDigest != digest {
		t.Fatalf("the stored record changed after the file was edited and removed: digest %s want %s", storedDigest, digest)
	}
}
