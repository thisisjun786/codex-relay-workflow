//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
)

// Path classes, copied from scripts/ci/scope.py. Changing them is a data change reviewed with
// the workflow; the rule that an unregistered path cannot pass CI stays.
var (
	scopeDocs = map[string]bool{"README.md": true, "CONTRIBUTING.md": true, "POLICY.md": true,
		"SECURITY.md": true, "AGENTS.md": true, "LICENSE": true}
	scopeFull = map[string]bool{".gitignore": true, ".gitleaks.toml": true, "pyproject.toml": true,
		"uv.lock": true, "plugins/crw/LICENSE": true, "go.mod": true, "go.sum": true, "tools.go": true,
		"Makefile": true, ".goreleaser.yaml": true, "conftest.py": true}
	scopePrefixes = []string{"scripts/", "packages/", "plugins/crw/wiring/", "plugins/crw/.codex-plugin/",
		".agents/", ".github/", "cmd/", "internal/", "contract/", "docs/port/"}
	scopeReasons = map[string]bool{"paths": true, "empty": true, "base-unavailable": true, "dispatch": true}
	scopeFields  = []string{"version", "event", "base", "head", "base_ref", "ref", "changed", "unknown",
		"unsafe", "reason", "selected"}
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Jobs is the selection's verdict on the expensive jobs.
type Jobs struct {
	Tests    bool
	Packages bool
}

// Selection is the object scope prints and the gate revalidates.
type Selection struct {
	Event, Base, Head, BaseRef, Ref string
	Changed, Unknown, Unsafe        []string
	Reason                          string
	Selected                        Jobs
}

// JSON is json.dumps(selection, separators=(",", ":")).
func (s Selection) JSON() string {
	return pyJSON(jsonObject{{"version", 1}, {"event", s.Event}, {"base", s.Base}, {"head", s.Head},
		{"base_ref", s.BaseRef}, {"ref", s.Ref}, {"changed", nonNil(s.Changed)},
		{"unknown", nonNil(s.Unknown)}, {"unsafe", nonNil(s.Unsafe)}, {"reason", s.Reason},
		{"selected", jsonObject{{"tests", s.Selected.Tests}, {"packages", s.Selected.Packages}}}}, true)
}

func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

// Classify names the verification class of a repository path: docs, skill, full or unknown.
func Classify(path string) string {
	if scopeDocs[path] || (posixParentIsDocs(path) && strings.HasSuffix(path, ".md")) {
		return "docs"
	}
	if path == "skills" || strings.HasPrefix(path, "plugins/crw/skills/") {
		return "skill"
	}
	if scopeFull[path] {
		return "full"
	}
	for _, prefix := range scopePrefixes {
		if strings.HasPrefix(path, prefix) {
			return "full"
		}
	}
	return "unknown"
}

// posixParentIsDocs is PurePosixPath(path).parent == PurePosixPath("docs").
func posixParentIsDocs(path string) bool {
	if strings.HasPrefix(path, "/") {
		return false
	}
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return len(parts) == 2 && parts[0] == "docs"
}

func scopeContext(event, baseRef, ref string) error {
	switch event {
	case "pull_request":
		if baseRef == "" || baseRef == "main" {
			return valueError{"PRs must target a development branch; main is a release mirror"}
		}
	case "push":
		if ref != "refs/heads/dev" {
			return valueError{"Only dev push CI supplies release evidence"}
		}
	case "workflow_dispatch":
	default:
		return valueError{"Unsupported CI event"}
	}
	return nil
}

func selectJobs(changed, unknown, unsafe []string, reason string) Jobs {
	kinds := map[string]bool{}
	for _, path := range changed {
		kinds[Classify(path)] = true
	}
	full := reason != "paths" || len(unknown) > 0 || len(unsafe) > 0 || kinds["full"]
	return Jobs{Tests: full || kinds["skill"], Packages: full}
}

// ValidateSelection applies scope's rules to a selection decoded from JSON (as by
// json.Decoder with UseNumber), the way the gate reads the one it was handed.
func ValidateSelection(raw any) (Selection, error) {
	var s Selection
	value, ok := raw.(map[string]any)
	if !ok || len(value) != len(scopeFields) {
		return s, valueError{"Invalid selection schema"}
	}
	for _, field := range scopeFields {
		if _, present := value[field]; !present {
			return s, valueError{"Invalid selection schema"}
		}
	}
	if number, ok := value["version"].(json.Number); !ok || string(number) != "1" {
		return s, valueError{"Invalid selection schema"}
	}
	text := map[string]string{}
	for _, key := range []string{"event", "base", "head", "base_ref", "ref", "reason"} {
		str, ok := value[key].(string)
		if !ok {
			return s, valueError{fmt.Sprintf("Invalid selection %s", key)}
		}
		text[key] = str
	}
	if !shaPattern.MatchString(text["head"]) {
		return s, valueError{"Invalid candidate SHA"}
	}
	if text["base"] != "" && !shaPattern.MatchString(text["base"]) {
		return s, valueError{"Invalid base SHA"}
	}
	if err := scopeContext(text["event"], text["base_ref"], text["ref"]); err != nil {
		return s, err
	}
	lists := map[string][]string{}
	for _, key := range []string{"changed", "unknown", "unsafe"} {
		items, ok := value[key].([]any)
		if !ok {
			return s, valueError{fmt.Sprintf("Invalid %s paths", key)}
		}
		paths := make([]string, 0, len(items))
		for _, item := range items {
			p, ok := item.(string)
			if !ok || p == "" || strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") {
				return s, valueError{fmt.Sprintf("Invalid %s paths", key)}
			}
			paths = append(paths, p)
		}
		canonical := slices.Clone(paths)
		sort.Strings(canonical)
		if !slices.Equal(paths, slices.Compact(canonical)) {
			return s, valueError{fmt.Sprintf("Unordered or duplicated %s paths", key)}
		}
		lists[key] = paths
	}
	if !scopeReasons[text["reason"]] {
		return s, valueError{"Invalid selection reason"}
	}
	if text["reason"] == "paths" && (len(lists["changed"]) == 0 || text["base"] == "") {
		return s, valueError{"Path selection requires a base and nonempty diff"}
	}
	if text["event"] == "workflow_dispatch" && text["reason"] != "dispatch" {
		return s, valueError{"Manual CI must run all checks"}
	}
	for _, p := range lists["unsafe"] {
		if !slices.Contains(lists["changed"], p) {
			return s, valueError{"Unsafe paths must belong to the diff"}
		}
	}
	jobs, ok := value["selected"].(map[string]any)
	if !ok || len(jobs) != 2 {
		return s, valueError{"Selection outputs must be booleans"}
	}
	tests, okTests := jobs["tests"].(bool)
	packages, okPackages := jobs["packages"].(bool)
	if !okTests || !okPackages {
		return s, valueError{"Selection outputs must be booleans"}
	}
	if (Jobs{tests, packages}) != selectJobs(lists["changed"], lists["unknown"], lists["unsafe"], text["reason"]) {
		return s, valueError{"Selected jobs disagree with path evidence"}
	}
	unregistered := len(lists["unknown"]) > 0
	for _, p := range lists["changed"] {
		unregistered = unregistered || Classify(p) == "unknown"
	}
	if unregistered {
		return s, valueError{"Unregistered paths require an explicit verification mapping"}
	}
	return Selection{Event: text["event"], Base: text["base"], Head: text["head"], BaseRef: text["base_ref"],
		Ref: text["ref"], Changed: lists["changed"], Unknown: lists["unknown"], Unsafe: lists["unsafe"],
		Reason: text["reason"], Selected: Jobs{tests, packages}}, nil
}

