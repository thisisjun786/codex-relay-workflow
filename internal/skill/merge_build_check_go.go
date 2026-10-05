package skill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// mergeBuildPackage is one Go package of the merged tree the check builds for one target. dir is its
// pattern relative to the module root, tags the build tag it needs ("" for none), darwin says which
// target it was listed for, lib and tests say whether it has files that build into the package and
// files that test it, and steps records what ran.
type mergeBuildPackage struct {
	path, dir, tags string
	darwin          bool
	lib, tests      bool
	steps           []string
}

// mergeBuildListed is the part of one go list -e -json answer the classification reads.
type mergeBuildListed struct {
	Match          []string
	ImportPath     string
	GoFiles        []string
	CgoFiles       []string
	TestGoFiles    []string
	XTestGoFiles   []string
	IgnoredGoFiles []string
	Error          *struct{ Err string }
}

func (l mergeBuildListed) dir() string {
	if len(l.Match) == 0 {
		return l.ImportPath
	}
	return strings.TrimPrefix(l.Match[0], "./")
}
func (l mergeBuildListed) lib() bool   { return len(l.GoFiles)+len(l.CgoFiles) > 0 }
func (l mergeBuildListed) tests() bool { return len(l.TestGoFiles)+len(l.XTestGoFiles) > 0 }
func (l mergeBuildListed) files() bool { return l.lib() || l.tests() }

// mergeBuildSteps are the four commands, in the order they run. The check stops at the first one
// that fails: a build that does not compile makes the same errors in vet. The darwin step runs on
// the packages as listed for darwin, the others on the packages as listed for the host.
var mergeBuildSteps = []struct {
	name, code string
	darwin     bool
	applies    func(*mergeBuildPackage) bool
}{
	{"build", "build_failed", false, func(p *mergeBuildPackage) bool { return p.lib }},
	{"vet", "vet_failed", false, func(p *mergeBuildPackage) bool { return p.lib || p.tests }},
	{"test_compile", "test_compile_failed", false, func(p *mergeBuildPackage) bool { return p.tests }},
	{"vet_darwin", "vet_darwin_failed", true, func(p *mergeBuildPackage) bool { return p.lib || p.tests }},
}

// mergeBuild runs the go tool on the extracted merged tree.
type mergeBuild struct {
	goTool, work, tags string
	parallel           int
	env                []string
}

