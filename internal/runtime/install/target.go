package install

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// provides answers whether the runtime the pointer is about to name serves rel: a path relative
// to that runtime's directory, as a registration or the Stop settings reach it through the
// pointer. A path it does not serve is one the swap would leave naming nothing.
type provides func(rel string) bool

// goProvides is what a Go runtime directory serves: bin/crw and its three compatibility links
// (docs/port/decisions.md 10), and nothing else a command could run. A promotion always moves
// the pointer to one, so it needs no reading of the candidate to answer.
func goProvides(rel string) bool {
	switch rel {
	case "", "bin", "bin/" + Binary:
		return true
	}
	for _, link := range definition.Links() {
		if rel == "bin/"+link {
			return true
		}
	}
	return false
}

// providesFor is what the runtime at environment serves: a Go runtime by its layout, a venv by
// what is there.
func providesFor(kind, environment string) provides {
	if kind != doctor.KindPythonVenv {
		return goProvides
	}
	return func(rel string) bool {
		_, err := os.Stat(filepath.Join(environment, rel))
		return err == nil
	}
}

// throughPointer is the part of an absolute path that lies beyond the pointer: "" for the
// pointer itself, and ok false for a path the pointer does not lead to. The pointer is found by
// identity, not only by spelling: an ancestor of path that is the pointer link itself (Lstat,
// os.SameFile) - reached through a symlinked home, /tmp -> /private/tmp or a bind mount - is the
// pointer as much as its recorded spelling is.
func throughPointer(path, pointerPath string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	path, pointerPath = filepath.Clean(path), filepath.Clean(pointerPath)
	if path == pointerPath {
		return "", true
	}
	if rest, ok := strings.CutPrefix(path, pointerPath+"/"); ok {
		return rest, true
	}
	link, err := os.Lstat(pointerPath)
	if err != nil {
		return "", false
	}
	for dir, rest := path, ""; ; {
		if info, err := os.Lstat(dir); err == nil && os.SameFile(info, link) {
			return rest, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		if rest == "" {
			rest = filepath.Base(dir)
		} else {
			rest = filepath.Base(dir) + "/" + rest
		}
		dir = parent
	}
}

// installsAt is every install entry of component name whose runtime directory is exactly
// environment (both resolved), newest last. A directory that merely contains one - the
// destination, or / - is not a runtime the record lists.
func installsAt(rec Object, name, environment string) []Object {
	want, err := record.Resolve(environment)
	if err != nil {
		return nil
	}
	components, _ := record.Get(rec, "components").(Object)
	component, _ := record.Get(components, name).(Object)
	entries, _ := record.Get(component, "installs").([]any)
	var found []Object
	for _, raw := range entries {
		install, ok := raw.(Object)
		if !ok {
			continue
		}
		if location, _ := record.Get(install, "location").(string); location == "" {
			continue
		}
		if resolved, err := record.Resolve(environmentOf(install)); err == nil && resolved == want {
			found = append(found, install)
		}
	}
	return found
}

// targetProblems is why the runtime at environment could not be launched through the pointer,
// and what could not be examined (never read as launchable). Nothing is run: a Go runtime is
// judged as the doctor judges a selected one, and a venv by what the kernel would need to
// execute each name the pointer exposes, plus what the one Stop settings document needs from it.
func targetProblems(rec Object, environment, kind string) (problems, unread []string) {
	if kind == doctor.KindGoRuntime {
		return doctor.LaunchProblems(environment)
	}
	bin := filepath.Join(environment, "bin")
	for _, name := range []string{definition.Relay, definition.Bridge, definition.HookScript} {
		p, u := executableProblems(filepath.Join(bin, name))
		problems, unread = append(problems, p...), append(unread, u...)
	}
	seen := map[string]bool{}
	for _, c := range definition.Components {
		for _, install := range installsAt(rec, c.Name, environment) {
			interpreter, _ := record.Get(install, "interpreterPath").(string)
			if interpreter == "" || seen[interpreter] {
				continue
			}
			seen[interpreter] = true
			p, u := executableProblems(interpreter)
			problems, unread = append(problems, p...), append(unread, u...)
		}
	}
	p, u := fenceProblems(rec, environment)
	return append(problems, p...), append(unread, u...)
}

// fenceProblems is why the venv at environment could not serve the one plugin-owned Stop
// settings document through the pointer: /usr/bin/env runs <pointer>/bin/crw-completion-hook
// with the settings path as its one argument, which on a venv has to be the Python Stop
// adapter's console script (codex_session_relay.stopadapter:main reads argv[1] as its
// settings), and the relay it asks has to be the retained fence release - the only Python
// release that may run beside a store the Go runtime owns (docs/port/cutover.md).
func fenceProblems(rec Object, environment string) (problems, unread []string) {
	script := filepath.Join(environment, "bin", definition.HookScript)
	if text, err := readLimited(script, 64<<10); err == nil && !bytes.Contains(text, []byte("codex_session_relay.stopadapter")) {
		problems = append(problems, script+" is not the console script of codex_session_relay.stopadapter, so the Stop settings (/usr/bin/env "+definition.HookScript+" through the pointer) would not reach the Python Stop adapter there")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		unread = append(unread, "what "+script+" runs: "+store.PythonOSError(err))
	}
	relays := installsAt(rec, definition.Relay, environment)
	if len(relays) == 0 {
		return append(problems, "the host record lists no "+definition.Relay+" install in "+environment+", so which relay release it carries is not established"), unread
	}
	location, _ := record.Get(relays[len(relays)-1], "location").(string)
	if info, err := os.Stat(filepath.Join(location, "stopadapter.py")); err != nil || !info.Mode().IsRegular() {
		problems = append(problems, location+" carries no stopadapter.py, so the console script has no Python Stop adapter to import")
	}
	declared := "BUILD = " + `"` + ownership.PythonBuild + `"`
	text, err := readLimited(filepath.Join(location, "ownership.py"), 1<<20)
	switch {
	case errors.Is(err, os.ErrNotExist):
		problems = append(problems, location+" carries no ownership.py, so it is a relay release from before the fence (docs/port/cutover.md), which must never run beside a store the Go runtime owns")
	case err != nil:
		unread = append(unread, "whether "+location+" is the fence release: "+store.PythonOSError(err))
	case !declaresLine(text, declared):
		problems = append(problems, location+"/ownership.py does not declare "+declared+", so it is not the retained fence release this build hands a store back to")
	}
	return problems, unread
}

// declaresLine is whether one line of text, trimmed, is exactly line.
func declaresLine(text []byte, line string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, 4096), len(text)+1)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == line {
			return true
		}
	}
	return false
}

