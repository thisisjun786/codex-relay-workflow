package delivery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// The twelve delivery commands of `codex-session-relay` (cli.py: emit, deliver, reconcile,
// recover, claim, ack-proof, ack, verdict, criteria-register, criteria-show, revision-head,
// verify-acks), registered in the relay command table with their argparse defaults; argparse's
// parsing rules and main()'s JSON replies and exit codes are the table's.

// commandSpec is one relay command of this package: its registration and its handler.
type commandSpec struct {
	dispatch.Command
	run func(*cliRun) (any, error)
}

var deliveryCommands = []commandSpec{
	{dispatch.Command{Name: "emit", Defaults: map[string]any{"attempt": int64(1), "turn-status": "inProgress"}}, cmdEmit},
	{dispatch.Command{Name: "deliver", Defaults: map[string]any{"limit": int64(4)}}, needsHost},
	{dispatch.Command{Name: "reconcile"}, needsHost},
	{dispatch.Command{Name: "recover"}, needsHost},
	{dispatch.Command{Name: "claim"}, cmdClaim},
	// ack-proof derives a proof from its two options alone: it resolves no state directory.
	{dispatch.Command{Name: "ack-proof", Unselected: true, ReadOnly: true}, cmdAckProof},
	{dispatch.Command{Name: "ack"}, cmdAck},
	{dispatch.Command{Name: "verdict"}, cmdVerdict},
	{dispatch.Command{Name: "criteria-register"}, cmdCriteriaRegister},
	{dispatch.Command{Name: "criteria-show", ReadOnly: true}, cmdCriteriaShow},
	{dispatch.Command{Name: "revision-head", ReadOnly: true}, cmdRevisionHead},
	{dispatch.Command{Name: "verify-acks", Defaults: map[string]any{"limit": int64(8)}}, needsHost},
}

// family is this package's commands' family: a failure no other ending classifies reads in
// Python's words (hostDetail).
var family = &dispatch.Family{HostDetail: hostDetail}

func init() {
	for _, spec := range append(deliveryCommands, intentCommands...) {
		registration := spec.Command
		registration.Run = func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
			return spec.execute(ctx, services, args)
		}
		dispatch.Register(family, registration)
	}
}

// execute runs the handler over the selected store's directory, once the relay CLI checked it.
func (spec commandSpec) execute(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	run := &cliRun{command: spec.Name, ctx: ctx, args: argsOf(spec.Name, args), state: services.Selection.Path, socket: services.SocketPath, clock: cliClock}
	if CommandClock != nil {
		run.clock = CommandClock
	}
	defer func() {
		if run.store != nil {
			_ = run.store.Close()
		}
	}()
	return spec.run(run)
}

// CommandClock is an optional composition/test seam; nil keeps SystemClock.
var CommandClock Clock

type cliRun struct {
	command string

	ctx    context.Context
	args   map[string]any
	state  string
	socket string
	store  *store.Store
	clock  Clock
}

func (c *cliRun) s(name string) string      { v, _ := c.args[name].(string); return v }
func (c *cliRun) opt(name string) any       { return c.args[name] }
func (c *cliRun) list(name string) []string { v, _ := c.args[name].([]string); return v }

func (c *cliRun) openStore() (*store.Store, error) {
	if c.store == nil {
		s, err := store.Open(c.ctx, c.state+"/relay.sqlite3", c.socket)
		if err != nil {
			return nil, err
		}
		c.store = s
	}
	return c.store, nil
}

func (c *cliRun) services() (*Service, *Ack, error) {
	s, err := c.openStore()
	if err != nil {
		return nil, nil, err
	}
	d := NewService(s, c.clock)
	return d, NewAck(d), nil
}

// argsOf is the handlers' view of the line: every option of the command's parser under its
// flag, as argparse would bind it (an int as its Python integer, an append as its list, a
// store_true as a bool), or its default (nil when it has none).
func argsOf(command string, args dispatch.Args) map[string]any {
	out := map[string]any{}
	for _, action := range argparse.Specs[command].Actions {
		if len(action.Flags) == 0 || action.Kind == "_HelpAction" {
			continue
		}
		flag := action.Flags[len(action.Flags)-1]
		name := strings.TrimPrefix(flag, "--")
		switch {
		case action.Kind == "_StoreTrueAction":
			out[flag] = args.Bool(name)
		case !args.Given(name):
			out[flag] = args.Defaults[name]
		case action.Type == "int":
			out[flag] = args.Number(name)
		case action.Kind == "_AppendAction":
			out[flag] = args.Strings(name)
		default:
			out[flag] = args.Text(name)
		}
	}
	return out
}

