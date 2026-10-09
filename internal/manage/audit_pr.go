package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The names and the fixed values of the pull request mode. The bundle directory is the
// ledger row's subject, so a later run tells a pull request already audited from one that
// is not by asking the ledger for that name.
const (
	auditPRBundlePrefix   = "pr-"
	auditPRDiffFile       = "diff.patch"
	auditPRTaskFile       = "task.md"
	auditPRCriteriaFile   = "criteria.json"
	auditPRFilesDir       = "files"
	auditPRListLimit      = "200"
	auditPRDefaultMax     = 9
	auditPRDefaultPattern = `[A-Z]+-\d+`
	auditPRPhaseLive      = "live"
	auditPRPairUnknown    = "unknown"
	auditPRRedacted       = "[redacted]"
	auditPRBaseRef        = "dev"
)

// auditPRSection is the part of the audit section the pull request mode reads. A map whose
// keys are timestamps or substrings is the shape the issue fixes for pairs and phases.
type auditPRSection struct {
	BundleDir    string            `json:"bundle_dir"`
	IssuePattern string            `json:"issue_pattern"`
	PRSince      string            `json:"pr_since"`
	Pairs        map[string]string `json:"pairs"`
	Phases       map[string]string `json:"phases"`
	Scrub        []string          `json:"scrub"`
}