// mergeBuildEnv is the go tool's environment: the caller's, except that what the tool writes outside
// the build cache goes into a directory this run owns and removes: its temporary files (TMPDIR,
// GOTMPDIR) and its telemetry counters, which say telemetry is off at every mode file that applies.
// The owned config directory is where a user config directory that follows XDG_CONFIG_HOME reads the
// mode; on darwin os.UserConfigDir reads $HOME/Library/Application Support and ignores
// XDG_CONFIG_HOME, so HOME moves into the owned directory there as well and the mode file is written
// under that home too, before any go command can start in it. The caches the caller's go environment
// names (GOCACHE, GOMODCACHE, GOPATH) are read with the caller's environment, in an owned directory
// that holds no go.mod so a module in the caller's working directory cannot choose the go tool's
// toolchain, and named again, so builds still share the caller's caches. Elsewhere HOME, the build cache, the module cache and every setting stay as
// the caller has them, and GOENV is pinned to the caller's file first, so the settings of go env -w
// keep their place between the environment and the defaults. The module workspace is off. targetOS
// names the platform the go tool targets and defaults to runtime.GOOS; a test names darwin and linux
// to exercise both on any host. ctx is the check's own deadline: what this environment prepares runs
// under it, so nothing it starts can outlast the check.
func mergeBuildEnv(ctx context.Context, base []string, owned string, targetOS ...string) ([]string, error) {
	goos := runtime.GOOS
	if len(targetOS) > 0 {
		goos = targetOS[0]
	}
	env := append([]string{}, base...)
	switch goenv := os.Getenv("GOENV"); goenv {
	case "off":
	case "":
		if config, err := os.UserConfigDir(); err == nil {
			env = append(env, "GOENV="+filepath.Join(config, "go", "env"))
		}
	default:
		// the go tool runs in another directory, where a relative name would be another file
		if abs, err := filepath.Abs(goenv); err == nil {
			env = append(env, "GOENV="+abs)
		}
	}
	config := filepath.Join(owned, "config")
	tmp := filepath.Join(owned, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	mode := []byte("off " + time.Now().UTC().Format("2006-01-02"))
	for _, file := range mergeBuildTelemetryModeFiles(owned, goos) {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(file, mode, 0o600); err != nil {
			return nil, err
		}
	}
	env = append(env, "GOWORK=off", "XDG_CONFIG_HOME="+config, "TMPDIR="+tmp, "GOTMPDIR="+tmp)
	if goos == "darwin" {
		home := filepath.Join(owned, "home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, err
		}
		// The read must not run with the caller's HOME either: every go command started there
		// initializes telemetry under it, and on darwin the telemetry directory is the one under
		// HOME. So it runs with the owned HOME, and a value that lands under that home is the
		// caller's own default with the home prefix swapped, which is swapped back here.
		probe := mergeBuildProbeDir(owned)
		if err := os.MkdirAll(probe, 0o700); err != nil {
			return nil, err
		}
		readEnv := append(append([]string{}, env...), "HOME="+home)
		caches, err := mergeBuildCallerCaches(ctx, readEnv, probe)
		if err != nil {
			return nil, err
		}
		callerHome := mergeBuildEnvValue(base, "HOME")
		if callerHome == "" {
			callerHome = os.Getenv("HOME")
		}
		for i, value := range caches {
			caches[i] = mergeBuildCallerPath(value, home, callerHome)
		}
		env = append(env, "HOME="+home, "GOCACHE="+caches[0], "GOMODCACHE="+caches[1], "GOPATH="+caches[2])
	}
	return env, nil
}

// mergeBuildTelemetryModeFiles are the mode files that say telemetry is off for the go tool: the one
// under the owned config directory, which is where a user config directory that follows
// XDG_CONFIG_HOME reads it, and on darwin the one under the owned home, which is where
// os.UserConfigDir reads the mode ($HOME/Library/Application Support) once HOME has moved there.
func mergeBuildTelemetryModeFiles(owned, goos string) []string {
	files := []string{filepath.Join(owned, "config", "go", "telemetry", "mode")}
	if goos == "darwin" {
		files = append(files, filepath.Join(owned, "home", "Library", "Application Support", "go", "telemetry", "mode"))
	}
	return files
}

// mergeBuildProbeDir is the directory the caller-cache read runs in: a directory this run owns that
// holds no go.mod, so a module the caller's working directory holds cannot choose the go tool's
// toolchain for it.
func mergeBuildProbeDir(owned string) string {
	return filepath.Join(owned, "probe")
}

// mergeBuildEnvValue is what an environment slice gives a key: of duplicated names the last value,
// as the go tool reads them.
func mergeBuildEnvValue(env []string, key string) string {
	value := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value = v
		}
	}
	return value
}

// mergeBuildCallerPath turns a value computed for one home into the value for another: a path that
// landed inside the owned home is the caller's default under that home, so its prefix becomes the
// caller's home; anything else, which is what the caller's environment or settings file names, is
// itself.
func mergeBuildCallerPath(value, ownedHome, callerHome string) string {
	if callerHome == "" {
		return value
	}
	rest, ok := strings.CutPrefix(value, ownedHome+string(filepath.Separator))
	if !ok {
		return value
	}
	return filepath.Join(callerHome, rest)
}

// mergeBuildCallerCaches reads the build cache, the module cache and GOPATH the caller's go
// environment names, so that moving HOME on darwin does not move the caches away from the caller.
// It runs in the directory dir, which the caller owns and which holds no go.mod, and in a process
// group of its own that the check's context ends as a whole, so a go env that stalls is cut off with
// the check instead of keeping it running past its timeout. The caller has already pointed the
// environment it passes at the run's own home, whose go defaults are the caller's own with the home
// prefix changed; the caller swaps that prefix back.
func mergeBuildCallerCaches(ctx context.Context, base []string, dir string) ([3]string, error) {
	var caches [3]string
	goTool, err := exec.LookPath("go")
	if err != nil {
		return caches, err
	}
	args := append([]string{"env", "-json"}, mergeBuildCacheKeys[:]...)
	cmd := exec.CommandContext(ctx, goTool, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{}, base...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return caches, err
	}
	return mergeBuildDecodeCaches(out)
}

