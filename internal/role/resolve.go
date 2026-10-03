package role

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// SpawnResolution is how a role is to be spawned (store.ts SpawnResolution): the model to spawn with, or none to inherit the main model,
// and the effort and prompt override the role holds. The oracle's trustWarning belongs to the project layer and is gone (I1).
type SpawnResolution struct {
	Role           RoleName    `json:"role"`
	Model          *string     `json:"model"`
	UsesMainModel  bool        `json:"usesMainModel"`
	Effort         *EffortName `json:"effort"`
	PromptOverride *string     `json:"promptOverride"`
}

// ResolveSpawnConfig is resolveSpawnConfig over the global store. A role the oracle does not know makes it throw a V8 TypeError (its
// lookup of the role's settings is unguarded); here it is the refusal "unknown role" (I11).
func ResolveSpawnConfig(env host.LookupEnv, role RoleName) (SpawnResolution, error) {
	if !validRole(role) {
		return SpawnResolution{}, fmt.Errorf("unknown role \"%s\"", role)
	}
	s, err := ReadSettings(env)
	if err != nil {
		return SpawnResolution{}, err
	}
	cfg := s.Roles[role]
	res := SpawnResolution{Role: role, UsesMainModel: cfg.Mode == ModeDefault, Effort: cfg.Effort, PromptOverride: cfg.PromptOverride}
	if !res.UsesMainModel {
		res.Model = cfg.Model
	}
	return res, nil
}

// The two helpers below are what the oracle used to ignore a Git-tracked project config until it was reviewed. Nothing reads a project
// config any more (decision 7) and nothing calls them yet; they keep the oracle's git commands and token. git runs with an argument list
// and no shell, in the process environment as the oracle's does: a routing variable such as GIT_DIR is inherited (known-defects).
const gitTimeout = 1500 * time.Millisecond

func projectConfigPath(cwd string) string { return filepath.Join(cwd, crwdir.DirName, StoreFile) }

// git runs git -C cwd with a timeout; a child that keeps the output open after git exits is waited for one second more.
func git(cwd string, args ...string) (stdout []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// IsTrackedProjectConfig is isTrackedProjectConfig: true only when git says <cwd>/.crw/subagents.json is part of the checkout. A git
// that fails, is absent or does not answer within 1.5 s is "not tracked", as in the oracle.
func IsTrackedProjectConfig(cwd string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "ls-files", "--error-unmatch", "--", crwdir.DirName+"/"+StoreFile)
	return cmd.Run() == nil // no pipe is connected, so nothing a child holds can delay the answer
}

// ProjectConfigTrustToken is projectConfigTrustToken: sha256 over the canonical repository root (the directory itself outside a
// repository, or when git does not answer), the canonical config path and the config's bytes, joined by a NUL each, so a token binds
// the repository and the exact bytes. ok is false where the oracle answers null: no config, an unresolvable path, or a git output
// that is not valid UTF-8 (JavaScript would decode it with U+FFFD and fail to resolve the path).
func ProjectConfigTrustToken(cwd string) (token string, ok bool) {
	configPath, err := realpath(projectConfigPath(cwd))
	if err != nil {
		return "", false
	}
	root := cwd
	if out, err := git(cwd, "rev-parse", "--show-toplevel"); err == nil {
		if !utf8.Valid(out) {
			return "", false
		}
		root = text.Trim(string(out))
	}
	if root, err = realpath(root); err != nil {
		return "", false
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", false
	}
	sum := sha256.New()
	for _, part := range [][]byte{[]byte(root), {0}, []byte(configPath), {0}, data} {
		sum.Write(part)
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil)), true
}

// realpath is realpathSync(resolve(p)): absolute, symbolic links followed, and an error for a path that is not there.
func realpath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
