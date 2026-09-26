//go:build dev

package ci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Constants and patterns of scripts/ci/plugin.py; see that script for why each exists.
const (
	pluginRelative      = "plugins/crw"
	marketplacePath     = ".agents/plugins/marketplace.json"
	manifestPath        = ".codex-plugin/plugin.json"
	licenseID           = "MIT"
	hookTimeoutSeconds  = 10
	payloadSuffixLength = 12
)

var (
	alwaysRoots     = []string{".codex-plugin", "LICENSE"}
	requiredFiles   = []string{manifestPath, "LICENSE"}
	semverIdent     = `(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`
	semverPattern   = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)` + `(?:-` + semverIdent + `(?:\.` + semverIdent + `)*)?` + `(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?\z`)
	textFields      = []string{"name", "version", "description", "license", "repository", "skills"}
	interfaceFields = []string{"displayName", "shortDescription", "longDescription", "developerName",
		"category", "capabilities", "defaultPrompt"}
	manifestKeys = []string{"name", "version", "description", "author", "homepage", "repository",
		"license", "keywords", "skills", "hooks", "mcpServers", "apps", "interface"}
	interfaceOptional = []string{"websiteURL", "privacyPolicyURL", "termsOfServiceURL",
		"brandColor", "composerIcon", "logo", "logoDark", "screenshots"}
	authorKeys            = []string{"name", "email", "url"}
	approvalModes         = []string{"auto", "prompt", "writes", "approve"}
	approvalKeys          = []string{"approval_mode"}
	requiredToolApprovals = map[string][][2]string{
		"codex-thread-bridge": {{"create_thread", "approve"}, {"send_message_to_thread", "approve"}},
	}
	// Each personal-path pattern with its lookbehind (?<![A-Za-z0-9._-]) as a flag, since
	// Go's regexp has no lookbehind; the patterns are anchored and tried at each position.
	homePaths = []struct {
		pattern    *regexp.Regexp
		lookbehind bool
	}{
		{regexp.MustCompile(`^/home/[A-Za-z0-9._-]+/`), true},
		{regexp.MustCompile(`^/Users/[A-Za-z0-9._-]+/`), true},
		{regexp.MustCompile(`^/root/`), true},
		{regexp.MustCompile(`[A-Za-z]:\\Users\\[A-Za-z0-9._-]+`), false},
	}
	// Python's re.match with a trailing $ also accepts one final newline, hence \n?\z.
	forbiddenNames = regexp.MustCompile(`(?i)^(\.git|\.codexclaw|\.env(\..*)?|id_(rsa|dsa|ecdsa|ed25519)(\..*)?` +
		`|\.netrc|\.pgpass|\.htpasswd|\.npmrc|authorized_keys` +
		`|.*credentials?(?:[._-].*)?|.*secrets?(?:[._-].*)?` +
		`|.*\.(sqlite3?|db|pem|key|p12|pfx))\n?\z`)
	httpsFields = []string{"websiteURL", "privacyPolicyURL", "termsOfServiceURL"}
	assetFields = []string{"composerIcon", "logo", "logoDark"}
	brandColor  = regexp.MustCompile(`^#[0-9A-Fa-f]{6}\n?\z`)
)

// packageError is plugin.py's PackageError: a fact could not be read, so no verdict.
type packageError struct{ text string }

func (e packageError) Error() string { return e.text }

// entry is one shipped file: its git mode and bytes.
type entry struct {
	mode string
	data []byte
}

type payload map[string]entry

func (p payload) names() []string {
	names := make([]string, 0, len(p))
	for name := range p {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p payload) has(name string) bool {
	_, ok := p[name]
	return ok
}

// pluginChecker holds the repository root the checks read, found lazily so --payload
// works outside a checkout.
type pluginChecker struct{ root string }

func (c *pluginChecker) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = c.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, packageError{pyStrip(decodeReplace(stderr.Bytes()))}
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (c *pluginChecker) gitText(args ...string) (string, error) {
	out, err := c.git(args...)
	if err != nil {
		return "", err
	}
	return decodeUTF8(out)
}

func (c *pluginChecker) revisionPayload(revision string) (payload, []string, error) {
	listing, err := c.gitText("ls-tree", "-r", "-z", revision, "--", pluginRelative)
	if err != nil {
		return nil, nil, err
	}
	result, errs := payload{}, []string{}
	type request struct{ name, mode, sha string }
	var requests []request
	for _, record := range strings.Split(listing, "\x00") {
		if record == "" {
			continue
		}
		meta, path, _ := strings.Cut(record, "\t")
		fields := strings.SplitN(meta, " ", 3)
		mode, kind, sha := fields[0], fields[1], fields[2]
		name := strings.Join(pyPurePath(path)[len(pyPurePath(pluginRelative)):], "/")
		if kind != "blob" {
			errs = append(errs, "release "+path+": the package may not contain a "+kind)
			continue
		}
		if mode == "120000" {
			errs = append(errs, "release "+path+": the installer drops symlinks, so the package may not contain one")
			continue
		}
		requests = append(requests, request{name, mode, sha})
	}
	if len(requests) > 0 {
		shas := make([]string, len(requests))
		for i, r := range requests {
			shas[i] = r.sha
		}
		cmd := exec.Command("git", "cat-file", "--batch")
		cmd.Dir = c.root
		cmd.Stdin = strings.NewReader(strings.Join(shas, "\n"))
		batch, err := cmd.Output()
		if err != nil {
			return nil, nil, fmt.Errorf("git cat-file --batch: %w", err)
		}
		offset := 0
		for _, r := range requests {
			headerEnd := offset + bytes.IndexByte(batch[offset:], '\n')
			size, err := strconv.Atoi(string(bytes.Fields(batch[offset:headerEnd])[2]))
			if err != nil {
				return nil, nil, fmt.Errorf("git cat-file --batch: %w", err)
			}
			start := headerEnd + 1
			result[r.name] = entry{r.mode, batch[start : start+size]}
			offset = start + size + 1
		}
	}
	return result, errs, nil
}

// directoryPayload reads an installed or working-tree plugin directory as the installer
// would copy it, refusing what it cannot copy faithfully.
func directoryPayload(pluginRoot string) (payload, []string) {
	result, errs := payload{}, []string{}
	filepath.WalkDir(pluginRoot, func(path string, d fs.DirEntry, err error) error {
		if path == pluginRoot {
			if err != nil {
				return filepath.SkipDir
			}
			return nil
		}
		name := filepath.ToSlash(strings.TrimPrefix(path, pluginRoot+string(filepath.Separator)))
		if d != nil && d.Type()&fs.ModeSymlink != 0 {
			errs = append(errs, "installed "+name+": the installer drops symlinks, so the package may not contain one")
			return nil
		}
		if d != nil && d.IsDir() {
			if entries, readErr := os.ReadDir(path); readErr == nil && len(entries) == 0 {
				errs = append(errs, name+": an empty directory still ships; remove it")
			}
			if err != nil {
				return filepath.SkipDir
			}
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil
		}
		mode := "100644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		data, readErr := regularBytes(path)
		if readErr != nil {
			errs = append(errs, "installed "+name+" could not be read as a regular file ("+readErr.Error()+
				"); the installer copies files, and this one is not a file it can copy")
			return nil
		}
		result[name] = entry{mode, data}
		return nil
	})
	return result, errs
}

// regularBytes reads a file through one descriptor opened without blocking and judged a
// regular file, so a pipe cannot hold the check (and a lock its caller holds) open. The
// error text is Python's "<OSError class>: <str(error)>".
func regularBytes(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New(pyOSError(err))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New(pyOSError(err))
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("OSError: [Errno %d] not a regular file: %s", int(syscall.EINVAL), pyRepr(path))
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, errors.New(pyOSError(err))
	}
	return data, nil
}

