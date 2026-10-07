package manage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The names inside a package bundle that audit.go does not already fix.
const (
	auditPkgSrcDir          = "src"
	auditPkgReferenceDir    = "reference"
	auditPkgKnownDefectsDir = "known-defects"
	auditPkgCriteriaFile    = "criteria.json"
	auditPkgTaskFile        = "task.md"
	auditPkgLargeListName   = "large-files.txt"
	auditPkgBundlePrefix    = "pkg-"
)

// auditPkgLargeFileBytes is the size past which a package file is not copied into the
// bundle whole: the listing carries its path, size and sha256 instead, so a big generated
// file never enters the bundle and its bytes are hashed as they stream past.
const auditPkgLargeFileBytes = 1 << 20

// auditPkgHeadChars is how much of the head a bundle directory name carries.
const auditPkgHeadChars = 12

// auditPkgIssue is the issue this product's own package audits are filed under, so a
// bundle always carries the field the ledger row quotes.
const auditPkgIssue = "CRW-764"

// auditPkgCheckout is the checkout section: the local repository the package files and the
// go list expansion are read from, and the ref a missing head resolves to.
type auditPkgCheckout struct {
	Repository string `json:"repository"`
	BaseRef    string `json:"base_ref"`
}

// auditPkgSource is one package_criteria entry: where the port source lives, which
// known-defects paths the package already carries, and which issue keys hold its criteria.
type auditPkgSource struct {
	Source       []string `json:"source"`
	KnownDefects []string `json:"known_defects"`
	Issues       []string `json:"issues"`
}

// auditPkgSection is the part of the audit section the package mode reads: where bundles
// are written and which criteria source each package path prefix has.
type auditPkgSection struct {
	BundleDir       string                    `json:"bundle_dir"`
	PackageCriteria map[string]auditPkgSource `json:"package_criteria"`
}

// auditPkgEntry is one file of the package at the head, as git ls-tree -l names it.
type auditPkgEntry struct {
	Path string
	Size int64
}

// The seams a test replaces instead of running git, reading the host or reaching the
// relay: the commands themselves, the file reader, the go list expansion and the criteria
// read.
var (
	auditPkgGit           = auditPkgGitRun
	auditPkgBlob          = auditPkgBlobRun
	auditPkgGoList        = auditPkgGoListRun
	auditPkgRelayCriteria = auditPkgRelayCriteriaRun
)

