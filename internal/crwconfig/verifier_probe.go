package crwconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// verifierProbeTimeout bounds each probe of a verifier tool.
const verifierProbeTimeout = 10 * time.Second

var goVersionWord = regexp.MustCompile(`\bgo version (go[0-9][^ \n]*)`)

// GoRootCandidate is a Go toolchain root found on the host, with the version its bin/go reports.
type GoRootCandidate struct {
	Root    string `json:"root"`
	Version string `json:"version"`
}

// VerifierProbe is what a run with an environment would verify with (CRW-1319): the verifier section it reads, the go it runs,
// where that go came from ("config" or "path") and its version, how crw-dev runs and whether it starts. Problems are what keeps
// the verifier from starting; Notes are what works only through PATH because nothing configures it (fail-open). Candidates are
// the Go toolchain roots found on the host when no root is configured, newest version first.
type VerifierProbe struct {
	Tools      VerifierTools
	Go         string
	GoSource   string
	GoVersion  string
	Mode       string
	Command    []string
	Runnable   bool
	Candidates []GoRootCandidate
	Problems   []string
	Notes      []string
}

// ProbeVerifierTools reads the verifier section the way a run with env reads it (flagPath as --config) and probes the tools: the
// go's version (`go version`, GOTOOLCHAIN=local, so no other toolchain is selected or fetched) and, for a crw-dev binary,
// `crw-dev help`. Each probe runs bounded in time, in a home of its own that is removed afterwards, so it writes nothing of the
// host's. In tree mode crw-dev is runnable when its go runs: the tree is the one verified. The probe's home is made below the
// temporary directory of the one place (tmploc); a refusal there (a relative TMPDIR, an unusable directory) is the probe's error
// and so a problem, never a home made somewhere else.
func ProbeVerifierTools(ctx context.Context, env []string, flagPath string) VerifierProbe {
	getenv := envGetter(env)
	p := VerifierProbe{}
	tools, err := LoadVerifierTools(getenv, flagPath)
	if err != nil {
		p.Problems = append(p.Problems, "the verifier section could not be read: "+err.Error())
		p.Tools = tools
		return p
	}
	if tools.VerifierKind() == VerifierGitHubActions {
		return probeGitHubActions(ctx, env, tools)
	}
	if tools.Ignored != "" {
		p.Problems = append(p.Problems, "the verifier section is ignored ("+tools.Ignored+"): the verifier takes crw-dev and go from PATH (fail-open)")
	}
	p.Tools = tools
	pathEnv := getenv("PATH")
	if goBin := tools.GoBinary(pathEnv); goBin != "" {
		p.Go, p.GoSource = goBin, "path"
		if tools.GoRoot != "" {
			p.GoSource = "config"
		}
		if out, err := verifierProbe(ctx, env, []string{"GOTOOLCHAIN=local"}, goBin, "version"); err != nil {
			p.Problems = append(p.Problems, "go "+goBin+" does not run: "+err.Error())
		} else if m := goVersionWord.FindStringSubmatch(string(out)); m != nil {
			p.GoVersion = m[1]
		} else {
			p.Problems = append(p.Problems, "go "+goBin+" printed no go version")
		}
	} else {
		p.Problems = append(p.Problems, "no go: verifier.go_root is not set and go is not on PATH; the verifier cannot build or run crw-dev ci local")
	}
	if tools.GoRoot == "" && err == nil && tools.Ignored == "" {
		if tools.CrwDev == "" {
			p.Notes = append(p.Notes, "verifier.go_root and verifier.crw_dev are not set: the verifier takes crw-dev and go from PATH (fail-open)")
		} else if p.Go != "" {
			p.Notes = append(p.Notes, "verifier.go_root is not set: the verifier takes go from PATH (fail-open)")
		}
	}
	p.Mode = tools.Mode()
	switch p.Mode {
	case "tree":
		bin := "go"
		if p.Go != "" {
			bin = p.Go
		}
		p.Command = []string{bin, "run", "-tags", "dev", "./cmd/crw-dev"}
		p.Runnable = p.GoVersion != ""
	default:
		bin := tools.CrwDev
		if p.Mode == "path" {
			bin = lookExecutable("crw-dev", pathEnv)
		}
		if bin == "" {
			p.Problems = append(p.Problems, "crw-dev is not on PATH")
			break
		}
		p.Command = []string{bin}
		var extra []string
		if c, cerr := tools.Command("", pathEnv); cerr == nil {
			extra = c.Env
		}
		if _, perr := verifierProbe(ctx, env, extra, bin, "help"); perr == nil {
			p.Runnable = true
		} else {
			p.Problems = append(p.Problems, "crw-dev "+bin+" does not run: "+perr.Error())
		}
	}
	if tools.GoRoot == "" && err == nil {
		var herr error
		p.Candidates, herr = goRootCandidates(ctx, env, p.Go)
		// the hint goes with the problems when the verifier cannot start, else with the notes of what works only through PATH
		if herr != nil {
			// no root could be probed, so "none was found" would be wrong: the reason is the probe's home
			p.Problems = append(p.Problems, "the Go toolchain roots on this host could not be probed: "+herr.Error())
		} else if len(p.Candidates) > 0 {
			c := p.Candidates[0]
			hint := fmt.Sprintf("a Go toolchain root is at %s (%s): record it with crw config verifier --go-root %s", c.Root, c.Version, c.Root)
			if len(p.Problems) > 0 {
				p.Problems = append(p.Problems, hint)
			} else {
				p.Notes = append(p.Notes, hint)
			}
		} else if len(p.Problems) > 0 {
			p.Problems = append(p.Problems, "no Go toolchain root was found on this host: install one and record it with crw config verifier --go-root <dir>")
		}
	}
	return p
}

