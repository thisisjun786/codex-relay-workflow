package manage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// crw manage premerge: the pre-merge evaluation of a pull request and the parent's dispositions of
// what it found, written as premerge-record/1 (CRW-952 owns the format, the validator and the
// judgment). This file only makes the record: it grades the pull request's head merged into dev with
// the audit's grader, and it adds the parent's dispositions to the same record. It does not classify a
// disposition or decide PASS or BLOCK; the relay's acceptance does that, from the record alone.

// PremergeError is a refusal or a failure of the pre-merge evaluation or disposition. Exit is the
// status the command line ends with: 1 when no grade or record came out, 2 for a command line this
// command cannot use, 3 for a refusal before anything was graded or recorded. Reason is the name the
// refusal carries, Detail the sentence for the operator.
type PremergeError struct {
	Exit   int
	Reason string
	Detail string
}

func (e *PremergeError) Error() string { return e.Reason + ": " + e.Detail }

// premergeFail is a PremergeError with a formatted detail.
func premergeFail(exit int, reason, format string, args ...any) *PremergeError {
	return &PremergeError{Exit: exit, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// PremergeEvalOptions is one evaluation: the pull request, and the optional head it must still be at,
// the plan node to bind when the issue has more than one live node, the inputs directory and whether
// an existing record for the head may be replaced.
type PremergeEvalOptions struct {
	PR      int
	Head    string
	Node    string
	Inputs  string
	Replace bool
}

// PremergeDefect is one defect of the evaluation as the command prints it.
type PremergeDefect struct {
	ID         string `json:"id"`
	Severity   string `json:"severity"`
	Impact     string `json:"impact"`
	Introduced bool   `json:"introduced"`
	InPromise  bool   `json:"in_promise"`
}

// PremergeEvalResult is what the evaluation prints: where the record and the grade are, what they are
// about, the score, the criteria that are not PASS and the defects.
type PremergeEvalResult struct {
	Record  string           `json:"record"`
	Grade   string           `json:"grade"`
	PR      int              `json:"pr"`
	Issue   string           `json:"issue"`
	Node    string           `json:"node"`
	Head    string           `json:"head"`
	Dev     string           `json:"dev"`
	Score   float64          `json:"score"`
	NotPass []string         `json:"not_pass"`
	Defects []PremergeDefect `json:"defects"`
}

// PremergeDisposeOptions is one disposition of a finding of a record.
type PremergeDisposeOptions struct {
	Record   string
	Ref      string
	Class    string
	Note     string
	FollowUp string
	By       string
}

// PremergeDisposeResult is what the disposition prints. Replaced is where the record before this
// disposition was kept, when the disposition replaced one of the same ref, else empty.
type PremergeDisposeResult struct {
	Record   string `json:"record"`
	Ref      string `json:"ref"`
	Class    string `json:"class"`
	By       string `json:"by"`
	At       string `json:"at"`
	Replaced string `json:"replaced"`
}

// premergePrompt is the grader prompt of the pre-merge evaluation, embedded so the record's
// prompt_digest names the bytes the grader was given.
//
//go:embed premerge_prompt.md
var premergePrompt string

// The fixed names of the evaluation.
const (
	premergeStateSubdir  = "premerge"
	premergeBundlesDir   = "bundles"
	premergeLocksDir     = "locks"
	premergeBundlePrefix = "pr-"
	premergeRecordPrefix = "pr"
	premergeHeadChars    = 8
	premergeManifestPath = ".codex-plugin/plugin.json"
	premergeStampFormat  = "20060102T150405Z"
	premergeEvalBy       = "crw manage premerge eval"
	premergeGradeLimit   = 8 << 20
	premergeCommitName   = "crw manage premerge"
	premergeCommitEmail  = "crw-manage-premerge@example.invalid"
)

// premergeSection is the premerge section of the configuration: where the bundles and the records go,
// who graded (the record names the model and effort the configured grader command runs with) and the
// project whose plans the node is looked for in (empty: every plan of the store).
type premergeSection struct {
	BundleDir    string `json:"bundle_dir"`
	RecordDir    string `json:"record_dir"`
	GraderModel  string `json:"grader_model"`
	GraderEffort string `json:"grader_effort"`
	Project      string `json:"project"`
}

func premergeSectionOf(cfg *Config) (premergeSection, error) {
	var section premergeSection
	if cfg != nil {
		if err := cfg.Section("premerge", &section); err != nil {
			return premergeSection{}, fmt.Errorf("the premerge section of the configuration: %w", err)
		}
	}
	return section, nil
}

// premergeStateDir is where the evaluation keeps what the configuration does not place: below the
// manage state directory, apart from the audit's.
func premergeStateDir(e *Env, cfg *Config) string {
	return filepath.Join(auditStateDir(e, cfg), premergeStateSubdir)
}

func premergeBundleRoot(e *Env, cfg *Config, section premergeSection) string {
	if section.BundleDir != "" {
		return section.BundleDir
	}
	return filepath.Join(premergeStateDir(e, cfg), premergeBundlesDir)
}

func premergeRecordDir(e *Env, cfg *Config, section premergeSection) string {
	if section.RecordDir != "" {
		return section.RecordDir
	}
	return premergeStateDir(e, cfg)
}

// premergeRecordName is the file a record lives in: the pull request and the first characters of the head.
func premergeRecordName(pr int, head string) string {
	return premergeRecordPrefix + strconv.Itoa(pr) + "-" + head[:premergeHeadChars] + ".json"
}

// premergeParentName is the name the configuration gives the manage parent, when it gives one: the label
// of the one configured parent, else the thread id of it. Two or none name nobody.
func premergeParentName(cfg *Config) string {
	if cfg == nil || len(cfg.Parents) != 1 {
		return ""
	}
	for thread, label := range cfg.Parents {
		if label != "" {
			return label
		}
		return thread
	}
	return ""
}

// ---- the evaluation ----

// premergePull is what gh answers for a pull request.
type premergePull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HeadOID string `json:"headRefOid"`
	State   string `json:"state"`
}

var premergeFullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func premergePullRequest(ctx context.Context, cfg *Config, number int) (premergePull, error) {
	args := []string{"pr", "view", strconv.Itoa(number)}
	if cfg != nil && cfg.Repository != "" {
		args = append(args, "--repo", cfg.Repository)
	}
	args = append(args, "--json", "number,title,body,headRefOid,state")
	out, err := auditPRGh(ctx, args...)
	if err != nil {
		return premergePull{}, premergeFail(3, "pr_unreadable", "%v", err)
	}
	var pull premergePull
	if err := json.Unmarshal(out, &pull); err != nil {
		return premergePull{}, premergeFail(3, "pr_unreadable", "the gh pull request answer: %v", err)
	}
	if pull.Number != number || !premergeFullSHA.MatchString(pull.HeadOID) {
		return premergePull{}, premergeFail(3, "pr_unreadable", "gh answered pull request %d with head %q", pull.Number, pull.HeadOID)
	}
	return pull, nil
}

// premergeHeadMatches reports whether the head the caller named is the head the pull request is at:
// the whole id or its first characters, seven at least.
func premergeHeadMatches(want, have string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	return len(want) >= 7 && strings.HasPrefix(have, want)
}

// premergeNode is one live node of an issue in a plan.
type premergeNode struct {
	Plan   string
	Node   string
	Digest string
}

// premergeLiveNodes reads the relay store, read-only, for the live implementation nodes of an issue:
// the nodes of the project's plans (every plan when no project is configured) that carry the issue
// key and that the plan has not cancelled or archived. A node's lifecycle of paused is still live.
func premergeLiveNodes(ctx context.Context, e *Env, cfg *Config, project, issue string) ([]premergeNode, error) {
	state, err := e.relayHelperState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	handle, err := relayReadOpenStore(ctx, state)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	var nodes []premergeNode
	err = handle.ReadSnapshot(ctx, func(ctx context.Context, st *store.Store) error {
		query, args := "SELECT plan_id FROM dag_plans", []any(nil)
		if project != "" {
			query, args = query+" WHERE project_key = ?", []any{project}
		}
		plans, err := relayReadIDs(ctx, st, query+" ORDER BY plan_id", args)
		if err != nil {
			return err
		}
		for _, plan := range plans {
			snap, _, err := dag.SnapshotAt(ctx, st.Q(ctx), plan, 0)
			if err != nil {
				return fmt.Errorf("plan %s: %w", plan, err)
			}
			for _, n := range snap.Nodes {
				if n.IssueKey == issue && n.Kind == dag.NodeImplementation && n.Lifecycle != dag.LifeCancelled && n.Lifecycle != dag.LifeArchived {
					nodes = append(nodes, premergeNode{Plan: plan, Node: n.NodeID, Digest: n.CriteriaSetDigest})
				}
			}
		}
		return nil
	})
	return nodes, err
}

// premergeChooseNode picks the node a record is bound to: the only live node of the issue, or the one
// --node names among them.
func premergeChooseNode(nodes []premergeNode, want, issue string) (premergeNode, error) {
	var candidates []premergeNode
	for _, n := range nodes {
		if want == "" || n.Node == want {
			candidates = append(candidates, n)
		}
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Node + " (plan " + n.Plan + ")"
	}
	switch {
	case len(nodes) == 0:
		return premergeNode{}, premergeFail(3, "node_unknown", "no live implementation node of %s in the plans read", issue)
	case len(candidates) == 0:
		return premergeNode{}, premergeFail(3, "node_unknown", "--node %s is not a live node of %s; they are %s", want, issue, strings.Join(names, ", "))
	case len(candidates) > 1:
		return premergeNode{}, premergeFail(3, "node_unknown", "%s has %d live nodes (%s); name one with --node", issue, len(candidates), strings.Join(names, ", "))
	}
	return candidates[0], nil
}

// premergeCriteriaMarkdown writes the registered criteria the way the grader reads them, one line each.
func premergeCriteriaMarkdown(issue string, criteria []map[string]any) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s: acceptance criteria given to the implementer\n\n", issue)
	for _, criterion := range criteria {
		id, _ := criterion["id"].(string)
		if id == "" {
			continue
		}
		title := ""
		for _, key := range []string{"title", "text", "statement"} {
			if text, _ := criterion[key].(string); text != "" {
				title = text
				break
			}
		}
		line := "- " + id + ": " + title
		if required, ok := criterion["required"].(bool); ok && !required {
			line += " (optional)"
		}
		out.WriteString(line + "\n")
	}
	return out.String()
}

