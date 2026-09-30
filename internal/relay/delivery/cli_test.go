package delivery

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The twelve delivery commands through the built `crw relay` over a copy of the seeded store.
// Every stdout is checked whole against the golden, byte for byte, after the wall-clock stamps
// (the only nondeterministic bytes) are replaced by one token, and so is every exit code. The
// goldens began as the Python console script's answers over its own copy of the same seed.

// TestMain builds this once before isolating HOME.
var crwPath string

func crwBinary(t *testing.T) string {
	t.Helper()
	return crwPath
}

type cliSide struct {
	t     *testing.T
	home  string
	state string
	work  string
	argv0 []string
	dir   string
	env   []string
	// checked numbers the golden keys.
	checked int
}

// newSide is the built `crw relay` with its own home and a copy of the CLI seed, over the artifact
// tree at work (a parityTree, because a revision hash covers the declared path).
func newSide(t *testing.T, work string) *cliSide {
	home := t.TempDir()
	s := &cliSide{t: t, home: home, state: filepath.Join(home, "state"), work: work}
	s.env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xs"), "XDG_DATA_HOME="+filepath.Join(home, "xd"), "XDG_CONFIG_HOME="+filepath.Join(home, "xc"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
	s.argv0 = []string{crwBinary(t), "relay"}
	s.dir = repoRoot(t)
	copyCLISeed(t, s.state, s.work)
	return s
}

// run is the command's stdout and exit code.
func (s *cliSide) run(args ...string) (string, int) {
	argv := append(append([]string(nil), s.argv0...), append([]string{"--state", s.state}, args...)...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.dir
	cmd.Env = s.env
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	code := 0
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
	return out.String(), code
}

// expect checks got, the answer to what with the side's home already masked (normal), against the
// golden under a key numbering it.
func (s *cliSide) expect(what, got string) {
	s.t.Helper()
	s.checked++
	key := fmt.Sprintf("%d %s", s.checked, strings.NewReplacer(s.work, "<work>", s.home, "<home>").Replace(what))
	golden.Check(s.t, key, []byte(got), golden.Substitute(s.work, "<work>"))
}

var stamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

func (s *cliSide) normal(text string) string {
	return strings.ReplaceAll(stamp.ReplaceAllString(text, "<stamp>"), s.home, "<home>")
}

func TestCLI_every_delivery_command_answers_byte_for_byte_like_python(t *testing.T) {
	work := filepath.Join(parityTree(t), "work")
	side := newSide(t, work)
	mustDo(t, os.WriteFile(filepath.Join(work, "out.txt"), []byte("the deliverable"), 0o644))
	rid := strings.Trim(goSQLiteDump(t, filepath.Join(side.state, "relay.sqlite3"), "select relationship_id from relationships"), "[]\"\n ")
	side.expect("relationship id", rid)
	mustDo(t, os.WriteFile(filepath.Join(work, "self.txt"), []byte("declares itself"), 0o644))
	entries, err := store.BuildManifest([]string{filepath.Join(work, "self.txt")}, []string{work})
	mustDo(t, err)
	selfHash, err := store.ManifestRevision(entries)
	mustDo(t, err)
	cases := [][]string{
		{"criteria-show", "--relationship", rid},
		{"criteria-register", "--relationship", rid, "--criterion", "c1=one", "--criterion", "c2=two", "--optional", "c2", "--source-ref", "doc"},
		{"criteria-show", "--relationship", rid},
		{"criteria-register", "--relationship", rid, "--criterion", "c1=one", "--criterion", " c1 =again"},
		{"ack-proof", "--event", "0123456789abcdef0123456789abcdef", "--turn", "t"},
		{"ack-proof", "--event", "nope", "--turn", "t"},
		{"revision-head", "--relationship", rid},
		{"revision-head", "--relationship", "rel-missing"},
		{"emit", "--relationship", rid, "--generation", "1", "--outcome", "failed", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "failed"},
		{"emit", "--relationship", rid, "--generation", "1", "--outcome", "failed", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "failed"},
		{"emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed", "--artifact", "<work>/out.txt"},
		// QA: a duplicate emit of the same revision, and a revision declaring itself its own
		// predecessor, which is refused with the same reason on both sides.
		{"emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed", "--artifact", "<work>/out.txt"},
		{"emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--artifact", "<work>/self.txt", "--supersedes-revision", selfHash},
		{"emit", "--relationship", rid, "--generation", "1", "--outcome", "failed", "--turn-thread", "someone-else", "--turn-id", "turn-dispatch-1", "--turn-status", "failed"},
		{"emit", "--relationship", "rel-missing", "--generation", "1", "--outcome", "failed", "--turn-thread", "a", "--turn-id", "b"},
		{"revision-head", "--relationship", rid},
		{"claim", "--event", "<failed>", "--turn", "t1"},
		{"claim", "--event", "<failed>", "--turn", "t1"},
		{"ack", "--event", "<failed>", "--ack-turn", "t1", "--ack-proof", "<proof>"},
		{"ack", "--event", "<failed>", "--ack-turn", "t1", "--ack-proof", "wrong"},
		{"verdict", "--event", "<failed>", "--verdict", "verified", "--verdict-turn", "v"},
		{"verdict", "--event", "<failed>", "--verdict", "verified", "--verdict-turn", "v", "--restoration", ""},
		{"verdict", "--event", "<failed>", "--verdict", "needs_changes", "--verdict-turn", "v", "--finding", "c1=needs_changes:fix", "--restoration", "c9"},
		{"deliver"},
		{"deliver", "--event", "<failed>"},
		{"reconcile", "--request-id", "del-x"},
		{"recover"},
		{"verify-acks"},
	}
	failed := "4a7c8d2e7b0b06e7e2b4b71c55f2b7c1"
	for i, args := range cases {
		expand := func(s *cliSide) []string {
			out := make([]string, len(args))
			for j, a := range args {
				a = strings.ReplaceAll(a, "<work>", s.work)
				a = strings.ReplaceAll(a, "<failed>", failed)
				a = strings.ReplaceAll(a, "<proof>", AckProof(failed, "t1"))
				out[j] = a
			}
			return out
		}
		out, code := side.run(expand(side)...)
		side.expect("run "+strings.Join(args, " "), fmt.Sprintf("%d\n%s", code, side.normal(out)))
		if args[0] == "emit" && i == 8 {
			if m := regexp.MustCompile(`"eventId": "([0-9a-f]{32})"`).FindStringSubmatch(out); m != nil {
				failed = m[1]
			}
		}
	}
	if n := strings.Count(fmtCases(cases), "supersedes-revision"); n != 1 {
		t.Fatal("the self-supersede case is present")
	}
}

func fmtCases(cases [][]string) string {
	var b strings.Builder
	for _, c := range cases {
		b.WriteString(strings.Join(c, " "))
	}
	return b.String()
}