// mergeBuildCacheKeys are the names the cache read asks for, in the order the values are kept.
var mergeBuildCacheKeys = [3]string{"GOCACHE", "GOMODCACHE", "GOPATH"}

// mergeBuildDecodeCaches reads what the cache read's go env -json command answered: the value of
// every name in mergeBuildCacheKeys, each kept exactly as the answer gives it.
func mergeBuildDecodeCaches(out []byte) ([3]string, error) {
	var caches [3]string
	values := map[string]string{}
	if err := json.Unmarshal(out, &values); err != nil {
		return caches, fmt.Errorf("go env -json wrote an answer this check cannot read: %w", err)
	}
	for i, key := range mergeBuildCacheKeys {
		value, ok := values[key]
		if !ok {
			return caches, fmt.Errorf("go env -json did not answer %s", key)
		}
		caches[i] = value
	}
	return caches, nil
}

// mergeBuildTagFlag is how a command line names a tag set. A command-line flag wins over GOFLAGS and
// over the pinned GOENV file, so the untagged pass names the empty set explicitly: without -tags= a
// -tags the caller carries would run the untagged pass tagged and it would miss what only an
// untagged build of the merge has (CRW-562).
func mergeBuildTagFlag(tags string) []string {
	if tags == "" {
		return []string{"-tags="}
	}
	return []string{"-tags", tags}
}

// run classifies the changed directories for both targets, runs the steps and reports. It returns the
// exit status, or an error when the go tool could not run to the end (a timeout, an interrupt).
func (b *mergeBuild) run(ctx context.Context, r *mergeBuildReport, dirs []string) (int, error) {
	var present []string
	for _, dir := range dirs {
		if info, err := os.Stat(filepath.Join(b.work, filepath.FromSlash(dir))); err != nil || !info.IsDir() {
			r.skipped = append(r.skipped, "skipped: "+dir+" reason=removed")
			continue
		}
		present = append(present, dir)
	}
	var targets [2][]*mergeBuildPackage
	var skips [2]map[string]string
	for i := range targets {
		var finding *mergeBuildFinding
		var err error
		if targets[i], skips[i], finding, err = b.classify(ctx, i == 1, present); err != nil {
			return 0, err
		} else if finding != nil {
			return r.refuse("list_failed", finding.detail, mergeBuildSafeFinding, "list", finding.output), nil
		}
	}
	r.skipped = append(r.skipped, mergeBuildSkips(present, targets, skips)...)
	r.checked = append(append([]*mergeBuildPackage{}, targets[0]...), targets[1]...)
	if len(r.checked) == 0 {
		return r.pass("changes no Go package that builds, so there is nothing to build"), nil
	}
	ranStep := map[string]bool{}
	for _, step := range mergeBuildSteps {
		pool := targets[0]
		if step.darwin {
			pool = targets[1]
		}
		var selected []*mergeBuildPackage
		for _, p := range pool {
			if step.applies(p) {
				selected = append(selected, p)
			}
		}
		for _, tags := range b.tagSets() {
			var group []string
			for _, p := range selected {
				if p.tags == tags {
					group = append(group, mergeBuildPattern(p.dir))
				}
			}
			if len(group) == 0 {
				continue
			}
			res, err := b.goRun(ctx, step.darwin, b.argv(step.name, tags, group)...)
			if err != nil {
				return 0, err
			}
			if !res.ok {
				named := mergeBuildNamed(res.output(), r.checked)
				detail := fmt.Sprintf("go %s failed in the merge of %s and %s", strings.Replace(step.name, "_", " ", 1), r.head[:12], r.base[:12])
				if len(named) > 0 {
					detail += " (packages named: " + strings.Join(named, ", ") + ")"
				}
				return r.refuse(step.code, detail, mergeBuildSafeFinding, step.name, res.output()), nil
			}
			ranStep[step.name] = true
			for _, p := range selected {
				if p.tags == tags {
					p.steps = append(p.steps, step.name)
				}
			}
		}
	}
	for _, step := range mergeBuildSteps {
		if ranStep[step.name] {
			r.steps = append(r.steps, step.name)
		}
	}
	return r.pass(""), nil
}

