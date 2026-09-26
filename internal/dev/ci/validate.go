//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// pySpace is the class Python's str-pattern \s matches (str.isspace).
const pySpace = `\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	markdownLink = regexp.MustCompile(`\[[^\]\n]*\]\((<[^>\n]+>|[^` + pySpace + `)]+)(?:[` + pySpace + `]+"[^"]*")?\)`)
	fenceMarker  = regexp.MustCompile("^(`{3,}|~{3,})")
	skillName    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

// pyIsSpace is str.isspace for one rune.
func pyIsSpace(r rune) bool {
	return strings.ContainsRune("\t\n\v\f\r\x1c\x1d\x1e\x1f \u0085\u00a0\u1680\u2028\u2029\u202f\u205f\u3000", r) ||
		(r >= 0x2000 && r <= 0x200a)
}

func pyStrip(s string) string  { return strings.TrimFunc(s, pyIsSpace) }
func pyLStrip(s string) string { return strings.TrimLeftFunc(s, pyIsSpace) }
func pyIsBlank(s string) bool  { return pyStrip(s) == "" }
func pyStripChars(s, chars string) string {
	return strings.Trim(s, chars)
}

// pySplitlines is str.splitlines().
func pySplitlines(s string) []string {
	var lines []string
	start := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\r':
			lines = append(lines, string(runes[start:i]))
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			lines = append(lines, string(runes[start:i]))
			start = i + 1
		}
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}

// readText is Path.read_text(encoding="utf-8"): newlines are not translated... except that
// Python's universal newlines turn "\r\n" and "\r" into "\n".
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", valueErrorOS(err)
	}
	text, err := decodeUTF8(data)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}

// osError carries str(OSError).
type osError struct{ text string }

func (e osError) Error() string { return e.text }

func valueErrorOS(err error) error { return osError{pyOSErrorText(err)} }

// realpath is os.path.realpath(path) (Path.resolve(), non-strict): symlinks are followed as
// far as the path exists, and the missing remainder is appended lexically.
func realpath(path string) string {
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			path = filepath.Join(wd, path)
		}
	}
	resolved := "/"
	rest := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for hops := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		info, err := os.Lstat(next)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			resolved = next
			continue
		}
		target, err := os.Readlink(next)
		if hops++; err != nil || hops > 40 {
			resolved = next
			continue
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return resolved
}

func isRelativeTo(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

// pyScalar is validate.py's scalar(): a nonempty string, JSON-quoted, single-quoted or bare.
func pyScalar(text string) (string, error) {
	text = pyStrip(text)
	var value any = text
	if strings.HasPrefix(text, `"`) {
		decoded, err := pyJSONLoads(text)
		if err != nil {
			return "", err
		}
		value = decoded
	} else if strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'") && len(text) >= 1 {
		inner := ""
		if len(text) >= 2 {
			inner = text[1 : len(text)-1]
		}
		value = strings.ReplaceAll(inner, "''", "'")
	}
	str, ok := value.(string)
	if !ok || pyIsBlank(str) {
		return "", valueError{"Expected a nonempty string scalar"}
	}
	return str, nil
}

// SkillMetadata is validate.py's metadata(): SKILL.md frontmatter and agents/openai.yaml.
func SkillMetadata(path string) error {
	text, err := readText(path)
	if err != nil {
		return err
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) != 3 || !pyIsBlank(parts[0]) {
		return valueError{"Missing frontmatter"}
	}
	fields := map[string]string{}
	for _, line := range pySplitlines(pyStrip(parts[1])) {
		key, value, found := strings.Cut(line, ":")
		if _, seen := fields[key]; !found || (key != "name" && key != "description") || seen {
			return valueError{"Expected one name and one description field"}
		}
		if fields[key], err = pyScalar(value); err != nil {
			return err
		}
	}
	dir := filepath.Dir(path)
	name, hasName := fields["name"]
	if !hasName || name != filepath.Base(dir) || fields["description"] == "" {
		return valueError{"Skill name must match its directory; description is required"}
	}
	if !skillName.MatchString(name) {
		return valueError{"Invalid skill name"}
	}
	ui, err := readText(filepath.Join(dir, "agents/openai.yaml"))
	if err != nil {
		return err
	}
	interfaceFields := map[string]string{}
	section := ""
	for _, line := range pySplitlines(ui) {
		if pyIsBlank(line) || strings.HasPrefix(pyLStrip(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			section = pyStrip(line)
		} else if section == "interface:" {
			key, value, found := strings.Cut(pyStrip(line), ":")
			if _, seen := interfaceFields[key]; !found || seen {
				return valueError{"Malformed interface metadata"}
			}
			if interfaceFields[key], err = pyScalar(value); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"display_name", "short_description", "default_prompt"} {
		if _, ok := interfaceFields[key]; !ok {
			return valueError{"Missing interface metadata"}
		}
	}
	if !strings.Contains(interfaceFields["default_prompt"], "$"+name) {
		return valueError{"Default prompt must name this skill"}
	}
	return nil
}

// pyUnquote is urllib.parse.unquote: %XX sequences decoded as UTF-8 with replacement.
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var raw []byte
	var out strings.Builder
	flush := func() {
		out.WriteString(strings.ToValidUTF8(string(raw), "\ufffd"))
		raw = raw[:0]
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s)+0 && i+2 <= len(s)-1 {
			if b, err := url.PathUnescape(s[i : i+3]); err == nil {
				raw = append(raw, b[0])
				i += 2
				continue
			}
		}
		flush()
		out.WriteByte(s[i])
	}
	flush()
	return out.String()
}

