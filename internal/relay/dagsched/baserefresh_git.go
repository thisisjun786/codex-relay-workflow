package dagsched

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// The proof of a base refresh (CRW-430): in a local checkout, whether a head differs from an accepted head only by merges of the base branch. The relay answers it from git objects alone, which are
// content addressed, so any clone that holds the commits gives the same answer; nothing is written to the checkout (every git command that computes runs in a throwaway bare repository that borrows the
// checkout's objects and has no configuration, hooks, attributes, refs or replace objects of its own).

// The closed codes a refused proof carries in the detail of its disposition_conflict.
const (
	RefreshNoUpdate           = "no_update"             // the head is the accepted head: nothing was refreshed
	RefreshNotBuiltOnAccepted = "not_built_on_accepted" // the first-parent history of the head does not lead to the accepted head
	RefreshNotAMerge          = "not_a_merge"           // a commit between the heads is not a merge of exactly two parents: it is work of the child's own
	RefreshNotFromBase        = "not_from_base"         // a merged commit is not on the first-parent line of the base tip
	RefreshTreeDiffers        = "tree_differs"          // a merge holds content that git does not merge from its parents, outside the files git could not merge
	RefreshChainTooLong       = "chain_too_long"        // more merges than MaxRefreshHops lie between the heads
)

// MaxRefreshHops bounds the merges between the accepted head and the refreshed head; MaxBaseLine bounds the first-parent line of the base tip that is read to place a merged commit on it.
const (
	MaxRefreshHops = 32
	MaxBaseLine    = 200000
)

// RefreshResolved is a path one hop left to settle and the blob the head holds for it (empty when the resolution deleted the file): a file git could not merge, or the plugin manifest the head recorded
// again after a clean merge (CRW-732). What a hand put there is the one thing the relay cannot prove; Rule is the proved mechanical rule, or empty when the parent must name and review the file.
type RefreshResolved struct {
	Path, Blob, Rule string
	// Conflicted is whether git could not merge the path, so two sides edited it. A clean
	// difference the head produced by regenerating (CRW-898, item 9) has no second edit to
	// attribute and needs no contributor agreement; a conflict does.
	Conflicted bool
}

// RefreshMechanicalChecker evaluates selected conflict paths in one already proved merge.
// The skill package registers its existing checker at initialization to avoid an import cycle.
// A missing checker leaves every path manual; a refusal never becomes a manual override.
type RefreshMechanicalChecker func(context.Context, string, RefreshStep, []Region, map[string]string) (*RefreshMechanicalRefusal, error)

// RefreshDecision is the relay's own decision for one path of a step: the rule it selected for the
// place, or empty when it selected none and the path is the plugin manifest, which the checker
// settles by its built-in rule (CRW-898, item 6). The checker never re-derives the decision from
// the candidate's declaration alone: the relay has already weighed the candidate and every
// contributing node, and a candidate-only declaration must not block the built-in rule.
const RefreshDecisionBuiltin = ""

type RefreshMechanicalRefusal struct {
	Detail string
	// Manual is an internal eligibility result, never part of the wire proof.
	// Empty Detail leaves these paths to the existing exact-name acceptance.
	Manual []string
	// Rules is the rule the checker proved for a path it settled, by path (CRW-732). A path absent
	// from it and from Manual is settled by the rule the proof selected for it itself.
	Rules map[string]string
}

var refreshMechanical RefreshMechanicalChecker

// RegisterRefreshMechanical is initialization-only, like command registration.
func RegisterRefreshMechanical(check RefreshMechanicalChecker) {
	if check == nil || refreshMechanical != nil {
		panic("base refresh mechanical checker registered twice or nil")
	}
	refreshMechanical = check
}

// RefreshStep is one merge of the chain: the previous head (first parent), the commit of the base that was merged (second parent), the commit and its tree, and the paths that needed a hand resolution.
type RefreshStep struct {
	Previous, BaseParent, Head, Tree string
	Resolved                         []RefreshResolved
}

// versionOnly is whether the step is the version-only re-record a chain accepts (CRW-808) rather than a
// merge: it has no base parent, because nothing was merged, and its resolution is the built-in
// plugin-version rule the proof already proved from the commit's own payload.
func (s RefreshStep) versionOnly() bool { return s.BaseParent == "" }