// payloadDigest is sha256 over length-framed "<len>:<name> <mode> <sha256>" lines in name order.
func payloadDigest(p payload) string {
	var blocks [][]byte
	for _, name := range p.names() {
		sum := sha256.Sum256(p[name].data)
		blocks = append(blocks, fmt.Appendf(nil, "%d:%s %s %s", len(name), name, p[name].mode, hex.EncodeToString(sum[:])))
	}
	sum := sha256.Sum256(bytes.Join(blocks, []byte("\n")))
	return hex.EncodeToString(sum[:])
}

func splitVersion(version string) (string, string) {
	release, suffix, _ := strings.Cut(version, "+")
	return release, suffix
}

// versionPayload is the payload with the manifest's recorded suffix elided, so recording the
// digest does not change it.
func versionPayload(p payload, version string) (payload, error) {
	release, suffix := splitVersion(version)
	if suffix == "" || !p.has(manifestPath) {
		return p, nil
	}
	manifest := p[manifestPath]
	recorded, plain := []byte(pyJSONString(version)), []byte(pyJSONString(release))
	if count := bytes.Count(manifest.data, recorded); count != 1 {
		return nil, valueError{fmt.Sprintf("%s spells %s %d times; the suffix has to be elided exactly once for the digest beneath it to be derived at all",
			manifestPath, pyRepr(version), count)}
	}
	out := maps(p)
	out[manifestPath] = entry{manifest.mode, bytes.ReplaceAll(manifest.data, recorded, plain)}
	return out, nil
}

func maps(p payload) payload {
	out := payload{}
	for k, v := range p {
		out[k] = v
	}
	return out
}

func payloadVersion(p payload, version string) (string, error) {
	release, _ := splitVersion(version)
	elided, err := versionPayload(p, version)
	if err != nil {
		return "", err
	}
	return release + "+" + payloadDigest(elided)[:payloadSuffixLength], nil
}

// manifestVersion is str(manifest.get("version", "")).
func manifestVersion(m *pyDict) string {
	if !m.has("version") {
		return ""
	}
	return pyStr(m.get("version"))
}

func versionErrors(m *pyDict, p payload, label string) []string {
	version := manifestVersion(m)
	expected, err := payloadVersion(p, version)
	if err != nil {
		return []string{label + " manifest: " + err.Error()}
	}
	var suffixes []string
	for _, v := range []string{expected, version} {
		if _, suffix := splitVersion(v); suffix != "" {
			suffixes = append(suffixes, suffix)
		}
	}
	var errs []string
	for _, name := range p.names() {
		if name == manifestPath {
			continue
		}
		for _, suffix := range suffixes {
			if bytes.Contains(p[name].data, []byte(suffix)) {
				errs = append(errs, label+" "+name+": a shipped file repeats the payload suffix, so recording"+
					" the digest would change the digest it records; the manifest version is the"+
					" one place that suffix belongs")
				break
			}
		}
	}
	if len(errs) > 0 {
		return errs
	}
	if version == expected {
		return nil
	}
	return []string{label + " manifest: version " + pyRepr(version) + " does not name this payload." +
		" Record " + pyRepr(expected) + "; `--record-version` writes it into the working" +
		" tree manifest. The suffix is this payload's own digest, because two packages" +
		" that ship different bytes may not offer one version"}
}

func readManifest(p payload, label string) (*pyDict, error) {
	file, ok := p[manifestPath]
	if !ok {
		return nil, packageError{label + ": " + manifestPath + " is missing from the package"}
	}
	text, err := decodeUTF8(file.data)
	if err != nil {
		return nil, packageError{label + " " + manifestPath + ": " + err.Error()}
	}
	value, err := pyJSONLoadsOrdered(text)
	if err != nil {
		return nil, packageError{label + " " + manifestPath + ": " + err.Error()}
	}
	m, ok := asDict(value)
	if !ok {
		return nil, packageError{label + " " + manifestPath + ": the manifest must be a JSON object"}
	}
	return m, nil
}

