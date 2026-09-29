package pluginwiring

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Flag is the first argument the plugin's declared commands pass to the bridge and `crw hook`.
const Flag = "--plugin-launch"

// The record contract of plugins/crw/wiring/crw_bridge_mcp.py (decision 26), whose checks and
// order this reproduces, and whose failure texts it keeps but for the repairs, which name the
// installer that writes the record since todo 38 (RepairCommand).
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

// readRegular is crw_bridge_mcp.py read_regular: opened without blocking, judged on the descriptor.
// path is the bytes opened; shown is the str Python opened them as, which str(OSError) names.
func readRegular(path, shown string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: shown, Err: err}
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

// fsencode is os.fsencode of a str the record decoded, which os.open and os.execve apply to every
// path, argument and environment value: a lone surrogate in U+DC80..U+DCFF, which the decoder keeps
// as WTF-8 and which is how runtime_install.py's json.dumps records a byte that is not UTF-8,
// becomes that byte again (surrogateescape), and everything else stays UTF-8. ok is false where
// Python raises UnicodeEncodeError: any other lone surrogate.
func fsencode(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i+2 < len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2] >= 0x80 && s[i+2] <= 0xbf {
			cp := 0xd000 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f)
			if cp < 0xdc80 || cp > 0xdcff {
				return "", false
			}
			b.WriteByte(byte(cp - 0xdc00))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), true
}

func repr(v any) string { return evidence.Repr(v) }

// policyEnvironment is crw_bridge_mcp.py policy_environment: the two variables the bridge starts
// under, or a refusal naming the record.
func policyEnvironment(env map[string]string, record string, reference any) (string, string, error) {
	repair := " Run " + RepairCommand + " --execution-policy <file>" +
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
	encoded, encodable := fsencode(path)
	if !encodable {
		return "", "", fail("the record at " + record + " names an execution policy path this system" +
			" cannot encode, found " + repr(path))
	}
	digest, isString := digestValue.(string)
	if !isString || !digestPattern.MatchString(digest) {
		return "", "", fail("the record at " + record + " must name the execution policy digest as 64" +
			" lowercase hexadecimal characters")
	}
	if named, set := inherited(env, execution.EnvPolicy); set && canonical(named) != canonical(encoded) {
		return "", "", fail("the record at " + record + " names the execution policy " + repr(path) +
			" and this process was started with " + execution.EnvPolicy + "=" + repr(named) +
			". Unset the variable, or register the other file")
	}
	if expected, set := inherited(env, execution.EnvDigest); set && expected != digest {
		return "", "", fail("the record at " + record + " names the policy digest " + digest +
			" and this process was started with " + execution.EnvDigest + "=" + expected +
			". Unset the variable, or register the policy it names")
	}
	raw, err := readRegular(encoded, path)
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
	return encoded, digest, nil
}

// Prepare is crw_bridge_mcp.py main() up to its exec: the record read and judged, then the bridge
// arguments (the record's, fs-encoded, then the launcher's own) and the environment it starts
// under. bridgeExecutable is checked as Python checks it, its execv included, and not executed: the
// Go runtime behind the pointer is the bridge.
func Prepare(env map[string]string, extra []string) ([]string, map[string]string, error) {
	home, how := codexHome(env)
	record := purePath(home + "/" + RecordName)
	raw, err := readRegular(record, record)
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
	version := evidence.Get(document, "recordVersion")
	if !equalsInt(version, 1) && !equalsInt(version, 2) {
		return nil, nil, fail("the record at " + record + " is version " + repr(version) +
			", and this package reads versions 1 and 2. Rewrite it with " + RepairCommand +
			" rather than starting a runtime under a contract this launcher does not implement.")
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
	executable, ok := evidence.Get(document, "bridgeExecutable").(string)
	if !ok || !strings.HasPrefix(executable, "/") {
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
	// os.execv then encodes the executable and each argument in turn, and a lone surrogate outside
	// the surrogateescape range or a NUL raises there: a traceback, exit 1, and no bridge. The
	// executable is not run here, but a record Python never starts is refused all the same.
	if _, encodable := fsencode(executable); !encodable || strings.ContainsRune(executable, 0) {
		return nil, nil, fail("the record at " + record + " names bridgeExecutable " + repr(executable) +
			", which this system cannot pass to exec. Rewrite it with " + RepairCommand + ".")
	}
	for i, word := range arguments {
		encoded, encodable := fsencode(word)
		if !encodable || strings.ContainsRune(word, 0) {
			return nil, nil, fail("the record at " + record + " lists the argument " + repr(word) +
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
	fmt.Fprintln(os.Stderr, stderrText("crw bridge launcher: "+refused.message))
	return 2
}

// stderrText is text as Python's sys.stderr writes it (errors="backslashreplace"): a lone
// surrogate, which UTF-8 cannot carry, as its escape, and every other character as it stands. A
// record path or policy path that holds one reaches the refusal unescaped, as Python's str() of it
// does, and only the stream escapes it.
func stderrText(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := settings.CodePoint(text, i)
		if r >= 0xd800 && r <= 0xdfff {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
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
