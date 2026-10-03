// The corpus engine: what a run does whatever it runs. The Node recorder (record.go) and a Go replay
// both build the case root, the given and the observed tree through it; a Runtime supplies what
// differs. These files carry no build tag so a Go test can import them; nothing in crw does.

package cxccorpus

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Epoch is the frozen clock of a recording: the oracle's fake-clock preload starts every Node
// process here, and every given file's mtime is set to it.
func Epoch() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// umask022 is the script that runs a command under the modes the corpus was recorded with.
const umask022 = `umask 022 && exec "$0" "$@"`

// DefaultStubs are on PATH in every case unless a scenario removes one with a null stub.
var DefaultStubs = []string{"codex", "ocx", "gh", "uv", "sg"}

// gitEnv fixes git's identity, dates and configuration.
var gitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
	"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
}

// Case is one scenario's isolated tree: its root, the environment every process of the case starts
// with, and the placeholder paths the runtime bound for it.
type Case struct {
	Root string
	Env  []string
	bind []Binding
}

// Bindings are the placeholder paths of the case root itself; a runtime adds its own (the plugin
// root, the checkout, the node binary).
func (c *Case) Bindings() []Binding {
	return []Binding{
		{"${WS}", filepath.Join(c.Root, "ws")},
		{"${CODEX_HOME}", filepath.Join(c.Root, "codex")},
		{"${CXC_HOME}", filepath.Join(c.Root, "cxc")},
		{"${HOME}", filepath.Join(c.Root, "home")},
		{"${TMP}", filepath.Join(c.Root, "tmp")},
		{"${STUBS}", filepath.Join(c.Root, "stubs")},
		{"${BIN}", filepath.Join(c.Root, "bin")},
		{"${REC}", filepath.Join(c.Root, ".rec")},
		{"${ROOT}", c.Root},
	}
}

