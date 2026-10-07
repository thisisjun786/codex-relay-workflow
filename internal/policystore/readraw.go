package policystore

import "github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"

// ReadRaw reads the policy file's bytes through the same non-blocking, descriptor-judged reader the
// reading half uses, so a path that names a named pipe or any other non-regular file is refused
// instead of blocking the request.
func ReadRaw(path string) ([]byte, error) {
	return readRegular(path)
}

// encodedPath is the path as the OS spells it: the launcher reads a record with
// pyvalue.FSEncode, so a surrogate-escaped byte the record carries is opened as that byte rather
// than as the WTF-8 text Go would otherwise pass. A path the conversion refuses names no file this
// host can open.
func encodedPath(path string) (string, error) {
	encoded, ok := pyvalue.FSEncode(path)
	if !ok {
		return "", errUnencodablePath
	}
	return encoded, nil
}