// auditPRListEntry is one merged pull request as gh answers for it.
type auditPRListEntry struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	MergedAt    string `json:"mergedAt"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

// auditPRTarget is one pull request this run will audit: what it is, the key its title
// carried, the child that delivered it, and the metadata the ledger row needs.
type auditPRTarget struct {
	Number   int
	Issue    string
	Title    string
	Body     string
	Merge    string
	MergedAt time.Time
	Pair     string
	Phase    string
	Child    auditPRChild
}

// auditPRChild is what the relay knows about the child that delivered a pull request. An
// empty relationship is a pull request with no assignment, which still gets a bundle.
type auditPRChild struct {
	Relationship string
	Task         string
	Model        string
}

// auditPRSource is everything a bundle is assembled from: the target, the description its
// author wrote, the patch and the criteria.
type auditPRSource struct {
	Target                  auditPRTarget
	Patch                   []byte
	Criteria                []map[string]any
	CriteriaUnavailable     bool
	RelationshipUnavailable bool
	Relationship            string
}

// auditPRDryRun is one line of a dry run: the targets only, with the metadata the run
// would attach, and never a model name.
type auditPRDryRun struct {
	Subject string `json:"subject"`
	Issue   string `json:"issue"`
	Head    string `json:"head"`
	Pair    string `json:"pair"`
	Phase   string `json:"phase"`
}

// The seams a test replaces instead of running gh, reaching the relay or reading a real
// repository. The git reader is the package mode's own, because a pull request's files are
// read exactly the way a package's are: fetch, then `git show <commit>:<path>`.
var (
	auditPRGh    = auditPRGhRun
	auditPRRelay = auditPRRelayRun
)

// auditPRGhRun runs one gh command line.
func auditPRGhRun(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// auditPRRelayRun runs one relay command line of the same executable and refuses a non-zero
// status, because every read this mode makes has to answer for the run to mean anything.
func auditPRRelayRun(ctx context.Context, e *Env, cfg *Config, args ...string) ([]byte, error) {
	out, code, err := e.Relay(ctx, cfg, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("relay %s exited with status %d", strings.Join(args, " "), code)
	}
	return out, nil
}

// auditPRSectionOf reads the audit section's pull request keys. A configuration with no
// audit section leaves every value at its default.
func auditPRSectionOf(cfg *Config) (auditPRSection, error) {
	var section auditPRSection
	if cfg != nil {
		if err := cfg.Section("audit", &section); err != nil {
			return auditPRSection{}, err
		}
	}
	return section, nil
}

// auditPRBundleRoot is where a pull request bundle is written: the configured bundle_dir,
// else the bundles directory below the audit state directory.
func auditPRBundleRoot(e *Env, cfg *Config, section auditPRSection) string {
	if section.BundleDir != "" {
		return section.BundleDir
	}
	return crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", "bundles")
}

// auditPRSubject is the bundle directory name and the ledger row's subject: the pull
// request number, so the ledger answers whether a pull request was already audited.
func auditPRSubject(number int) string {
	return auditPRBundlePrefix + strconv.Itoa(number)
}

// auditPRPattern is the title pattern an issue key is read with: the configured one, else
// the default the issue fixes.
func auditPRPattern(section auditPRSection) (*regexp.Regexp, error) {
	pattern := section.IssuePattern
	if pattern == "" {
		pattern = auditPRDefaultPattern
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("the audit section issue_pattern %q: %w", pattern, err)
	}
	return compiled, nil
}

// auditPRList asks gh for the merged pull requests of the integration branch at or after
// since, in the one shape the issue fixes.
func auditPRList(ctx context.Context, e *Env, cfg *Config, since time.Time) ([]auditPRListEntry, error) {
	args := []string{"pr", "list"}
	if cfg != nil && cfg.Repository != "" {
		args = append(args, "--repo", cfg.Repository)
	}
	args = append(args, "--state", "merged", "--base", auditPRBaseRef,
		"--search", "merged:>="+since.UTC().Format(time.RFC3339),
		"--json", "number,title,body,mergeCommit,mergedAt", "--limit", auditPRListLimit)
	out, err := auditPRGh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var entries []auditPRListEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("the gh pull request list: %w", err)
	}
	return entries, nil
}

// auditPRDiff is one pull request's patch.
func auditPRDiff(ctx context.Context, cfg *Config, number int) ([]byte, error) {
	args := []string{"pr", "diff", strconv.Itoa(number)}
	if cfg != nil && cfg.Repository != "" {
		args = append(args, "--repo", cfg.Repository)
	}
	return auditPRGh(ctx, args...)
}

// auditPRSelect is the target list: the merged pull requests whose title carries an issue
// key, that merged at or after since, and that the ledger does not already hold as a pull
// request audit. They come back newest first, which is the order a bounded run wants, and
// the list is cut to max.
//
// A merge time gh did not answer with as a timestamp is an error rather than a skip: gh
// filtered the list by the same field, so a value that does not parse means the answer is
// not the one that was asked for, and skipping it would silently drop a pull request from
// the audit.
func auditPRSelect(entries []auditPRListEntry, pattern *regexp.Regexp, since time.Time, audited map[string]bool, max int) ([]auditPRTarget, error) {
	var targets []auditPRTarget
	for _, entry := range entries {
		key := pattern.FindString(entry.Title)
		if key == "" {
			continue
		}
		mergedAt, err := time.Parse(time.RFC3339, entry.MergedAt)
		if err != nil {
			return nil, fmt.Errorf("the merged time of pull request #%d is %q: %w", entry.Number, entry.MergedAt, err)
		}
		if mergedAt.Before(since) {
			continue
		}
		if entry.MergeCommit.OID == "" || audited[auditPRSubject(entry.Number)] {
			continue
		}
		targets = append(targets, auditPRTarget{
			Number: entry.Number, Issue: key, Title: entry.Title, Body: entry.Body,
			Merge: entry.MergeCommit.OID, MergedAt: mergedAt,
		})
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if !targets[i].MergedAt.Equal(targets[j].MergedAt) {
			return targets[i].MergedAt.After(targets[j].MergedAt)
		}
		return targets[i].Number > targets[j].Number
	})
	if max >= 0 && len(targets) > max {
		targets = targets[:max]
	}
	return targets, nil
}

// auditPRAudited is the bundle subjects the ledger already holds as pull request audits,
// so a pull request is audited once.
func auditPRAudited(rows []auditLedgerRow) map[string]bool {
	audited := map[string]bool{}
	for _, row := range rows {
		if row.Mode == auditModePR {
			audited[row.Subject] = true
		}
	}
	return audited
}

// auditPRChildOf asks the relay which child delivered an issue's pull request. An issue with
// no assignment answers with an empty relationship rather than an error, because the issue
// fixes that such a pull request still gets a bundle; a relay failure is an error.
func auditPRChildOf(ctx context.Context, e *Env, cfg *Config, issue string) (auditPRChild, error) {
	out, err := auditPRRelay(ctx, e, cfg, "assignment-find", "--issue", issue)
	if err != nil {
		return auditPRChild{}, err
	}
	var answer struct {
		ResponsibleChild        string `json:"responsibleChild"`
		ResponsibleRelationship string `json:"responsibleRelationship"`
		Assignments             []struct {
			RelationshipID string `json:"relationshipId"`
			ChildTaskID    string `json:"childTaskId"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		return auditPRChild{}, fmt.Errorf("the assignment-find answer: %w", err)
	}
	child := auditPRChild{Relationship: answer.ResponsibleRelationship, Task: answer.ResponsibleChild}
	if child.Relationship == "" {
		// No responsible owner is a closed assignment, so the newest one that names a
		// relationship is the child that delivered this pull request.
		for i := len(answer.Assignments) - 1; i >= 0; i-- {
			if answer.Assignments[i].RelationshipID != "" {
				child.Relationship = answer.Assignments[i].RelationshipID
				child.Task = answer.Assignments[i].ChildTaskID
				break
			}
		}
	}
	if child.Task == "" {
		return child, nil
	}
	model, err := auditPRModel(ctx, e, cfg, child.Task)
	if err != nil {
		return auditPRChild{}, err
	}
	child.Model = model
	return child, nil
}

