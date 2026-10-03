//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/skillport"
)

var (
	markdownLink = regexp.MustCompile(`\[[^\]\n]*\]\((<[^>\n]+>|[^\s)]+)(?:\s+"[^"]*")?\)`)
	fenceMarker  = regexp.MustCompile("^(`{3,}|~{3,})")
	skillName    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	urlScheme    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// scalar is a nonempty string scalar: JSON-quoted, single-quoted or bare.
func scalar(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, `"`) {
		if err := json.Unmarshal([]byte(value), &value); err != nil {
			return "", err
		}
	} else if len(value) >= 2 && strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("expected a nonempty string scalar")
	}
	return value, nil
}

// SkillMetadata checks a skill's SKILL.md frontmatter and its agents/openai.yaml.
func SkillMetadata(path string) error {
	text, err := readText(path)
	if err != nil {
		return err
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[0]) != "" {
		return errors.New("missing frontmatter")
	}
	fields := map[string]string{}
	for _, line := range lines(strings.TrimSpace(parts[1])) {
		key, value, found := strings.Cut(line, ":")
		if _, seen := fields[key]; !found || (key != "name" && key != "description") || seen {
			return errors.New("expected one name and one description field")
		}
		if fields[key], err = scalar(value); err != nil {
			return err
		}
	}
	dir := filepath.Dir(path)
	name, hasName := fields["name"]
	if !hasName || name != filepath.Base(dir) || fields["description"] == "" {
		return errors.New("skill name must match its directory; description is required")
	}
	if !skillName.MatchString(name) {
		return errors.New("invalid skill name")
	}
	ui, err := readText(filepath.Join(dir, "agents/openai.yaml"))
	if err != nil {
		return err
	}
	interfaceFields := map[string]string{}
	section := ""
	for _, line := range lines(ui) {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			section = strings.TrimSpace(line)
		} else if section == "interface:" {
			key, value, found := strings.Cut(strings.TrimSpace(line), ":")
			if _, seen := interfaceFields[key]; !found || seen {
				return errors.New("malformed interface metadata")
			}
			if interfaceFields[key], err = scalar(value); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"display_name", "short_description", "default_prompt"} {
		if _, ok := interfaceFields[key]; !ok {
			return errors.New("missing interface metadata")
		}
	}
	if !strings.Contains(interfaceFields["default_prompt"], "$"+name) {
		return errors.New("default prompt must name this skill")
	}
	return nil
}

