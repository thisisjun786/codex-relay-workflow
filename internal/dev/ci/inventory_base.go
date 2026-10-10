//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/skillport"
)

// The inventory map: docs/skills/inventory-map.json gives each of the 83 overlap items of the skill
// inventory one row (disposition, owner, contributors, check tests, status and the evidence a
// done row needs), with the frozen `base` commit and the descriptions table. This file reads it,
// evaluates a row's asserts on a working tree or on a Git commit, and holds the base rule that
// `crw-dev ci validate` runs; the format checks are the tests in inventory_map_test.go.

const (
	inventoryMapPath = "docs/skills/inventory-map.json"
	inventoryIDsPath = "docs/skills/inventory-ids.json"
)

var (
	fullSHA     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	countWanted = regexp.MustCompile(`^count (0|[1-9][0-9]*)$`)
)

// inventoryAssert is one piece of evidence of a row. Want is present, absent or `count N` for the
// regular expression Pattern (matched with a (?m) prefix on the whole content of the file at Path, or
// of every regular file below it when Path is a directory; N is the total over the files), or
// exists / missing for the file at Path alone, with no Pattern. A guard assert protects text that
// must stay: it is not evidence.
type inventoryAssert struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern,omitempty"`
	Want    string `json:"want"`
	Guard   bool   `json:"guard,omitempty"`
}

// inventoryRow is one row of the map.
type inventoryRow struct {
	Disposition  string            `json:"disposition"`
	Owner        string            `json:"owner"`
	Contributors []string          `json:"contributors"`
	Check        []string          `json:"check"`
	Status       string            `json:"status"`
	Reason       string            `json:"reason,omitempty"`
	Triggers     map[string]string `json:"triggers,omitempty"`
	Paths        []string          `json:"paths,omitempty"`
	Assert       []inventoryAssert `json:"assert,omitempty"`
	SameAs       []string          `json:"sameAs,omitempty"`
}

// inventoryDescription is the descriptions table entry of one skill: the issue that rewrites its
// description and whether that is done.
type inventoryDescription struct {
	Owner  string `json:"owner"`
	Status string `json:"status"`
}

// inventoryMap is docs/skills/inventory-map.json.
type inventoryMap struct {
	Base         string                          `json:"base"`
	Rows         map[string]inventoryRow         `json:"rows"`
	Descriptions map[string]inventoryDescription `json:"descriptions"`
}

// loadInventoryMap decodes the map. An unknown field and a key that occurs twice in an object are
// errors, so a typo or a pasted row cannot hide.
func loadInventoryMap(data []byte) (inventoryMap, error) {
	var m inventoryMap
	if err := duplicateKeyError(data); err != nil {
		return m, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return m, errors.New("unexpected data after the JSON value")
	}
	return m, nil
}

// duplicateKeyError is an error naming the first object key that occurs twice in one object of data.
func duplicateKeyError(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var keys []map[string]bool // one set per open object, nil for an array
	var expectKey []bool
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				keys, expectKey = append(keys, map[string]bool{}), append(expectKey, true)
			case '[':
				keys, expectKey = append(keys, nil), append(expectKey, false)
			default:
				keys, expectKey = keys[:len(keys)-1], expectKey[:len(expectKey)-1]
				if len(keys) > 0 && keys[len(keys)-1] != nil {
					expectKey[len(keys)-1] = true
				}
				continue
			}
			if len(keys) > 1 && keys[len(keys)-2] != nil {
				expectKey[len(keys)-2] = false
			}
			continue
		case string:
			if n := len(keys); n > 0 && keys[n-1] != nil && expectKey[n-1] {
				if keys[n-1][value] {
					return fmt.Errorf("duplicate key %q", value)
				}
				keys[n-1][value], expectKey[n-1] = true, false
				continue
			}
		}
		if n := len(keys); n > 0 && keys[n-1] != nil {
			expectKey[n-1] = true
		}
	}
}

// plainPath is the error of a path that is not a plain repository-relative path: one written as its
// clean form (no "..", ".", empty or leading "./" element), with a trailing "/" allowed for a
// directory. A path the inventory map names is compared by prefix, so it names the file it reads.
func plainPath(path string) error {
	trimmed := strings.TrimSuffix(path, "/")
	clean := filepath.ToSlash(filepath.Clean(trimmed))
	if trimmed == "" || filepath.IsAbs(path) || clean != trimmed || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("must be a plain repository-relative path")
	}
	return nil
}

// validate is the shape of the assert: a repository-relative path, and a want that fits the pattern.
func (a inventoryAssert) validate() error {
	if err := plainPath(a.Path); err != nil {
		return fmt.Errorf("path %q %v", a.Path, err)
	}
	switch {
	case a.Want == "exists" || a.Want == "missing":
		if a.Pattern != "" {
			return fmt.Errorf("%s takes no pattern", a.Want)
		}
	case a.Want == "present" || a.Want == "absent" || countWanted.MatchString(a.Want):
		if a.Pattern == "" {
			return fmt.Errorf("%s needs a pattern", a.Want)
		}
		if _, err := regexp.Compile("(?m)" + a.Pattern); err != nil {
			return fmt.Errorf("pattern: %v", err)
		}
	default:
		return fmt.Errorf("want %q is not present, absent, count N, exists or missing", a.Want)
	}
	return nil
}

