//go:build dev

package ci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// The package rules: what a manifest may declare and what the payload may ship.
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
	forbiddenNames = regexp.MustCompile(`(?i)^(\.git|\.codexclaw|\.env(\..*)?|id_(rsa|dsa|ecdsa|ed25519)(\..*)?` +
		`|\.netrc|\.pgpass|\.htpasswd|\.npmrc|authorized_keys` +
		`|.*credentials?(?:[._-].*)?|.*secrets?(?:[._-].*)?` +
		`|.*\.(sqlite3?|db|pem|key|p12|pfx))$`)
	httpsFields = []string{"websiteURL", "privacyPolicyURL", "termsOfServiceURL"}
	assetFields = []string{"composerIcon", "logo", "logoDark"}
	brandColor  = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

// packageError is a fact the check could not read, so it gives no verdict.
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
			return nil, packageError{strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "\ufffd"))}
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
	if !utf8.Valid(out) {
		return "", fmt.Errorf("git %s: %w", args[0], errNotUTF8)
	}
	return string(out), nil
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
		name := strings.Join(pathParts(path)[len(pathParts(pluginRelative)):], "/")
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
// regular file, so a pipe cannot hold the check (and a lock its caller holds) open.
func regularBytes(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return io.ReadAll(file)
}

// payloadDigest is sha256 over length-framed "<len>:<name> <mode> <sha256>" lines in name order.
// Its first 12 hex digits are the version suffix the manifest records, so these bytes are fixed.
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
	recorded, plain := []byte(show(version)), []byte(show(release))
	if count := bytes.Count(manifest.data, recorded); count != 1 {
		return nil, fmt.Errorf("%s spells %q %d times; the suffix has to be elided exactly once for the digest beneath it to be derived at all",
			manifestPath, version, count)
	}
	out := maps.Clone(p)
	out[manifestPath] = entry{manifest.mode, bytes.ReplaceAll(manifest.data, recorded, plain)}
	return out, nil
}

func payloadVersion(p payload, version string) (string, error) {
	release, _ := splitVersion(version)
	elided, err := versionPayload(p, version)
	if err != nil {
		return "", err
	}
	return release + "+" + payloadDigest(elided)[:payloadSuffixLength], nil
}

// manifestVersion is the manifest's version as text, "" when it has none.
func manifestVersion(m map[string]any) string {
	if !has(m, "version") {
		return ""
	}
	return text(m["version"])
}

func versionErrors(m map[string]any, p payload, label string) []string {
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
	return []string{fmt.Sprintf("%s manifest: version %q does not name this payload. Record %q; `--record-version` writes it into the working"+
		" tree manifest. The suffix is this payload's own digest, because two packages"+
		" that ship different bytes may not offer one version", label, version, expected)}
}

func readManifest(p payload, label string) (map[string]any, error) {
	file, ok := p[manifestPath]
	if !ok {
		return nil, packageError{label + ": " + manifestPath + " is missing from the package"}
	}
	value, err := decodeJSON(file.data)
	if err != nil {
		return nil, packageError{label + " " + manifestPath + ": " + err.Error()}
	}
	m, ok := object(value)
	if !ok {
		return nil, packageError{label + " " + manifestPath + ": the manifest must be a JSON object"}
	}
	return m, nil
}

// insidePath is a ./ relative path that stays in the package, without its ./.
func insidePath(declared any) (string, bool) {
	text, ok := declared.(string)
	if !ok || !strings.HasPrefix(text, "./") {
		return "", false
	}
	parts := pathParts(strings.Trim(text[2:], "/"))
	if len(parts) == 0 || slices.Contains(parts, "..") {
		return "", false
	}
	return strings.Join(parts, "/"), true
}

func declaredSkillsPath(m map[string]any) (string, error) {
	declared, ok := m["skills"].(string)
	if !ok || !strings.HasPrefix(declared, "./") {
		return "", errors.New("manifest must declare skills as a ./ relative path")
	}
	relative, ok := insidePath(declared)
	if !ok {
		return "", errors.New("declared skills path must stay inside the plugin root")
	}
	return relative, nil
}

