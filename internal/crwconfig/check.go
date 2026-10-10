package crwconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Level is how serious a finding is. An error is a structural fault: a product command that uses
// the root refuses to run. A warning is reported and never stops anything, so a host that is short
// of space, or that shares one directory between two uses, keeps working.
type Level string

const (
	LevelError   Level = "error"
	LevelWarning Level = "warning"
)

// The checks. Each name is the stable key of one refusal or warning.
const (
	CheckNotDirectory      = "not_directory"
	CheckNotCreatable      = "not_creatable"
	CheckNotWritable       = "not_writable"
	CheckSocketPath        = "socket_path_too_long"
	CheckStateSplit        = "state_split"
	CheckLowSpace          = "low_space"
	CheckSharedPath        = "shared_path"
	CheckUnknownFilesystem = "unknown_filesystem"
	CheckDrift             = "drift"
)

// SocketSuffixLen is the bytes a relay store directory's socket adds below relay_state:
// "/<16 hex scope>/control.sock".
const SocketSuffixLen = len("/") + 16 + len("/control.sock")

// Finding is one thing a check found about one root.
type Finding struct {
	Level   Level  `json:"level"`
	Root    string `json:"root"`
	Check   string `json:"check"`
	Message string `json:"message"`
}

// FS is what a probe learns about the file system under a path: its name (empty when it is not a
// type this package knows) and the bytes a process that is not privileged can still allocate.
type FS struct {
	Type string
	Free uint64
}

// Probe is the host reading a check makes, replaceable so a test pins each answer. The zero value
// reads the host.
type Probe struct {
	// Statfs reads the file system under an existing path.
	Statfs func(path string) (FS, error)
	// SocketLimit is the bind limit of a Unix socket path in bytes, sun_path with its NUL: 104 on
	// macOS and 108 on Linux when zero.
	SocketLimit int
	// Only limits the check to these roots; empty checks every root.
	Only []string
	// ConfiguredOnly limits the check to roots whose path the configuration file names, so a
	// default placement is never refused for a fault the host did not choose. The split keys of
	// relay_state are read either way.
	ConfiguredOnly bool
}

// RootStatus is what the check saw of one root.
type RootStatus struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Source     Source `json:"source"`
	Set        bool   `json:"set"`
	Exists     bool   `json:"exists"`
	Filesystem string `json:"filesystem,omitempty"`
	FreeBytes  uint64 `json:"free_bytes,omitempty"`
	// Observed is the placement already in use, when it holds data: the default location of the
	// root, or Go's own cache. Empty when there is none.
	Observed string `json:"observed,omitempty"`
}

// Report is the result of Check: the configuration file, every checked root and the findings.
type Report struct {
	Config   ReportConfig `json:"config"`
	Roots    []RootStatus `json:"roots"`
	Findings []Finding    `json:"findings"`
}

// ReportConfig is the configuration file a report was made from.
type ReportConfig struct {
	Path   string `json:"path"`
	Source Source `json:"source"`
}

// Faults are the findings that are errors.
func (r Report) Faults() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Level == LevelError {
			out = append(out, f)
		}
	}
	return out
}

// Refusal is nil when the report holds no error, else the errors in one message: the refusal a
// product command gives instead of running against a root it cannot use.
func (r Report) Refusal() error {
	faults := r.Faults()
	if len(faults) == 0 {
		return nil
	}
	parts := make([]string, len(faults))
	for i, f := range faults {
		parts[i] = fmt.Sprintf("%s: %s: %s", f.Root, f.Check, f.Message)
	}
	return errors.New(strings.Join(parts, "; "))
}

// runProbe is the probe crw config check reads the host with; a test replaces it.
var runProbe = Probe{}

// minFree is the free space below which a root warns. A cache or a worktree root needs more than a
// root that holds a few files; no root is refused for it.
var minFree = map[string]uint64{
	RootBuildCache:  4 << 30,
	RootModuleCache: 2 << 30,
	RootBuildWork:   2 << 30,
	RootWorktree:    8 << 30,
	RootCache:       2 << 30,
	RootData:        2 << 30,
}

const defaultMinFree = 1 << 30

// splitKeys are the paths keys that would point a part of the relay's durable state somewhere of
// its own. The store, its WAL and its sidecars are one bundle, moved only by relay_state.
var splitKeys = []string{"relay_store", "relay_db", "relay_wal", "relay_sidecar", "relay_sidecars"}

// relaySidecars are the files beside a store that belong to its bundle.
var relaySidecars = []string{"relay.sqlite3-wal", "relay.sqlite3-shm", "relay.sqlite3-journal", "daemon.json", "daemon.log", "control.sock"}

