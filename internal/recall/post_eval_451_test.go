package recall

import (
	"bytes"
	"strings"
	"testing"
)

// CRW-451 post-evaluation d1: Node decodes process.argv as UTF-8 before the CLI sees it, so a query byte outside UTF-8 is U+FFFD and finds
// the decoded text it stands for. jsnode.Lower keeps such a byte, so the CLI has to decode the query itself.
func TestCRW451RawQueryBytesAreDecodedAtTheCLI(t *testing.T) {
	const raw = "TOKEN\xff"
	t.Run("chat", func(t *testing.T) {
		home := sweepChatRollout(t, "", "aa", [][3]string{{"user", "the TOKEN� marker", "08"}})
		var out, errOut bytes.Buffer
		code := Run([]string{"chat", "search", "--home", home, "--scan", "--days", "0", "--source", "all", "--json", "--", raw}, &out, &errOut, scanTestNow())
		if code != 0 || !strings.Contains(out.String(), "TOKEN") || !strings.Contains(out.String(), `"text"`) {
			t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errOut.String())
		}
	})
	t.Run("memory", func(t *testing.T) {
		home := t.TempDir()
		writeRolloutTestFile(t, home, "memories/a.md", "note: the TOKEN� marker\n")
		var out, errOut bytes.Buffer
		code := Run([]string{"memory", "search", "--home", home, "--no-chat", "--json", "--", raw}, &out, &errOut, scanTestNow())
		if code != 0 || !strings.Contains(out.String(), "a.md") {
			t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errOut.String())
		}
	})
}
