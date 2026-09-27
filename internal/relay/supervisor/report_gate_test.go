package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test24_SCH_53_ReportCorrectionGateSentFrozen(t *testing.T) {
	root, _ := pythonSupervisorCapture(t, "WhatTheEighthReviewRoundFound.test_a_report_sent_upward_can_no_longer_be_corrected")
	db := filepath.Join(root, "tree", "state", "relay.sqlite3")
	original, err := os.ReadFile(filepath.Join(root, "event.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(db, original, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), db, "")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	c := &Channel{Store: s, Linkage: StoreLinkage{s}, Program: filepath.Join(repo, ".venv/bin/codex-session-relay")}
	previous := tokenSource
	tokenSource = bytes.NewReader(make([]byte, 128))
	defer func() { tokenSource = previous }()
	_, id := stageSet3(t, c, s)
	c.Settings = &delivery.TaskSettings{}
	h := &captureHost4{sendHost: sendHost{status: "idle"}}
	if _, err = c.Attempt(context.Background(), id, h, 1700000000); err != nil {
		t.Fatal(err)
	}
	event := captureEvent(t, s)
	got := map[string]any{}
	for _, number := range []int64{1, 2} {
		got[fmt.Sprint(number)] = reportRefusalValue(AssertReportResubmission(context.Background(), s, event, number))
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/report_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	capture := t.TempDir()
	copy, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(capture, "relay.sqlite3"), copy, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, "SCH-53-gate", capture, event)
	cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "CODEX_HOME="+home, "TMPDIR="+os.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python gate: %v %s", err, output)
	}
	var want map[string]any
	if err = json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var normalized map[string]any
	if err = json.Unmarshal(raw, &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized, want) {
		t.Errorf("Go=%s Python=%s", raw, jsonText(want))
	}
}
