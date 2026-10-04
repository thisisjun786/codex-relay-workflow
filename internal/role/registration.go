package role

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// Native registration ports subagent-config/src/role-registration.ts at CXC v0.2.40.
// Only architect/executor are registerable; explorer/reviewer definitions remain data.
// Ownership digests use Node-decoded text. Backups and mutation checks retain raw bytes
// (the oracle's decode/re-encode loses invalid UTF-8). Publication is synced, including
// the backup directory entry before replacement. A post-publication failure can leave
// complete new content visible. The mkdir lock serializes cooperative updates only.
type NativeRoleName = RoleName

//go:embed agents/*.toml
var registrationAgents embed.FS

type RegistrationResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Updated bool   `json:"updated,omitempty"`
}

func NativeRoles() []NativeRoleName { return []NativeRoleName{Architect, Executor} }

// ResolveNativeRoleHome preserves nonempty CODEX_HOME exactly, including whitespace.
func ResolveNativeRoleHome(env host.LookupEnv, userHome string) string {
	if value, _ := env("CODEX_HOME"); value != "" {
		return value
	}
	return filepath.Join(userHome, ".codex")
}

// RegisterRole is explicit registration, never invoked by a hook or dispatch.
// Omitted home resolves the process environment; an explicit blank home is refused.
func RegisterRole(role NativeRoleName, home ...string) (RegistrationResult, error) {
	return registrationRegister(role, home, nil)
}
func RegisterExecutor(home ...string) (RegistrationResult, error) {
	return RegisterRole(Executor, home...)
}
func RegisterArchitect(home ...string) (RegistrationResult, error) {
	return RegisterRole(Architect, home...)
}

type registrationStep int

const (
	registrationFileSync registrationStep = iota
	registrationBackupDirSync
	registrationAfterBackup
	registrationBeforeRename
	registrationRoleDirSync
)

func registrationRegister(role NativeRoleName, homes []string, fail func(registrationStep) error) (RegistrationResult, error) {
	if role != Architect && role != Executor {
		return RegistrationResult{}, fmt.Errorf("unsupported native role %s", registrationQuote(string(role)))
	}
	if len(homes) > 1 {
		return RegistrationResult{}, errors.New("invalid native role home: expected at most one path")
	}
	var root string
	if len(homes) == 1 {
		root = homes[0]
		if text.Trim(root) == "" {
			return RegistrationResult{}, fmt.Errorf("invalid native role home %s", registrationQuote(root))
		}
	} else {
		home, err := host.Home(os.LookupEnv)
		if err != nil {
			return RegistrationResult{}, err
		}
		root = ResolveNativeRoleHome(os.LookupEnv, home)
	}
	template := "agents/" + string(role) + ".toml"
	raw, err := registrationAgents.ReadFile(template)
	if err != nil {
		return RegistrationResult{}, err
	}
	body := registrationBody(string(raw))
	content := []byte("# crw-managed: " + registrationDigest(body) + "\n" + body)
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(root, 0777); err != nil {
		return RegistrationResult{}, err
	}
	if err := os.Mkdir(dir, 0777); err != nil && !errors.Is(err, fs.ErrExist) {
		return RegistrationResult{}, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return RegistrationResult{}, err
	}
	if !st.IsDir() || st.Mode()&fs.ModeSymlink != 0 {
		return RegistrationResult{}, fmt.Errorf("Refusing non-regular agents directory: %s", dir)
	}
	path := filepath.Join(dir, string(role)+".toml")
	prior, err := registrationRead(path)
	if err != nil {
		return RegistrationResult{}, err
	}
	decoded := source.DecodeUTF8(prior)
	if prior != nil && decoded == string(content) {
		return RegistrationResult{Path: path}, nil
	}
	if prior != nil {
		if !registrationManaged(decoded) && decoded != body {
			return RegistrationResult{}, fmt.Errorf("Existing %s role differs; preserved %s. Compare it with internal/role/%s before updating.", role, path, template)
		}
		return registrationUpdate(role, path, prior, content, fail)
	}
	return registrationCreate(path, content, fail)
}