// refreshRefusal is an answer of the proof and not a failure: the head is something other than the accepted head plus merges of the base.
type refreshRefusal struct{ Code, Detail string }

// refreshProof is a passed proof: the merges from the accepted head to the refreshed head, oldest first.
type refreshProof struct{ Steps []RefreshStep }

// resolvedPaths is every path requiring a manual name in any hop, sorted without repeats.
func (p refreshProof) resolvedPaths() []string {
	seen := map[string]bool{}
	for _, s := range p.Steps {
		for _, r := range s.Resolved {
			if r.Rule == "" {
				seen[r.Path] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (p *refreshProof) applyMechanical(ctx context.Context, g *refreshRepo, checkout string, regions []Region, contributors map[string][][]Region) (*refreshRefusal, error) {
	if refreshMechanical == nil {
		return nil, nil
	}
	for i := range p.Steps {
		st := &p.Steps[i]
		if st.versionOnly() {
			// A version-only step merged nothing, so there is no base parent to read contributor
			// regions from: the proof already settled its one path by the built-in rule.
			continue
		}
		sets, err := g.contributingRegions(ctx, *st, contributors)
		if err != nil {
			return nil, err
		}
		var paths, descriptions []string
		rules := map[string]string{}
		decided := map[string]string{}
		// A clean difference under a regenerate declaration that the declarations do not agree on is
		// refused rather than left to a --resolved name: git merged the path, so no hand resolution can
		// exist there, and admitting it unproved would accept content beyond git's merge of the accepted
		// candidate and the base (CRW-898, item 9).
		var cleanUnproved []string
		for _, r := range st.Resolved {
			// A conflict needs every contributing node to agree, because two sides edited the place. A
			// clean difference the head produced by regenerating (CRW-898, item 9) has no such second
			// edit to attribute, so the agreement the declarations have to reach is the rule itself:
			// the candidate"s declaration and every identified contributor"s must name the same
			// regenerate command, which is what MechanicalRuleFor answers over all of them. A
			// declaration only the candidate makes, or one the contributors disagree with, is not an
			// agreement and leaves the path refused as before. Everything else keeps the contributor
			// requirement.
			// The manifest is never admitted by the clean-regeneration extension below: it has its own
			// branch, because a declaration the contributors do not agree with must fall to the
			// built-in rule rather than being taken as an agreement of one (CRW-898, item 6).
			admitted := len(sets[r.Path]) > 0 || (!r.Conflicted && r.Path != pluginversion.ManifestRepoPath && agreedRegenerate(append([][]Region{regions}, sets[r.Path]...), r.Path))
			if rule, ok := MechanicalRuleFor(append([][]Region{regions}, sets[r.Path]...), r.Path); ok && admitted {
				paths = append(paths, r.Path)
				descriptions = append(descriptions, fmt.Sprintf("%s (%s)", r.Path, rule))
				rules[r.Path] = rule
				decided[r.Path] = rule
				continue
			}
			if !r.Conflicted && r.Path != pluginversion.ManifestRepoPath {
				cleanUnproved = append(cleanUnproved, r.Path)
			}
			// The plugin manifest's version line is the one place no declaration has to settle with one
			// agreed rule: the line is derived from the payload, and the checker applies its built-in
			// rule to it whether or not a declaration covers it (CRW-732). Every other unproved path
			// stays manual.
			if r.Path == pluginversion.ManifestRepoPath {
				paths = append(paths, r.Path)
				descriptions = append(descriptions, r.Path)
				// The manifest is the one place no declaration has to settle with one agreed rule.
				// A rule the candidate declares still has its say (CRW-732): an unprovable declared
				// rule leaves the path to the parent"s --resolved name rather than being stamped
				// with the built-in rule. What must not survive is a disagreement: when an
				// identified contributor declares a different rule for the path, no rule is agreed
				// across the declarations and the built-in rule is handed over instead (CRW-898,
				// item 6).
				decided[r.Path] = RefreshDecisionBuiltin
				decided[r.Path] = manifestRefreshDecision(regions, sets[r.Path])
			}
		}
		if len(paths) == 0 {
			if len(cleanUnproved) == 0 {
				continue
			}
			return &refreshRefusal{Code: RefreshTreeDiffers, Detail: fmt.Sprintf("the head differs from git's merge of %s and %s in %s, and the declarations given do not agree on one regenerate rule for it, so it is not a regeneration of the base", st.Previous, st.BaseParent, refreshPathsText(cleanUnproved))}, nil
		}
		if len(cleanUnproved) > 0 {
			return &refreshRefusal{Code: RefreshTreeDiffers, Detail: fmt.Sprintf("the head differs from git's merge of %s and %s in %s, and the declarations given do not agree on one regenerate rule for it, so it is not a regeneration of the base", st.Previous, st.BaseParent, refreshPathsText(cleanUnproved))}, nil
		}
		why, err := refreshMechanical(ctx, checkout, *st, regions, decided)
		if err != nil {
			return nil, fmt.Errorf("mechanical resolution of %s: %w", strings.Join(descriptions, ", "), err)
		}
		if why != nil && why.Detail != "" {
			return &refreshRefusal{Code: RefreshTreeDiffers, Detail: fmt.Sprintf("mechanical resolution of %s: %s", strings.Join(descriptions, ", "), why.Detail)}, nil
		}
		if why != nil {
			for _, path := range why.Manual {
				delete(rules, path)
			}
			// Only the checker's own answer settles a path: it decides which rule proved what, and a
			// path it left out of Rules keeps the empty rule that sends it to the parent's --resolved
			// name. Assuming the built-in rule here would hand a manifest the checker refused a
			// mechanical mark it never proved.
			for path, rule := range why.Rules {
				rules[path] = rule
			}
		}
		for j := range st.Resolved {
			st.Resolved[j].Rule = rules[st.Resolved[j].Path]
		}
	}
	return nil, nil
}

// contributingRegions reads every new first-parent landing, without Git's path
// history simplification. An unmapped path delta vetoes automatic classification
// even when other landings for that path map to known nodes.
func (g *refreshRepo) contributingRegions(ctx context.Context, st RefreshStep, heads map[string][][]Region) (map[string][][]Region, error) {
	_, line, err := g.run(ctx, nil, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", MaxBaseLine+1), st.BaseParent, "^"+st.Previous)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(line)
	sets := map[string][][]Region{}
	if len(ids) > MaxBaseLine {
		return sets, nil
	}
	unknown := map[string]bool{}
	for _, id := range ids {
		parents, err := g.parents(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(parents) == 0 {
			return map[string][][]Region{}, nil
		}
		paths, err := g.differing(ctx, parents[0], id)
		if err != nil {
			return nil, err
		}
		carrier := ""
		switch len(parents) {
		case 1:
			carrier = id
		case 2:
			carrier = parents[1]
		}
		for _, path := range paths {
			if len(heads[carrier]) == 0 {
				unknown[path] = true
			}
			sets[path] = append(sets[path], heads[carrier]...)
		}
	}
	for path := range unknown {
		delete(sets, path)
	}
	return sets, nil
}

var refreshCommitPattern = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

// refreshRepo is a throwaway bare repository that reads a checkout's objects through an alternates file.
type refreshRepo struct {
	dir, gitdir string
	env         []string
}

func refreshGitEnv(extra ...string) []string {
	return append(append(cleanGitEnv(), "GIT_NO_REPLACE_OBJECTS=1"), extra...)
}

// refreshGit runs git and returns the exit code, the standard output and, for a failure, git's own words. A non-zero exit is an answer for the callers that expect one.
func refreshGit(ctx context.Context, env []string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.String(), fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), exit.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return -1, "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

func openRefreshRepo(ctx context.Context, checkout string) (*refreshRepo, error) {
	env := refreshGitEnv()
	_, objects, err := refreshGit(ctx, env, "-C", checkout, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	_, format, err := refreshGit(ctx, env, "-C", checkout, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	if format = strings.TrimSpace(format); format != "sha1" && format != "sha256" {
		return nil, fmt.Errorf("git answered %q to --show-object-format, which is not an object format this proof knows", firstLine(format))
	}
	dir, err := os.MkdirTemp("", "dag-base-refresh-")
	if err != nil {
		return nil, err
	}
	g := &refreshRepo{dir: dir, gitdir: filepath.Join(dir, "g.git")}
	g.env = refreshGitEnv("GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TEMPLATE_DIR=", "HOME="+dir, "XDG_CONFIG_HOME="+dir)
	if _, _, err := refreshGit(ctx, g.env, "init", "--bare", "-q", "--object-format="+format, g.gitdir); err != nil {
		g.close()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(g.gitdir, "objects", "info"), 0o700); err != nil {
		g.close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(g.gitdir, "objects", "info", "alternates"), []byte(strings.TrimSpace(objects)+"\n"), 0o600); err != nil {
		g.close()
		return nil, err
	}
	return g, nil
}

func (g *refreshRepo) close() { _ = os.RemoveAll(g.dir) }

func (g *refreshRepo) run(ctx context.Context, extraEnv []string, args ...string) (int, string, error) {
	return refreshGit(ctx, append(append([]string{}, g.env...), extraEnv...), append([]string{"--git-dir=" + g.gitdir}, args...)...)
}

// hasCommit is whether the checkout holds the commit.
func (g *refreshRepo) hasCommit(ctx context.Context, id string) bool {
	code, _, _ := g.run(ctx, nil, "cat-file", "-e", id+"^{commit}")
	return code == 0
}

func (g *refreshRepo) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	code, _, err := g.run(ctx, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

func (g *refreshRepo) parents(ctx context.Context, commit string) ([]string, error) {
	_, out, err := g.run(ctx, nil, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != commit {
		return nil, fmt.Errorf("git rev-list answered %q for %s", firstLine(out), commit)
	}
	return fields[1:], nil
}

// firstParentLine is the commits on the first-parent history of a tip: the base branch's own line (the merges of its pull requests and its direct commits), which a merge of the base takes its second parent
// from. A commit that only lies in the ancestry of the tip, on a branch the base merged, is not on it.
func (g *refreshRepo) firstParentLine(ctx context.Context, tip string) (map[string]bool, error) {
	_, out, err := g.run(ctx, nil, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", MaxBaseLine), tip)
	if err != nil {
		return nil, err
	}
	line := map[string]bool{}
	for _, id := range strings.Fields(out) {
		line[id] = true
	}
	return line, nil
}

func (g *refreshRepo) treeOf(ctx context.Context, commit string) (string, error) {
	_, out, err := g.run(ctx, nil, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if !refreshCommitPattern.MatchString(id) {
		return "", fmt.Errorf("git answered %q for the tree of %s", firstLine(out), commit)
	}
	return id, nil
}

// mergeTree merges two commits in memory: the tree git writes and the sorted names of the files it cannot merge (none for a clean merge). The attributes are the ones committed in the first commit, never a
// working tree's. Anything but a clean merge or a conflict is a failure to compute it.
func (g *refreshRepo) mergeTree(ctx context.Context, first, second string) (string, []string, error) {
	code, out, err := g.run(ctx, []string{"GIT_ATTR_SOURCE=" + first}, "merge-tree", "-z", "--write-tree", "--name-only", "--no-messages", first, second)
	if code != 0 && code != 1 {
		return "", nil, fmt.Errorf("git merge-tree could not merge %s and %s (git 2.40 or newer is needed): %w", first, second, err)
	}
	records := strings.Split(out, "\x00")
	if len(records) == 0 || !refreshCommitPattern.MatchString(records[0]) {
		return "", nil, fmt.Errorf("git merge-tree answered %q for %s and %s, which is not a merge result", firstLine(out), first, second)
	}
	if code == 0 {
		return records[0], nil, nil
	}
	seen := map[string]bool{}
	for _, name := range records[1:] {
		if name == "" {
			break
		}
		seen[name] = true
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return "", nil, fmt.Errorf("git merge-tree reported a conflict for %s and %s and named no file", first, second)
	}
	return records[0], files, nil
}

// differing is the sorted paths in which two trees differ.
func (g *refreshRepo) differing(ctx context.Context, a, b string) ([]string, error) {
	_, out, err := g.run(ctx, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", a, b)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(out, "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// blobAt is the blob id a commit holds for a path, empty when the commit has no such file.
func (g *refreshRepo) blobAt(ctx context.Context, commit, path string) (string, error) {
	entry, err := g.entryAt(ctx, commit, path)
	if err != nil {
		return "", err
	}
	return entry.oid, nil
}

// repoEntry is one path of a commit: its mode, kind and blob id.
type repoEntry struct{ mode, kind, oid string }

// blob is the bytes of a blob the repository holds.
func (g *refreshRepo) blob(ctx context.Context, oid string) (string, error) {
	_, out, err := g.run(ctx, nil, "cat-file", "blob", oid)
	return out, err
}

// runner is the proof's own git view as a pluginversion.GitRunner: the reads of a version go through
// the same throwaway repository the rest of the proof reads, so a replace ref or an inherited GIT_*
// variable cannot make the payload a different tree's.
func (g *refreshRepo) runner() pluginversion.GitRunner {
	return func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + g.gitdir}, args...)...)
		cmd.Env = g.env
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return nil, fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), exit.ExitCode(), strings.TrimSpace(stderr.String()))
			}
			return nil, err
		}
		return stdout.Bytes(), nil
	}
}

// versionOnlyResolution decides whether a single-parent commit C is the version-only re-record a chain
// accepts (CRW-808). It answers the rule and true when all of these hold, and false with the reason
// otherwise; an empty reason means C changes something other than the manifest, which is work of the
// child's own and keeps not_a_merge. The caller has already proved that parent is a merge in this
// chain.
//
//   - the only path C changes against parent is the plugin manifest, with the same file mode;
//   - the manifest equals parent's byte for byte apart from the version line, with the same release;
//   - the version is the one C's own payload derives.
func (g *refreshRepo) versionOnlyResolution(ctx context.Context, parent, commit string) (rule string, settled bool, why string, err error) {
	differing, err := g.differing(ctx, parent, commit)
	if err != nil {
		return "", false, "", err
	}
	if len(differing) != 1 || differing[0] != pluginversion.ManifestRepoPath {
		return "", false, "", nil
	}
	before, err := g.entryAt(ctx, parent, pluginversion.ManifestRepoPath)
	if err != nil {
		return "", false, "", err
	}
	after, err := g.entryAt(ctx, commit, pluginversion.ManifestRepoPath)
	if err != nil {
		return "", false, "", err
	}
	if before.kind != "blob" || after.kind != "blob" || before.mode != after.mode || (after.mode != "100644" && after.mode != "100755") {
		return "", false, fmt.Sprintf("it is not the same regular file of one mode in both commits (%s and %s)", before.mode, after.mode), nil
	}
	parentContent, err := g.blob(ctx, before.oid)
	if err != nil {
		return "", false, "", err
	}
	commitContent, err := g.blob(ctx, after.oid)
	if err != nil {
		return "", false, "", err
	}
	elidedParent, parentVersion, err := pluginversion.ManifestVersionElided([]byte(parentContent))
	if err != nil {
		return "", false, "its parent's manifest cannot be read apart from its version: " + err.Error(), nil
	}
	elidedCommit, commitVersion, err := pluginversion.ManifestVersionElided([]byte(commitContent))
	if err != nil {
		return "", false, "its manifest cannot be read apart from its version: " + err.Error(), nil
	}
	if !bytes.Equal(elidedParent, elidedCommit) {
		return "", false, "its manifest differs from its parent's outside the version line", nil
	}
	parentRelease, _ := pluginversion.SplitVersion(parentVersion)
	commitRelease, _ := pluginversion.SplitVersion(commitVersion)
	if parentRelease != commitRelease {
		return "", false, fmt.Sprintf("its version %q does not keep the release %q its parent records", commitVersion, parentRelease), nil
	}
	want, reason, err := pluginversion.TreeVersion(ctx, g.runner(), commit)
	if err != nil {
		return "", false, "", err
	}
	if reason != "" {
		return "", false, "the payload of " + commit + " cannot name a version: " + reason, nil
	}
	if commitVersion != want {
		return "", false, fmt.Sprintf("its version %q is not the %q its own payload derives", commitVersion, want), nil
	}
	return BuiltinPluginVersionRule, true, "", nil
}

// entryAt is the entry a commit holds for a path; a commit without that path answers an empty entry.
func (g *refreshRepo) entryAt(ctx context.Context, commit, path string) (repoEntry, error) {
	_, out, err := g.run(ctx, nil, "ls-tree", "-z", commit, "--", path)
	if err != nil {
		return repoEntry{}, err
	}
	entry, _, _ := strings.Cut(out, "\x00")
	meta, name, ok := strings.Cut(entry, "\t")
	if !ok || name != path {
		return repoEntry{}, nil
	}
	if fields := strings.Fields(meta); len(fields) == 3 {
		return repoEntry{mode: fields[0], kind: fields[1], oid: fields[2]}, nil
	}
	return repoEntry{}, nil
}

// scanConflictMarkers reads a file and says whether it holds the start line and the end line of a conflict: a line that begins with width or more of the same marker character (< or >) and then ends or goes
// on with a space. The width is the conflict-marker-size attribute's, seven by default (markerWidth). It reads byte by byte through a bounded buffer, so a file or a line of any length is read to its end or to
// the second marker, and a run of marker characters that goes on past a buffer is judged by what follows it.
func scanConflictMarkers(r io.Reader, width int) (started, ended bool) {
	reader := bufio.NewReaderSize(r, 64<<10)
	atStart, inRun := true, false
	var c byte
	run := 0
	mark := func() {
		started = started || c == '<'
		ended = ended || c == '>'
	}
	for !(started && ended) {
		b, err := reader.ReadByte()
		if err != nil {
			if inRun && run >= width {
				mark()
			}
			break
		}
		switch {
		case b == '\n' || b == '\r':
			if inRun && run >= width {
				mark()
			}
			atStart, inRun, run = b == '\n', false, 0
		case atStart:
			atStart = false
			if b == '<' || b == '>' {
				c, inRun, run = b, true, 1
			}
		case inRun && b == c:
			run++
		case inRun:
			if run >= width && b == ' ' {
				mark()
			}
			inRun = false
		}
	}
	return started, ended
}

// hasConflictMarkers is whether a resolved file still holds a conflict's start and end line (git's own labels differ from the working tree's, so the tree comparison alone cannot tell a file that was
// committed with its markers from one that was edited). The blob is streamed and the read stops at the second marker.
func (g *refreshRepo) hasConflictMarkers(ctx context.Context, blob string, width int) (bool, error) {
	if blob == "" {
		return false, nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", "--git-dir="+g.gitdir, "cat-file", "blob", blob)
	cmd.Env = g.env
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return false, err
	}
	started, ended := scanConflictMarkers(out, width)
	if started && ended {
		cancel()
		_ = cmd.Wait()
		return true, nil
	}
	if err := cmd.Wait(); err != nil {
		return false, fmt.Errorf("git cat-file blob %s: %w: %s", blob, err, strings.TrimSpace(stderr.String()))
	}
	return false, nil
}

// refreshPathsText names up to ten paths for a refusal.
func refreshPathsText(paths []string) string {
	if len(paths) > 10 {
		return strings.Join(paths[:10], ", ") + fmt.Sprintf(" and %d more", len(paths)-10)
	}
	return strings.Join(paths, ", ")
}

// declaresRegenerate is whether the candidate's own declaration covers a path with a
// mechanical regenerate rule: a whole-file mechanical region naming regenerate:<command>. It admits
// the path as one a rule may settle; whether every contributing node agrees is decided later, by
// MechanicalRuleFor over every declaration given (CRW-898, item 9).
func declaresRegenerate(declared []Region, path string) bool {
	rule, ok := MechanicalRuleFor([][]Region{declared}, path)
	return ok && strings.HasPrefix(rule, RuleRegeneratePref)
}

// agreedRegenerate is whether every declaration given settles a path by one agreed mechanical
// regenerate rule (CRW-898, item 9). It is declaresRegenerate over all the declarations together,
// so a candidate-only declaration with no identified contributor is an agreement of one, while a
// declaration a contributor disagrees with is not an agreement at all.
func agreedRegenerate(sets [][]Region, path string) bool {
	rule, ok := MechanicalRuleFor(sets, path)
	return ok && strings.HasPrefix(rule, RuleRegeneratePref)
}

// manifestRefreshDecision is the relay's decision for the plugin manifest, the one path where a
// declaration does not have to settle the place on its own (CRW-732). A rule the candidate declares
// is used only when the declarations settle the path by one rule: the candidate alone when no
// identified contributor touches it, or the candidate and every contributor together. When a
// contributor touches the path with a different rule - or with none - no rule is agreed, and the
// built-in rule is handed to the checker instead, so a candidate-only declaration cannot block the
// built-in proof (CRW-898, item 6).
func manifestRefreshDecision(candidate []Region, contributors [][]Region) string {
	sets := [][]Region{candidate}
	if len(contributors) > 0 {
		sets = append(sets, contributors...)
	}
	if rule, ok := MechanicalRuleFor(sets, pluginversion.ManifestRepoPath); ok {
		return rule
	}
	return RefreshDecisionBuiltin
}

// proveBaseRefresh walks the first parents from head back to accepted. Every commit on the way must be a merge of exactly two parents, the previous commit first and a commit on the first-parent line of baseTip
// second, and must hold the tree git merges from those two parents. A merge git cannot do cleanly is accepted only when its tree differs from the tree git writes in the files git could not merge and nowhere
// else; those files are returned as resolved by hand. A refusal is an answer; an error means git could not answer (a commit the checkout does not hold is the caller's to name before this).
func proveBaseRefresh(ctx context.Context, g *refreshRepo, accepted, head, baseTip string, declared []Region) (*refreshProof, *refreshRefusal, error) {
	refuse := func(code, format string, args ...any) (*refreshProof, *refreshRefusal, error) {
		return nil, &refreshRefusal{Code: code, Detail: fmt.Sprintf(format, args...)}, nil
	}
	if head == accepted {
		return refuse(RefreshNoUpdate, "the head %s is the accepted head: nothing was refreshed", head)
	}
	built, err := g.isAncestor(ctx, accepted, head)
	if err != nil {
		return nil, nil, err
	}
	if !built {
		return refuse(RefreshNotBuiltOnAccepted, "the accepted head %s is not in the history of %s", accepted, head)
	}
	line, err := g.firstParentLine(ctx, baseTip)
	if err != nil {
		return nil, nil, err
	}
	var steps []RefreshStep
	merges := 0
	for cur := head; cur != accepted; {
		// The bound counts the merges between the heads, not the steps of the chain: a version-only
		// re-record is a step but not a merge, and it can only follow a merge this chain proved, so it
		// never carries the count past the number of merges (CRW-898, item 7).
		if merges >= MaxRefreshHops {
			return refuse(RefreshChainTooLong, "more than %d merges lie between the accepted head %s and %s", MaxRefreshHops, accepted, head)
		}
		parents, err := g.parents(ctx, cur)
		if err != nil {
			return nil, nil, err
		}
		if len(parents) == 1 {
			// A single-parent commit is the version-only re-record a chain accepts (CRW-808), and only
			// when its parent is a base merge this chain proved; the walk proves that parent next, so
			// the condition is checked after it (below). Everything else keeps not_a_merge.
			parent := parents[0]
			rule, settled, why, err := g.versionOnlyResolution(ctx, parent, cur)
			if err != nil {
				return nil, nil, err
			}
			if !settled {
				if why == "" {
					names, err := g.differing(ctx, parent, cur)
					if err != nil {
						return nil, nil, err
					}
					return refuse(RefreshNotAMerge, "%s has 1 parent(s) and changes %s: a single-parent commit is work of the child's own unless it only re-records the plugin manifest's version line", cur, refreshPathsText(names))
				}
				return refuse(RefreshTreeDiffers, "%s re-records %s but is not the version-only refresh this proof accepts: %s", cur, pluginversion.ManifestRepoPath, why)
			}
			headTree, err := g.treeOf(ctx, cur)
			if err != nil {
				return nil, nil, err
			}
			blob, err := g.blobAt(ctx, cur, pluginversion.ManifestRepoPath)
			if err != nil {
				return nil, nil, err
			}
			steps = append(steps, RefreshStep{Previous: parent, Head: cur, Tree: headTree,
				Resolved: []RefreshResolved{{Path: pluginversion.ManifestRepoPath, Blob: blob, Rule: rule}}})
			cur = parent
			continue
		}
		if len(parents) != 2 {
			return refuse(RefreshNotAMerge, "%s has %d parent(s): a refresh is a chain of merges of exactly two parents, and this commit is work of the child's own", cur, len(parents))
		}
		merges++
		previous, merged := parents[0], parents[1]
		if previous != accepted {
			under, err := g.isAncestor(ctx, accepted, previous)
			if err != nil {
				return nil, nil, err
			}
			if !under {
				return refuse(RefreshNotBuiltOnAccepted, "the first parent %s of %s does not contain the accepted head %s (parents swapped, or a branch that is not the accepted one was merged in)", previous, cur, accepted)
			}
		}
		if !line[merged] {
			return refuse(RefreshNotFromBase, "%s merges %s, which is not on the first-parent line of the base tip %s: a branch that is not the base, or a base that moved after the tip was read", cur, merged, baseTip)
		}
		tree, conflicts, err := g.mergeTree(ctx, previous, merged)
		if err != nil {
			return nil, nil, err
		}
		headTree, err := g.treeOf(ctx, cur)
		if err != nil {
			return nil, nil, err
		}
		step := RefreshStep{Previous: previous, BaseParent: merged, Head: cur, Tree: headTree}
		var outside, clean []string
		if headTree != tree {
			names, err := g.differing(ctx, tree, headTree)
			if err != nil {
				return nil, nil, err
			}
			conflicted := map[string]bool{}
			for _, c := range conflicts {
				conflicted[c] = true
			}
			for _, d := range names {
				if conflicted[d] {
					continue
				}
				// Two clean differences are not a hand resolution: the plugin manifest's version line,
				// which is derived from the payload and recomputed from the head itself (CRW-732), and a
				// path the candidate's own declaration covers with a mechanical regenerate rule
				// (CRW-898, item 9): the command rebuilds the file, and the mechanical step re-runs it and
				// compares. Every other clean difference keeps today's refusal.
				if d == pluginversion.ManifestRepoPath || declaresRegenerate(declared, d) {
					clean = append(clean, d)
					continue
				}
				outside = append(outside, d)
			}
		}
		if len(outside) > 0 {
			where := ""
			if len(conflicts) > 0 {
				where = " outside the files git could not merge"
			}
			return refuse(RefreshTreeDiffers, "the tree of %s is not what git merges from %s and %s; paths that differ%s: %s", cur, previous, merged, where, refreshPathsText(outside))
		}
		for _, c := range conflicts {
			blob, err := g.blobAt(ctx, cur, c)
			if err != nil {
				return nil, nil, err
			}
			// a file git could not merge that still holds the start and the end line of a conflict, at the width the attributes of the first parent give, was never resolved, whether or not it differs from
			// what git wrote; a conflict that has no markers (a binary file, a file under a merge driver) is resolved by keeping git's own side as well as by an edit
			width, err := g.markerWidth(ctx, previous, c)
			if err != nil {
				return nil, nil, err
			}
			if marked, err := g.hasConflictMarkers(ctx, blob, width); err != nil {
				return nil, nil, err
			} else if marked {
				return refuse(RefreshTreeDiffers, "%s commits %s with conflict markers in it, which no one resolved", cur, c)
			}
			step.Resolved = append(step.Resolved, RefreshResolved{Path: c, Blob: blob, Conflicted: true})
		}
		// The manifest the head recorded again after a clean merge is settled like a conflicted one:
		// by the rule the mechanical step proves for it, or by the parent naming it.
		for _, c := range clean {
			blob, err := g.blobAt(ctx, cur, c)
			if err != nil {
				return nil, nil, err
			}
			step.Resolved = append(step.Resolved, RefreshResolved{Path: c, Blob: blob})
		}
		steps = append(steps, step)
		cur = previous
	}
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	// A version-only step is accepted only on a base merge this chain proved: the chain's first commit
	// and a version-only commit after another single-parent commit are refused (CRW-808).
	for i := range steps {
		if steps[i].BaseParent != "" {
			continue
		}
		if i == 0 || steps[i-1].BaseParent == "" || steps[i-1].Head != steps[i].Previous {
			return refuse(RefreshNotAMerge, "%s re-records only %s but sits on %s, which this chain did not prove as a base merge: a version-only commit is accepted only on top of a merge the chain already proved", steps[i].Head, pluginversion.ManifestRepoPath, steps[i].Previous)
		}
	}
	return &refreshProof{Steps: steps}, nil, nil
}

// markerWidth is the width of the conflict markers git writes for a path: the conflict-marker-size attribute committed in the commit the merge takes its attributes from, seven when there is none or it is
// not a positive number.
func (g *refreshRepo) markerWidth(ctx context.Context, commit, path string) (int, error) {
	_, out, err := g.run(ctx, nil, "check-attr", "--source", commit, "conflict-marker-size", "--", path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(out[strings.LastIndex(out, ": ")+1:])
	var width int
	if _, err := fmt.Sscanf(value, "%d", &width); err != nil || width < 1 {
		return 7, nil
	}
	return width, nil
}
