//go:build dev

// Package skills is `crw-dev skills`: link this checkout's skills into a Codex installation.
// It lives in the development binary because it links a checkout, and a release archive has no
// checkout to link. It replaces scripts/install.py, which stays until the Python execution path
// is removed.
package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/ci"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Run dispatches `crw-dev skills <command> [args]`.
func Run(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: crw-dev skills {link} ..."
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "crw-dev skills: error: the following arguments are required: command")
		return 2
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "link":
		return Link(args[1:], os.Getenv, stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	fmt.Fprintf(stderr, "crw-dev skills: error: invalid command %q\n", args[0])
	return 2
}

const linkUsage = "usage: crw-dev skills link [-h] (--check | --apply) [--dest DEST]"

const linkHelp = `Link this checkout's skills into Codex without replacing existing work.

  --check      inspect the links and write nothing; exit 1 while one is missing
  --apply      create the missing links only
  --dest DEST  the skills directory to link into (default $CODEX_HOME/skills, else ~/.codex/skills)`

// Link is `crw-dev skills link`. getenv reads CODEX_HOME and HOME.
func Link(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usageError := func(message string) int {
		fmt.Fprintln(stderr, linkUsage)
		fmt.Fprintln(stderr, "crw-dev skills link: error: "+message)
		return 2
	}
	var mode, dest string
	destGiven := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprintln(stdout, linkUsage+"\n\n"+linkHelp)
			return 0
		case arg == "--check" || arg == "--apply":
			if mode != "" && mode != arg {
				return usageError("argument " + arg + ": not allowed with argument " + mode)
			}
			mode = arg
		case arg == "--dest":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return usageError("argument --dest: expected one argument")
			}
			i++
			dest, destGiven = args[i], true
		case strings.HasPrefix(arg, "--dest="):
			dest, destGiven = strings.TrimPrefix(arg, "--dest="), true
		default:
			return usageError("unrecognized arguments: " + strings.Join(args[i:], " "))
		}
	}
	if mode == "" {
		return usageError("one of the arguments --check --apply is required")
	}
	if destGiven && dest == "" {
		return usageError("argument --dest: expected one argument")
	}
	home := getenv("HOME")
	// install.py computes Path(CODEX_HOME or Path.home()/".codex") / "skills" (or --dest), then
	// .expanduser().absolute(): a leading ~ is expanded on whichever destination it ends up with,
	// the working directory is prefixed to a relative one, and nothing else is normalised beyond
	// Path()'s own spelling rules. In particular ".." is never folded, so a symlink followed by
	// ".." means what the filesystem makes of it, as for Codex; filepath.Join and filepath.Abs
	// would fold it lexically and could name another directory.
	var expanded string
	if !destGiven {
		codexHome := getenv("CODEX_HOME")
		if codexHome == "" && home == "" {
			return usageError("neither CODEX_HOME nor HOME is set, so there is no default destination; pass --dest")
		}
		if codexHome == "" {
			codexHome = pathlibJoin(home, ".codex")
		}
		codexDir, err := expandUser(codexHome, home)
		if err != nil {
			return usageError("CODEX_HOME: " + err.Error())
		}
		expanded = pathlibJoin(codexDir, "skills")
	} else {
		var err error
		if expanded, err = expandUser(dest, home); err != nil {
			return usageError("argument --dest: " + err.Error())
		}
	}
	if !strings.HasPrefix(expanded, "/") {
		cwd, err := os.Getwd()
		if err != nil {
			return usageError(err.Error())
		}
		expanded = pathlibJoin(cwd, expanded)
	}
	destination := store.PathlibSpelling(expanded)
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return usageError("not inside a Git checkout, so there is no checkout to link: " + err.Error())
	}
	return link(strings.TrimSpace(string(out)), destination, mode == "--apply", stdout, stderr)
}

