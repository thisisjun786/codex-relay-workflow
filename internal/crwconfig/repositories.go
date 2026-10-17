package crwconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// RepositoryCommands overrides only the commands named for one repository. Missing
// members preserve the configured verifier and CRW's merge implementation.
type RepositoryCommands struct {
	VerifyCommand    string `json:"verify_command,omitempty"`
	IntegrateCommand string `json:"integrate_command,omitempty"`
	Key              string `json:"-"`
}

const RepositoryCommandsFallback = "one or more repository commands are not configured; keeping existing behavior for each unset command"

func (f *File) RepositoryCommands() (map[string]RepositoryCommands, error) {
	result := map[string]RepositoryCommands{}
	raw, exists := f.doc["repositories"]
	if !exists {
		return result, nil
	}
	var entries map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("%s: repositories must be an object keyed by absolute path or OWNER/NAME", f.path)
	}
	for key, fields := range entries {
		if fields == nil || (!filepath.IsAbs(key) && (!githubRepositoryPattern.MatchString(key) || strings.Contains(key, ".."))) {
			return nil, fmt.Errorf("%s: invalid repositories entry %q", f.path, key)
		}
		commands := RepositoryCommands{Key: key}
		for name, dest := range map[string]*string{"verify_command": &commands.VerifyCommand, "integrate_command": &commands.IntegrateCommand} {
			if v, ok := fields[name]; ok {
				if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, dest) != nil || strings.TrimSpace(*dest) == "" || strings.ContainsRune(*dest, 0) {
					return nil, fmt.Errorf("%s: repositories[%q].%s must be a nonempty command string", f.path, key, name)
				}
			}
		}
		result[key] = commands
	}
	return result, nil
}

// LoadRepositoryCommands selects the checkout path, then its common worktree's
// path, then origin's OWNER/NAME. Path keys take precedence over forge keys.
func LoadRepositoryCommands(ctx context.Context, getenv func(string) string, checkout string) (RepositoryCommands, error) {
	f, err := loadOptional(getenv, "")
	if err != nil || f == nil {
		return RepositoryCommands{}, err
	}
	entries, err := f.RepositoryCommands()
	if err != nil || len(entries) == 0 {
		return RepositoryCommands{}, err
	}
	if c, ok := entries[checkout]; ok {
		return c, nil
	}
	physical, err := filepath.EvalSymlinks(checkout)
	if err == nil {
		if c, ok := entries[physical]; ok {
			return c, nil
		}
	}
	git := func(dir string, args ...string) string {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = []string{}
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	commonOf := func(dir string) string {
		common := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if physical, err := filepath.EvalSymlinks(common); err == nil {
			return physical
		}
		return common
	}
	common := commonOf(checkout)
	if filepath.Base(common) == ".git" {
		if c, ok := entries[filepath.Dir(common)]; ok {
			return c, nil
		}
	}
	// A configured path can itself be a linked checkout or a symlink. Its
	// sibling node worktrees must select the same repository command.
	var paths []string
	for key := range entries {
		if filepath.IsAbs(key) {
			paths = append(paths, key)
		}
	}
	slices.Sort(paths)
	var matched *RepositoryCommands
	for _, path := range paths {
		if common == "" || commonOf(path) != common {
			continue
		}
		c := entries[path]
		if matched != nil && (matched.VerifyCommand != c.VerifyCommand || matched.IntegrateCommand != c.IntegrateCommand) {
			return RepositoryCommands{}, fmt.Errorf("repository paths %q and %q configure different commands for the same repository", matched.Key, c.Key)
		}
		matched = &c
	}
	if matched != nil {
		return *matched, nil
	}
	remote := git(checkout, "remote", "get-url", "origin")
	var slug string
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		slug = strings.TrimPrefix(u.Path, "/")
	} else if _, rest, ok := strings.Cut(remote, ":"); ok && strings.Contains(strings.Split(remote, ":")[0], "@") {
		slug = rest
	}
	slug = strings.TrimSuffix(slug, ".git")
	if c, ok := entries[slug]; ok && slug != "" {
		return c, nil
	}
	return RepositoryCommands{}, nil
}
