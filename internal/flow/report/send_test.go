package report

import (
	"context"
	"os"
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

// CRW-1579 (U-A5-4): the message file's default directory is tmploc's temporary directory. A relative
// TMPDIR is tmploc's refusal: nothing is sent and nothing is made below the working directory, where before
// the file was made relative to it.
func TestManageSender_relativeTMPDIRIsAnError(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("TMPDIR", "rel")
	ran := false
	m := ManageSender{Program: "crw", Run: func(context.Context, string, ...string) ([]byte, int, error) {
		ran = true
		return []byte(`{"class":"accepted","requestId":"r1"}`), 0, nil
	}}
	_, err := m.SendParent(context.Background(), ParentSend{Thread: "orch", LogicalID: "esc-1", Text: "t", Queue: true})
	if err == nil || !strings.Contains(err.Error(), "tmpdir_not_absolute") || !strings.Contains(err.Error(), `TMPDIR="rel"`) {
		t.Errorf("a relative TMPDIR: %v, want tmploc's tmpdir_not_absolute", err)
	}
	if ran {
		t.Error("the notice was sent although its message file had no place")
	}
	if left, _ := os.ReadDir(cwd); len(left) != 0 {
		t.Errorf("a relative TMPDIR made %v below the working directory", left)
	}
	// A relative Dir is refused the same way; an absolute TMPDIR is used as given.
	m.Dir = "rel-dir"
	if _, err := m.SendParent(context.Background(), ParentSend{Thread: "orch", LogicalID: "esc-2", Text: "t", Queue: true}); err == nil || !strings.Contains(err.Error(), "temp_parent_not_absolute") {
		t.Errorf("a relative Dir: %v, want temp_parent_not_absolute", err)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var file string
	m.Dir = ""
	m.Run = func(_ context.Context, _ string, args ...string) ([]byte, int, error) {
		for i, a := range args {
			if a == "--message-file" && i+1 < len(args) {
				file = args[i+1]
			}
		}
		return []byte(`{"class":"accepted","requestId":"r1"}`), 0, nil
	}
	if _, err := m.SendParent(context.Background(), ParentSend{Thread: "orch", LogicalID: "esc-3", Text: "t", Queue: true}); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(file) != tmp {
		t.Errorf("the message file %q is not in TMPDIR %s", file, tmp)
	}
}
