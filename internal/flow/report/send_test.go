package report

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The report was written in the relay the operator selected; the notice for its recipient names that relay, so send-parent reads the
// binding's authorization there and the pump tells the flow rows of that store. A parent notice reads no relay and carries no option for
// one. (post-evaluation d6)
func TestManageSenderNamesTheSelectedRelayOnASupervisorNotice(t *testing.T) {
	var argv [][]string
	m := ManageSender{Program: "crw", State: "/relay/selected", Socket: "/sock/selected", Dir: t.TempDir(),
		Run: func(_ context.Context, _ string, args ...string) ([]byte, int, error) {
			argv = append(argv, args)
			return []byte(`{"class":"accepted","requestId":"r1"}`), 0, nil
		}}
	if _, err := m.SendSupervisor(context.Background(), SupervisorSend{Thread: "lead-1", LogicalID: "run-1:M1:report", Text: "t", ReportID: "rep_1", Initiative: "INI-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SendParent(context.Background(), ParentSend{Thread: "orch", LogicalID: "esc-1", Text: "t", Queue: true}); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 {
		t.Fatalf("argv %v", argv)
	}
	join := strings.Join(argv[0], " ")
	if !strings.Contains(join, "--role supervisor") || !strings.Contains(join, "--state /relay/selected") || !strings.Contains(join, "--socket /sock/selected") {
		t.Errorf("the supervisor notice was sent as %q", join)
	}
	if strings.Contains(strings.Join(argv[1], " "), "--state") || strings.Contains(strings.Join(argv[1], " "), "--socket") {
		t.Errorf("the parent notice names a relay: %v", argv[1])
	}
	// without a selection the options are absent and the command reads the configuration's relay as it did
	argv = nil
	plain := ManageSender{Program: "crw", Dir: t.TempDir(), Run: m.Run}
	if _, err := plain.SendSupervisor(context.Background(), SupervisorSend{Thread: "lead-1", LogicalID: "x", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv[0], " "), "--state") || strings.Contains(strings.Join(argv[0], " "), "--socket") {
		t.Errorf("argv %v", argv[0])
	}
}

// Without a Dir the message file is made below the one temp function's directory (CRW-1570 U5): with TMPDIR a directory that is not
// there, the send refuses with tmploc's reason, naming the directory, and starts no command.
func TestManageSenderRefusesAMissingTMPDIRWithTheTempFunctionsReason(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("TMPDIR", missing)
	ran := false
	m := ManageSender{Program: "crw", Run: func(context.Context, string, ...string) ([]byte, int, error) {
		ran = true
		return []byte(`{"class":"accepted","requestId":"r1"}`), 0, nil
	}}
	_, err := m.SendParent(context.Background(), ParentSend{Thread: "orch", LogicalID: "esc-1", Text: "t", Queue: true})
	if err == nil || !strings.Contains(err.Error(), "temp_dir_unusable") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("SendParent with TMPDIR=%s: %v, want tmploc's temp_dir_unusable naming the directory", missing, err)
	}
	if ran {
		t.Error("the command was started without a message file")
	}
}
