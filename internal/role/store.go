package role

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// UnusableSettingsError is a helper role store that exists but cannot be used: a file that cannot be read or is not the store's
// JSON document (Role is empty, and every role is unusable), or one role whose routing fields are not valid (Role names it, and only
// that role is unusable). The oracle read both as no override, so a spawn silently inherited the main model (CRW-1119). Nothing that
// reports it writes the store: its bytes stay as they are until the operator repairs them.
type UnusableSettingsError struct {
	Path   string
	Role   RoleName // empty for the whole store
	Reason string
}

func (e *UnusableSettingsError) Error() string {
	if e.Role == "" {
		return fmt.Sprintf("unusable helper role settings in %s: %s; correct the file, or move it away to inherit the defaults (it is left as it is)", e.Path, e.Reason)
	}
	return fmt.Sprintf("unusable helper role settings for role \"%s\" in %s: %s; run crw role helper reset %s to inherit, or correct the role in the file (it is left as it is)", e.Role, e.Path, e.Reason, e.Role)
}

// StorePath is <CRW_HOME>/subagents.json under the root rule of internal/crwconfig (CRW-1119, the common boundary of CRW-1136):
// a CRW_HOME that is set and not empty is used as written, white space included, and must be absolute; an empty one counts as
// unset, and the store is then .crw below the host's home (host.HostHome: an empty HOME is the account home, never the working
// directory). The path is joined as raw text (crwconfig.JoinRoot), so a spelling that climbs through a link names the directory the
// kernel resolves it to.
func StorePath(env host.LookupEnv) (string, error) {
	if dir, _ := env("CRW_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", errors.New("CRW_HOME is not an absolute path; set it to the absolute directory of the helper role store, as written with no surrounding white space")
		}
		return crwconfig.JoinRoot(dir, StoreFile), nil
	}
	home, err := host.HostHome(env)
	if err != nil {
		return "", err
	}
	return home.Join(".crw", StoreFile), nil
}

// storeEarlierPath is where the oracle's reading put the store: a trimmed CRW_HOME, or .crw below os.homedir(), which is the
// working directory for an empty HOME, joined with lexical cleaning. A store found there while the current place has none is
// reported, never read or moved (CRW-1119).
func storeEarlierPath(env host.LookupEnv) string {
	dir, _ := env("CRW_HOME")
	if dir = text.Trim(dir); dir == "" {
		var err error
		if dir, err = host.CRWHome(env); err != nil {
			return ""
		}
	}
	path, err := filepath.Abs(filepath.Join(dir, StoreFile))
	if err != nil {
		return ""
	}
	return path
}

// rawConfig is the oracle's RawConfig: the document, whose "roles" member is the parsed roles object.
type rawConfig struct{ doc, roles *object }

func parseConfig(data []byte) (rawConfig, error) {
	doc, err := parseObject(data)
	if errors.Is(err, errNotObject) {
		return rawConfig{}, errors.New("config must be an object")
	} else if err != nil {
		return rawConfig{}, err
	}
	roles := &object{}
	if raw, ok := doc.raw("roles"); ok {
		if *roles, err = parseObject(raw); err != nil {
			return rawConfig{}, errors.New("roles must be an object")
		}
	}
	doc.set("roles", roles) // keeps its place, or goes last when the file has none
	return rawConfig{&doc, roles}, nil
}

// readRaw reads the store. Only a missing store is the empty config. Anything else that cannot be read or parsed is an error: for a
// read an UnusableSettingsError (CRW-1119; the oracle read it as the empty config), for a write (forWrite) the refusal "cannot update
// subagent config", so a store nobody can understand is never overwritten. A missing store while the place an earlier reading used
// holds one is unusable too: the operator's settings would otherwise vanish without a word.
func readRaw(env host.LookupEnv, path string, forWrite bool) (rawConfig, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var raw rawConfig
		if raw, err = parseConfig(data); err == nil {
			return raw, nil
		}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		if forWrite {
			return rawConfig{}, fmt.Errorf("cannot update subagent config: %w", err)
		}
		return rawConfig{}, &UnusableSettingsError{Path: path, Reason: err.Error()}
	}
	if earlier := storeEarlierPath(env); !forWrite && earlier != "" && earlier != path {
		if _, statErr := os.Stat(earlier); statErr == nil {
			return rawConfig{}, &UnusableSettingsError{Path: path, Reason: "the store is missing here, but " + earlier +
				" holds one from an earlier reading of CRW_HOME or HOME; move it to " + path + " or set CRW_HOME to its directory (nothing reads or moves it automatically)"}
		}
	}
	empty, _ := parseConfig([]byte("{}"))
	return empty, nil
}

