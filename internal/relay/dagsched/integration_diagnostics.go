package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// The bounded diagnostics of a failed integration verification (CRW-1595): the batch's temporary worktree — the
// verification record included — is removed on every path, so a failure would leave nothing to tell a real failure
// from a flake by. Each failed attempt instead journals a bounded detail: the cause (the command's output tail, or the
// reason its record could not be judged), and the record's failed jobs and steps. Diagnostics never establish a
// passing verification and never classify a failure as a flake.

// integrationDiagnosticLimit caps the bytes of one diagnostic, of the output tail and of the assembled detail.
const integrationDiagnosticLimit = 8192

// integrationRecordReadLimit caps how much of a failed verification's record is read for the diagnostic; the path is
// read as bytes only, nothing it names is followed.
const integrationRecordReadLimit = 1 << 20

// integrationOutputTail is a bounded sink for a verification command's output: it keeps the last
// integrationDiagnosticLimit bytes written to it. CommandVerifierEnv gives one tail as both Stdout and Stderr, which
// os/exec serializes, so a failure's error carries the tail of both streams in write order.
type integrationOutputTail struct {
	data    []byte
	clipped bool
}

func (b *integrationOutputTail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= integrationDiagnosticLimit {
		b.clipped = b.clipped || len(b.data)+len(p) > integrationDiagnosticLimit
		b.data = append(b.data[:0], p[len(p)-integrationDiagnosticLimit:]...)
	} else {
		drop := len(b.data) + len(p) - integrationDiagnosticLimit
		if drop > 0 {
			b.data = b.data[drop:]
			b.clipped = true
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *integrationOutputTail) String() string {
	text := strings.ToValidUTF8(string(b.data), "")
	if b.clipped {
		return "[earlier output omitted]\n" + text
	}
	return text
}

// boundedIntegrationDiagnostic is the assembled diagnostic capped to integrationDiagnosticLimit bytes, cut at a rune
// boundary and marked.
func boundedIntegrationDiagnostic(text string) string {
	text = strings.ToValidUTF8(text, "")
	if len(text) <= integrationDiagnosticLimit {
		return text
	}
	const marker = "[earlier diagnostic omitted]\n"
	start := len(text) - (integrationDiagnosticLimit - len(marker))
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return marker + text[start:]
}

// verificationJob is the shape the diagnostic parses of a record's jobs: name, result and the steps' name, result and
// reason. Nothing else of the record is read.
type verificationJob struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Steps  []struct {
		Name   string `json:"name"`
		Result string `json:"result"`
		Reason string `json:"reason"`
	} `json:"steps"`
}

// verificationResultPassed is whether a job or step result needs no explanation: a pass (the job-level "pass", the
// step-level "passed") or a step the run never performs (not_applicable). A skipped step is a failure (the record's
// own rule), as is any result not in this set.
func verificationResultPassed(result string) bool {
	return result == "pass" || result == "passed" || result == "not_applicable"
}

// verificationFailed records the bounded diagnostic of one failed verification: the cause, then the failed jobs and
// steps of the record the command wrote. A record that is missing, unreadable or malformed adds its actual reason —
// it never stands in as passing evidence. The detail is journaled as one generic row of kind
// dag_integration_verification_failed keyed by the batch (the frozen dag_integration_stages has no such stage), and
// kept on the worktree for the split the settle loop then makes. A recording error propagates: evidence is never
// silently lost.
func (w *integrationWorktree) verificationFailed(ctx context.Context, commit, recordPath, cause string) error {
	var b strings.Builder
	b.WriteString(cause)
	raw, err := readIntegrationRecord(recordPath)
	switch {
	case err != nil:
		b.WriteString("\nthe verification record could not be read: " + err.Error())
	default:
		var rec struct {
			Jobs []verificationJob `json:"jobs"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			b.WriteString("\nthe verification record is malformed: " + err.Error())
			break
		}
		for _, job := range rec.Jobs {
			if !verificationResultPassed(job.Result) {
				fmt.Fprintf(&b, "\njob %s: %s", job.Name, job.Result)
			}
			for _, step := range job.Steps {
				if verificationResultPassed(step.Result) {
					continue
				}
				fmt.Fprintf(&b, "\nstep %s/%s: %s", job.Name, step.Name, step.Result)
				if step.Reason != "" {
					b.WriteString(": " + step.Reason)
				}
			}
		}
	}
	w.lastFailure = boundedIntegrationDiagnostic(b.String())
	detail, err := json.Marshal(map[string]any{
		"batch_id": w.batch, "plan": w.in.Plan, "integration_ref": w.in.IntegrationRef,
		"verified_head": commit, "attempt": w.records, "detail": w.lastFailure})
	if err != nil {
		return err
	}
	return w.s.Store.Compose(ctx, func(txCtx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(txCtx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)",
			w.s.now(), "dag_integration_verification_failed", w.batch, string(detail))
		return err
	})
}

// readIntegrationRecord reads at most integrationRecordReadLimit bytes of the record the verification command was
// asked to write.
func readIntegrationRecord(recordPath string) ([]byte, error) {
	f, err := os.Open(recordPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, integrationRecordReadLimit))
}
