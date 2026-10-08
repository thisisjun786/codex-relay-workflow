//go:build dev

package cxcfuzz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The causes a case the harness could not answer is recorded under (CRW-978 c7).
const (
	CauseTimeout    = "timeout"
	CauseDeadWorker = "dead worker"
	CauseNoAnswer   = "no answer"
	CauseDecode     = "decode"
	CauseTempRoot   = "temporary root"
	CauseError      = "error"
)

// Failure is one case record under <out>/failures/: the case the campaign could not answer, its input and cause.
type Failure struct {
	Case   int    `json:"case"`
	Cause  string `json:"cause"`
	Input  string `json:"input"`
	Detail string `json:"detail"`
	Seed   int64  `json:"seed"`
	DevSHA string `json:"devSha"`
}

// CaseFailure is a case the harness could not answer, under the cause its record names it by. Its error chain
// keeps the underlying error, so errors.Is still finds a Timeout.
type CaseFailure struct {
	Cause string
	Err   error
}

func (e CaseFailure) Error() string { return e.Cause + ": " + e.Err.Error() }

func (e CaseFailure) Unwrap() error { return e.Err }

// transportFailure is a failed exchange that is not a missed deadline: the worker ended, or its pipe broke,
// before it replied. A missed deadline stays a Timeout.
func transportFailure(err error) error {
	if errors.Is(err, Timeout{}) {
		return err
	}
	return CaseFailure{Cause: CauseDeadWorker, Err: err}
}

// failureCause is the cause a case's error is recorded under.
func failureCause(err error) string {
	var failure CaseFailure
	if errors.As(err, &failure) {
		return failure.Cause
	}
	if errors.Is(err, Timeout{}) {
		return CauseTimeout
	}
	return CauseError
}

// failureDir is the subdirectory of --out the case records go in.
const failureDir = "failures"

// writeFailure writes one case record, once per cause and input hash, and reports the name the summary lists it
// under and whether this call wrote it (CRW-978 c7).
func writeFailure(out string, record Failure, written map[string]bool) (string, bool, error) {
	sum := sha256.Sum256([]byte(record.Input))
	name := failureDir + "/" + strings.ReplaceAll(record.Cause, " ", "-") + "-" + hex.EncodeToString(sum[:])[:12] + ".json"
	if written[name] {
		return name, false, nil
	}
	written[name] = true
	if err := os.MkdirAll(filepath.Join(out, failureDir), 0o755); err != nil {
		return "", false, err
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(out, filepath.FromSlash(name)), append(raw, '\n'), 0o644); err != nil {
		return "", false, err
	}
	return name, true, nil
}

// recordFailure counts one case the harness could not answer under its cause and writes its record, so the
// summary can name the input that failed (CRW-978 c7).
func recordFailure(cfg Config, summary *Summary, seed int64, number int, input any, cause error, written map[string]bool) error {
	kind := failureCause(cause)
	switch kind {
	case CauseTimeout:
		summary.Timeouts++
	case CauseDeadWorker:
		summary.DeadWorkers++
	case CauseNoAnswer:
		summary.NoAnswers++
	default:
		summary.Errors++
	}
	name, fresh, err := writeFailure(cfg.Out, Failure{Case: number, Cause: kind, Input: canonical(input), Detail: cause.Error(), Seed: seed, DevSHA: cfg.DevSHA}, written)
	if err != nil {
		return err
	}
	if fresh {
		summary.Failures = append(summary.Failures, name)
	}
	return nil
}
