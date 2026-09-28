package store

import (
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"golang.org/x/sys/unix"
	"strings"
)

const ManifestSerialization = "MANIFEST-CANON-01: absolute normalized POSIX paths, sorted byte-wise, '<absolutePath>:<lowercase hex sha256>' per entry, joined with a single LF and no trailing newline, sha256 of that UTF-8 string"

func accessFailure(err error) bool {
	var scope *RefusedError
	if errors.As(err, &scope) && scope.Reason == "unverifiable_path_binding" {
		return true
	}
	var errno unix.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno != unix.ELOOP && errno != unix.ENOTDIR && errno != unix.ENOENT && errno != unix.ESTALE
}
func VerifyAgainstDiskDetailed(entries []ManifestEntry, roots []string, allowLease bool) ([]string, map[string]ArtifactBinding, []string) {
	problems, unreadable := []string{}, []string{}
	bindings := map[string]ArtifactBinding{}
	for _, entry := range entries {
		digest, size, binding, err := HashArtifact(entry.Path, roots, allowLease)
		if err != nil {
			message := entry.Path + ": " + err.Error()
			problems = append(problems, message)
			if accessFailure(err) {
				unreadable = append(unreadable, message)
			}
			continue
		}
		bindings[entry.Path] = binding
		if digest != entry.SHA256 {
			problems = append(problems, fmt.Sprintf("%s: bytes hash to %s but the manifest claims %s", entry.Path, digest, entry.SHA256))
		} else if entry.Bytes != nil && *entry.Bytes != size {
			problems = append(problems, fmt.Sprintf("%s: size %d but the manifest claims %d", entry.Path, size, *entry.Bytes))
		}
	}
	return problems, bindings, unreadable
}

func frozenDocument(entries []ManifestEntry, revision string) ([]byte, error) {
	records := []any{}
	for _, entry := range entries {
		object := contract.OrderedObject{}
		if entry.Bytes != nil {
			object = append(object, contract.Field{Key: "bytes", Value: *entry.Bytes})
		}
		object = append(object, contract.Field{Key: "path", Value: entry.Path}, contract.Field{Key: "sha256", Value: entry.SHA256})
		records = append(records, object)
	}
	var buffer strings.Builder
	if err := contract.Emit(&buffer, contract.OrderedObject{{Key: "entries", Value: records}, {Key: "revisionHash", Value: revision}, {Key: "serialization", Value: ManifestSerialization}}); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buffer.String(), "\n")), nil
}
