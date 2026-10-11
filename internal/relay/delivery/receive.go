package delivery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// receive (CRW-1194, R4) is claim, ack and verdict in one process. The caller is a parent in a
// delivery turn that holds a reviewed verdict: it gives its own turn id, which is the turn the claim
// is made from, the turn the acknowledgement is proved from and the turn the verdict is ruled from.
//
// Each step is the step's own command run over the same library function, in its own store
// transaction, so every refusal keeps its name and detail and every record is the one the command
// writes; what an earlier step recorded stays recorded when a later one refuses, as it does when the
// three commands are run one after another, and running receive again replays each step (the claim is
// already held, the acknowledgement is settled, the verdict is the one on record).
//
// The relay makes no proof for anyone: the proof is AckProof of the event and the caller's own turn,
// and ReceiveRequest has no field a proof could be brought in.

// ReceiveRequest is one receive.
type ReceiveRequest struct {
	Event string
	// AckTurn is the caller's own turn id.
	AckTurn string
	// Verdict is the verdict to rule; its Event and VerdictTurn are Event and AckTurn.
	Verdict VerdictRequest
}

// AckStep is the acknowledgement step: AckCommand over the host the caller reaches, or without one.
// The proof it is given is AckProof(event, ackTurn), computed by Receive.
type AckStep func(ctx context.Context, event, ackTurn, proof string) (Obj, error)

// PrepareReceive is everything receive judges before any step writes, and before the store is opened:
// the proof's inputs are checked as ack-proof checks them, then the verdict line is read as far as it can
// be without the store. It reads no store and writes nothing.
func PrepareReceive(req ReceiveRequest) (*PreparedVerdict, error) {
	if !eventIDPattern.MatchString(req.Event) {
		return nil, dispatch.Host("event id must be 32 lowercase hex characters")
	}
	if pyvalue.Strip(req.AckTurn) == "" {
		return nil, dispatch.Host("ack_turn_id must be a non-empty string")
	}
	verdict := req.Verdict
	verdict.Event, verdict.VerdictTurn = req.Event, req.AckTurn
	prepared, err := PrepareVerdict(verdict)
	if err != nil {
		return nil, err
	}
	if err := prepared.checkHead(); err != nil {
		return nil, err
	}
	return prepared, nil
}

// Receive runs claim, ack and verdict in turn and answers {"claim", "ack", "verdict"}, each the
// answer of its command. A step that refuses ends the receive with its refusal.
func Receive(ctx context.Context, ack *Ack, req ReceiveRequest, doAck AckStep) (Obj, error) {
	prepared, err := PrepareReceive(req)
	if err != nil {
		return nil, err
	}
	claim, err := ack.ClaimVerification(ctx, req.Event, req.AckTurn)
	if err != nil {
		return nil, err
	}
	acknowledged, err := doAck(ctx, req.Event, req.AckTurn, AckProof(req.Event, req.AckTurn))
	if err != nil {
		return nil, err
	}
	ruled, err := prepared.Record(ctx, ack)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "claim", Value: claim}, {Key: "ack", Value: acknowledged}, {Key: "verdict", Value: ruled}}, nil
}

// verdictFileKeys are the verdict command's options a verdict file may carry, by the option's name.
// The verdict's turn is not among them: it is the caller's turn, given once as --ack-turn.
var verdictFileKeys = []string{"verdict", "criterion", "finding", "criteria", "restoration", "reason", "expect-criteria-digest", "verified-head"}