// SkillsRoot is the skills directory the plugin manifest under the checkout root declares,
// with symlinks resolved: the directory a linked installation links from, so the linked and
// the packaged installation read one source. It is refused when the declaration is not a ./
// path inside the plugin, when it resolves outside the plugin through a symlink, or when it is
// not a directory.
func SkillsRoot(root string) (string, error) {
	manifest := filepath.Join(root, pluginRelative, manifestPath)
	data, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	value, err := decodeJSON(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", manifest, err)
	}
	m, ok := object(value)
	if !ok {
		return "", fmt.Errorf("%s: the manifest must be a JSON object", manifest)
	}
	relative, err := declaredSkillsPath(m)
	if err != nil {
		return "", fmt.Errorf("%s: %w", manifest, err)
	}
	pluginRoot := resolve(filepath.Join(root, pluginRelative))
	skills := resolve(filepath.Join(pluginRoot, filepath.FromSlash(relative)))
	if !strings.HasPrefix(skills, pluginRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: the declared skills path resolves outside %s", skills, pluginRoot)
	}
	if info, err := os.Stat(skills); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s: the declared skills directory does not exist", skills)
	}
	return skills, nil
}

func declaredHooks(m map[string]any) ([]any, error) {
	switch declared := m["hooks"].(type) {
	case nil:
		return nil, nil
	case string:
		return []any{declared}, nil
	case []any:
		return declared, nil
	}
	return nil, errors.New("hooks must be a ./ relative path or a list of them; an inline document does not load")
}

type component struct{ field, relative string }

func declaredComponents(m map[string]any) ([]component, []string, []string) {
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
	mcp := m["mcpServers"]
	if _, inline := object(mcp); inline {
		errs = append(errs, "mcpServers must name a ./ relative file: an inline table would ship a server this check never reads")
		mcp = nil
	}
	if mcp != nil {
		declarations = append(declarations, declaration{"mcpServers", mcp})
	}
	for _, d := range declarations {
		relative, ok := insidePath(d.value)
		if !ok {
			errs = append(errs, d.field+" "+show(d.value)+" must be a ./ relative path inside the plugin root")
			continue
		}
		paths = append(paths, component{d.field, relative})
		roots = append(roots, strings.Split(relative, "/")[0])
	}
	if has(m, "apps") {
		errs = append(errs, "apps is not declared by this package")
	}
	return paths, sortedSet(roots), errs
}

// nonempty is a string that is not blank; ingestion treats whitespace-only as absent.
func nonempty(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}

func loadDocument(name string, data []byte, label string) (any, []string) {
	document, err := decodeJSON(data)
	if err != nil {
		return nil, []string{label + " " + name + ": " + err.Error()}
	}
	return document, nil
}