// Check validates the roots of a loaded configuration and reports. It reads the host and writes
// nothing: it does not create a directory it finds missing. getenv is the environment the
// configuration was resolved with, from which it reads the placement already in use.
func Check(file *File, getenv func(string) string, probe Probe) Report {
	statfs := probe.Statfs
	if statfs == nil {
		statfs = statfsHost
	}
	limit := probe.SocketLimit
	if limit == 0 {
		limit = 108
		if runtime.GOOS == "darwin" {
			limit = 104
		}
	}
	only := map[string]bool{}
	for _, name := range probe.Only {
		only[name] = true
	}
	defaults, _ := Resolve(getenv, nil)
	report := Report{Findings: []Finding{}}
	report.Config.Path, report.Config.Source = file.Path()
	add := func(level Level, root, check, format string, args ...any) {
		report.Findings = append(report.Findings, Finding{Level: level, Root: root, Check: check, Message: fmt.Sprintf(format, args...)})
	}
	var checked []RootStatus
	for _, name := range RootNames() {
		if len(only) > 0 && !only[name] {
			continue
		}
		root := file.roots[name]
		if probe.ConfiguredOnly && root.Source != SourceConfig {
			if name == RootRelayState {
				checkRelayKeys(file, add)
			}
			continue
		}
		status := RootStatus{Name: name, Path: root.Path, Source: root.Source, Set: root.Set()}
		observed := observedPlacement(name, defaults[name], getenv)
		if !root.Set() {
			status.Observed = observed
			checked = append(checked, status)
			continue
		}
		at, exists := existingPart(root.Path)
		status.Exists = exists && at == root.Path
		checkUsable(root, name, at, exists, add)
		if exists {
			info, err := statfs(at)
			switch {
			case err != nil || info.Type == "":
				add(LevelWarning, name, CheckUnknownFilesystem, "the file system under %s is not one this check knows, so its space and behaviour are unchecked", at)
				if err == nil {
					status.FreeBytes = info.Free
				}
			default:
				status.Filesystem, status.FreeBytes = info.Type, info.Free
			}
			if err == nil {
				want := minFree[name]
				if want == 0 {
					want = defaultMinFree
				}
				if info.Free < want {
					add(LevelWarning, name, CheckLowSpace, "%s has %s free on its file system, below the %s this root is expected to need", at, size(info.Free), size(want))
				}
			}
		}
		checkSocket(name, root.Path, limit, add)
		if observed != "" && root.Source == SourceConfig && !samePlace(observed, root.Path) {
			status.Observed = observed
			add(LevelWarning, name, CheckDrift, "%s is in use at %s, but the configuration points to %s; the check moves nothing", name, observed, root.Path)
		} else if observed != "" {
			status.Observed = observed
		}
		if name == RootRelayState {
			checkRelay(file, root.Path, getenv, add)
		}
		checked = append(checked, status)
	}
	report.Roots = checked
	checkShared(checked, add)
	return report
}

// checkUsable judges whether the root exists as a directory this process can write, or can be made
// below an existing directory it can write. at is the root or its nearest existing ancestor.
func checkUsable(root Root, name, at string, exists bool, add func(Level, string, string, string, ...any)) {
	if !exists {
		add(LevelError, name, CheckNotCreatable, "no part of %s exists as a directory this process can reach", root.Path)
		return
	}
	info, err := os.Stat(at)
	switch {
	case err != nil:
		add(LevelError, name, CheckNotCreatable, "%s cannot be read: %v", at, err)
	case !info.IsDir() && at == root.Path:
		add(LevelError, name, CheckNotDirectory, "%s exists and is not a directory", root.Path)
	case !info.IsDir():
		add(LevelError, name, CheckNotCreatable, "%s cannot be made: %s is not a directory", root.Path, at)
	case unix.Access(at, unix.W_OK|unix.X_OK) != nil && at == root.Path:
		add(LevelError, name, CheckNotWritable, "%s is a directory this process cannot write", root.Path)
	case unix.Access(at, unix.W_OK|unix.X_OK) != nil:
		add(LevelError, name, CheckNotCreatable, "%s cannot be made: %s is not writable", root.Path, at)
	}
}

// existingPart is the longest prefix of path, taken a component at a time on the raw text so a
// link followed by ".." keeps the meaning the filesystem gives it, that exists. The second result
// is false when not even "/" does.
func existingPart(path string) (string, bool) {
	for p := path; ; {
		if _, err := os.Stat(p); err == nil {
			return p, true
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			// a prefix that cannot be examined (permission) is the part that is there
			return p, true
		}
		cut := strings.LastIndex(strings.TrimRight(p, "/"), "/")
		if cut < 0 {
			return "", false
		}
		if cut == 0 {
			p = "/"
			if _, err := os.Stat(p); err != nil {
				return "", false
			}
			return p, true
		}
		p = p[:cut]
	}
}

// checkSocket holds a root that carries a Unix socket to the bind limit, counting the bytes the
// socket's own name adds. The relay's store directory is known and makes the refusal; a test root
// holds sockets of names this package does not know, and is warned about the same suffix.
func checkSocket(name, path string, limit int, add func(Level, string, string, string, ...any)) {
	long := len(strings.TrimRight(path, "/")) + SocketSuffixLen
	switch name {
	case RootRelayState:
		if long >= limit {
			add(LevelError, name, CheckSocketPath, "a store socket would be %d bytes (%s plus /<scope>/control.sock), but a Unix socket path must be under %d", long, path, limit)
		}
	case RootTemp, RootScratch:
		if long >= limit {
			add(LevelWarning, name, CheckSocketPath, "a socket made below this root would be at least %d bytes (%s plus a 16-hex directory and a file name), but a Unix socket path must be under %d; a test that binds a socket there fails", long, path, limit)
		}
	}
}

