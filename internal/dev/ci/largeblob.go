//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The large-blob guard of `crw-dev ci validate` (CRW-536). This repository merges pull requests
// with merge commits, so every blob of every commit on a branch becomes part of dev's public
// history, and a public history cannot drop it again. The guard refuses any blob over 2 MiB that
// the range under judgment brings into the history, unless the allow list names its path with a
// ceiling that covers it. The range is the commits a pull request adds to its base, the commits a
// push to dev adds, or every commit reachable from HEAD when no base is known (docs/CI.md).

const (
	// largeBlobLimit is the largest size a blob may have without an allow-list entry.
	largeBlobLimit = 2 << 20
	// largeBlobAllowListPath is the allow list, relative to the repository root. It starts empty.
	largeBlobAllowListPath = ".large-blob-allowlist.json"
	// largeBlobBaseEnv carries the range's base: the pull request's base commit, or the commit a
	// push replaced. largeBlobEventEnv is the workflow event that decides how strictly it is read.
	largeBlobBaseEnv  = "BLOB_RANGE_BASE"
	largeBlobEventEnv = "GITHUB_EVENT_NAME"
)

var largeBlobFullHash = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// largeBlobAllowance is one allow-list entry: a path and the largest size it may have, with the
// reason the file has to be committed.
type largeBlobAllowance struct {
	maxBytes int64
	reason   string
}

// largeBlob is an object over the limit that the range brings into the history.
type largeBlob struct {
	oid     string
	size    int64
	commit  string   // the first commit of the range that brought it, "" when none is found
	subject string   // that commit's short hash and subject
	paths   []string // the paths the blob has in that commit, sorted
}

// largeBlobCheck judges root's history. It returns the refusals, one per blob plus a closing
// explanation, or the one line a pass is reported with; both are empty when the branch is unborn.
func largeBlobCheck(root string, getenv func(string) string) (errs []string, summary string) {
	if _, err := largeBlobCommit(root, "HEAD"); err != nil {
		unborn, cause := largeBlobUnbornHead(root)
		if !unborn {
			return []string{"validate: HEAD cannot be read as a commit: " + cause}, ""
		}
		return nil, "" // the branch is unborn: nothing has come into any history
	}
	shallow, err := runGit(root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return []string{"validate: " + err.Error()}, ""
	}
	if strings.TrimSpace(string(shallow)) != "false" {
		return []string{"validate: the large-blob check needs the full history, but this checkout is shallow; fetch it with git fetch --unshallow"}, ""
	}
	base, err := largeBlobRange(root, getenv)
	if err != nil {
		return []string{err.Error()}, ""
	}
	allow, err := largeBlobAllowList(root)
	if err != nil {
		return []string{err.Error()}, ""
	}
	blobs, err := largeBlobOffenders(root, base)
	if err != nil {
		return []string{"validate: " + err.Error()}, ""
	}
	allowed := 0
	for _, blob := range blobs {
		if line, refused := largeBlobRefusal(blob, allow); refused {
			errs = append(errs, line)
		} else {
			allowed++
		}
	}
	if len(errs) > 0 {
		return append(errs, largeBlobAdvice(base)), ""
	}
	scope := "every commit reachable from HEAD"
	if base != "" {
		scope = "commits added since " + base
	}
	summary = "No blob over 2 MiB comes into the history (" + scope + ")"
	if allowed > 0 {
		summary += fmt.Sprintf(" beyond %d allow-listed in %s", allowed, largeBlobAllowListPath)
	}
	return nil, summary + "."
}

