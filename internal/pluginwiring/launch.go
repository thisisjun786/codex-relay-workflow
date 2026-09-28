package pluginwiring

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Flag is the first argument the plugin's declared commands pass to `crw bridge` and `crw hook`.
const Flag = "--plugin-launch"

// The record contract of plugins/crw/wiring/crw_bridge_mcp.py (decision 26), whose checks,
// order and failure texts this reproduces.
const (
	RecordName     = "crw-bridge-mcp.json"
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

// readRegular is crw_bridge_mcp.py read_regular: opened without blocking, judged on the descriptor.
// The error text is str(OSError).
func readRegular(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(file)
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

func isSpace(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f }

// pyStrip is str.strip() with no arguments.
func pyStrip(s string) string { return strings.TrimFunc(s, isSpace) }

// inherited is crw_bridge_mcp.py inherited(): stripped, and empty reads as unset.
func inherited(env map[string]string, name string) (string, bool) {
	value := pyStrip(env[name])
	return value, value != ""
}

func canonical(path string) string {
	resolved, err := store.CanonicalSocket(path)
	if err != nil {
		return path
	}
	return resolved
}

// fsEncodable is os.fsencode's answer: a lone surrogate (kept WTF-8 by the decoder) encodes only
// in U+DC80..U+DCFF, the surrogateescape range.
func fsEncodable(s string) bool {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0xed && b[i+1] >= 0xa0 && b[i+1] <= 0xbf {
			cp := 0xd000 | rune(b[i+1]&0x3f)<<6 | rune(b[i+2]&0x3f)
			if cp < 0xdc80 || cp > 0xdcff {
				return false
			}
		}
	}
	return true
}

func repr(v any) string { return evidence.Repr(v) }

// policyEnvironment is crw_bridge_mcp.py policy_environment: the two variables the bridge starts
// under, or a refusal naming the record.
func policyEnvironment(env map[string]string, record string, reference any) (string, string, error) {
	repair := " Run runtime_install.py register-mcp --owner plugin --execution-policy <file>" +
		" again after moving " + record + " aside, so the record names the policy as" +
		" it now stands."
	object, ok := evidence.Object(reference)
	keys := []string{}
	for _, field := range object {
		keys = append(keys, field.Key)
	}
	if !ok || len(keys) != 2 || !(keys[0] == "digest" && keys[1] == "path" || keys[0] == "path" && keys[1] == "digest") {
		return "", "", fail("the record at " + record + " is version 2 and must name " + policyField +
			" as an object with exactly digest and path")
	}
	pathValue, digestValue := evidence.Get(object, "path"), evidence.Get(object, "digest")
	path, isString := pathValue.(string)
	badPath := !isString || path == "" || path != pyStrip(path) || !strings.HasPrefix(path, "/")
	if isString {
		for _, r := range path {
			if r < 32 || r == 127 {
				badPath = true
			}
		}
	}
	if badPath {
		return "", "", fail("the record at " + record + " must name the execution policy as an absolute" +
			" path with no surrounding whitespace or control characters, found " + repr(pathValue))
	}
	if !fsEncodable(path) {
		return "", "", fail("the record at " + record + " names an execution policy path this system" +
			" cannot encode, found " + repr(path))
	}
	digest, isString := digestValue.(string)
	if !isString || !digestPattern.MatchString(digest) {
		return "", "", fail("the record at " + record + " must name the execution policy digest as 64" +
			" lowercase hexadecimal characters")
	}
	if named, set := inherited(env, execution.EnvPolicy); set && canonical(named) != canonical(path) {
		return "", "", fail("the record at " + record + " names the execution policy " + repr(path) +
			" and this process was started with " + execution.EnvPolicy + "=" + repr(named) +
			". Unset the variable, or register the other file")
	}
	if expected, set := inherited(env, execution.EnvDigest); set && expected != digest {
		return "", "", fail("the record at " + record + " names the policy digest " + digest +
			" and this process was started with " + execution.EnvDigest + "=" + expected +
			". Unset the variable, or register the policy it names")
	}
	raw, err := readRegular(path)
	if err != nil {
		return "", "", fail("the execution policy the record at " + record + " names could not be read (" +
			path + ": " + osText(err) + "). The bridge is not started without it, because it" +
			" would then check no role." + repair)
	}
	sum := sha256.Sum256(raw)
	if actual := hex.EncodeToString(sum[:]); actual != digest {
		return "", "", fail("the execution policy at " + path + " now hashes to " + actual + ", and the record" +
			" at " + record + " was written when it hashed to " + digest + ". It changed" +
			" after it was registered, so the bridge is not started under a policy nobody" +
			" registered." + repair)
	}
	return path, digest, nil
}

// Prepare is crw_bridge_mcp.py main() up to its exec: the record read and judged, then the bridge
// arguments (the record's, then the launcher's own) and the environment it starts under.
// bridgeExecutable is checked as Python checks it and not executed: the Go runtime behind the
// pointer is the bridge.
func Prepare(env map[string]string, extra []string) ([]string, map[string]string, error) {
	home, how := codexHome(env)
	record := purePath(home + "/" + RecordName)
	raw, err := readRegular(record)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fail("no record at " + record + " (resolved from " + how + "). Which command" +
			" writes it depends on who owns this server. If this host registers the bridge" +
			" in its Codex configuration, run runtime_install.py register-mcp --apply and" +
			" this launcher will stand down for that registration. If the package is to own" +
			" it, add --owner plugin. Either way this package never installs a runtime.")
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
	version := evidence.Get(document, "recordVersion")
	if !equalsInt(version, 1) && !equalsInt(version, 2) {
		return nil, nil, fail("the record at " + record + " is version " + repr(version) +
			", and this package reads versions 1 and 2. Rewrite it with the runtime_install.py" +
			" that ships with this package rather than starting a runtime under a contract this" +
			" launcher does not implement.")
	}
	if owner := evidence.Get(document, "owner"); owner != pluginOwner {
		return nil, nil, fail("the record at " + record + " names " + repr(owner) +
			" as the owner of this server, so the Codex configuration registers it and this" +
			" package must not start a second one")
	}
	if name := evidence.Get(document, "serverName"); name != nil && name != declaredServer {
		return nil, nil, fail("the record at " + record + " names the server " + repr(name) +
			", and this package declares " + repr(declaredServer) +
			"; the record belongs to a registration this launcher does not start")
	}
	if executable, ok := evidence.Get(document, "bridgeExecutable").(string); !ok || !strings.HasPrefix(executable, "/") {
		return nil, nil, fail("the record at " + record + " must name bridgeExecutable as an absolute path")
	}
	arguments := []string{}
	if listed := evidence.Get(document, "args"); listed != nil {
		items, ok := listed.([]any)
		if !ok {
			return nil, nil, fail("the record at " + record + " must list args as strings")
		}
		for _, item := range items {
			word, ok := item.(string)
			if !ok {
				return nil, nil, fail("the record at " + record + " must list args as strings")
			}
			arguments = append(arguments, word)
		}
	}
	environment := env
	if equalsInt(version, 1) {
		if _, present := evidence.Lookup(document, policyField); present {
			return nil, nil, fail("the record at " + record + " is version 1 and names an execution policy," +
				" which only a version 2 record carries. Starting it as version 1 would start the" +
				" bridge without that policy.")
		}
	} else {
		path, digest, err := policyEnvironment(env, record, evidence.Get(document, policyField))
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
	return append(arguments, extra...), environment, nil
}

// Bridge is `crw bridge --plugin-launch`: the record contract, then the bridge under it.
func Bridge(ctx context.Context, args []string) int {
	env := mcp.Environ(os.Environ())
	arguments, environment, err := Prepare(env, args)
	var refused *refusal
	if errors.As(err, &refused) {
		fmt.Fprintln(os.Stderr, "crw bridge launcher: "+refused.message)
		return 2
	}
	for _, name := range []string{execution.EnvPolicy, execution.EnvDigest} {
		if value, ok := environment[name]; ok && value != env[name] {
			if err := os.Setenv(name, value); err != nil {
				fmt.Fprintln(os.Stderr, "crw bridge launcher: "+err.Error())
				return 2
			}
		}
	}
	return mcp.Main(ctx, arguments, environment, os.Stdin, os.Stdout, os.Stderr)
}
