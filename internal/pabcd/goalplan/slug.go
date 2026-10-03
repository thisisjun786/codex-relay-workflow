package goalplan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The slug rules and the path of a plan's directory (goalplan.ts:292-331), under the CRW names: the state directory is
// crwdir.DirName. A slug is an identifier, never a path, and the directory it names is refused when any step of the way to it is a
// symbolic link, because a lexically safe slug under a linked state root would still write outside the project.

// maxSlugBytes is MAX_SLUG_BYTES.
const maxSlugBytes = 128

// ValidateGoalplanSlug is validateGoalplanSlug: 1 to 128 bytes, ASCII, the first an ASCII letter or digit and the rest letters,
// digits, ".", "_" or "-". The oracle's separate refusals of "." and ".." are covered by the first-character rule and are not
// ported. The error text is the oracle's, with JSON.stringify of the slug.
func ValidateGoalplanSlug(slug string) (string, error) {
	alnum := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' }
	ok := len(slug) > 0 && len(slug) <= maxSlugBytes && alnum(slug[0])
	for i := 1; ok && i < len(slug); i++ {
		ok = alnum(slug[i]) || slug[i] == '.' || slug[i] == '_' || slug[i] == '-'
	}
	if !ok {
		return "", errors.New("invalid goalplan slug: " + quote(slug))
	}
	return slug, nil
}

// assertNotSymlink refuses a path that is a symbolic link. The oracle asks existsSync first, which follows the link, so a link
// whose target is absent is not refused (known-defects.md, port: kept); an lstat that fails after a stat that worked is returned as it is.
func assertNotSymlink(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("goalplan state path must not be a symlink: " + path)
	}
	return nil
}

// GoalplanDir is goalplanDir: <cwd>/.crw/goalplans/<slug> for a valid slug, refused when the state root, the plans root or the
// slug directory is a symbolic link. cwd is resolved as path.resolve does: a relative one against the process's physical working
// directory (syscall.Getwd, which is what process.cwd() answers), and cleaned. Nothing is created.
func GoalplanDir(cwd, slug string) (string, error) {
	safe, err := ValidateGoalplanSlug(slug)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(cwd) {
		wd, err := syscall.Getwd()
		if err != nil {
			return "", err
		}
		cwd = filepath.Join(wd, cwd)
	}
	stateRoot := filepath.Join(filepath.Clean(cwd), crwdir.DirName)
	plansRoot := filepath.Join(stateRoot, GoalplansSubdir)
	dir := filepath.Join(plansRoot, safe)
	if !strings.HasPrefix(dir, plansRoot+string(filepath.Separator)) {
		return "", errors.New("goalplan path escapes state root")
	}
	for _, path := range [...]string{stateRoot, plansRoot, dir} {
		if err := assertNotSymlink(path); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// goalplanPath is goalplanPath: the plan file of a slug.
func goalplanPath(cwd, slug string) (string, error) { return inPlanDir(cwd, slug, GoalplanFile) }

// goalplanLedgerPath is goalplanLedgerPath: the ledger of a slug.
func goalplanLedgerPath(cwd, slug string) (string, error) {
	return inPlanDir(cwd, slug, GoalplanLedgerFile)
}

func inPlanDir(cwd, slug, name string) (string, error) {
	dir, err := GoalplanDir(cwd, slug)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}