// link links every skill the checkout at root declares into destination. It decides every name
// before it writes anything: one CONFLICT and nothing is written.
func link(root, destination string, apply bool, stdout, stderr io.Writer) int {
	skillsRoot, err := ci.SkillsRoot(root)
	if err != nil {
		fmt.Fprintln(stderr, linkUsage)
		fmt.Fprintln(stderr, "crw-dev skills link: error: "+err.Error())
		return 2
	}
	sources, err := skillSources(skillsRoot)
	if err != nil || len(sources) == 0 {
		fmt.Fprintln(stderr, linkUsage)
		fmt.Fprintf(stderr, "crw-dev skills link: error: no skills found in %s\n", skillsRoot)
		return 2
	}
	type pair struct{ source, target string }
	var pending []pair
	conflicts := 0
	for _, source := range sources {
		target := pathlibJoin(destination, filepath.Base(source)) // destination / name, never folded
		switch linkState(target, source) {
		case "LINKED":
			fmt.Fprintf(stdout, "LINKED %s -> %s\n", target, source)
		case "CONFLICT":
			conflicts++
			fmt.Fprintf(stderr, "CONFLICT %s\n", target)
		default:
			pending = append(pending, pair{source, target})
			fmt.Fprintf(stdout, "MISSING %s\n", target)
		}
	}
	if conflicts > 0 {
		fmt.Fprintln(stderr, "Existing paths were left untouched. Resolve conflicts before installing.")
		return 1
	}
	if !apply {
		if len(pending) > 0 {
			return 1
		}
		return 0
	}
	if len(pending) > 0 {
		if err := os.MkdirAll(destination, 0o777); err != nil {
			return failed(stderr, err)
		}
	}
	for _, p := range pending {
		// symlink(2) refuses a path created after the preflight as well: nothing is replaced.
		if err := symlink(p.source, p.target); err != nil {
			return failed(stderr, err)
		}
		fmt.Fprintf(stdout, "CREATED %s -> %s\n", p.target, p.source)
	}
	return 0
}

// symlink creates one link; a test replaces it to act between the preflight and the write.
var symlink = os.Symlink

// lookupHome answers a named user's home directory, for a ~name destination; a test replaces it.
var lookupHome = func(name string) (string, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return "", err
	}
	return account.HomeDir, nil
}

// expandUser expands a leading ~ or ~name as install.py's Path.expanduser does: ~ is HOME, ~name
// is that user's home directory, and the rest of the path follows it as given. An unknown user is
// refused, as expanduser refuses it ("Could not determine home directory"). So is ~ with HOME
// empty or unset, where expanduser would fall back to the password database or to /: the default
// destination refuses a missing HOME the same way. Never is a ~ left in place, a relative path
// that filepath.Abs would turn into a directory named ~ inside the checkout.
func expandUser(path, home string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	name, rest, _ := strings.Cut(path[1:], "/")
	dir := home
	if name == "" {
		if home == "" {
			return "", fmt.Errorf("HOME is not set, so %q cannot be expanded", path)
		}
	} else {
		var err error
		if dir, err = lookupHome(name); err != nil || dir == "" {
			return "", fmt.Errorf("%q names no user with a home directory", path)
		}
	}
	// Path() collapses repeated slashes before expanduser, so ~//codex is <home>/codex.
	return pathlibJoin(dir, strings.TrimLeft(rest, "/")), nil
}

// pathlibJoin is str(Path(base) / rest) before Path()'s spelling rules: an absolute rest wins,
// and nothing is folded.
func pathlibJoin(base, rest string) string {
	switch {
	case rest == "":
		return base
	case strings.HasPrefix(rest, "/"):
		return rest
	case base == "":
		return rest
	}
	return strings.TrimRight(base, "/") + "/" + rest
}

func failed(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "Installation failed: %v. Completed links are retained; rerun after resolving the error.\n", err)
	return 1
}

// skillSources are the directories directly under root that hold a SKILL.md file, by name.
func skillSources(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, entry := range entries {
		source := filepath.Join(root, entry.Name())
		if info, err := os.Stat(filepath.Join(source, "SKILL.md")); err == nil && info.Mode().IsRegular() {
			sources = append(sources, source)
		}
	}
	sort.Strings(sources)
	return sources, nil
}

// linkState is what target holds for source, without following anything but a symlink's own
// resolution: LINKED when target is a symlink resolving to the same directory as source (a link
// through the repository root's skills alias counts), MISSING when nothing is there, and
// CONFLICT for anything else, a dangling link included.
func linkState(target, source string) string {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return "MISSING"
	}
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "CONFLICT"
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "CONFLICT"
	}
	wanted, err := filepath.EvalSymlinks(source)
	if err != nil || resolved != wanted {
		return "CONFLICT"
	}
	return "LINKED"
}