// localPath is the path a Markdown link target names in the repository, or "" when the target
// is a URL (it has a scheme or a host) or only a fragment or query.
func localPath(target string) string {
	if urlScheme.MatchString(target) || strings.HasPrefix(target, "//") {
		return ""
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if unescaped, err := url.PathUnescape(target); err == nil {
		return unescaped
	}
	return target
}

// LinkErrors is every unfenced local Markdown link in the file root/name that does not resolve
// to an existing path inside root. name is the path relative to root as reported.
func LinkErrors(root, name string) ([]string, error) {
	path := filepath.Join(root, name)
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	var errs []string
	fence := ""
	resolvedRoot := resolve(root)
	for number, line := range lines(text) {
		if marker := fenceMarker.FindStringSubmatch(strings.TrimLeft(line, " \t")); marker != nil {
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
			target := strings.Trim(match[1], "<>")
			local := localPath(target)
			if local == "" {
				continue
			}
			destination := local
			if !strings.HasPrefix(local, "/") {
				destination = filepath.Join(filepath.Dir(path), local)
			}
			destination, err := filepath.EvalSymlinks(destination)
			if err != nil || !isRelativeTo(destination, resolvedRoot) {
				errs = append(errs, fmt.Sprintf("%s:%d: invalid local link %s", name, number+1, target))
			}
		}
	}
	return errs, nil
}

// skillsRoot is the skills directory the manifest at manifestPath declares.
func skillsRoot(manifestPath string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	manifest, err := decodeJSON(data)
	if err != nil {
		return "", err
	}
	m, _ := object(manifest)
	declared, ok := m["skills"].(string)
	if !ok || !strings.HasPrefix(declared, "./") {
		return "", errors.New("skills must be declared as a ./ relative path")
	}
	relative := pathParts(strings.Trim(declared[2:], "/"))
	if len(relative) == 0 || slices.Contains(relative, "..") {
		return "", errors.New("the declared skills path must stay inside the plugin")
	}
	pluginRoot := resolve(filepath.Dir(filepath.Dir(manifestPath)))
	root := resolve(filepath.Join(pluginRoot, filepath.Join(relative...)))
	if !isRelativeTo(root, pluginRoot) {
		return "", errors.New("the declared skills path resolves outside " + pluginRoot)
	}
	return root, nil
}

// pythonFileErrors names the files that make the repository carry Python: a name ending in .py,
// and a regular file whose first line is a python shebang. Nothing is exempt. The Python
// implementation left in todo 44 and the last developer tools in todo 48, and nothing in the
// product or in CI runs Python; a sample that has to stay can be kept under another name.
func pythonFileErrors(root string, names []string) []string {
	var errs []string
	for _, name := range names {
		switch {
		case filepath.Ext(name) == ".py":
			errs = append(errs, name+": a Python file; the repository tracks no Python")
		case pythonShebang(filepath.Join(root, name)):
			errs = append(errs, name+": a script with a python shebang; the repository tracks no Python")
		}
	}
	return errs
}

// pythonShebang reports whether path is a regular file whose first line starts with #! and names
// python. A symbolic link is judged as what it is, never followed; only the first 512 bytes are
// read, so a binary without a newline is not read whole.
func pythonShebang(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	line, _, _ := bytes.Cut(head[:n], []byte("\n"))
	return bytes.HasPrefix(line, []byte("#!")) && bytes.Contains(line, []byte("python"))
}

// repositoryRoot is the resolved top level of the checkout holding the working directory.
func repositoryRoot() (string, error) {
	out, err := runGit(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return resolve(strings.TrimSpace(string(out))), nil
}

// Validate is `crw-dev ci validate`: skill metadata, local link paths and the absence of Python.
func Validate(args []string, stdout, stderr io.Writer) int {
	if code := parseFlags(newFlags("validate"), "Validate this repository's supported metadata format and link paths, and that it holds no Python.",
		args, stdout, stderr); code >= 0 {
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
	manifest := filepath.Join(root, "plugins/crw/.codex-plugin/plugin.json")
	skills, err := skillsRoot(manifest)
	if err != nil {
		return failf(stderr, "%s: %s", manifest, err)
	}
	staging := resolve(filepath.Join(root, skillport.StagingRoot))
	names := sortedSet(strings.Split(string(out), "\x00"))
	errs := pythonFileErrors(root, names)
	count := 0
	for _, name := range names {
		path := filepath.Join(root, name)
		if filepath.Ext(name) == ".md" {
			found, err := LinkErrors(root, name)
			if err != nil {
				errs = append(errs, name+": "+err.Error())
				continue
			}
			errs = append(errs, found...)
		}
		if parent := resolve(filepath.Dir(filepath.Dir(path))); filepath.Base(name) == "SKILL.md" && (parent == skills || parent == staging) {
			if err := SkillMetadata(path); err != nil {
				errs = append(errs, name+": "+err.Error())
				continue
			}
			count++
		}
	}
	_, fidelity := skillport.Check(root, nil)
	errs = append(errs, fidelity...)
	if count == 0 {
		errs = append(errs, "No skills validated")
	}
	if len(errs) > 0 {
		return failf(stderr, "%s", strings.Join(errs, "\n"))
	}
	fmt.Fprintf(stdout, "Validated %d skills, local link paths and no Python files.\n", count)
	return 0
}
