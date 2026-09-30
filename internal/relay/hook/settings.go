package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const ConfigName = "crw-completion-hook.json"

// Evidence file bounds are independent of the timed stdin read.
const maxInputBytes = 4 << 20

// configurationPath is the settings `crw hook` reads without --plugin-launch: <home>/ConfigName,
// home being given, else CODEX_HOME, else ~/.codex, with _settled's lexical normalization (never
// resolving symlinks). A settings path argument and CRW_COMPLETION_HOOK_CONFIG are no longer read
// (decision 66).
func configurationPath(home string, environ map[string]string) (string, error) {
	env := os.Getenv
	if environ != nil {
		env = func(key string) string { return environ[key] }
	}
	if home == "" {
		home = env("CODEX_HOME")
	}
	if home == "" {
		h, err := store.Home()
		if err != nil {
			return "", err
		}
		home = strings.TrimSuffix(h, "/") + "/.codex"
	}
	// Path(home) / CONFIG_NAME: a root home gains no second slash, and "//" keeps both.
	path, err := store.ExpandUser(store.PathlibChild(home, ConfigName))
	if err != nil {
		return "", err
	}
	return abspath(path)
}

// abspath is os.path.abspath (store.Abspath): a relative path joined to the working directory the
// kernel names (os.getcwd, where os.Getwd prefers a $PWD that reaches it through a symbolic link),
// then normpath, which folds ".." lexically and keeps exactly two leading slashes.
func abspath(path string) (string, error) { return store.Abspath(path) }

func seconds(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && n > 0 && n <= 86400
	case int64:
		return float64(n), n > 0 && n <= 86400
	case int:
		return float64(n), n > 0 && n <= 86400
	}
	return 0, false
}
func Complaints(value any) []string {
	o, ok := evidence.Object(value)
	if !ok {
		return []string{"the configuration is a " + evidence.TypeName(value) + ", not an object"}
	}
	found := []string{}
	version := get(o, "configVersion")
	validVersion := version == true || version == int64(1) || version == float64(1) || version == int(1)
	if !validVersion {
		found = append(found, "configVersion must be 1, found "+evidence.Repr(version))
	}
	for _, k := range []string{"relayExecutable", "markerRoot"} {
		v := get(o, k)
		if !delivery.Named(v) {
			found = append(found, k+" must be a non-empty string")
		} else if !filepath.IsAbs(text(v)) {
			found = append(found, k+" must be an absolute path, because this hook runs in the session's workspace and a relative path resolves there")
		}
	}
	for _, k := range []string{"dbPath", "socketPath"} {
		v := get(o, k)
		if v == nil {
			continue
		}
		if !delivery.Named(v) {
			found = append(found, k+" must be a non-empty string when it is present at all")
		} else if !filepath.IsAbs(text(v)) {
			found = append(found, k+" must be an absolute path")
		}
	}
	mode := text(get(o, "mode"))
	if mode != Observe && mode != Hold {
		found = append(found, "mode must be one of observe, hold")
	}
	if mode == Hold && !delivery.Named(get(o, "isolationAssertedBy")) {
		found = append(found, "holding requires isolationAssertedBy to name who established that a held child cannot write the facts the decision reads; observing requires nothing, which is why it is the default")
	}
	if p := get(o, "journalPolicy"); p != nil && !slices.Contains(JournalPolicies, text(p)) {
		found = append(found, "journalPolicy must be one of every_invocation, faults_only, no_journal")
	}
	owner := get(o, "owner")
	if owner != nil && owner != "user" && owner != "plugin" {
		found = append(found, "owner must be one of user, plugin when it is present at all, found "+evidence.Repr(owner))
	}
	// adapterInterpreter and adapterEntryPoint named the adapter the retired Python launchers ran;
	// nothing reads them, and a document that still carries them is read as it is (decision 66).
	if owner == "plugin" {
		budget, _ := seconds(get(o, "timeoutSeconds"))
		if number, ok := get(o, "timeoutSeconds").(json.Number); ok {
			budget, _ = number.Float64()
		}
		if budget > 7 {
			found = append(found, "timeoutSeconds must not exceed 7 when owner is plugin, because the packaged launcher waits the budget plus 2s capped at 9s and has to outlast the adapter it runs")
		}
	}
	if budget := get(o, "timeoutSeconds"); budget != nil {
		if _, ok := seconds(budget); !ok {
			found = append(found, "timeoutSeconds must be a positive number of seconds, at most 86400")
		}
	}
	if root := get(o, "journalRoot"); root != nil {
		if !delivery.Named(root) {
			found = append(found, "journalRoot must be a non-empty string when it is present at all")
		} else if !filepath.IsAbs(text(root)) {
			found = append(found, "journalRoot must be an absolute path")
		}
	}
	return found
}

