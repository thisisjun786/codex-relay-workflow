package ledger

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

// The answer ledger's append refuses a symbolic link at .crw, .crw/interviews or the ledger file, naming it, and writes nothing to
// the directory or file it leads to.
func TestAppendEventRefusesALinkInTheLedgerPath(t *testing.T) {
	for _, c := range []struct {
		link string
		file bool
	}{{".crw", false}, {".crw/interviews", false}, {".crw/interviews/s1.jsonl", true}} {
		t.Run(c.link, func(t *testing.T) {
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
			err := appendEvent(cwd, QaEvent{TS: "t", SessionID: "s1", TurnID: "t1", Event: QuestionAsked, QuestionID: "q", EventID: "e", Question: "?"})
			if err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("err = %v, want a refusal naming %s as a symbolic link", err, link)
			}
			if after := linkRefuseListing(t, outside); after != before {
				t.Fatalf("outside the workspace changed:\n%s\nwas\n%s", after, before)
			}
		})
	}
}
