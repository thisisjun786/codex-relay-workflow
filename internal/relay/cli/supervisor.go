package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// supervisorOrdered restores the order of each public Python record, not one
// preferred-key order shared by unrelated record types. Persisted JSON is decoded
// separately with decodeJSON, preserving its actual order and integer tokens.
func supervisorOrdered(value any) any {
	switch v := value.(type) {
	case *supervisor.Obligation:
		if v == nil {
			return nil
		}
		return supervisorOrdered(map[string]any{"schema": v.Schema, "obligationId": v.ID, "kind": v.Kind, "relationId": v.RelationID, "subject": v.Subject, "executionGeneration": v.Generation, "revisionHash": v.Revision, "issueKey": v.Issue, "basis": v.Basis, "detail": v.Detail})
	case map[string]any:
		if v == nil {
			return nil
		}
		order := supervisorObjectKeys(v)
		keys := strings.Fields(order)
		var extra []string
		for key := range v {
			if !slices.Contains(keys, key) {
				extra = append(extra, key)
			}
		}
		slices.Sort(extra)
		keys = append(keys, extra...)
		result := make(contract.OrderedObject, 0, len(v))
		for _, key := range keys {
			if item, ok := v[key]; ok {
				result = append(result, contract.Field{Key: key, Value: supervisorOrdered(item)})
			}
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = supervisorOrdered(item)
		}
		return result
	case supervisor.StageResult:
		return supervisorOrdered(map[string]any(v))
	case []map[string]any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = supervisorOrdered(item)
		}
		return result
	case []string:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = item
		}
		return result
	case *int:
		if v == nil {
			return nil
		}
		return *v
	case *int64:
		if v == nil {
			return nil
		}
		return *v
	case *string:
		if v == nil {
			return nil
		}
		return *v
	default:
		return value
	}
}

func supervisorObjectKeys(v map[string]any) string {
	present := func(key string) bool { _, ok := v[key]; return ok }
	switch {
	case present("message_id"):
		return "message_id obligation_id obligation_kind relationship_id project_key purpose kind sender_task_id recipient_task_id subject packet state attempt_count next_eligible_at hold_reason lease_owner lease_until staged_at updated_at event_id submission_no reading"
	case present("stagedAt"):
		return "schema messageId obligationId kind relationshipId projectKey purpose sender recipient state turnOrigin readEstablishes holdReason stagedAt packet stagedFrom attempts readback reach limits"
	case present("staged") && present("projectKey"):
		return "schema projectKey staged refused gaps limits"
	case present("staged"):
		return "schema staged readdressed restated messageId from to fromEvent toEvent reason message recipient sender"
	case present("recorded") && present("readTurnId"):
		return "schema messageId recorded raced verified readTurnId turnOrigin detail delivered readAt assertedBy reconciled establishes limits"
	case present("recorded"):
		return "recorded seq obligation"
	case present("relations"):
		return "schema projectKey relations standing gaps limits projectState answeredBecause"
	case present("relationId") && present("subject"):
		return "schema obligationId kind relationId subject executionGeneration revisionHash issueKey basis detail decision relationshipStatus supersededBy"
	case present("dischargeReason"):
		return "schema obligationId kind standing dischargeReason priorReport recipient report reason obligation"
	case present("candidate"):
		return "schema report reason obligationId detail candidate"
	case present("gap"):
		return "schema gap relationId obligationIds reason detail"
	case present("attached"):
		return "state readable projectKey attached outstanding competingOwners unfinished basis limits"
	case present("contactable"):
		return "deliverable observedAt contactable asked ageSeconds reason"
	case present("seq"):
		return "seq at detail"
	case present("table"):
		return "table eventId outcome cxcStatus schema reason turn"
	case present("attempted"):
		return "attempted sent schema requestId messageId attemptNo recipientTaskId deliveryState sendAttempted retrySafe transportReceiptStatus failedOperation turnId observedAt messageState"
	case present("attemptNo"):
		return "requestId attemptNo state sendAttempted retrySafe turnId sentAt transportStartedAt observedAt message"
	case present("readTurnId"):
		return "readTurnId verified turnOrigin establishes requestId readAt detail"
	case present("transport_accepted"):
		return "transport_accepted received agreed applied verified"
	case present("source") && present("state"):
		return "state source detail"
	case present("reading") && present("recheck"):
		return "reading recheck note"
	case present("sender") && present("projectKey"):
		return "sender recipient projectKey"
	case present("relationshipId") && present("state"):
		return "relationshipId state"
	case present("obligationId") && present("reason"):
		return "obligationId kind reason detail"
	case present("repository") && present("handoffGuidance"):
		return "repository number url observation pinned reread handoffGuidance gates supersededRuns connections handoff findings checkDetail problems verdict provenance restatement"
	case present("startedAt") && present("atomic"):
		return "startedAt finishedAt atomic note"
	case present("mergeStateStatus"):
		return "number url state merged isDraft headSha baseSha baseRef mergeable mergeStateStatus verifiedAt"
	case present("baseRefExists") && !present("threadResolutionNote"):
		return "readable requiredDeclared strictBase baseRefExists threadResolutionRequired requiredProviders digest"
	case present("baseRefExists"):
		return "readable baseRefExists requiredDeclared requiredProviders strictBase threadResolutionRequired threadResolutionNote digest"
	case present("connection") && present("pagesRead"):
		return "connection pagesRead totalCount distinct complete pages"
	case present("token") && present("returned"):
		return "token returned totalCount"
	case present("baseVerifiedAt"):
		return "isDraft baseVerifiedAt baseSha baseRef reviewCoverage checks requiredDeclared requiredProviders threadDispositions criterionEvidence limitations"
	case present("hasNextPage") && present("threadsSeen"):
		return "hasNextPage pagesRead totalCount threadsSeen unresolved"
	case v["kind"] == "reviewThread":
		return "kind id resolved outdated path line author url excerpt"
	case v["kind"] == "review":
		return "kind id state author url submittedAt excerpt"
	case v["kind"] == "comment":
		return "kind id author url createdAt excerpt"
	case present("source") && present("workflowRunUrl"):
		return "source runId name superseded status conclusion attempt startedAt completedAt url workflowRunUrl workflowName"
	case v["source"] == "check-run":
		return "source runId name superseded status conclusion attempt startedAt completedAt url app provider"
	case v["source"] == "commit-status":
		return "source runId name superseded status conclusion attempt url updatedAt"
	case present("runId") && present("provider"):
		return "runId name headSha conclusion attempt provider"
	case present("workflowId"):
		return "runId workflowId event url conclusion"
	case present("calls") && present("disabledReviewers"):
		return "calls disabledReviewers conflictingRequiredReviewers"
	case present("argv") && present("exitCode"):
		return "argv exitCode"
	case present("headSha") && present("current"):
		return "headSha current problems"
	case present("code") && present("detail"):
		return "code detail incumbent"
	default:
		return ""
	}
}

