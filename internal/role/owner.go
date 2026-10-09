package role

import (
	"errors"
	"fmt"
	"io/fs"
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

// ReadRoleFile is the role file's bytes, or nil when it is absent. A path that is not a regular file is refused.
func ReadRoleFile(codexHome string, role NativeRoleName) ([]byte, error) {
	raw, err := registrationRead(RoleFilePath(codexHome, role))
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