// auditPRModel is the model the relay recorded for a child task. A child whose settings
// were never recorded has no model, which leaves the pair unknown rather than failing.
func auditPRModel(ctx context.Context, e *Env, cfg *Config, task string) (string, error) {
	out, err := auditPRRelay(ctx, e, cfg, "settings-show", "--task", task)
	if err != nil {
		return "", err
	}
	var answer struct {
		Settings *struct {
			Model string `json:"model"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		return "", fmt.Errorf("the settings-show answer: %w", err)
	}
	if answer.Settings == nil {
		return "", nil
	}
	return answer.Settings.Model, nil
}

// auditPRPairOf names the pair a model belongs to: the longest configured key the model
// contains, ties broken by sorted key so the answer never depends on map order. A model no
// key names, and a pull request with no child, are unknown.
func auditPRPairOf(pairs map[string]string, model string) string {
	if model == "" {
		return auditPRPairUnknown
	}
	lowered := strings.ToLower(model)
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	best, bestLen := "", -1
	for _, key := range keys {
		name := pairs[key]
		if key == "" || name == "" || len(key) <= bestLen || !strings.Contains(lowered, strings.ToLower(key)) {
			continue
		}
		best, bestLen = name, len(key)
	}
	if best == "" {
		return auditPRPairUnknown
	}
	return best
}

// auditPRPhaseOf names the phase a merge time falls in: the configured start at or before
// it that is the greatest, else live. A start that is not a timestamp, or a phase name that
// is empty, is refused rather than silently attributed.
func auditPRPhaseOf(section auditPRSection, merged time.Time) (string, error) {
	starts := make([]string, 0, len(section.Phases))
	for start := range section.Phases {
		starts = append(starts, start)
	}
	sort.Strings(starts)
	best, bestAt := "", time.Time{}
	for _, start := range starts {
		at, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return "", fmt.Errorf("the audit section phase start %q: %w", start, err)
		}
		name := section.Phases[start]
		if name == "" {
			return "", fmt.Errorf("the audit section phase start %q names no phase", start)
		}
		if at.After(merged) {
			continue
		}
		if best == "" || at.After(bestAt) {
			best, bestAt = name, at
		}
	}
	if best == "" {
		return auditPRPhaseLive, nil
	}
	return best, nil
}

// auditPRCriteria reads the criteria a relationship was held to. A pull request with no
// relationship, and a relationship with no registered criteria, both leave the criteria
// unavailable and the bundle is still built, because the issue fixes that the grading
// proceeds on what could be read. A relay the criteria could not be read from at all is an
// error: reporting that as "no criteria" would grade a pull request against nothing while
// looking like a clean run.
func auditPRCriteria(ctx context.Context, e *Env, cfg *Config, relationship string) ([]map[string]any, bool, error) {
	if relationship == "" {
		return nil, true, nil
	}
	out, err := auditPRRelay(ctx, e, cfg, "criteria-show", "--relationship", relationship)
	if err != nil {
		return nil, true, err
	}
	var answer struct {
		Criteria []map[string]any `json:"criteria"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		return nil, true, fmt.Errorf("the criteria-show answer: %w", err)
	}
	return answer.Criteria, len(answer.Criteria) == 0, nil
}

// auditPRScrub replaces the configured strings with a redaction marker. The longest string
// goes first, so a shorter one cannot cut into a longer one and leave a fragment behind. The
// marker carries no model, no pair and no child id, which is the whole point of the scrub.
func auditPRScrub(data []byte, scrubs []string) []byte {
	ordered := make([]string, 0, len(scrubs))
	for _, scrub := range scrubs {
		if scrub != "" {
			ordered = append(ordered, scrub)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, scrub := range ordered {
		data = bytes.ReplaceAll(data, []byte(scrub), []byte(auditPRRedacted))
	}
	return data
}

// auditPRScrubDocument scrubs every string value of a decoded JSON document, the keys
// excepted and nested maps and slices included. It runs before the document is encoded,
// because encoding/json escapes &, < and > as their \u00XX forms and a configured string that
// carries one of those bytes would otherwise never match the encoded text.
func auditPRScrubDocument(value any, scrubs []string) any {
	switch typed := value.(type) {
	case string:
		return string(auditPRScrub([]byte(typed), scrubs))
	case []string:
		out := make([]string, len(typed))
		for i, item := range typed {
			out[i] = string(auditPRScrub([]byte(item), scrubs))
		}
		return out
	case []map[string]any:
		if typed == nil {
			// A nil slice is the criteria read's "nothing was registered", which encodes as
			// null rather than []: the document keeps the shape it had before the walk.
			return typed
		}
		out := make([]map[string]any, len(typed))
		for i, item := range typed {
			out[i] = auditPRScrubObject(item, scrubs)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = auditPRScrubDocument(item, scrubs)
		}
		return out
	case map[string]any:
		return auditPRScrubObject(typed, scrubs)
	default:
		return value
	}
}

// auditPRScrubObject scrubs the values of one decoded JSON object, leaving its keys as they
// are: a key is the document's own field name, not a value a configuration can name.
func auditPRScrubObject(value map[string]any, scrubs []string) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = auditPRScrubDocument(item, scrubs)
	}
	return out
}

// auditPRDiffPaths is the paths a merge commit changes, read from git rather than from the
// patch text. A patch spells a path in more than one place and adds a tab after a `+++` line
// whose path carries a space, so reading the paths back out of it means re-implementing git's
// own quoting rules; `diff-tree --name-status -z` answers the paths themselves, NUL-delimited,
// with nothing to unquote.
//
// A deletion is left out, because a deleted path has no content at the merge commit; a rename
// or a copy contributes the path it became.
func auditPRDiffPaths(ctx context.Context, co auditPkgCheckout, merge string) ([]string, error) {
	if co.Repository == "" {
		return nil, errors.New("checkout_unconfigured: the checkout section names no repository")
	}
	if merge == "" {
		return nil, errors.New("the pull request names no merge commit")
	}
	out, err := auditPkgGit(ctx, co.Repository, "diff-tree", "-r", "-z", "--no-commit-id", "--name-status", "-M", merge+"^1", merge)
	if err != nil {
		return nil, err
	}
	return auditPRNameStatusPaths(out)
}

// auditPRNameStatusPaths reads the NUL-delimited records of `git diff-tree --name-status -z`.
// Each change is a status record and its path; a rename or a copy carries two paths and
// contributes the second, the path it became. A deletion is skipped, because a deleted path
// has no content at the merge commit.
//
// A record that is not well formed is an error rather than the end of the list: the answer is
// not the one that was asked for, and half-reading it would build a bundle missing a file the
// change carries while the bundle still reads as complete. An empty path is a framing error
// too, so a record that is present but blank is refused rather than silently shifting every
// later record by one.
func auditPRNameStatusPaths(out []byte) ([]string, error) {
	records, err := auditPRNameStatusRecords(out)
	if err != nil {
		return nil, err
	}
	var paths []string
	seen := map[string]bool{}
	add := func(path string) error {
		if path == "" {
			return errors.New("the git diff-tree listing names an empty path")
		}
		if seen[path] {
			return nil
		}
		seen[path] = true
		paths = append(paths, path)
		return nil
	}
	for i := 0; i < len(records); {
		status := records[i]
		if !auditPRNameStatus(status) {
			return nil, fmt.Errorf("the git diff-tree record %d is %q, not a status", i+1, status)
		}
		i++
		if status[0] == 'R' || status[0] == 'C' {
			// A rename or a copy names the path it came from and the path it became; the
			// bundle reads the latter.
			if i+2 > len(records) {
				return nil, fmt.Errorf("the git diff-tree record %q names fewer than two paths", status)
			}
			if records[i] == "" {
				return nil, fmt.Errorf("the git diff-tree record %q names an empty source path", status)
			}
			if err := add(records[i+1]); err != nil {
				return nil, err
			}
			i += 2
			continue
		}
		if i >= len(records) {
			return nil, fmt.Errorf("the git diff-tree record %q names no path", status)
		}
		// A deletion contributes no path to the bundle, but its record still has to carry one:
		// a blank path is the same framing error here as anywhere else, and accepting it would
		// hide a listing this build cannot read.
		if records[i] == "" {
			return nil, fmt.Errorf("the git diff-tree record %q names an empty path", status)
		}
		if status[0] != 'D' {
			if err := add(records[i]); err != nil {
				return nil, err
			}
		}
		i++
	}
	return paths, nil
}

// auditPRNameStatusRecords splits a `git diff-tree --name-status -z` listing into its records.
// It keeps an empty record: a record is a status or a path, so a blank one where a path belongs
// is a listing this build cannot read, and dropping it would shift every later record by one.
// The last record must be terminated by a NUL, as git always terminates it; a listing that ends
// in the middle of a record is truncated and is refused. An empty listing is no changes.
func auditPRNameStatusRecords(out []byte) ([]string, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, errors.New("the git diff-tree listing does not end with a NUL")
	}
	fields := bytes.Split(out[:len(out)-1], []byte{0})
	records := make([]string, 0, len(fields))
	for _, field := range fields {
		records = append(records, string(field))
	}
	return records, nil
}