// ReadVerdictFile reads receive's verdict file: a JSON object whose keys are the verdict command's
// options. criterion and finding are lists of strings as the command takes them; criteria is a JSON
// array of findings, or a string (the text of one, or @path to one); the others are strings. The
// file is read before any step writes, and one that cannot be read is a usage error.
func ReadVerdictFile(path, event, turn string) (VerdictRequest, error) {
	var req VerdictRequest
	raw, err := os.ReadFile(path)
	if err != nil {
		return req, verdictFileError("could not be read: " + err.Error())
	}
	// A new input, read strictly: one document with nothing after it, and no object repeating a key
	// (the shared loads would keep the last value of a repeated key and let a closing bracket trail).
	parsed, err := pyjson.Loads(string(raw), pyjson.LoadOptions{Numbers: pyjson.Int64FloatNumbers, RangeErrors: true, Unique: true, Trailing: pyjson.TrailingNothing})
	if err != nil {
		var repeated *pyjson.RepeatedKey
		if errors.As(err, &repeated) {
			return req, verdictFileError(fmt.Sprintf("gives %q twice", repeated.Key))
		}
		return req, verdictFileError("is not JSON: " + err.Error())
	}
	doc, ok := parsed.(Obj)
	if !ok {
		return req, verdictFileError(fmt.Sprintf("is a JSON %s, not an object of the verdict command's options", jsonType(parsed)))
	}
	req.Event, req.VerdictTurn = event, turn
	seen := map[string]bool{}
	for _, field := range doc {
		if !slices.Contains(verdictFileKeys, field.Key) {
			hint := ""
			if field.Key == "verdict-turn" {
				hint = ": the verdict is ruled from --ack-turn, the caller's own turn"
			}
			return req, verdictFileError(fmt.Sprintf("has the key %q, which is not an option of the verdict it carries (%s)%s", field.Key, strings.Join(verdictFileKeys, ", "), hint))
		}
		seen[field.Key] = true
		switch field.Key {
		case "verdict":
			text, ok := field.Value.(string)
			if !ok || !slices.Contains(verdicts, text) {
				return req, verdictFileError(fmt.Sprintf("has a verdict that is not one of %s", strings.Join(verdicts, ", ")))
			}
			req.Verdict = text
		case "criterion":
			if req.Criterion, err = fileList(field); err != nil {
				return req, err
			}
		case "finding":
			if req.Finding, err = fileList(field); err != nil {
				return req, err
			}
		case "criteria":
			text, isText := field.Value.(string)
			if !isText {
				text = dumps(field.Value)
			}
			req.Criteria = &text
		case "restoration":
			text, err := fileText(field)
			if err != nil {
				return req, err
			}
			req.Restoration = &text
		case "verified-head":
			text, err := fileText(field)
			if err != nil {
				return req, err
			}
			req.VerifiedHead = &text
		case "reason":
			text, err := fileText(field)
			if err != nil {
				return req, err
			}
			req.Reason = text
		case "expect-criteria-digest":
			text, err := fileText(field)
			if err != nil {
				return req, err
			}
			req.ExpectCriteriaDigest = text
		}
	}
	if !seen["verdict"] {
		return req, verdictFileError("has no verdict")
	}
	return req, nil
}

func verdictFileError(detail string) error {
	return &dispatch.UsageError{Detail: "--verdict-file " + detail, Code: contract.ExitUsage}
}

func fileText(field F) (string, error) {
	text, ok := field.Value.(string)
	if !ok {
		return "", verdictFileError(fmt.Sprintf("has %q as a JSON %s, not a string", field.Key, jsonType(field.Value)))
	}
	return text, nil
}

func fileList(field F) ([]string, error) {
	items, ok := field.Value.([]any)
	if !ok {
		return nil, verdictFileError(fmt.Sprintf("has %q as a JSON %s, not a list of strings", field.Key, jsonType(field.Value)))
	}
	list := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, verdictFileError(fmt.Sprintf("has a %s in %q, not a string", jsonType(item), field.Key))
		}
		list = append(list, text)
	}
	return list, nil
}

// receiveCommands is the relay command of this package that composes the three above.
var receiveCommands = []commandSpec{
	{dispatch.Command{Name: "receive", Validate: validateReceive}, cmdReceive},
}

// validateReceive is receive's whole line judged before dispatch admits the store: admission creates a
// store, installs its schema and binds an unbound one to the line's socket, so a line that is refused
// (a verdict file that cannot be read, a malformed event id, turn or head) must be refused first.
func validateReceive(_ dispatch.Services, args dispatch.Args) error {
	event, turn := args.Text("event"), args.Text("ack-turn")
	req, err := ReadVerdictFile(args.Text("verdict-file"), event, turn)
	if err != nil {
		return err
	}
	_, err = PrepareReceive(ReceiveRequest{Event: event, AckTurn: turn, Verdict: req})
	return err
}

func cmdReceive(c *cliRun) (any, error) {
	event, turn := c.s("--event"), c.s("--ack-turn")
	// the verdict file was read by validateReceive before dispatch admitted the store; it is read again for its request
	req, err := ReadVerdictFile(c.s("--verdict-file"), event, turn)
	if err != nil {
		return nil, err
	}
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	doAck := func(ctx context.Context, event, ackTurn, proof string) (Obj, error) {
		// With a socket the acknowledgement is verified against the host, as `ack --socket` does it.
		if c.socket != "" && HostCommand != nil {
			out, err := HostCommand(ctx, "ack", c.state, c.socket, map[string]any{"--event": event, "--ack-turn": ackTurn, "--ack-proof": proof, "--reject": nil}, c.clock)
			if err != nil {
				return nil, err
			}
			record, _ := out.(Obj)
			return record, nil
		}
		return AckCommand(ctx, ack, nil, nil, event, ackTurn, proof, nil)
	}
	return Receive(c.ctx, ack, ReceiveRequest{Event: event, AckTurn: turn, Verdict: req}, doAck)
}
