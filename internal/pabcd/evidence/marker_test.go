package evidence

import (
	"os"
	"path/filepath"
	"testing"
)

// CRW-1112: the marker is the last line of the reply that is a marker with a path; the directive the child may quote names the
// marker inside a sentence, so it is never the marker.
func TestExtractReceiptPathLastStandaloneLine(t *testing.T) {
	for _, c := range []struct {
		name, message, want string
		ok                  bool
	}{
		{"quoted directive then the last line", "I was told: make the LAST line of your reply exactly `EVIDENCE_RECORDED: <path>` pointing at that file.\nDone.\nEVIDENCE_RECORDED: /abs/ok.md", "/abs/ok.md", true},
		{"two marker lines, the last wins", "EVIDENCE_RECORDED: /abs/first.md\nEVIDENCE_RECORDED: /abs/second.md", "/abs/second.md", true},
		{"an empty marker does not take the next line", "EVIDENCE_RECORDED:\n/abs/next.md", "", false},
		{"an empty last marker leaves the last valid one", "EVIDENCE_RECORDED: /abs/ok.md\nEVIDENCE_RECORDED:   ", "/abs/ok.md", true},
		{"indented, CRLF and trailing space", "x\r\n   EVIDENCE_RECORDED:  /abs/ok.md  \r\n", "/abs/ok.md", true},
		{"a marker inside a sentence only", "see `EVIDENCE_RECORDED: /abs/ok.md` above", "", false},
		{"nothing", "Done.", "", false},
	} {
		got, ok := ExtractReceiptPath(c.message)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// CRW-1112: only a relative path that is ".." or starts with ".." and a separator leaves the root; a name that merely starts with
// two dots is inside it. The other refusals are unchanged.
func TestHasValidReceiptDotDotNames(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	for _, p := range []string{".crw/evidence/..notes.txt", ".crw/evidence/..d/result.txt", ".crw/evidence/ok.txt", ".crw/evidence/empty.txt", "outside.txt"} {
		text := "verified"
		if filepath.Base(p) == "empty.txt" {
			text = ""
		}
		put(t, filepath.Join(cwd, p), []byte(text))
	}
	put(t, filepath.Join(outside, "x.txt"), []byte("verified"))
	must(t, os.MkdirAll(filepath.Join(cwd, ".crw/evidence/dir"), 0o777))
	must(t, os.Symlink(filepath.Join(cwd, ".crw/evidence/ok.txt"), filepath.Join(cwd, ".crw/evidence/link.txt")))
	for claim, want := range map[string]bool{
		".crw/evidence/..notes.txt":           true,
		".crw/evidence/..d/result.txt":        true,
		cwd + "/.crw/evidence/..notes.txt":    true,
		".crw/evidence/../outside":            false,
		".crw/evidence/../../outside.txt":     false,
		".crw/evidence":                       false,
		".crw/evidence/":                      false,
		filepath.Join(outside, "x.txt"):       false,
		".crw/evidence/empty.txt":             false,
		".crw/evidence/dir":                   false,
		".crw/evidence/link.txt":              false,
		".crw/evidence/..":                    false,
		".crw/evidence/../evidence/ok.txt":    true,
		".crw/evidence/..d/../../outside.txt": false,
	} {
		if got := HasValidReceipt(cwd, claim); got != want {
			t.Errorf("%s: %v, want %v", claim, got, want)
		}
	}
}
