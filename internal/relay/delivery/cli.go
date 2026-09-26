package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The twelve delivery commands of `codex-session-relay` (cli.py: emit, deliver, reconcile,
// recover, claim, ack-proof, ack, verdict, criteria-register, criteria-show, revision-head,
// verify-acks), with argparse's parsing rules and main()'s JSON replies and exit codes.

type flagSpec struct {
	name     string
	kind     string // store, int, append, true
	required bool
	choices  []string
	def      any
}

type commandSpec struct {
	flags []flagSpec
	run   func(*cliRun) (any, error)
}

var deliveryCommands = map[string]commandSpec{
	"emit": {flags: []flagSpec{{"--relationship", "store", true, nil, nil}, {"--generation", "int", true, nil, nil}, {"--attempt", "int", false, nil, int64(1)},
		{"--outcome", "store", true, []string{"ready_for_review", "failed", "interrupted", "blocked_needs_input"}, nil}, {"--turn-thread", "store", true, nil, nil}, {"--turn-id", "store", true, nil, nil},
		{"--turn-status", "store", false, []string{"completed", "failed", "interrupted", "inProgress"}, "inProgress"}, {"--artifact", "append", false, nil, nil}, {"--manifest-ref", "store", false, nil, nil},
		{"--continues-anchor", "store", false, nil, nil}, {"--continuation-actor", "store", false, nil, nil}, {"--continuation-reason", "store", false, nil, nil}, {"--supersedes-revision", "store", false, nil, nil}, {"--no-enqueue", "true", false, nil, false}},
		run: cmdEmit},
	"deliver":     {flags: []flagSpec{{"--event", "store", false, nil, nil}, {"--limit", "int", false, nil, int64(4)}}, run: needsHost},
	"reconcile":   {flags: []flagSpec{{"--request-id", "store", true, nil, nil}}, run: needsHost},
	"recover":     {run: needsHost},
	"claim":       {flags: []flagSpec{{"--event", "store", true, nil, nil}, {"--turn", "store", false, nil, nil}}, run: cmdClaim},
	"ack-proof":   {flags: []flagSpec{{"--event", "store", true, nil, nil}, {"--turn", "store", true, nil, nil}}, run: cmdAckProof},
	"ack":         {flags: []flagSpec{{"--event", "store", true, nil, nil}, {"--ack-turn", "store", true, nil, nil}, {"--ack-proof", "store", true, nil, nil}, {"--reject", "store", false, nil, nil}}, run: cmdAck},
	"verify-acks": {flags: []flagSpec{{"--limit", "int", false, nil, int64(8)}}, run: needsHost},
	"verdict": {flags: []flagSpec{{"--event", "store", true, nil, nil}, {"--verdict", "store", true, []string{"verified", "needs_changes", "unverified", "aborted"}, nil}, {"--verdict-turn", "store", true, nil, nil},
		{"--criterion", "append", false, nil, nil}, {"--finding", "append", false, nil, nil}, {"--criteria", "store", false, nil, nil}, {"--restoration", "store", false, nil, nil}, {"--reason", "store", false, nil, nil}, {"--expect-criteria-digest", "store", false, nil, nil}},
		run: cmdVerdict},
	"criteria-register": {flags: []flagSpec{{"--relationship", "store", true, nil, nil}, {"--criterion", "append", true, nil, nil}, {"--optional", "append", false, nil, nil}, {"--source-ref", "store", false, nil, nil}}, run: cmdCriteriaRegister},
	"criteria-show":     {flags: []flagSpec{{"--relationship", "store", true, nil, nil}}, run: cmdCriteriaShow},
	"revision-head":     {flags: []flagSpec{{"--relationship", "store", true, nil, nil}, {"--generation", "int", false, nil, nil}}, run: cmdRevisionHead},
}

// usageError is SystemExit2: a JSON usage reply with its exit code.
type usageError struct {
	detail string
	code   int
}

func (u *usageError) Error() string { return u.detail }

type cliRun struct {
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

// ParseDeliveryArgs is argparse for one command: the parsed values, or its error message.
func parseArgs(spec commandSpec, argv []string) (map[string]any, string) {
	out := map[string]any{}
	for _, f := range spec.flags {
		out[f.name] = f.def
	}
	seen := map[string]bool{}
	var unknown []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, value, hasValue := strings.Cut(arg, "=")
		var f *flagSpec
		for j := range spec.flags {
			if spec.flags[j].name == name {
				f = &spec.flags[j]
			}
		}
		if f == nil {
			unknown = append(unknown, arg)
			continue
		}
		seen[f.name] = true
		if f.kind == "true" {
			out[f.name] = true
			continue
		}
		if !hasValue {
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
				return nil, fmt.Sprintf("argument %s: expected one argument", f.name)
			}
			i++
			value = argv[i]
		}
		if f.choices != nil && !slices.Contains(f.choices, value) {
			quoted := make([]string, len(f.choices))
			for k, c := range f.choices {
				quoted[k] = "'" + c + "'"
			}
			return nil, fmt.Sprintf("argument %s: invalid choice: '%s' (choose from %s)", f.name, value, strings.Join(quoted, ", "))
		}
		switch f.kind {
		case "int":
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return nil, fmt.Sprintf("argument %s: invalid int value: '%s'", f.name, value)
			}
			out[f.name] = n
		case "append":
			list, _ := out[f.name].([]string)
			out[f.name] = append(list, value)
		default:
			out[f.name] = value
		}
	}
	var missing []string
	for _, f := range spec.flags {
		if f.required && !seen[f.name] {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, "the following arguments are required: " + strings.Join(missing, ", ")
	}
	if len(unknown) > 0 {
		return nil, "unrecognized arguments: " + strings.Join(unknown, " ")
	}
	return out, ""
}

