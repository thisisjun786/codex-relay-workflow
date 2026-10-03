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
	"os/exec"
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
	return resolve(strings.TrimSpace(string(out))), nil
}

// Validate is `crw-dev ci validate`: skill metadata, local link paths and Python syntax.
func Validate(args []string, stdout, stderr io.Writer) int {
	if code := parseFlags(newFlags("validate"), "Validate this repository's supported metadata format, link paths and syntax.",
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
	fmt.Fprintf(stdout, "Validated %d skills, local link paths and Python syntax.\n", count)
	return 0
}