// premergeReadInputs reads the regular files of the inputs directory (not its subdirectories, not links).
func premergeReadInputs(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if dir == "" {
		return files, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, premergeFail(3, "inputs_rejected", "the inputs directory cannot be read: %v", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return nil, premergeFail(3, "inputs_rejected", "%s is not a regular file: the inputs hold files only, no link and no directory", entry.Name())
		}
		file, err := os.OpenFile(filepath.Join(dir, entry.Name()), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, premergeFail(3, "inputs_rejected", "%s cannot be read: %v", entry.Name(), err)
		}
		body, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			return nil, premergeFail(3, "inputs_rejected", "%s cannot be read: %v", entry.Name(), err)
		}
		files[entry.Name()] = body
	}
	return files, nil
}

// ---- git ----

// premergeGitResult is one git run that ended with a status, whatever it was.
type premergeGitResult struct {
	stdout []byte
	stderr string
	code   int
}

// premergeGitEnv is the environment git runs in: the process's, without the variables that would point
// git at another repository or index, and with the extra assignments after.
func premergeGitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR", "GIT_NAMESPACE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// premergeGitRun runs git in repo and reports its exit status; an error is a git that could not run at all.
func premergeGitRun(ctx context.Context, repo string, env []string, args ...string) (premergeGitResult, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	cmd.Env = premergeGitEnv(env...)
	var out bytes.Buffer
	errOut := &auditLog{}
	cmd.Stdout = &out
	cmd.Stderr = errOut
	err := cmd.Run()
	result := premergeGitResult{stdout: out.Bytes(), stderr: strings.TrimSpace(errOut.String())}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.code = exit.ExitCode()
		return result, nil
	}
	return result, err
}

// premergeGitOK runs git and refuses any status but 0.
func premergeGitOK(ctx context.Context, repo string, env []string, args ...string) ([]byte, error) {
	result, err := premergeGitRun(ctx, repo, env, args...)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if result.code != 0 {
		return nil, fmt.Errorf("git %s: exit status %d: %s", strings.Join(args, " "), result.code, result.stderr)
	}
	return result.stdout, nil
}