// insidePath is plugin.py's inside(): a ./ relative path that stays in the package.
func insidePath(declared any) (string, bool) {
	text, ok := declared.(string)
	if !ok || !strings.HasPrefix(text, "./") {
		return "", false
	}
	parts := pyPurePath(strings.Trim(text[2:], "/"))
	if len(parts) == 0 || slices.Contains(parts, "..") {
		return "", false
	}
	return strings.Join(parts, "/"), true
}

func declaredSkillsPath(m *pyDict) (string, error) {
	declared, ok := m.get("skills").(string)
	if !ok || !strings.HasPrefix(declared, "./") {
		return "", valueError{"manifest must declare skills as a ./ relative path"}
	}
	relative, ok := insidePath(declared)
	if !ok {
		return "", valueError{"declared skills path must stay inside the plugin root"}
	}
	return relative, nil
}

func declaredHooks(m *pyDict) ([]any, error) {
	switch declared := m.get("hooks").(type) {
	case nil:
		return nil, nil
	case string:
		return []any{declared}, nil
	case []any:
		return declared, nil
	}
	return nil, valueError{"hooks must be a ./ relative path or a list of them; an inline document does not load"}
}

type component struct{ field, relative string }

func declaredComponents(m *pyDict) ([]component, []string, []string) {
	var paths []component
	roots := slices.Clone(alwaysRoots)
	var errs []string
	if skillsPath, err := declaredSkillsPath(m); err != nil {
		errs = append(errs, err.Error())
	} else {
		roots = append(roots, strings.Split(skillsPath, "/")[0])
	}
	hooks, err := declaredHooks(m)
	if err != nil {
		errs = append(errs, err.Error())
		hooks = nil
	}
	type declaration struct {
		field string
		value any
	}
	var declarations []declaration
	for _, hook := range hooks {
		declarations = append(declarations, declaration{"hooks", hook})
	}
	mcp := m.get("mcpServers")
	if _, inline := asDict(mcp); inline {
		errs = append(errs, "mcpServers must name a ./ relative file: an inline table would ship a server this check never reads")
		mcp = nil
	}
	if mcp != nil {
		declarations = append(declarations, declaration{"mcpServers", mcp})
	}
	for _, d := range declarations {
		relative, ok := insidePath(d.value)
		if !ok {
			errs = append(errs, d.field+" "+pyReprValue(d.value)+" must be a ./ relative path inside the plugin root")
			continue
		}
		paths = append(paths, component{d.field, relative})
		roots = append(roots, strings.Split(relative, "/")[0])
	}
	if m.has("apps") {
		errs = append(errs, "apps is not declared by this package")
	}
	return paths, sortedSet(roots), errs
}

// nonempty is a string that is not blank; ingestion treats whitespace-only as absent.
func nonempty(value any) bool {
	text, ok := value.(string)
	return ok && pyStrip(text) != ""
}

func loadDocument(name string, data []byte, label string) (any, []string) {
	text, err := decodeUTF8(data)
	if err == nil {
		var document any
		if document, err = pyJSONLoadsOrdered(text); err == nil {
			return document, nil
		}
	}
	return nil, []string{label + " " + name + ": " + err.Error()}
}

func hookDocumentErrors(name string, data []byte, label string) []string {
	document, errs := loadDocument(name, data, label)
	if errs != nil {
		return errs
	}
	var events *pyDict
	if d, ok := asDict(document); ok {
		events, _ = asDict(d.get("hooks"))
	}
	if events == nil || len(events.keys) == 0 {
		return []string{label + " " + name + ": a hook file must hold a nonempty hooks object"}
	}
	prefix := label + " " + name + ": "
	for _, event := range events.sortedKeys() {
		groups, ok := events.get(event).([]any)
		if !ok || len(groups) == 0 {
			errs = append(errs, prefix+event+" must hold a nonempty list")
			continue
		}
		for _, group := range groups {
			var entries []any
			if g, ok := asDict(group); ok {
				entries, _ = g.get("hooks").([]any)
			}
			if len(entries) == 0 {
				errs = append(errs, prefix+"every "+event+" group must hold a nonempty hooks list")
				continue
			}
			for _, item := range entries {
				hook, ok := asDict(item)
				if !ok || hook.get("type") != "command" {
					errs = append(errs, prefix+"every hook must be a command hook")
					continue
				}
				if !nonempty(hook.get("command")) {
					errs = append(errs, prefix+"every hook needs a command")
				}
				timeout, isInt := pyInt(hook.get("timeout"))
				if !isInt || timeout.Sign() <= 0 {
					errs = append(errs, prefix+"every hook needs a positive integer timeout")
				} else if timeout.Cmp(bigInt(hookTimeoutSeconds)) > 0 {
					errs = append(errs, prefix+"a hook timeout may not exceed "+strconv.Itoa(hookTimeoutSeconds)+" seconds")
				}
			}
		}
	}
	return errs
}