// executableProblems is why the kernel could not execute path: it must resolve to a regular file
// this user may execute, and a script's #! interpreter must too (to the kernel's four levels).
func executableProblems(path string) (problems, unread []string) {
	for depth := 0; ; depth++ {
		info, err := os.Stat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return []string{path + " does not exist, or is a link that names nothing"}, nil
		case err != nil:
			return nil, []string{path + ": " + store.PythonOSError(err)}
		case !info.Mode().IsRegular():
			return []string{path + " is not a regular file"}, nil
		}
		if err := unix.Access(path, unix.X_OK); errors.Is(err, unix.EACCES) {
			return []string{path + " is not executable by this user (" + info.Mode().Perm().String() + ")"}, nil
		} else if err != nil {
			return nil, []string{"whether " + path + " is executable: " + err.Error()}
		}
		head, err := readLimited(path, 256)
		if err != nil {
			return nil, []string{path + ": " + store.PythonOSError(err)}
		}
		if !bytes.HasPrefix(head, []byte("#!")) {
			if nativeImage(head) {
				return nil, nil
			}
			return []string{path + " is neither a native executable nor a script with #!, so the kernel would not execute it"}, nil
		}
		line, _, _ := bytes.Cut(head[2:], []byte("\n"))
		words := strings.Fields(string(line))
		switch {
		case len(words) == 0 || !filepath.IsAbs(words[0]):
			return []string{path + " names no absolute interpreter on its #! line"}, nil
		case depth >= 3:
			return []string{path + " is reached through more interpreters than the kernel follows"}, nil
		}
		path = words[0]
	}
}

func nativeImage(head []byte) bool {
	for _, magic := range [][]byte{{0x7f, 'E', 'L', 'F'}, {0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.HasPrefix(head, magic) {
			return true
		}
	}
	return false
}

// readLimited is at most limit bytes of the file at path.
func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

// errNoExchange is exchange's answer where the two names cannot be swapped in one step.
var errNoExchange = errors.New("the filesystem cannot exchange two names in one step")
