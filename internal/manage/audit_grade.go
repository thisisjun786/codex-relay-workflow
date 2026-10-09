package manage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// auditGraderUnconfiguredError is what AuditGrade reports when the audit section carries no
// grader command; the command line turns it into exit 2. It is a type rather than a sentinel
// variable because every package-level name this issue adds starts with audit, and the
// err-prefixed variable name staticcheck reserves for a sentinel would break that.
type auditGraderUnconfiguredError struct{}

func (auditGraderUnconfiguredError) Error() string { return "grader_unconfigured" }

// auditSection is the audit section of the configuration: the grader command, how long
// one run may take and how many run at once. The product carries no default grader,
// because a model name in it would name one of the pairs the audit compares.
type auditSection struct {
	Grader               []string `json:"grader"`
	GraderTimeoutSeconds int      `json:"grader_timeout_seconds"`
	Workers              int      `json:"workers"`
}

// The values the issue fixes for a setting the configuration leaves out.
const (
	auditDefaultTimeoutSeconds = 2700
	auditDefaultWorkers        = 3
)

// auditTimeFormat is how a graded result and a round carry their timestamps: UTC in
// RFC 3339, so a ledger row and a round file are read the same way anywhere.
const auditTimeFormat = time.RFC3339

// auditLogLimit is how much of one grader's output is kept: the first bytes, so a grader
// that writes without end cannot grow the audit's memory, and the diagnostic line the
// operator reads still comes from the head of what it said.
const auditLogLimit = 8192

// auditLog keeps the first auditLogLimit bytes written to it and discards the rest, so a
// noisy grader neither fills memory nor blocks on a full pipe.
type auditLog struct{ buf bytes.Buffer }