// auditPRNameStatus reports whether a record is a `git diff-tree --name-status` status: one
// letter of the set git writes, followed by the similarity score a rename or a copy carries.
func auditPRNameStatus(record string) bool {
	if record == "" || !strings.ContainsRune("ACDMRTUXB", rune(record[0])) {
		return false
	}
	for i := 1; i < len(record); i++ {
		if record[i] < '0' || record[i] > '9' {
			return false
		}
	}
	return true
}

// auditPRFetch brings the integration branch's commits into the configured checkout, so the
// merge commit of every target can be read from it. The ref is the branch target discovery
// itself queries, not the checkout section's base_ref: a checkout configured for another
// branch would otherwise leave the dev merge objects missing and fail every `git show`.
func auditPRFetch(ctx context.Context, co auditPkgCheckout) error {
	if co.Repository == "" {
		return errors.New("checkout_unconfigured: the checkout section names no repository")
	}
	remote := "origin"
	if co.BaseRef != "" {
		remote, _ = auditPkgSplitRef(co.BaseRef)
	}
	_, err := auditPkgGit(ctx, co.Repository, "fetch", remote, auditPRBaseRef)
	return err
}

// auditPRTask is the task.md the grader reads: what the pull request is, the description its
// author wrote, and the criteria it is judged against.
func auditPRTask(source auditPRSource) string {
	var out strings.Builder
	out.WriteString("# Pull request audit\n\n")
	fmt.Fprintf(&out, "Pull request: #%d %s\n", source.Target.Number, source.Target.Title)
	out.WriteString("Head: " + source.Target.Merge + "\n")
	out.WriteString("Issue: " + source.Target.Issue + "\n\n")
	out.WriteString("The change is `" + auditPRDiffFile + "` and the changed files' content at the merge commit is under `" + auditPRFilesDir + "/`.\n\n")
	out.WriteString("## Description\n\n")
	if strings.TrimSpace(source.Target.Body) == "" {
		out.WriteString("The author wrote no description.\n")
	} else {
		out.WriteString(source.Target.Body + "\n")
	}
	out.WriteString("\n## Criteria\n\n")
	if len(source.Criteria) == 0 {
		out.WriteString("No criterion could be read. Judge against the description above and the change in `" + auditPRDiffFile + "` and `" + auditPRFilesDir + "/`, and say in each note which of the two you used.\n")
	} else {
		for _, criterion := range source.Criteria {
			id, _ := criterion["id"].(string)
			title, _ := criterion["title"].(string)
			out.WriteString("- " + id + ": " + title + "\n")
		}
	}
	return out.String()
}

