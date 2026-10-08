package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/manage"
	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// GET /api/status is the run-state screen's read-only endpoint. Every value it carries comes
// from a named read the runtime already has: the relay relationships, DAG plan progress and
// merge turns from crw manage relay-read; capacity from crw manage capacity --dry-run; the DAG
// anomalies from crw manage dag-review --no-state; the App Server's answer from
// crw manage host-read --method thread/loaded/list; and the execution policy from
// internal/policystore. The commands run in this process through manage.Run, one at a time
// behind a mutex and each under its own time bound, so this package starts no subprocess of its
// own and opens no store itself.
//
// The package makes no judgement about a value. The three manage documents are passed through
// verbatim, and a source that could not be read is reported as unknown with the reason the
// command itself gave - never as zero, ok or an empty list. The three status-bar readings stay
// separate, so one unread source cannot hide behind the other two.

// statusSchema is the document's schema name.
const statusSchema = "crw-gui-status/1"

// statusSourceTimeout bounds one named read. It sits above capacity's own signal timeout
// (internal/manage's capacitySignalTimeout, 30s), so a slow capacity signal expires inside the
// command and reports its own reason instead of being cut off here.
var statusSourceTimeout = 45 * time.Second

// statusPolicyGrace is how long a policy read that hit its deadline may still hand back what it
// already read.
const statusPolicyGrace = time.Second

// statusPolicyTimeout bounds the policy read. It is separate from statusSourceTimeout so a test
// can drive the timeout path without waiting out the manage sources' bound.
var statusPolicyTimeout = statusSourceTimeout

// The two answers of one read.
const (
	statusOK      = "ok"
	statusUnknown = "unknown"
)

// The argument lists of the named reads, and the exit statuses internal/manage declares for
// them. The status constants there (relayReadUnknownExit, dagReviewAnomalyExit) are unexported,
// so the values are restated with the name each carries there. capacity and dag-review are
// called only in the form that writes no state of their own.
const (
	statusCommandRelay     = "relay-read"
	statusCommandCapacity  = "capacity"
	statusCapacityDryRun   = "--dry-run"
	statusCommandDagReview = "dag-review"
	statusDagNoState       = "--no-state"
	statusCommandHostRead  = "host-read"
	statusHostReadMethod   = "thread/loaded/list"

	// statusRelayUnreadExit is internal/manage's relayReadUnknownExit: the command printed the
	// projection and reported that at least one value could not be read.
	statusRelayUnreadExit = 1
	// statusDagAnomalyExit is internal/manage's dagReviewAnomalyExit: the review found
	// something, which is a complete read rather than a failure.
	statusDagAnomalyExit = 1
)

// statusManageRunner runs one manage command line and reports its exit status and output. The
// production value runs manage in this process; a test replaces it, following this package's
// seam pattern (catalog_api.go's catalogReader), so no test runs a manage command against a
// real host and the argument list each source uses is asserted from the fake.
type statusManageRunner func(ctx context.Context, args []string) (code int, stdout, stderr string)

// statusManageGate serializes the in-process manage calls: one at a time, as the decided answer
// requires. A manage call may run the same executable's relay mode, and the endpoint serves one
// poll at a time rather than a burst.
//
// It is a channel rather than a mutex so a caller can stop waiting for its turn: a request whose
// context ended while another read held the gate returns without starting a call, instead of
// queueing behind a read it can no longer use. That is the only cancellation this layer can
// offer, because a manage read that has already started is pre-empted only where it honours the
// context, and internal/manage is out of this issue's scope with readers that are not all
// ctx-aware.
var statusManageGate = make(chan struct{}, 1)

var statusManage statusManageRunner = statusRunManage

// statusPolicyReader is the policy seam: the same read the policy endpoint exposes, replaceable
// so a test can observe the deadline it runs under and the case where it does not answer.
type statusPolicyReaderFunc func(ctx context.Context) statusPolicyReading

var statusPolicyReader statusPolicyReaderFunc = statusPolicyReadProduction

