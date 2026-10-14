// Package reviewadapter runs a read-only review tool once on a worktree and turns what it printed into one record, the unit check
// a lane keeps (Result). The tool is a profile (profiles.go): its argv, the model and effort it runs, the texts that mean its quota
// is spent, and the reader that turns its stdout into a document of AnswerSchema. The adapter never judges the change itself; it
// only says whether the tool gave a judgment and, when it did, what the judgment was, in one vocabulary for every tool.
//
// The state of a record is decided in this order (classify); the first rule that holds wins:
//
//	worktree_changed  the worktree's fingerprint after the call differs from the one before: the answer is refused whatever it is
//	tool_failed       the tool did not start, ran past the time limit, or the call broke after the start (output over the cap)
//	quota             a quota text of the profile (any case) is on stderr or stdout of a call that failed or gave a blank answer,
//	                  or in the text of an error the tool reported in its own output (the profile's reader gave a *ToolError)
//	tool_failed       the tool exited non-zero without a quota text, or reported its own error without one
//	no_answer         the tool exited 0 and its answer is blank
//	invalid           the answer is not exactly one JSON value valid against the request's schema, has a field the answer does not
//	                  know, a severity no grade word reads, or an impact outside premerge.Impacts, or its verdict is verified over a
//	                  finding that premerge.Blocks (the adapter's form of verdict_invalid)
//	ok                otherwise: verdict and findings are kept
//
// An answer that merely quotes a quota text on a successful call is not quota. Only an ok record carries a verdict and findings;
// every record names the engine family, model, effort, guide revision, prompt digest and both fingerprints. Findings are graded
// P0-P3 (review.NormalizeGrade) and carry an id that ignores the line (FindingID); findings of one id collapse into one.
//
// The adapter reaches the outside only through its seams (Run, Fingerprint), so it imports no store and no flow op.
package reviewadapter

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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// State is what a call gave.
type State string

// The states of a record.
const (
	StateOK              State = "ok"
	StateNoAnswer        State = "no_answer"
	StateQuota           State = "quota"
	StateInvalid         State = "invalid"
	StateWorktreeChanged State = "worktree_changed"
	StateToolFailed      State = "tool_failed"
)

// The verdicts of an ok record.
const (
	VerdictVerified     = "verified"
	VerdictNeedsChanges = "needs_changes"
)

// AnswerSchema is the adapter's own answer schema: a verdict and the findings. A profile's Answer yields a document of it, and a
// caller passes it as Request.Schema.
var AnswerSchema = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict", "findings"],
  "properties": {
    "verdict": {"type": "string", "enum": ["verified", "needs_changes"]},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["severity", "file", "line", "text"],
        "properties": {
          "severity": {"type": "string"},
          "impact": {"type": "string"},
          "file": {"type": "string"},
          "line": {"type": "integer", "minimum": 0},
          "text": {"type": "string"}
        }
      }
    }
  }
}`)

// Request is one unit check.
type Request struct {
	Profile  Profile
	Worktree string        // the directory the tool runs in and the fingerprint covers
	Prompt   string        // the review prompt; the exec runner appends it to the argv
	Schema   []byte        // the JSON Schema the answer must satisfy (AnswerSchema)
	Limit    time.Duration // the time limit of the call
	GuideRev string        // the revision of the review guide the prompt was built from
}

// Finding is one finding of an ok record. Severity is a grade P0-P3; Impact is one of premerge.Impacts or empty (a security
// finding that names none gets safety_or_data).
type Finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Impact   string `json:"impact"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Text     string `json:"text"`
}

// Result is the record a unit check leaves.
type Result struct {
	State             State     `json:"state"`
	Detail            string    `json:"detail"`
	Verdict           string    `json:"verdict"`
	Findings          []Finding `json:"findings"`
	EngineFamily      string    `json:"engine_family"`
	Model             string    `json:"model"`
	Effort            string    `json:"effort"`
	GuideRev          string    `json:"guide_rev"`
	PromptDigest      string    `json:"prompt_digest"`
	FingerprintBefore string    `json:"fingerprint_before"`
	FingerprintAfter  string    `json:"fingerprint_after"`
	ElapsedMS         int64     `json:"elapsed_ms"`
}