// auditPRBuild assembles one pull request bundle at the merge commit and returns its
// directory. The bundle is rebuilt from scratch, because the grader reads whatever the
// directory holds. A build that fails part way removes the directory it was writing: bundle.json
// is written last, so a partial directory would already read as incomplete, and the caller's
// per-target skip relies on a failed target leaving nothing behind.
func auditPRBuild(ctx context.Context, e *Env, cfg *Config, section auditPRSection, co auditPkgCheckout, source auditPRSource) (string, error) {
	if co.Repository == "" {
		return "", errors.New("checkout_unconfigured: the checkout section names no repository")
	}
	root := auditPRBundleRoot(e, cfg, section)
	dir := crwconfig.JoinRoot(root, auditPRSubject(source.Target.Number))
	if err := auditPkgResetDir(root, dir); err != nil {
		return "", err
	}
	// A failure anywhere below leaves no half-built directory for the next run, and no directory
	// the grader could mistake for a bundle. The path was proved strictly below the root by
	// auditPkgResetDir just above, so this removal cannot reach anything else.
	complete := false
	defer func() {
		if !complete {
			if err := os.RemoveAll(dir); err != nil {
				// The invariant this cleanup exists for (no half-built bundle for the next run
				// or for the grader) is broken if the removal fails, so it is reported rather
				// than swallowed. The run still reports this target as failed below.
				fmt.Fprintf(e.Stderr, "crw manage audit pr: %s: %v\n", dir, err)
			}
		}
	}()
	// Every file the bundle holds goes through the scrub, so no model, pair or child id the
	// configuration names reaches the grader through a description, a criterion or a patch.
	if err := auditPRWriteFile(crwconfig.JoinRoot(dir, auditPRDiffFile), auditPRScrub(source.Patch, section.Scrub)); err != nil {
		return "", err
	}
	// A path is a bundle entry name as well as a source path, so it goes through the same scrub
	// as the bytes inside it: a changed path that carries a configured string would otherwise
	// hand the grader by directory listing exactly the value every file's content redacts.
	placed := map[string]string{}
	paths, err := auditPRDiffPaths(ctx, co, source.Target.Merge)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		name := string(auditPRScrub([]byte(path), section.Scrub))
		if other, taken := placed[name]; taken {
			return "", fmt.Errorf("the pull request paths %q and %q both redact to %q", other, path, name)
		}
		placed[name] = path
		target := crwconfig.JoinRoot(dir, auditPRFilesDir, filepath.FromSlash(name))
		if !auditPkgContained(dir, target) {
			return "", fmt.Errorf("the pull request names %q, which leaves the bundle", path)
		}
		body, err := auditPRBlob(ctx, co, source.Target.Merge, path)
		if err != nil {
			return "", err
		}
		if err := auditPRWriteBlob(target, auditPRScrub(body, section.Scrub)); err != nil {
			return "", err
		}
	}
	if err := auditPRWriteFile(crwconfig.JoinRoot(dir, auditPRTaskFile), auditPRScrub([]byte(auditPRTask(source)), section.Scrub)); err != nil {
		return "", err
	}
	criteria, err := auditPRCriteriaDocument(source, section.Scrub)
	if err != nil {
		return "", err
	}
	if err := auditPRWriteFile(crwconfig.JoinRoot(dir, auditPRCriteriaFile), criteria); err != nil {
		return "", err
	}
	bundle := map[string]any{
		"schema": auditBundleSchema, "mode": auditModePR, "subject": auditPRSubject(source.Target.Number),
		"head": source.Target.Merge, "issue": source.Target.Issue,
		"criteria_unavailable":     source.CriteriaUnavailable,
		"relationship_unavailable": source.RelationshipUnavailable,
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", err
	}
	if err := auditPRWriteFile(crwconfig.JoinRoot(dir, auditBundleFile), append(data, '\n')); err != nil {
		return "", err
	}
	complete = true
	return dir, nil
}