// premergeMerged is the merge the evaluation grades: the commit, dev's tip, and whether the tree took dev's
// plugin manifest in place of a version-line conflict.
type premergeMerged struct {
	commit        string
	dev           string
	manifestPaths []string
}

// premergeFetch brings dev and the pull request's head into the checkout's object store without a
// working tree change and without a ref of the pull request, and returns dev's tip. The head the pull
// request advertises must be the one gh answered; a head that moved between the two is head_moved.
func premergeFetch(ctx context.Context, co auditPkgCheckout, number int, head string) (string, error) {
	remote, _ := auditPkgSplitRef(co.BaseRef)
	pullRef := "refs/pull/" + strconv.Itoa(number) + "/head"
	advertised, err := premergeGitOK(ctx, co.Repository, nil, "ls-remote", remote, pullRef)
	if err != nil {
		return "", premergeFail(3, "git_failed", "%v", err)
	}
	if fields := strings.Fields(string(advertised)); len(fields) < 2 || fields[0] != head {
		return "", premergeFail(2, "head_moved", "the remote's %s is not %s", pullRef, head)
	}
	trackingRef := "refs/remotes/" + remote + "/" + auditPRBaseRef
	if _, err := premergeGitOK(ctx, co.Repository, nil, "fetch", "--quiet", "--no-write-fetch-head", remote, "+refs/heads/"+auditPRBaseRef+":"+trackingRef); err != nil {
		return "", premergeFail(3, "git_failed", "%v", err)
	}
	if _, err := premergeGitOK(ctx, co.Repository, nil, "fetch", "--quiet", "--no-write-fetch-head", remote, pullRef); err != nil {
		return "", premergeFail(3, "git_failed", "%v", err)
	}
	dev, err := premergeGitOK(ctx, co.Repository, nil, "rev-parse", "--verify", "--quiet", trackingRef+"^{commit}")
	if err != nil || !premergeFullSHA.MatchString(strings.TrimSpace(string(dev))) {
		return "", premergeFail(3, "git_failed", "dev cannot be resolved after the fetch: %v", err)
	}
	if _, err := premergeGitOK(ctx, co.Repository, nil, "cat-file", "-e", head+"^{commit}"); err != nil {
		return "", premergeFail(2, "head_moved", "the head %s is not in what %s fetched", head, pullRef)
	}
	return strings.TrimSpace(string(dev)), nil
}

// premergeMergeTree merges the head into dev as a local object. A merge that conflicts only in plugin
// manifests is made over again with dev's manifest, because the version line is regenerated by the merge
// lane; any other conflict is base_refresh_required. Nothing is pushed, no ref is made and the checkout's
// working tree and index are not touched.
func premergeMergeTree(ctx context.Context, e *Env, co auditPkgCheckout, indexDir, indexName string, number int, dev, head string) (premergeMerged, error) {
	result, err := premergeGitRun(ctx, co.Repository, nil, "merge-tree", "--write-tree", "--name-only", "-z", dev, head)
	if err != nil {
		return premergeMerged{}, premergeFail(3, "git_failed", "git merge-tree: %v", err)
	}
	records := bytes.Split(result.stdout, []byte{0})
	tree := ""
	if len(records) > 0 {
		tree = string(records[0])
	}
	var conflicted []string
	for _, record := range records[min(1, len(records)):] {
		if len(record) == 0 {
			break
		}
		conflicted = append(conflicted, string(record))
	}
	merged := premergeMerged{dev: dev}
	switch {
	case result.code == 0 && len(conflicted) == 0:
	case result.code == 1 && len(conflicted) > 0:
		for _, path := range conflicted {
			if path != premergeManifestPath && !strings.HasSuffix(path, "/"+premergeManifestPath) {
				return premergeMerged{}, premergeFail(3, "base_refresh_required", "the head %s does not merge cleanly with dev %s; conflicting paths: %s", head, dev, strings.Join(conflicted, ", "))
			}
		}
		fixed, err := premergeTreeWithDevManifests(ctx, co, indexDir, indexName, dev, tree, conflicted)
		if err != nil {
			return premergeMerged{}, err
		}
		tree, merged.manifestPaths = fixed, conflicted
	default:
		return premergeMerged{}, premergeFail(3, "git_failed", "git merge-tree exited with status %d: %s", result.code, result.stderr)
	}
	if !premergeFullSHA.MatchString(tree) {
		return premergeMerged{}, premergeFail(3, "git_failed", "git merge-tree answered %q, not a tree", tree)
	}
	stamp := e.Now().UTC().Format(time.RFC3339)
	commit, err := premergeGitOK(ctx, co.Repository, []string{
		"GIT_AUTHOR_NAME=" + premergeCommitName, "GIT_AUTHOR_EMAIL=" + premergeCommitEmail, "GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_NAME=" + premergeCommitName, "GIT_COMMITTER_EMAIL=" + premergeCommitEmail, "GIT_COMMITTER_DATE=" + stamp,
	}, "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", dev, "-p", head, "-m", "pre-merge evaluation of pull request #"+strconv.Itoa(number))
	if err != nil {
		return premergeMerged{}, premergeFail(3, "git_failed", "%v", err)
	}
	merged.commit = strings.TrimSpace(string(commit))
	return merged, nil
}

