package pluginwiring

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Flag is the first argument the plugin's declared commands pass to the bridge and `crw hook`.
const Flag = "--plugin-launch"

// The record contract of crw_bridge_mcp.py (decision 26), the Python launcher the package shipped
// until todo 43. Its recorded answers are the oracle (the copy kept in testdata/pre-native-wiring
// left with the Python implementation in todo 44): this reproduces its checks and their order,
// and keeps its failure texts but for the repairs, which name the installer that writes the
// record since todo 38 (RepairCommand).
const (
	RecordName = "crw-bridge-mcp.json"
	// RepairCommand writes the record (internal/runtime/install RegisterMCP).
	RepairCommand  = "crw install register-mcp --owner plugin"
	pluginOwner    = "plugin"
	declaredServer = "codex-thread-bridge"
	policyField    = "executionPolicy"
	// version/wiring/launcher -> version -> plugin -> marketplace -> cache -> plugins -> home
	cacheDepth = 6
	// launcherName stands where crw_bridge_mcp.py's own file stood in the cache layout.
	launcherName = "crw-bridge.sh"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// refusal is a launcher failure: its text goes to stderr after the launcher's prefix, exit 2.
type refusal struct{ message string }

func (r *refusal) Error() string { return r.message }

func fail(message string) error { return &refusal{message} }

// purePath is str(pathlib.PurePosixPath(p)): empty and "." components dropped, "//" kept as a root.
func purePath(p string) string {
	root := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		root = "//"
	case strings.HasPrefix(p, "/"):
		root = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if root == "" && len(parts) == 0 {
		return "."
	}
	return root + strings.Join(parts, "/")
}

func parent(p string) string {
	i := strings.LastIndex(p, "/")
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	}
	return p[:i]
}

