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
const configEnv = "CRW_COMPLETION_HOOK_CONFIG"

// Evidence file bounds are independent of the timed stdin read.
const maxInputBytes = 4 << 20

// configurationPath follows the checkout adapter's configuration_path, including
// empty overrides and _settled's lexical normalization (never resolving symlinks).
func configurationPath(home string, environ map[string]string, named string) (string, error) {
	env := os.Getenv
	if environ != nil {
		env = func(key string) string { return environ[key] }
	}
	path := named
	if path == "" {
		path = env(configEnv)
	}
	if path == "" {
		if home == "" {
			home = env("CODEX_HOME")
		}
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			home = filepath.Join(h, ".codex")
		}
		path = home + "/" + ConfigName
	}
	path, err := store.ExpandUser(path)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	// posixpath.abspath preserves exactly two leading slashes; filepath.Abs does not.
	if strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///") {
		absolute = "/" + absolute
	}
	return absolute, err
}

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
	if p := get(o, "journalPolicy"); p != nil && !slices.Contains([]string{"every_invocation", "faults_only", "no_journal"}, text(p)) {
		found = append(found, "journalPolicy must be one of every_invocation, faults_only, no_journal")
	}
	owner := get(o, "owner")
	if owner != nil && owner != "user" && owner != "plugin" {
		found = append(found, "owner must be one of user, plugin when it is present at all, found "+evidence.Repr(owner))
	}
	for _, k := range []string{"adapterInterpreter", "adapterEntryPoint"} {
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
	if owner == "plugin" {
		for _, k := range []string{"adapterInterpreter", "adapterEntryPoint"} {
			if !evidence.Truthy(get(o, k)) {
				found = append(found, k+" is required when owner is plugin: a registration declared by the plugin package cannot resolve this repository's adapter, so the install records it here")
			}
		}
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
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
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
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// RoutingState uses the relay's selection rules, including legacy socket spellings.
// A settings-pinned DB still routes directly to the owner of that store.
func RoutingState(config Object) (string, error) {
	if db := text(get(config, "dbPath")); db != "" {
		return filepath.Dir(db), nil
	}
	selected, err := store.ResolveStateDir("", text(get(config, "socketPath")))
	return selected.Path, err
}