// Expand substitutes the case's placeholders into given text, argv and stdin.
func (c *Case) Expand(text string) string {
	pairs := make([]string, 0, 2*len(c.bind))
	for _, b := range c.bind {
		pairs = append(pairs, b.Placeholder, b.Path)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// NewCase makes the isolated root under scratch, with a fixed-width name (a byte count or a cut over
// an expanded path is then the same in every case): the five roots, the stub, tool and record
// directories, the git configuration, the scripted network replies and an empty call log, and the
// base environment of every process of the case (nothing is inherited). homeVar names the variable
// that points at the CXC home root. A partial Case comes back with its error, to be removed.
func NewCase(scratch, homeVar string, g Given) (*Case, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	root := filepath.Join(scratch, "cxc-rec-"+hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(root, 0o700); err != nil {
		return nil, err
	}
	c := &Case{Root: root}
	for _, dir := range append(append([]string{}, Roots...), "stubs", "bin", ".rec", ".rec/stubs") {
		if err := mkdirAll(filepath.Join(root, dir)); err != nil {
			return c, err
		}
	}
	if err := writeFile(filepath.Join(root, "gitconfig"), []byte("[user]\n\tname = fixture\n\temail = fixture@example.invalid\n[init]\n\tdefaultBranch = main\n")); err != nil {
		return c, err
	}
	if len(g.Fetch) > 0 {
		raw, _ := json.Marshal(g.Fetch)
		if err := writeFile(filepath.Join(root, ".rec", "fetch.json"), raw); err != nil {
			return c, err
		}
	}
	if err := writeFile(filepath.Join(root, ".rec", "calls.jsonl"), nil); err != nil {
		return c, err
	}
	c.Env = []string{
		"PATH=" + filepath.Join(root, "stubs") + ":" + filepath.Join(root, "bin"),
		"HOME=" + filepath.Join(root, "home"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"TZ=UTC", "LANG=C.UTF-8",
		"CODEX_HOME=" + filepath.Join(root, "codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(root, "codex"),
		homeVar + "=" + filepath.Join(root, "cxc"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "home", ".config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "home", ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(root, "home", ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "home", ".cache"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "tmp", "run"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"),
	}
	return c, nil
}

// Prepare readies the case for a scenario: the runtime's own files and variables, the placeholder
// bindings, then the fixed git identity (after the runtime's variables, the order the oracle's
// processes have always seen).
func (c *Case) Prepare(rt Runtime, s Scenario) error {
	if err := rt.Setup(c, s); err != nil {
		return err
	}
	c.bind = rt.Bindings(c)
	c.Env = append(c.Env, gitEnv...)
	return nil
}

// InstallStubs puts every stub program of the case in place: the default ones, plus or minus what
// the given says (a null stub removes one). install makes the program called name; a scripted
// stub's answer is written beside it as .rec/stubs/<name>.json, which a stub program reads, and an
// unscripted one answers 127.
func InstallStubs(c *Case, g Given, install func(name string) error) error {
	stubs := map[string]*Stub{}
	for _, name := range DefaultStubs {
		stubs[name] = &Stub{Exit: 127}
	}
	for name, stub := range g.Stubs {
		if stub == nil {
			delete(stubs, name)
			continue
		}
		stubs[name] = stub
	}
	for name, stub := range stubs {
		if err := install(name); err != nil {
			return err
		}
		if _, scripted := g.Stubs[name]; !scripted {
			continue
		}
		raw, _ := json.Marshal(stub)
		if err := writeFile(filepath.Join(c.Root, ".rec", "stubs", name+".json"), raw); err != nil {
			return err
		}
	}
	return nil
}

// mkdirAll is os.MkdirAll with mode 0755 on every directory it creates, whatever the umask.
func mkdirAll(path string) error {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil
	}
	if err := mkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// writeFile writes a file, making its directory first. A file it creates gets mode 0644 whatever the
// umask is; one that already exists keeps its mode, as with os.WriteFile.
func writeFile(path string, data []byte) error {
	if err := mkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		return os.Chmod(path, 0o644)
	}
	return nil
}

// Runtime is what differs between recording the Node oracle and replaying a Go build.
type Runtime interface {
	// Setup puts the runtime's programs, scripts and environment variables into the case.
	Setup(c *Case, s Scenario) error
	// Bindings are every placeholder path of the case: Case.Bindings plus the runtime's own.
	Bindings(c *Case) []Binding
	// Command says what to run for one step; the engine does the rest.
	Command(c *Case, s Scenario, step Step) (Invocation, error)
	// SeedSQLite creates the given databases (path to statements, placeholders already expanded).
	SeedSQLite(c *Case, seeds map[string][]string) error
	// DumpSQLite prints a database the run wrote as {"tables": [{"name", "sql", "rows"}]}.
	DumpSQLite(path string) (string, error)
	// GitPath is the real git the given's repositories are made with.
	GitPath() string
	// HookObservations is the diagnostic store left out of the default observation.
	HookObservations() string
}

// Invocation is what a runtime decides for one step. The engine expands the placeholders in Argv,
// lets the step's own stdin replace Stdin, applies the step's cwd and layers the environment: the
// case's, then Env, then the given's, then the step's, then its unset list.
type Invocation struct {
	Argv  []string
	Env   []string
	Dir   string // default: the workspace
	Stdin []byte
}

// RunOptions is how a run is carried out.
type RunOptions struct {
	Scratch string        // parent of every case root
	HomeVar string        // the variable naming the CXC home root
	Rules   *Normaliser   // normalises every output
	Timeout time.Duration // per step; a minute when zero
	Keep    bool          // keep the case root for inspection
}

// RunScenario runs a scenario once in a fresh case root and returns its normalised outcome. A case
// root that cannot be removed afterwards fails the run, so none is left behind unnoticed.
func RunScenario(rt Runtime, o RunOptions, s Scenario) (expect Expect, err error) {
	if o.Timeout == 0 {
		o.Timeout = time.Minute
	}
	c, err := NewCase(o.Scratch, o.HomeVar, s.Given)
	if c != nil && !o.Keep {
		defer func() {
			if rmErr := removeTree(c.Root); rmErr != nil && err == nil {
				err = fmt.Errorf("%s: remove case root %s: %w", s.ID, c.Root, rmErr)
			}
		}()
	}
	if err != nil {
		return Expect{}, err
	}
	if err := c.Prepare(rt, s); err != nil {
		return Expect{}, err
	}
	if err := setUp(c, s.Given, rt); err != nil {
		return Expect{}, fmt.Errorf("%s: given: %w", s.ID, err)
	}
	session := o.Rules.NewSession(c.bind)
	var results []StepResult
	for i, step := range s.Steps {
		if step.Wait != "" {
			if step.Hook != "" || step.CLI != nil || step.Node != nil || step.MCP != nil || step.Write != nil {
				return Expect{}, fmt.Errorf("%s: step %d: a wait step runs nothing else", s.ID, i)
			}
			if err := waitFor(c, step.Wait, o.Timeout); err != nil {
				return Expect{}, fmt.Errorf("%s: step %d: %w", s.ID, i, err)
			}
			results = append(results, StepResult{Action: "wait", StdoutForm: "empty"})
			continue
		}
		if step.Write != nil {
			if step.Hook != "" || step.CLI != nil || step.Node != nil || step.MCP != nil {
				return Expect{}, fmt.Errorf("%s: step %d: a write step runs nothing else", s.ID, i)
			}
			for _, rel := range sortedKeys(step.Write) {
				path, err := casePath(c, rel)
				if err == nil {
					err = writeFile(path, []byte(c.Expand(step.Write[rel])))
				}
				if err != nil {
					return Expect{}, fmt.Errorf("%s: step %d: %w", s.ID, i, err)
				}
			}
			results = append(results, StepResult{Action: "write", StdoutForm: "empty"})
			continue
		}
		raw, err := runStep(rt, o.Timeout, c, s, step)
		if err != nil {
			return Expect{}, fmt.Errorf("%s: step %d: %w", s.ID, i, err)
		}
		results = append(results, shapeStep(session, raw))
	}
	observe := s.Observe
	if len(observe) == 0 {
		observe = DefaultObserve
	}
	tree, err := observeTree(rt, c, session, observe)
	if err != nil {
		return Expect{}, fmt.Errorf("%s: observe: %w", s.ID, err)
	}
	calls, err := readCalls(c, session)
	if err != nil {
		return Expect{}, fmt.Errorf("%s: calls: %w", s.ID, err)
	}
	exit := 0
	if len(results) > 0 {
		exit = results[len(results)-1].Exit
	}
	return Expect{Exit: exit, Steps: results, Tree: tree, Calls: calls}, nil
}

// waitFor polls for a file the case-path glob names, which a detached process of an earlier step writes after that step returned.
func waitFor(c *Case, pattern string, limit time.Duration) error {
	path, err := casePath(c, pattern)
	for deadline := time.Now().Add(limit); err == nil; time.Sleep(50 * time.Millisecond) {
		if hits, _ := filepath.Glob(path); len(hits) > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			err = fmt.Errorf("no file matches %s after %s", pattern, limit)
		}
	}
	return err
}

// command is one step's process: the runtime says what to run, the engine expands the placeholders
// of argv, lets the step's own stdin text (and its stdin_pad) replace the runtime's default, applies
// the step's cwd and layers the environment. The process gets its own group, killed with the step.
func command(ctx context.Context, rt Runtime, c *Case, s Scenario, step Step) (*exec.Cmd, []byte, error) {
	inv, err := rt.Command(c, s, step)
	if err != nil {
		return nil, nil, err
	}
	if len(inv.Argv) == 0 {
		return nil, nil, errors.New("the runtime gave a step no command")
	}
	argv := append([]string{}, inv.Argv...)
	for i := range argv {
		argv[i] = c.Expand(argv[i])
	}
	dir, stdin := inv.Dir, inv.Stdin
	if dir == "" {
		dir = filepath.Join(c.Root, "ws")
	}
	if step.Stdin != nil {
		var text string
		if json.Unmarshal(step.Stdin, &text) != nil {
			var compact bytes.Buffer
			if err := json.Compact(&compact, step.Stdin); err != nil {
				return nil, nil, err
			}
			text = compact.String()
		}
		stdin = []byte(c.Expand(text))
	}
	if step.StdinPad > 0 {
		stdin = append(stdin, bytes.Repeat([]byte{' '}, step.StdinPad)...)
	}
	if step.Cwd != "" {
		path, err := casePath(c, step.Cwd)
		if err != nil {
			return nil, nil, err
		}
		dir = path
	}
	env := append(append([]string{}, c.Env...), inv.Env...)
	for _, key := range sortedKeys(s.Given.Env) {
		env = setEnv(env, key, c.Expand(s.Given.Env[key]))
	}
	for _, key := range sortedKeys(step.Env) {
		env = setEnv(env, key, c.Expand(step.Env[key]))
	}
	for _, key := range step.Unset {
		env = setEnv(env, key, "\x00unset")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, stdin, nil
}

// casePath resolves a given path, refusing one outside the scenario roots.
func casePath(c *Case, rel string) (string, error) {
	clean := filepath.Clean(rel)
	first, _, _ := strings.Cut(clean, "/")
	known := false
	for _, root := range Roots {
		known = known || first == root
	}
	if !known || strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("given path %q is not under one of %v", rel, Roots)
	}
	return filepath.Join(c.Root, clean), nil
}

// setUp writes the given state. What it creates has the modes of umask 022 whatever the process
// umask is (files and directories are chmod-ed when made, git runs under umask 022); SQLite seeding
// and the step processes are the runtime's, and so is their umask.
func setUp(c *Case, g Given, rt Runtime) error {
	for _, dir := range g.Dirs {
		path, err := casePath(c, dir)
		if err != nil {
			return err
		}
		if err := mkdirAll(path); err != nil {
			return err
		}
	}
	write := func(rel string, data []byte) error {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		return writeFile(path, data)
	}
	for _, rel := range sortedKeys(g.Files) {
		if err := write(rel, []byte(c.Expand(g.Files[rel]))); err != nil {
			return err
		}
	}
	for _, rel := range sortedKeys(g.JSON) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, g.JSON[rel]); err != nil {
			return fmt.Errorf("given json %s: %w", rel, err)
		}
		if err := write(rel, []byte(c.Expand(compact.String())+"\n")); err != nil {
			return err
		}
	}
	if len(g.SQLite) > 0 {
		seeds := map[string][]string{}
		for rel, statements := range g.SQLite {
			path, err := casePath(c, rel)
			if err != nil {
				return err
			}
			if err := mkdirAll(filepath.Dir(path)); err != nil {
				return err
			}
			for _, statement := range statements {
				seeds[path] = append(seeds[path], c.Expand(statement))
			}
		}
		if err := rt.SeedSQLite(c, seeds); err != nil {
			return err
		}
	}
	for _, rel := range sortedKeys(g.Symlinks) {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		if err := mkdirAll(filepath.Dir(path)); err != nil {
			return err
		}
		if err := os.Symlink(c.Expand(g.Symlinks[rel]), path); err != nil {
			return err
		}
	}
	if g.Git != nil {
		dir := g.Git.Dir
		if dir == "" {
			dir = "ws"
		}
		ws, err := casePath(c, dir)
		if err != nil {
			return err
		}
		if err := mkdirAll(ws); err != nil {
			return err
		}
		steps := [][]string{{"init", "-q"}}
		if g.Git.Origin != "" {
			steps = append(steps, []string{"remote", "add", "origin", g.Git.Origin})
		}
		if g.Git.Commit != "" {
			steps = append(steps, []string{"add", "-A"}, []string{"commit", "-q", "--allow-empty", "-m", g.Git.Commit})
		}
		for _, wt := range g.Git.Worktrees {
			path, err := casePath(c, wt.Path)
			if err != nil {
				return err
			}
			steps = append(steps, []string{"worktree", "add", "-q", "-b", wt.Branch, path})
		}
		for _, args := range steps {
			cmd := exec.Command("/bin/sh", append([]string{"-c", umask022, rt.GitPath()}, args...)...)
			cmd.Dir = ws
			cmd.Env = c.Env
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git %v: %w: %s", args, err, out)
			}
		}
	}
	for _, rel := range sortedKeys(g.Modes) {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		if err := os.Chmod(path, fs.FileMode(g.Modes[rel])); err != nil {
			return err
		}
	}
	// Every given entry carries the frozen clock's time, so age and staleness readings agree
	// with the oracle's Date.
	if err := freezeTimes(c.Root, Epoch(), os.Chtimes); err != nil {
		return err
	}
	for _, rel := range sortedKeys(g.Mtimes) {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		when, err := time.Parse(time.RFC3339, g.Mtimes[rel])
		if err != nil {
			return fmt.Errorf("mtime %s: %w", rel, err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			return err
		}
	}
	return nil
}

// freezeTimes gives every entry under root, symlinks apart, the time when through set (os.Chtimes
// in a run; a test passes a set that makes an entry vanish first). A walk or set error fails it.
func freezeTimes(root string, when time.Time, set func(path string, atime, mtime time.Time) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink == 0 {
			err = set(path, when, when)
		}
		return err
	})
}