func (l *auditLog) Write(p []byte) (int, error) {
	if room := auditLogLimit - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (l *auditLog) String() string { return l.buf.String() }

// auditWriteNewFile writes a fresh file at path, refusing to follow anything already there.
// A bundle can come from another process, so a symbolic link left in place of one of the
// files this product writes is never followed into the file it points at.
func auditWriteNewFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// auditConfigOf reads the audit section, filling the defaults.
func auditConfigOf(cfg *Config) (auditSection, error) {
	section := auditSection{GraderTimeoutSeconds: auditDefaultTimeoutSeconds, Workers: auditDefaultWorkers}
	if cfg != nil {
		if err := cfg.Section("audit", &section); err != nil {
			return auditSection{}, err
		}
	}
	if section.GraderTimeoutSeconds <= 0 {
		section.GraderTimeoutSeconds = auditDefaultTimeoutSeconds
	}
	if section.Workers <= 0 {
		section.Workers = auditDefaultWorkers
	}
	return section, nil
}

// AuditGrade grades every job against the grader its bundle names and records each result
// in the ledger, with one alert line for every result that carries a P0 or a P1. At most
// workers jobs run at once, and the results come back in the order of the jobs.
//
// A bundle that cannot be read is an error for the whole call. A grader that left no
// usable grade.json, or ran past grader_timeout_seconds, is a recorded result rather than
// an error, because the mode issues read the ledger, not this call's error.
func AuditGrade(ctx context.Context, e *Env, cfg *Config, jobs []AuditJob) ([]AuditResult, error) {
	// The grade file and the ledger row that names it are one record: this run replaces grade.json
	// and then appends the row, and the drafts surface reads that pair, the ledger first and then
	// the file. The lock is taken here for the whole grade, and a concurrent grade or drafts run is
	// refused by name rather than allowed to read a half-recorded pair. A caller that also rebuilds
	// the bundle takes the same lock around its build (auditPkgBuildAndGrade,
	// auditPRBuildAndGrade), so a rebuild can never empty a bundle a grade is reading or writing.
	release, err := auditDraftLock(e, cfg)
	if err != nil {
		return nil, err
	}
	defer release()
	return auditGradeLocked(ctx, e, cfg, jobs)
}

// auditGradeLocked grades the jobs with the drafts lock already held, so a caller that must keep a
// bundle from being rebuilt between its own build and this grade takes the lock once around both.
// AuditGrade is the entry point everything else uses; this one never takes or releases the lock.
func auditGradeLocked(ctx context.Context, e *Env, cfg *Config, jobs []AuditJob) ([]AuditResult, error) {
	section, err := auditConfigOf(cfg)
	if err != nil {
		return nil, err
	}
	if len(section.Grader) == 0 {
		return nil, auditGraderUnconfiguredError{}
	}
	bundles := make([]*auditBundle, len(jobs))
	resolved := make([]string, len(jobs))
	seen := make(map[string]int, len(jobs))
	for i, job := range jobs {
		// The path is resolved first and the bundle read from what it resolved to, so the files
		// this run reads and writes are the ones the caller's path really names. Resolving a
		// spelling with the kernel (EvalSymlinks) and only then using it is what makes
		// "link/../B" name the directory the shell would, rather than one a lexical clean picks.
		// A path the kernel cannot resolve is refused rather than lexically cleaned: cleaning
		// collapses "link/../B" to "work/B" and would grade a directory the caller's spelling does
		// not name. A caller that names a bundle through a link that does not exist yet has to
		// create it first.
		path, err := auditBundleResolvedPath(job.Bundle)
		if err != nil {
			return nil, err
		}
		bundle, err := auditReadBundle(path)
		if err != nil {
			return nil, err
		}
		// Two jobs naming one bundle would race over the same grade.json and the same
		// prompt, so the second is refused rather than silently sharing the directory. The
		// spelling does not decide that: the path is resolved to the directory it is, so a
		// link, a relative form and a trailing separator are one job target.
		if first, ok := seen[path]; ok {
			return nil, fmt.Errorf("jobs %d and %d name the same bundle %s", first, i, job.Bundle)
		}
		seen[path] = i
		bundles[i] = bundle
		resolved[i] = path
	}
	// Each bundle is marked as carrying a run whose ledger row is not recorded yet, from the moment
	// its own worker starts and before that grader can leave a file in it. The mark is what a later
	// reader fails closed on, so a run that is killed, or one whose row cannot be appended, leaves a
	// bundle the drafts surface refuses to read a result from rather than one that silently hands an
	// older row another run's defects. The mark is taken per worker and not in a batch preflight: a
	// job still waiting for a worker slot has not touched its bundle, and marking it would make a
	// killed batch leave bundles this run never graded looking unrecorded.
	marks := make([]*os.File, len(jobs))
	paths := make([]string, len(jobs))
	var markMu sync.Mutex
	var markErr error
	results := make([]AuditResult, len(jobs))
	logs := make([]auditLog, len(jobs))
	sem := make(chan struct{}, section.Workers)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		// The worker checks the refusal again after it has a slot and immediately before it grades:
		// a job that waited for a slot while an earlier marker failed must not start, because a
		// bundle this run never touched has to keep the result it already had.
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			markMu.Lock()
			stopped := markErr != nil
			markMu.Unlock()
			if stopped {
				return
			}
			path := auditPendingPath(e, cfg, resolved[i])
			mark, _, err := auditPendingMark(path)
			if err != nil {
				// This bundle is not graded at all. Nothing is recorded for it, and the batch is
				// refused once the workers stop: recording a row for a run that never started would
				// void a bundle this call never touched.
				markMu.Lock()
				if markErr == nil {
					markErr = err
				}
				markMu.Unlock()
				return
			}
			marks[i], paths[i] = mark, path
			results[i] = auditGradeOne(ctx, e, section, bundles[i], resolved[i], jobs[i], &logs[i])
		}(i)
	}
	wg.Wait()
	for i := range jobs {
		if results[i].Status != auditStatusOK {
			fmt.Fprintf(e.Stderr, "crw manage audit: %s: %s: %s\n", results[i].Bundle, results[i].Status, auditFirstLine(logs[i].String()))
		}
	}
	// auditRecord appends one ledger row per result in order and reports how many it appended, so
	// the rows on disk are a prefix of the results and the count comes from the writer rather than
	// a re-read that may itself fail. The marker is removed for exactly that prefix: a row on disk
	// names its file and its result is complete, while a result whose row is missing keeps its
	// marker and stays unreadable. A recorded result is never thrown away, so a failure that comes
	// after some rows (an alert write, for one) costs only the results it actually lost.
	// A job whose marker could not be taken is not graded, so it has no result to record; the jobs
	// that did grade are still recorded, because their rows are real results and leaving them
	// unrecorded would take back markers from bundles that ran. The refusal is reported after that.
	graded := make([]AuditResult, 0, len(results))
	gradedAt := make([]int, 0, len(results))
	for i := range results {
		if marks[i] == nil {
			continue
		}
		graded = append(graded, results[i])
		gradedAt = append(gradedAt, i)
	}
	recorded, err := auditRecord(e, cfg, graded)
	if err == nil && markErr != nil {
		err = markErr
	}
	if err != nil {
		for n, i := range gradedAt {
			if n >= recorded {
				auditPendingUnlock(marks[i])
				continue
			}
			auditPendingClear(e, marks[i], paths[i])
		}
		return nil, err
	}
	for i := range paths {
		if marks[i] == nil {
			continue
		}
		auditPendingClear(e, marks[i], paths[i])
	}
	return results, nil
}