// probeGitHubActions is the probe of a remote verifier (CRW-1594): no Go and no crw-dev is needed — git and
// gitleaks on PATH, gh logged in to github.com, write permission on the repository and an active configured
// workflow. The gh probes are read-only and keep the caller's HOME and GH_CONFIG_DIR, so gh reads its own
// login; they never write auth configuration.
func probeGitHubActions(ctx context.Context, env []string, tools VerifierTools) VerifierProbe {
	p := VerifierProbe{Tools: tools, Mode: VerifierGitHubActions}
	workflow, repository := tools.GitHubWorkflow(), tools.Repository
	p.Command = []string{"gh", "workflow", "run", workflow, "--repo", repository}
	pathEnv := envGetter(env)("PATH")
	for _, tool := range []string{"git", "gitleaks"} {
		if lookExecutable(tool, pathEnv) == "" {
			p.Problems = append(p.Problems, tool+" is not on PATH: the remote verifier needs it")
		}
	}
	gh := lookExecutable("gh", pathEnv)
	if gh == "" {
		p.Problems = append(p.Problems, "gh is not on PATH: the remote verifier needs it")
	} else {
		if _, err := verifierProbeAuth(ctx, env, gh, "auth", "status", "--hostname", "github.com"); err != nil {
			p.Problems = append(p.Problems, "gh is not logged in to github.com (gh auth status): "+err.Error())
		}
		if out, err := verifierProbeAuth(ctx, env, gh, "repo", "view", repository, "--json", "viewerPermission"); err != nil {
			p.Problems = append(p.Problems, "the repository "+repository+" could not be read: "+err.Error())
		} else {
			var view struct {
				Permission string `json:"viewerPermission"`
			}
			if err := json.Unmarshal(out, &view); err != nil {
				p.Problems = append(p.Problems, "the repository "+repository+" answered no viewerPermission")
			} else {
				switch view.Permission {
				case "WRITE", "MAINTAIN", "ADMIN":
				default:
					p.Problems = append(p.Problems, "gh has "+view.Permission+" permission to "+repository+": the remote verifier needs write permission to push the verification branch")
				}
			}
		}
		if out, err := verifierProbeAuth(ctx, env, gh, "api", "--paginate", "repos/"+repository+"/actions/workflows?per_page=100"); err != nil {
			p.Problems = append(p.Problems, "the workflows of "+repository+" could not be read: "+err.Error())
		} else if _, err := DecodeGitHubWorkflow(out, workflow); err != nil {
			p.Problems = append(p.Problems, err.Error())
		}
	}
	p.Runnable = len(p.Problems) == 0
	return p
}