// codexHome is crw_bridge_mcp.py codex_home(), with the launcher's path standing for __file__.
// The declared cwd is the installed version directory, so the launcher is cwd/wiring/crw-bridge.sh.
func codexHome(env map[string]string) (string, string) {
	if named := env["CODEX_HOME"]; named != "" {
		return purePath(named), "the CODEX_HOME environment variable"
	}
	if cwd, err := os.Getwd(); err == nil {
		if resolved, err := store.ResolvePath(cwd); err == nil {
			here := purePath(resolved + "/wiring/" + launcherName)
			depth := strings.Count(here, "/")
			if depth > cacheDepth {
				candidate := here
				for range cacheDepth + 1 {
					candidate = parent(candidate)
				}
				if strings.HasSuffix(candidate, "/.codex") || exists(candidate+"/config.toml") {
					return candidate, "the installed package location " + here
				}
				return candidate, "the installed package location " + here + ", which does not look like a Codex home"
			}
		}
	}
	// Path.home() with HOME="" is "/", so the trailing separator is trimmed before joining.
	return purePath(strings.TrimRight(pythonHome(env), "/") + "/.codex"), "the default home, because nothing else named one"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pythonHome is Path.home(): HOME when set, else the password database.
func pythonHome(env map[string]string) string {
	if home, ok := env["HOME"]; ok {
		return home
	}
	if account, err := user.Current(); err == nil {
		return account.HomeDir
	}
	return ""
}

func osText(err error) string { return store.PythonOSErrorText(err) }

// equalsInt is Python ==, where True == 1 and 1.0 == 1.
func equalsInt(v any, n int64) bool {
	switch x := v.(type) {
	case bool:
		return (x && n == 1) || (!x && n == 0)
	case int64:
		return x == n
	case float64:
		return x == float64(n)
	}
	return false
}

// inherited is crw_bridge_mcp.py inherited(): stripped, and empty reads as unset.
func inherited(env map[string]string, name string) (string, bool) {
	value := pyvalue.Strip(env[name])
	return value, value != ""
}

func canonical(path string) string {
	resolved, err := store.CanonicalSocket(path)
	if err != nil {
		return path
	}
	return resolved
}

// policyEnvironment is crw_bridge_mcp.py policy_environment: the two variables the bridge starts
// under, or a refusal naming the record.
func policyEnvironment(env map[string]string, record string, reference any) (string, string, error) {
	repair := " Run " + RepairCommand + " --execution-policy <file>" +
		" again after moving " + record + " aside, so the record names the policy as" +
		" it now stands."
	policy := ReadPolicyReference(reference)
	if !policy.Shaped {
		return "", "", fail("the record at " + record + " is version 2 and must name " + policyField +
			" as an object with exactly digest and path")
	}
	if !policy.File.OK() {
		return "", "", fail("the record at " + record + " must name the execution policy as an absolute" +
			" path with no surrounding whitespace or control characters, found " + pyvalue.Repr(policy.File.Value))
	}
	path, digest := policy.File.Text, policy.Digest
	// What os.open and os.execve use: the path fs-encoded, a surrogate-escaped byte that byte again.
	encoded, encodable := pyvalue.FSEncode(path)
	if !encodable {
		return "", "", fail("the record at " + record + " names an execution policy path this system" +
			" cannot encode, found " + pyvalue.Repr(path))
	}
	if !policy.DigestOK {
		return "", "", fail("the record at " + record + " must name the execution policy digest as 64" +
			" lowercase hexadecimal characters")
	}
	if named, set := inherited(env, execution.EnvPolicy); set && canonical(named) != canonical(encoded) {
		return "", "", fail("the record at " + record + " names the execution policy " + pyvalue.Repr(path) +
			" and this process was started with " + execution.EnvPolicy + "=" + pyvalue.Repr(named) +
			". Unset the variable, or register the other file")
	}
	if expected, set := inherited(env, execution.EnvDigest); set && expected != digest {
		return "", "", fail("the record at " + record + " names the policy digest " + digest +
			" and this process was started with " + execution.EnvDigest + "=" + expected +
			". Unset the variable, or register the policy it names")
	}
	actual, _, err := PolicyDigest(path)
	if err != nil {
		return "", "", fail("the execution policy the record at " + record + " names could not be read (" +
			path + ": " + osText(err) + "). The bridge is not started without it, because it" +
			" would then check no role." + repair)
	}
	if actual != digest {
		return "", "", fail("the execution policy at " + path + " now hashes to " + actual + ", and the record" +
			" at " + record + " was written when it hashed to " + digest + ". It changed" +
			" after it was registered, so the bridge is not started under a policy nobody" +
			" registered." + repair)
	}
	return encoded, digest, nil
}

// Prepare is crw_bridge_mcp.py main() up to its exec: the record read and judged, then the bridge
// arguments (the record's, fs-encoded, then the launcher's own) and the environment it starts
// under. bridgeExecutable is checked as Python checks it, its execv included, and not executed: the
// Go runtime behind the pointer is the bridge.
func Prepare(env map[string]string, extra []string) ([]string, map[string]string, error) {
	home, how := codexHome(env)
	record := purePath(home + "/" + RecordName)
	raw, err := readRegular(record)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fail("no record at " + record + " (resolved from " + how + "). If the" +
			" package is to own this server, run " + RepairCommand + " to write it. If this host" +
			" registers the bridge in its Codex configuration instead, this package must not" +
			" start a second one. Either way this package never installs a runtime.")
	}
	if err != nil {
		return nil, nil, fail("the record at " + record + " could not be read: " + osText(err))
	}
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, nil, fail("the record at " + record + " could not be read: " + err.Error())
	}
	value, err := hook.Decode(raw)
	if err != nil {
		return nil, nil, fail("the record at " + record + " could not be read: " + err.Error())
	}
	document, ok := evidence.Object(value)
	if !ok {
		return nil, nil, fail("the record at " + record + " is not an object")
	}
	read := ReadBridgeRecord(document)
	if read.Version == 0 {
		return nil, nil, fail("the record at " + record + " is version " + pyvalue.Repr(read.VersionValue) +
			", and this package reads versions 1 and 2. Rewrite it with " + RepairCommand +
			" rather than starting a runtime under a contract this launcher does not implement.")
	}
	if read.Owner != pluginOwner {
		return nil, nil, fail("the record at " + record + " names " + pyvalue.Repr(read.Owner) +
			" as the owner of this server, so the Codex configuration registers it and this" +
			" package must not start a second one")
	}
	if read.ServerName != nil && read.ServerName != declaredServer {
		return nil, nil, fail("the record at " + record + " names the server " + pyvalue.Repr(read.ServerName) +
			", and this package declares " + pyvalue.Repr(declaredServer) +
			"; the record belongs to a registration this launcher does not start")
	}
	executable := read.Executable
	if !read.IsString || !strings.HasPrefix(executable, "/") {
		return nil, nil, fail("the record at " + record + " must name bridgeExecutable as an absolute path")
	}
	if !read.ArgsOK {
		return nil, nil, fail("the record at " + record + " must list args as strings")
	}
	arguments := append([]string{}, read.Args...)
	environment := env
	if read.Version == 1 {
		if read.HasPolicy {
			return nil, nil, fail("the record at " + record + " is version 1 and names an execution policy," +
				" which only a version 2 record carries. Starting it as version 1 would start the" +
				" bridge without that policy.")
		}
	} else {
		path, digest, err := policyEnvironment(env, record, read.Policy)
		if err != nil {
			return nil, nil, err
		}
		environment = map[string]string{}
		for key, value := range env {
			environment[key] = value
		}
		environment[execution.EnvPolicy] = path
		environment[execution.EnvDigest] = digest
	}
	// os.execv then encodes the executable and each argument in turn (os.fsencode, which
	// reading.FSEncode is: a lone surrogate in U+DC80..U+DCFF, how runtime_install.py's json.dumps
	// records a byte that is not UTF-8, becomes that byte again), and a lone surrogate outside that
	// range or a NUL raises there: a traceback, exit 1, and no bridge. The executable is not run
	// here, but a record Python never starts is refused all the same.
	if _, encodable := pyvalue.FSEncode(executable); !encodable || strings.ContainsRune(executable, 0) {
		return nil, nil, fail("the record at " + record + " names bridgeExecutable " + pyvalue.Repr(executable) +
			", which this system cannot pass to exec. Rewrite it with " + RepairCommand + ".")
	}
	for i, word := range arguments {
		encoded, encodable := pyvalue.FSEncode(word)
		if !encodable || strings.ContainsRune(word, 0) {
			return nil, nil, fail("the record at " + record + " lists the argument " + pyvalue.Repr(word) +
				", which this system cannot pass to exec. Rewrite it with " + RepairCommand + ".")
		}
		arguments[i] = encoded
	}
	return append(arguments, extra...), environment, nil
}