// unbounded is readRegular's limit for a file the fence reads whole: decision 24's native
// context-aware reads keep the deadline and the regular-file check and impose no byte bound.
const unbounded = -1

// readRegular reads a regular file within ctx, refusing one longer than limit bytes unless the
// limit is unbounded.
func readRegular(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	var reader io.Reader = f
	if limit != unbounded {
		reader = io.LimitReader(f, limit+1)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit != unbounded && int64(len(raw)) > limit {
		return nil, fmt.Errorf("file exceeds read bound")
	}
	return raw, ctx.Err()
}
func ReadSettings(ctx context.Context, path string) (Object, string, string) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "config_absent", "nothing exists at " + path
	}
	if err != nil {
		return nil, "config_unreachable", "whether anything exists at " + path + " could not be established: " + store.PythonOSError(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, "config_unreadable", "the configuration at " + path + " is a symbolic link whose target does not exist"
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, "config_unreadable", "the configuration at " + path + " is a symbolic link that loops"
		}
		if err != nil {
			return nil, "config_unreachable", "the configuration at " + path + " is a symbolic link whose target could not be resolved: " + store.PythonOSError(err)
		}
	}
	if !info.Mode().IsRegular() {
		kind := "not a regular file"
		switch {
		case info.IsDir():
			kind = "directory"
		case info.Mode()&os.ModeNamedPipe != 0:
			kind = "named pipe"
		case info.Mode()&os.ModeSocket != 0:
			kind = "socket"
		case info.Mode()&os.ModeCharDevice != 0:
			kind = "character device"
		case info.Mode()&os.ModeDevice != 0:
			kind = "block device"
		}
		return nil, "config_unreadable", "the configuration at " + path + " is a " + kind + ", not a regular file"
	}
	raw, err := readRegular(ctx, path, 1<<20)
	if err != nil {
		return nil, "config_unreachable", "the configuration at " + path + " could not be read: " + store.PythonOSError(err)
	}
	v, err := Decode(raw)
	if err != nil {
		return nil, "config_unreadable", "the configuration at " + path + " could not be decoded: " + err.Error()
	}
	if wrong := Complaints(v); len(wrong) > 0 {
		return nil, "config_malformed", strings.Join(wrong, "; ")
	}
	return object(v), "", ""
}
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		expanded, err := store.ExpandUser(h)
		if err == nil {
			return expanded
		}
		return h
	}
	return defaultCodexHome()
}

// hostLedger is stopadapter.host_ledger: os.path.abspath of the Codex home, then HostLedgerParts
// joined as pathlib joins them. A home of two leading slashes keeps both, which filepath.Join
// would fold, and a root home gains no second slash.
func hostLedger() (string, error) {
	host, err := abspath(codexHome())
	if err != nil {
		return "", err
	}
	for _, part := range HostLedgerParts {
		host = store.PathlibChild(host, part)
	}
	return host, nil
}

// defaultCodexHome is Path.home() / ".codex": an empty HOME is the root and an unset one the
// passwd entry.
func defaultCodexHome() string {
	h, _ := store.Home()
	return strings.TrimSuffix(h, "/") + "/.codex"
}

// RoutingState uses the relay's selection rules, including legacy socket spellings.
// A settings-pinned DB still routes directly to the owner of that store. That dbPath is the
// settings' str, so its directory reaches the system as os.fsencode's bytes (a surrogate escape
// is the byte it stands for). A directory the environment selects is already the bytes it names:
// encoding it again would turn a literal ED B2..B3 run in it into another directory.
func RoutingState(config Object) (string, error) {
	if db := text(get(config, "dbPath")); db != "" {
		if encoded, ok := fsencode(db); ok {
			db = encoded
		}
		return filepath.Dir(db), nil
	}
	selected, err := store.ResolveStateDir("", text(get(config, "socketPath")))
	return selected.Path, err
}