// statusRunManage is the production seam: crw manage, in this process, with the output captured.
func statusRunManage(ctx context.Context, args []string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := manage.Run(ctx, args, nil, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// statusMark is one status-bar reading: whether the source answered, and why not.
type statusMark struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// statusPolicyReading is the execution-policy reading. The file, registered and running digests
// are separate values, each with its own reason, so the bar can never conflate the bytes on disk
// with the digest the wiring record names and with the digest the running service published.
type statusPolicyReading struct {
	State            string  `json:"state"`
	Reason           string  `json:"reason,omitempty"`
	PolicyState      string  `json:"policyState"`
	Path             string  `json:"path,omitempty"`
	FileDigest       *string `json:"fileDigest"`
	FileReason       string  `json:"fileReason,omitempty"`
	RegisteredDigest *string `json:"registeredDigest"`
	RegisteredReason string  `json:"registeredReason,omitempty"`
	RunningDigest    *string `json:"runningDigest"`
	RunningReason    string  `json:"runningReason,omitempty"`
	Applied          string  `json:"applied"`
}

// statusSource is one manage document as the response carries it: the read mark and the
// command's own document, verbatim, or null when the command printed none.
type statusSource struct {
	State  string          `json:"state"`
	Reason string          `json:"reason,omitempty"`
	Data   json.RawMessage `json:"data"`
}

// statusBar is the three readings, kept apart.
type statusBar struct {
	RelayStore      statusMark          `json:"relayStore"`
	AppServer       statusMark          `json:"appServer"`
	ExecutionPolicy statusPolicyReading `json:"executionPolicy"`
}

// statusBody is the whole answer.
type statusBody struct {
	Schema   string       `json:"schema"`
	ReadAt   string       `json:"readAt"`
	Bar      statusBar    `json:"bar"`
	Relay    statusSource `json:"relay"`
	Capacity statusSource `json:"capacity"`
	Dag      statusSource `json:"dag"`
}

// statusHandler answers GET /api/status. It reads the four named manage sources and the policy,
// then reports what each of them said. The response is always 200: a source that failed is data
// the screen renders, not an HTTP failure.
func statusHandler(_ *Env, r *http.Request) (Response, error) {
	ctx := r.Context()
	relay := statusRelaySource(ctx)
	capacity := statusManageSource(ctx, []int{0}, statusCommandCapacity, statusCapacityDryRun)
	dag := statusManageSource(ctx, []int{0, statusDagAnomalyExit}, statusCommandDagReview, statusDagNoState)
	appServer := statusAppServerSource(ctx)
	policy := statusPolicyRead(ctx)
	body := statusBody{
		Schema: statusSchema,
		ReadAt: time.Now().UTC().Format(time.RFC3339),
		Bar: statusBar{
			RelayStore:      statusMark{State: relay.State, Reason: relay.Reason},
			AppServer:       appServer,
			ExecutionPolicy: policy,
		},
		Relay:    relay,
		Capacity: capacity,
		Dag:      dag,
	}
	return Response{Status: http.StatusOK, Body: body}, nil
}

// statusRelaySource reads crw manage relay-read. Exit 1 is the command's own report that it
// printed the projection but could not read part of it: the reading is unknown with the reason
// the command recorded, and the document it printed is carried unchanged, so the readable parts
// stay visible.
func statusRelaySource(ctx context.Context) statusSource {
	code, stdout, stderr := statusManageCall(ctx, statusCommandRelay)
	document, decodeErr := statusDecode(stdout)
	if decodeErr != nil {
		return statusSource{State: statusUnknown, Reason: statusFirstNonEmpty(statusStderrReason(stderr), decodeErr.Error())}
	}
	switch code {
	case 0:
		return statusSource{State: statusOK, Data: document}
	case statusRelayUnreadExit:
		return statusSource{State: statusUnknown, Reason: statusRelayUnreadReason(document), Data: document}
	default:
		return statusSource{State: statusUnknown,
			Reason: statusFirstNonEmpty(statusStderrReason(stderr), fmt.Sprintf("the relay read exited with status %d", code)),
			Data:   document}
	}
}

// statusManageSource reads one named manage source and maps its answer. okExits are the exit
// statuses that mean the command answered; any other status is unknown with the command's own
// reason. The document is decoded first, so a command that printed its JSON and then reported a
// failure still contributes what it printed.
func statusManageSource(ctx context.Context, okExits []int, args ...string) statusSource {
	code, stdout, stderr := statusManageCall(ctx, args...)
	document, decodeErr := statusDecode(stdout)
	if decodeErr != nil {
		return statusSource{State: statusUnknown, Reason: statusFirstNonEmpty(statusStderrReason(stderr), decodeErr.Error())}
	}
	for _, ok := range okExits {
		if code == ok {
			return statusSource{State: statusOK, Data: document}
		}
	}
	return statusSource{State: statusUnknown,
		Reason: statusFirstNonEmpty(statusStderrReason(stderr), fmt.Sprintf("the %s read exited with status %d", args[0], code)),
		Data:   document}
}

// statusAppServerSource reads the App Server through crw manage host-read. The result body is
// not carried: the bar needs the answer, and the loaded thread ids are operator data this
// endpoint has no reason to publish.
func statusAppServerSource(ctx context.Context) statusMark {
	code, stdout, stderr := statusManageCall(ctx, statusCommandHostRead, "--method", statusHostReadMethod)
	if code == 0 {
		if _, err := statusDecode(stdout); err != nil {
			return statusMark{State: statusUnknown, Reason: statusFirstNonEmpty(statusStderrReason(stderr), err.Error())}
		}
		return statusMark{State: statusOK}
	}
	return statusMark{State: statusUnknown, Reason: statusFirstNonEmpty(statusHostReadReason(stdout), statusStderrReason(stderr),
		fmt.Sprintf("the App Server read exited with status %d", code))}
}

// statusPolicyRead reads the execution policy the wiring record names, through the same
// internal/policystore reads GET /api/policy uses. It writes nothing.
func statusPolicyRead(ctx context.Context) statusPolicyReading {
	// The policy read is a source like the manage reads, so it gets the same bound: a read that
	// does not answer inside it is unknown with a reason rather than something that holds the
	// request open.
	readCtx, cancel := context.WithTimeout(ctx, statusPolicyTimeout)
	defer cancel()
	// The read runs beside this request so a read that never returns is still answered on its
	// deadline. Its answer is used only when it arrives in time.
	done := make(chan statusPolicyReading, 1)
	go func() { done <- statusPolicyReader(readCtx) }()
	var reading statusPolicyReading
	select {
	case reading = <-done:
	case <-readCtx.Done():
		// The production reader stops its own running read at the deadline and hands back what
		// it already has, so it gets a short grace to do so. Past the grace the whole read is
		// unknown.
		select {
		case reading = <-done:
		case <-time.After(statusPolicyGrace):
			return statusPolicyReading{State: statusUnknown, Applied: policystore.AppliedUnverifiable,
				Reason: "the execution policy read did not finish: " + readCtx.Err().Error()}
		}
	}
	// A reader that reports ok after the deadline without saying what it read for the running
	// digest is treated as unfinished, so an ok it never earned does not survive.
	if err := readCtx.Err(); err != nil && reading.State == statusOK && reading.RunningReason == "" && reading.RunningDigest == nil {
		return statusPolicyReading{State: statusUnknown, PolicyState: reading.PolicyState,
			Path: reading.Path, Applied: policystore.AppliedUnverifiable,
			Reason: "the execution policy read did not finish: " + err.Error()}
	}
	return reading
}

// statusPolicyReadProduction is the policy read itself: the same internal/policystore calls
// GET /api/policy makes. It writes nothing.
func statusPolicyReadProduction(ctx context.Context) statusPolicyReading {
	located := policystore.Locate(envLookup)
	reading := policystore.Read(located)
	running := statusRunningReader(ctx)
	out := statusPolicyReading{
		PolicyState: reading.State,
		Path:        reading.Path,
		Applied:     policystore.Applied(reading, running),
	}
	if reading.State != policystore.Registered {
		out.State = statusUnknown
		out.Reason = statusFirstNonEmpty(reading.Reason, "the execution policy is not registered")
	} else {
		out.State = statusOK
	}
	if reading.Digest != "" {
		digest := reading.Digest
		out.FileDigest = &digest
	} else {
		out.FileReason = statusFirstNonEmpty(reading.Reason, "the execution policy file digest was not read")
	}
	if reading.RegisteredDigest != "" {
		digest := reading.RegisteredDigest
		out.RegisteredDigest = &digest
	} else {
		out.RegisteredReason = statusFirstNonEmpty(reading.Reason, "the wiring record names no registered digest")
	}
	if running.State == policystore.RunningObserved {
		digest := running.Digest
		out.RunningDigest = &digest
	} else {
		out.RunningReason = statusFirstNonEmpty(running.Reason, "the running policy digest was not read")
	}
	return out
}

// statusRunningReader reads the running digest. A test replaces it to hold the read open; the
// production value is statusRunningDigestGated.
var statusRunningReader = statusRunningDigestGated

// statusRunningDigestGated reads the running digest under the manage gate. Its manage config
// call is a manage read like the others, so it takes its turn behind them.
func statusRunningDigestGated(ctx context.Context) policystore.Running {
	select {
	case statusManageGate <- struct{}{}:
	case <-ctx.Done():
		return policystore.Running{State: policystore.RunningUnavailable,
			Reason: "the running policy digest was not read: " + ctx.Err().Error()}
	}
	defer func() { <-statusManageGate }()
	return policystore.RunningDigest(ctx, envLookup)
}

// statusCallResult is one finished manage call.
type statusCallResult struct {
	code           int
	stdout, stderr string
}

// statusManageCall runs one manage command line under its own time bound. The call runs beside
// this request: a call that does not return by the bound is reported unknown on time, and it keeps
// the gate until it does return, so the next read waits behind it rather than running beside it.
func statusManageCall(ctx context.Context, args ...string) (int, string, string) {
	callCtx, cancel := context.WithTimeout(ctx, statusSourceTimeout)
	defer cancel()
	// A context that already ended starts no call at all. The check is made before the gate as
	// well as while waiting for it, because a select over a free gate and a done context would
	// otherwise choose between them at random.
	if err := callCtx.Err(); err != nil {
		return 0, "", "the read did not start: " + err.Error()
	}
	// Take the gate before the call. A context that ended while another read held it is this
	// call's own failure: it is reported rather than waited on, so a cancelled request does not
	// queue behind a read it can no longer use.
	select {
	case statusManageGate <- struct{}{}:
	case <-callCtx.Done():
		return 0, "", "the read did not start: " + callCtx.Err().Error()
	}
	done := make(chan statusCallResult, 1)
	go func() {
		defer func() { <-statusManageGate }()
		code, stdout, stderr := statusManage(callCtx, args)
		done <- statusCallResult{code: code, stdout: stdout, stderr: stderr}
	}()
	select {
	case result := <-done:
		return result.code, result.stdout, result.stderr
	case <-callCtx.Done():
		return 0, "", "the read did not finish: " + callCtx.Err().Error()
	}
}

// statusDecode reads one JSON document from a command's stdout. An empty or unparseable answer
// is an error, never an empty document.
func statusDecode(stdout string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return nil, errors.New("the command printed no JSON document")
	}
	var document json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &document); err != nil {
		return nil, fmt.Errorf("the command's output is not a JSON document: %w", err)
	}
	return document, nil
}