func auditPkgGitRun(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

func auditPkgBlobRun(ctx context.Context, repo, head, path string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "git", "show", head+":"+path)
	cmd.Dir = repo
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		return err
	}
	_, copyErr := io.Copy(w, stdout)
	waitErr := cmd.Wait()
	if copyErr != nil {
		return copyErr
	}
	if waitErr != nil {
		return fmt.Errorf("git show %s:%s: %w: %s", head, path, waitErr, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// auditPkgGoListRun expands a package pattern. It asks go list for each package's directory
// rather than its import path, because a package is addressed here the way git addresses it:
// by its path inside the checkout.
func auditPkgGoListRun(ctx context.Context, repo, pattern string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-f", "{{.Dir}}", pattern)
	cmd.Dir = repo
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list %s: %w: %s", pattern, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// auditPkgRelative turns the directories go list answered with into paths inside the
// checkout, which is how the package files are read and how a package_criteria prefix is
// matched. A directory outside the checkout is refused rather than silently read.
func auditPkgRelative(co auditPkgCheckout, dirs []string) ([]string, error) {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		rel, err := filepath.Rel(co.Repository, dir)
		if err != nil {
			return nil, fmt.Errorf("package directory %s: %w", dir, err)
		}
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("package directory %s is outside the checkout %s", dir, co.Repository)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

// auditPkgRelayCriteriaRun reads one issue key's registered criteria: the assignment that
// names the relationship, then that relationship's criteria. Both are read-only relay
// commands, and a key with no assignment is an error the caller records as missing.
func auditPkgRelayCriteriaRun(ctx context.Context, e *Env, cfg *Config, issue string) ([]byte, error) {
	found, code, err := e.Relay(ctx, cfg, "assignment-find", "--issue", issue)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("assignment-find exited with status %d", code)
	}
	var doc struct {
		Assignments []struct {
			RelationshipID string `json:"relationshipId"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(found, &doc); err != nil {
		return nil, fmt.Errorf("assignment-find answer: %w", err)
	}
	if len(doc.Assignments) == 0 {
		return nil, errors.New("no assignment for " + issue)
	}
	relationship := doc.Assignments[len(doc.Assignments)-1].RelationshipID
	if relationship == "" {
		return nil, errors.New("the assignment names no relationship")
	}
	criteria, code, err := e.Relay(ctx, cfg, "criteria-show", "--relationship", relationship)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("criteria-show exited with status %d", code)
	}
	return criteria, nil
}

// auditPkgSectionOf reads the audit section's package keys. A configuration with no audit
// section leaves every value at its default.
func auditPkgSectionOf(cfg *Config) (auditPkgSection, error) {
	var section auditPkgSection
	if cfg != nil {
		if err := cfg.Section("audit", &section); err != nil {
			return auditPkgSection{}, err
		}
	}
	return section, nil
}

// auditPkgCheckoutOf reads the checkout section, defaulting the ref to origin/dev.
func auditPkgCheckoutOf(cfg *Config) (auditPkgCheckout, error) {
	co := auditPkgCheckout{BaseRef: "origin/dev"}
	if cfg != nil {
		if err := cfg.Section("checkout", &co); err != nil {
			return auditPkgCheckout{}, err
		}
	}
	if co.BaseRef == "" {
		co.BaseRef = "origin/dev"
	}
	return co, nil
}

// auditPkgBundleRoot is where a package bundle is written: the configured bundle_dir, else
// the bundles directory below the audit state directory.
func auditPkgBundleRoot(e *Env, cfg *Config, section auditPkgSection) string {
	if section.BundleDir != "" {
		return section.BundleDir
	}
	return filepath.Join(auditStateDir(e, cfg), "audit", "bundles")
}

// auditPkgSourceFor is the criteria source of a package: the longest declared path prefix
// the package path starts with at a path-segment boundary, so a more specific entry wins and
// a prefix that only shares the first characters of a segment does not match. Without the
// boundary, `internal/manage` would claim `internal/manager` and judge it against another
// package's criteria, port sources and known defects.
func auditPkgSourceFor(section auditPkgSection, pkg string) auditPkgSource {
	best, bestLen := auditPkgSource{}, -1
	for prefix, source := range section.PackageCriteria {
		if !auditPkgPrefixMatches(pkg, prefix) || len(prefix) <= bestLen {
			continue
		}
		best, bestLen = source, len(prefix)
	}
	return best
}

// auditPkgPrefixMatches reports whether pkg is the prefix itself or starts with it at a
// segment boundary. A trailing separator on the prefix already marks the boundary.
func auditPkgPrefixMatches(pkg, prefix string) bool {
	if prefix == "" || !strings.HasPrefix(pkg, prefix) {
		return false
	}
	if pkg == prefix || strings.HasSuffix(prefix, "/") {
		return true
	}
	return strings.HasPrefix(pkg[len(prefix):], "/")
}

// auditPkgBundleName is the bundle directory name: the package path with its slashes written
// as underscores, the first characters of the head, and a short digest of the whole package
// path. The digest is what keeps two package paths apart when the substitution alone is not
// injective, so `a/b` and `a_b` at one head never share a directory.
func auditPkgBundleName(pkg, head string) string {
	short := head
	if len(short) > auditPkgHeadChars {
		short = short[:auditPkgHeadChars]
	}
	sum := sha256.Sum256([]byte(pkg))
	return auditPkgBundlePrefix + strings.ReplaceAll(pkg, "/", "_") + "-" + short + "-" + hex.EncodeToString(sum[:4])
}

// auditPkgResolveHead is the head a bundle is built at: the one given, else the tip of the
// configured base ref after a fetch.
func auditPkgResolveHead(ctx context.Context, co auditPkgCheckout, head string) (string, error) {
	if head != "" {
		return head, nil
	}
	if co.Repository == "" {
		return "", errors.New("checkout_unconfigured: the checkout section names no repository")
	}
	remote, ref := auditPkgSplitRef(co.BaseRef)
	if _, err := auditPkgGit(ctx, co.Repository, "fetch", remote, ref); err != nil {
		return "", err
	}
	out, err := auditPkgGit(ctx, co.Repository, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("the fetched head is empty")
	}
	return sha, nil
}

// auditPkgSplitRef splits a base ref into the remote to fetch and the ref to ask it for.
func auditPkgSplitRef(ref string) (string, string) {
	if i := strings.IndexByte(ref, '/'); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return "origin", ref
}

// auditPkgBuild assembles one package bundle at the head and returns its directory. The
// bundle is rebuilt from scratch, because the grader reads whatever the directory holds.
func auditPkgBuild(ctx context.Context, e *Env, cfg *Config, section auditPkgSection, co auditPkgCheckout, pkg, head string) (string, error) {
	if co.Repository == "" {
		return "", errors.New("checkout_unconfigured: the checkout section names no repository")
	}
	root := auditPkgBundleRoot(e, cfg, section)
	dir := filepath.Join(root, auditPkgBundleName(pkg, head))
	if err := auditPkgResetDir(root, dir); err != nil {
		return "", err
	}
	source := auditPkgSourceFor(section, pkg)
	entries, err := auditPkgTreeEntriesAt(ctx, co, head, pkg)
	if err != nil {
		return "", err
	}
	large, err := auditPkgWriteSrc(ctx, co, head, dir, entries)
	if err != nil {
		return "", err
	}
	if len(large) > 0 {
		listing := filepath.Join(dir, auditPkgSrcDir, auditPkgLargeListName)
		if err := os.WriteFile(listing, []byte(strings.Join(large, "\n")+"\n"), 0o600); err != nil {
			return "", err
		}
	}
	criteria, missing, err := auditPkgCriteria(ctx, e, cfg, source)
	if err != nil {
		return "", err
	}
	unavailable := len(criteria) == 0 && len(source.Source) == 0 && len(source.KnownDefects) == 0
	criteriaDoc := map[string]any{
		"package": pkg, "head": head, "criteria": criteria, "criteria_missing": missing,
	}
	if err := auditPkgWriteJSON(filepath.Join(dir, auditPkgCriteriaFile), criteriaDoc); err != nil {
		return "", err
	}
	if err := auditPkgCopySources(source.Source, filepath.Join(dir, auditPkgReferenceDir)); err != nil {
		return "", err
	}
	if err := auditPkgWriteAtHead(ctx, co, head, source.KnownDefects, filepath.Join(dir, auditPkgKnownDefectsDir)); err != nil {
		return "", err
	}
	task := auditPkgTask(pkg, head, criteria, missing)
	if err := os.WriteFile(filepath.Join(dir, auditPkgTaskFile), []byte(task), 0o600); err != nil {
		return "", err
	}
	bundle := map[string]any{
		"schema": auditBundleSchema, "mode": auditModePackage, "subject": pkg,
		"head": head, "issue": auditPkgIssue, "criteria_unavailable": unavailable,
	}
	if err := auditPkgWriteJSON(filepath.Join(dir, auditBundleFile), bundle); err != nil {
		return "", err
	}
	return dir, nil
}

// auditPkgResetDir makes the bundle directory empty. It refuses a directory that is not
// strictly below the bundle root, so a package path can never make this remove anything
// else.
func auditPkgResetDir(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("bundle %s is outside %s", dir, root)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

// auditPkgTreeEntriesAt lists the package's files at the head with their sizes. The listing
// is NUL-delimited, because the default newline-delimited output quotes a path that carries
// a tab, a newline or a non-ASCII byte, and a quoted path is not the path git would read.
func auditPkgTreeEntriesAt(ctx context.Context, co auditPkgCheckout, head, pkg string) ([]auditPkgEntry, error) {
	out, err := auditPkgGit(ctx, co.Repository, "ls-tree", "-r", "-l", "-z", head, "--", pkg)
	if err != nil {
		return nil, err
	}
	return auditPkgTreeEntries(out)
}

// auditPkgTreeEntries reads git ls-tree -r -l -z output: mode, type, object, size and a tab
// before the path, with each record ended by a NUL byte.
func auditPkgTreeEntries(out []byte) ([]auditPkgEntry, error) {
	var entries []auditPkgEntry
	for _, line := range auditPkNulRecords(out) {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-tree line %q names no path", line)
		}
		fields := strings.Fields(meta)
		if len(fields) < 4 {
			return nil, fmt.Errorf("git ls-tree line %q is not mode, type, object and size", line)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git ls-tree size %q: %w", fields[3], err)
		}
		entries = append(entries, auditPkgEntry{Path: path, Size: size})
	}
	return entries, nil
}

// auditPkgWriteSrc writes the package files of the head into src/, and returns the listing
// lines of the ones too large to copy.
func auditPkgWriteSrc(ctx context.Context, co auditPkgCheckout, head, dir string, entries []auditPkgEntry) ([]string, error) {
	var large []string
	for _, entry := range entries {
		if entry.Size > auditPkgLargeFileBytes {
			sum := sha256.New()
			if err := auditPkgBlob(ctx, co.Repository, head, entry.Path, sum); err != nil {
				return nil, err
			}
			large = append(large, fmt.Sprintf("%s %d sha256:%s", entry.Path, entry.Size, hex.EncodeToString(sum.Sum(nil))))
			continue
		}
		target := filepath.Join(dir, auditPkgSrcDir, filepath.FromSlash(entry.Path))
		if !auditPkgContained(dir, target) {
			return nil, fmt.Errorf("the head names %q, which leaves the bundle", entry.Path)
		}
		if err := auditPkgWriteBlob(ctx, co, head, entry.Path, target); err != nil {
			return nil, err
		}
	}
	return large, nil
}

// auditPkgWriteBlob writes one file of the head to target.
func auditPkgWriteBlob(ctx context.Context, co auditPkgCheckout, head, path, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := auditPkgBlob(ctx, co.Repository, head, path, f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// auditPkgContained reports whether target is strictly below root, so a path that came out
// of the tree being read can never make the builder write outside the bundle.
func auditPkgContained(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// auditPkgWriteAtHead writes the repository paths' content at the head below dst. A file
// path and a directory path both go through ls-tree, so a path that names a directory
// brings the whole directory.
func auditPkgWriteAtHead(ctx context.Context, co auditPkgCheckout, head string, paths []string, dst string) error {
	for _, path := range paths {
		out, err := auditPkgGit(ctx, co.Repository, "ls-tree", "-r", "--name-only", "-z", head, "--", path)
		if err != nil {
			return err
		}
		for _, name := range auditPkNulRecords(out) {
			target := filepath.Join(dst, filepath.FromSlash(name))
			if !auditPkgContained(filepath.Dir(dst), target) {
				return fmt.Errorf("the head names %q, which leaves the bundle", name)
			}
			if err := auditPkgWriteBlob(ctx, co, head, name, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// auditPkNulRecords splits command output on its NUL terminators, dropping the empty record
// the final terminator leaves.
func auditPkNulRecords(out []byte) []string {
	var records []string
	for _, record := range strings.Split(string(out), "\x00") {
		if record != "" {
			records = append(records, record)
		}
	}
	return records
}

// auditPkgCopySources copies the local read-only port sources into reference/. Two sources
// with the same base name would collide, so the second is refused rather than silently
// replacing the first. A symbolic link is refused too: following one would copy whatever it
// points at, which may be a file outside the configured source entirely.
func auditPkgCopySources(sources []string, dst string) error {
	for _, source := range sources {
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("reference source %s is a symbolic link", source)
		}
		target := filepath.Join(dst, filepath.Base(source))
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("two reference sources share the name %q", filepath.Base(source))
		}
		if !info.IsDir() {
			if err := auditPkgCopyFile(source, target); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("reference source %s is a symbolic link", path)
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, filepath.Base(source), rel)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			return auditPkgCopyFile(path, target)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func auditPkgCopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// auditPkgCriteria merges the criteria of every issue key the source names. A key the
// relay cannot answer is recorded as missing rather than failing the bundle, so the
// grading still proceeds on what could be read.
func auditPkgCriteria(ctx context.Context, e *Env, cfg *Config, source auditPkgSource) ([]map[string]any, []string, error) {
	merged := []map[string]any{}
	missing := []string{}
	for _, issue := range source.Issues {
		raw, err := auditPkgRelayCriteria(ctx, e, cfg, issue)
		if err != nil {
			missing = append(missing, issue)
			continue
		}
		var doc struct {
			Criteria []map[string]any `json:"criteria"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			missing = append(missing, issue)
			continue
		}
		for _, criterion := range doc.Criteria {
			entry := map[string]any{"issue": issue}
			for key, value := range criterion {
				entry[key] = value
			}
			merged = append(merged, entry)
		}
	}
	return merged, missing, nil
}

// auditPkgTask is the task.md the grader reads: what is being audited, which criteria it
// is judged against, and the instruction about defects the known-defects files already
// carry.
func auditPkgTask(pkg, head string, criteria []map[string]any, missing []string) string {
	var out strings.Builder
	out.WriteString("# Package audit\n\n")
	out.WriteString("Package: " + pkg + "\n")
	out.WriteString("Head: " + head + "\n\n")
	out.WriteString("The whole package at this head is under \u0060src/\u0060, tests and testdata included. A file too large to copy is listed in \u0060src/" + auditPkgLargeListName + "\u0060 with its size and sha256 instead.\n\n")
	out.WriteString("## Criteria\n\n")
	if len(criteria) == 0 {
		out.WriteString("No criterion could be read. Judge the package against the task text you were given and say in each note which file or symbol you used.\n\n")
	} else {
		for _, criterion := range criteria {
			id, _ := criterion["id"].(string)
			title, _ := criterion["title"].(string)
			out.WriteString("- " + id + ": " + title + "\n")
		}
		out.WriteString("\n")
	}
	if len(missing) > 0 {
		out.WriteString("These issue keys could not be read and their criteria are missing: " + strings.Join(missing, ", ") + ".\n\n")
	}
	out.WriteString("## Known defects\n\n")
	out.WriteString("The defects this repository already records are under \u0060" + auditPkgKnownDefectsDir + "/\u0060. A defect you find that one of those entries already carries must be marked as an already-known one in its note rather than reported as new.\n")
	return out.String()
}

// auditPkgWriteJSON writes one JSON document with a final newline.
func auditPkgWriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// auditPkgLines splits command output into its non-empty lines.
func auditPkgLines(out []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// auditPkgUsage is the line the package subcommand prints.
const auditPkgUsage = "usage: crw manage audit package --round R [--next N] [--head SHA]"

// auditRunPackage is crw manage audit package.
func auditRunPackage(ctx context.Context, e *Env, args []string) int {
	return auditPkgRun(ctx, e, args)
}

// auditPkgParseArgs reads --name value and --name=value pairs against an allow-list. A
// token that is not an allowed option, an option without a value, or a stray positional is
// an error naming what is wrong.
func auditPkgParseArgs(args []string, allowed map[string]bool) (map[string]string, error) {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		name := args[i]
		if !strings.HasPrefix(name, "--") {
			return nil, fmt.Errorf("unexpected argument %q", name)
		}
		key, value := strings.TrimPrefix(name, "--"), ""
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			key, value = key[:eq], key[eq+1:]
		} else {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, fmt.Errorf("the option %s needs a value", name)
			}
			value = args[i]
		}
		if !allowed[key] {
			return nil, fmt.Errorf("unknown option %s", name)
		}
		values[key] = value
	}
	return values, nil
}

// auditPkgRun is crw manage audit package. It attaches to one round, audits up to N
// packages the round still holds pending or failed, and writes each result back into the
// round file before releasing the lock.
func auditPkgRun(ctx context.Context, e *Env, args []string) int {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			fmt.Fprintln(e.Stdout, auditPkgUsage)
			return 0
		}
	}
	values, err := auditPkgParseArgs(args, map[string]bool{"round": true, "next": true, "head": true})
	round := values["round"]
	if err == nil && round == "" {
		err = errors.New("--round is required")
	}
	next := 1
	if err == nil {
		if raw := values["next"]; raw != "" {
			if next, err = strconv.Atoi(raw); err != nil || next < 0 {
				err = fmt.Errorf("--next %q is not a count", raw)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, auditPkgUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
		return usageExit
	}
	return auditPkgRunOne(ctx, e, round, next, values["head"])
}

// auditPkgRunOne does the work auditPkgRun validated the arguments for.
func auditPkgRunOne(ctx context.Context, e *Env, round string, next int, headArg string) int {
	cfg := coreDefaults(e)
	section, err := auditPkgSectionOf(cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
		return 1
	}
	co, err := auditPkgCheckoutOf(cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
		return 1
	}
	path, err := auditRoundPath(e, cfg, round)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
		return usageExit
	}
	release, err := auditRoundLock(path)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
		return 1
	}
	defer release()
	doc, err := auditRoundLoad(path)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
		return 1
	}
	pending := auditRoundOutstanding(doc)
	if next < len(pending) {
		pending = pending[:next]
	}
	if len(pending) > 0 {
		head, err := auditPkgResolveHead(ctx, co, headArg)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
			return 1
		}
		for _, pkg := range pending {
			// The marker is taken before the bundle is emptied and held until the grade records its
			// row, so this rebuild cannot delete the files of a grade that is running and the grade
			// cannot be drafted while its row is missing.
			bundleDir := filepath.Join(auditPkgBundleRoot(e, cfg, section), auditPkgBundleName(pkg, head))
			mark, markPath, made, err := auditPendingHold(e, cfg, bundleDir)
			if err != nil {
				fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
				return 1
			}
			dir, err := auditPkgBuild(ctx, e, cfg, section, co, pkg, head)
			if err != nil {
				auditPendingDiscard(mark, markPath, made)
				fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
				return 1
			}
			results, err := AuditGrade(ctx, e, cfg, []AuditJob{{Bundle: dir, Round: round, held: mark}})
			if err != nil {
				fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
				return 1
			}
			if len(results) != 1 {
				fmt.Fprintf(e.Stderr, "crw manage audit package: error: the grader answered %d results for one bundle\n", len(results))
				return 1
			}
			auditRoundApply(doc, pkg, head, dir, results[0])
			// The round is saved after each package rather than once at the end: the grade is
			// already in the ledger, and a later package failing must not leave this one
			// pending and ready to be graded and recorded a second time.
			if err := auditRoundSave(path, doc); err != nil {
				fmt.Fprintf(e.Stderr, "crw manage audit package: error: %v\n", err)
				return 1
			}
		}
	}
	return auditRoundWriteStatus(e, doc)
}