// RunFunc runs the profile's tool once under ctx (which carries the time limit). err is set when the tool did not start (wrapping
// ErrNotStarted), ctx ended it, or the call broke after the start (its output over the cap); otherwise exit is the tool's exit code.
type RunFunc func(ctx context.Context, p Profile, req Request) (stdout, stderr []byte, exit int, err error)

// FingerprintFunc digests the state of a worktree (a lane passes ops.Snapshot).
type FingerprintFunc func(ctx context.Context, dir string) (string, error)

// Adapter holds the seams of a unit check. Now defaults to time.Now.
type Adapter struct {
	Run         RunFunc
	Fingerprint FingerprintFunc
	Now         func() time.Time
}

// ErrNotStarted is wrapped by a RunFunc's error when the tool did not start.
var ErrNotStarted = errors.New("the tool did not start")

// ToolError is the error a profile's answer reader returns when the tool's output is the tool's own report of a failure (an error
// envelope) rather than an answer; Text is that report. classify reads it as quota when a quota text is in it, else tool_failed.
type ToolError struct{ Text string }

func (e *ToolError) Error() string {
	if strings.TrimSpace(e.Text) == "" {
		return "the tool reported an error with no text"
	}
	return "the tool reported an error: " + firstLine([]byte(e.Text))
}

// call is what one run of the tool gave.
type call struct {
	stdout, stderr []byte
	exit           int
	err            error
	timedOut       bool
	changed        bool
}

// Check validates the request, runs the tool between two fingerprints of the worktree, and returns the record. A Go error means
// the request or the adapter is wrong, a fingerprint could not be taken, or the caller's ctx ended; it is never a state.
func (a Adapter) Check(ctx context.Context, req Request) (Result, error) {
	schema, err := a.validate(req)
	if err != nil {
		return Result{}, err
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	res := Result{Model: req.Profile.Model, Effort: req.Profile.Effort, EngineFamily: req.Profile.Family, GuideRev: req.GuideRev, PromptDigest: hex.EncodeToString(sum[:])}
	if res.EngineFamily == "" {
		res.EngineFamily = execution.ModelFamily(res.Model)
	}
	if res.FingerprintBefore, err = a.Fingerprint(ctx, req.Worktree); err != nil {
		return Result{}, fmt.Errorf("reviewadapter: fingerprint before the call: %w", err)
	}
	start := now()
	rctx, cancel := context.WithTimeout(ctx, req.Limit)
	var c call
	c.stdout, c.stderr, c.exit, c.err = a.Run(rctx, req.Profile, req)
	c.timedOut = c.err != nil && (errors.Is(c.err, context.DeadlineExceeded) || errors.Is(rctx.Err(), context.DeadlineExceeded))
	cancel()
	res.ElapsedMS = now().Sub(start).Milliseconds()
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("reviewadapter: the caller ended the call: %w", err)
	}
	if res.FingerprintAfter, err = a.Fingerprint(ctx, req.Worktree); err != nil {
		return Result{}, fmt.Errorf("reviewadapter: fingerprint after the call: %w", err)
	}
	c.changed = res.FingerprintAfter != res.FingerprintBefore
	res.State, res.Detail, res.Verdict, res.Findings = classify(req, schema, c)
	return res, nil
}

// validate refuses a request the adapter cannot run (D4) and resolves its schema.
func (a Adapter) validate(req Request) (*jsonschema.Resolved, error) {
	p := req.Profile
	switch {
	case a.Run == nil:
		return nil, errors.New("reviewadapter: the adapter has no Run")
	case a.Fingerprint == nil:
		return nil, errors.New("reviewadapter: the adapter has no Fingerprint")
	case strings.TrimSpace(req.Worktree) == "":
		return nil, errors.New("reviewadapter: the request names no worktree")
	case len(bytes.TrimSpace(req.Schema)) == 0:
		return nil, errors.New("reviewadapter: the request has no schema")
	case strings.TrimSpace(req.GuideRev) == "":
		return nil, errors.New("reviewadapter: the request names no guide revision")
	case req.Limit <= 0:
		return nil, errors.New("reviewadapter: the request has no time limit")
	case strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.Effort) == "":
		return nil, fmt.Errorf("reviewadapter: profile %q names no model or no effort", p.Name)
	case p.Answer == nil:
		return nil, fmt.Errorf("reviewadapter: profile %q has no answer reader", p.Name)
	}
	var sch jsonschema.Schema
	if err := json.Unmarshal(req.Schema, &sch); err != nil {
		return nil, fmt.Errorf("reviewadapter: the request's schema: %w", err)
	}
	r, err := sch.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("reviewadapter: the request's schema: %w", err)
	}
	return r, nil
}

