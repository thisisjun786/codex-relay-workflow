package manage

import (
	"bytes"
	"context"
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
	section, err := auditConfigOf(cfg)
	if err != nil {
		return nil, err
	}
	if len(section.Grader) == 0 {
		return nil, auditGraderUnconfiguredError{}
	}
	bundles := make([]*auditBundle, len(jobs))
	seen := make([]auditBundleID, 0, len(jobs))
	for i, job := range jobs {
		bundle, err := auditReadBundle(job.Bundle)
		if err != nil {
			return nil, err
		}
		// Two jobs naming one bundle would race over the same grade.json and the same
		// prompt, so the second is refused rather than silently sharing the directory. The
		// spelling does not decide that: a link to a bundle, a relative form and a trailing
		// separator all name the one directory the jobs would write into, so the same
		// resolution the drafts surface groups ledger rows by is used here too.
		key := auditBundleIDOf(job.Bundle)
		for first, have := range seen {
			if auditBundleIDSame(have, key) {
				return nil, fmt.Errorf("jobs %d and %d name the same bundle %s", first, i, job.Bundle)
			}
		}
		seen = append(seen, key)
		bundles[i] = bundle
	}
	// The grade file and the ledger row that names it are one record: this run replaces
	// grade.json and then appends the row. The drafts surface reads that pair, the ledger
	// first and then the file, so the two must not interleave: the lock the drafts surface
	// holds is taken here for the whole grade, and a concurrent grade or drafts run is
	// refused by name rather than allowed to read a half-recorded pair.
	release, err := auditDraftLock(e, cfg)
	if err != nil {
		return nil, err
	}
	defer release()
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
			results[i] = auditGradeOne(ctx, e, section, bundles[i], jobs[i], &logs[i])
		}(i)
	}
	wg.Wait()
	for i := range jobs {
		if results[i].Status != auditStatusOK {
			fmt.Fprintf(e.Stderr, "crw manage audit: %s: %s: %s\n", results[i].Bundle, results[i].Status, auditFirstLine(logs[i].String()))
		}
	}
	if err := auditRecord(e, cfg, results); err != nil {
		// The grade file and the ledger row that names it are one record, and this run has
		// already replaced the file. A row that was never recorded must not leave a file an
		// older ok row of the same bundle would be drafted from, so the replacement is taken
		// back and the bundle reports no usable grade at all: failing to record is not a
		// result, and a later reader fails closed rather than reading another run's defects.
		for i := range results {
			auditDiscardUnrecordedGrade(e, results[i].Bundle)
		}
		return nil, err
	}
	return results, nil
}

// auditDiscardUnrecordedGrade removes the grade file a run that could not record its ledger
// row left in a bundle, so the file and the row stay one record. A bundle that is already gone
// or carries no file is left alone, and a removal that fails is reported on stderr rather than
// silently swallowed: the caller has already failed, and a stale file is the one state a later
// reader must not be able to draft from.
func auditDiscardUnrecordedGrade(e *Env, bundle string) {
	if bundle == "" {
		return
	}
	if err := os.Remove(filepath.Join(bundle, auditGradeFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(e.Stderr, "crw manage audit: %s: %s could not be discarded: %v\n", bundle, auditGradeFile, err)
	}
}

// auditGradeOne writes the prompt into the bundle, runs the grader there under the time
// limit, and reads what it wrote.
func auditGradeOne(ctx context.Context, e *Env, section auditSection, bundle *auditBundle, job AuditJob, log *auditLog) AuditResult {
	// The grader's own paths are absolute, because the grader runs with the bundle as its
	// working directory and a relative argument would be resolved against it twice. The
	// ledger records that same absolute directory, not the spelling the caller passed: a
	// later reader (audit drafts, for one) resolves the row's bundle from its own working
	// directory, and a relative spelling would name a different directory there, so two
	// grades of one bundle would read as two bundles.
	absBundle, err := filepath.Abs(job.Bundle)
	result := AuditResult{
		Mode: bundle.Mode, Subject: bundle.Subject, Head: bundle.Head, Issue: bundle.Issue,
		Pair: job.Pair, Phase: job.Phase, Round: job.Round,
		Bundle: absBundle, GradedAt: e.Now().UTC().Format(auditTimeFormat),
	}
	if err != nil {
		result.Bundle = job.Bundle
		result.Status = auditStatusInvalid
		return result
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