// describe is the assert as an error names it.
func (a inventoryAssert) describe() string {
	if a.Want == "exists" || a.Want == "missing" {
		return a.Path + " " + a.Want
	}
	return a.Path + ": /" + a.Pattern + "/ " + a.Want
}

// treeFiles is a tree an assert is evaluated on: the regular files at a path.
type treeFiles interface {
	// list is the regular files at name, as sorted repository-relative paths: name itself when it is
	// a regular file, every regular file below it when it is a directory, none when it is missing.
	list(name string) ([]string, error)
	// read is the content of a file list returned.
	read(name string) (string, error)
}

// holds evaluates the assert on the tree.
func (a inventoryAssert) holds(tree treeFiles) (bool, error) {
	if err := a.validate(); err != nil {
		return false, err
	}
	files, err := tree.list(a.Path)
	if err != nil {
		return false, err
	}
	switch a.Want {
	case "exists":
		return len(files) > 0, nil
	case "missing":
		return len(files) == 0, nil
	}
	re := regexp.MustCompile("(?m)" + a.Pattern)
	matches := 0
	for _, file := range files {
		text, err := tree.read(file)
		if err != nil {
			return false, err
		}
		matches += len(re.FindAllStringIndex(text, -1))
	}
	switch {
	case a.Want == "present":
		return matches >= 1, nil
	case a.Want == "absent":
		return matches == 0, nil
	}
	want, _ := strconv.Atoi(strings.TrimPrefix(a.Want, "count "))
	return matches == want, nil
}

// dirTree is the working tree at root.
type dirTree struct{ root string }

func (d dirTree) list(name string) ([]string, error) {
	name = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(name)), "/")
	info, err := os.Lstat(filepath.Join(d.root, filepath.FromSlash(name)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		return []string{name}, nil
	}
	if !info.IsDir() {
		return nil, nil
	}
	var files []string
	err = filepath.WalkDir(filepath.Join(d.root, filepath.FromSlash(name)), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			relative, err := filepath.Rel(d.root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	return files, err
}

func (d dirTree) read(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(name)))
	return string(data), err
}

// gitTree is the tree of a commit of the repository at root, read with git.
type gitTree struct {
	root, commit string
}

func (g gitTree) list(name string) ([]string, error) {
	name = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(name)), "/")
	out, err := runGit(g.root, "ls-tree", "-r", "-z", g.commit, "--", name)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, path, found := strings.Cut(entry, "\t")
		if found && strings.HasPrefix(meta, "100") { // a regular file, not a link or a submodule
			files = append(files, path)
		}
	}
	return sortedSet(files), nil
}

func (g gitTree) read(name string) (string, error) {
	out, err := runGit(g.root, "show", g.commit+":"+name)
	return string(out), err
}

// InventoryBaseErrors is the base rule of the inventory map: every non-guard assert of every fix,
// split or delete row, open or done, is evaluated on the tree at the map's `base` commit (read with
// git), and an assert that already holds there is an error, so a row cannot be flipped by text that
// was true before its issue. A base commit the repository does not hold is an error, never a skip:
// the required validate job checks out the full history (fetch-depth 0), the test legs do not,
// which is why the rule is here and not in a test. A checkout without the map has nothing to check.
func InventoryBaseErrors(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, inventoryMapPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	label := inventoryMapPath + ": "
	if err != nil {
		return []string{label + err.Error()}
	}
	m, err := loadInventoryMap(data)
	if err != nil {
		return []string{label + err.Error()}
	}
	if !fullSHA.MatchString(m.Base) {
		return []string{label + "base must be a full 40-hex commit sha, not " + strconv.Quote(m.Base)}
	}
	if _, err := runGit(root, "cat-file", "-e", m.Base+"^{commit}"); err != nil {
		return []string{label + "base commit " + m.Base + " is not in the repository (the validate job needs the full history: fetch-depth 0)"}
	}
	tree := gitTree{root: root, commit: m.Base}
	var errs []string
	for _, key := range sortedKeys(m.Rows) {
		row := m.Rows[key]
		if row.Disposition != "fix" && row.Disposition != "split" && row.Disposition != "delete" {
			continue
		}
		for i, assert := range row.Assert {
			if assert.Guard {
				continue
			}
			holds, err := assert.holds(tree)
			switch {
			case err != nil:
				errs = append(errs, fmt.Sprintf("%s%s: assert %d (%s): %v", label, key, i+1, assert.describe(), err))
			case holds:
				errs = append(errs, fmt.Sprintf("%s%s: assert %d (%s) already holds at base %s", label, key, i+1, assert.describe(), m.Base[:12]))
			}
		}
	}
	return errs
}

// skillDescriptions is the frontmatter description of every skill below root's plugins/crw/skills,
// by directory name.
func skillDescriptions(root string) (map[string]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(skillport.SkillsRoot)))
	if err != nil {
		return nil, err
	}
	descriptions := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		text, err := readText(filepath.Join(root, filepath.FromSlash(skillport.SkillsRoot), entry.Name(), "SKILL.md"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fields, err := frontmatterFields(text)
		if err != nil {
			return nil, fmt.Errorf("%s/%s/SKILL.md: %w", skillport.SkillsRoot, entry.Name(), err)
		}
		descriptions[entry.Name()] = fields["description"]
	}
	return descriptions, nil
}