// hostDetail is the host envelope's detail, in Python's words, for a failure no other ending
// classifies.
func hostDetail(err error) string {
	if encode := store.EncodeError(err); encode != nil {
		return encode.HostDetail()
	}
	var overflow *argparse.IntegerOverflow
	if errors.As(err, &overflow) {
		return overflow.Error()
	}
	// The fence's own exception: a bounded lock wait that expired.
	var expired *ownership.LockWaitExpired
	if errors.As(err, &expired) {
		return expired.Error()
	}
	if detail, ok := store.PythonHostDetail(err); ok {
		return detail
	}
	return "RuntimeError: " + err.Error()
}

// HostCommand is installed by the production adapter at executable composition time.
// Nil preserves the offline port's existing host-unavailable answer.
var HostCommand func(context.Context, string, string, string, map[string]any, Clock) (any, error)

// needsHost is the four commands that reach the App Server. Without --socket they are a usage
// error, as in Python; with it, the host adapter they drive is the bridge adapter port (todo 28).
func needsHost(c *cliRun) (any, error) {
	if c.socket == "" {
		return nil, &dispatch.UsageError{Detail: "this command needs --socket to reach the host", Code: contract.ExitUsage}
	}
	if HostCommand != nil {
		return HostCommand(c.ctx, c.command, c.state, c.socket, c.args, c.clock)
	}
	return nil, &dispatch.HostError{Class: "HostUnavailable", Detail: "the relay host adapter (bridge_adapter.py) is not ported to Go yet (todo 28)"}
}

// Python re.match's $ also matches immediately before one final LF.
var eventIDPattern = regexp.MustCompile(`^[0-9a-f]{32}\n?$`)

func cmdAckProof(c *cliRun) (any, error) {
	event, turn := c.s("--event"), c.s("--turn")
	if !eventIDPattern.MatchString(event) {
		return nil, &dispatch.HostError{Class: "ValueError", Detail: "event id must be 32 lowercase hex characters"}
	}
	if pyvalue.Strip(turn) == "" {
		return nil, &dispatch.HostError{Class: "ValueError", Detail: "ack_turn_id must be a non-empty string"}
	}
	return Obj{{Key: "eventId", Value: event}, {Key: "turnId", Value: turn}, {Key: "ackProof", Value: AckProof(event, turn)}}, nil
}

func cmdClaim(c *cliRun) (any, error) {
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	claim, err := ack.ClaimVerification(c.ctx, c.s("--event"), c.opt("--turn"))
	return Obj{{Key: "claim", Value: claim}}, err
}

func cmdAck(c *cliRun) (any, error) {
	if c.socket != "" && HostCommand != nil {
		return HostCommand(c.ctx, "ack", c.state, c.socket, c.args, c.clock)
	}
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	return AckCommand(c.ctx, ack, nil, nil, c.s("--event"), c.s("--ack-turn"), c.s("--ack-proof"), c.opt("--reject"))
}