// classify decides the state of one call in the order of the package doc, and for ok the verdict and findings.
func classify(req Request, schema *jsonschema.Resolved, c call) (State, string, string, []Finding) {
	if c.changed {
		return StateWorktreeChanged, "the worktree changed during the call; the answer is refused", "", nil
	}
	if c.err != nil {
		if c.timedOut {
			return StateToolFailed, fmt.Sprintf("the tool ran past the time limit of %s", req.Limit), "", nil
		}
		if errors.Is(c.err, ErrNotStarted) {
			return StateToolFailed, c.err.Error(), "", nil
		}
		return StateToolFailed, fmt.Sprintf("the tool call broke after the tool started: %v", c.err), "", nil
	}
	var answer []byte
	var answerErr error
	blank := len(bytes.TrimSpace(c.stdout)) == 0
	if !blank && c.exit == 0 {
		answer, answerErr = req.Profile.Answer(c.stdout)
		blank = answerErr == nil && len(bytes.TrimSpace(answer)) == 0
	}
	if c.exit != 0 || blank {
		if text, ok := quotaText(req.Profile.QuotaTexts, c.stderr, c.stdout); ok {
			return StateQuota, fmt.Sprintf("the tool reports its quota spent (%q)", text), "", nil
		}
	}
	if toolErr, ok := errors.AsType[*ToolError](answerErr); ok {
		if text, ok := quotaText(req.Profile.QuotaTexts, []byte(toolErr.Text), c.stderr); ok {
			return StateQuota, fmt.Sprintf("the tool reports its quota spent (%q)", text), "", nil
		}
		return StateToolFailed, toolErr.Error(), "", nil
	}
	if c.exit != 0 {
		return StateToolFailed, fmt.Sprintf("the tool failed with exit %d: %s", c.exit, firstLine(c.stderr)), "", nil
	}
	if blank {
		return StateNoAnswer, "the tool exited 0 with a blank answer", "", nil
	}
	if answerErr != nil {
		return StateInvalid, fmt.Sprintf("profile %s could not read the answer: %v", req.Profile.Name, answerErr), "", nil
	}
	verdict, findings, err := decodeAnswer(schema, answer)
	if err != nil {
		return StateInvalid, err.Error(), "", nil
	}
	return StateOK, "", verdict, findings
}

// quotaText returns the first quota text found, case-insensitively, in any of outs. A text counts only where no letter or digit
// borders it, so "429" is not found in "1429" or "4290".
func quotaText(texts []string, outs ...[]byte) (string, bool) {
	for _, out := range outs {
		lower := strings.ToLower(string(out))
		for _, t := range texts {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" && containsWord(lower, t) {
				return t, true
			}
		}
	}
	return "", false
}