// mergeBuildSkips states, for each directory, what a target could not build: both targets (the
// directory is no package of the merge), the host only (it builds on darwin only) or darwin only.
func mergeBuildSkips(dirs []string, targets [2][]*mergeBuildPackage, skips [2]map[string]string) []string {
	var rows []string
	named := map[string]string{}
	for _, p := range append(append([]*mergeBuildPackage{}, targets[0]...), targets[1]...) {
		named[p.dir] = p.path
	}
	for _, dir := range dirs {
		host, onHost := skips[0][dir]
		_, onDarwin := skips[1][dir]
		switch {
		case onHost && onDarwin:
			rows = append(rows, "skipped: "+dir+" reason="+host)
		case onHost:
			rows = append(rows, "skipped: "+named[dir]+" steps=build,vet,test_compile reason=not_built_on_host")
		case onDarwin:
			rows = append(rows, "skipped: "+named[dir]+" step=vet_darwin reason=not_built_on_darwin")
		}
	}
	return rows
}

func (b *mergeBuild) tagSets() []string {
	if b.tags == "" {
		return []string{""}
	}
	return []string{"", b.tags}
}

func mergeBuildPattern(dir string) string {
	if dir == "." {
		return "."
	}
	return "./" + dir
}

// mergeBuildFinding is a refusal found while listing: a package that does not load.
type mergeBuildFinding struct{ detail, output string }

// classify lists the directories for one target, once under the default build and once under
// --tags, and makes a package of each directory that has files in either; the same package can be
// checked in both, because a tag can change which files of it, and of what it imports, compile. A
// directory with no file in either is skipped for a stated reason; a package that is broken (it
// lists with an error) is a finding, never a skip.
func (b *mergeBuild) classify(ctx context.Context, darwin bool, dirs []string) (pkgs []*mergeBuildPackage, skips map[string]string, finding *mergeBuildFinding, err error) {
	skips = map[string]string{}
	have, ignored := map[string]bool{}, map[string]bool{}
	if len(dirs) == 0 {
		return nil, skips, nil, nil // go list with no pattern lists the current directory
	}
	for _, tags := range b.tagSets() {
		listed, out, ok, listErr := b.list(ctx, darwin, tags, dirs)
		if listErr != nil {
			return nil, nil, nil, listErr
		}
		if !ok {
			return nil, nil, &mergeBuildFinding{"go list could not read the merged tree", out}, nil
		}
		for _, l := range listed {
			dir := l.dir()
			switch {
			case l.files() && l.Error != nil:
				return nil, nil, &mergeBuildFinding{fmt.Sprintf("%s does not load: %s", dir, l.Error.Err), l.Error.Err}, nil
			case l.files():
				have[dir] = true
				pkgs = append(pkgs, &mergeBuildPackage{path: l.ImportPath, dir: dir, tags: tags, darwin: darwin, lib: l.lib(), tests: l.tests()})
			case len(l.IgnoredGoFiles) > 0:
				ignored[dir] = true
			case l.Error != nil && !strings.HasPrefix(l.Error.Err, "no Go files"):
				return nil, nil, &mergeBuildFinding{fmt.Sprintf("%s does not load: %s", dir, l.Error.Err), l.Error.Err}, nil
			}
		}
	}
	for _, dir := range dirs {
		switch {
		case have[dir]:
		case ignored[dir]:
			skips[dir] = "excluded_by_build_constraints"
		default:
			skips[dir] = "no_go_files"
		}
	}
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].dir != pkgs[j].dir {
			return pkgs[i].dir < pkgs[j].dir
		}
		return pkgs[i].tags < pkgs[j].tags
	})
	return pkgs, skips, nil, nil
}