// AckCommand is cmd_ack. With a host adapter and reconciler, a delivery the relay has not
// confirmed is reconciled first through the acknowledging turn (the proof is checked first,
// locally, so a wrong one makes no host read).
func AckCommand(ctx context.Context, ack *Ack, rc *Reconciler, adapter Adapter, event, ackTurn, proof string, reject any) (Obj, error) {
	if adapter != nil && rc != nil {
		// Python derives the proof before looking up the event or reading the host.
		if !eventIDPattern.MatchString(event) {
			return nil, &dispatch.HostError{Class: "ValueError", Detail: "event id must be 32 lowercase hex characters"}
		}
		if pyvalue.Strip(ackTurn) == "" {
			return nil, &dispatch.HostError{Class: "ValueError", Detail: "ack_turn_id must be a non-empty string"}
		}
	}
	if adapter != nil && rc != nil && proof == AckProof(event, ackTurn) {
		if _, err := rc.ConfirmDelivery(ctx, event, adapter, ackTurn); err != nil {
			return nil, err
		}
	}
	record, err := ack.Acknowledge(ctx, event, ackTurn, proof, !truthy(reject), reject, adapter)
	if err != nil {
		return nil, err
	}
	if why, ok := get(record, "_deliveryUnconfirmed"); ok && truthy(why) {
		record = append(record, F{Key: "_note", Value: "kept as the parent's authored acknowledgement: the relay could not yet confirm this delivery for the turn it read (" + pyStr(why) + "). The daemon completes it once the delivery is confirmed, and a verdict completes it first; nothing needs to be acknowledged or sent again."})
	} else if str(record, "_verified") != "verified" {
		record = append(record, F{Key: "_note", Value: "recorded as the parent's authored intent; this turn is not established yet, so it does not close the attempt and cannot yet produce a verdict. Run verify-acks from a process with host access."})
	}
	return record, nil
}

// VerifyAcksCommand is cmd_verify_acks: kept acknowledgements have their delivery confirmed
// through their turn first, then the pending pass completes what it can.
func VerifyAcksCommand(ctx context.Context, ack *Ack, rc *Reconciler, adapter Adapter, limit int) (Obj, error) {
	confirmations := []any{}
	kept, err := ack.KeptUnconfirmed(ctx, ack.Clock.Now(), limit)
	if err != nil {
		return nil, err
	}
	for _, k := range kept {
		outcome, err := rc.ConfirmDelivery(ctx, k[0], adapter, k[1])
		if err != nil {
			return nil, err
		}
		if outcome == nil {
			continue
		}
		picked := Obj{}
		for _, key := range []string{"eventId", "requestId", "confirmed", "turnRead", "error"} {
			if v, ok := get(outcome, key); ok {
				picked = append(picked, F{Key: key, Value: v})
			}
		}
		confirmations = append(confirmations, picked)
	}
	results, err := ack.VerifyPendingAcks(ctx, adapter, limit, nil)
	if err != nil {
		return nil, err
	}
	out := Obj{{Key: "results", Value: results}}
	if len(confirmations) > 0 {
		out = append(out, F{Key: "confirmations", Value: confirmations})
	}
	return out, nil
}

func cmdCriteriaRegister(c *cliRun) (any, error) {
	d, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	_ = d
	optional := c.list("--optional")
	var entries []any
	for _, item := range c.list("--criterion") {
		id, title, _ := strings.Cut(item, "=")
		entries = append(entries, Obj{{Key: "id", Value: strings.TrimSpace(id)}, {Key: "title", Value: strings.TrimSpace(title)}, {Key: "required", Value: !slices.Contains(optional, strings.TrimSpace(id))}})
	}
	return ack.Criteria.Register(c.ctx, c.s("--relationship"), entries, c.opt("--source-ref"))
}

func cmdCriteriaShow(c *cliRun) (any, error) {
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	rid := c.s("--relationship")
	registered, err := ack.Criteria.Get(c.ctx, rid)
	if err != nil {
		return nil, err
	}
	mode, err := ack.Criteria.Mode(c.ctx, rid)
	if err != nil {
		return nil, err
	}
	var criteria, digest, source any
	if registered != nil {
		criteria, _ = get(registered, "criteria")
		digest, _ = get(registered, "setDigest")
		source, _ = get(registered, "sourceRef")
	}
	return Obj{{Key: "relationshipId", Value: rid}, {Key: "mode", Value: mode}, {Key: "criteria", Value: criteria}, {Key: "setDigest", Value: digest}, {Key: "sourceRef", Value: source}}, nil
}

func cmdRevisionHead(c *cliRun) (any, error) {
	d, _, err := c.services()
	if err != nil {
		return nil, err
	}
	rid := c.s("--relationship")
	r, err := LoadRelationship(c.ctx, d.Store, rid)
	if err != nil {
		return nil, err
	}
	generation := r.Generation
	if v := c.opt("--generation"); v != nil {
		g := argparse.IntegerValue(v)
		if g.Sign() != 0 {
			generation, err = argparse.SQLiteInteger(g)
			if err != nil {
				return nil, err
			}
		}
	}
	head, err := HeadRevision(c.ctx, d.Store, rid, generation)
	return Obj{{Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: generation}, {Key: "head", Value: head}}, err
}