// auditBundleResolvedPath is the directory a bundle really is: the absolute path with every link
// resolved. The ledger records this rather than the spelling the caller passed, so two grades of
// one directory are one bundle to a later reader even after a link used for one of them is gone
// or repointed, and so a reader anywhere names the same directory.
func auditBundleResolvedPath(bundle string) (string, error) {
	// The empty spelling is refused here as the bundle reader refuses it, so resolving a caller's
	// path can never turn "no bundle named" into a directory (the current one, for instance) that
	// then gets graded.
	if bundle == "" {
		return "", errors.New("the bundle directory is empty")
	}
	// A relative spelling is made absolute by joining the working directory without cleaning it.
	// The kernel then resolves the logical prefix of that directory too, so a working directory
	// reached through a symbolic link names the real directory and not the link.
	if !filepath.IsAbs(bundle) {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("bundle %s: the working directory cannot be read: %w", bundle, err)
		}
		bundle = wd + string(filepath.Separator) + bundle
	}
	// The kernel resolves a path component by component, so a link followed by ".." names the
	// link target's parent. The whole spelling is asked for first for that reason: cleaning it
	// would collapse "link/../B" to "work/B" and name a directory the caller's path does not.
	resolved, err := filepath.EvalSymlinks(bundle)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	// Only a missing leaf is resolved from its parent. Any other failure, such as a component
	// that is not a directory, is refused: the spelling then names nothing this run may grade.
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("bundle %s: the path cannot be resolved: %w", bundle, err)
	}
	// The path does not exist yet, so the kernel has nothing to resolve for its leaf. Its parent
	// is resolved and the leaf rejoined, which answers the same before and after a bundle is
	// built, so a marker taken for a build and the grade that adopts it name one file.
	//
	// The parent is taken from the spelling as written, not from its cleaned form: filepath.Dir
	// cleans, and cleaning "link/../B" to "work/B" would resolve the wrong directory and could
	// substitute an unrelated bundle for one whose real target is gone.
	parent, leaf := auditBundleSplitParent(bundle)
	if leaf == "" || leaf == "." || leaf == ".." {
		return "", fmt.Errorf("bundle %s: the last element names no directory", bundle)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("bundle %s: the path cannot be resolved: %w", bundle, err)
	}
	info, err := os.Stat(resolvedParent)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("bundle %s: its parent is not a directory", bundle)
	}
	return crwconfig.JoinRoot(resolvedParent, leaf), nil
}

// auditBundleSplitParent splits a path into the part before its last separator and the last
// element, without cleaning either: filepath.Dir and filepath.Base both clean, and a cleaned
// parent resolves a link-and-".." spelling to a different directory than the kernel would.
func auditBundleSplitParent(path string) (string, string) {
	if i := strings.LastIndexByte(path, filepath.Separator); i >= 0 {
		if i == 0 {
			return string(filepath.Separator), path[1:]
		}
		return path[:i], path[i+1:]
	}
	return ".", path
}