func mcpDocumentErrors(name string, data []byte, p payload, label string) []string {
	document, errs := loadDocument(name, data, label)
	if errs != nil {
		return errs
	}
	var servers *pyDict
	if d, ok := asDict(document); ok {
		servers, _ = asDict(d.get("mcpServers"))
	}
	if servers == nil || len(servers.keys) == 0 {
		return []string{label + " " + name + ": an MCP file must hold a nonempty mcpServers object"}
	}
	for _, server := range servers.sortedKeys() {
		where := label + " " + name + " " + server + ": "
		declared, ok := asDict(servers.get(server))
		if !ok {
			errs = append(errs, where+"a server must be an object")
			continue
		}
		var words []string
		if command, ok := declared.get("command").(string); ok && nonempty(command) {
			words = append(words, command)
		} else {
			errs = append(errs, where+"a server needs a command")
		}
		if declared.get("cwd") != "." {
			errs = append(errs, where+`cwd must be ".": a relative command resolves against it, and a server declared without one never starts`)
		}
		arguments, ok := declared.get("args").([]any)
		allStrings := ok
		for _, word := range arguments {
			if _, isString := word.(string); !isString {
				allStrings = false
			}
		}
		if !allStrings {
			errs = append(errs, where+"args must be a list of strings")
		} else {
			for _, word := range arguments {
				words = append(words, word.(string))
			}
		}
		for _, word := range words {
			switch {
			case strings.ContainsAny(word, "$%"):
				errs = append(errs, where+pyRepr(word)+" carries a variable; a plugin MCP server"+
					" runs without a shell and inherits no plugin root, so it would arrive as literal text")
			case strings.HasPrefix(word, "/"):
				errs = append(errs, where+pyRepr(word)+" is an absolute path; the package ships to hosts it has not seen")
			case strings.HasPrefix(word, "./") && !p.has(word[2:]):
				errs = append(errs, where+pyRepr(word)+" names a file the package does not ship")
			}
		}
		errs = append(errs, toolApprovalErrors(server, declared, where)...)
	}
	return errs
}

func toolApprovalErrors(server string, declared *pyDict, where string) []string {
	required := requiredToolApprovals[server]
	var errs []string
	if !declared.has("tools") {
		if len(required) > 0 {
			var gates []string
			for _, gate := range required {
				gates = append(gates, gate[0]+" with "+pyRepr(gate[1]))
			}
			errs = append(errs, where+"declares no tools, and this server must gate "+strings.Join(gates, ", ")+
				". The user configuration this package replaces carries that gate, and"+
				" the declaration is what serves the server once the table is removed")
		}
		return errs
	}
	tools, ok := asDict(declared.get("tools"))
	if !ok || len(tools.keys) == 0 {
		return []string{where + "tools must be a nonempty object; what a host does with an empty one is not measured"}
	}
	for _, tool := range tools.sortedKeys() {
		named := where + "tools." + tool + ": "
		if !nonempty(tool) {
			errs = append(errs, where+"a tool name must be a nonempty string")
			continue
		}
		gate, ok := asDict(tools.get(tool))
		if !ok {
			errs = append(errs, named+"a tool gate must be an object")
			continue
		}
		var unknown []string
		for _, key := range gate.sortedKeys() {
			if !slices.Contains(approvalKeys, key) {
				unknown = append(unknown, pyRepr(key))
			}
		}
		if len(unknown) > 0 {
			errs = append(errs, named+"carries "+strings.Join(unknown, ", ")+"; only "+strings.Join(approvalKeys, ", ")+
				" is checked here, and a declaration this host dislikes disappears without a word")
			continue
		}
		mode, isString := gate.get("approval_mode").(string)
		if !isString || !slices.Contains(approvalModes, mode) {
			errs = append(errs, named+"approval_mode "+pyReprValue(gate.get("approval_mode"))+" is not one of "+strings.Join(approvalModes, ", "))
		}
	}
	for _, gate := range required {
		var carried any
		if g, ok := asDict(tools.get(gate[0])); ok {
			carried = g.get("approval_mode")
		}
		if !pyEqual(carried, gate[1]) {
			errs = append(errs, where+"must gate "+gate[0]+" with "+pyRepr(gate[1])+", and it declares "+pyReprValue(carried))
		}
	}
	return errs
}

// yamlScalar is plugin.py's yaml_scalar: one quoted or bare scalar.
func yamlScalar(text string) (any, error) {
	text = pyStrip(text)
	if strings.HasPrefix(text, `"`) {
		return pyJSONLoadsOrdered(text)
	}
	if strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'") && len([]rune(text)) > 1 {
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil
	}
	return text, nil
}

