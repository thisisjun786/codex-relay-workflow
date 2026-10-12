package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// The supervisor queue (CRW-1204): a notice for an initiative lead's thread - a milestone report, a project blocker, a decision request -
// that could not be sent at once is a file under <state_dir>/supervisor-queue/<thread>/. It is not the parent queue: it carries the role
// supervisor and the place its settings are read from (the task of the supervision binding), and the pump delivers it with that role and
// the settings the binding records at flush time. The parent queue (pump_queue.go) is not touched by it.
//
// The pump gates the flush as it gates the parent queue: an active thread is steered, an idle thread is sent the notice once the oldest
// notice has waited max_queue_seconds, and a thread in any other state (notLoaded: the lead's thread is not open anywhere) keeps its
// notices and raises one report_undelivered event for each report. When the thread is loaded, the same flush delivers.
const (
	supervisorQueueDir     = "supervisor-queue"
	supervisorNoticeSchema = "crw-supervisor-notice/1"
	supervisorRole         = "supervisor"
	supervisorRefusedDir   = "refused"

	// unloadedPairCode is the bridge's refusal of a send to a thread that is not loaded when the role's pair cannot be checked
	// (internal/bridge/mutations.go): the thread unloaded between the probe and the send.
	unloadedPairCode = "unverified_pair_for_unloaded_thread"
)