// rawResult is one step before normalisation.
type rawResult struct {
	exit           int
	signal         string
	timeout        bool
	stdout, stderr string
}

func runStep(rt Runtime, timeout time.Duration, c *Case, s Scenario, step Step) (rawResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd, stdin, err := command(ctx, rt, c, s, step)
	if err != nil {
		return rawResult{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	res := rawResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() != nil {
		res.timeout = true
	}
	// Whatever the step left in its process group (a detached child, a stray stub) dies with it.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		status := exitErr.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			res.signal = status.Signal().String()
			res.exit = -1
		} else {
			res.exit = status.ExitStatus()
		}
	case res.timeout:
		res.exit = -1
	default:
		return res, err
	}
	return res, nil
}

func setEnv(env []string, key, value string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	if value == "\x00unset" {
		return out
	}
	return append(out, key+"="+value)
}

// removeTree deletes a case root, first making read-only directories writable again.
func removeTree(root string) error {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// shapeStep normalises a step and classifies its stdout (classify).
func shapeStep(s *Session, raw rawResult) StepResult {
	res := StepResult{Exit: raw.exit, Signal: raw.signal, Timeout: raw.timeout, Stderr: s.Stderr(raw.stderr)}
	out := s.Text(raw.stdout)
	form, doc, lines := classify(out)
	res.StdoutForm = form
	switch form {
	case "empty":
	case "text":
		res.Stdout = &out
	case "jsonl":
		res.StdoutJSONL = lines
	default:
		res.StdoutJSON = doc
	}
	return res
}

// classify names how a text is framed, so a fixture can hold the JSON readable and a replay can
// still rebuild the exact bytes: empty; json (one compact document, as JSON.stringify prints it)
// and json-line (the same plus a newline); json-pretty-nonl and json-pretty (two-space indented,
// as JSON.stringify(v, null, 2) prints it, without and with a newline); jsonl (two or more compact
// documents, one per line, newline-terminated); text (anything else, kept verbatim).
func classify(out string) (string, json.RawMessage, []json.RawMessage) {
	if out == "" {
		return "empty", nil, nil
	}
	body, newline := strings.CutSuffix(out, "\n")
	if json.Valid([]byte(body)) {
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(body)) == nil && compact.String() == body {
			if newline {
				return "json-line", indent(body), nil
			}
			return "json", indent(body), nil
		}
		if pretty := indent(body); string(pretty) == body {
			if newline {
				return "json-pretty", pretty, nil
			}
			return "json-pretty-nonl", pretty, nil
		}
	}
	if newline && strings.Contains(body, "\n") {
		var docs []json.RawMessage
		for _, line := range strings.Split(body, "\n") {
			var compact bytes.Buffer
			if !json.Valid([]byte(line)) || json.Compact(&compact, []byte(line)) != nil || compact.String() != line {
				return "text", nil, nil
			}
			docs = append(docs, indent(line))
		}
		return "jsonl", nil, docs
	}
	return "text", nil, nil
}