func (b *mergeBuild) list(ctx context.Context, darwin bool, tags string, dirs []string) ([]mergeBuildListed, string, bool, error) {
	args := []string{"list", "-e", "-p=" + strconv.Itoa(b.parallel), "-json=Match,ImportPath,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,IgnoredGoFiles,Error"}
	args = append(args, mergeBuildTagFlag(tags)...)
	for _, dir := range dirs {
		args = append(args, mergeBuildPattern(dir))
	}
	res, err := b.goRun(ctx, darwin, args...)
	if err != nil || !res.ok {
		return nil, res.output(), false, err
	}
	var listed []mergeBuildListed
	for decoder := json.NewDecoder(strings.NewReader(res.stdout)); ; {
		var l mergeBuildListed
		if err := decoder.Decode(&l); errors.Is(err, io.EOF) {
			return listed, "", true, nil
		} else if err != nil {
			return nil, "go list wrote an answer this check cannot read: " + err.Error(), false, nil
		}
		listed = append(listed, l)
	}
}

// argv is one step's go command line without the tool: the step's flags, the tags and the packages.
// A build writes nothing: -o names the null device, so a main package does not leave a binary or
// collide with a directory of its name. -exec true builds and links each test binary and then runs
// true on it, so no test or TestMain of the head runs.
func (b *mergeBuild) argv(step, tags string, patterns []string) []string {
	p := "-p=" + strconv.Itoa(b.parallel)
	var args []string
	switch step {
	case "build":
		args = []string{"build", p, "-buildvcs=false", "-o", os.DevNull}
	case "test_compile":
		args = []string{"test", p, "-count=1", "-run", "^$", "-vet=off", "-exec", "true"}
	default:
		args = []string{"vet", p}
	}
	args = append(args, mergeBuildTagFlag(tags)...)
	return append(args, patterns...)
}

// mergeBuildResult is what one go command wrote and whether it succeeded.
type mergeBuildResult struct {
	stdout, stderr string
	ok             bool
}

// output is what a person reads: the diagnostics, which go writes to standard error, then the rest.
func (r mergeBuildResult) output() string { return r.stderr + r.stdout }

// mergeBuildBuffer keeps the first quarter megabyte a command writes.
type mergeBuildBuffer struct{ buf bytes.Buffer }

func (b *mergeBuildBuffer) Write(p []byte) (int, error) {
	if room := 256<<10 - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// goRun runs the go tool in the merged tree, in a process group of its own that the context ends as
// a whole. A command that ran and failed is a result; one that could not run, or was cut off, is an error.
func (b *mergeBuild) goRun(ctx context.Context, darwin bool, args ...string) (mergeBuildResult, error) {
	cmd := exec.CommandContext(ctx, b.goTool, args...)
	cmd.Dir = b.work
	cmd.Env = append([]string{}, b.env...)
	if darwin {
		cmd.Env = append(cmd.Env, "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr mergeBuildBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := mergeBuildResult{stdout: stdout.buf.String(), stderr: stderr.buf.String(), ok: err == nil}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case err == nil || errors.As(err, &exit):
		return res, nil
	}
	return res, fmt.Errorf("go %s could not run: %w", args[0], err)
}

// mergeBuildHeader matches the line the go tool puts before a package's diagnostics: "# path",
// "# path [path.test]" or, from vet, "# [path]". mergeBuildDiagnostic matches a diagnostic itself,
// which is how vet reports a finding in the only package it vets, with no header at all.
var (
	mergeBuildHeader     = regexp.MustCompile(`(?m)^# (\S+)`)
	mergeBuildDiagnostic = regexp.MustCompile(`(?m)^(?:vet: |\t)?(\S+\.go):\d+:\d+:`)
)

// mergeBuildNamed lists the packages a go command's output names, in order: by a header line, or by
// the directory of a file it reports on when that is one of the packages checked.
func mergeBuildNamed(output string, pkgs []*mergeBuildPackage) []string {
	var named []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			named = append(named, p)
		}
	}
	byDir := map[string]string{}
	for _, p := range pkgs {
		byDir[p.dir] = p.path
	}
	for _, m := range mergeBuildHeader.FindAllStringSubmatch(output, -1) {
		add(strings.Trim(m[1], "[]"))
	}
	for _, m := range mergeBuildDiagnostic.FindAllStringSubmatch(output, -1) {
		add(byDir[path.Dir(m[1])])
	}
	return named
}