func hookDocumentErrors(name string, data []byte, label string) []string {
	document, errs := loadDocument(name, data, label)
	if errs != nil {
		return errs
	}
	var events map[string]any
	if d, ok := object(document); ok {
		events, _ = object(d["hooks"])
	}
	if len(events) == 0 {
		return []string{label + " " + name + ": a hook file must hold a nonempty hooks object"}
	}
	prefix := label + " " + name + ": "
	for _, event := range sortedKeys(events) {
		groups, ok := events[event].([]any)
		if !ok || len(groups) == 0 {
			errs = append(errs, prefix+event+" must hold a nonempty list")
			continue
		}
		for _, group := range groups {
			var entries []any
			if g, ok := object(group); ok {
				entries, _ = g["hooks"].([]any)
			}
			if len(entries) == 0 {
				errs = append(errs, prefix+"every "+event+" group must hold a nonempty hooks list")
				continue
			}
			for _, item := range entries {
				hook, ok := object(item)
				if !ok || hook["type"] != "command" {
					errs = append(errs, prefix+"every hook must be a command hook")
					continue
				}
				if !nonempty(hook["command"]) {
					errs = append(errs, prefix+"every hook needs a command")
				}
				timeout, isInt := integer(hook["timeout"])
				if !isInt || timeout <= 0 {
					errs = append(errs, prefix+"every hook needs a positive integer timeout")
				} else if timeout > hookTimeoutSeconds {
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
	var servers map[string]any
	if d, ok := object(document); ok {
		servers, _ = object(d["mcpServers"])
	}
	if len(servers) == 0 {
		return []string{label + " " + name + ": an MCP file must hold a nonempty mcpServers object"}
	}
	for _, server := range sortedKeys(servers) {
		where := label + " " + name + " " + server + ": "
		declared, ok := object(servers[server])
		if !ok {
			errs = append(errs, where+"a server must be an object")
			continue
		}
		var words []string
		if command, ok := declared["command"].(string); ok && nonempty(command) {
			words = append(words, command)
		} else {
			errs = append(errs, where+"a server needs a command")
		}
		if declared["cwd"] != "." {
			errs = append(errs, where+`cwd must be ".": a relative command resolves against it, and a server declared without one never starts`)
		}
		arguments, ok := declared["args"].([]any)
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
				errs = append(errs, where+strconv.Quote(word)+" carries a variable; a plugin MCP server"+
					" runs without a shell and inherits no plugin root, so it would arrive as literal text")
			case strings.HasPrefix(word, "/"):
				errs = append(errs, where+strconv.Quote(word)+" is an absolute path; the package ships to hosts it has not seen")
			case strings.HasPrefix(word, "./") && !p.has(word[2:]):
				errs = append(errs, where+strconv.Quote(word)+" names a file the package does not ship")
			}
		}
		errs = append(errs, toolApprovalErrors(server, declared, where)...)
	}
	return errs
}

func toolApprovalErrors(server string, declared map[string]any, where string) []string {
	required := requiredToolApprovals[server]
	var errs []string
	if !has(declared, "tools") {
		if len(required) > 0 {
			var gates []string
			for _, gate := range required {
				gates = append(gates, fmt.Sprintf("%s with %q", gate[0], gate[1]))
			}
			errs = append(errs, where+"declares no tools, and this server must gate "+strings.Join(gates, ", ")+
				". The user configuration this package replaces carries that gate, and"+
				" the declaration is what serves the server once the table is removed")
		}
		return errs
	}
	tools, ok := object(declared["tools"])
	if !ok || len(tools) == 0 {
		return []string{where + "tools must be a nonempty object; what a host does with an empty one is not measured"}
	}
	for _, tool := range sortedKeys(tools) {
		named := where + "tools." + tool + ": "
		if !nonempty(tool) {
			errs = append(errs, where+"a tool name must be a nonempty string")
			continue
		}
		gate, ok := object(tools[tool])
		if !ok {
			errs = append(errs, named+"a tool gate must be an object")
			continue
		}
		var unknown []string
		for _, key := range sortedKeys(gate) {
			if !slices.Contains(approvalKeys, key) {
				unknown = append(unknown, strconv.Quote(key))
			}
		}
		if len(unknown) > 0 {
			errs = append(errs, named+"carries "+strings.Join(unknown, ", ")+"; only "+strings.Join(approvalKeys, ", ")+
				" is checked here, and a declaration this host dislikes disappears without a word")
			continue
		}
		mode, isString := gate["approval_mode"].(string)
		if !isString || !slices.Contains(approvalModes, mode) {
			errs = append(errs, named+"approval_mode "+show(gate["approval_mode"])+" is not one of "+strings.Join(approvalModes, ", "))
		}
	}
	for _, gate := range required {
		var carried any
		if g, ok := object(tools[gate[0]]); ok {
			carried = g["approval_mode"]
		}
		if carried != gate[1] {
			errs = append(errs, fmt.Sprintf("%smust gate %s with %q, and it declares %s", where, gate[0], gate[1], show(carried)))
		}
	}
	return errs
}

// yamlScalar is one quoted or bare YAML scalar: a double-quoted one reads as JSON.
func yamlScalar(text string) (any, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, `"`) {
		return decodeJSON([]byte(text))
	}
	if strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'") && len(text) > 1 {
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil
	}
	return text, nil
}