// LinkErrors is validate.py's link_errors(): unfenced local Markdown links must resolve to an
// existing path inside root. name is the path relative to root as reported.
func LinkErrors(root, name string) ([]string, error) {
	path := filepath.Join(root, name)
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	var errs []string
	fence := ""
	resolvedRoot := realpath(root)
	for number, line := range pySplitlines(text) {
		if marker := fenceMarker.FindStringSubmatch(pyLStrip(line)); marker != nil {
			delimiter := marker[1]
			if fence == "" {
				fence = delimiter
			} else if delimiter[0] == fence[0] && len(delimiter) >= len(fence) {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		for _, match := range markdownLink.FindAllStringSubmatch(line, -1) {
			target := pyStripChars(match[1], "<>")
			scheme, netloc, urlPath, err := pyURLSplit(target)
			if err != nil {
				return nil, err
			}
			if scheme != "" || netloc != "" || urlPath == "" {
				continue
			}
			unquoted := pyUnquote(urlPath)
			destination := unquoted
			if !strings.HasPrefix(unquoted, "/") {
				destination = filepath.Dir(path) + "/" + unquoted
			}
			destination = realpath(destination)
			if _, statErr := os.Stat(destination); !isRelativeTo(destination, resolvedRoot) || statErr != nil {
				errs = append(errs, fmt.Sprintf("%s:%d: invalid local link %s", name, number+1, target))
			}
		}
	}
	return errs, nil
}

// skillsRoot is validate.py's skills_root(): the manifest's declared skills directory.
func skillsRoot(manifestPath string) (string, error) {
	text, err := readText(manifestPath)
	if err != nil {
		return "", err
	}
	manifest, err := pyJSONLoads(text)
	if err != nil {
		return "", err
	}
	object, _ := manifest.(map[string]any)
	declared, ok := object["skills"].(string)
	if !ok || !strings.HasPrefix(declared, "./") {
		return "", valueError{"skills must be declared as a ./ relative path"}
	}
	relative := pyPurePath(strings.Trim(declared[2:], "/"))
	if len(relative) == 0 || slices.Contains(relative, "..") {
		return "", valueError{"the declared skills path must stay inside the plugin"}
	}
	pluginRoot := realpath(filepath.Dir(filepath.Dir(manifestPath)))
	root := realpath(pluginRoot + "/" + strings.Join(relative, "/"))
	if !isRelativeTo(root, pluginRoot) {
		return "", valueError{"the declared skills path resolves outside " + pluginRoot}
	}
	return root, nil
}

// pyPurePath is PurePosixPath(text).parts without the root, "." and empty parts.
func pyPurePath(text string) []string {
	var parts []string
	for _, part := range strings.Split(text, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// pythonSyntaxErrors compiles the named Python sources with the interpreter on PATH, the only
// parser of the language; it returns "<name>: <error>" per failing file. It is needed only
// while the repository still carries Python sources.
func pythonSyntaxErrors(root string, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	const program = `import ast, json, sys
out = {}
for name in sys.stdin.read().split("\0"):
    try:
        with open(name, encoding="utf-8") as handle:
            ast.parse(handle.read(), filename=name)
    except (OSError, SyntaxError, ValueError) as exc:
        out[name] = f"{name}: {exc}"
json.dump(out, sys.stdout)
`
	cmd := exec.Command("python3", "-I", "-c", program)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(names, "\x00"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("python3 could not check Python syntax: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	var result map[string]string
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("python3 syntax check: %v", err)
	}
	return result, nil
}

// repositoryRoot is the resolved top level of the checkout holding the working directory.
func repositoryRoot() (string, error) {
	out, err := runGit(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return realpath(strings.TrimSpace(string(out))), nil
}

// Validate is `crw-dev ci validate`: skill metadata, local link paths and Python syntax.
func Validate(args []string, stdout, stderr io.Writer) int {
	if _, code := parseFlags("validate", "Validate this repository's supported metadata format, link paths and syntax.",
		nil, args, stdout, stderr); code >= 0 {
		return code
	}
	root, err := repositoryRoot()
	if err != nil {
		return failf(stderr, "validate: %s", err)
	}
	out, err := runGit(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return failf(stderr, "validate: %s", err)
	}
	listing, err := decodeUTF8(out)
	if err != nil {
		return failf(stderr, "validate: %s", err)
	}
	manifest := filepath.Join(root, "plugins/crw/.codex-plugin/plugin.json")
	skills, err := skillsRoot(manifest)
	if err != nil {
		return failf(stderr, "%s: %s", manifest, err)
	}
	names := sortedSet(strings.Split(listing, "\x00"))
	var sources []string
	for _, name := range names {
		if filepath.Ext(name) == ".py" {
			sources = append(sources, name)
		}
	}
	syntax, err := pythonSyntaxErrors(root, sources)
	if err != nil {
		return failf(stderr, "validate: %s", err)
	}
	var errs []string
	count := 0
	for _, name := range names {
		if message, bad := syntax[name]; bad {
			errs = append(errs, message)
			continue
		}
		path := filepath.Join(root, name)
		if filepath.Ext(name) == ".md" {
			found, err := LinkErrors(root, name)
			if err != nil {
				errs = append(errs, name+": "+err.Error())
				continue
			}
			errs = append(errs, found...)
		}
		if filepath.Base(name) == "SKILL.md" && realpath(filepath.Dir(filepath.Dir(path))) == skills {
			if err := SkillMetadata(path); err != nil {
				errs = append(errs, name+": "+err.Error())
				continue
			}
			count++
		}
	}
	if count == 0 {
		errs = append(errs, "No skills validated")
	}
	if len(errs) > 0 {
		return failf(stderr, "%s", strings.Join(errs, "\n"))
	}
	fmt.Fprintf(stdout, "Validated %d skills, local link paths and Python syntax.\n", count)
	return 0
}