// indent re-indents a JSON document keeping its key order and escapes.
func indent(doc string) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(strings.TrimSpace(doc)), "", "  "); err != nil {
		return json.RawMessage(doc)
	}
	return json.RawMessage(buf.Bytes())
}

// observeTree records every entry under the observed roots, keyed by its normalised path.
func observeTree(rt Runtime, c *Case, s *Session, roots []string) (map[string]Entry, error) {
	hookObservations := rt.HookObservations()
	includeObservations := false
	type found struct {
		rel, key string
		path     string
		d        fs.DirEntry
	}
	var all []found
	for _, root := range roots {
		if root == hookObservations {
			includeObservations = true
		}
	}
	for _, root := range roots {
		// A root inside another listed root is walked by that one.
		nested := false
		for _, other := range roots {
			nested = nested || (other != root && strings.HasPrefix(root, other+"/"))
		}
		if nested {
			continue
		}
		if _, err := casePath(c, root); err != nil {
			return nil, fmt.Errorf("observe: %w", err)
		}
		base := filepath.Join(c.Root, root)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if path == base {
				return nil
			}
			rel, _ := filepath.Rel(c.Root, path)
			if d.IsDir() && d.Name() == ".git" {
				// Git's own files (index timestamps, object packing) are not CXC's output.
				all = append(all, found{rel: rel, path: path, d: d})
				return fs.SkipDir
			}
			if !includeObservations && (rel == hookObservations || strings.HasPrefix(rel, hookObservations+"/")) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			all = append(all, found{rel: rel, path: path, d: d})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Keys are normalised paths, and aliases are numbered in key order: sort by the path with
	// the placeholders and text rules applied (a throwaway session), so a random component does
	// not decide the numbering.
	probe := s.n.NewSession(s.bindings)
	for i := range all {
		all[i].key = probe.Text(all[i].rel)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key < all[j].key
		}
		return all[i].rel < all[j].rel
	})
	tree := map[string]Entry{}
	for _, f := range all {
		info, err := os.Lstat(f.path)
		if err != nil {
			return nil, err
		}
		key := s.Text(f.rel)
		entry := Entry{Mode: fmt.Sprintf("%04o", info.Mode().Perm())}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(f.path)
			if err != nil {
				return nil, err
			}
			entry.Type, entry.Mode, entry.Target = "symlink", "", s.Text(target)
		case info.IsDir():
			entry.Type = "dir"
		case info.Mode().IsRegular():
			entry.Type = "file"
			raw, err := os.ReadFile(f.path)
			if err != nil {
				return nil, err
			}
			if base := f.path; strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") || strings.HasSuffix(base, "-journal") {
				if _, err := os.Stat(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, "-wal"), "-shm"), "-journal")); err == nil {
					// SQLite's own sidecar files carry salts and frame counters; their
					// presence is the observation.
					entry.Form = "sqlite-sidecar"
					break
				}
			}
			if bytes.HasPrefix(raw, []byte("SQLite format 3\x00")) {
				if dump, err := rt.DumpSQLite(f.path); err == nil {
					if text := s.Text(dump); json.Valid([]byte(text)) {
						entry.Form, entry.JSON = "sqlite", indent(text)
					} else {
						entry.Form, entry.Text = "sqlite-text", &text
					}
					break
				}
			}
			fileContent(&entry, s, raw)
		default:
			entry.Type = "other"
		}
		tree[key] = entry
	}
	return tree, nil
}

