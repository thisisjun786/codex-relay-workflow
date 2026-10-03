package role

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// StorePath is <CRW_HOME>/subagents.json. An explicit CRW_HOME is trimmed, as the oracle trims CODEXCLAW_HOME (cxcHome); a blank
// one falls back to host.CRWHome, whose default home is not trimmed.
func StorePath(env host.LookupEnv) (string, error) {
	dir, _ := env("CRW_HOME")
	if dir = text.Trim(dir); dir == "" {
		var err error
		if dir, err = host.CRWHome(env); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, StoreFile), nil
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

// readRaw reads the store. A read gets an empty config for a missing or unusable file; a write (forWrite) only for a missing one,
// and an error for anything else, so a store it cannot understand is never overwritten.
func readRaw(path string, forWrite bool) (rawConfig, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var raw rawConfig
		if raw, err = parseConfig(data); err == nil {
			return raw, nil
		}
	}
	if forWrite && !errors.Is(err, fs.ErrNotExist) {
		return rawConfig{}, fmt.Errorf("cannot update subagent config: %w", err)
	}
	empty, _ := parseConfig([]byte("{}"))
	return empty, nil
}

// ReadSettings is the effective settings: a role the file holds is read field by field, every other role is at its default and comes
// from the session. It never changes the store and never fails on its content.
func ReadSettings(env host.LookupEnv) (Settings, error) {
	path, err := StorePath(env)
	if err != nil {
		return Settings{}, err
	}
	raw, _ := readRaw(path, false)
	s := Settings{Roles: RoleMap[RoleConfig]{}, Scope: ScopeGlobal, Sources: RoleMap[ConfigSource]{}, Overrides: RoleMap[bool]{}}
	for _, role := range Roles() {
		s.Roles[role], s.Sources[role], s.Overrides[role] = DefaultRole(), SourceSession, false
		if value, ok := raw.roles.raw(string(role)); ok {
			s.Roles[role], s.Sources[role], s.Overrides[role] = reconstructRole(value), SourceGlobal, true
		}
	}
	return s, nil
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

// open is what a write starts with: the role checked, the store's path and its parsed content.
func open(env host.LookupEnv, role RoleName) (string, rawConfig, error) {
	if !validRole(role) {
		return "", rawConfig{}, fmt.Errorf("unknown role \"%s\"", role)
	}
	path, err := StorePath(env)
	if err != nil {
		return "", rawConfig{}, err
	}
	raw, err := readRaw(path, true)
	return path, raw, err
}

// SetRole merges patch into one role and writes the store, keeping every member it does not own, then returns the effective config.
// The patch merges into the role the file holds (normalised) or the default, and the merged role must validate.
func SetRole(env host.LookupEnv, role RoleName, patch RolePatch) (Config, error) {
	path, raw, err := open(env, role)
	if err != nil {
		return Config{}, err
	}
	current, existing := DefaultRole(), object(nil)
	if value, ok := raw.roles.raw(string(role)); ok {
		current = reconstructRole(value)
		existing, _ = parseObject(value) // a stored value that is not an object is replaced (I6)
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
	if err := writeRaw(path, raw.doc, crwdir.Rename); err != nil {
		return Config{}, err
	}
	return ReadConfig(env)
}

// ResetRole removes a role's override, so it inherits again, and returns the effective config; a role the store does not hold
// writes nothing.
func ResetRole(env host.LookupEnv, role RoleName) (Config, error) {
	path, raw, err := open(env, role)
	if err != nil {
		return Config{}, err
	}
	if raw.roles.remove(string(role)) {
		if err := writeRaw(path, raw.doc, crwdir.Rename); err != nil {
			return Config{}, err
		}
	}
	return ReadConfig(env)
}