// auditBundleAbs is a path's absolute form, or its cleaned form when the working directory itself
// cannot be read.
func auditBundleAbs(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// auditPendingDir is where a grade's in-flight markers live, below the audit state directory.
const auditPendingDir = "pending"

// auditPendingPath is where the marker for one bundle lives. It is kept beside the ledger rather
// than inside the bundle, named after the directory the bundle resolves to: the bundle is a
// directory this product did not create and may hold a link planted by another process, so the
// marker is never opened through a path the bundle controls, and a reader that has only the
// ledger row still finds it.
func auditPendingPath(e *Env, cfg *Config, bundle string) string {
	sum := sha256.Sum256([]byte(auditBundleIdentity(bundle)))
	return crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", auditPendingDir, hex.EncodeToString(sum[:]))
}

// auditBundleIdentity is the name a marker is keyed by: the directory the kernel resolves the
// spelling to when it can, and the cleaned absolute spelling when it cannot. A marker is keyed
// from a ledger row, and a row names a bundle that exists or once did, so the resolved form is
// what a grade of that row will look for; the fallback keeps a row whose bundle is gone from
// keying every such row to one name.
func auditBundleIdentity(bundle string) string {
	if resolved, err := auditBundleResolvedPath(bundle); err == nil {
		return resolved
	}
	return auditBundleAbs(bundle)
}

// auditPendingMark creates or opens a bundle's marker and holds an exclusive lock on it for the
// whole grade, reporting whether this call created it. The file stays behind when a run cannot
// record its row, so a later reader can tell that the bundle's grade.json belongs to a run nothing
// names; the lock is what tells a builder that a run is in flight right now. The open refuses to
// follow a symbolic link, so a link planted at the marker path cannot redirect this write.
func auditPendingMark(path string) (*os.File, bool, error) {
	if err := os.MkdirAll(rootDir(path), 0o700); err != nil {
		return nil, false, err
	}
	made := false
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		made = true
	} else if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	}
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return auditPendingLockFailed(f, path, made, err)
	}
	return f, made, nil
}

// auditPendingLockFailed closes a marker descriptor whose lock could not be taken, and removes the
// file when this call created it: nothing was graded under a marker this call made, and leaving it
// would make a later reader distrust a bundle this run never touched.
func auditPendingLockFailed(f *os.File, path string, made bool, err error) (*os.File, bool, error) {
	_ = f.Close()
	if made {
		_ = os.Remove(path)
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, fmt.Errorf("bundle_locked: %s is being graded", path)
	}
	return nil, false, err
}

// auditPendingUnlock gives up the marker's lock and keeps the file, so the bundle stays marked as
// carrying a grade no ledger row names.
func auditPendingUnlock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// auditPendingClear removes the marker once the ledger row that names the bundle's file is on
// disk. A marker that cannot be removed is reported rather than swallowed: the bundle then stays
// unusable to the drafts surface, which is the fail-closed direction, and a later run that
// records clears it.
func auditPendingClear(e *Env, f *os.File, path string) {
	if f == nil {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(e.Stderr, "crw manage audit: %s: the in-flight marker could not be cleared: %v\n", path, err)
	}
	auditPendingUnlock(f)
}

// auditPending reports whether a bundle carries the marker of a run whose ledger row was never
// recorded, so the drafts surface reads no result from it: the file it holds belongs to a run
// nothing names, and attributing it to an older ok row would report defects that row never found.
// A grade holds the drafts lock for its whole run, so a reader never sees a live run's marker,
// only the one a run that died left behind.
func auditPending(e *Env, cfg *Config, bundle string) bool {
	if bundle == "" {
		return false
	}
	_, err := os.Stat(auditPendingPath(e, cfg, bundle))
	if err == nil {
		return true
	}
	// Only a marker that is certainly absent lets a result be read. A marker that cannot be
	// inspected at all (a permission or I/O error on the marker store) is treated as present: the
	// file it guards may belong to a run nothing names, and reading it would report defects an
	// older row never found.
	return !errors.Is(err, os.ErrNotExist)
}

