package faults

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// FLF scenarios are replayed through fault-sweep. f1ReplayCLI compares the
// complete CLI streams and every table row with the golden.
func flfSeedStart(t *testing.T, ctx context.Context, gd, request, issue, receipt string) {
	t.Helper()
	value := "NULL"
	if receipt != "" {
		value = "'" + receipt + "'"
	}
	f1Seed(t, ctx, gd, []string{"INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,receipt_status,created_at,updated_at) VALUES('" + request + "','" + issue + "','fp','1','/work','/markers','sock','create-" + request + "','dispatch-" + request + "','create_armed',1," + value + ",'stamp','stamp')"})
}

func flfAnswer(t *testing.T, ctx context.Context, gd, request, reason string) {
	t.Helper()
	detail, _ := json.Marshal(map[string]any{"stage": "creation", "state": "incomplete", "reason": reason})
	quoted := strings.ReplaceAll(string(detail), "'", "''")
	f1Seed(t, ctx, gd, []string{"INSERT INTO journal(at,kind,subject,detail) VALUES('stamp','managed_start_observed','" + request + "','" + quoted + "')"})
}

func Test22_FLF_1_CreationAnswersWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	flfSeedStart(t, ctx, gd, "managed-1", "REL-FAILED", "")
	flfAnswer(t, ctx, gd, "managed-1", "creation_failed")
	flfSeedStart(t, ctx, gd, "managed-2", "REL-UNKNOWN", "")
	flfAnswer(t, ctx, gd, "managed-2", "creation_unknown")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-show"})
}

func Test22_FLF_2_CreatingAndPreflightWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	flfSeedStart(t, ctx, gd, "managed-1", "REL-CREATING", "")
	flfSeedStart(t, ctx, gd, "managed-2", "REL-PREFLIGHT", "")
	flfAnswer(t, ctx, gd, "managed-2", "worker_policy_unconfigured")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
}

func Test22_FLF_3_LaterAnswerClearsWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	flfSeedStart(t, ctx, gd, "managed-1", "REL-MANAGED", "")
	flfAnswer(t, ctx, gd, "managed-1", "creation_failed")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	flfAnswer(t, ctx, gd, "managed-1", "worker_policy_unconfigured")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	flfAnswer(t, ctx, gd, "managed-1", "creation_unknown")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
}

func Test22_FLF_4_AnswerFactsWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	for i, status := range []string{"settings_unverified", "failed", "unknown", "partial"} {
		req := "managed-" + string(rune('1'+i))
		flfSeedStart(t, ctx, gd, req, "REL-"+strings.ToUpper(status), status)
	}
	flfSeedStart(t, ctx, gd, "managed-5", "REL-CHILD", "")
	flfAnswer(t, ctx, gd, "managed-5", "creation_failed")
	f1Seed(t, ctx, gd, []string{`UPDATE journal SET detail='{"stage":"creation","state":"incomplete","reason":"creation_failed","retainedChildTaskId":"child-new"}' WHERE subject='managed-5'`})
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
}

func Test22_FLF_5_ClearConditionsWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	flfSeedStart(t, ctx, gd, "managed-1", "REL-MANAGED", "unknown")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-next"})
}

func Test22_FLF_6_CreationAnswerIndexWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	flfSeedStart(t, ctx, gd, "managed-1", "REL-MANAGED", "")
	flfAnswer(t, ctx, gd, "managed-1", "creation_unknown")
	for i := 0; i < 200; i++ {
		flfAnswer(t, ctx, gd, "managed-1", "worker_policy_unconfigured")
	}
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	s, err := store.Open(ctx, filepath.Join(gd, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.All(ctx, "EXPLAIN QUERY PLAN "+managedCreationQuery, "managed-1")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, row := range rows {
		got = append(got, row.Text("detail"))
	}
	checkGolden(t, "query plan", nil, runPathsOf(t, filepath.Dir(gd)), got)
}

func flfInstallRecord(t *testing.T, location string, source map[string]any) {
	t.Helper()
	home := os.Getenv("XDG_STATE_HOME")
	path := filepath.Join(home, "codex-relay-workflow", "host-record.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"recordVersion": 1, "components": map[string]any{"codex-session-relay": map[string]any{"installs": []any{map[string]any{"location": location, "source": source}}}}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func flfLocation(t *testing.T) string {
	t.Helper()
	return relayPackageLocation
}

func Test22_FLF_7_InstalledRevisionWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	flfInstallRecord(t, "/foreign/install", testSource())
	flfSeedStart(t, ctx, gd, "managed-1", "REL-MANAGED", "failed")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
}

func Test22_FLF_8_UnknownAndDirtyRevisionWholeOutput(t *testing.T) {
	goldenParent(t)
	for name, source := range map[string]map[string]any{"incomplete": {"repositoryCommit": strings.Repeat("a", 40)}, "dirty": func() map[string]any { x := testSource(); x["workingTreeClean"] = false; return x }()} {
		t.Run(name, func(t *testing.T) {
			ctx, gd := f1ReplayStores(t)
			flfInstallRecord(t, flfLocation(t), source)
			flfSeedStart(t, ctx, gd, "managed-1", "REL-MANAGED", "failed")
			f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
		})
	}
}

func Test22_FLF_9_ReplacedRecordRereadWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	location := flfLocation(t)
	flfInstallRecord(t, location, testSource())
	flfSeedStart(t, ctx, gd, "managed-1", "REL-ONE", "failed")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	source := testSource()
	source["repositoryCommit"] = strings.Repeat("9", 40)
	flfInstallRecord(t, location, source)
	flfSeedStart(t, ctx, gd, "managed-2", "REL-TWO", "failed")
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
}