// Prior report detail is persisted JSON, not a newly constructed public record.
// Keep its actual insertion order, including rows written by older relay builds.
func supervisorOutput(ctx context.Context, c *supervisor.Channel, answer map[string]any) (any, error) {
	var preserve func(any) error
	preserve = func(value any) error {
		switch v := value.(type) {
		case map[string]any:
			if prior, ok := v["priorReport"].(map[string]any); ok && prior != nil {
				var raw string
				if err := c.Store.DB.QueryRowContext(ctx, "SELECT detail FROM journal WHERE seq=?", prior["seq"]).Scan(&raw); err != nil {
					return err
				}
				if raw != "" {
					detail, err := decodeJSON([]byte(raw))
					if err == nil {
						prior["detail"] = detail
					} else {
						prior["detail"] = contract.OrderedObject{{Key: "detail", Value: raw}}
					}
				}
			}
			for _, item := range v {
				if err := preserve(item); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range v {
				if err := preserve(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := preserve(answer); err != nil {
		return nil, err
	}
	return supervisorOrdered(answer), nil
}

// observationFile accepts any JSON value, as Python's json.load does. Shape is
// interpreted by the obligation reader, not by the file decoder.
func observationFile(path string) (any, error) {
	unreadable := func(detail string) (any, error) {
		return nil, &UsageError{Detail: "the observation at " + store.PythonRepr(path) + " could not be read as a reporting-observation/1 record: " + detail, Code: contract.ExitUsage}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return unreadable(store.PythonOSError(err))
	}
	if !utf8.Valid(data) {
		for i := 0; i < len(data); {
			_, size := utf8.DecodeRune(data[i:])
			if size == 1 && data[i] >= utf8.RuneSelf {
				cause := "invalid start byte"
				if data[i] >= 0xc2 && data[i] <= 0xf4 && i+1 == len(data) {
					cause = "unexpected end of data"
				} else if data[i] >= 0xc2 && data[i] <= 0xf4 && i+1 < len(data) && data[i+1]&0xc0 != 0x80 {
					cause = "invalid continuation byte"
				}
				return unreadable(fmt.Sprintf("UnicodeDecodeError: 'utf-8' codec can't decode byte 0x%02x in position %d: %s", data[i], i, cause))
			}
			i += size
		}
	}
	reading, err := decodeJSON(data)
	if err != nil {
		return unreadable("JSONDecodeError: " + jsonErrorText(data, err))
	}
	return observationMaps(reading), nil
}

// Preserve numeric kinds while adapting the channel's map-shaped API.
func observationMaps(v any) any {
	switch x := v.(type) {
	case contract.OrderedObject:
		m := map[string]any{}
		for _, f := range x {
			m[f.Key] = f.Value
			if f.Key == "selectors" {
				m[f.Key] = observationMaps(f.Value)
			}
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, one := range x {
			out[i] = observationMaps(one)
		}
		return out
	default:
		return v
	}
}

// Todo 24's offline channel commands and the host gate shared by send and read.
func observationMapSlice(values []any) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if reading, ok := value.(map[string]any); ok {
			out = append(out, reading)
		}
	}
	return out
}

var supervisorStandingCommand = Command{Name: "supervisor-standing", Required: []string{"project"}, Flags: func(f *flag.FlagSet) { f.String("project", "", ""); f.Var(new(stringsFlag), "observation", "") }, Run: func(ctx context.Context, services Services, args Args) (any, error) {
	project, _ := args.String("project")
	paths := []string(*args.Flags.Lookup("observation").Value.(*stringsFlag))
	readings := make([]any, 0, len(paths))
	for _, path := range paths {
		reading, err := observationFile(path)
		if err != nil {
			return nil, err
		}
		readings = append(readings, reading)
	}
	c, close, err := supervisorChannel(ctx, services)
	if err != nil {
		return nil, err
	}
	defer close()
	derived, err := c.OmissionReadingsExcept(ctx, project, delivery.ISOOf(clockNow()), 300, observationMapSlice(readings))
	if err != nil {
		return nil, err
	}
	for _, reading := range derived {
		readings = append(readings, reading)
	}
	answer, err := c.StatusAnswer(ctx, project, readings)
	if err != nil {
		return nil, err
	}
	holds, err := c.ReportHolds(ctx, answer)
	if err != nil {
		return nil, err
	}
	for _, hold := range holds {
		answer["gaps"] = append(answer["gaps"].([]any), hold)
	}
	return supervisorOutput(ctx, c, answer)
}}

var supervisorSelectCommand = Command{Name: "supervisor-select", Required: []string{"event"}, Flags: func(f *flag.FlagSet) { f.String("event", "", ""); f.String("recipient", "", "") }, Run: func(ctx context.Context, services Services, args Args) (any, error) {
	event, _ := args.String("event")
	recipient, _ := args.String("recipient")
	c, close, err := supervisorChannel(ctx, services)
	if err != nil {
		return nil, err
	}
	defer close()
	answer, err := c.SelectEvent(ctx, event, recipient, float64(time.Now().UnixMicro())/1e6)
	if err != nil {
		return nil, err
	}
	return supervisorOutput(ctx, c, answer)
}}
var recordSupervisorReport = func(ctx context.Context, c *supervisor.Channel, o supervisor.Obligation, at string, message, note *string) (map[string]any, error) {
	return c.RecordReport(ctx, o, at, message, note)
}

var supervisorReportRecordedCommand = Command{Name: "supervisor-report-recorded", Flags: func(f *flag.FlagSet) {
	f.String("event", "", "")
	f.String("observation", "", "")
	f.String("message", "", "")
	f.String("note", "", "")
}, Run: func(ctx context.Context, services Services, args Args) (any, error) {
	event, hasEvent := args.TruthyString("event")
	observation, hasObservation := args.TruthyString("observation")
	if !hasEvent && !hasObservation {
		return nil, &UsageError{Detail: "one of the arguments --event --observation is required", Code: 2}
	}
	var reading map[string]any
	if hasObservation {
		value, err := observationFile(observation)
		if err != nil {
			return nil, err
		}
		reading, _ = value.(map[string]any)
	}
	c, close, err := supervisorChannel(ctx, services)
	if err != nil {
		return nil, err
	}
	defer close()
	var o *supervisor.Obligation
	if hasEvent {
		o, err = c.FromEvent(ctx, event)
		if err != nil {
			return nil, err
		}
	} else {
		o = supervisor.ObservationObligation(reading)
	}
	if o == nil {
		about := "event " + store.PythonRepr(event)
		if hasObservation {
			about = "the observation at " + store.PythonRepr(observation)
		}
		return nil, &UsageError{Detail: about + " raises no obligation: an event has to be a completion, a new block or a decision the user owes, and an observation has to report state unreported. There is nothing here to record a report against", Code: contract.ExitUsage}
	}
	message, hasMessage := args.String("message")
	note, hasNote := args.String("note")
	var messagePtr, notePtr *string
	if hasMessage {
		messagePtr = &message
	}
	if hasNote {
		notePtr = &note
	}
	result, err := recordSupervisorReport(ctx, c, *o, delivery.ISOOf(float64(time.Now().UnixMicro())/1e6), messagePtr, notePtr)
	if err != nil {
		return nil, supervisorResult(err)
	}
	result["obligation"] = o
	return supervisorOrdered(result), nil
}}

func supervisorChannel(ctx context.Context, services Services) (*supervisor.Channel, func(), error) {
	opened, err := openStore(ctx, services)
	if err != nil {
		return nil, nil, err
	}
	return &supervisor.Channel{Store: opened, Linkage: supervisor.StoreLinkage{Store: opened}, Program: services.Program, Socket: services.SocketPath}, func() { _ = opened.Close() }, nil
}
func supervisorResult(err error) error {
	var refused supervisor.Refusal
	if errors.As(err, &refused) {
		return &store.RefusedError{Reason: refused.Reason, Detail: refused.Detail}
	}
	return err
}

var supervisorStageCommand = Command{
	Name: "supervisor-stage",
	Flags: func(f *flag.FlagSet) {
		f.String("event", "", "")
		f.String("project", "", "")
		f.Var(new(stringsFlag), "observation", "")
		f.String("recipient", "", "")
	},
	Run: func(ctx context.Context, services Services, args Args) (any, error) {
		event, hasEvent := args.TruthyString("event")
		project, hasProject := args.TruthyString("project")
		observations := []string(*args.Flags.Lookup("observation").Value.(*stringsFlag))
		hasObservation := len(observations) > 0
		recipient, hasRecipient := args.TruthyString("recipient")
		if hasEvent && hasProject {
			return nil, &UsageError{Detail: "argument --project: not allowed with argument --event", Code: 2}
		}
		if hasEvent && hasObservation {
			return nil, &UsageError{Detail: "--event and --observation are two different subjects: one obligation comes from an event in this store and the other from a turn that left no event at all. Name one", Code: contract.ExitUsage}
		}
		if hasProject && hasRecipient {
			return nil, &UsageError{Detail: "--recipient names the supervisor ONE message is addressed to, and --project stages every standing obligation, each resolved through its own relationship. Ignoring the one you typed is how a caller learns too late that it was never checked", Code: contract.ExitUsage}
		}
		if !hasEvent && !hasProject && len(observations) > 1 {
			return nil, &UsageError{Detail: "one obligation is one message, so a single staging takes one observation. Pass --project to stage several, where each reading is placed by the relationship it names", Code: contract.ExitUsage}
		}
		readings := make([]map[string]any, 0, len(observations))
		for _, path := range observations {
			value, err := observationFile(path)
			if err != nil {
				return nil, err
			}
			reading, _ := value.(map[string]any)
			readings = append(readings, reading)
		}
		if !hasEvent && !hasProject && !hasObservation {
			return nil, &UsageError{Detail: "supervisor-stage needs a subject: --event for one event's obligation, --project for everything a project owes, or one --observation for the obligation a turn left by ending without reporting", Code: contract.ExitUsage}
		}
		c, close, err := supervisorChannel(ctx, services)
		if err != nil {
			return nil, err
		}
		defer close()
		at := delivery.ISOOf(clockNow())
		if hasProject {
			derived, err := c.OmissionReadingsExcept(ctx, project, at, 300, readings)
			if err != nil {
				return nil, err
			}
			readings = append(readings, derived...)
			answer, err := c.StageStandingWithObservations(ctx, project, readings, at)
			if err != nil {
				return nil, err
			}
			return supervisorOrdered(answer), nil
		}
		var o *supervisor.Obligation
		if hasObservation {
			o = supervisor.ObservationObligation(readings[0])
		} else {
			o, err = c.FromEvent(ctx, event)
			if err != nil {
				return nil, err
			}
		}
		if o == nil {
			about := "event " + store.PythonRepr(event)
			if hasObservation {
				about = "the observation at " + store.PythonRepr(observations[0])
			}
			return nil, &UsageError{Detail: about + " raises no obligation, so there is nothing to stage. An event has to be a completion, a new block or a decision the user owes, and an observation has to report state unreported", Code: contract.ExitUsage}
		}
		if hasObservation {
			result, stageErr := c.StageWithReading(ctx, *o, readings[0], recipient, at)
			return supervisorOrdered(result), supervisorResult(stageErr)
		}
		result, err := c.Stage(ctx, *o, recipient, at)
		return supervisorOrdered(result), supervisorResult(err)
	},
}
var supervisorShowCommand = Command{Name: "supervisor-show", Required: []string{"message"}, Flags: func(f *flag.FlagSet) { f.String("message", "", "") }, Run: func(ctx context.Context, services Services, args Args) (any, error) {
	id, _ := args.String("message")
	c, close, err := supervisorChannel(ctx, services)
	if err != nil {
		return nil, err
	}
	defer close()
	result, err := c.Show(ctx, id)
	if err != nil {
		return nil, supervisorResult(err)
	}
	// Show's domain answer uses maps; the frozen documents already have their
	// own byte order and number kinds in the store. Do not round-trip via float64.
	row, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	result["packet"], err = decodeJSON([]byte(row.Packet))
	if err != nil {
		return nil, err
	}
	if frozen, ok := result["stagedFrom"].(map[string]any); ok {
		frozen["reading"], err = decodeJSON([]byte(row.Reading.String))
		if err != nil {
			return nil, err
		}
		frozen["note"] = "the reading this report was composed from, frozen when it was staged. recheck re-reads the turn now and can answer differently - a report that reached the turn afterwards is news about the turn, and does not change what this message said"
	}
	if readback, ok := result["readback"].(map[string]any); ok {
		stored, err := c.Store.SupervisorReadback(ctx, id)
		if err != nil {
			return nil, err
		}
		if stored.Detail.Valid && stored.Detail.String != "" {
			readback["detail"], err = decodeJSON([]byte(stored.Detail.String))
			if err != nil {
				return nil, err
			}
			if !truthy(readback["detail"]) {
				readback["detail"] = nil
			}
		} else {
			readback["detail"] = nil
		}
	}
	return supervisorOrdered(result), nil
}}

func requireSupervisorHost(services Services, name, why string) error {
	if services.SocketPath == "" {
		return &UsageError{Detail: name + " needs --socket: " + why, Code: contract.ExitUsage}
	}
	return nil
}

// SupervisorHostCommand is installed by the executable's adapter composition.
// Keeping the seam here avoids coupling the CLI package back to its host implementation.
var SupervisorHostCommand func(context.Context, string, string, string, string, map[string]string, float64) (any, error)

// SupervisorClock is the host-command clock. Production follows the process clock;
// the built-binary parity harness installs the same fixed clock as Python.
var SupervisorClock = clockNow

func runSupervisorHost(ctx context.Context, command string, services Services, args map[string]string) (any, error) {
	result, err := SupervisorHostCommand(ctx, command, services.Selection.Path, services.SocketPath, services.Program, args, SupervisorClock())
	return supervisorOrdered(result), supervisorResult(err)
}

var supervisorSendCommand = Command{Name: "supervisor-send", Required: []string{"message"}, Flags: func(f *flag.FlagSet) { f.String("message", "", "") }, Run: func(ctx context.Context, services Services, args Args) (any, error) {
	if err := requireSupervisorHost(services, "supervisor-send", "a send observes the recipient's lifecycle and resumes its thread. Without a host every read fails, which reads as an unmeasured recipient and would record a withholding that describes this process rather than the task"); err != nil {
		return nil, err
	}
	message, _ := args.String("message")
	return runSupervisorHost(ctx, "supervisor-send", services, map[string]string{"message": message})
}}

// readbackNeedsHost is why supervisor-read refuses to run without --socket.
const readbackNeedsHost = "a readback is checked against the host's own turn list and the recipient's transcript. Without a host it would record an unverified readback, which is a statement about this process and reads as one about the recipient"

var supervisorReadCommand = Command{Name: "supervisor-read", Required: []string{"message", "turn", "proof", "as"}, Flags: func(f *flag.FlagSet) {
	f.String("message", "", "")
	f.String("turn", "", "")
	f.String("proof", "", "")
	f.String("as", "", "")
}, Run: func(ctx context.Context, services Services, args Args) (any, error) {
	if err := requireSupervisorHost(services, "supervisor-read", readbackNeedsHost); err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, name := range []string{"message", "turn", "proof", "as"} {
		values[name], _ = args.String(name)
	}
	return runSupervisorHost(ctx, "supervisor-read", services, values)
}}