func interfaceErrors(skillPath string, p payload, label string) []string {
	name := skillPath[strings.LastIndex(skillPath, "/")+1:]
	where := label + " " + skillPath + "/agents/openai.yaml: "
	file, ok := p[skillPath+"/agents/openai.yaml"]
	if !ok {
		return []string{where + pyRepr(skillPath+"/agents/openai.yaml")}
	}
	text, err := decodeUTF8(file.data)
	if err != nil {
		return []string{where + err.Error()}
	}
	values := map[string]any{}
	section := ""
	for _, line := range pySplitlines(text) {
		if pyIsBlank(line) || strings.HasPrefix(pyLStrip(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			section = pyStrip(line)
		} else if section == "interface:" {
			key, value, found := strings.Cut(pyStrip(line), ":")
			if _, seen := values[key]; !found || seen {
				return []string{where + "malformed interface metadata"}
			}
			scalar, err := yamlScalar(value)
			if err != nil {
				return []string{where + key + " is not a readable scalar (" + err.Error() + ")"}
			}
			values[key] = scalar
		}
	}
	var missing []string
	for _, key := range []string{"default_prompt", "display_name", "short_description"} {
		if _, ok := values[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return []string{where + "missing interface " + strings.Join(missing, ", ")}
	}
	if prompt, _ := values["default_prompt"].(string); !strings.Contains(prompt, "$"+name) {
		return []string{where + "default_prompt must name $" + name}
	}
	return nil
}

func interfaceOptionErrors(iface *pyDict, p payload, label string) []string {
	var errs []string
	for _, field := range httpsFields {
		if value := iface.get(field); value != nil && !httpsURL(value) {
			errs = append(errs, label+" manifest: interface."+field+" must be an https URL")
		}
	}
	if color := iface.get("brandColor"); color != nil {
		if text, ok := color.(string); !ok || !brandColor.MatchString(text) {
			errs = append(errs, label+" manifest: interface.brandColor must be #RRGGBB")
		}
	}
	type asset struct {
		field string
		value any
	}
	var assets []asset
	for _, field := range assetFields {
		if iface.has(field) {
			assets = append(assets, asset{field, iface.get(field)})
		}
	}
	if screenshots := iface.get("screenshots"); screenshots != nil {
		if shots, ok := screenshots.([]any); !ok || len(shots) == 0 {
			errs = append(errs, label+" manifest: interface.screenshots must be a nonempty list")
		} else {
			for _, shot := range shots {
				assets = append(assets, asset{"screenshots", shot})
			}
		}
	}
	for _, a := range assets {
		text, ok := a.value.(string)
		if !ok || !strings.HasPrefix(text, "./") {
			errs = append(errs, label+" manifest: interface."+a.field+" must be a ./ relative path")
		} else if p != nil && !p.has(text[2:]) {
			errs = append(errs, label+" manifest: interface."+a.field+" names "+pyRepr(text)+", which the package does not ship")
		}
	}
	return errs
}

// manifestErrors checks the manifest; rootName is "" for an installed tree, whose directory
// is named by version, and p is nil when no payload is known.
func manifestErrors(m *pyDict, rootName, label string, p payload) []string {
	var errs []string
	add := func(text string) { errs = append(errs, label+" manifest: "+text) }
	for _, field := range textFields {
		if !nonempty(m.get(field)) {
			add(field + " must be a nonempty string")
		}
	}
	if keywords, ok := m.get("keywords").([]any); !ok || len(keywords) == 0 {
		add("keywords must be a nonempty list")
	} else if !allNonempty(keywords) {
		add("keywords must be nonempty strings")
	}
	if m.get("license") != licenseID {
		add("license " + pyReprValue(m.get("license")) + " must be " + pyRepr(licenseID) + ", the license this repository ships")
	}
	author, isDict := asDict(m.get("author"))
	if !isDict || !nonempty(author.get("name")) {
		add("author.name is required")
	} else if extra := without(author.sortedKeys(), authorKeys); len(extra) > 0 {
		add("author carries unsupported keys " + pyReprList(extra))
	}
	if isDict {
		if author.has("email") && !nonempty(author.get("email")) {
			add("author.email must be a nonempty string")
		}
		if author.has("url") && !httpsURL(author.get("url")) {
			add("author.url must be an https URL with a host")
		}
	}
	for _, key := range without(m.sortedKeys(), manifestKeys) {
		add(pyRepr(key) + " is not a supported manifest key")
	}
	if rootName != "" && m.get("name") != rootName {
		add("name " + pyReprValue(m.get("name")) + " must match the plugin directory " + pyRepr(rootName))
	}
	if !semverPattern.MatchString(manifestVersion(m)) {
		add("version " + pyReprValue(m.get("version")) + " is not a semantic version")
	} else if p != nil {
		errs = append(errs, versionErrors(m, p, label)...)
	}
	if _, err := declaredSkillsPath(m); err != nil {
		add(err.Error())
	}
	iface, ok := asDict(m.get("interface"))
	if !ok {
		add("interface must be an object")
	} else {
		for _, key := range without(iface.sortedKeys(), append(slices.Clone(interfaceFields), interfaceOptional...)) {
			add("interface." + key + " is not a supported interface key")
		}
		for _, key := range iface.sortedKeys() {
			if !slices.Contains(interfaceOptional, key) {
				continue
			}
			value := iface.get(key)
			valid := nonempty(value)
			if list, isList := value.([]any); key == "screenshots" && isList {
				valid = allNonempty(list) && len(list) > 0
			}
			if !valid {
				add("interface." + key + " is present but not a usable value")
			}
		}
		errs = append(errs, interfaceOptionErrors(iface, p, label)...)
		for _, field := range interfaceFields {
			value := iface.get(field)
			list := field == "capabilities" || field == "defaultPrompt"
			valid := nonempty(value)
			if list {
				items, isList := value.([]any)
				valid = isList && len(items) > 0 && allNonempty(items)
			}
			if !valid {
				suffix := ""
				if list {
					suffix = " list"
				}
				add("interface." + field + " must be a nonempty string" + suffix)
			}
		}
	}
	declared, _, componentErrs := declaredComponents(m)
	for _, problem := range componentErrs {
		add(problem)
	}
	if p != nil {
		for _, c := range declared {
			file, ok := p[c.relative]
			if !ok {
				add(c.field + " names " + pyRepr("./"+c.relative) + ", which the package does not ship")
				continue
			}
			if c.field == "hooks" {
				errs = append(errs, hookDocumentErrors(c.relative, file.data, label)...)
			} else {
				errs = append(errs, mcpDocumentErrors(c.relative, file.data, p, label)...)
			}
		}
	}
	return errs
}

func allNonempty(items []any) bool {
	for _, item := range items {
		if !nonempty(item) {
			return false
		}
	}
	return true
}

// without is sorted(set(items) - set(allowed)) for already sorted items.
func without(items, allowed []string) []string {
	var out []string
	for _, item := range items {
		if !slices.Contains(allowed, item) {
			out = append(out, item)
		}
	}
	return out
}

// errNotIterable stands in for the TypeError plugin.py raises iterating a catalog whose
// plugins value is a number, boolean or null.
var errNotIterable = errors.New("marketplace: plugins must be a list")

func marketplaceErrors(catalog, m *pyDict) ([]string, error) {
	var errs []string
	if !pyEqual(catalog.get("name"), m.get("name")) {
		errs = append(errs, "marketplace: name "+pyReprValue(catalog.get("name"))+" must match the plugin name "+pyReprValue(m.get("name")))
	}
	var expected any
	declared, declaredIsDict := asDict(m.get("interface"))
	if declaredIsDict {
		expected = declared.get("displayName")
	}
	if iface, ok := asDict(catalog.get("interface")); !ok || !pyEqual(iface.get("displayName"), expected) {
		errs = append(errs, "marketplace: interface.displayName must match the manifest display name "+pyReprValue(expected))
	}
	var candidates []any
	switch plugins := catalog.get("plugins").(type) {
	case []any:
		candidates = plugins
	case string, *pyDict:
	default:
		if catalog.has("plugins") {
			return nil, errNotIterable
		}
	}
	var entries []*pyDict
	for _, candidate := range candidates {
		if e, ok := asDict(candidate); ok && pyEqual(e.get("name"), m.get("name")) {
			entries = append(entries, e)
		}
	}
	if len(entries) != 1 {
		return append(errs, "marketplace: expected exactly one entry named "+pyReprValue(m.get("name"))), nil
	}
	e := entries[0]
	source, ok := asDict(e.get("source"))
	if !ok {
		errs = append(errs, "marketplace: entry source must be an object")
		source = &pyDict{vals: map[string]any{}}
	}
	if source.get("source") != "local" {
		errs = append(errs, "marketplace: entry source.source must be local")
	}
	if path := source.get("path"); path != "./"+pluginRelative {
		errs = append(errs, "marketplace: entry source.path "+pyReprValue(path)+" must be "+pyRepr("./"+pluginRelative))
	}
	policy, ok := asDict(e.get("policy"))
	if !ok {
		errs = append(errs, "marketplace: entry policy must be an object")
		policy = &pyDict{vals: map[string]any{}}
	}
	if policy.get("installation") != "AVAILABLE" {
		errs = append(errs, "marketplace: entry policy.installation must be AVAILABLE")
	}
	if policy.get("authentication") != "ON_USE" {
		errs = append(errs, "marketplace: entry policy.authentication must be ON_USE; installation does not create credentials")
	}
	var category any
	if declaredIsDict {
		category = declared.get("category")
	}
	if text, ok := e.get("category").(string); !ok || !pyEqual(text, category) {
		errs = append(errs, "marketplace: entry category must be the manifest category "+pyReprValue(category))
	}
	return errs, nil
}

// personalPath finds the first personal home path in text, pattern by pattern.
func personalPath(text string) string {
	for _, home := range homePaths {
		if !home.lookbehind {
			if found := home.pattern.FindString(text); found != "" {
				return found
			}
			continue
		}
		for i := 0; i < len(text); i++ {
			if text[i] != '/' {
				continue
			}
			if i > 0 && strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-", text[i-1]) >= 0 {
				continue
			}
			if found := home.pattern.FindString(text[i:]); found != "" {
				return found
			}
		}
	}
	return ""
}

func hygiene(p payload, m *pyDict, label string) []string {
	var errs []string
	for _, required := range requiredFiles {
		if !p.has(required) {
			errs = append(errs, label+": "+required+" must ship with the package")
		}
	}
	_, roots, _ := declaredComponents(m)
	for _, name := range p.names() {
		parts := pyPurePath(name)
		if !slices.Contains(roots, parts[0]) {
			errs = append(errs, label+" "+name+": only "+strings.Join(roots, ", ")+
				" may ship in the package; a component the manifest does not declare installs without ever loading")
		}
		for _, part := range parts {
			if forbiddenNames.MatchString(part) {
				errs = append(errs, label+" "+name+": operational state and credentials may not ship")
				break
			}
		}
		text, err := decodeUTF8(p[name].data)
		if err != nil {
			continue
		}
		if found := personalPath(text); found != "" {
			errs = append(errs, label+" "+name+": contains the personal path "+pyRepr(found)+
				"; the package must not require one account checkout")
		}
	}
	return errs
}

// skillSet is plugin.py's skills(): the skill set is the declared directory's children.
func skillSet(p payload, m *pyDict, label string) ([]string, map[string]map[string]bool) {
	prefix, err := declaredSkillsPath(m)
	if err != nil {
		return []string{label + ": " + err.Error()}, map[string]map[string]bool{}
	}
	var errs []string
	found := map[string]map[string]bool{}
	prefixParts := strings.Split(prefix, "/")
	for name := range p {
		parts := pyPurePath(name)
		if len(parts) < len(prefixParts)+2 || !slices.Equal(parts[:len(prefixParts)], prefixParts) {
			continue
		}
		skill := parts[len(prefixParts)]
		if found[skill] == nil {
			found[skill] = map[string]bool{}
		}
		found[skill][strings.Join(parts[len(prefixParts)+1:], "/")] = true
	}
	if len(found) == 0 {
		errs = append(errs, label+": the declared skills path ships no skill")
	}
	declared, _, _ := declaredComponents(m)
	for _, name := range p.names() {
		parts := pyPurePath(name)
		isComponent := false
		for _, c := range declared {
			isComponent = isComponent || c.relative == name ||
				strings.HasPrefix(name, strings.Split(c.relative, "/")[0]+"/")
		}
		if slices.Contains(alwaysRoots, parts[0]) || isComponent ||
			(len(parts) >= len(prefixParts) && slices.Equal(parts[:len(prefixParts)], prefixParts)) {
			continue
		}
		errs = append(errs, label+" "+name+": ships outside every declared component path")
	}
	for _, skill := range sortedKeys(found) {
		for _, required := range []string{"SKILL.md", "agents/openai.yaml"} {
			if !found[skill][required] {
				errs = append(errs, label+" "+prefix+"/"+skill+": missing "+required)
			}
		}
		if found[skill]["agents/openai.yaml"] {
			errs = append(errs, interfaceErrors(prefix+"/"+skill, p, label)...)
		}
	}
	return errs, found
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// errSkillsPathCrash stands in for the ValueError plugin.py raises (uncaught) when the root
// link is checked against a manifest with no usable skills path.
var errSkillsPathCrash = errors.New("skills path unusable")

func (c *pluginChecker) compatibilityLinkErrors(revision string, m *pyDict) ([]string, error) {
	listing, err := c.gitText("ls-tree", "-z", revision, "--", "skills")
	if err != nil {
		return nil, err
	}
	record, _, _ := strings.Cut(listing, "\x00")
	if record == "" {
		return []string{"skills: the repository root must keep a link to the packaged skills"}, nil
	}
	meta, _, _ := strings.Cut(record, "\t")
	fields := strings.SplitN(meta, " ", 3)
	if fields[0] != "120000" || fields[1] != "blob" {
		return []string{"skills: the repository root entry must be a symlink to the packaged skills"}, nil
	}
	target, err := c.git("cat-file", "blob", fields[2])
	if err != nil {
		return nil, err
	}
	skillsPath, err := declaredSkillsPath(m)
	if err != nil {
		return nil, errSkillsPathCrash
	}
	expected := pluginRelative + "/" + skillsPath
	if string(target) != expected {
		return []string{"skills: the root link points at " + pyRepr(decodeReplace(target)) + " instead of " +
			pyRepr(expected) + "; both installation paths must read one source"}, nil
	}
	return nil, nil
}

// report is the --json result, keys in sort_keys order when printed.
type report map[string]any

func reportPayload(p payload, m *pyDict, found map[string]map[string]bool, extra report) report {
	skills := sortedKeys(found)
	name := ""
	if m.has("name") {
		name = pyStr(m.get("name"))
	}
	var expected []string
	for _, skill := range skills {
		expected = append(expected, name+":"+skill)
	}
	sort.Strings(expected)
	r := report{"digest": payloadDigest(p), "files": len(p), "version": m.get("version"),
		"skills": nonNil(skills), "expectedSkillNames": nonNil(expected)}
	for key, value := range extra {
		r[key] = value
	}
	return r
}

func (r report) json() string {
	var object jsonObject
	for _, key := range sortedKeys(r) {
		object = append(object, jsonKV{key, r[key]})
	}
	return pyJSONIndent(object, 2)
}

func checkInstalled(path string) ([]string, report, error) {
	p, errs := directoryPayload(path)
	m, err := readManifest(p, "installed")
	if err != nil {
		return nil, nil, err
	}
	errs = append(errs, manifestErrors(m, "", "installed", p)...)
	errs = append(errs, hygiene(p, m, "installed")...)
	skillErrs, found := skillSet(p, m, "installed")
	return append(errs, skillErrs...), reportPayload(p, m, found, report{"source": "payload", "path": path}), nil
}

func (c *pluginChecker) recordVersion(stdout, stderr io.Writer) int {
	pluginRoot := filepath.Join(c.root, pluginRelative)
	p, errs := directoryPayload(pluginRoot)
	m, err := readManifest(p, "working tree")
	if err != nil {
		return failf(stderr, "%s", err)
	}
	version := manifestVersion(m)
	if !semverPattern.MatchString(version) {
		errs = append(errs, "working tree manifest: version "+pyRepr(version)+
			" is not a semantic version, so no payload suffix can be recorded under it")
	}
	if len(errs) > 0 {
		return failf(stderr, "%s", strings.Join(sortedSet(errs), "\n"))
	}
	expected, err := payloadVersion(p, version)
	if err != nil {
		return failf(stderr, "working tree manifest: %s", err)
	}
	path := filepath.Join(pluginRoot, manifestPath)
	document, err := readText(path)
	if err != nil {
		return failf(stderr, "%s", err)
	}
	recorded := pyJSONString(version)
	if count := strings.Count(document, recorded); count != 1 {
		return failf(stderr, "%s spells %s %d times; exactly one of them is the version to rewrite", manifestPath, pyRepr(version), count)
	}
	if version != expected {
		info, err := os.Stat(path)
		if err != nil {
			return failf(stderr, "%s", err)
		}
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(document, recorded, pyJSONString(expected))), info.Mode().Perm()); err != nil {
			return failf(stderr, "%s", err)
		}
		fmt.Fprintf(stdout, "Version %s in %s, recorded from %d shipped files. Commit it: the release payload is read from the revision, not from this tree\n",
			expected, manifestPath, len(p))
		return 0
	}
	fmt.Fprintf(stdout, "Version %s in %s: already recorded\n", expected, manifestPath)
	return 0
}

