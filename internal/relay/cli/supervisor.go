package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// answerValue is a supervisor or merge-evidence answer as contract.Emit can write it. Emit prints a
// map with its keys in sorted order, so no key order is rebuilt here (decision R3D-2). What is kept
// is what a plain map would change or what Emit cannot write: a nil map is JSON null (an absent
// record, not an empty one), and an obligation and a stage result are the plain maps they stand
// for. A stored document decoded by decodeJSON (contract.OrderedObject) is left as it is, in its
// stored order.
func answerValue(value any) any {
	switch v := value.(type) {
	case *supervisor.Obligation:
		if v == nil {
			return nil
		}
		return answerValue(map[string]any{"schema": v.Schema, "obligationId": v.ID, "kind": v.Kind, "relationId": v.RelationID, "subject": v.Subject, "executionGeneration": v.Generation, "revisionHash": optionalString(v.Revision), "issueKey": optionalString(v.Issue), "basis": v.Basis, "detail": v.Detail})
	case supervisor.StageResult:
		return answerValue(map[string]any(v))
	case map[string]any:
		if v == nil {
			return nil
		}
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = answerValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = answerValue(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = answerValue(item)
		}
		return out
	}
	return value
}

// optionalString is a nullable string as JSON has it: the string, or null.
func optionalString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
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
	return answerValue(answer), nil
}

// observationFile accepts any JSON value; its shape is the obligation reader's to interpret.
func observationFile(path string) (any, error) {
	unreadable := func(detail string) (any, error) {
		return nil, &dispatch.UsageError{Detail: fmt.Sprintf("the observation at %q could not be read as a reporting-observation/1 record: %s", path, detail), Code: contract.ExitUsage}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return unreadable(err.Error())
	}
	reading, err := decodeInput(data)
	if err != nil {
		return unreadable(err.Error())
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

var supervisorStandingCommand = dispatch.Command{Name: "supervisor-standing", ReadOnly: true, Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	project, _ := args.String("project")
	paths := args.Strings("observation")
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

var supervisorSelectCommand = dispatch.Command{Name: "supervisor-select", ReadOnly: true, Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
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

var supervisorReportRecordedCommand = dispatch.Command{Name: "supervisor-report-recorded", Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	event, hasEvent := args.TruthyString("event")
	observation, hasObservation := args.TruthyString("observation")
	if !hasEvent && !hasObservation {
		return nil, &dispatch.UsageError{Detail: "one of the arguments --event --observation is required", Code: 2}
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
		about := fmt.Sprintf("event %q", event)
		if hasObservation {
			about = fmt.Sprintf("the observation at %q", observation)
		}
		return nil, &dispatch.UsageError{Detail: about + " raises no obligation: an event has to be a completion, a new block or a decision the user owes, and an observation has to report state unreported. There is nothing here to record a report against", Code: contract.ExitUsage}
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
	return answerValue(result), nil
}}

func supervisorChannel(ctx context.Context, services dispatch.Services) (*supervisor.Channel, func(), error) {
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

var supervisorStageCommand = dispatch.Command{
	Name: "supervisor-stage",
	Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
		event, hasEvent := args.TruthyString("event")
		project, hasProject := args.TruthyString("project")
		observations := args.Strings("observation")
		hasObservation := len(observations) > 0
		recipient, hasRecipient := args.TruthyString("recipient")
		if hasEvent && hasProject {
			return nil, &dispatch.UsageError{Detail: "argument --project: not allowed with argument --event", Code: 2}
		}
		if hasEvent && hasObservation {
			return nil, &dispatch.UsageError{Detail: "--event and --observation are two different subjects: one obligation comes from an event in this store and the other from a turn that left no event at all. Name one", Code: contract.ExitUsage}
		}
		if hasProject && hasRecipient {
			return nil, &dispatch.UsageError{Detail: "--recipient names the supervisor ONE message is addressed to, and --project stages every standing obligation, each resolved through its own relationship. Ignoring the one you typed is how a caller learns too late that it was never checked", Code: contract.ExitUsage}
		}
		if !hasEvent && !hasProject && len(observations) > 1 {
			return nil, &dispatch.UsageError{Detail: "one obligation is one message, so a single staging takes one observation. Pass --project to stage several, where each reading is placed by the relationship it names", Code: contract.ExitUsage}
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
			return nil, &dispatch.UsageError{Detail: "supervisor-stage needs a subject: --event for one event's obligation, --project for everything a project owes, or one --observation for the obligation a turn left by ending without reporting", Code: contract.ExitUsage}
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
			return answerValue(answer), nil
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
			about := fmt.Sprintf("event %q", event)
			if hasObservation {
				about = fmt.Sprintf("the observation at %q", observations[0])
			}
			return nil, &dispatch.UsageError{Detail: about + " raises no obligation, so there is nothing to stage. An event has to be a completion, a new block or a decision the user owes, and an observation has to report state unreported", Code: contract.ExitUsage}
		}
		if hasObservation {
			result, stageErr := c.StageWithReading(ctx, *o, readings[0], recipient, at)
			return answerValue(result), supervisorResult(stageErr)
		}
		result, err := c.Stage(ctx, *o, recipient, at)
		return answerValue(result), supervisorResult(err)
	},
}
var supervisorShowCommand = dispatch.Command{Name: "supervisor-show", ReadOnly: true, Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
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
			if !pyvalue.Truthy(readback["detail"]) {
				readback["detail"] = nil
			}
		} else {
			readback["detail"] = nil
		}
	}
	return answerValue(result), nil
}}

func requireSupervisorHost(services dispatch.Services, name, why string) error {
	if services.SocketPath == "" {
		return &dispatch.UsageError{Detail: name + " needs --socket: " + why, Code: contract.ExitUsage}
	}
	return nil
}

// SupervisorHostCommand is installed by the executable's adapter composition.
// Keeping the seam here avoids coupling the CLI package back to its host implementation.
var SupervisorHostCommand func(context.Context, string, string, string, string, map[string]string, float64) (any, error)

// SupervisorClock is the host-command clock. Production follows the process clock;
// the built-binary parity harness installs the same fixed clock as Python.
var SupervisorClock = clockNow

func runSupervisorHost(ctx context.Context, command string, services dispatch.Services, args map[string]string) (any, error) {
	result, err := SupervisorHostCommand(ctx, command, services.Selection.Path, services.SocketPath, services.Program, args, SupervisorClock())
	return answerValue(result), supervisorResult(err)
}

var supervisorSendCommand = dispatch.Command{Name: "supervisor-send", Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	if err := requireSupervisorHost(services, "supervisor-send", "a send observes the recipient's lifecycle and resumes its thread. Without a host every read fails, which reads as an unmeasured recipient and would record a withholding that describes this process rather than the task"); err != nil {
		return nil, err
	}
	message, _ := args.String("message")
	return runSupervisorHost(ctx, "supervisor-send", services, map[string]string{"message": message})
}}

// readbackNeedsHost is why supervisor-read refuses to run without --socket.
const readbackNeedsHost = "a readback is checked against the host's own turn list and the recipient's transcript. Without a host it would record an unverified readback, which is a statement about this process and reads as one about the recipient"

var supervisorReadCommand = dispatch.Command{Name: "supervisor-read", Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	if err := requireSupervisorHost(services, "supervisor-read", readbackNeedsHost); err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, name := range []string{"message", "turn", "proof", "as"} {
		values[name], _ = args.String(name)
	}
	return runSupervisorHost(ctx, "supervisor-read", services, values)
}}
