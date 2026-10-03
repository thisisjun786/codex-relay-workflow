package faults

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	hostrecord "github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The Go installer's entry (internal/runtime/hostrecord.GoInstall) is found by the sweeper that a
// Go relay running from <runtime>/bin/crw builds (Location = filepath.Dir of its resolved
// executable), and its source block is what the sweeper reads as the installed revision. An
// unstamped build's null trees are reported as an incomplete revision naming exactly those
// keys, never as a revision.
func Test37_SweeperReadsTheGoInstallEntry(t *testing.T) {
	home := t.TempDir()
	environment := filepath.Join(t.TempDir(), "bin-0.3.0-aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(environment, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "codex-relay-workflow", hostrecord.Name)
	digest := strings.Repeat("a", 64)
	entry := hostrecord.GoInstall(environment, "codex-session-relay", digest, "crw install", true)
	write := func(entry hostrecord.Object) {
		if err := hostrecord.Save(path, hostrecord.PutInstall(hostrecord.Empty(1), "codex-session-relay", entry)); err != nil {
			t.Fatal(err)
		}
	}
	sw := &Sweeper{HostRecordPath: path, Installation: Installation{Package: "codex-session-relay", Version: "1", Location: filepath.Join(environment, "bin")}}

	unstamped := hostrecord.Set(append(hostrecord.Object{}, entry...), "source", hostrecord.Object{
		{Key: "repositoryCommit", Value: strings.Repeat("1", 40)}, {Key: "repositoryTree", Value: nil},
		{Key: "subdirectoryTree", Value: nil}, {Key: "workingTreeClean", Value: true}})
	write(unstamped)
	if got := sw.installation(); got["revision"] != nil || got["revisionReason"] != "this copy's install entry records an incomplete revision (repositoryTree, subdirectoryTree missing or malformed), which identifies nothing" {
		t.Fatalf("an unstamped Go entry: %+v", got)
	}

	stamped := hostrecord.Set(append(hostrecord.Object{}, entry...), "source", hostrecord.Object{
		{Key: "repositoryCommit", Value: strings.Repeat("1", 40)}, {Key: "repositoryTree", Value: strings.Repeat("2", 40)},
		{Key: "subdirectoryTree", Value: strings.Repeat("2", 40)}, {Key: "workingTreeClean", Value: false}})
	write(stamped)
	got := sw.installation()
	revision, ok := got["revision"].(map[string]any)
	if !ok || revision["environment"] != environment || revision["integrity"] != digest || revision["repositoryTree"] != strings.Repeat("2", 40) || revision["workingTreeClean"] != false {
		t.Fatalf("a stamped Go entry: %+v", got)
	}
}

// A released crw's fault-sweep (backlog before todo 42) names the running binary's own
// installation, as the daemon's sweeper does (decision 34): the fact it records carries the
// directory crw install recorded for this binary and the revision stamped there. The binary is
// the release-shaped one testsupport.CRW gives (-trimpath, as make build and goreleaser build it),
// copied into the installation, where a location derived from the Go source file is a module path
// that names no directory and matches no install entry.
func Test37_FaultSweepCLIRecordsTheRunningBinarysInstallEntry(t *testing.T) {
	root := t.TempDir()
	environment := filepath.Join(root, "bin-0.3.0-aaaaaaaaaaaa")
	crw := testsupport.CRWAt(t, filepath.Join(environment, "bin", "crw"))
	stateHome := filepath.Join(root, "state-home")
	path := filepath.Join(stateHome, "codex-relay-workflow", hostrecord.Name)
	digest := strings.Repeat("a", 64)
	entry := hostrecord.Set(hostrecord.GoInstall(environment, "codex-session-relay", digest, "crw install", true), "source", hostrecord.Object{
		{Key: "repositoryCommit", Value: strings.Repeat("1", 40)}, {Key: "repositoryTree", Value: strings.Repeat("2", 40)},
		{Key: "subdirectoryTree", Value: strings.Repeat("2", 40)}, {Key: "workingTreeClean", Value: true}})
	if err := hostrecord.Save(path, hostrecord.PutInstall(hostrecord.Empty(1), "codex-session-relay", entry)); err != nil {
		t.Fatal(err)
	}
	// A sync write that has failed for good is a source the sweep records, with its facts.
	state := filepath.Join(root, "state")
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{f1Relationship, "INSERT INTO relationship_scope VALUES('rel','CRW','stamp')", "INSERT INTO sync_outbox(sync_id,relationship_id,issue_key,target,target_ref,subject_kind,identity_digest,summary,state,attempts,last_error,created_at,updated_at) VALUES('sync','rel','ISSUE','issue','ISSUE','event','digest','summary','failed',6,'lost','stamp','stamp')"} {
		if _, err := s.Q(ctx).ExecContext(ctx, query); err != nil {
			_ = s.Close()
			t.Fatalf("%s: %v", query, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sweep := exec.Command(crw, "relay", "--state", state, "fault-sweep")
	sweep.Env = append(os.Environ(), "XDG_STATE_HOME="+stateHome)
	if output, err := sweep.CombinedOutput(); err != nil {
		t.Fatalf("fault-sweep: %v\n%s", err, output)
	}
	s, err = store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.All(ctx, "SELECT evidence FROM fault_occurrences")
	if err != nil {
		t.Fatal(err)
	}
	var installations []map[string]any
	for _, row := range rows {
		var items []map[string]any
		if err := json.Unmarshal([]byte(row.Text("evidence")), &items); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item["kind"] == "facts" {
				observed, _ := item["observed"].(map[string]any)
				installation, _ := observed["installation"].(map[string]any)
				installations = append(installations, installation)
			}
		}
	}
	if len(installations) == 0 {
		t.Fatalf("the sweep recorded no facts: %d occurrences", len(rows))
	}
	for _, installation := range installations {
		revision, _ := installation["revision"].(map[string]any)
		if installation["location"] != filepath.Join(environment, "bin") || installation["package"] != "codex-session-relay" || installation["version"] != RelayPackageVersion ||
			revision == nil || revision["environment"] != environment || revision["integrity"] != digest || revision["repositoryTree"] != strings.Repeat("2", 40) || installation["revisionRecord"] != path || installation["revisionReason"] != nil {
			t.Fatalf("the recorded installation: %+v", installation)
		}
	}
}