// containsWord reports whether word occurs in s with no letter or digit right before or after it.
func containsWord(s, word string) bool {
	for from := 0; from <= len(s)-len(word); {
		i := strings.Index(s[from:], word)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(word)
		before, _ := utf8.DecodeLastRuneInString(s[:start])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if (start == 0 || !isWordRune(before)) && (end == len(s) || !isWordRune(after)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(s[start:])
		from = start + size
	}
	return false
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// answerDoc is the strict decoding of AnswerSchema.
type answerDoc struct {
	Verdict  string `json:"verdict"`
	Findings []struct {
		Severity string `json:"severity"`
		Impact   string `json:"impact"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Text     string `json:"text"`
	} `json:"findings"`
}

// decodeAnswer validates the answer against the schema (exactly one JSON value), decodes it strictly and normalizes it.
func decodeAnswer(schema *jsonschema.Resolved, answer []byte) (string, []Finding, error) {
	dec := json.NewDecoder(bytes.NewReader(answer))
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", nil, fmt.Errorf("the answer is not JSON: %v", err)
	}
	if err := onlyValue(dec); err != nil {
		return "", nil, fmt.Errorf("the answer is more than one JSON value: %v", err)
	}
	if err := schema.Validate(v); err != nil {
		return "", nil, fmt.Errorf("the answer breaks the schema: %v", err)
	}
	strict := json.NewDecoder(bytes.NewReader(answer))
	strict.DisallowUnknownFields()
	var doc answerDoc
	if err := strict.Decode(&doc); err != nil {
		return "", nil, fmt.Errorf("the answer does not decode: %v", err)
	}
	if doc.Verdict != VerdictVerified && doc.Verdict != VerdictNeedsChanges {
		return "", nil, fmt.Errorf("the answer's verdict %q is neither %s nor %s", doc.Verdict, VerdictVerified, VerdictNeedsChanges)
	}
	findings := make([]Finding, 0, len(doc.Findings))
	security := make([]bool, 0, len(doc.Findings))
	for _, f := range doc.Findings {
		if f.Line < 0 {
			return "", nil, fmt.Errorf("finding at %s has a negative line %d", f.File, f.Line)
		}
		grade, sec, ok := review.NormalizeGrade(f.Severity)
		if !ok {
			return "", nil, fmt.Errorf("finding at %s:%d has a severity no grade word reads: %q", f.File, f.Line, f.Severity)
		}
		findings = append(findings, Finding{ID: FindingID(f.File, f.Text), Severity: string(grade), Impact: f.Impact, File: f.File, Line: f.Line, Text: f.Text})
		security = append(security, sec)
	}
	findings, err := normalize(doc.Verdict, findings, security)
	if err != nil {
		return "", nil, err
	}
	return doc.Verdict, findings, nil
}

// onlyValue reports an error unless dec, having decoded one value, holds nothing but whitespace after it: a second value or any
// trailing data, even a stray '}' or ']' that dec.More does not see, is an error.
func onlyValue(dec *json.Decoder) error {
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case err == io.EOF:
		return nil
	case err != nil:
		return err
	default:
		return fmt.Errorf("a second value follows: %s", firstLine(extra))
	}
}

// normalize applies the impact rules of the flow (CRW-1438) and collapses findings that share an id into the first of them,
// keeping the most severe grade and the first blocking impact (else the first non-empty one), so ids are unique inside a record. security[i] is the security
// flag review.NormalizeGrade read from the severity of findings[i]. In order:
//   - an impact that is not empty and not one of premerge.Impacts is an error;
//   - a security finding without an impact gets safety_or_data;
//   - after the collapse, a verified verdict over a finding that premerge.Blocks is an error naming the finding (the adapter's
//     form of verdict_invalid).
func normalize(verdict string, findings []Finding, security []bool) ([]Finding, error) {
	out := make([]Finding, 0, len(findings))
	at := map[string]int{}
	for k, f := range findings {
		if f.Impact != "" && !slices.Contains(premerge.Impacts, f.Impact) {
			return nil, fmt.Errorf("finding %s at %s:%d has an impact outside the closed set: %q", f.ID, f.File, f.Line, f.Impact)
		}
		if f.Impact == "" && k < len(security) && security[k] {
			f.Impact = impactSafetyOrData
		}
		i, seen := at[f.ID]
		if !seen {
			at[f.ID] = len(out)
			out = append(out, f)
			continue
		}
		if f.Severity < out[i].Severity { // "P0" < "P1" < "P2" < "P3"
			out[i].Severity = f.Severity
		}
		if out[i].Impact == "" || (out[i].Impact == premerge.MinorSeparable && f.Impact != "" && f.Impact != premerge.MinorSeparable) {
			out[i].Impact = f.Impact // a blocking impact of any duplicate is never hidden by a non-blocking one
		}
	}
	if verdict == VerdictVerified {
		if f, ok := firstBlocking(out); ok {
			return nil, fmt.Errorf("the answer is verified over the blocking finding %s at %s:%d (severity %s, impact %q)", f.ID, f.File, f.Line, f.Severity, f.Impact)
		}
	}
	return out, nil
}

// firstBlocking returns the first of findings that premerge.Blocks, the finding a verified verdict may not stand over.
func firstBlocking(findings []Finding) (Finding, bool) {
	for _, f := range findings {
		if premerge.Blocks(f.Severity, f.Impact) {
			return f, true
		}
	}
	return Finding{}, false
}

// impactSafetyOrData is the impact a security finding that names none gets.
const impactSafetyOrData = "safety_or_data"

// FindingID is the identity of a finding: the file and the text with whitespace collapsed and case folded. The line and the
// severity are not part of it, so a finding keeps its id when code above it moves.
func FindingID(file, text string) string {
	sum := sha256.Sum256([]byte(file + "\x00" + strings.ToLower(strings.Join(strings.Fields(text), " "))))
	return hex.EncodeToString(sum[:])[:16]
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// maxOutput caps what the exec runner keeps of each stream.
const maxOutput = 8 << 20

// killGrace is how long the exec runner waits for the streams after it killed the process group.
const killGrace = 2 * time.Second

// ExecRun is the real RunFunc: it runs the profile's argv with the prompt as the last argument, in the worktree, in a process
// group of its own that is killed when ctx ends (the time limit). A tool that did not start, ran past ctx, or wrote more than
// the cap is an error.
//
// The tool writes into pipes the runner reads itself, so the wait for the tool ends when the tool exits, not when the last
// holder of its streams closes them: once it has exited, the runner kills what it left running in its group (a background child
// that inherited stdout keeps the pipe open) and only then collects the output. A stream still open killGrace after that, held by
// a process that left the group, is an error.
func ExecRun(ctx context.Context, p Profile, req Request) ([]byte, []byte, int, error) {
	if len(p.Argv) == 0 {
		return nil, nil, -1, fmt.Errorf("%w: profile %q has no argv", ErrNotStarted, p.Name)
	}
	args := append(append([]string{}, p.Argv[1:]...), req.Prompt)
	cmd := exec.CommandContext(ctx, p.Argv[0], args...)
	cmd.Dir = req.Worktree
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, nil, -1, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var killed atomic.Bool
	cmd.Cancel = func() error {
		killed.Store(true)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = killGrace
	startErr := cmd.Start()
	// The tool holds its own copies of the write ends now (or never will): the runner's must close, or the reads never end.
	outW.Close()
	errW.Close()
	if startErr != nil {
		outR.Close()
		errR.Close()
		return nil, nil, -1, fmt.Errorf("%w: %w", ErrNotStarted, startErr)
	}
	stdout, stderr := &capped{max: maxOutput}, &capped{max: maxOutput}
	var copies sync.WaitGroup
	for _, s := range []struct {
		w *capped
		r *os.File
	}{{stdout, outR}, {stderr, errR}} {
		copies.Go(func() { _, _ = io.Copy(s.w, s.r) })
	}
	err = cmd.Wait()
	// The tool has exited: end whatever it left running in its group, which may still hold the streams open.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	drained := make(chan struct{})
	go func() { copies.Wait(); close(drained) }()
	var streamsErr error
	select {
	case <-drained:
	case <-time.After(killGrace):
		outR.Close() // ends the reads a process outside the group still feeds
		errR.Close()
		<-drained
		streamsErr = fmt.Errorf("the tool's output streams stayed open %s after it exited", killGrace)
	}
	outR.Close()
	errR.Close()
	if killed.Load() || ctx.Err() != nil {
		return stdout.buf.Bytes(), stderr.buf.Bytes(), -1, fmt.Errorf("the tool was killed: %w", context.Cause(ctx))
	}
	if stdout.over || stderr.over {
		return stdout.buf.Bytes(), stderr.buf.Bytes(), -1, fmt.Errorf("the tool wrote more than %d bytes", maxOutput)
	}
	if streamsErr != nil {
		return stdout.buf.Bytes(), stderr.buf.Bytes(), -1, streamsErr
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return stdout.buf.Bytes(), stderr.buf.Bytes(), -1, err
	}
	return stdout.buf.Bytes(), stderr.buf.Bytes(), cmd.ProcessState.ExitCode(), nil
}

// capped keeps the first max bytes written to it and notes that more came.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.over = true
		return len(p), nil
	}
	return c.buf.Write(p)
}