// ExecuteCLI runs one delivery command of `crw relay`. handled is false for any other command.
func ExecuteCLI(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, bool) {
	var state, socket string
	i := 0
	for ; i < len(argv); i++ {
		name, value, has := strings.Cut(argv[i], "=")
		switch name {
		case "--json":
			continue
		case "--state", "--socket", "--kind-module":
			if !has {
				if i+1 >= len(argv) {
					return 0, false
				}
				i++
				value = argv[i]
			}
			if name == "--state" {
				state = value
			} else if name == "--socket" {
				socket = value
			}
			continue
		}
		break
	}
	if i >= len(argv) {
		return 0, false
	}
	command := argv[i]
	spec, ok := deliveryCommands[command]
	if !ok {
		return 0, false
	}
	parsed, problem := parseArgs(spec, argv[i+1:])
	if problem != "" {
		fmt.Fprintf(stderr, "usage: codex-session-relay %s [-h] ...\ncodex-session-relay %s: error: %s\n", command, command, problem)
		return 2, true
	}
	run := &cliRun{ctx: ctx, args: parsed, socket: socket, clock: SystemClock{}}
	if command != "ack-proof" {
		selection, err := store.ResolveStateDir(state, socket)
		if err != nil {
			return reply(stdout, Obj{{Key: "error", Value: "host"}, {Key: "detail", Value: "OSError: " + err.Error()}}, contract.ExitHost), true
		}
		if refusal := selectionRefusal(selection, socket); refusal != nil {
			return reply(stdout, refusal, contract.ExitRefused), true
		}
		run.state = selection.Path
	}
	defer func() {
		if run.store != nil {
			_ = run.store.Close()
		}
	}()
	result, err := spec.run(run)
	var usage *usageError
	var refused *store.RefusedError
	switch {
	case errors.As(err, &usage):
		return reply(stdout, Obj{{Key: "error", Value: "usage"}, {Key: "detail", Value: usage.detail}}, usage.code), true
	case errors.As(err, &refused):
		return reply(stdout, Obj{{Key: "error", Value: "refused"}, {Key: "reason", Value: refused.Reason}, {Key: "detail", Value: refused.Detail}}, contract.ExitRefused), true
	case err != nil:
		return reply(stdout, Obj{{Key: "error", Value: "host"}, {Key: "detail", Value: hostDetail(err)}}, contract.ExitHost), true
	}
	return reply(stdout, result, contract.ExitOk), true
}

// hostError carries the Python exception class name of a host failure.
type hostError struct{ kind, message string }

func (h *hostError) Error() string { return h.kind + ": " + h.message }

func hostDetail(err error) string {
	var h *hostError
	if errors.As(err, &h) {
		return h.Error()
	}
	return "RuntimeError: " + err.Error()
}

func reply(w io.Writer, value any, code int) int {
	if err := contract.Emit(w, value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return contract.ExitHost
	}
	return code
}

// needsHost is the four commands that reach the App Server. Without --socket they are a usage
// error, as in Python; with it, the host adapter they drive is the bridge adapter port (todo 28).
func needsHost(c *cliRun) (any, error) {
	if c.socket == "" {
		return nil, &usageError{"this command needs --socket to reach the host", contract.ExitUsage}
	}
	return nil, &hostError{"HostUnavailable", "the relay host adapter (bridge_adapter.py) is not ported to Go yet (todo 28)"}
}

var eventIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func cmdAckProof(c *cliRun) (any, error) {
	event, turn := c.s("--event"), c.s("--turn")
	if !eventIDPattern.MatchString(event) {
		return nil, &hostError{"ValueError", "event id must be 32 lowercase hex characters"}
	}
	if strings.TrimSpace(turn) == "" {
		return nil, &hostError{"ValueError", "ack_turn_id must be a non-empty string"}
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
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	reject := c.opt("--reject")
	record, err := ack.Acknowledge(c.ctx, c.s("--event"), c.s("--ack-turn"), c.s("--ack-proof"), !truthy(reject), reject, nil)
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
	if g, ok := c.opt("--generation").(int64); ok && g != 0 {
		generation = g
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
				return nil, &hostError{"FileNotFoundError", err.Error()}
			}
			text = string(content)
		}
		parsed, err := loads(text)
		if err != nil {
			return nil, &hostError{"JSONDecodeError", err.Error()}
		}
		switch v := parsed.(type) {
		case []any:
			findings = append(findings, v...)
		case Obj:
			for _, f := range v {
				findings = append(findings, f.Key)
			}
		default:
			return nil, &hostError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
		}
	}
	if c.opt("--restoration") != nil {
		wanted := strings.TrimSpace(c.s("--restoration"))
		if wanted == "" {
			return nil, &usageError{"--restoration names the criterion id whose finding carries the block, so it cannot be empty. Leave the option out to carry no block", contract.ExitUsage}
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
			return nil, &usageError{"--restoration names " + store.PyRepr(c.s("--restoration")) + ", which is not one of the findings this verdict carries. The block travels inside a finding, so it names one", contract.ExitUsage}
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
					return nil, &usageError{"--restoration names " + store.PyRepr(wanted) + ", whose finding declares the restoration block false. One correction carries one block and says so once", contract.ExitUsage}
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

// CommandNames are the relay subcommands this package serves.
func CommandNames() []string {
	names := make([]string, 0, len(deliveryCommands))
	for name := range deliveryCommands {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
