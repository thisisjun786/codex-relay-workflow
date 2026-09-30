package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The command tests in this package hold what the Go skill commands answer to goldens
// (internal/testsupport/golden), first taken as what the Python reference scripts answered.
// Every value a test and its subtests check is kept in one golden file named after the
// top-level test, which therefore calls goldenRoot before its subtests check.

var goldenRoots sync.Map // top-level test name -> testing.TB

// goldenRoot makes t, a top-level test, the owner of the golden file its subtests' values are
// kept in.
func goldenRoot(t *testing.T) {
	t.Helper()
	goldenRoots.Store(t.Name(), testing.TB(t))
	t.Cleanup(func() { goldenRoots.Delete(t.Name()) })
}

// goldenScope is a subtest seen as its top-level test for naming and keeping the golden file,
// while failures still land on the subtest that checked.
type goldenScope struct {
	testing.TB
	root testing.TB
}

func (s goldenScope) Name() string                      { return s.root.Name() }
func (s goldenScope) Cleanup(f func())                  { s.root.Cleanup(f) }
func (s goldenScope) Errorf(format string, args ...any) { s.root.Errorf(format, args...) }

// skillGolden answers the testing.TB that holds t's golden file and the key prefix that names t
// inside it: t itself and "" for a top-level test, the top-level test and the subtest path
// otherwise.
func skillGolden(t testing.TB) (testing.TB, string) {
	t.Helper()
	top, sub, found := strings.Cut(t.Name(), "/")
	if !found {
		return t, ""
	}
	root, ok := goldenRoots.Load(top)
	if !ok {
		t.Fatalf("%s checks a golden from a subtest; call goldenRoot(t) at the start of %s", t.Name(), top)
	}
	return goldenScope{TB: t, root: root.(testing.TB)}, sub
}

func goldenKey(sub, key string) string {
	switch {
	case sub == "":
		return key
	case key == "":
		return sub
	}
	return sub + " | " + key
}

// argsQuestion stores the run-specific paths a command's answer can carry as placeholders: the
// checkout as <ROOT> and each temporary directory the command names, in its arguments or as its
// working directory, as <TMP>, <TMP2>, ... It also answers the arguments spelled with those
// placeholders, which names the question in the golden file.
func argsQuestion(dir string, args []string) ([]golden.Option, string) {
	root := repositoryRoot()
	pairs := [][2]string{{root, "<ROOT>"}}
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	seen := map[string]bool{}
	for _, value := range append([]string{dir}, args...) {
		value = strings.ReplaceAll(value, root, "")
		for {
			at := strings.Index(value, tmp)
			if at < 0 {
				break
			}
			rest := value[at+len(tmp):]
			base, _, _ := strings.Cut(rest, string(filepath.Separator))
			value = rest
			if base == "" || seen[base] {
				continue
			}
			seen[base] = true
			placeholder := "<TMP>"
			if len(seen) > 1 {
				placeholder = fmt.Sprintf("<TMP%d>", len(seen))
			}
			pairs = append(pairs, [2]string{tmp + base, placeholder})
		}
	}
	options := make([]golden.Option, 0, len(pairs))
	question := strconv.Quote(strings.Join(args, " "))
	for _, pair := range pairs {
		options = append(options, golden.Substitute(pair[0], pair[1]))
		question = strings.ReplaceAll(question, pair[0], pair[1])
	}
	return options, question
}

// checkSkillAnswer holds what a skill command exited with and printed to the golden kept in t's
// golden file under the command's arguments (after the binary: `skill <family> ...`) run from
// dir, and under key too when one test asks the same arguments more than once.
func checkSkillAnswer(t testing.TB, key, dir string, args []string, answer skillProcessResult) {
	t.Helper()
	scope, sub := skillGolden(t)
	options, question := argsQuestion(dir, args)
	key = goldenKey(goldenKey(sub, key), question)
	golden.Check(scope, goldenKey(key, "exit"), []byte(strconv.Itoa(answer.exit)), options...)
	golden.Check(scope, goldenKey(key, "stdout"), []byte(answer.stdout), options...)
	golden.Check(scope, goldenKey(key, "stderr"), []byte(answer.stderr), options...)
}

// checkSkillValue holds a value to the golden kept in t's golden file under key.
func checkSkillValue(t testing.TB, key string, value []byte, opts ...golden.Option) {
	t.Helper()
	scope, sub := skillGolden(t)
	golden.Check(scope, goldenKey(sub, key), value, append([]golden.Option{golden.Substitute(repositoryRoot(), "<ROOT>")}, opts...)...)
}