// isPythonInt reports whether json.loads would read the number as an int, not a float.
func isPythonInt(number json.Number) bool {
	return regexp.MustCompile(`^-?[0-9]+$`).MatchString(string(number))
}

// gitError is subprocess.CalledProcessError's text for a failed git command.
type gitError struct {
	args   []string
	status string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("Command '%s' %s", pyReprList(append([]string{"git"}, e.args...)), e.status)
}

func runGit(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		status := exit.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			return nil, &gitError{args, fmt.Sprintf("died with <Signals.%s: %d>.", signalName(status.Signal()), int(status.Signal()))}
		}
		return nil, &gitError{args, fmt.Sprintf("returned non-zero exit status %d.", exit.ExitCode())}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("[Errno 2] No such file or directory: 'git'")
	}
	return nil, err
}

func signalName(sig syscall.Signal) string {
	names := map[syscall.Signal]string{syscall.SIGKILL: "SIGKILL", syscall.SIGTERM: "SIGTERM",
		syscall.SIGINT: "SIGINT", syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT", syscall.SIGPIPE: "SIGPIPE"}
	if name, ok := names[sig]; ok {
		return name
	}
	return fmt.Sprintf("SIG%d", int(sig))
}

func resolveCommit(root, revision string) (string, error) {
	out, err := runGit(root, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	text, err := decodeUTF8(out)
	return strings.TrimSpace(text), err
}

// gitTree maps every path in the revision's tree to its git mode.
func gitTree(root, revision string) (map[string]string, error) {
	out, err := runGit(root, "ls-tree", "-rz", revision)
	if err != nil {
		return nil, err
	}
	entries := map[string]string{}
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		metadata, path, _ := bytes.Cut(record, []byte("\t"))
		name, err := decodeUTF8(path)
		if err != nil {
			return nil, err
		}
		entries[name] = string(bytes.Fields(metadata)[0])
	}
	return entries, nil
}

// Select computes the selection for a candidate from Git evidence.
func Select(root, base, head, event, baseRef, ref string) (Selection, error) {
	if err := scopeContext(event, baseRef, ref); err != nil {
		return Selection{}, err
	}
	candidate, err := resolveCommit(root, head)
	if err != nil {
		return Selection{}, err
	}
	inventory, err := gitTree(root, candidate)
	if err != nil {
		return Selection{}, err
	}
	before := map[string]string{}
	baseSHA := ""
	var changed []string
	reason := "paths"
	compare := func() error {
		if base == "" || base == strings.Repeat("0", 40) {
			return valueError{"No comparison base"}
		}
		sha, err := resolveCommit(root, base)
		if err != nil {
			return err
		}
		baseSHA = sha
		if before, err = gitTree(root, baseSHA); err != nil {
			return err
		}
		out, err := runGit(root, "diff", "--no-renames", "--name-only", "-z", baseSHA, candidate, "--")
		if err != nil {
			return err
		}
		text, err := decodeUTF8(out)
		if err != nil {
			return err
		}
		changed = sortedSet(strings.Split(text, "\x00"))
		if len(changed) == 0 {
			reason = "empty"
		}
		return nil
	}
	if err := compare(); err != nil {
		// ValueError (no base, undecodable output) and CalledProcessError mean no usable
		// comparison; anything else (git missing) is a failure, as in scope.py.
		if _, isGit := err.(*gitError); !isGit && !errors.Is(err, errNoValue) {
			return Selection{}, err
		}
		reason = "base-unavailable"
	}
	if event == "workflow_dispatch" {
		reason = "dispatch"
	}
	var all []string
	for path := range inventory {
		all = append(all, path)
	}
	var unknown []string
	for _, path := range sortedSet(append(all, changed...)) {
		if Classify(path) == "unknown" {
			unknown = append(unknown, path)
		}
	}
	var unsafe []string
	for _, p := range changed {
		oldMode, inBefore := before[p]
		newMode, inAfter := inventory[p]
		modeChanged := inBefore && inAfter && oldMode != newMode
		docsOdd := Classify(p) == "docs" && ((inBefore && oldMode != "100644") || (inAfter && newMode != "100644"))
		if modeChanged || docsOdd {
			unsafe = append(unsafe, p)
		}
	}
	return Selection{Event: event, Base: baseSHA, Head: candidate, BaseRef: baseRef, Ref: ref,
		Changed: changed, Unknown: unknown, Unsafe: unsafe, Reason: reason,
		Selected: selectJobs(changed, unknown, unsafe, reason)}, nil
}

// errNoValue marks a Python ValueError (including UnicodeDecodeError) raised by a helper.
var errNoValue = valueError{"ValueError"}

type valueError struct{ text string }

func (e valueError) Error() string        { return e.text }
func (e valueError) Is(target error) bool { return target == errNoValue }

func sortedSet(items []string) []string {
	var out []string
	for _, item := range items {
		if item != "" {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// Scope is `crw-dev ci scope`: print the selection and, under GitHub Actions, its outputs.
func Scope(args []string, stdout, stderr io.Writer) int {
	values, code := parseFlags("scope", "Choose CRW checks from Git evidence; an unregistered path cannot pass CI.",
		[]string{"base", "head", "event", "base-ref", "ref"}, args, stdout, stderr)
	if code >= 0 {
		return code
	}
	root, err := os.Getwd()
	if err != nil {
		return failf(stderr, "Selection failed: %s", err)
	}
	result, err := Select(root, values["base"], values["head"], values["event"], values["base-ref"], values["ref"])
	if err != nil {
		return failf(stderr, "Selection failed: %s", err)
	}
	encoded := result.JSON()
	if path := os.Getenv("GITHUB_OUTPUT"); path != "" {
		output, err := appendFile(path)
		if err != nil {
			return failf(stderr, "Selection failed: %s", pyOSErrorText(err))
		}
		fmt.Fprintf(output, "scope=%s\ntests=%t\npackages=%t\n", encoded, result.Selected.Tests, result.Selected.Packages)
		if err := output.Close(); err != nil {
			return failf(stderr, "Selection failed: %s", pyOSErrorText(err))
		}
	}
	fmt.Fprintln(stdout, encoded)
	return 0
}

// pyOSError is f"{type(error).__name__}: {error}" for an OSError.
func pyOSError(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	class := map[syscall.Errno]string{syscall.ENOENT: "FileNotFoundError", syscall.EACCES: "PermissionError",
		syscall.EPERM: "PermissionError", syscall.EISDIR: "IsADirectoryError", syscall.ENOTDIR: "NotADirectoryError",
		syscall.EAGAIN: "BlockingIOError", syscall.EINTR: "InterruptedError"}[errno]
	if class == "" {
		class = "OSError"
	}
	return class + ": " + pyOSErrorText(err)
}

// pyOSErrorText is str(OSError) for a path error: "[Errno N] Text: 'path'".
func pyOSErrorText(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	text := errno.Error()
	text = fmt.Sprintf("[Errno %d] %s%s", int(errno), strings.ToUpper(text[:1]), text[1:])
	var path *os.PathError
	if errors.As(err, &path) {
		text += ": " + pyRepr(path.Path)
	}
	return text
}

// parseFlags reads argparse-style string options ("--name value" or "--name=value", unique
// prefixes allowed, default ""). code is -1 to continue, else the exit status to return.
func parseFlags(prog, description string, names []string, args []string, stdout, stderr io.Writer) (map[string]string, int) {
	values, _, code := parseOptions(prog, description, names, nil, args, stdout, stderr)
	return values, code
}

// parseOptions is parseFlags plus store_true switches; present names every option given.
func parseOptions(prog, description string, names, switches []string, args []string, stdout, stderr io.Writer) (map[string]string, map[string]bool, int) {
	usage := "usage: crw-dev ci " + prog + " [-h]"
	for _, name := range names {
		usage += fmt.Sprintf(" [--%s %s]", name, strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
	}
	for _, name := range switches {
		usage += " [--" + name + "]"
	}
	fail := func(message string) (map[string]string, map[string]bool, int) {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "crw-dev ci %s: error: %s\n", prog, message)
		return nil, nil, 2
	}
	values := map[string]string{}
	present := map[string]bool{}
	for _, name := range names {
		values[name] = ""
	}
	all := append(slices.Clone(names), switches...)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-h" || arg == "--help" {
			fmt.Fprintf(stdout, "%s\n\n%s\n", usage, description)
			return nil, nil, 0
		}
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return fail("unrecognized arguments: " + strings.Join(args[i:], " "))
		}
		key, value, inline := strings.Cut(arg[2:], "=")
		var match []string
		for _, name := range all {
			if name == key {
				match = []string{name}
				break
			}
			if strings.HasPrefix(name, key) {
				match = append(match, name)
			}
		}
		switch {
		case len(match) == 0:
			return fail("unrecognized arguments: " + arg)
		case len(match) > 1:
			return fail(fmt.Sprintf("ambiguous option: --%s could match --%s", key, strings.Join(match, ", --")))
		}
		present[match[0]] = true
		if slices.Contains(switches, match[0]) {
			if inline {
				return fail(fmt.Sprintf("argument --%s: ignored explicit argument %s", match[0], pyRepr(value)))
			}
			continue
		}
		if !inline {
			if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-") {
				return fail(fmt.Sprintf("argument --%s: expected one argument", match[0]))
			}
			i++
			value = args[i]
		}
		values[match[0]] = value
	}
	return values, present, -1
}