func (c *pluginChecker) checkRevision(revision string) ([]string, report, error) {
	out, err := c.gitText("rev-parse", revision)
	if err != nil {
		return nil, nil, err
	}
	resolved := pyStrip(out)
	release, errs, err := c.revisionPayload(resolved)
	if err != nil {
		return nil, nil, err
	}
	m, err := readManifest(release, "release")
	if err != nil {
		return nil, nil, err
	}
	pluginName := filepath.Base(pluginRelative)
	errs = append(errs, manifestErrors(m, pluginName, "release", release)...)
	errs = append(errs, hygiene(release, m, "release")...)
	skillErrs, found := skillSet(release, m, "release")
	errs = append(errs, skillErrs...)
	catalogText, err := c.gitText("show", resolved+":"+marketplacePath)
	errs, err = c.catalogErrors(errs, "marketplace: ", catalogText, err, m)
	if err != nil {
		return nil, nil, err
	}
	licenseBlob, err := c.git("show", resolved+":LICENSE")
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(release["LICENSE"].data, licenseBlob) {
		errs = append(errs, "release LICENSE: the package copy must match the repository license")
	}
	linkErrs, err := c.compatibilityLinkErrors(resolved, m)
	if err != nil {
		return nil, nil, err
	}
	errs = append(errs, linkErrs...)
	working, workingErrs := directoryPayload(filepath.Join(c.root, pluginRelative))
	errs = append(errs, workingErrs...)
	workingManifest, err := readManifest(working, "working tree")
	if err != nil {
		errs = append(errs, err.Error())
	}
	hygieneManifest := workingManifest
	if hygieneManifest == nil || len(hygieneManifest.keys) == 0 {
		hygieneManifest = &pyDict{vals: map[string]any{}}
	}
	errs = append(errs, hygiene(working, hygieneManifest, "working tree")...)
	if workingManifest != nil {
		errs = append(errs, manifestErrors(workingManifest, pluginName, "working tree", working)...)
		workingSkillErrs, workingFound := skillSet(working, workingManifest, "working tree")
		errs = append(errs, workingSkillErrs...)
		for _, name := range sortedKeys(found) {
			if _, ok := workingFound[name]; !ok {
				errs = append(errs, "working tree: "+name+" ships in the revision but is missing here")
			}
		}
		for _, name := range sortedKeys(workingFound) {
			if _, ok := found[name]; !ok {
				errs = append(errs, "working tree: "+name+" is not part of the revision payload")
			}
		}
		catalogNow, readErr := readText(filepath.Join(c.root, marketplacePath))
		errs, err = c.catalogErrors(errs, "working tree marketplace: ", catalogNow, readErr, workingManifest)
		if err != nil {
			return nil, nil, err
		}
		repoLicense, _ := os.ReadFile(filepath.Join(c.root, "LICENSE"))
		if !bytes.Equal(working["LICENSE"].data, repoLicense) {
			errs = append(errs, "working tree LICENSE: the package copy must match the repository license")
		}
		if skillsPath, err := declaredSkillsPath(workingManifest); err == nil {
			expected := pluginRelative + "/" + skillsPath
			if target, err := os.Readlink(filepath.Join(c.root, "skills")); err != nil || target != expected {
				errs = append(errs, "working tree skills: the repository root link must be a symlink to "+expected)
			}
		}
	}
	status, err := c.gitText("status", "--porcelain", "--ignored", "--", pluginRelative)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range pySplitlines(status) {
		if strings.HasPrefix(line, "??") || strings.HasPrefix(line, "!!") {
			errs = append(errs, "working tree "+pyStrip(line[min(3, len(line)):])+": untracked or ignored files "+
				"inside the plugin root are copied into the cache; commit or remove it")
		}
	}
	var drift []string
	for name := range release {
		if w, ok := working[name]; !ok || w.mode != release[name].mode || !bytes.Equal(w.data, release[name].data) {
			drift = append(drift, name)
		}
	}
	for name := range working {
		if !release.has(name) {
			drift = append(drift, name)
		}
	}
	sort.Strings(drift)
	return errs, reportPayload(release, m, found, report{"source": "revision", "revision": revision,
		"resolved": resolved, "worktreeDigest": payloadDigest(working), "worktreeDrift": nonNil(drift)}), nil
}