// largeBlobUnbornHead tells an unborn branch from a HEAD that cannot be read as a commit. A branch
// is unborn when HEAD is a symbolic ref to a branch whose ref does not resolve yet: the first
// commit has not been made, so no history exists to judge. Every other state names its cause, and
// that cause is what the refusal reports - a HEAD this checkout does not hold as a commit, or a
// ref store git itself could not read.
func largeBlobUnbornHead(root string) (unborn bool, cause string) {
	ref, err := runGit(root, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		// HEAD is not a symbolic ref: a detached HEAD, or one git cannot read at all.
		id, idErr := runGit(root, "rev-parse", "--verify", "--quiet", "HEAD")
		if idErr != nil {
			return false, largeBlobOneLine(idErr.Error())
		}
		return false, fmt.Sprintf("HEAD is detached at %s, which this checkout does not hold as a commit", strings.TrimSpace(string(id)))
	}
	// The command ends its output with a newline; a ref name may itself end in whitespace, so only
	// the newline is removed.
	name := strings.TrimRight(string(ref), "\r\n")
	id, err := runGit(root, "rev-parse", "--verify", "--quiet", name)
	switch {
	case err == nil:
		return false, fmt.Sprintf("HEAD names %s at %s, which this checkout does not hold as a commit", name, strings.TrimSpace(string(id)))
	case !strings.HasPrefix(name, "refs/heads/"):
		return false, fmt.Sprintf("HEAD is a symbolic ref to %s, which is not a branch and does not resolve", name)
	case largeBlobExitCode(err) == 1:
		return true, "" // the branch ref does not resolve: nothing has come into any history
	default:
		return false, largeBlobOneLine(err.Error())
	}
}

// largeBlobExitCode is the status a runGit failure ended with, or 0 when the failure carries none.
// rev-parse --verify --quiet answers 1 for a revision that does not exist and a different status
// (128 for a fatal error) for a ref store it cannot read; only the first is an unborn branch.
func largeBlobExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

// largeBlobOneLine is text as one line: git's own message can carry newlines (a path inside it, a
// multi-line fatal), and a refusal is one line.
func largeBlobOneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// largeBlobRange is the base commit of the range to judge, or "" for every commit reachable from
// HEAD. A pull request run must name its base, as the secrets scan requires, so a wiring fault
// cannot change what is judged unnoticed. Any other run uses a base it can resolve and otherwise
// judges the whole history, which is stricter, never weaker.
func largeBlobRange(root string, getenv func(string) string) (string, error) {
	value := strings.TrimSpace(getenv(largeBlobBaseEnv))
	if getenv(largeBlobEventEnv) == "pull_request" {
		if !largeBlobFullHash.MatchString(value) {
			return "", fmt.Errorf("validate: a pull_request run needs %s, the full SHA of the pull request's base commit; got %q", largeBlobBaseEnv, value)
		}
		base, err := largeBlobCommit(root, value)
		if err != nil {
			return "", fmt.Errorf("validate: %s %s is not a commit in this checkout; fetch the full history", largeBlobBaseEnv, value)
		}
		return base, nil
	}
	if value == "" || strings.Trim(value, "0") == "" || strings.HasPrefix(value, "-") {
		return "", nil
	}
	if base, err := largeBlobCommit(root, value); err == nil {
		return base, nil
	}
	return "", nil
}