// premergeTreeWithDevManifests is the merge tree with dev's blob in place of each conflicted manifest, made
// through a temporary index file below the bundle root that is removed again.
func premergeTreeWithDevManifests(ctx context.Context, co auditPkgCheckout, indexDir, indexName, dev, tree string, paths []string) (string, error) {
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return "", premergeFail(3, "git_failed", "%v", err)
	}
	index := filepath.Join(indexDir, indexName)
	_ = os.Remove(index)
	defer func() { _ = os.Remove(index); _ = os.Remove(index + ".lock") }()
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := premergeGitOK(ctx, co.Repository, env, "read-tree", tree); err != nil {
		return "", premergeFail(3, "git_failed", "%v", err)
	}
	for _, path := range paths {
		listing, err := premergeGitOK(ctx, co.Repository, nil, "ls-tree", "-z", dev, "--", path)
		if err != nil {
			return "", premergeFail(3, "git_failed", "%v", err)
		}
		meta, _, _ := strings.Cut(strings.TrimSuffix(string(listing), "\x00"), "\t")
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return "", premergeFail(3, "base_refresh_required", "dev has no regular file %s to take in place of the conflicting one; conflicting paths: %s", path, strings.Join(paths, ", "))
		}
		if _, err := premergeGitOK(ctx, co.Repository, env, "update-index", "--add", "--cacheinfo", fields[0]+","+fields[2]+","+path); err != nil {
			return "", premergeFail(3, "git_failed", "%v", err)
		}
	}
	out, err := premergeGitOK(ctx, co.Repository, env, "write-tree")
	if err != nil {
		return "", premergeFail(3, "git_failed", "%v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// premergeTreeEntry is one file of the merge commit's tree.
type premergeTreeEntry struct {
	mode string
	sha  string
	path string
}

// premergeTreeEntries lists the merge commit's regular files. A link and a submodule are left out: the
// bundle never makes a link, because it could point out of the bundle, and a submodule has no content here.
func premergeTreeEntries(ctx context.Context, co auditPkgCheckout, commit string) ([]premergeTreeEntry, error) {
	out, err := premergeGitOK(ctx, co.Repository, nil, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	var entries []premergeTreeEntry
	for _, record := range auditPkNulRecords(out) {
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("git ls-tree record %q is not mode, type, object and path", record)
		}
		if fields[1] != "blob" || fields[0] == "120000" {
			continue
		}
		entries = append(entries, premergeTreeEntry{mode: fields[0], sha: fields[2], path: path})
	}
	return entries, nil
}

// premergeWriteTree writes the merge commit's files into dir through one `git cat-file --batch`. The files
// the change touches go through the scrub, and so do their path names; the others are written as they are.
func premergeWriteTree(ctx context.Context, co auditPkgCheckout, commit, dir string, changed map[string]bool, scrubs []string) error {
	entries, err := premergeTreeEntries(ctx, co, commit)
	if err != nil {
		return err
	}
	placed := map[string]string{}
	names := make([]string, len(entries))
	for i, entry := range entries {
		name := entry.path
		if changed[entry.path] {
			name = string(auditPRScrub([]byte(entry.path), scrubs))
		}
		if other, taken := placed[name]; taken {
			return fmt.Errorf("the paths %q and %q both come out as %q in the bundle", other, entry.path, name)
		}
		placed[name] = entry.path
		if !auditPkgContained(dir, filepath.Join(dir, filepath.FromSlash(name))) {
			return fmt.Errorf("the merge names %q, which leaves the bundle", entry.path)
		}
		names[i] = name
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = co.Repository
	cmd.Env = premergeGitEnv()
	errOut := &auditLog{}
	cmd.Stderr = errOut
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(stdout, 1<<16)
	readErr := func() error {
		for i, entry := range entries {
			if _, err := io.WriteString(stdin, entry.sha+"\n"); err != nil {
				return err
			}
			header, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[1] != "blob" {
				return fmt.Errorf("git cat-file answered %q for %s", strings.TrimSpace(header), entry.path)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || size < 0 {
				return fmt.Errorf("git cat-file answered a size %q for %s", fields[2], entry.path)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(reader, body); err != nil {
				return err
			}
			if _, err := reader.ReadByte(); err != nil {
				return err
			}
			if changed[entry.path] {
				body = auditPRScrub(body, scrubs)
			}
			if err := auditPRWriteBlob(filepath.Join(dir, filepath.FromSlash(names[i])), body); err != nil {
				return err
			}
		}
		return nil
	}()
	_ = stdin.Close()
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("git cat-file: %w: %s", waitErr, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// ---- the bundle ----

// premergeBuildBundle makes the bundle of one evaluation from nothing: the criteria, the inputs and the
// candidate (the change against dev, the pull request's description and the merged tree).
func premergeBuildBundle(ctx context.Context, e *Env, co auditPkgCheckout, root, dir string, pull premergePull, issue string, criteria []map[string]any, inputs map[string][]byte, merged premergeMerged, scrubs []string) error {
	if err := auditPkgResetDir(root, dir); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			if err := os.RemoveAll(dir); err != nil {
				fmt.Fprintf(e.Stderr, "crw manage premerge: %s: %v\n", dir, err)
			}
		}
	}()
	text := premergeCriteriaMarkdown(issue, criteria)
	if len(merged.manifestPaths) > 0 {
		text += "\nEvaluation note: the candidate's plugin manifest (" + strings.Join(merged.manifestPaths, ", ") + ") conflicted with dev only in its version line, " +
			"so this tree carries dev's manifest. The merge lane regenerates that line by the repository's built-in rule when it merges. " +
			"Do not report the manifest version line or its derived value.\n"
	}
	if err := auditPRWriteFile(filepath.Join(dir, "criteria.md"), auditPRScrub([]byte(text), scrubs)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "inputs"), 0o700); err != nil {
		return err
	}
	for name, body := range inputs {
		if err := auditPRWriteBlob(filepath.Join(dir, "inputs", name), body); err != nil {
			return err
		}
	}
	patch, err := auditPkgGit(ctx, co.Repository, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", merged.dev, merged.commit)
	if err != nil {
		return err
	}
	if err := auditPRWriteFile(filepath.Join(dir, "candidate", "diff.patch"), auditPRScrub(patch, scrubs)); err != nil {
		return err
	}
	description := pull.Title + "\n\n" + pull.Body
	if err := auditPRWriteFile(filepath.Join(dir, "candidate", "pr.md"), auditPRScrub([]byte(description), scrubs)); err != nil {
		return err
	}
	paths, err := auditPRDiffPaths(ctx, co, merged.commit)
	if err != nil {
		return err
	}
	changed := make(map[string]bool, len(paths))
	for _, path := range paths {
		changed[path] = true
	}
	if err := premergeWriteTree(ctx, co, merged.commit, filepath.Join(dir, "candidate", "tree"), changed, scrubs); err != nil {
		return err
	}
	complete = true
	return nil
}

// ---- the grade ----

// premergeImpacts are the six impacts the pre-merge prompt allows a defect.
var premergeImpacts = map[string]bool{
	"core_function": true, "safety_or_data": true, "permission_boundary": true,
	"criterion_unmet": true, "regression_introduced": true, "minor_separable": true,
}

var premergeIntegerText = regexp.MustCompile(`^-?[0-9]+$`)

// premergeGrade is a grade.json that the format accepted.
type premergeGrade struct {
	criteria map[string]premerge.Criterion
	defects  []premerge.Defect
	score    int
	summary  string
}

// premergeReadGrade reads the grade.json a grader left, strictly: every criterion has a verdict of PASS,
// PARTIAL or FAIL and its evidence is text; every defect has a severity P0 to P3, one of the six impacts
// and the booleans introduced and in_promise; the score is an integer from 0 to 10; and there is at least
// one criterion. The defects are numbered d1, d2.. in the order the grader gave them. A member the
// contract has no place for (paths, trigger, where) is left in the file and ignored here.
func premergeReadGrade(path string) (premergeGrade, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return premergeGrade{}, fmt.Errorf("the grader left no grade.json: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, premergeGradeLimit+1))
	_ = file.Close()
	if err != nil {
		return premergeGrade{}, err
	}
	if len(data) > premergeGradeLimit {
		return premergeGrade{}, errors.New("grade.json is larger than the limit")
	}
	var doc struct {
		Criteria map[string]struct {
			Verdict  *string `json:"verdict"`
			Evidence string  `json:"evidence"`
		} `json:"criteria"`
		Defects *[]struct {
			Severity   *string `json:"severity"`
			Impact     *string `json:"impact"`
			Introduced *bool   `json:"introduced"`
			InPromise  *bool   `json:"in_promise"`
			What       string  `json:"what"`
		} `json:"defects"`
		Score   json.RawMessage `json:"score"`
		Summary string          `json:"summary"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return premergeGrade{}, fmt.Errorf("grade.json does not match the format: %w", err)
	}
	grade := premergeGrade{criteria: map[string]premerge.Criterion{}, summary: doc.Summary}
	if len(doc.Criteria) == 0 {
		return premergeGrade{}, errors.New("grade.json has no criteria")
	}
	for id, criterion := range doc.Criteria {
		if id == "" || criterion.Verdict == nil || !auditVerdicts[*criterion.Verdict] {
			return premergeGrade{}, fmt.Errorf("criterion %q has no verdict PASS, PARTIAL or FAIL", id)
		}
		grade.criteria[id] = premerge.Criterion{Verdict: *criterion.Verdict, Evidence: criterion.Evidence}
	}
	if doc.Defects == nil {
		return premergeGrade{}, errors.New("grade.json has no defects list")
	}
	for i, defect := range *doc.Defects {
		id := "d" + strconv.Itoa(i+1)
		switch {
		case defect.Severity == nil || !auditSeverities[*defect.Severity]:
			return premergeGrade{}, fmt.Errorf("defect %s has no severity P0 to P3", id)
		case defect.Impact == nil || !premergeImpacts[*defect.Impact]:
			return premergeGrade{}, fmt.Errorf("defect %s has no impact of the six the prompt names", id)
		case defect.Introduced == nil || defect.InPromise == nil:
			return premergeGrade{}, fmt.Errorf("defect %s lacks introduced or in_promise as true or false", id)
		}
		grade.defects = append(grade.defects, premerge.Defect{
			ID: id, Severity: *defect.Severity, Impact: *defect.Impact, Introduced: *defect.Introduced, InPromise: *defect.InPromise, What: defect.What,
		})
	}
	score := strings.TrimSpace(string(doc.Score))
	if !premergeIntegerText.MatchString(score) {
		return premergeGrade{}, fmt.Errorf("the score %q is not an integer from 0 to 10", score)
	}
	value, err := strconv.Atoi(score)
	if err != nil || value < 0 || value > 10 {
		return premergeGrade{}, fmt.Errorf("the score %q is not an integer from 0 to 10", score)
	}
	grade.score = value
	return grade, nil
}

// ---- the record ----

// premergeDirLock holds an exclusive lock on a directory, so two writers of its records take turns. With
// create, a directory that is not there yet is made; without it, a missing one is an error.
func premergeDirLock(dir string, create bool) (func(), error) {
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	handle, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN); _ = handle.Close() }, nil
}

// premergeReplacedPath is where the record before a replacement is kept: beside it, named for the moment
// of the replacement.
func premergeReplacedPath(path string, now time.Time) (string, error) {
	base := strings.TrimSuffix(path, ".json") + ".replaced-" + now.UTC().Format(premergeStampFormat)
	for n := 1; n < 1000; n++ {
		candidate := base + ".json"
		if n > 1 {
			candidate = base + "-" + strconv.Itoa(n) + ".json"
		}
		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name for the replaced record beside %s", path)
}

// premergeWriteRecord writes data to path through a temporary file of the same directory and a rename. When
// keep is not empty, the record that is at path is first made available at keep (a hard link, else a copy),
// so a failure leaves the old record where it was and the kept one is never a half.
func premergeWriteRecord(path string, data []byte, keep string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".premerge-*.tmp")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if keep != "" {
		if err := os.Link(path, keep); err != nil {
			old, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if err := os.WriteFile(keep, old, 0o600); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	if handle, err := os.Open(dir); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

// premergeEncode is the record as it is written, validated by the contract's own validator.
func premergeEncode(record premerge.Record) ([]byte, error) {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if _, err := premerge.Decode(data); err != nil {
		return nil, err
	}
	return data, nil
}

// premergeEvalLock takes the lock of one pull request head, in the premerge state directory, so two
// evaluations of it do not rebuild one bundle at once. A second one is refused rather than waited for.
func premergeEvalLock(e *Env, cfg *Config, number int, head string) (func(), error) {
	dir := filepath.Join(premergeStateDir(e, cfg), premergeLocksDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, premergeFail(1, "lock_failed", "%v", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, premergeBundlePrefix+strconv.Itoa(number)+"-"+head[:premergeHeadChars]+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, premergeFail(1, "lock_failed", "%v", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, premergeFail(3, "eval_in_progress", "an evaluation of pull request %d at %s is running", number, head)
		}
		return nil, premergeFail(1, "lock_failed", "%v", err)
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

// PremergeEval grades the head of a pull request merged into dev against the criteria the relay holds for
// the issue, and writes the premerge-record/1 of it with no disposition. Nothing is written when the
// pull request, the plan, the merge or the grader does not give a grade the format accepts.
func PremergeEval(ctx context.Context, e *Env, cfg *Config, opts PremergeEvalOptions) (PremergeEvalResult, error) {
	if opts.PR <= 0 {
		return PremergeEvalResult{}, premergeFail(2, "usage", "the pull request is a number above zero")
	}
	section, err := premergeSectionOf(cfg)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "config_invalid", "%v", err)
	}
	audit, err := auditConfigOf(cfg)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "config_invalid", "%v", err)
	}
	switch {
	case len(audit.Grader) == 0:
		return PremergeEvalResult{}, premergeFail(3, "grader_unconfigured", "the audit section of the configuration names no grader command")
	case section.GraderModel == "" || section.GraderEffort == "":
		return PremergeEvalResult{}, premergeFail(3, "grader_unconfigured", "the premerge section of the configuration names no grader_model and grader_effort")
	}
	co, err := auditPkgCheckoutOf(cfg)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "config_invalid", "%v", err)
	}
	if co.Repository == "" {
		return PremergeEvalResult{}, premergeFail(3, "checkout_unconfigured", "the checkout section names no repository")
	}
	prSection, err := auditPRSectionOf(cfg)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "config_invalid", "%v", err)
	}
	pattern, err := auditPRPattern(prSection)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "config_invalid", "%v", err)
	}
	inputs, err := premergeReadInputs(opts.Inputs)
	if err != nil {
		return PremergeEvalResult{}, err
	}

	pull, err := premergePullRequest(ctx, cfg, opts.PR)
	if err != nil {
		return PremergeEvalResult{}, err
	}
	if pull.State != "OPEN" {
		return PremergeEvalResult{}, premergeFail(3, "pr_not_open", "pull request %d is %s: only an open pull request is graded", opts.PR, pull.State)
	}
	if opts.Head != "" && !premergeHeadMatches(opts.Head, pull.HeadOID) {
		return PremergeEvalResult{}, premergeFail(2, "head_moved", "pull request %d is at %s, not %s", opts.PR, pull.HeadOID, opts.Head)
	}
	issue := pattern.FindString(pull.Title)
	if issue == "" {
		return PremergeEvalResult{}, premergeFail(3, "issue_unknown", "the title of pull request %d carries no issue key", opts.PR)
	}
	recordPath := filepath.Join(premergeRecordDir(e, cfg, section), premergeRecordName(opts.PR, pull.HeadOID))
	if _, err := os.Lstat(recordPath); err == nil && !opts.Replace {
		return PremergeEvalResult{}, premergeFail(3, "record_exists", "%s exists; --replace grades again and keeps it beside the new one", recordPath)
	}

	nodes, err := premergeLiveNodes(ctx, e, cfg, section.Project, issue)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "relay_unreadable", "%v", err)
	}
	node, err := premergeChooseNode(nodes, opts.Node, issue)
	if err != nil {
		return PremergeEvalResult{}, err
	}
	child, err := auditPRChildOf(ctx, e, cfg, issue)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "relay_unreadable", "%v", err)
	}
	criteria, unavailable, err := auditPRCriteria(ctx, e, cfg, child.Relationship)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(3, "relay_unreadable", "%v", err)
	}
	if unavailable {
		return PremergeEvalResult{}, premergeFail(3, "criteria_unavailable", "the relay holds no criteria for %s: a record would bind a digest of criteria the grader never saw", issue)
	}

	unlock, err := premergeEvalLock(e, cfg, opts.PR, pull.HeadOID)
	if err != nil {
		return PremergeEvalResult{}, err
	}
	defer unlock()

	dev, err := premergeFetch(ctx, co, opts.PR, pull.HeadOID)
	if err != nil {
		return PremergeEvalResult{}, err
	}
	root := premergeBundleRoot(e, cfg, section)
	merged, err := premergeMergeTree(ctx, e, co, root, ".premerge-index-"+strconv.Itoa(opts.PR)+"-"+pull.HeadOID[:premergeHeadChars], opts.PR, dev, pull.HeadOID)
	if err != nil {
		return PremergeEvalResult{}, err
	}
	bundle := filepath.Join(root, premergeBundlePrefix+strconv.Itoa(opts.PR)+"-"+pull.HeadOID[:premergeHeadChars])
	if err := premergeBuildBundle(ctx, e, co, root, bundle, pull, issue, criteria, inputs, merged, prSection.Scrub); err != nil {
		return PremergeEvalResult{}, premergeFail(3, "bundle_rejected", "%v", err)
	}
	absBundle, err := auditBundleResolvedPath(bundle)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(1, "grader_failed", "%v", err)
	}

	log := &auditLog{}
	switch auditRunGrader(ctx, audit, absBundle, premergePrompt, log) {
	case auditStatusTimeout:
		return PremergeEvalResult{}, premergeFail(1, "grader_timeout", "the grader ran past %d seconds: %s", audit.GraderTimeoutSeconds, auditFirstLine(log.String()))
	case auditStatusInvalid:
		return PremergeEvalResult{}, premergeFail(1, "grader_failed", "the bundle could not be prepared for the grader")
	}
	gradePath := filepath.Join(absBundle, auditGradeFile)
	grade, err := premergeReadGrade(gradePath)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(1, "grade_invalid", "%v (%s)", err, auditFirstLine(log.String()))
	}

	promptSum := sha256.Sum256([]byte(premergePrompt))
	score := float64(grade.score)
	by := premergeParentName(cfg)
	if by == "" {
		by = premergeEvalBy
	}
	record := premerge.Record{
		Schema: premerge.RecordSchema, Issue: issue, Node: node.Node, Head: pull.HeadOID, Dev: merged.dev, CriteriaDigest: node.Digest,
		Grader:   premerge.Grader{Model: section.GraderModel, Effort: section.GraderEffort, PromptDigest: "sha256:" + hex.EncodeToString(promptSum[:])},
		GradedAt: e.Now().UTC().Format(time.RFC3339), Criteria: grade.criteria, Defects: grade.defects, Score: &score, Summary: grade.summary,
		Dispositions: premerge.Dispositions{By: by},
	}
	if record.Defects == nil {
		record.Defects = []premerge.Defect{}
	}
	data, err := premergeEncode(record)
	if err != nil {
		return PremergeEvalResult{}, premergeFail(1, "grade_invalid", "the contract's validator refuses the record: %v", err)
	}
	if err := premergeStoreRecord(recordPath, data, opts.Replace, e.Now()); err != nil {
		return PremergeEvalResult{}, err
	}

	result := PremergeEvalResult{
		Record: recordPath, Grade: gradePath, PR: opts.PR, Issue: issue, Node: node.Node, Head: pull.HeadOID, Dev: merged.dev,
		Score: score, NotPass: []string{}, Defects: []PremergeDefect{},
	}
	ids := make([]string, 0, len(record.Criteria))
	for id := range record.Criteria {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if record.Criteria[id].Verdict != "PASS" {
			result.NotPass = append(result.NotPass, id)
		}
	}
	for _, d := range record.Defects {
		result.Defects = append(result.Defects, PremergeDefect{ID: d.ID, Severity: d.Severity, Impact: d.Impact, Introduced: d.Introduced, InPromise: d.InPromise})
	}
	return result, nil
}

// premergeStoreRecord writes the evaluation's record. With replace, the record that is there is kept beside
// the new one; without it, a record that appeared while the grader ran is not overwritten.
func premergeStoreRecord(path string, data []byte, replace bool, now time.Time) error {
	unlock, err := premergeDirLock(filepath.Dir(path), true)
	if err != nil {
		return premergeFail(1, "record_write_failed", "%v", err)
	}
	defer unlock()
	keep := ""
	if _, err := os.Lstat(path); err == nil {
		if !replace {
			return premergeFail(3, "record_exists", "%s appeared while the grader ran; --replace grades again and keeps it beside the new one", path)
		}
		if keep, err = premergeReplacedPath(path, now); err != nil {
			return premergeFail(1, "record_write_failed", "%v", err)
		}
	}
	if err := premergeWriteRecord(path, data, keep); err != nil {
		return premergeFail(1, "record_write_failed", "%v", err)
	}
	return nil
}

// ---- the dispositions ----

// premergeClasses are the classes of a disposition the contract names.
var premergeClasses = map[string]bool{"blocking": true, "separable": true, "carried": true, "not_applicable": true, "already_resolved": true}

var premergeFollowUp = regexp.MustCompile(`^CRW-[0-9]+$`)

// PremergeDispose writes the parent's disposition of one finding (a criterion id or a defect id) into the
// record, replacing an earlier disposition of the same ref and keeping the record it replaced. It checks
// the record with the contract's validator before and after, and does not judge whether the class is one
// the gate will accept for the finding: the relay's acceptance does.
func PremergeDispose(_ context.Context, e *Env, cfg *Config, opts PremergeDisposeOptions) (PremergeDisposeResult, error) {
	switch {
	case opts.Record == "" || opts.Ref == "" || opts.Class == "" || strings.TrimSpace(opts.Note) == "":
		return PremergeDisposeResult{}, premergeFail(2, "usage", "a disposition names the record, --ref, --class and --note")
	case !premergeClasses[opts.Class]:
		return PremergeDisposeResult{}, premergeFail(2, "class_unknown", "the class %q is none of blocking, separable, carried, not_applicable, already_resolved", opts.Class)
	case opts.FollowUp != "" && !premergeFollowUp.MatchString(opts.FollowUp):
		return PremergeDisposeResult{}, premergeFail(2, "usage", "--follow-up %q is not an issue key like CRW-123", opts.FollowUp)
	}
	by := opts.By
	if by == "" {
		by = premergeParentName(cfg)
	}
	if by == "" {
		return PremergeDisposeResult{}, premergeFail(2, "by_unknown", "name who disposes with --by: the configuration names no single manage parent")
	}
	unlock, err := premergeDirLock(filepath.Dir(opts.Record), false)
	if err != nil {
		return PremergeDisposeResult{}, premergeFail(3, "record_unreadable", "%v", err)
	}
	defer unlock()
	raw, err := os.ReadFile(opts.Record)
	if err != nil {
		return PremergeDisposeResult{}, premergeFail(3, "record_unreadable", "%v", err)
	}
	record, err := premerge.Decode(raw)
	if err != nil {
		return PremergeDisposeResult{}, premergeFail(3, "record_invalid", "%v", err)
	}
	known := false
	if _, ok := record.Criteria[opts.Ref]; ok {
		known = true
	}
	for _, d := range record.Defects {
		known = known || d.ID == opts.Ref
	}
	if !known {
		return PremergeDisposeResult{}, premergeFail(2, "ref_unknown", "%q is neither a criterion nor a defect of the record", opts.Ref)
	}

	// The document is changed as a document, so a member this build does not know stays as it was.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return PremergeDisposeResult{}, premergeFail(3, "record_invalid", "%v", err)
	}
	dispositions, _ := doc["dispositions"].(map[string]any)
	if dispositions == nil {
		return PremergeDisposeResult{}, premergeFail(3, "record_invalid", "dispositions is not an object")
	}
	now := e.Now().UTC()
	item := map[string]any{"ref": opts.Ref, "class": opts.Class, "note": opts.Note, "at": now.Format(time.RFC3339)}
	if opts.FollowUp != "" {
		item["followUp"] = opts.FollowUp
	}
	var items []any
	replaced := false
	placed := false
	existing, _ := dispositions["items"].([]any)
	for _, old := range existing {
		if fields, ok := old.(map[string]any); ok && fields["ref"] == opts.Ref {
			replaced = true
			if !placed {
				items = append(items, item)
				placed = true
			}
			continue
		}
		items = append(items, old)
	}
	if !placed {
		items = append(items, item)
	}
	dispositions["items"] = items
	dispositions["by"] = by
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return PremergeDisposeResult{}, premergeFail(1, "record_write_failed", "%v", err)
	}
	data = append(data, '\n')
	if _, err := premerge.Decode(data); err != nil {
		return PremergeDisposeResult{}, premergeFail(3, "record_invalid", "the record with this disposition is refused by the contract's validator: %v", err)
	}
	result := PremergeDisposeResult{Record: opts.Record, Ref: opts.Ref, Class: opts.Class, By: by, At: now.Format(time.RFC3339)}
	keep := ""
	if replaced {
		if keep, err = premergeReplacedPath(opts.Record, now); err != nil {
			return PremergeDisposeResult{}, premergeFail(1, "record_write_failed", "%v", err)
		}
		result.Replaced = keep
	}
	if err := premergeWriteRecord(opts.Record, data, keep); err != nil {
		return PremergeDisposeResult{}, premergeFail(1, "record_write_failed", "%v", err)
	}
	return result, nil
}

// ---- the command ----

const premergeUsage = "usage: crw manage premerge eval <PR> [--head SHA] [--node ID] [--inputs DIR] [--replace]\n" +
	"       crw manage premerge dispose <record> --ref ID --class CLASS --note TEXT [--follow-up KEY] [--by NAME]"

var premergeCommand = Command{
	Name: "premerge", Summary: "grade a pull request before the merge and record the parent's dispositions", Run: premergeRun,
	HelpRequested: premergeHelpRequested,
}

func init() { Register(premergeCommand) }

// premergeHelpRequested reports whether the arguments ask for the usage: a bare "help", or -h or --help as
// the command's or its subcommand's first argument.
func premergeHelpRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "help":
		return true
	case "eval", "dispose":
		return len(args) > 1 && (args[1] == "-h" || args[1] == "--help" || args[1] == "help")
	}
	return false
}

func premergeRun(ctx context.Context, e *Env, args []string) int {
	return premergeRunWith(ctx, e, coreDefaults(e), args)
}

// premergeUsageError is a command line this command cannot use: the usage and the reason, exit 2.
func premergeUsageError(e *Env, sub string, err error) int {
	fmt.Fprintln(e.Stderr, premergeUsage)
	fmt.Fprintf(e.Stderr, "crw manage premerge%s: error: %v\n", sub, err)
	return usageExit
}

// premergeOptions reads --name value and --name=value options. names lists the options that take a value and
// flags the ones that do not; an option given twice, an unknown one and a value that is itself an option are
// errors. The rest are the positional arguments.
func premergeOptions(args []string, names, flags []string) (values map[string]string, set map[string]bool, rest []string, err error) {
	values, set = map[string]string{}, map[string]bool{}
	isName, isFlag := map[string]bool{}, map[string]bool{}
	for _, n := range names {
		isName[n] = true
	}
	for _, n := range flags {
		isFlag[n] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			rest = append(rest, arg)
			continue
		}
		key, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		switch {
		case isFlag[key] && !hasValue:
		case isName[key]:
			if !hasValue {
				i++
				if i >= len(args) || strings.HasPrefix(args[i], "--") {
					return nil, nil, nil, fmt.Errorf("the option --%s needs a value", key)
				}
				value = args[i]
			}
		default:
			return nil, nil, nil, fmt.Errorf("unknown option %s", arg)
		}
		if set[key] {
			return nil, nil, nil, fmt.Errorf("the option --%s is given twice", key)
		}
		set[key], values[key] = true, value
	}
	return values, set, rest, nil
}

// premergeRunWith is crw manage premerge for a configuration already read.
func premergeRunWith(ctx context.Context, e *Env, cfg *Config, args []string) int {
	if len(args) == 0 {
		return premergeUsageError(e, "", errors.New("the following arguments are required: eval or dispose"))
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(e.Stdout, premergeUsage)
		return 0
	case "eval", "dispose":
		if premergeHelpRequested(args) {
			fmt.Fprintln(e.Stdout, premergeUsage)
			return 0
		}
	}
	switch args[0] {
	case "eval":
		values, set, rest, err := premergeOptions(args[1:], []string{"head", "node", "inputs"}, []string{"replace"})
		if err != nil {
			return premergeUsageError(e, " eval", err)
		}
		if len(rest) != 1 {
			return premergeUsageError(e, " eval", errors.New("name one pull request number"))
		}
		number, convErr := strconv.Atoi(rest[0])
		if convErr != nil || number <= 0 {
			return premergeUsageError(e, " eval", fmt.Errorf("%q is not a pull request number", rest[0]))
		}
		result, err := PremergeEval(ctx, e, cfg, PremergeEvalOptions{PR: number, Head: values["head"], Node: values["node"], Inputs: values["inputs"], Replace: set["replace"]})
		return premergeFinish(e, " eval", result, err)
	case "dispose":
		values, _, rest, err := premergeOptions(args[1:], []string{"ref", "class", "note", "follow-up", "by"}, nil)
		if err != nil {
			return premergeUsageError(e, " dispose", err)
		}
		if len(rest) != 1 {
			return premergeUsageError(e, " dispose", errors.New("name one record"))
		}
		for _, required := range []string{"ref", "class", "note"} {
			if values[required] == "" {
				return premergeUsageError(e, " dispose", fmt.Errorf("the option --%s is required", required))
			}
		}
		if values["follow-up"] != "" && !premergeFollowUp.MatchString(values["follow-up"]) {
			return premergeUsageError(e, " dispose", fmt.Errorf("--follow-up %q is not an issue key like CRW-123", values["follow-up"]))
		}
		result, err := PremergeDispose(ctx, e, cfg, PremergeDisposeOptions{Record: rest[0], Ref: values["ref"], Class: values["class"], Note: values["note"], FollowUp: values["follow-up"], By: values["by"]})
		return premergeFinish(e, " dispose", result, err)
	}
	return premergeUsageError(e, "", fmt.Errorf("invalid command %q (choose from 'eval', 'dispose')", args[0]))
}

// premergeFinish prints the one JSON document of a success, or the single stderr line of a refusal, and
// returns the exit status. A document that cannot be written is exit 1.
func premergeFinish(e *Env, sub string, result any, err error) int {
	if err != nil {
		var refusal *PremergeError
		code := 1
		if errors.As(err, &refusal) {
			code = refusal.Exit
		}
		fmt.Fprintf(e.Stderr, "crw manage premerge%s: error: %s\n", sub, strings.Join(strings.Fields(err.Error()), " "))
		return code
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr == nil {
		var written int
		written, marshalErr = e.Stdout.Write(append(data, '\n'))
		if marshalErr == nil && written != len(data)+1 {
			marshalErr = io.ErrShortWrite
		}
	}
	if marshalErr != nil {
		fmt.Fprintf(e.Stderr, "crw manage premerge%s: error: the output could not be written: %v\n", sub, marshalErr)
		return 1
	}
	return 0
}
