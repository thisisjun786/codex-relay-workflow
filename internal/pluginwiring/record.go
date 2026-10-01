package pluginwiring

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// BridgeRecord is a bridge record's fields read as the launcher reads them (the record contract,
// decision 26). The launcher (Prepare), the installer that writes the record and the doctor that
// judges it read a record through it, and each words its own refusals.
type BridgeRecord struct {
	// Version is recordVersion by Python's == (True is 1, 1.0 is 1): 1 or 2, or 0 when it is
	// neither. VersionValue is the value as written.
	Version      int64
	VersionValue any
	// Owner is owner as written.
	Owner any
	// ServerName is serverName as written, nil when absent or null.
	ServerName any
	// Executable is bridgeExecutable when IsString, and ExecutableValue the value as written.
	Executable      string
	IsString        bool
	ExecutableValue any
	// Args are the args when ArgsOK: absent, null, or a list of strings. ArgsValue is args as
	// written.
	Args      []string
	ArgsOK    bool
	ArgsValue any
	// Policy is executionPolicy as written, and HasPolicy whether the key is present at all.
	Policy    any
	HasPolicy bool
}

// ReadBridgeRecord reads a decoded bridge record.
func ReadBridgeRecord(document contract.OrderedObject) BridgeRecord {
	r := BridgeRecord{VersionValue: evidence.Get(document, "recordVersion"), Owner: evidence.Get(document, "owner"),
		ServerName: evidence.Get(document, "serverName"), ExecutableValue: evidence.Get(document, "bridgeExecutable"),
		ArgsValue: evidence.Get(document, "args"), ArgsOK: true}
	for _, n := range []int64{1, 2} {
		if equalsInt(r.VersionValue, n) {
			r.Version = n
		}
	}
	r.Executable, r.IsString = r.ExecutableValue.(string)
	if r.ArgsValue != nil {
		items, ok := r.ArgsValue.([]any)
		r.ArgsOK = ok
		for _, item := range items {
			word, isString := item.(string)
			r.ArgsOK = r.ArgsOK && isString
			r.Args = append(r.Args, word)
		}
		if !r.ArgsOK {
			r.Args = nil
		}
	}
	r.Policy, r.HasPolicy = evidence.Lookup(document, policyField)
	return r
}

// PolicyPath is an execution policy path read as the launcher reads it. What is wrong with it,
// each on its own: not a non-empty string, surrounded by whitespace (str.strip, which the bridge
// applies to the variable it reads, so it would open another file), holding a control
// character, or not absolute.
type PolicyPath struct {
	// Value is the path as written, and Text the path when it is a string.
	Value any
	Text  string

	NotString, Padded, Control, Relative bool
}

// OK is whether the path is one the launcher opens.
func (p PolicyPath) OK() bool { return !p.NotString && !p.Padded && !p.Control && !p.Relative }

// ReadPolicyPath reads an execution policy path.
func ReadPolicyPath(value any) PolicyPath {
	p := PolicyPath{Value: value}
	p.Text, _ = value.(string)
	p.NotString = p.Text == ""
	p.Padded = p.Text != pyvalue.Strip(p.Text)
	p.Control = strings.IndexFunc(p.Text, func(r rune) bool { return r < 32 || r == 127 }) >= 0
	p.Relative = !strings.HasPrefix(p.Text, "/")
	return p
}

// PolicyReference is a version-2 record's executionPolicy reference read as the launcher reads it.
type PolicyReference struct {
	// Shaped is whether it is an object with exactly the keys digest and path.
	Shaped bool
	File   PolicyPath
	// DigestValue is the digest as written, Digest the digest when it is a string, and DigestOK
	// whether it is 64 lowercase hexadecimal characters.
	DigestValue any
	Digest      string
	DigestOK    bool
}

// ReadPolicyReference reads an executionPolicy reference.
func ReadPolicyReference(reference any) PolicyReference {
	object, ok := evidence.Object(reference)
	_, hasPath := evidence.Lookup(object, "path")
	_, hasDigest := evidence.Lookup(object, "digest")
	p := PolicyReference{Shaped: ok && len(object) == 2 && hasPath && hasDigest,
		File: ReadPolicyPath(evidence.Get(object, "path")), DigestValue: evidence.Get(object, "digest")}
	p.Digest, _ = p.DigestValue.(string)
	p.DigestOK = digestPattern.MatchString(p.Digest)
	return p
}

// ErrNotRegular is PolicyDigest's answer for a policy path that names something other than a
// regular file.
var ErrNotRegular = errors.New("not a regular file")

// PolicyDigest is the SHA-256 of the bytes of the policy file at path, read as the launcher reads
// it: opened fs-encoded (os.fsencode: a surrogate escape is the byte it stands for), without
// blocking, and judged on the descriptor. encodable is false for a path os.fsencode refuses; an
// error opening it is the open's *os.PathError, one that is not a regular file is ErrNotRegular,
// and one that cannot be read is the read's error.
func PolicyDigest(path string) (digest string, encodable bool, err error) {
	name, encodable := pyvalue.FSEncode(path)
	if !encodable {
		return "", false, nil
	}
	raw, err := readRegular(name)
	if err != nil {
		return "", true, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true, nil
}

// readRegular is crw_bridge_mcp.py read_regular: opened without blocking, judged on the descriptor.
// path is the bytes opened, which str(OSError) names as Python holds them: os.fsdecode of them
// (store.PathRepr), the str Python opened. A descriptor that cannot be judged is not a regular
// file.
func readRegular(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	return io.ReadAll(file)
}