// checkRelay holds the relay's durable state together: no paths key points a part of it apart, no
// store in the root has a WAL or sidecar that lives in another directory, and an environment
// variable that names one store directory is reported as taking the place of the configuration.
func checkRelay(file *File, path string, getenv func(string) string, add func(Level, string, string, string, ...any)) {
	checkRelayKeys(file, add)
	if value := getenv("CODEX_SESSION_RELAY_STATE"); value != "" && file.roots[RootRelayState].Source == SourceConfig {
		add(LevelWarning, RootRelayState, CheckDrift, "CODEX_SESSION_RELAY_STATE=%s names the store directory and takes the place of paths.%s while it is set", value, RootRelayState)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	for _, entry := range entries {
		dir := filepath.Join(path, entry.Name())
		db := filepath.Join(dir, "relay.sqlite3")
		if _, err := os.Lstat(db); err != nil {
			continue
		}
		realDB, err := filepath.EvalSymlinks(db)
		if err != nil {
			continue
		}
		home := filepath.Dir(realDB)
		for _, name := range relaySidecars {
			side := filepath.Join(dir, name)
			if _, err := os.Lstat(side); err != nil {
				continue
			}
			realSide, err := filepath.EvalSymlinks(side)
			if err != nil {
				continue
			}
			if filepath.Dir(realSide) != home {
				add(LevelError, RootRelayState, CheckStateSplit, "%s is kept in %s, but its store is in %s; the store, its WAL and its sidecars must stay together", side, filepath.Dir(realSide), home)
			}
		}
	}
}

// checkRelayKeys refuses a paths key that points a part of the relay's state apart.
func checkRelayKeys(file *File, add func(Level, string, string, string, ...any)) {
	for _, key := range splitKeys {
		if value, ok := file.paths[key]; ok {
			add(LevelError, RootRelayState, CheckStateSplit, "paths.%s=%q points part of the relay state apart; the store, its WAL and its sidecars are one bundle moved only by paths.%s", key, value, RootRelayState)
		}
	}
}

// Refusal checks the roots this file's paths section names, limited to only (every root when none
// is given), and returns the structural faults as one error; warnings never refuse. A product
// command calls it for the roots it is about to use, and starts nothing against a root it cannot.
func (f *File) Refusal(getenv func(string) string, only ...string) error {
	return Check(f, getenv, Probe{Only: only, ConfiguredOnly: true}).Refusal()
}

// checkShared warns when two uses name one directory.
func checkShared(roots []RootStatus, add func(Level, string, string, string, ...any)) {
	seen := map[string]string{}
	for _, status := range roots {
		if !status.Set {
			continue
		}
		key := canonical(status.Path)
		if first, ok := seen[key]; ok {
			add(LevelWarning, status.Name, CheckSharedPath, "%s and %s both name %s", first, status.Name, status.Path)
			continue
		}
		seen[key] = status.Name
	}
}

// canonical is a path with its links resolved when it exists, else cleaned.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// samePlace reports whether two paths name one directory.
func samePlace(a, b string) bool {
	if canonical(a) == canonical(b) {
		return true
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	return aerr == nil && berr == nil && os.SameFile(ai, bi)
}

// observedPlacement is the directory already in use for a root, if it holds data: the root's
// default location, and for the Go caches Go's own default or its environment variable. A
// directory that is missing or empty is not a placement in use.
func observedPlacement(name string, def Root, getenv func(string) string) string {
	candidate := def.Path
	switch name {
	case RootBuildCache:
		candidate = getenv("GOCACHE")
		if candidate == "" {
			if runtime.GOOS == "darwin" {
				candidate = rootJoin(getenv("HOME"), "Library", "Caches", "go-build")
			} else {
				base, _ := baseDir(getenv, "XDG_CACHE_HOME", ".cache")
				candidate = rootJoin(base, "go-build")
			}
		}
	case RootModuleCache:
		candidate = getenv("GOMODCACHE")
		if candidate == "" {
			gopath := getenv("GOPATH")
			if first := filepath.SplitList(gopath); len(first) > 0 && first[0] != "" {
				candidate = rootJoin(first[0], "pkg", "mod")
			} else {
				candidate = rootJoin(getenv("HOME"), "go", "pkg", "mod")
			}
		}
	case RootBuildWork:
		return ""
	}
	if candidate == "" || !filepath.IsAbs(candidate) || !holdsData(candidate) {
		return ""
	}
	return candidate
}

// holdsData reports whether path is a directory with at least one entry.
func holdsData(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

// size spells a byte count in binary units.
func size(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}