// statusRelayUnreadReason names why a relay read is incomplete: the first section-level failure
// the command recorded, else the first item-level reason, else a plain statement that the read
// was not complete. It is never empty.
func statusRelayUnreadReason(document json.RawMessage) string {
	const fallback = "the relay read reported values it could not read"
	var parsed struct {
		Failures []struct {
			Reason string `json:"reason"`
		} `json:"failures"`
		Bindings      []statusRelayItem `json:"bindings"`
		Relationships []statusRelayItem `json:"relationships"`
		Plans         []statusRelayItem `json:"plans"`
		MergeTurns    []statusRelayItem `json:"mergeTurns"`
	}
	if err := json.Unmarshal(document, &parsed); err != nil {
		return fallback
	}
	for _, failure := range parsed.Failures {
		if failure.Reason != "" {
			return failure.Reason
		}
	}
	for _, group := range [][]statusRelayItem{parsed.Bindings, parsed.Relationships, parsed.Plans, parsed.MergeTurns} {
		for _, item := range group {
			if item.Read.State != statusOK && item.Read.Reason != "" {
				return item.Read.Reason
			}
		}
	}
	return fallback
}

// statusRelayItem is one item of a relay projection, as far as its read mark.
type statusRelayItem struct {
	Read statusReadMark `json:"read"`
}

// statusReadMark is the relay's own per-item read mark.
type statusReadMark struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// statusHostReadReason reads the reason out of a refused host-read answer, which is
// {"ok":false,"reason":"...","detail":"..."}.
func statusHostReadReason(stdout string) string {
	var answer struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &answer); err != nil || answer.OK || answer.Reason == "" {
		return ""
	}
	if answer.Detail == "" {
		return answer.Reason
	}
	return answer.Reason + ": " + answer.Detail
}

// statusStderrReason is the last non-empty line a command wrote to stderr, which is the line it
// reports its failure on.
func statusStderrReason(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// statusFirstNonEmpty is the first value that is not empty, so a reason is never blank.
func statusFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func init() {
	Register(http.MethodGet, "/api/status", statusHandler)
}