func interfaceErrors(skillPath string, p payload, label string) []string {
	name := skillPath[strings.LastIndex(skillPath, "/")+1:]
	where := label + " " + skillPath + "/agents/openai.yaml: "
	file := p[skillPath+"/agents/openai.yaml"]
	if !utf8.Valid(file.data) {
		return []string{where + errNotUTF8.Error()}
	}
	values := map[string]any{}
	section := ""
	for _, line := range lines(string(file.data)) {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			section = strings.TrimSpace(line)
		} else if section == "interface:" {
			key, value, found := strings.Cut(strings.TrimSpace(line), ":")
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

// httpsURL is an https URL with a host.
func httpsURL(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	u, err := url.Parse(text)
	return err == nil && u.Scheme == "https" && u.Hostname() != ""
}

func interfaceOptionErrors(iface map[string]any, p payload, label string) []string {
	var errs []string
	for _, field := range httpsFields {
		if value := iface[field]; value != nil && !httpsURL(value) {
			errs = append(errs, label+" manifest: interface."+field+" must be an https URL")
		}
	}
	if color := iface["brandColor"]; color != nil {
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
		if has(iface, field) {
			assets = append(assets, asset{field, iface[field]})
		}
	}
	if screenshots := iface["screenshots"]; screenshots != nil {
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
			errs = append(errs, label+" manifest: interface."+a.field+" names "+strconv.Quote(text)+", which the package does not ship")
		}
	}
	return errs
}

// manifestErrors checks the manifest; rootName is "" for an installed tree, whose directory
// is named by version, and p is nil when no payload is known.
func manifestErrors(m map[string]any, rootName, label string, p payload) []string {
	var errs []string
	add := func(text string) { errs = append(errs, label+" manifest: "+text) }
	for _, field := range textFields {
		if !nonempty(m[field]) {
			add(field + " must be a nonempty string")
		}
	}
	if keywords, ok := m["keywords"].([]any); !ok || len(keywords) == 0 {
		add("keywords must be a nonempty list")
	} else if !allNonempty(keywords) {
		add("keywords must be nonempty strings")
	}
	if m["license"] != licenseID {
		add(fmt.Sprintf("license %s must be %q, the license this repository ships", show(m["license"]), licenseID))
	}
	author, isObject := object(m["author"])
	if !isObject || !nonempty(author["name"]) {
		add("author.name is required")
	} else if extra := without(sortedKeys(author), authorKeys); len(extra) > 0 {
		add("author carries unsupported keys " + show(extra))
	}
	if isObject {
		if has(author, "email") && !nonempty(author["email"]) {
			add("author.email must be a nonempty string")
		}
		if has(author, "url") && !httpsURL(author["url"]) {
			add("author.url must be an https URL with a host")
		}
	}
	for _, key := range without(sortedKeys(m), manifestKeys) {
		add(strconv.Quote(key) + " is not a supported manifest key")
	}
	if rootName != "" && m["name"] != rootName {
		add(fmt.Sprintf("name %s must match the plugin directory %q", show(m["name"]), rootName))
	}
	if !semverPattern.MatchString(manifestVersion(m)) {
		add("version " + show(m["version"]) + " is not a semantic version")
	} else if p != nil {
		errs = append(errs, versionErrors(m, p, label)...)
	}
	if _, err := declaredSkillsPath(m); err != nil {
		add(err.Error())
	}
	iface, ok := object(m["interface"])
	if !ok {
		add("interface must be an object")
	} else {
		for _, key := range without(sortedKeys(iface), append(slices.Clone(interfaceFields), interfaceOptional...)) {
			add("interface." + key + " is not a supported interface key")
		}
		for _, key := range sortedKeys(iface) {
			if !slices.Contains(interfaceOptional, key) {
				continue
			}
			value := iface[key]
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
			value := iface[field]
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
				add(c.field + " names " + strconv.Quote("./"+c.relative) + ", which the package does not ship")
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

// without is the items allowed does not hold, in their order.
func without(items, allowed []string) []string {
	var out []string
	for _, item := range items {
		if !slices.Contains(allowed, item) {
			out = append(out, item)
		}
	}
	return out
}

func marketplaceErrors(catalog, m map[string]any) []string {
	var errs []string
	if !same(catalog["name"], m["name"]) {
		errs = append(errs, "marketplace: name "+show(catalog["name"])+" must match the plugin name "+show(m["name"]))
	}
	var expected any
	declared, declaredIsObject := object(m["interface"])
	if declaredIsObject {
		expected = declared["displayName"]
	}
	if iface, ok := object(catalog["interface"]); !ok || !same(iface["displayName"], expected) {
		errs = append(errs, "marketplace: interface.displayName must match the manifest display name "+show(expected))
	}
	candidates, isList := catalog["plugins"].([]any)
	if !isList && has(catalog, "plugins") {
		return append(errs, "marketplace: plugins must be a list")
	}
	var entries []map[string]any
	for _, candidate := range candidates {
		if e, ok := object(candidate); ok && same(e["name"], m["name"]) {
			entries = append(entries, e)
		}
	}
	if len(entries) != 1 {
		return append(errs, "marketplace: expected exactly one entry named "+show(m["name"]))
	}
	e := entries[0]
	source, ok := object(e["source"])
	if !ok {
		errs = append(errs, "marketplace: entry source must be an object")
	}
	if source["source"] != "local" {
		errs = append(errs, "marketplace: entry source.source must be local")
	}
	if path := source["path"]; path != "./"+pluginRelative {
		errs = append(errs, fmt.Sprintf("marketplace: entry source.path %s must be %q", show(path), "./"+pluginRelative))
	}
	policy, ok := object(e["policy"])
	if !ok {
		errs = append(errs, "marketplace: entry policy must be an object")
	}
	if policy["installation"] != "AVAILABLE" {
		errs = append(errs, "marketplace: entry policy.installation must be AVAILABLE")
	}
	if policy["authentication"] != "ON_USE" {
		errs = append(errs, "marketplace: entry policy.authentication must be ON_USE; installation does not create credentials")
	}
	var category any
	if declaredIsObject {
		category = declared["category"]
	}
	if text, ok := e["category"].(string); !ok || !same(text, category) {
		errs = append(errs, "marketplace: entry category must be the manifest category "+show(category))
	}
	return errs
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

func hygiene(p payload, m map[string]any, label string) []string {
	var errs []string
	for _, required := range requiredFiles {
		if !p.has(required) {
			errs = append(errs, label+": "+required+" must ship with the package")
		}
	}
	_, roots, _ := declaredComponents(m)
	for _, name := range p.names() {
		parts := pathParts(name)
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
		if !utf8.Valid(p[name].data) {
			continue
		}
		if found := personalPath(string(p[name].data)); found != "" {
			errs = append(errs, label+" "+name+": contains the personal path "+strconv.Quote(found)+
				"; the package must not require one account checkout")
		}
	}
	return errs
}

// skillSet is the skill set: the declared skills directory's children.
func skillSet(p payload, m map[string]any, label string) ([]string, map[string]map[string]bool) {
	prefix, err := declaredSkillsPath(m)
	if err != nil {
		return []string{label + ": " + err.Error()}, map[string]map[string]bool{}
	}
	var errs []string
	found := map[string]map[string]bool{}
	prefixParts := strings.Split(prefix, "/")
	for name := range p {
		parts := pathParts(name)
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
		parts := pathParts(name)
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

// report is the --json result, printed with sorted keys, one top-level member per line.
type report map[string]any

func reportPayload(p payload, m map[string]any, found map[string]map[string]bool, extra report) report {
	skills := sortedKeys(found)
	name := ""
	if has(m, "name") {
		name = text(m["name"])
	}
	var expected []string
	for _, skill := range skills {
		expected = append(expected, name+":"+skill)
	}
	sort.Strings(expected)
	r := report{"digest": payloadDigest(p), "files": len(p), "version": m["version"],
		"skills": nonNil(skills), "expectedSkillNames": nonNil(expected)}
	for key, value := range hookReport(p, m) {
		r[key] = value
	}
	for key, value := range extra {
		r[key] = value
	}
	return r
}

// hookReport is what the files the manifest names under hooks declare, read as the host loads
// them. stopHooks counts the hooks those files list under hooks.Stop, which is what a Stop runs:
// a Stop list anywhere else in a file, or in a file the manifest does not name, never loads.
// hooksDigest frames each named file, in the order the manifest names it, as payloadDigest
// frames a shipped file, so two payloads share it exactly when they name the same files in the
// same order with the same bytes.
func hookReport(p payload, m map[string]any) report {
	declared, _ := declaredHooks(m)
	stop := 0
	var blocks [][]byte
	for _, value := range declared {
		relative, inside := insidePath(value)
		file, shipped := p[relative]
		if !inside || !shipped {
			continue
		}
		sum := sha256.Sum256(file.data)
		blocks = append(blocks, fmt.Appendf(nil, "%d:%s %s %s", len(relative), relative, file.mode, hex.EncodeToString(sum[:])))
		document, err := decodeJSON(file.data)
		if err != nil {
			continue
		}
		var groups []any
		if d, ok := object(document); ok {
			if events, ok := object(d["hooks"]); ok {
				groups, _ = events["Stop"].([]any)
			}
		}
		for _, group := range groups {
			if g, ok := object(group); ok {
				entries, _ := g["hooks"].([]any)
				stop += len(entries)
			}
		}
	}
	sum := sha256.Sum256(bytes.Join(blocks, []byte("\n")))
	return report{"stopHooks": stop, "hooksDigest": hex.EncodeToString(sum[:])}
}

// json is the report as --json prints it: indented by two spaces, so each top-level member is
// one line that docs/plugin-packaging.md reads with sed.
func (r report) json() string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any(r)); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
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
		errs = append(errs, fmt.Sprintf("working tree manifest: version %q is not a semantic version, so no payload suffix can be recorded under it", version))
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
	recorded := show(version)
	if count := strings.Count(document, recorded); count != 1 {
		return failf(stderr, "%s spells %q %d times; exactly one of them is the version to rewrite", manifestPath, version, count)
	}
	if version != expected {
		info, err := os.Stat(path)
		if err != nil {
			return failf(stderr, "%s", err)
		}
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(document, recorded, show(expected))), info.Mode().Perm()); err != nil {
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
	resolved := strings.TrimSpace(out)
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
	errs = catalogErrors(errs, "marketplace: ", catalogText, err, m)
	licenseBlob, err := c.git("show", resolved+":LICENSE")
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(release["LICENSE"].data, licenseBlob) {
		errs = append(errs, "release LICENSE: the package copy must match the repository license")
	}
	working, workingErrs := directoryPayload(filepath.Join(c.root, pluginRelative))
	errs = append(errs, workingErrs...)
	workingManifest, err := readManifest(working, "working tree")
	if err != nil {
		errs = append(errs, err.Error())
	}
	hygieneManifest := workingManifest
	if hygieneManifest == nil {
		hygieneManifest = map[string]any{}
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
		errs = catalogErrors(errs, "working tree marketplace: ", catalogNow, readErr, workingManifest)
		repoLicense, _ := os.ReadFile(filepath.Join(c.root, "LICENSE"))
		if !bytes.Equal(working["LICENSE"].data, repoLicense) {
			errs = append(errs, "working tree LICENSE: the package copy must match the repository license")
		}
	}
	status, err := c.gitText("status", "--porcelain", "--ignored", "--", pluginRelative)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range lines(status) {
		if strings.HasPrefix(line, "??") || strings.HasPrefix(line, "!!") {
			errs = append(errs, "working tree "+strings.TrimSpace(line[min(3, len(line)):])+": untracked or ignored files "+
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
func catalogErrors(errs []string, prefix, text string, readErr error, m map[string]any) []string {
	if readErr != nil {
		return append(errs, prefix+readErr.Error())
	}
	value, err := decodeJSON([]byte(text))
	if err != nil {
		return append(errs, prefix+err.Error())
	}
	catalog, ok := object(value)
	if !ok {
		return append(errs, prefix+"the marketplace file must be a JSON object")
	}
	return append(errs, marketplaceErrors(catalog, m)...)
}

// Plugin is `crw-dev ci plugin`: validate the Codex plugin package this repository publishes.
func Plugin(args []string, stdout, stderr io.Writer) int {
	flags := newFlags("plugin")
	revision := flags.String("revision", "HEAD", "check the package as this Git revision ships it")
	payloadDir := flags.String("payload", "", "check an installed plugin directory instead of a revision")
	record := flags.Bool("record-version", false, "write the payload's version suffix into the working tree manifest")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if code := parseFlags(flags, "Validate the Codex plugin package this repository publishes.", args, stdout, stderr); code >= 0 {
		return code
	}
	checker := &pluginChecker{}
	if *record || !given(flags, "payload") {
		out, err := runGit(".", "rev-parse", "--show-toplevel")
		if err != nil {
			return failf(stderr, "plugin: not inside a Git checkout: %s", err)
		}
		checker.root = resolve(strings.TrimSpace(string(out)))
	}
	var errs []string
	var result report
	var err error
	switch {
	case *record:
		return checker.recordVersion(stdout, stderr)
	case given(flags, "payload"):
		errs, result, err = checkInstalled(resolve(*payloadDir))
	default:
		errs, result, err = checker.checkRevision(*revision)
	}
	if err != nil {
		return failf(stderr, "%s", err)
	}
	if len(errs) > 0 {
		return failf(stderr, "%s", strings.Join(sortedSet(errs), "\n"))
	}
	if *asJSON {
		fmt.Fprintln(stdout, result.json())
		return 0
	}
	where := result["path"]
	if resolved, ok := result["resolved"]; ok {
		where = resolved
	}
	fmt.Fprintf(stdout, "Package %s at %s: %d files, digest %s. The version's suffix is these same files digested with that suffix elided\n",
		text(result["version"]), where, result["files"], result["digest"].(string)[:16])
	fmt.Fprintln(stdout, "Skill names under the plugin namespace: "+strings.Join(result["expectedSkillNames"].([]string), ", "))
	if drift, _ := result["worktreeDrift"].([]string); len(drift) > 0 {
		fmt.Fprintln(stdout, "Working tree differs from the revision payload: "+strings.Join(drift, ", "))
	}
	return 0
}