// verifierProbeAuth runs one read-only probe with the caller's environment as it is — HOME and GH_CONFIG_DIR
// included, so gh reads the login it would use — bounded in time. Nothing of the host's is written.
func verifierProbeAuth(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, verifierProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(append([]string{}, env...), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}

// goRootCandidates are the Go toolchain roots found on the host: the root of the go on PATH (`go env GOROOT`), then the usual
// install locations ($HOME/.local/go/go, $HOME/.local/go, $HOME/sdk/go*, $HOME/go/go, /usr/local/go, /usr/lib/go, /opt/go),
// each a directory whose bin/go is an executable regular file that runs and reports a version. A root is judged by what it holds,
// never by where it is: one below /tmp or another temporary directory is offered like any other (CRW-1579). A root reached twice
// (the same directory once its links are resolved) is listed once. They are ordered by version, newest first, the earlier location
// first on a tie. When a probe's home cannot be made (tmploc refuses TMPDIR), no root can be probed: the search stops and answers
// that refusal, so the caller reports it instead of "no root was found".
func goRootCandidates(ctx context.Context, env []string, pathGo string) ([]GoRootCandidate, error) {
	getenv := envGetter(env)
	var roots []string
	if pathGo != "" {
		out, err := verifierProbe(ctx, env, []string{"GOTOOLCHAIN=local"}, pathGo, "env", "GOROOT")
		if herr := (*probeHomeError)(nil); errors.As(err, &herr) {
			return nil, herr
		}
		if err == nil {
			if root := strings.TrimSpace(string(out)); filepath.IsAbs(root) {
				roots = append(roots, root)
			}
		}
	}
	if home := getenv("HOME"); filepath.IsAbs(home) {
		roots = append(roots, filepath.Join(home, ".local", "go", "go"), filepath.Join(home, ".local", "go"))
		sdk, _ := filepath.Glob(filepath.Join(home, "sdk", "go*"))
		roots = append(roots, sdk...)
		roots = append(roots, filepath.Join(home, "go", "go"))
	}
	roots = append(roots, "/usr/local/go", "/usr/lib/go", "/opt/go")
	seen := map[string]bool{}
	var out []GoRootCandidate
	for _, root := range roots {
		root = filepath.Clean(root)
		real, err := filepath.EvalSymlinks(root)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		bin := filepath.Join(root, "bin", "go")
		if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		answer, err := verifierProbe(ctx, env, []string{"GOTOOLCHAIN=local", "GOROOT=" + root}, bin, "version")
		if herr := (*probeHomeError)(nil); errors.As(err, &herr) {
			return nil, herr
		}
		m := goVersionWord.FindStringSubmatch(string(answer))
		if err != nil || m == nil {
			continue
		}
		out = append(out, GoRootCandidate{Root: root, Version: m[1]})
	}
	slices.SortStableFunc(out, func(a, b GoRootCandidate) int { return version.Compare(b.Version, a.Version) })
	return out, nil
}

// probeHomeError is a probe that did not start because its home could not be made (tmploc's refusal, which it wraps): the tool
// was never asked, so it says nothing about the tool.
type probeHomeError struct{ err error }

func (e *probeHomeError) Error() string {
	return "the probe's home could not be made: " + e.err.Error()
}

func (e *probeHomeError) Unwrap() error { return e.err }

// verifierProbe runs one probe of a tool with env and the extra entries over it, bounded in time, and answers its output. The
// probe has a home of its own below tmploc's temporary directory, removed afterwards: go writes its telemetry counters below the
// user configuration directory whenever it starts. tmploc's refusal is the probe's error.
func verifierProbe(ctx context.Context, env []string, extra []string, name string, args ...string) ([]byte, error) {
	home, err := tmploc.MkdirTemp("crw-verifier-probe-")
	if err != nil {
		return nil, &probeHomeError{err: err}
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, verifierProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(append(append([]string{}, env...), extra...), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"), "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"))
	cmd.Dir = home
	return cmd.CombinedOutput()
}

// lookExecutable is the executable named name in a directory of pathEnv, or "".
func lookExecutable(name, pathEnv string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path
		}
	}
	return ""
}

// envGetter reads an environment list the way the process environment is read: the last entry of a key wins.
func envGetter(env []string) func(string) string {
	return func(key string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
				return v
			}
		}
		return ""
	}
}
