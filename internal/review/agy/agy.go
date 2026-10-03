package agy

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// DefaultModel is the model of a call unless Config.Model says otherwise. It is the one place the model is chosen.
const DefaultModel = "gemini-3.8-flash-high"

// Class is how a call ended. Only ClassNormal carries a structured output the caller may use; an invalid or unavailable call is never "no findings".
type Class string

const (
	ClassNormal      Class = "normal"
	ClassInvalid     Class = "invalid"     // agy answered, but the answer cannot be trusted
	ClassUnavailable Class = "unavailable" // agy did not answer
)

// Reason says why a call is invalid or unavailable; it is empty for a normal call.
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
	TimeLimitFloor   time.Duration // call time limit for a small prompt; default 5 minutes
	TimeLimitCeiling time.Duration // the most the limit grows to; default 20 minutes
	MaxOutputBytes   int           // cap on what is kept of stdout and of stderr each; default 8 MiB
	Env              []string      // KEY=VALUE pairs added to the scrubbed environment
}

// Request is one call: the prompt (sent on stdin) and, if the answer must be structured, the JSON Schema text.
type Request struct {
	Prompt []byte
	Schema []byte
}

// Result is the outcome of one call.
type Result struct {
	Class            Class
	Reason           Reason
	Detail           string          // what decided the class, for a person
	StructuredOutput json.RawMessage // the envelope's structured_output as agy wrote it; only for ClassNormal
	Model            string          // the model agy's log names as served; empty if the log names none
	Usage            Usage
	ExitCode         int // -1 if agy was killed or did not start
	Elapsed          time.Duration
	Waited           time.Duration // time spent waiting for the host-wide lock
	Limit            time.Duration // the call time limit that applied
}

// Run makes one agy call.
func Run(ctx context.Context, cfg Config, req Request) (Result, error) {
	return Result{}, errors.New("agy: not implemented")
}