// auditPRBuildAndGrade assembles one pull request bundle and grades it with the drafts lock held
// across both, for the reason auditPkgBuildAndGrade holds it: a rebuild empties the bundle a grade
// reads, and a grade replaces the grade.json the rebuild's ledger row would describe, so the two
// must not interleave. The target carries only the pair and phase the job needs.
//
// A build error is one target's, and the caller names it and moves on. A grading or recording error
// is the run's (an unconfigured grader, an unwritable ledger), so it is returned as a
// auditPRRunError and the caller stops rather than rebuilding and grading every later target.
func auditPRBuildAndGrade(ctx context.Context, e *Env, cfg *Config, section auditPRSection, co auditPkgCheckout, source auditPRSource, target auditPRTarget) (string, []AuditResult, error) {
	release, err := auditDraftLock(e, cfg)
	if err != nil {
		return "", nil, auditPRRunError{err}
	}
	defer release()
	dir, err := auditPRBuild(ctx, e, cfg, section, co, source)
	if err != nil {
		return "", nil, err
	}
	results, err := auditGradeLocked(ctx, e, cfg, []AuditJob{{Bundle: dir, Pair: target.Pair, Phase: target.Phase}})
	if err != nil {
		return "", nil, auditPRRunError{err}
	}
	if len(results) != 1 {
		return "", nil, auditPRRunError{fmt.Errorf("the grader answered %d results for one bundle", len(results))}
	}
	return dir, results, nil
}

