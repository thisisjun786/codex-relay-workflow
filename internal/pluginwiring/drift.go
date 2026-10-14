package pluginwiring

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
)

// PolicyDrift is whether the execution policy file a bridge registration record names still hashes to the digest the record
// carries. The packaged launcher refuses to start the bridge under a policy whose bytes differ from the record, so a drifted
// host loses codex-thread-bridge in every new session without the host saying why (CRW-1324).
type PolicyDrift struct {
	// Drifted is true when the bridge would be refused: the file's digest differs, or the file is gone or is not a regular file.
	Drifted bool
	// Path is the policy file the record names, Registered the digest it carries and Actual the digest of the bytes now (empty when
	// the file cannot be read as a regular file).
	Path, Registered, Actual string
	// Reason says in a few words how the file differs, empty when it does not.
	Reason string
	// Override is set, and Drifted with it, when the launcher would refuse on a CODEX_THREAD_BRIDGE_EXECUTION_POLICY or
	// CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST that names another policy or digest than the record; the launcher judges it
	// before it reads the file, so it is reported before any difference of the file's bytes (CRW-1360).
	Override LauncherOverride
}

// ReadPolicyDrift compares the record in codexHome with the policy file it names. It reads two files and writes nothing. A host
// with no record, a record that is not a plugin-owned version 2 record naming a policy, and a policy this process cannot read
// for another reason answer no drift: they are not the disagreement this reports, and the doctor judges them.
func ReadPolicyDrift(codexHome string) PolicyDrift {
	return ReadPolicyDriftUnder(codexHome, nil)
}

// ReadPolicyDriftUnder is ReadPolicyDrift for a process whose environment is env (a nil map has none of the launcher's
// variables): the launcher's EnvPolicy and EnvDigest overrides are compared with the record by the code the launcher uses.
func ReadPolicyDriftUnder(codexHome string, env map[string]string) PolicyDrift {
	found := reading.ReadJSON(filepath.Join(codexHome, RecordName), "the bridge MCP record", nil, nil)
	if !found.OK() {
		return PolicyDrift{}
	}
	document, ok := found.Value.(pyjson.Object)
	if !ok {
		return PolicyDrift{}
	}
	read := ReadBridgeRecord(document)
	if read.Version != 2 || read.Owner != "plugin" || !read.HasPolicy {
		return PolicyDrift{}
	}
	reference := ReadPolicyReference(read.Policy)
	if !reference.Shaped || !reference.File.OK() || !reference.DigestOK {
		return PolicyDrift{}
	}
	drift := PolicyDrift{Path: reference.File.Text, Registered: reference.Digest}
	if encoded, ok := pyvalue.FSEncode(reference.File.Text); ok {
		if override := launcherOverride(env, encoded, reference.Digest); override.Variable != "" {
			drift.Drifted, drift.Override, drift.Reason = true, override, override.Variable+" names another policy than the record"
			return drift
		}
	}
	actual, encodable, err := PolicyDigest(reference.File.Text)
	var opening *os.PathError
	switch {
	case !encodable:
		return PolicyDrift{}
	case errors.As(err, &opening) && (errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)):
		drift.Drifted, drift.Reason = true, "the file does not exist"
	case errors.Is(err, ErrNotRegular):
		drift.Drifted, drift.Reason = true, "the path is not a regular file"
	case err != nil:
		return PolicyDrift{}
	case actual != reference.Digest:
		drift.Drifted, drift.Actual, drift.Reason = true, actual, "the file's bytes changed after the record was written"
	default:
		drift.Actual = actual
	}
	return drift
}