// readSettings is the effective settings of every usable role, the reason each unusable role cannot be used, and the error of a
// store that cannot be used at all. A role the file holds is read field by field (parseRole); every other role is at its default
// and comes from the session. It never changes the store.
func readSettings(env host.LookupEnv) (Settings, map[RoleName]error, error) {
	path, err := StorePath(env)
	if err != nil {
		return Settings{}, nil, err
	}
	raw, err := readRaw(env, path, false)
	if err != nil {
		return Settings{}, nil, err
	}
	s := Settings{Roles: RoleMap[RoleConfig]{}, Scope: ScopeGlobal, Sources: RoleMap[ConfigSource]{}, Overrides: RoleMap[bool]{}}
	unusable := map[RoleName]error{}
	for _, role := range Roles() {
		s.Roles[role], s.Sources[role], s.Overrides[role] = DefaultRole(), SourceSession, false
		if value, ok := raw.roles.raw(string(role)); ok {
			cfg, reason := parseRole(value)
			if reason != "" {
				unusable[role] = &UnusableSettingsError{Path: path, Role: role, Reason: reason}
				continue
			}
			s.Roles[role], s.Sources[role], s.Overrides[role] = cfg, SourceGlobal, true
		}
	}
	return s, unusable, nil
}

// ReadSettings is the effective settings. A store that cannot be used, or one role whose routing fields are not valid, is an
// UnusableSettingsError (for several roles, one per role in Roles order, joined), so a caller that shows the settings shows why they
// cannot be used. It never changes the store.
func ReadSettings(env host.LookupEnv) (Settings, error) {
	s, unusable, err := readSettings(env)
	if err != nil {
		return Settings{}, err
	}
	if err := joinUnusable(unusable); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func joinUnusable(unusable map[RoleName]error) error {
	var errs []error
	for _, role := range Roles() {
		if err := unusable[role]; err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// readRole is one role's settings: the store's error, that role's error, or the role. Another role's error does not stop it, so an
// unusable role only stops its own routing (CRW-1119).
func readRole(env host.LookupEnv, role RoleName) (RoleConfig, error) {
	s, unusable, err := readSettings(env)
	if err != nil {
		return RoleConfig{}, err
	}
	if err := unusable[role]; err != nil {
		return RoleConfig{}, err
	}
	return s.Roles[role], nil
}

func ReadConfig(env host.LookupEnv) (Config, error) {
	s, err := ReadSettings(env)
	return Config{Roles: s.Roles}, err
}

// writeRaw publishes the document: a 0600 temporary file beside the store, renamed over it, so a reader sees the old file or the new
// one whole. The temporary file is removed whatever happens.
func writeRaw(path string, doc *object, rename func(tmp, finalPath string) error) (err error) {
	body, err := Stringify(doc, "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.Remove(f.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = rmErr
		}
	}()
	_, writeErr := f.Write(append(body, '\n'))
	if err = errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return rename(f.Name(), path)
}

// open is what a write starts with: the role checked, the store's path, the store's lock held (the caller releases it when the write
// is done, whatever happens) and the store's parsed content.
func open(env host.LookupEnv, role RoleName, sleep func(time.Duration)) (string, rawConfig, func(), error) {
	if !validRole(role) {
		return "", rawConfig{}, nil, fmt.Errorf("unknown role \"%s\"", role)
	}
	path, err := StorePath(env)
	if err != nil {
		return "", rawConfig{}, nil, err
	}
	release, err := lockStore(path, sleep)
	if err != nil {
		return "", rawConfig{}, nil, err
	}
	raw, err := readRaw(env, path, true)
	if err != nil {
		release()
		return "", rawConfig{}, nil, err
	}
	return path, raw, release, nil
}

// SetRole merges patch into one role and writes the store, keeping every member it does not own, then returns the effective config.
// The patch merges into the role the file holds (normalised) or the default, and the merged role must validate.
func SetRole(env host.LookupEnv, role RoleName, patch RolePatch) (Config, error) {
	return setRole(env, role, patch, crwdir.Rename, time.Sleep)
}

// setRole is SetRole with the publishing rename and the lock's sleep as seams, so a test can hold a writer inside its critical section.
func setRole(env host.LookupEnv, role RoleName, patch RolePatch, rename func(tmp, finalPath string) error, sleep func(time.Duration)) (Config, error) {
	path, raw, release, err := open(env, role, sleep)
	if err != nil {
		return Config{}, err
	}
	defer release()
	// A set merges into what the file holds, so a role whose stored routing is not usable would be merged into a normalised copy
	// and lose what the operator wrote; it is refused, and so is a set while another role is unusable, since the answer is the
	// settings after the write. A reset is the repair (CRW-1119).
	for _, r := range Roles() {
		if value, ok := raw.roles.raw(string(r)); ok {
			if _, reason := parseRole(value); reason != "" {
				return Config{}, fmt.Errorf("cannot update subagent config: %w", &UnusableSettingsError{Path: path, Role: r, Reason: reason})
			}
		}
	}
	current, existing := DefaultRole(), object(nil)
	if value, ok := raw.roles.raw(string(role)); ok {
		current, _ = parseRole(value)
		existing, _ = parseObject(value)
	}
	if err := Validate(RolePatch{Fallback: patch.Fallback}); err != nil {
		return Config{}, err
	}
	base := current.patch()
	next := RolePatch{Mode: pick(base.Mode, patch.Mode), Model: pick(base.Model, patch.Model), Effort: pick(base.Effort, patch.Effort),
		PromptOverride: pick(base.PromptOverride, patch.PromptOverride), Fallback: fallbackOpt(mergeFallback(current.Fallback, patch.Fallback))}
	if err := Validate(next); err != nil {
		return Config{}, err
	}
	cfg := next.config()
	if cfg.Mode == ModeDefault {
		cfg.Model = nil
	}
	for _, m := range []member{{"mode", cfg.Mode}, {"model", cfg.Model}, {"effort", cfg.Effort}, {"promptOverride", cfg.PromptOverride}, {"fallback", cfg.Fallback}} {
		existing.set(m.key, m.value)
	}
	raw.roles.set(string(role), &existing)
	if err := writeRaw(path, raw.doc, rename); err != nil {
		return Config{}, err
	}
	return ReadConfig(env)
}

// ResetRole removes a role's override, so it inherits again, and returns the effective config; a role the store does not hold
// writes nothing.
func ResetRole(env host.LookupEnv, role RoleName) (Config, error) {
	return resetRole(env, role, crwdir.Rename, time.Sleep)
}

// resetRole is ResetRole with the same seams as setRole.
func resetRole(env host.LookupEnv, role RoleName, rename func(tmp, finalPath string) error, sleep func(time.Duration)) (Config, error) {
	path, raw, release, err := open(env, role, sleep)
	if err != nil {
		return Config{}, err
	}
	defer release()
	if raw.roles.remove(string(role)) {
		if err := writeRaw(path, raw.doc, rename); err != nil {
			return Config{}, err
		}
	}
	return ReadConfig(env)
}

// SettingsSnapshot is one read of the store: the settings of every usable role, the error of each unusable role, and the error of a
// store that cannot be used at all. A spawn hook reads one per event and asks it about the roles it routes, so an unusable role
// stops only its own routing (CRW-1119).
type SettingsSnapshot struct {
	settings Settings
	unusable map[RoleName]error
	err      error
}

// ReadSettingsSnapshot reads the store once.
func ReadSettingsSnapshot(env host.LookupEnv) SettingsSnapshot {
	s, unusable, err := readSettings(env)
	return SettingsSnapshot{settings: s, unusable: unusable, err: err}
}

// Err is the store's error, or the joined errors of its unusable roles, or nil.
func (s SettingsSnapshot) Err() error {
	if s.err != nil {
		return s.err
	}
	return joinUnusable(s.unusable)
}

// Role is one role's settings, or the store's error, or that role's error.
func (s SettingsSnapshot) Role(role RoleName) (RoleConfig, error) {
	if !validRole(role) {
		return RoleConfig{}, fmt.Errorf("unknown role \"%s\"", role)
	}
	if s.err != nil {
		return RoleConfig{}, s.err
	}
	if err := s.unusable[role]; err != nil {
		return RoleConfig{}, err
	}
	return s.settings.Roles[role], nil
}

// Resolve is ResolveSpawnConfig over the snapshot.
func (s SettingsSnapshot) Resolve(role RoleName) (SpawnResolution, error) {
	cfg, err := s.Role(role)
	if err != nil {
		return SpawnResolution{}, err
	}
	return spawnResolution(role, cfg), nil
}
