package role

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Owner says whose role file a ~/.codex/agents/<role>.toml is.
type Owner string

// The owners ReadOwner tells apart. A file is OwnerCRW or OwnerCXC only when its first line is that
// product's marker and the digest in it matches the rest of the file, as the product's own
// registration decides; anything else that exists is OwnerOther and is never replaced.
const (
	OwnerNone  Owner = "none"
	OwnerCRW   Owner = "crw"
	OwnerCXC   Owner = "codexclaw"
	OwnerOther Owner = "other"
)

var ownerCXCMarker = regexp.MustCompile(`^# codexclaw-managed: ([a-f0-9]{64})\n`)

// OwnerOf classifies a role file's bytes; nil is an absent file.
func OwnerOf(raw []byte) Owner {
	if raw == nil {
		return OwnerNone
	}
	decoded := string(raw)
	if registrationManaged(decoded) {
		return OwnerCRW
	}
	if m := ownerCXCMarker.FindStringSubmatch(decoded); m != nil && registrationDigest(decoded[len(m[0]):]) == m[1] {
		return OwnerCXC
	}
	return OwnerOther
}

// RoleFilePath is <codex home>/agents/<role>.toml.
func RoleFilePath(codexHome string, role NativeRoleName) string {
	return filepath.Join(codexHome, "agents", string(role)+".toml")
}

// CheckAgentsDirectory refuses an agents directory that is not a real directory, a symlink above all,
// as RegisterRole refuses it; an absent one is no error. A caller checks it before each read and each
// write of a role file in it, so no file behind a symlink is read, replaced or removed.
func CheckAgentsDirectory(dir string) error {
	st, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&fs.ModeSymlink != 0 {
		return sentinel(fmt.Sprintf("Refusing non-regular agents directory: %s", dir))
	}
	return nil
}

// ReadRoleFile is the role file's bytes, or nil when it is absent. A path that is not a regular file is refused.
// The agents directory is checked first (CheckAgentsDirectory).
func ReadRoleFile(codexHome string, role NativeRoleName) ([]byte, error) {
	path := RoleFilePath(codexHome, role)
	if err := CheckAgentsDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	raw, err := registrationRead(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// NativeRoleContent is the exact bytes RegisterRole writes for role: the managed marker and the body.
func NativeRoleContent(role NativeRoleName) ([]byte, error) {
	if role != Architect && role != Executor {
		return nil, fmt.Errorf("unsupported native role %s", registrationQuote(string(role)))
	}
	raw, err := registrationAgents.ReadFile("agents/" + string(role) + ".toml")
	if err != nil {
		return nil, err
	}
	body := registrationBody(string(raw))
	return []byte("# crw-managed: " + registrationDigest(body) + "\n" + body), nil
}