// largeBlobCommit is the full hash of the commit rev names.
func largeBlobCommit(root, rev string) (string, error) {
	out, err := runGit(root, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

// largeBlobAllowList reads the allow list. A missing file is an empty list; a file that does not
// read exactly as documented is refused, so a typo cannot leave an entry without effect or a
// ceiling or a reason out.
func largeBlobAllowList(root string) (map[string]largeBlobAllowance, error) {
	data, err := os.ReadFile(filepath.Join(root, largeBlobAllowListPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s", largeBlobAllowListPath, fmt.Sprintf(format, args...))
	}
	if err != nil {
		return nil, fail("%v", err)
	}
	value, err := decodeJSON(data)
	if err != nil {
		return nil, fail("%v", err)
	}
	top, ok := object(value)
	if !ok {
		return nil, fail("must be a JSON object")
	}
	for _, key := range sortedKeys(top) {
		if key != "entries" {
			return nil, fail("unknown key %q", key)
		}
	}
	list, ok := top["entries"].([]any)
	if !ok {
		return nil, fail(`"entries" must be a list`)
	}
	allow := map[string]largeBlobAllowance{}
	for i, item := range list {
		label := fmt.Sprintf("entries[%d]", i)
		entry, ok := object(item)
		if !ok {
			return nil, fail("%s: must be an object", label)
		}
		for _, key := range sortedKeys(entry) {
			if key != "path" && key != "max_bytes" && key != "reason" {
				return nil, fail("%s: unknown key %q", label, key)
			}
		}
		name, _ := entry["path"].(string)
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fail("%s: path must be a clean relative path", label)
		}
		reason, _ := entry["reason"].(string)
		if strings.TrimSpace(reason) == "" {
			return nil, fail("%s: reason must not be empty", label)
		}
		number, _ := entry["max_bytes"].(json.Number)
		ceiling, err := number.Int64()
		if number == "" || err != nil || ceiling <= largeBlobLimit {
			return nil, fail("%s: max_bytes must be an integer above %d", label, largeBlobLimit)
		}
		if _, seen := allow[name]; seen {
			return nil, fail("%s: duplicate path %q", label, name)
		}
		allow[name] = largeBlobAllowance{ceiling, reason}
	}
	return allow, nil
}

// largeBlobOffenders is every blob over the limit that comes into the history in the range: the
// objects HEAD reaches that base does not (all of them when base is ""), with the commit and paths
// of each. The ids go to cat-file bare, so no path is ever read as part of an object name.
func largeBlobOffenders(root, base string) ([]largeBlob, error) {
	span := "HEAD"
	if base != "" {
		span = base + "..HEAD"
	}
	ids, err := runGit(root, "rev-list", "--objects", "--no-object-names", span)
	if err != nil {
		return nil, err
	}
	sizes, err := largeBlobGitInput(root, ids, "cat-file", "--batch-check=%(objecttype) %(objectname) %(objectsize)")
	if err != nil {
		return nil, err
	}
	var blobs []largeBlob
	for _, line := range lines(string(sizes)) {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case len(fields) == 2 && fields[1] == "missing":
			return nil, fmt.Errorf("object %s is missing from this checkout; fetch the full history", fields[0])
		case len(fields) == 3 && fields[0] == "blob":
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("git cat-file: unreadable size in %q", line)
			}
			if size > largeBlobLimit {
				blobs = append(blobs, largeBlob{oid: fields[1], size: size})
			}
		}
	}
	if len(blobs) == 0 {
		return nil, nil
	}
	if base != "" { // rev-list excludes only the base's own tree: one the base's history held and dropped before it is not new
		held, err := runGit(root, "rev-list", "--objects", "--no-object-names", base)
		if err != nil {
			return nil, err
		}
		inBase := map[string]bool{}
		for _, id := range lines(string(held)) {
			inBase[id] = true
		}
		kept := blobs[:0]
		for _, blob := range blobs {
			if !inBase[blob.oid] {
				kept = append(kept, blob)
			}
		}
		blobs = kept
	}
	for i := range blobs {
		if err := largeBlobIntroducer(root, span, &blobs[i]); err != nil {
			return nil, err
		}
	}
	sort.Slice(blobs, func(i, j int) bool {
		return strings.Join(blobs[i].paths, "\x00")+blobs[i].oid < strings.Join(blobs[j].paths, "\x00")+blobs[j].oid
	})
	return blobs, nil
}