// auditGradeOne writes the prompt into the bundle, runs the grader there under the time
// limit, and reads what it wrote.
func auditGradeOne(ctx context.Context, e *Env, section auditSection, bundle *auditBundle, absBundle string, job AuditJob, log *auditLog) AuditResult {
	// The grader's own paths are absolute, because the grader runs with the bundle as its
	// working directory and a relative argument would be resolved against it twice, and the
	// ledger records that same resolved directory, not the spelling the caller passed, so two
	// grades of one bundle read as one bundle wherever a later reader runs.
	result := AuditResult{
		Mode: bundle.Mode, Subject: bundle.Subject, Head: bundle.Head, Issue: bundle.Issue,
		Pair: job.Pair, Phase: job.Phase, Round: job.Round,
		Bundle: absBundle, BundleGiven: job.Bundle, GradedAt: e.Now().UTC().Format(auditTimeFormat),
	}
	grade := crwconfig.JoinRoot(absBundle, auditGradeFile)
	// The grader is told to write this file, so a file an earlier run left is not this
	// run's result and must not be read as one.
	if err := os.Remove(grade); err != nil && !errors.Is(err, os.ErrNotExist) {
		result.Status = auditStatusInvalid
		return result
	}
	prompt := crwconfig.JoinRoot(absBundle, auditPromptFile)
	// A bundle can come from another process, so an existing prompt.md is removed first and
	// the new one is created exclusively: a symlink left there is never followed into
	// whatever file it points at.
	if err := os.Remove(prompt); err != nil && !errors.Is(err, os.ErrNotExist) {
		result.Status = auditStatusInvalid
		return result
	}
	if err := auditWriteNewFile(prompt, []byte(auditPrompt(bundle)), 0o644); err != nil {
		result.Status = auditStatusInvalid
		return result
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(section.GraderTimeoutSeconds)*time.Second)
	defer cancel()
	argv := auditGraderArgv(section.Grader, prompt, absBundle)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = absBundle
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The grader may be a wrapper with children of its own, so the whole group goes.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// The outcome comes from grade.json and the time limit, not from the exit status: a
	// grader that failed leaves no usable file, and one that reports is done.
	_ = cmd.Run()
	// The time limit wins over the file: a grader that wrote a usable document and then hung
	// did not finish inside its limit, and recording it as ok would score an unfinished run.
	if runCtx.Err() != nil {
		result.Status = auditStatusTimeout
		return result
	}
	result.Status = auditStatusInvalid
	doc, ok := auditParseResult(grade)
	if !ok {
		return result
	}
	result.Status = auditStatusOK
	result.Score = *doc.Score
	result.Criteria = make([]AuditCriterion, 0, len(doc.Criteria))
	for _, criterion := range doc.Criteria {
		result.Criteria = append(result.Criteria, AuditCriterion(criterion))
	}
	result.Defects = doc.Defects
	return result
}

// auditGraderArgv fills the placeholders of the configured grader command. The prompt
// file lives in the bundle, and the bundle is also the grader's working directory.
func auditGraderArgv(grader []string, prompt, bundle string) []string {
	argv := make([]string, len(grader))
	for i, arg := range grader {
		argv[i] = strings.ReplaceAll(strings.ReplaceAll(arg, "{prompt_file}", prompt), "{bundle}", bundle)
	}
	return argv
}

// auditGradeDoc is the document a grader must write, in the shape crw-audit-result/1 fixes.
type auditGradeDoc struct {
	Schema   string                `json:"schema"`
	Criteria []auditGradeCriterion `json:"criteria"`
	Defects  []AuditDefect         `json:"defects"`
	Score    *int                  `json:"score"`
}

type auditGradeCriterion struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
}

// The verdicts and severities the result format allows.
var (
	auditVerdicts   = map[string]bool{"PASS": true, "PARTIAL": true, "FAIL": true}
	auditSeverities = map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}
)

// auditParseResult reads a grader's grade.json. A missing file, unreadable JSON, a schema
// that is not crw-audit-result/1, an unknown verdict or severity, or a score outside 0 to
// 10 is not a result: it reports false and the run is recorded as invalid.
func auditParseResult(path string) (auditGradeDoc, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return auditGradeDoc{}, false
	}
	return auditParseResultBytes(data)
}

// auditParseResultBytes is auditParseResult over the bytes of a grade.json already read, so a
// reader of a ledger row's copy judges the copy's bytes by the same rules.
func auditParseResultBytes(data []byte) (auditGradeDoc, bool) {
	var doc auditGradeDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return auditGradeDoc{}, false
	}
	if doc.Schema != auditResultSchema || doc.Score == nil || *doc.Score < 0 || *doc.Score > 10 {
		return auditGradeDoc{}, false
	}
	for _, criterion := range doc.Criteria {
		if criterion.ID == "" || !auditVerdicts[criterion.Verdict] {
			return auditGradeDoc{}, false
		}
	}
	for _, defect := range doc.Defects {
		if !auditSeverities[defect.Severity] {
			return auditGradeDoc{}, false
		}
	}
	return doc, true
}

// auditFirstLine is the first non-empty line of a grader's output, so one diagnostic line
// per failed run reaches the operator without the whole transcript.
func auditFirstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "(no output)"
}