// Bridge is `codex-thread-bridge --plugin-launch` (what wiring/crw-bridge.sh execs) and `crw
// bridge --plugin-launch`: the record contract, then an exec of this same binary as the bridge,
// as crw_bridge_mcp.py ended in an execve. program is this process's argv[0] and is kept, so the
// running bridge's argv still ends in codex-thread-bridge where /proc scans look for it, and a
// version-2 record's policy is in that process's own environment (/proc/<pid>/environ), not only
// in a map handed to the server.
func Bridge(program string, args []string) int {
	arguments, environment, err := Prepare(mcp.Environ(os.Environ()), args)
	if err == nil && len(arguments) > 0 && arguments[0] == Flag {
		// The exec below would read this as another plugin launch and start this launcher again.
		err = fail("the bridge's arguments begin with " + Flag + ", which would start this launcher" +
			" again instead of the bridge")
	}
	var self string
	if err == nil {
		self, err = os.Executable()
	}
	if err == nil {
		argv := []string{program}
		if filepath.Base(program) != declaredServer {
			argv = append(argv, "bridge")
		}
		err = syscall.Exec(self, append(argv, arguments...), environ(os.Environ(), environment))
		err = fail("could not start " + self + ": " + osText(err) + ". The installer's pointer names" +
			" the runtime; check that it is installed.")
	}
	var refused *refusal
	if !errors.As(err, &refused) {
		refused = &refusal{err.Error()}
	}
	fmt.Fprintln(os.Stderr, settings.StderrText("crw bridge launcher: "+refused.message))
	return 2
}

// environ is the process environment entries with environment's values for the two policy
// variables: replaced where they stand, appended where absent, as os.execve(env) with the
// launcher's dict(os.environ) plus two keys lays them out.
func environ(entries []string, environment map[string]string) []string {
	out := make([]string, 0, len(entries)+2)
	placed := map[string]bool{}
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		if value, ok := environment[key]; ok && (key == execution.EnvPolicy || key == execution.EnvDigest) {
			entry = key + "=" + value
			placed[key] = true
		}
		out = append(out, entry)
	}
	for _, key := range []string{execution.EnvPolicy, execution.EnvDigest} {
		if value, ok := environment[key]; ok && !placed[key] {
			out = append(out, key+"="+value)
		}
	}
	return out
}