func registrationQuote(value string) string {
	b, _ := Stringify(value, "") // a typed string is always serializable
	return string(b)
}
func registrationDigest(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
func registrationBody(template string) string {
	// The oracle removes one match only, even if it occurs inside a multiline prompt.
	pattern := regexp.MustCompile(`(?m)^model\s*=\s*"default"[^\r\n]*\r?\n`)
	if at := pattern.FindStringIndex(template); at != nil {
		return template[:at[0]] + template[at[1]:]
	}
	return template
}
func registrationManaged(value string) bool {
	match := regexp.MustCompile(`^# crw-managed: ([a-f0-9]{64})\n`).FindStringSubmatch(value)
	return match != nil && registrationDigest(value[len(match[0]):]) == match[1]
}

func registrationRead(path string) (raw []byte, err error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("Refusing non-regular role file: %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	st, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("Refusing non-regular role file: %s", path)
	}
	return io.ReadAll(f)
}
func registrationAt(fail func(registrationStep) error, step registrationStep) error {
	if fail != nil {
		return fail(step)
	}
	return nil
}
func registrationRemove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
func registrationStage(path string, data []byte, fail func(registrationStep) error) (tmp string, err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmp = f.Name()
	defer func() {
		if err != nil {
			err = errors.Join(err, registrationRemove(tmp))
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = registrationAt(fail, registrationFileSync)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	return tmp, err
}
func registrationExclusive(path string, data []byte, fail func(registrationStep) error) (err error) {
	tmp, err := registrationStage(path, data, fail)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := registrationRemove(tmp); cleanupErr != nil {
			err = cleanupErr // the oracle's finally throws cleanup errors, including after EEXIST
		}
	}()
	return os.Link(tmp, path)
}
func registrationSyncDirectory(dir string, fail func(registrationStep) error, step registrationStep) error {
	if err := registrationAt(fail, step); err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func registrationCreate(path string, content []byte, fail func(registrationStep) error) (RegistrationResult, error) {
	if err := registrationExclusive(path, content, fail); err != nil {
		if errors.Is(err, fs.ErrExist) {
			prior, readErr := registrationRead(path)
			if readErr != nil {
				return RegistrationResult{}, readErr
			}
			if prior != nil && source.DecodeUTF8(prior) == string(content) {
				return RegistrationResult{Path: path}, nil
			}
		}
		return RegistrationResult{}, err
	}
	if err := registrationSyncDirectory(filepath.Dir(path), fail, registrationRoleDirSync); err != nil {
		return RegistrationResult{}, err
	}
	return RegistrationResult{Path: path, Created: true}, nil
}
func registrationUnchanged(role NativeRoleName, path string, prior []byte) error {
	got, err := registrationRead(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, prior) {
		return fmt.Errorf("%s role changed during update; preserved %s", strings.ToUpper(string(role[:1]))+string(role[1:]), path)
	}
	return nil
}
func registrationUpdate(role NativeRoleName, path string, prior, content []byte, fail func(registrationStep) error) (result RegistrationResult, err error) {
	dir := filepath.Dir(path)
	lock := filepath.Join(dir, "."+string(role)+"-update.lock")
	if err := os.Mkdir(lock, 0777); err != nil {
		return RegistrationResult{}, err
	}
	defer func() { err = errors.Join(err, syscall.Rmdir(lock)) }() // never remove a replacement file
	if err := registrationUnchanged(role, path, prior); err != nil {
		return RegistrationResult{}, err
	}
	backup := path + ".backup-" + registrationDigest(source.DecodeUTF8(prior))
	if err := registrationExclusive(backup, prior, fail); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return RegistrationResult{}, err
		}
		got, readErr := registrationRead(backup)
		if readErr != nil {
			return RegistrationResult{}, readErr
		}
		if !bytes.Equal(got, prior) {
			return RegistrationResult{}, err
		}
	}
	if err := registrationSyncDirectory(dir, fail, registrationBackupDirSync); err != nil {
		return RegistrationResult{}, err
	}
	if err := registrationAt(fail, registrationAfterBackup); err != nil {
		return RegistrationResult{}, err
	}
	tmp, err := registrationStage(path, content, fail)
	if err != nil {
		return RegistrationResult{}, err
	}
	defer func() { err = errors.Join(err, registrationRemove(tmp)) }()
	if err := registrationUnchanged(role, path, prior); err != nil {
		return RegistrationResult{}, err
	}
	if err := registrationAt(fail, registrationBeforeRename); err != nil {
		return RegistrationResult{}, err
	}
	if err := crwdir.Rename(tmp, path); err != nil {
		return RegistrationResult{}, err
	}
	if err := registrationSyncDirectory(dir, fail, registrationRoleDirSync); err != nil {
		return RegistrationResult{}, err
	}
	return RegistrationResult{Path: path, Updated: true}, nil
}
