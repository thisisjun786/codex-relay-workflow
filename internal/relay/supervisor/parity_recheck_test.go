package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func Test24_AutoFaultJournalBytes(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	fault := "<host> & café"
	if err := f.c.deferAutoFault(f.ctx, id, 1700000000, errors.New(fault)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := f.s.DB.QueryRow("SELECT detail FROM journal WHERE kind='supervisor_attempt_faulted'").Scan(&got); err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "defer_after_fault", []byte(got), golden.Substitute(f.root, "<fixture>"), golden.Substitute(repoRoot(t), "<repo>"))
}

func Test24_ObligationHTMLBuiltBinaryBytes(t *testing.T) {
	root := supervisorFixture(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer")
	state := filepath.Join(root, "tree", "state")
	path := filepath.Join(state, "relay.sqlite3")
	snapshot, err := os.ReadFile(filepath.Join(root, "event.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	restoreSnapshot(t, path, snapshot, "go")
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	var event string
	if err := s.DB.QueryRow("SELECT event_id FROM events LIMIT 1").Scan(&event); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE events SET outcome='blocked_needs_input'; UPDATE work_reports SET cxc_status='BLOCKED',cxc_reason='<reason> & café',summary='<summary> & café'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	binary := testsupport.CRW(t)
	alias := filepath.Join(t.TempDir(), "codex-session-relay")
	if err := os.Symlink(binary, alias); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(alias, "--state", state, "supervisor-stage", "--event", event)
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root)
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go: %v %s", err, got)
	}
	var answer struct {
		Message struct {
			At string `json:"staged_at"`
		} `json:"message"`
	}
	if err := json.Unmarshal(got, &answer); err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "supervisor-stage", got, append([]golden.Option{golden.Substitute(alias, "<alias>"), golden.Substitute(answer.Message.At, "<staged_at>")}, treeGolden(t, root)...)...)
}
