package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkRefuseListing is every entry below dir with its content, so a test can show that nothing outside the workspace changed.
func linkRefuseListing(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != dir {
			data, _ := os.ReadFile(path)
			out = append(out, strings.TrimPrefix(path, dir)+"="+string(data))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// Every writer of this package refuses a symbolic link where .crw, .crw/interviews, .crw/sessions, the session's ledger outbox or
// the file itself belongs: the call fails naming the link and "symbolic link", and the directory or file it leads to outside the
// workspace is neither created nor modified.
func TestStateWritersRefuseALinkInTheStateTree(t *testing.T) {
	interview := InterviewEvent{TS: "t", SessionID: "s1", Event: ScanCompleted, RoundID: 1}
	event := LedgerEvent{ID: "e1", SessionID: "s1", Seq: 1, PrePhase: PhaseP, PostPhase: PhaseA}
	writers := map[string]func(cwd string) error{
		"AppendLedger":         func(cwd string) error { return AppendLedger(cwd, LedgerEntry{TS: "t", SessionID: "s1", To: PhaseI}) },
		"AppendLedgerLine":     func(cwd string) error { return AppendLedgerLine(cwd, []byte(`{"row":1}`)) },
		"AppendInterviewEvent": func(cwd string) error { return AppendInterviewEvent(cwd, interview) },
		"WriteState":           func(cwd string) error { return WriteState(cwd, DefaultState("s1", "")) },
		"EnsureState":          func(cwd string) error { _, err := EnsureState(cwd, "s1"); return err },
		"writeLedgerEvent":     func(cwd string) error { return writeLedgerEvent(cwd, event) },
	}
	cases := []struct {
		writer, link string // link is the place, relative to cwd, that leads outside
		file         bool   // the link leads to a file outside rather than a directory
	}{
		{"AppendLedger", ".crw", false},
		{"AppendLedger", ".crw/ledger.jsonl", true},
		{"AppendLedgerLine", ".crw", false},
		{"AppendLedgerLine", ".crw/ledger.jsonl", true},
		{"AppendInterviewEvent", ".crw", false},
		{"AppendInterviewEvent", ".crw/interviews", false},
		{"AppendInterviewEvent", ".crw/interviews/s1.jsonl", true},
		{"WriteState", ".crw", false},
		{"WriteState", ".crw/sessions", false},
		{"WriteState", ".crw/sessions/s1.json", true},
		{"EnsureState", ".crw/sessions", false},
		{"writeLedgerEvent", ".crw/sessions", false},
		{"writeLedgerEvent", ".crw/sessions/s1.json" + ledgerOutboxSuffix, false},
	}
	for _, c := range cases {
		t.Run(c.writer+" "+c.link, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			target := outside
			if c.file {
				target = filepath.Join(outside, "victim")
				if err := os.WriteFile(target, []byte("keep\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(cwd, c.link)
			if err := os.MkdirAll(filepath.Dir(link), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			before := linkRefuseListing(t, outside)
			err := writers[c.writer](cwd)
			if err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("err = %v, want a refusal naming %s as a symbolic link", err, link)
			}
			if after := linkRefuseListing(t, outside); after != before {
				t.Fatalf("outside the workspace changed:\n%s\nwas\n%s", after, before)
			}
			if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the link was not kept: %v %v", info, err)
			}
		})
	}
}