// supervisorNotice is one queued notice.
type supervisorNotice struct {
	Schema    string `json:"schema"`
	LogicalID string `json:"logical_id"`
	Thread    string `json:"thread"`
	Role      string `json:"role"`
	// BindingTask is the task of the supervision binding whose recorded authorization the settings are read from at send time.
	BindingTask string `json:"binding_task"`
	ReportID    string `json:"report_id,omitempty"`
	Initiative  string `json:"initiative,omitempty"`
	// LoadRetry is the number of times a send of this notice met the unloaded-thread refusal; the next attempt goes out under
	// <logical id>-l<n>.
	LoadRetry int    `json:"load_retry,omitempty"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

// pumpSupervisorState is what the flush keeps between rounds.
type pumpSupervisorState struct {
	// Attempts are the ordinals of a notice's attempts, by its logical id.
	Attempts map[string]pumpSupervisorAttempt `json:"attempts"`
	// Undelivered are the notices (by report id, else logical id) whose report_undelivered event was raised.
	Undelivered map[string]bool `json:"undelivered"`
	// Delivered are the accepted deliveries whose `crw flow report-delivered` call is still to be made: report id to request id.
	Delivered map[string]string `json:"delivered"`
}

// pumpSupervisorAttempt counts the refusals of one notice's logical id (-r<n>) and the unloaded-thread refusals (-l<n>).
type pumpSupervisorAttempt struct {
	Refused int `json:"refused,omitempty"`
	Loaded  int `json:"loaded,omitempty"`
}

func newPumpSupervisorState() pumpSupervisorState {
	return pumpSupervisorState{Attempts: map[string]pumpSupervisorAttempt{}, Undelivered: map[string]bool{}, Delivered: map[string]string{}}
}

func (s *pumpSupervisorState) fill() {
	if s.Attempts == nil {
		s.Attempts = map[string]pumpSupervisorAttempt{}
	}
	if s.Undelivered == nil {
		s.Undelivered = map[string]bool{}
	}
	if s.Delivered == nil {
		s.Delivered = map[string]string{}
	}
}

// attemptID is the logical id a notice is delivered under now: its own, then <id>-r<n> after n refusals, then ...-l<m> after m
// unloaded-thread refusals (a refused id is settled and is never replayed).
func (a pumpSupervisorAttempt) attemptID(base string, noticeLoaded int) string {
	id := base
	if a.Refused > 0 {
		id = fmt.Sprintf("%s-r%d", id, a.Refused)
	}
	if n := max(a.Loaded, noticeLoaded); n > 0 {
		id = fmt.Sprintf("%s-l%d", id, n)
	}
	return id
}

// supervisorSettingsRead reads the settings a delivery to the supervisor of a thread carries: the authorization recorded for the
// supervision binding of that thread, in the relay. A test replaces it.
var supervisorSettingsRead = supervisorSettingsFromRelay

// supervisorSettingsFromRelay is the real read: the relay store is opened read-only and the binding's authorized settings are read.
func supervisorSettingsFromRelay(ctx context.Context, e *Env, cfg *Config, task string) (coreSettings, error) {
	state, err := e.relayHelperState(ctx, cfg)
	if err != nil {
		return coreSettings{}, err
	}
	return supervisorSettingsAt(ctx, state, task)
}

// supervisorQueueDirOf is the queue directory of a thread.
func supervisorQueueDirOf(cfg *Config, thread string) string {
	return crwconfig.JoinRoot(cfg.StateDir, supervisorQueueDir, thread)
}

// supervisorQueueSafe refuses a queue path that would not stay under the state directory.
func supervisorQueueSafe(cfg *Config, thread, logicalID string) error {
	if err := deliverPathComponentLimit(thread, "thread", deliverThreadLimit); err != nil {
		return err
	}
	if err := deliverPathComponent(logicalID, "logical id"); err != nil {
		return err
	}
	root := crwconfig.JoinRoot(cfg.StateDir, supervisorQueueDir)
	if err := sendParentNoSymlink(root, "supervisor queue directory"); err != nil {
		return err
	}
	return sendParentNoSymlink(crwconfig.JoinRoot(root, thread), "supervisor queue thread directory")
}

// supervisorQueueWrite writes the notice into the queue of its thread. A notice of the same logical id and the same text is already
// queued (or delivered): nothing is written, and the state it is in is returned (pending, sent, refused). A notice of the same id with
// other text is refused.
func supervisorQueueWrite(cfg *Config, n supervisorNotice) (state string, err error) {
	if state, found, err := supervisorQueueFind(cfg, n); err != nil || found {
		return state, err
	}
	n.Schema, n.Role = supervisorNoticeSchema, supervisorRole
	body, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return "", err
	}
	return "queued", deliverWriteAtomic(crwconfig.JoinRoot(supervisorQueueDirOf(cfg, n.Thread), n.LogicalID+".json"), append(body, 0x0a))
}

// supervisorQueueFind looks for the notice of the logical id in the queue of its thread: pending, sent or refused. Another text under
// the same id is an error.
func supervisorQueueFind(cfg *Config, n supervisorNotice) (state string, found bool, err error) {
	if err := supervisorQueueSafe(cfg, n.Thread, n.LogicalID); err != nil {
		return "", false, err
	}
	dir := supervisorQueueDirOf(cfg, n.Thread)
	for _, probe := range []struct{ sub, state string }{{pumpSentDir, "sent"}, {supervisorRefusedDir, "refused"}, {"", "pending"}} {
		raw, err := os.ReadFile(crwconfig.JoinRoot(dir, probe.sub, n.LogicalID+".json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		var existing supervisorNotice
		if json.Unmarshal(raw, &existing) != nil || existing.Text != n.Text {
			return "", false, fmt.Errorf("crw manage: the logical id %q already belongs to another supervisor notice", n.LogicalID)
		}
		return probe.state, true, nil
	}
	return "", false, nil
}

// supervisorQueueNames lists the pending notices of a thread, oldest first.
func supervisorQueueNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type item struct {
		name string
		at   time.Time
	}
	var items []item
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{entry.Name(), info.ModTime()})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].at.Equal(items[j].at) {
			return items[i].at.Before(items[j].at)
		}
		return items[i].name < items[j].name
	})
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
	}
	return names, nil
}

func supervisorQueueRead(dir, name string) (supervisorNotice, error) {
	var n supervisorNotice
	raw, err := os.ReadFile(crwconfig.JoinRoot(dir, name))
	if err != nil {
		return n, err
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return n, fmt.Errorf("the supervisor notice %s is not JSON: %w", name, err)
	}
	if n.LogicalID == "" || n.Text == "" || n.Thread == "" {
		return n, fmt.Errorf("the supervisor notice %s lacks its logical id, thread or text", name)
	}
	return n, nil
}

// supervisorMove moves a notice out of the queue into sent/ or refused/.
func supervisorMove(dir, name, sub string) error {
	dest := crwconfig.JoinRoot(dir, sub)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	return os.Rename(crwconfig.JoinRoot(dir, name), crwconfig.JoinRoot(dest, name))
}

// supervisorUnloaded says whether a delivery's receipt is the bridge's refusal of a send to a thread that unloaded after the probe.
func supervisorUnloaded(out Outcome) bool {
	return out.Receipt != nil && deliverRPCErrorCode(out.Receipt) == unloadedPairCode
}

// pumpSupervisorFlush delivers the queued notices of every thread of the supervisor queue; see the file comment. A cancelled round
// makes no durable change.
func pumpSupervisorFlush(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings, now time.Time, dry bool) error {
	root := crwconfig.JoinRoot(cfg.StateDir, supervisorQueueDir)
	if err := sendParentNoSymlink(root, "supervisor queue directory"); err != nil {
		return err
	}
	if !dry {
		pumpSupervisorConfirm(ctx, e, cfg, st)
	}
	threads, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, thread := range threads {
		if !thread.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := pumpSupervisorFlushThread(ctx, e, cfg, st, s, now, thread.Name(), dry); err != nil {
			pumpLog(cfg, "supervisor queue "+thread.Name()+": "+err.Error())
		}
	}
	return nil
}

func pumpSupervisorFlushThread(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings, now time.Time, thread string, dry bool) error {
	dir := supervisorQueueDirOf(cfg, thread)
	names, err := supervisorQueueNames(dir)
	if err != nil || len(names) == 0 {
		return err
	}
	if dry {
		fmt.Fprintf(e.Stdout, "supervisor queue %s %d notices\n", thread, len(names))
		return nil
	}
	observation, turnID, err := pumpQueueObserve(ctx, e, cfg, thread)
	if err != nil {
		// the probe failed: the notices stay queued and the thread is unmeasured, as a source whose read failed is
		pumpLog(cfg, "supervisor queue "+thread+" "+pumpSourceUnmeasured+": "+err.Error())
		return nil
	}
	for _, name := range names {
		n, err := supervisorQueueRead(dir, name)
		if err != nil {
			pumpLog(cfg, "supervisor queue "+thread+": "+err.Error())
			continue
		}
		created := supervisorNoticeTime(n, dir, name)
		switch {
		case observation == "active" && turnID != "":
			// an active thread is steered at once
		case observation == "idle":
			if now.Sub(created).Seconds() < float64(pumpSettingInt(s.MaxQueueSeconds, pumpDefaultMaxQueueSeconds)) {
				return nil
			}
		default:
			// notLoaded or anything that is neither: nothing is sent; the report is told to the management thread once
			pumpSupervisorUndelivered(cfg, st, n, observation, now.Sub(created))
			continue
		}
		done, err := pumpSupervisorDeliver(ctx, e, cfg, st, dir, name, n, observation, now.Sub(created))
		if err != nil {
			return err
		}
		if !done {
			// a notice that is not delivered holds the notices behind it, so their order stays
			return nil
		}
	}
	return nil
}

// supervisorNoticeTime is when a notice was queued.
func supervisorNoticeTime(n supervisorNotice, dir, name string) time.Time {
	if t, err := time.Parse(time.RFC3339, n.CreatedAt); err == nil {
		return t
	}
	if info, err := os.Stat(crwconfig.JoinRoot(dir, name)); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// pumpSupervisorDeliver sends one notice with the role supervisor and the settings its binding records now. done is whether the notice
// left the queue (delivered, or moved aside after too many refusals).
func pumpSupervisorDeliver(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, name string, n supervisorNotice, observation string, waited time.Duration) (done bool, err error) {
	task := firstNonEmpty(n.BindingTask, n.Thread)
	settings, err := supervisorSettingsRead(ctx, e, cfg, task)
	if err != nil {
		// no settings, no send: the notice stays queued and says why
		pumpLog(cfg, fmt.Sprintf("supervisor queue %s: %s: settings_unavailable (binding %s): %v", n.Thread, n.LogicalID, task, err))
		return false, nil
	}
	attempt := st.Supervisor.Attempts[n.LogicalID]
	if n.LoadRetry > 0 {
		// send-parent --role supervisor met the unloaded-thread refusal and queued the notice with the next ordinal: the lead's thread is
		// reported undelivered once here too, whether or not this attempt delivers
		pumpSupervisorUndelivered(cfg, st, n, "notLoaded", waited)
	}
	id := attempt.attemptID(n.LogicalID, n.LoadRetry)
	out, derr := Deliver(ctx, e, cfg, Message{LogicalID: id, Thread: n.Thread, Text: n.Text, Role: supervisorRole, Settings: settings})
	if derr != nil && out.Class == "" {
		return false, nil
	}
	pumpLog(cfg, fmt.Sprintf("supervisor queue thread=%s notice=%s request=%s class=%s", n.Thread, n.LogicalID, out.RequestID, out.Class))
	switch {
	case out.Class == deliverClassAccepted:
		if err := supervisorMove(dir, name, pumpSentDir); err != nil {
			return false, err
		}
		delete(st.Supervisor.Attempts, n.LogicalID)
		if n.ReportID != "" {
			st.Supervisor.Delivered[n.ReportID] = out.RequestID
		}
		if err := st.pumpSave(cfg); err != nil {
			return true, err
		}
		pumpSupervisorConfirm(ctx, e, cfg, st)
		return true, nil
	case supervisorUnloaded(out):
		// the thread unloaded between the probe and the send: not a refusal of the notice, so it is not counted against the refusal
		// limit; the next attempt goes out under a new id and the lead's thread is reported once
		attempt.Loaded++
		st.Supervisor.Attempts[n.LogicalID] = attempt
		pumpSupervisorUndelivered(cfg, st, n, "notLoaded", waited)
		return false, st.pumpSave(cfg)
	case out.Class == deliverClassRefused:
		attempt.Refused++
		if attempt.Refused >= pumpQueueRefusalLimit {
			if err := supervisorMove(dir, name, supervisorRefusedDir); err != nil {
				return false, err
			}
			delete(st.Supervisor.Attempts, n.LogicalID)
			pumpLog(cfg, fmt.Sprintf("supervisor queue %s: notice %s moved to %s after %d refusals", n.Thread, n.LogicalID, supervisorRefusedDir, attempt.Refused))
			if n.ReportID != "" {
				st.Supervisor.Delivered[n.ReportID] = "refused:" + out.RequestID
			}
			if err := st.pumpSave(cfg); err != nil {
				return true, err
			}
			pumpSupervisorConfirm(ctx, e, cfg, st)
			return true, nil
		}
		st.Supervisor.Attempts[n.LogicalID] = attempt
		return false, st.pumpSave(cfg)
	}
	// unknown: the next round reconciles the same id
	return false, nil
}

func firstNonEmpty(texts ...string) string {
	for _, t := range texts {
		if t != "" {
			return t
		}
	}
	return ""
}

// pumpSupervisorUndelivered raises the report_undelivered event of a notice once: the pump state remembers the report (else the logical
// id) so the next rounds do not raise it again.
func pumpSupervisorUndelivered(cfg *Config, st *pumpState, n supervisorNotice, observation string, waited time.Duration) {
	key := firstNonEmpty(n.ReportID, n.LogicalID)
	if st.Supervisor.Undelivered[key] {
		return
	}
	if observation == "" {
		observation = "unknown"
	}
	st.Supervisor.Undelivered[key] = true
	text := fmt.Sprintf("[flow] report undelivered report=%s initiative=%s thread=%s observation=%s waiting=%s\nThe supervisor queue holds the notice %s; it is delivered with the supervisor role when the thread is loaded.",
		firstNonEmpty(n.ReportID, "-"), firstNonEmpty(n.Initiative, "-"), n.Thread, observation, waited.Round(time.Second), n.LogicalID)
	st.Pending = append(st.Pending, pumpEvent{ID: "report_undelivered:" + key, Kind: pumpKindReportUndelivered, Text: text})
	if st.FirstAt == nil {
		first := float64(time.Now().Unix())
		st.FirstAt = &first
	}
	if err := st.pumpSave(cfg); err != nil {
		pumpLog(cfg, "supervisor queue: the state could not be saved: "+err.Error())
	}
}

// pumpFlowRun runs `crw flow <args>` with the running executable (the one flow command that writes flow_events from outside a run:
// the pump reads the relay store and writes none of it). A test replaces it.
var pumpFlowRun = func(ctx context.Context, e *Env, cfg *Config, args ...string) error {
	exe := e.Executable
	if exe == "" {
		return errors.New("the running executable is unknown")
	}
	argv := append([]string{"flow"}, args...)
	if cfg.Relay.State != "" {
		argv = append(argv, "--state", cfg.Relay.State)
	}
	if cfg.Relay.Socket != "" {
		argv = append(argv, "--socket", cfg.Relay.Socket)
	}
	cmd := exec.CommandContext(ctx, exe, argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()
	if err := coreChildEnv(cmd, cfg); err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crw flow %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// pumpSupervisorConfirm tells the flow rows that notices were delivered: for each report whose delivery the pump settled it calls
// `crw flow report-delivered`, the one writer of the delivery row. A call that fails stays in the state and is made again.
func pumpSupervisorConfirm(ctx context.Context, e *Env, cfg *Config, st *pumpState) {
	reports := make([]string, 0, len(st.Supervisor.Delivered))
	for report := range st.Supervisor.Delivered {
		reports = append(reports, report)
	}
	sort.Strings(reports)
	changed := false
	for _, report := range reports {
		request := st.Supervisor.Delivered[report]
		result := "accepted"
		if rest, ok := strings.CutPrefix(request, "refused:"); ok {
			result, request = "refused", rest
		}
		args := []string{"report-delivered", "--report", report, "--result", result, "--request-id", request}
		if err := pumpFlowRun(ctx, e, cfg, args...); err != nil {
			pumpLog(cfg, "supervisor queue: report-delivered "+report+": "+err.Error())
			continue
		}
		delete(st.Supervisor.Delivered, report)
		changed = true
	}
	if changed {
		if err := st.pumpSave(cfg); err != nil {
			pumpLog(cfg, "supervisor queue: the state could not be saved: "+err.Error())
		}
	}
}
