package integrator

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Config is the integrator's configuration: the integrate section of the manage object of
// the crw configuration file. Every host path and command is named here and none has a
// default; only the repository-relative names and the remote's own names do.
type Config struct {
	// Checkout is the repository whose remote the heads are merged onto. It is read and
	// fetched into, never modified: the merge happens in a worktree below WorkRoot.
	Checkout string `json:"checkout"`
	// Remote and Branch name the integration branch (default origin and dev).
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	// Git is the git program (default "git", found on PATH).
	Git string `json:"git"`

	// QueueDir holds pending/, done/, conflict/, blocked/ and retried/, and the runner log.
	QueueDir string `json:"queue_dir"`
	// LockFile is the integration lock, shared with every other integration run.
	LockFile string `json:"lock_file"`
	// RunsDir receives <tag>.log, .out, .summary.json, .verify.log and the verification record.
	RunsDir string `json:"runs_dir"`
	// WorkRoot receives the private worktree run-<tag> of one integration.
	WorkRoot string `json:"work_root"`

	// KeyPrefix is the issue key's prefix (default CRW): the file named 1038 is issue CRW-1038
	// and node crw-1038.
	KeyPrefix string `json:"key_prefix"`
	// TagPrefix starts the tag of an integration run (default cc-q).
	TagPrefix string `json:"tag_prefix"`

	// GateCommand is the pre-merge gate: argv with {issue} and {head} replaced. It runs in
	// Checkout; its last output line must start with PASS and its status must be 0.
	GateCommand []string `json:"gate_command"`
	// VerifyCommand is the verification of the merged tree: argv run in the worktree with
	// CRW_VERIFY_HEAD, CRW_VERIFY_BASE and CRW_VERIFY_RECORD in its environment.
	VerifyCommand []string `json:"verify_command"`
	// RecordVersionCommand re-derives the plugin manifest's version line in the worktree.
	RecordVersionCommand []string `json:"record_version_command"`
	// RecordVersionEnv is added to RecordVersionCommand's environment; {tmpdir} in a value is
	// the run's private temporary directory below WorkRoot.
	RecordVersionEnv map[string]string `json:"record_version_env"`

	// PluginDir is the plugin package's directory and PluginManifest the file whose version
	// line may conflict, both relative to the repository root.
	PluginDir      string `json:"plugin_dir"`
	PluginManifest string `json:"plugin_manifest"`

	// IntervalSeconds is the pause between passes of watch (default 60).
	IntervalSeconds int `json:"interval_seconds"`

	// PullRequestRepo is the repository ("owner/name") whose pull requests the queue lands (PR-unit landing, CRW-1355): a pending
	// file pr-<N> is pull request N of it, read through the tracking adapter. Empty: the queue lands no pull request.
	PullRequestRepo string `json:"pull_request_repo"`
	// ReviewCommand is the repository's independent review of a pull request's merged tree, run in the worktree after the
	// verification passed and before dev moves: argv with {pr}, {pr_number}, {head}, {merged}, {base}, {verdict}, {worktree} and
	// {record} replaced, and CRW_REVIEW_PR, CRW_REVIEW_HEAD, CRW_REVIEW_MERGED, CRW_REVIEW_BASE, CRW_REVIEW_VERDICT and
	// CRW_VERIFY_RECORD in its environment. It writes its verdict file ({"verdict": "accept"|"needs_changes"}, at the top or
	// under structured_output); needs_changes refuses the merge. Empty: the queue warns, records "review not configured" and
	// lands on the verification-record alone.
	ReviewCommand []string `json:"review_command"`
}

var (
	keyPrefixRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	tagPrefixRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	refNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	repoRE      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Resolved is c with the defaults filled and every field checked, or the first problem found.
func (c Config) Resolved() (Config, error) {
	if c.Remote == "" {
		c.Remote = "origin"
	}
	if c.Branch == "" {
		c.Branch = "dev"
	}
	if c.Git == "" {
		c.Git = "git"
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = "CRW"
	}
	if c.TagPrefix == "" {
		c.TagPrefix = "cc-q"
	}
	if c.PluginDir == "" {
		c.PluginDir = "plugins/crw"
	}
	if c.PluginManifest == "" {
		c.PluginManifest = "plugins/crw/.codex-plugin/plugin.json"
	}
	if c.IntervalSeconds == 0 {
		c.IntervalSeconds = 60
	}
	var problems []string
	for _, p := range []struct{ name, value string }{
		{"checkout", c.Checkout}, {"queue_dir", c.QueueDir}, {"lock_file", c.LockFile},
		{"runs_dir", c.RunsDir}, {"work_root", c.WorkRoot},
	} {
		switch {
		case p.value == "":
			problems = append(problems, p.name+" is required")
		case !filepath.IsAbs(p.value):
			problems = append(problems, p.name+" must be an absolute path")
		}
	}
	for _, p := range []struct {
		name string
		argv []string
	}{{"gate_command", c.GateCommand}, {"verify_command", c.VerifyCommand}, {"record_version_command", c.RecordVersionCommand}} {
		if len(p.argv) == 0 || p.argv[0] == "" {
			problems = append(problems, p.name+" is required")
		}
	}
	if !keyPrefixRE.MatchString(c.KeyPrefix) {
		problems = append(problems, "key_prefix must be letters and digits")
	}
	if !tagPrefixRE.MatchString(c.TagPrefix) {
		problems = append(problems, "tag_prefix must be a file name part")
	}
	if !refNameRE.MatchString(c.Remote) || strings.Contains(c.Remote, "/") {
		problems = append(problems, "remote must be a remote name")
	}
	if !refNameRE.MatchString(c.Branch) || strings.Contains(c.Branch, "..") || strings.HasSuffix(c.Branch, "/") || strings.HasSuffix(c.Branch, ".lock") {
		problems = append(problems, "branch must be a branch name")
	}
	for _, p := range []struct{ name, value string }{{"plugin_dir", c.PluginDir}, {"plugin_manifest", c.PluginManifest}} {
		if filepath.IsAbs(p.value) || p.value != filepath.ToSlash(filepath.Clean(p.value)) || strings.HasPrefix(p.value, "..") {
			problems = append(problems, p.name+" must be a clean path relative to the repository")
		}
	}
	if c.PullRequestRepo != "" && !repoRE.MatchString(c.PullRequestRepo) {
		problems = append(problems, "pull_request_repo must be owner/name")
	}
	if c.ReviewCommand != nil && (len(c.ReviewCommand) == 0 || c.ReviewCommand[0] == "") {
		problems = append(problems, "review_command must name a program")
	}
	if c.IntervalSeconds < 1 {
		problems = append(problems, "interval_seconds must be positive")
	}
	if len(problems) > 0 {
		return c, errors.New("the integrate section of the configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// Interval is the pause between passes of watch.
func (c Config) Interval() time.Duration { return time.Duration(c.IntervalSeconds) * time.Second }

// remoteRef is the remote-tracking ref the heads are merged onto.
func (c Config) remoteRef() string { return "refs/remotes/" + c.Remote + "/" + c.Branch }

// key is the issue key of a queue file name, and node the relay-style node name.
func (c Config) key(name string) string  { return c.KeyPrefix + "-" + name }
func (c Config) node(name string) string { return strings.ToLower(c.KeyPrefix) + "-" + name }

// expand replaces the placeholders of one argument.
func expand(arg string, pairs ...string) string {
	return strings.NewReplacer(pairs...).Replace(arg)
}

func expandAll(argv []string, pairs ...string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = expand(a, pairs...)
	}
	return out
}