func fileContent(entry *Entry, s *Session, raw []byte) {
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		sum := sha256.Sum256(raw)
		size := int64(len(raw))
		entry.Form, entry.SHA256, entry.Size = "binary", hex.EncodeToString(sum[:]), &size
		return
	}
	text := s.Text(string(raw))
	form, doc, lines := classify(text)
	entry.Form = form
	switch form {
	case "empty":
	case "text":
		entry.Text = &text
	case "jsonl":
		entry.JSONL = lines
	default:
		entry.JSON = doc
	}
}

// readCalls is the stub call log, normalised.
func readCalls(c *Case, s *Session) ([]Call, error) {
	raw, err := os.ReadFile(filepath.Join(c.Root, ".rec", "calls.jsonl"))
	if err != nil {
		return nil, err
	}
	calls := []Call{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var call Call
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			return nil, err
		}
		for i := range call.Argv {
			if call.Cmd == "ps" && i > 0 && call.Argv[i-1] == "-p" && call.Argv[i] != "" && strings.Trim(call.Argv[i], "0123456789") == "" { // a live process id, which no text rule can tell from another number
				call.Argv[i] = "<PID>"
			}
			call.Argv[i] = s.Text(call.Argv[i])
		}
		call.Cwd = s.Text(call.Cwd)
		calls = append(calls, call)
	}
	return calls, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