func cmdVerdict(c *cliRun) (any, error) {
	var criteria, findings []any
	for _, item := range c.list("--criterion") {
		name, value, _ := strings.Cut(item, "=")
		if value == "" {
			value = "verified"
		}
		criteria = append(criteria, Obj{{Key: "id", Value: name}, {Key: "verdict", Value: value}})
	}
	for _, item := range c.list("--finding") {
		name, rest, _ := strings.Cut(item, "=")
		disposition, note, _ := strings.Cut(rest, ":")
		if disposition == "" {
			disposition = "verified"
		}
		findings = append(findings, Obj{{Key: "id", Value: name}, {Key: "verdict", Value: disposition}, {Key: "note", Value: strings.TrimSpace(note)}})
	}
	if raw := c.s("--criteria"); c.opt("--criteria") != nil {
		text := raw
		if strings.HasPrefix(raw, "@") {
			content, err := os.ReadFile(raw[1:])
			if err != nil {
				return nil, &dispatch.HostError{Class: "FileNotFoundError", Detail: err.Error()}
			}
			text = string(content)
		}
		parsed, err := loads(text)
		if err != nil {
			return nil, &dispatch.HostError{Class: "JSONDecodeError", Detail: err.Error()}
		}
		switch v := parsed.(type) {
		case []any:
			findings = append(findings, v...)
		case Obj:
			for _, f := range v {
				findings = append(findings, f.Key)
			}
		default:
			return nil, &dispatch.HostError{Class: "TypeError", Detail: fmt.Sprintf("'%s' object is not iterable", pyvalue.TypeName(v))}
		}
	}
	if c.opt("--restoration") != nil {
		wanted := strings.TrimSpace(c.s("--restoration"))
		if wanted == "" {
			return nil, &dispatch.UsageError{Detail: "--restoration names the criterion id whose finding carries the block, so it cannot be empty. Leave the option out to carry no block", Code: contract.ExitUsage}
		}
		all := append(append([]any(nil), criteria...), findings...)
		var marked []Obj
		wellFormed := true
		for _, item := range all {
			o, ok := item.(Obj)
			if !ok {
				wellFormed = false
				continue
			}
			if id, _ := get(o, "id"); strings.TrimSpace(pyStrOrEmpty(id)) == wanted {
				marked = append(marked, o)
			}
		}
		if len(marked) == 0 && wellFormed {
			return nil, &dispatch.UsageError{Detail: "--restoration names " + pyvalue.StrRepr(c.s("--restoration")) + ", which is not one of the findings this verdict carries. The block travels inside a finding, so it names one", Code: contract.ExitUsage}
		}
		for _, list := range [][]any{criteria, findings} {
			for i, item := range list {
				o, ok := item.(Obj)
				if !ok {
					continue
				}
				if id, _ := get(o, "id"); strings.TrimSpace(pyStrOrEmpty(id)) != wanted {
					continue
				}
				existing, present := get(o, "restoration")
				if present && existing == false {
					return nil, &dispatch.UsageError{Detail: "--restoration names " + pyvalue.StrRepr(wanted) + ", whose finding declares the restoration block false. One correction carries one block and says so once", Code: contract.ExitUsage}
				}
				if present && existing != nil {
					if _, isBool := existing.(bool); !isBool {
						continue
					}
				}
				list[i] = set(o, "restoration", true)
			}
		}
	}
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	ack.Sync = VerdictSync(ack.Store, ack.Clock)
	event := c.s("--event")
	record, err := ack.RecordVerdict(c.ctx, event, c.s("--verdict"), c.s("--verdict-turn"), criteria, findings, c.opt("--reason"), c.opt("--expect-criteria-digest"))
	if err != nil {
		return nil, err
	}
	restoration, err := ack.RestorationOf(c.ctx, event)
	if err != nil {
		return nil, err
	}
	return append(record, F{Key: "_restoration", Value: restoration}), nil
}
