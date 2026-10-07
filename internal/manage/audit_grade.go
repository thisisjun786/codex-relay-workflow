package manage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
		path := auditBundleResolvedPath(job.Bundle)
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
	// Each bundle is marked as carrying a run whose ledger row is not recorded yet, before any
	// grader can leave a file in it. The mark is what a later reader fails closed on, so a run
	// that is killed, or one whose row cannot be appended, leaves a bundle the drafts surface
	// refuses to read a result from rather than one that silently hands an older row another
	// run's defects. A mark that cannot be written stops the batch before anything is graded, and
	// the marks this preflight made are taken back: nothing was graded, so a marker left here
	// would make a later reader distrust a bundle this run never touched. A marker that was
	// already there is kept, because it is the record of a grade that never recorded its row.
	marks := make([]*os.File, len(jobs))
	paths := make([]string, len(jobs))
	fresh := make([]bool, len(jobs))
	for i := range jobs {
		path := auditPendingPath(e, cfg, resolved[i])
		mark, made, err := auditPendingMark(path)
		if err != nil {
			for j := 0; j < i; j++ {
				if marks[j] != nil {
					auditPendingDiscard(marks[j], paths[j], fresh[j])
				}
			}
			return nil, err
		}
		marks[i] = mark
		paths[i] = path
		fresh[i] = made
	}
	results := make([]AuditResult, len(jobs))
	logs := make([]auditLog, len(jobs))
	sem := make(chan struct{}, section.Workers)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = auditGradeOne(ctx, e, section, bundles[i], resolved[i], jobs[i], &logs[i])
		}(i)
	}
	wg.Wait()
	for i := range jobs {
		if results[i].Status != auditStatusOK {
			fmt.Fprintf(e.Stderr, "crw manage audit: %s: %s: %s\n", results[i].Bundle, results[i].Status, auditFirstLine(logs[i].String()))
		}
	}
	// auditRecord appends one ledger row per result in order, so the rows it managed to append are
	// a prefix of the results. The marker is removed for exactly that prefix: a row on disk names its
	// file and its result is complete, while a result whose row is missing keeps its marker and
	// stays unreadable. A recorded result is never thrown away, so a failure that comes after some
	// rows (an alert write, for one) costs only the results it actually lost.
	mark, torn := auditLedgerProgress(e, cfg)
	if err := auditRecord(e, cfg, results); err != nil {
		recorded := auditLedgerRowsRecorded(e, cfg, mark, torn)
		for i := range results {
			if i >= recorded {
				auditPendingUnlock(marks[i])
				continue
			}
			auditPendingClear(e, marks[i], paths[i])
		}
		return nil, err
	}
	for i := range paths {
		auditPendingClear(e, marks[i], paths[i])
	}
	return results, nil
}

// auditLedgerProgress is where a run's own ledger rows begin: the offset just past the last line
// feed, and whether a partial row an earlier writer left sits before it. A row exists once its
// terminating line feed reached the file, so counting line feeds after this mark counts exactly
// the rows this run appended, even when a write stopped in the middle of one.
func auditLedgerProgress(e *Env, cfg *Config) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(auditStateDir(e, cfg), "audit", auditLedgerFile))
	if err != nil {
		return 0, false
	}
	mark := int64(bytes.LastIndexByte(data, '\n') + 1)
	return mark, int64(len(data)) > mark
}

// auditLedgerRowsRecorded counts the ledger rows a run appended after the mark.
func auditLedgerRowsRecorded(e *Env, cfg *Config, mark int64, torn bool) int {
	data, err := os.ReadFile(filepath.Join(auditStateDir(e, cfg), "audit", auditLedgerFile))
	if err != nil || int64(len(data)) < mark {
		return 0
	}
	rows := bytes.Count(data[mark:], []byte{'\n'})
	if torn {
		rows--
	}
	if rows < 0 {
		return 0
	}
	return rows
}

// auditBundleResolvedPath is the directory a bundle really is: the absolute path with every link
// resolved. The ledger records this rather than the spelling the caller passed, so two grades of
// one directory are one bundle to a later reader even after a link used for one of them is gone
// or repointed, and so a reader anywhere names the same directory.
func auditBundleResolvedPath(bundle string) string {
	// The kernel resolves a path component by component, so a link followed by ".." names the
	// link target's parent. The whole spelling is asked for first for that reason: cleaning it
	// would collapse "link/../B" to "work/B" and name a directory the caller's path does not.
	if resolved, err := filepath.EvalSymlinks(bundle); err == nil {
		return auditBundleAbs(resolved)
	}
	// The path does not exist yet, so the kernel has nothing to resolve for its leaf. Its parent
	// is resolved and the leaf rejoined, which answers the same before and after a bundle is
	// built, so a marker taken for a build and the grade that adopts it name one file.
	cleaned := filepath.Clean(bundle)
	parent, err := filepath.EvalSymlinks(filepath.Dir(cleaned))
	if err != nil {
		return auditBundleAbs(bundle)
	}
	return filepath.Join(parent, filepath.Base(cleaned))
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
	sum := sha256.Sum256([]byte(auditBundleResolvedPath(bundle)))
	return filepath.Join(auditStateDir(e, cfg), "audit", auditPendingDir, hex.EncodeToString(sum[:]))
}

// auditPendingMark creates or opens a bundle's marker and holds an exclusive lock on it for the
// whole grade, reporting whether this call created it. The file stays behind when a run cannot
// record its row, so a later reader can tell that the bundle's grade.json belongs to a run nothing
// names; the lock is what tells a builder that a run is in flight right now. The open refuses to
// follow a symbolic link, so a link planted at the marker path cannot redirect this write.
func auditPendingMark(path string) (*os.File, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, fmt.Errorf("bundle_locked: %s is being graded", path)
		}
		return nil, false, err
	}
	return f, made, nil
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

// auditPendingDiscard takes back a marker this run created for a bundle it then never graded, so a
// batch refused before any worker started leaves no bundle looking unrecorded. A marker that was
// already there is kept: it is the record of an earlier run that could not record its row.
func auditPendingDiscard(f *os.File, path string, fresh bool) {
	if f == nil {
		return
	}
	if fresh {
		_ = os.Remove(path)
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
		Bundle: absBundle, GradedAt: e.Now().UTC().Format(auditTimeFormat),
	}
	grade := filepath.Join(absBundle, auditGradeFile)
	// The grader is told to write this file, so a file an earlier run left is not this
	// run's result and must not be read as one.
	if err := os.Remove(grade); err != nil && !errors.Is(err, os.ErrNotExist) {
		result.Status = auditStatusInvalid
		return result
	}
	prompt := filepath.Join(absBundle, auditPromptFile)
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
