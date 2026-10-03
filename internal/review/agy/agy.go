// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/agent/antigravity.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package agy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// DefaultModel is the model of a call unless Config.Model says otherwise. It is the one place the model is chosen.
const DefaultModel = "gemini-3.8-flash-high"

// Class is how a call ended. Only ClassNormal carries a structured output the caller may use; an invalid or unavailable call is never "no findings".
type Class string

const (
	ClassNormal      Class = "normal"
	ClassInvalid     Class = "invalid"     // agy answered, but the answer cannot be trusted
	ClassUnavailable Class = "unavailable" // agy gave no answer
)

// Reason says why a call is invalid or unavailable; it is empty for a normal call. The package documentation lists what each one means.
type Reason string

const (
	ReasonDeniedActions   Reason = "denied_actions"
	ReasonEmptyResponse   Reason = "empty_response"
	ReasonNoStructured    Reason = "no_structured_output"
	ReasonPartialResponse Reason = "print_timeout_partial"
	ReasonTimeLimit       Reason = "time_limit_exceeded"
	ReasonPromptLength    Reason = "prompt_length_mismatch"
	ReasonQuota           Reason = "quota"
	ReasonAuthentication  Reason = "authentication"
	ReasonUnknownModel    Reason = "unknown_model"
	ReasonContentFilter   Reason = "content_filter"
	ReasonCrash           Reason = "crash"
	ReasonNotStarted      Reason = "not_started"
	ReasonLockWaitExpired Reason = "lock_wait_expired"
)

// Usage is the token count of the envelope.
type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

// Config holds the settings of a call. A zero field takes its default.
type Config struct {
	Binary           string        // the agy executable, looked up on PATH; default "agy"
	Model            string        // default DefaultModel
	LockPath         string        // the host-wide lock file; default DefaultLockPath()
	LockWait         time.Duration // how long to wait for the lock; default 30 minutes, negative: try once
	WorkRoot         string        // the directory the per-call directory is made in; default os.TempDir()
	TimeLimitFloor   time.Duration // the call time limit for a prompt up to 100 KiB; default 5 minutes
	TimeLimitCeiling time.Duration // the most the limit grows to; default 20 minutes
	MaxOutputBytes   int           // what is kept of stdout and of stderr, each; default 8 MiB
}

func (c Config) withDefaults() Config {
	if c.Binary == "" {
		c.Binary = "agy"
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.LockPath == "" {
		c.LockPath = DefaultLockPath()
	}
	if c.LockWait == 0 {
		c.LockWait = 30 * time.Minute
	}
	if c.WorkRoot == "" {
		c.WorkRoot = os.TempDir()
	}
	if c.TimeLimitFloor <= 0 {
		c.TimeLimitFloor = 5 * time.Minute
	}
	if c.TimeLimitCeiling <= 0 {
		c.TimeLimitCeiling = 20 * time.Minute
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8 << 20
	}
	return c
}

// Request is one call: the prompt, sent on stdin exactly as given, and, if the answer must be structured, the text of the JSON Schema.
type Request struct {
	Prompt []byte
	Schema []byte
}

// Result is the outcome of one call.
type Result struct {
	Class            Class
	Reason           Reason
	Detail           string          // what decided the class, for a person
	StructuredOutput json.RawMessage // the envelope's structured_output as agy wrote it; only for ClassNormal with a schema
	Model            string          // the model agy's log names as served (its label); empty if the log names none, and not the requested id
	Usage            Usage
	ExitCode         int // -1 when agy was killed, did not start or was not started
	Elapsed          time.Duration
	Waited           time.Duration // time spent waiting for the host-wide lock
	Limit            time.Duration // the call time limit that applied
}

// Run makes one agy call: it waits for the host-wide lock, makes the disposable directory, starts agy with the prompt on stdin, and classifies what came
// back. The error is for a cancelled ctx, an empty prompt and a failure of the host (the lock file, the working directory, removing the directory);
// when removing the directory fails, the Result is still valid.
func Run(ctx context.Context, cfg Config, req Request) (res Result, err error) {
	cfg = cfg.withDefaults()
	if len(req.Prompt) == 0 {
		return Result{}, errors.New("agy: the prompt is empty")
	}
	limit := timeLimit(cfg.TimeLimitFloor, cfg.TimeLimitCeiling, len(req.Prompt))
	res = Result{ExitCode: -1, Limit: limit}
	bin, lookErr := exec.LookPath(cfg.Binary)
	if lookErr == nil {
		bin, lookErr = filepath.Abs(bin) // agy starts in another directory, so a relative path would no longer lead to it
	}
	if lookErr != nil {
		return res.with(ClassUnavailable, ReasonNotStarted, "%v", lookErr), nil
	}
	waitStart := time.Now()
	release, lockErr := acquire(ctx, cfg.LockPath, cfg.LockWait)
	res.Waited = time.Since(waitStart)
	if errors.Is(lockErr, errLockWait) {
		return res.with(ClassUnavailable, ReasonLockWaitExpired, "the host-wide agy lock %s was not free within %s", cfg.LockPath, cfg.LockWait), nil
	}
	if lockErr != nil {
		return Result{}, lockErr
	}
	defer release()
	dir, dirErr := newCallDir(cfg.WorkRoot, req.Schema)
	if dirErr != nil {
		return Result{}, dirErr
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir.root)) }()
	ex := execute(ctx, limit, bin, dir.args(cfg.Model), dir.work, scrubEnv(os.Environ()), req.Prompt, cfg.MaxOutputBytes)
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	out := classify(ex, readLog(dir.log, cfg.MaxOutputBytes), len(req.Prompt), len(req.Schema) > 0)
	out.Waited = res.Waited
	return out, nil
}
