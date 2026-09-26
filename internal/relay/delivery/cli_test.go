package delivery

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The twelve delivery commands through the real processes: Python's `codex-session-relay` and
// the built `crw relay`, each over its own copy of the same seeded store. Every stdout is
// compared whole, byte for byte, after the wall-clock stamps (the only nondeterministic bytes)
// are replaced by one token; exit codes are compared exactly.

var (
	crwOnce sync.Once
	crwPath string
	crwErr  error
)

func crwBinary(t *testing.T) string {
	t.Helper()
	crwOnce.Do(func() {
		dir, err := os.MkdirTemp("", "crw-delivery-cli-")
		if err != nil {
			crwErr = err
			return
		}
		crwPath = filepath.Join(dir, "crw")
		build := exec.Command("go", "build", "-buildvcs=false", "-o", crwPath, "./cmd/crw")
		build.Dir = repoRoot(t)
		if out, err := build.CombinedOutput(); err != nil {
			crwErr = err
			t.Log(string(out))
		}
	})
	if crwErr != nil {
		t.Fatal(crwErr)
	}
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
}

// newSide keeps the artifact tree at one shared path (work), because a revision hash covers the
// declared path; each side has its own home and store.
func newSide(t *testing.T, python bool, work string) *cliSide {
	home := t.TempDir()
	s := &cliSide{t: t, home: home, state: filepath.Join(home, "state"), work: work}
	root := repoRoot(t)
	s.env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xs"), "XDG_DATA_HOME="+filepath.Join(home, "xd"), "XDG_CONFIG_HOME="+filepath.Join(home, "xc"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
	if python {
		s.argv0 = []string{"uv", "run", "--no-sync", "codex-session-relay"}
		s.dir = filepath.Join(root, "packages", "codex-session-relay")
	} else {
		s.argv0 = []string{crwBinary(t), "relay"}
		s.dir = root
	}
	seed := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/delivery/testdata/cliseed.py"), s.state, s.work)
	seed.Dir = filepath.Join(root, "packages", "codex-session-relay")
	seed.Env = s.env
	out, err := seed.Output()
	mustDo(t, err)
	if strings.TrimSpace(string(out)) != "rel-4675b3fb54d7b85d" && !strings.HasPrefix(string(out), "rel-") {
		t.Fatalf("seed %s", out)
	}
	return s
}

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

var stamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

func (s *cliSide) normal(text string) string {
	return strings.ReplaceAll(stamp.ReplaceAllString(text, "<stamp>"), s.home, "<home>")
}

func TestCLI_every_delivery_command_answers_byte_for_byte_like_python(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	py, gosd := newSide(t, true, work), newSide(t, false, work)
	mustDo(t, os.WriteFile(filepath.Join(work, "out.txt"), []byte("the deliverable"), 0o644))
	realRID := func(s *cliSide) string {
		seedOut := strings.TrimSpace(func() string {
			cmd := exec.Command("uv", "run", "--no-sync", "python", "-c", "import sqlite3,sys;print(sqlite3.connect(sys.argv[1]).execute('select relationship_id from relationships').fetchone()[0])", filepath.Join(s.state, "relay.sqlite3"))
			cmd.Dir = py.dir
			cmd.Env = s.env
			b, err := cmd.Output()
			mustDo(t, err)
			return string(b)
		}())
		return seedOut
	}
	pr, gr := realRID(py), realRID(gosd)
	if pr != gr {
		t.Fatalf("seeded ids differ %s %s", pr, gr)
	}
	mustDo(t, os.WriteFile(filepath.Join(work, "self.txt"), []byte("declares itself"), 0o644))
	entries, err := store.BuildManifest([]string{filepath.Join(work, "self.txt")}, []string{work})
	mustDo(t, err)
	selfHash, err := store.ManifestRevision(entries)
	mustDo(t, err)
	cases := [][]string{
		{"criteria-show", "--relationship", pr},
		{"criteria-register", "--relationship", pr, "--criterion", "c1=one", "--criterion", "c2=two", "--optional", "c2", "--source-ref", "doc"},
		{"criteria-show", "--relationship", pr},
		{"criteria-register", "--relationship", pr, "--criterion", "c1=one", "--criterion", " c1 =again"},
		{"ack-proof", "--event", "0123456789abcdef0123456789abcdef", "--turn", "t"},
		{"ack-proof", "--event", "nope", "--turn", "t"},
		{"revision-head", "--relationship", pr},
		{"revision-head", "--relationship", "rel-missing"},
		{"emit", "--relationship", pr, "--generation", "1", "--outcome", "failed", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "failed"},
		{"emit", "--relationship", pr, "--generation", "1", "--outcome", "failed", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "failed"},
		{"emit", "--relationship", pr, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed", "--artifact", "<work>/out.txt"},
		// QA: a duplicate emit of the same revision, and a revision declaring itself its own
		// predecessor, which is refused with the same reason on both sides.
		{"emit", "--relationship", pr, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed", "--artifact", "<work>/out.txt"},
		{"emit", "--relationship", pr, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--artifact", "<work>/self.txt", "--supersedes-revision", selfHash},
		{"emit", "--relationship", pr, "--generation", "1", "--outcome", "failed", "--turn-thread", "someone-else", "--turn-id", "turn-dispatch-1", "--turn-status", "failed"},
		{"emit", "--relationship", "rel-missing", "--generation", "1", "--outcome", "failed", "--turn-thread", "a", "--turn-id", "b"},
		{"revision-head", "--relationship", pr},
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
		pout, pcode := py.run(expand(py)...)
		gout, gcode := gosd.run(expand(gosd)...)
		if pcode != gcode || py.normal(pout) != gosd.normal(gout) {
			t.Errorf("case %d %v: exit python %d go %d\npython:\n%s\ngo:\n%s", i, args, pcode, gcode, py.normal(pout), gosd.normal(gout))
		}
		if args[0] == "emit" && i == 8 {
			if m := regexp.MustCompile(`"eventId": "([0-9a-f]{32})"`).FindStringSubmatch(pout); m != nil {
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