// catalogErrors reads a marketplace document (text, or the error reading it) and appends
// its findings; a read or decode failure is itself a finding under prefix.
func (c *pluginChecker) catalogErrors(errs []string, prefix, text string, readErr error, m *pyDict) ([]string, error) {
	if readErr != nil {
		var pkg packageError
		var val valueError
		var osErr osError
		if !errors.As(readErr, &pkg) && !errors.As(readErr, &val) && !errors.As(readErr, &osErr) {
			return nil, readErr
		}
		return append(errs, prefix+readErr.Error()), nil
	}
	value, err := pyJSONLoadsOrdered(text)
	if err != nil {
		return append(errs, prefix+err.Error()), nil
	}
	catalog, ok := asDict(value)
	if !ok {
		return append(errs, prefix+"the marketplace file must be a JSON object"), nil
	}
	found, err := marketplaceErrors(catalog, m)
	if err != nil {
		return append(errs, err.Error()), nil
	}
	return append(errs, found...), nil
}

// Plugin is `crw-dev ci plugin`: validate the Codex plugin package this repository publishes.
func Plugin(args []string, stdout, stderr io.Writer) int {
	values, present, code := parseOptions("plugin", "Validate the Codex plugin package this repository publishes.",
		[]string{"revision", "payload"}, []string{"record-version", "json"}, args, stdout, stderr)
	if code >= 0 {
		return code
	}
	revision := "HEAD"
	if present["revision"] {
		revision = values["revision"]
	}
	checker := &pluginChecker{}
	needRoot := present["record-version"] || !present["payload"]
	if needRoot {
		out, err := runGit(".", "rev-parse", "--show-toplevel")
		if err != nil {
			return failf(stderr, "plugin: not inside a Git checkout: %s", err)
		}
		checker.root = realpath(strings.TrimSpace(string(out)))
	}
	var errs []string
	var result report
	var err error
	switch {
	case present["record-version"]:
		return checker.recordVersion(stdout, stderr)
	case present["payload"]:
		errs, result, err = checkInstalled(realpath(values["payload"]))
	default:
		errs, result, err = checker.checkRevision(revision)
	}
	if err != nil {
		return failf(stderr, "%s", err)
	}
	if len(errs) > 0 {
		return failf(stderr, "%s", strings.Join(sortedSet(errs), "\n"))
	}
	if present["json"] {
		fmt.Fprintln(stdout, result.json())
		return 0
	}
	where := result["path"]
	if resolved, ok := result["resolved"]; ok {
		where = resolved
	}
	fmt.Fprintf(stdout, "Package %s at %s: %d files, digest %s. The version's suffix is these same files digested with that suffix elided\n",
		pyStr(result["version"]), where, result["files"], result["digest"].(string)[:16])
	fmt.Fprintln(stdout, "Skill names under the plugin namespace: "+strings.Join(result["expectedSkillNames"].([]string), ", "))
	if drift, _ := result["worktreeDrift"].([]string); len(drift) > 0 {
		fmt.Fprintln(stdout, "Working tree differs from the revision payload: "+strings.Join(drift, ", "))
	}
	return 0
}