// auditPRRunError is a grading or recording failure that belongs to the whole run rather than to one
// target, so the caller stops instead of skipping and rebuilding every later target against the same
// broken shared state.
type auditPRRunError struct{ err error }

func (e auditPRRunError) Error() string { return e.err.Error() }

func (e auditPRRunError) Unwrap() error { return e.err }

// auditPRCriteriaDocument encodes the criteria document the grader reads. Every string value
// is scrubbed before the document is encoded, because encoding/json escapes &, < and > as
// \u0026 and the like: a scrub applied only to the encoded bytes would never match a name that
// carries one of them, and the grader would read the name itself back out of the JSON. The
// encoder writes no HTML escape, and the encoded bytes go through the byte scrub once more, so
// a value that reached the document by another route is still replaced.
func auditPRCriteriaDocument(source auditPRSource, scrubs []string) ([]byte, error) {
	doc := map[string]any{
		"issue": source.Target.Issue, "criteria": source.Criteria,
		"criteria_unavailable": source.CriteriaUnavailable,
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(auditPRScrubDocument(doc, scrubs)); err != nil {
		return nil, err
	}
	return auditPRScrub(encoded.Bytes(), scrubs), nil
}

// auditPRWriteBlob writes one changed file into the bundle. The open refuses to follow a
// symbolic link at the path, so a link planted at a bundle entry cannot redirect this write
// outside the bundle even though the lexical containment check passed.
func auditPRWriteBlob(path string, data []byte) error {
	if err := os.MkdirAll(rootDir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// auditPRBlob reads one file of the merge commit out of the checkout.
func auditPRBlob(ctx context.Context, co auditPkgCheckout, merge, path string) ([]byte, error) {
	var buf bytes.Buffer
	if err := auditPkgBlob(ctx, co.Repository, merge, path, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// auditPRWriteFile writes one bundle file, making its directory first.
func auditPRWriteFile(path string, data []byte) error {
	if err := os.MkdirAll(rootDir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// auditPRUsage is the line the pr subcommand prints.
const auditPRUsage = "usage: crw manage audit pr [--max N] [--dry-run]"

// auditPRRun is crw manage audit pr.
func auditPRRun(ctx context.Context, e *Env, args []string) int {
	max, dryRun, help, err := auditPRParseArgs(args)
	if help {
		fmt.Fprintln(e.Stdout, auditPRUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, auditPRUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return usageExit
	}
	return auditPRRunWith(ctx, e, coreDefaults(e), max, dryRun)
}

// auditPRParseArgs reads the pr flags: the target cap and the dry run switch.
func auditPRParseArgs(args []string) (max int, dryRun, help bool, err error) {
	max = auditPRDefaultMax
	maxSet := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help", "help":
			return max, false, true, nil
		case "--dry-run":
			dryRun = true
			continue
		}
		name := args[i]
		if !strings.HasPrefix(name, "--") {
			return max, false, false, fmt.Errorf("unexpected argument %q", name)
		}
		key, value := strings.TrimPrefix(name, "--"), ""
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			key, value = key[:eq], key[eq+1:]
		} else {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return max, false, false, fmt.Errorf("the option %s needs a value", name)
			}
			value = args[i]
		}
		if key != "max" {
			return max, false, false, fmt.Errorf("unknown option %s", name)
		}
		if maxSet {
			return max, false, false, fmt.Errorf("the option --%s is given twice", key)
		}
		maxSet = true
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return max, false, false, fmt.Errorf("--max %q is not a count", value)
		}
		max = parsed
	}
	return max, dryRun, false, nil
}

// auditPRRunWith does the work auditPRRun validated the arguments for: it selects the
// targets, assembles and grades one bundle each, and rewrites the report from the ledger.
//
// A failure that belongs to one target (its relay child read, its patch, its criteria, its
// bundle build) names that pull request on stderr and skips it, leaving no ledger row so a
// later run retries it, and the run exits 1 at the end when any target failed. A failure
// common to every target (the configuration, the checkout, the fetch, the grader) still
// stops the run at once, because retrying the next target would not fix it.
func auditPRRunWith(ctx context.Context, e *Env, cfg *Config, max int, dryRun bool) int {
	section, err := auditPRSectionOf(cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return 1
	}
	if section.PRSince == "" {
		fmt.Fprintln(e.Stderr, "crw manage audit: error: audit_since_unset: the audit section names no pr_since")
		return usageExit
	}
	since, err := time.Parse(time.RFC3339, section.PRSince)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: the audit section pr_since %q: %v\n", section.PRSince, err)
		return 1
	}
	pattern, err := auditPRPattern(section)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return 1
	}
	entries, err := auditPRList(ctx, e, cfg, since)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return 1
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return 1
	}
	targets, err := auditPRSelect(entries, pattern, since, auditPRAudited(rows), max)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return 1
	}
	failed := false
	// Resolving a target reads the relay for that one pull request, so a failure is that
	// target's: it is named and skipped, and the run reports the failure at the end. A phase
	// start is configuration, common to every target, so a bad one still stops the run.
	resolved := make([]auditPRTarget, 0, len(targets))
	for i := range targets {
		if auditPRCancelled(e, ctx) {
			return 1
		}
		child, err := auditPRChildOf(ctx, e, cfg, targets[i].Issue)
		if err != nil {
			if auditPRCancelled(e, ctx) {
				return 1
			}
			auditPRSkipTarget(e, targets[i], err)
			failed = true
			continue
		}
		targets[i].Child = child
		targets[i].Pair = auditPRPairOf(section.Pairs, child.Model)
		phase, err := auditPRPhaseOf(section, targets[i].MergedAt)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
			return 1
		}
		targets[i].Phase = phase
		resolved = append(resolved, targets[i])
	}
	if dryRun {
		if code := auditPRWriteDryRun(e, resolved); code != 0 {
			return code
		}
		if failed {
			return 1
		}
		return 0
	}
	if len(resolved) > 0 {
		co, err := auditPkgCheckoutOf(cfg)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
			return 1
		}
		if err := auditPRFetch(ctx, co); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
			return 1
		}
		for _, target := range resolved {
			if auditPRCancelled(e, ctx) {
				return 1
			}
			patch, err := auditPRDiff(ctx, cfg, target.Number)
			if err != nil {
				if auditPRCancelled(e, ctx) {
					return 1
				}
				auditPRSkipTarget(e, target, err)
				failed = true
				continue
			}
			criteria, unavailable, err := auditPRCriteria(ctx, e, cfg, target.Child.Relationship)
			if err != nil {
				if auditPRCancelled(e, ctx) {
					return 1
				}
				auditPRSkipTarget(e, target, err)
				failed = true
				continue
			}
			_, _, err = auditPRBuildAndGrade(ctx, e, cfg, section, co, auditPRSource{
				Target: target, Patch: patch, Criteria: criteria, CriteriaUnavailable: unavailable,
				RelationshipUnavailable: target.Child.Relationship == "", Relationship: target.Child.Relationship,
			}, target)
			if err != nil {
				// A grading or recording failure belongs to the whole run: every later target would
				// meet the same broken shared state, so the run stops rather than rebuilding and
				// grading on.
				var run auditPRRunError
				if errors.As(err, &run) {
					fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
					return 1
				}
				if auditPRCancelled(e, ctx) {
					return 1
				}
				auditPRSkipTarget(e, target, err)
				failed = true
				continue
			}
		}
	}
	if err := auditReportWrite(e, cfg); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

// auditPRCancelled reports whether the run was interrupted, writing the interrupt to stderr.
// An interrupt is the run's, not one target's, so it stops the run rather than becoming a skip:
// a skip would keep working after the first interrupt and let the run rewrite the report.
func auditPRCancelled(e *Env, ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
		return true
	}
	return false
}

// auditPRSkipTarget names one pull request whose bundle could not be built and moves on to the
// next. The target is left with no ledger row, so a later run picks it up again, and the run
// reports the failure at the end.
func auditPRSkipTarget(e *Env, target auditPRTarget, err error) {
	fmt.Fprintf(e.Stderr, "crw manage audit pr: #%d: %v\n", target.Number, err)
}

// auditPRWriteDryRun writes the targets, one JSON document per line, and nothing else.
func auditPRWriteDryRun(e *Env, targets []auditPRTarget) int {
	for _, target := range targets {
		line, err := json.Marshal(auditPRDryRun{
			Subject: auditPRSubject(target.Number), Issue: target.Issue,
			Head: target.Merge, Pair: target.Pair, Phase: target.Phase,
		})
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage audit pr: error: %v\n", err)
			return 1
		}
		fmt.Fprintf(e.Stdout, "%s\n", line)
	}
	return 0
}