// largeBlobIntroducer fills in the first commit of span that brought blob, its subject and the
// paths the blob has in it. git log lists the commits oldest first and, with separate merge diffs,
// once per parent of a merge; with -z --raw it writes NUL separated tokens: a commit's hash, then
// per changed path a record (": modes ids status", after one newline) and the path itself. The
// token after a record is always a path, kept byte for byte, never read as a hash or a record.
// The options pin what the repository's own configuration (log.showRoot, log.diffMerges,
// diff.renames, color.ui, log.showSignature) would otherwise change in that output.
func largeBlobIntroducer(root, span string, blob *largeBlob) error {
	out, err := runGit(root, "log", "--topo-order", "--reverse", "--root", "--diff-merges=separate", "--no-renames", "--no-abbrev",
		"--no-color", "--no-show-signature", "-z", "--raw", "--format=%H", "--find-object="+blob.oid, span)
	if err != nil {
		return err
	}
	var header string
	var present, wantPath bool
	for _, token := range strings.Split(string(out), "\x00") {
		if wantPath {
			wantPath = false
			if present && (blob.commit == "" || header == blob.commit) {
				blob.commit = header
				blob.paths = append(blob.paths, token)
			}
			continue
		}
		control := strings.TrimPrefix(token, "\n")
		switch {
		case strings.HasPrefix(control, ":"):
			fields := strings.Fields(control[1:]) // source mode, new mode, source id, new id, status
			present = len(fields) == 5 && fields[3] == blob.oid
			wantPath = true
		case largeBlobFullHash.MatchString(control):
			header = control
		}
	}
	blob.paths = sortedSet(blob.paths)
	if blob.commit == "" {
		return nil
	}
	subject, err := runGit(root, "log", "-1", "--no-color", "--no-show-signature", "--format=%h %s", blob.commit)
	if err != nil {
		return err
	}
	blob.subject = strings.TrimSpace(string(subject))
	return nil
}

// largeBlobGitInput runs git in root with input on its standard input.
func largeBlobGitInput(root string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			err = fmt.Errorf("%w: %s", err, text)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// largeBlobRefusal is the line that refuses blob, and whether it is refused: false when the allow
// list admits the blob.
func largeBlobRefusal(blob largeBlob, allow map[string]largeBlobAllowance) (string, bool) {
	names := make([]string, len(blob.paths))
	var listed *largeBlobAllowance
	for i, name := range blob.paths {
		names[i] = largeBlobDisplay(name)
		if entry, ok := allow[name]; ok {
			if entry.maxBytes >= blob.size {
				return "", false
			}
			listed = &entry
		}
	}
	where := "a commit this run could not find"
	if blob.commit != "" {
		short, subject, _ := strings.Cut(blob.subject, " ")
		where = fmt.Sprintf("commit %s %q", short, subject)
	}
	label := strings.Join(names, ", ")
	if len(names) == 0 {
		label = blob.oid
	}
	if listed != nil {
		return fmt.Sprintf("%s: %d bytes, allow-listed in %s only up to %d bytes (%s); brought in by %s",
			label, blob.size, largeBlobAllowListPath, listed.maxBytes, listed.reason, where), true
	}
	return fmt.Sprintf("%s: %d bytes, over the 2 MiB (%d bytes) limit; brought in by %s", label, blob.size, largeBlobLimit, where), true
}

// largeBlobDisplay is name as a refusal shows it: as it is, or quoted when it holds a character a
// line cannot show.
func largeBlobDisplay(name string) string {
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return strconv.Quote(name)
		}
	}
	if name != strings.TrimSpace(name) {
		return strconv.Quote(name)
	}
	return name
}

// largeBlobAdvice closes the refusal: why it matters, how to shrink the file or admit it, and how
// to rebuild the branch from its base so that no commit of it carries the blob.
func largeBlobAdvice(base string) string {
	if base == "" {
		base = "<base>"
	}
	return strings.Join([]string{
		"A blob that reaches dev stays in its public history for good, even when a later commit deletes or shrinks the file, because pull requests are merged with merge commits.",
		"Shrink the file, or admit it:",
		"  - regenerate a generated input deterministically in the test that uses it, and check a large record by its hash and count instead of committing it;",
		"  - or, when the file has to be committed, add it to " + largeBlobAllowListPath + " with its path, a max_bytes ceiling above its size and the reason.",
		"Then rebuild the branch from its base, so that no commit of it still carries the blob (deleting the file in a later commit leaves the blob in the branch history):",
		"  git switch -c <new-branch> " + base,
		"  git merge --squash <old-branch>",
		"  (shrink or remove the file in the working tree)",
		"  git add -A && git commit",
		"  git push -u origin <new-branch>",
		"Open a replacement pull request from <new-branch> and close the old one unmerged, so that no commit that carries the blob reaches dev.",
	}, "\n")
}
