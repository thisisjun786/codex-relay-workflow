package capacity

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay CLI commands todo 27 part A owns (cli.py:5215-5262 capacity, :5271-5387 edit
// regions). Parsing follows argparse as registry's parser does: a command line it cannot parse
// exits 2 with usage on stderr; every other ending prints one JSON document.

type option struct {
	name     string
	required bool
	flag     bool // action="store_true"
	integer  bool // type=int
	number   bool // type=float
	choices  []string
	def      string
}

type parsed struct {
	values map[string]string
	set    map[string]bool
}

func (p parsed) text(name string) string { return p.values[name] }

func (p parsed) optional(name string) sql.NullString {
	if !p.set[name] {
		return sql.NullString{}
	}
	return sql.NullString{String: p.values[name], Valid: true}
}

func (p parsed) float(name string) float64 {
	n, _ := parseFloat(p.values[name])
	return n
}

// parseFloat is Python's float(text): surrounding whitespace, underscores between digits,
// and the nan/inf/infinity spellings in any case.
func parseFloat(text string) (float64, error) {
	trimmed := strings.TrimSpace(text)
	if strings.Contains(trimmed, "_") {
		if strings.HasPrefix(trimmed, "_") || strings.HasSuffix(trimmed, "_") || strings.Contains(trimmed, "__") {
			return 0, strconv.ErrSyntax
		}
		trimmed = strings.ReplaceAll(trimmed, "_", "")
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimLeft(trimmed, "+-")), "0x") {
		return 0, strconv.ErrSyntax
	}
	n, err := strconv.ParseFloat(trimmed, 64)
	if errors.Is(err, strconv.ErrRange) {
		return n, nil
	}
	return n, err
}

type command struct {
	name    string
	options []option
	run     func(context.Context, *store.Store, parsed) (any, error)
}

var scopeChoices = []string{"initiative", "project", "store"}

var commands = []command{
	{"slot-reserve", []option{{name: "kind", required: true}, {name: "subject", required: true}, {name: "parent-task", required: true},
		{name: "project", required: true}, {name: "actor", required: true}, {name: "detail"}}, cmdSlotReserve},
	{"slot-release", []option{{name: "kind", required: true}, {name: "subject", required: true}, {name: "actor", required: true},
		{name: "reason", required: true}, {name: "tenure", integer: true}}, cmdSlotRelease},
	{"limit-declare", []option{{name: "scope-kind", required: true, choices: scopeChoices}, {name: "scope", required: true},
		{name: "dimension", required: true}, {name: "unit", required: true}, {name: "ceiling", required: true, number: true},
		{name: "declared-by", required: true}, {name: "source", required: true}, {name: "no-enforce", flag: true}}, cmdLimitDeclare},
	{"usage-observe", []option{{name: "scope-kind", required: true, choices: scopeChoices}, {name: "scope", required: true},
		{name: "dimension", required: true}, {name: "observed", required: true, number: true}, {name: "observed-by", required: true},
		{name: "method", required: true}}, cmdUsageObserve},
	{"capacity-show", []option{{name: "project"}, {name: "parent-task"}, {name: "initiative"}, {name: "scope"},
		{name: "scope-kind", choices: scopeChoices, def: "project"}}, cmdCapacityShow},
	{"region-propose", []option{{name: "repository", required: true}, {name: "revision", required: true}, {name: "path", required: true},
		{name: "kind", required: true, choices: regionKinds}, {name: "key"}, {name: "class", choices: regionClasses, def: classSource},
		{name: "regenerate-from"}, {name: "left-project", required: true}, {name: "right-project", required: true},
		{name: "peer-link", required: true}, {name: "task", required: true}, {name: "constraint", required: true},
		{name: "condition"}, {name: "issue"}, {name: "next-owner"}}, cmdRegionPropose},
	{"region-settle", []option{{name: "agreement", required: true}, {name: "actor", required: true},
		{name: "disposition", required: true, choices: dispositions}, {name: "condition"}, {name: "reason"}}, cmdRegionSettle},
	{"region-restate-revision", []option{{name: "repository", required: true}, {name: "from-revision", required: true},
		{name: "to-revision", required: true}, {name: "actor", required: true}}, cmdRegionRestate},
	{"region-reaffirm", []option{{name: "agreement", required: true}, {name: "actor", required: true}, {name: "revision", required: true},
		{name: "condition"}}, cmdRegionReaffirm},
	{"region-followup", []option{{name: "agreement", required: true}, {name: "trigger", required: true}, {name: "acceptance", required: true},
		{name: "recorded-by", required: true}, {name: "issue-ref"}, {name: "assignee"}, {name: "assignee-project"}}, cmdRegionFollowup},
	{"region-followup-accept", []option{{name: "followup", required: true}, {name: "actor", required: true},
		{name: "assignee-project", required: true}}, cmdRegionFollowupAccept},
	{"region-followup-settle", []option{{name: "followup", required: true}, {name: "actor", required: true},
		{name: "disposition", required: true, choices: followupDispositions}, {name: "reason"}}, cmdRegionFollowupSettle},
	{"region-show", []option{{name: "repository", required: true}, {name: "revision"}, {name: "project"}, {name: "path"}}, cmdRegionShow},
}

func newRegions(s *store.Store) *EditRegions { return &EditRegions{Store: s, Now: registry.SystemISO} }

func cmdRegionPropose(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newRegions(s).Propose(ctx, Proposal{Repository: p.text("repository"), BaseRevision: p.text("revision"), Path: p.text("path"),
		RegionKind: p.text("kind"), RegionKey: p.text("key"), RegionClass: p.text("class"), RegenerateFrom: p.optional("regenerate-from"),
		LeftProject: p.text("left-project"), RightProject: p.text("right-project"), PeerLinkID: p.text("peer-link"),
		ProposerTaskID: p.text("task"), ConstraintText: p.text("constraint"), Condition: p.optional("condition"),
		IssueKey: p.optional("issue"), NextOwner: p.optional("next-owner")})
}

// settleConditionRefusal is cli.cmd_region_settle's own refusal (cli.py:1433), printed whole
// before the service is reached.
func settleConditionRefusal() error {
	return &cli.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bad_invocation"},
		{Key: "detail", Value: "an acceptance takes no condition and --condition would be dropped. State" +
			" your condition when you propose, restate it with region-reaffirm" +
			" --condition after a base move, or decline with the condition you would accept."}}, Code: contract.ExitRefused}
}

func cmdRegionSettle(ctx context.Context, s *store.Store, p parsed) (any, error) {
	if p.text("disposition") == "accepted" && p.set["condition"] {
		return nil, settleConditionRefusal()
	}
	return newRegions(s).Settle(ctx, p.text("agreement"), p.text("actor"), p.text("disposition"), p.optional("condition"), p.optional("reason"))
}

func cmdRegionRestate(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newRegions(s).RestateRevision(ctx, p.text("repository"), p.text("from-revision"), p.text("to-revision"), p.text("actor"))
}

func cmdRegionReaffirm(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newRegions(s).Reaffirm(ctx, p.text("agreement"), p.text("actor"), p.text("revision"), p.optional("condition"))
}

func cmdRegionFollowup(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newRegions(s).Followup(ctx, Followup{Agreement: p.text("agreement"), Trigger: p.text("trigger"), Acceptance: p.text("acceptance"),
		RecordedBy: p.text("recorded-by"), IssueRef: p.optional("issue-ref"), AssigneeTask: p.optional("assignee"),
		AssigneeProject: p.optional("assignee-project")})
}

func cmdRegionFollowupAccept(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newRegions(s).AcceptFollowup(ctx, p.text("followup"), p.text("actor"), p.text("assignee-project"))
}

func cmdRegionFollowupSettle(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newRegions(s).SettleFollowup(ctx, p.text("followup"), p.text("actor"), p.text("disposition"), p.optional("reason"))
}

func cmdRegionShow(ctx context.Context, s *store.Store, p parsed) (any, error) {
	answer, err := newRegions(s).Show(ctx, p.text("repository"), ShowFilter{BaseRevision: p.optional("revision"),
		Project: p.optional("project"), Path: p.optional("path")})
	if err != nil {
		return nil, err
	}
	return withEnforcement(s, answer), nil
}

func newCapacity(s *store.Store) *Capacity { return &Capacity{Store: s, Now: registry.SystemISO} }

func cmdSlotReserve(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newCapacity(s).Reserve(ctx, Reservation{SubjectKind: p.text("kind"), SubjectKey: p.text("subject"),
		ParentTask: p.text("parent-task"), Project: p.text("project"), ReservedBy: p.text("actor"), Detail: p.optional("detail")})
}

func cmdSlotRelease(ctx context.Context, s *store.Store, p parsed) (any, error) {
	var tenure sql.NullInt64
	if p.set["tenure"] {
		n, _ := strconv.ParseInt(strings.TrimSpace(p.text("tenure")), 10, 64)
		tenure = sql.NullInt64{Int64: n, Valid: true}
	}
	return newCapacity(s).Release(ctx, Release{SubjectKind: p.text("kind"), SubjectKey: p.text("subject"),
		ReleasedBy: p.text("actor"), Reason: p.text("reason"), Tenure: tenure})
}

func cmdLimitDeclare(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newCapacity(s).DeclareLimit(ctx, Limit{ScopeKind: p.text("scope-kind"), ScopeKey: p.text("scope"),
		Dimension: p.text("dimension"), Unit: p.text("unit"), Ceiling: p.float("ceiling"), DeclaredBy: p.text("declared-by"),
		Source: p.text("source"), Enforce: !p.set["no-enforce"]})
}

func cmdUsageObserve(ctx context.Context, s *store.Store, p parsed) (any, error) {
	return newCapacity(s).Observe(ctx, Observation{ScopeKind: p.text("scope-kind"), ScopeKey: p.text("scope"),
		Dimension: p.text("dimension"), Observed: p.float("observed"), ObservedBy: p.text("observed-by"), Method: p.text("method")})
}

func cmdCapacityShow(ctx context.Context, s *store.Store, p parsed) (any, error) {
	c := newCapacity(s)
	answer, err := c.Report(ctx, ReportFilter{Project: p.optional("project"), ParentTask: p.optional("parent-task"),
		Initiative: p.optional("initiative")})
	if err != nil {
		return nil, err
	}
	if p.text("scope") != "" {
		room, err := c.Headroom(ctx, p.text("scope-kind"), p.text("scope"))
		if err != nil {
			return nil, err
		}
		answer = append(answer, contract.Field{Key: "headroom", Value: room})
	}
	return withEnforcement(s, answer), nil
}

// withEnforcement is cli._with_enforcement: say when the store could not install a guard index.
func withEnforcement(s *store.Store, answer contract.OrderedObject) contract.OrderedObject {
	if len(s.UnenforcedIndexes) == 0 {
		return answer
	}
	unenforced := []any{}
	for _, index := range s.UnenforcedIndexes {
		unenforced = append(unenforced, contract.OrderedObject{{Key: "index", Value: index.Index}, {Key: "detail", Value: index.Detail}})
	}
	return append(answer, contract.Field{Key: "unenforcedIndexes", Value: unenforced})
}

// Names lists this package's relay commands, in cli.py's add_parser order.
func Names() []string {
	names := make([]string, len(commands))
	for i, c := range commands {
		names[i] = c.name
	}
	return names
}

// CommandOf is the relay command argv names after the global flags, if this package owns it.
func CommandOf(argv []string) (string, bool) {
	_, rest, err := globals(argv)
	if err != nil || len(rest) == 0 || !slices.Contains(Names(), rest[0]) {
		return "", false
	}
	return rest[0], true
}

type globalFlags struct{ state, socket string }

func globals(argv []string) (globalFlags, []string, error) {
	var g globalFlags
	for i := 0; i < len(argv); i++ {
		name, value, inline := strings.Cut(argv[i], "=")
		switch name {
		case "--state", "--socket", "--kind-module":
			if !inline {
				if i+1 >= len(argv) {
					return g, nil, fmt.Errorf("argument %s: expected one argument", name)
				}
				i++
				value = argv[i]
			}
			if name == "--state" {
				g.state = value
			} else if name == "--socket" {
				g.socket = value
			}
		case "--json":
		default:
			return g, argv[i:], nil
		}
	}
	return g, nil, nil
}

type usageError struct{ usage, message string }

func (e *usageError) Error() string { return e.message }

func (c command) usage(prog string) string {
	parts := []string{"usage: " + prog + " " + c.name + " [-h]"}
	for _, o := range c.options {
		metavar := strings.ToUpper(strings.ReplaceAll(o.name, "-", "_"))
		if o.choices != nil {
			metavar = "{" + strings.Join(o.choices, ",") + "}"
		}
		piece := "--" + o.name
		if !o.flag {
			piece += " " + metavar
		}
		if !o.required {
			piece = "[" + piece + "]"
		}
		parts = append(parts, piece)
	}
	return strings.Join(parts, " ")
}

func (c command) parse(prog string, argv []string) (parsed, error) {
	p := parsed{values: map[string]string{}, set: map[string]bool{}}
	var unrecognized []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-h" || arg == "--help" {
			return p, &usageError{usage: c.usage(prog), message: "help"}
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		var o *option
		if strings.HasPrefix(arg, "--") {
			for j := range c.options {
				if c.options[j].name == name {
					o = &c.options[j]
				}
			}
		}
		if o == nil {
			unrecognized = append(unrecognized, arg)
			continue
		}
		if o.flag {
			p.values[o.name], p.set[o.name] = "true", true
			continue
		}
		if !inline {
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
				return p, &usageError{c.usage(prog), "argument --" + o.name + ": expected one argument"}
			}
			i++
			value = argv[i]
		}
		if o.choices != nil && !slices.Contains(o.choices, value) {
			quoted := make([]string, len(o.choices))
			for k, choice := range o.choices {
				quoted[k] = repr(choice)
			}
			return p, &usageError{c.usage(prog), "argument --" + o.name + ": invalid choice: " + repr(value) + " (choose from " + strings.Join(quoted, ", ") + ")"}
		}
		if o.integer {
			if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
				return p, &usageError{c.usage(prog), "argument --" + o.name + ": invalid int value: " + repr(value)}
			}
		}
		if o.number {
			if _, err := parseFloat(value); err != nil {
				return p, &usageError{c.usage(prog), "argument --" + o.name + ": invalid float value: " + repr(value)}
			}
		}
		p.values[o.name], p.set[o.name] = value, true
	}
	var missing []string
	for _, o := range c.options {
		if o.required && !p.set[o.name] {
			missing = append(missing, "--"+o.name)
		}
		if !p.set[o.name] && o.def != "" {
			p.values[o.name] = o.def
		}
	}
	if len(missing) > 0 {
		return p, &usageError{c.usage(prog), "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	if len(unrecognized) > 0 {
		return p, &usageError{"usage: " + prog + " [-h] [--state STATE] [--socket SOCKET] ...", "unrecognized arguments: " + strings.Join(unrecognized, " ")}
	}
	return p, nil
}

// Precheck is argparse for this package's commands: it runs before the relay CLI parses the
// line, so a bad line exits 2 with argparse's usage and message on stderr, exactly as Python
// does. handled is false when argv names no command of this package or the line parses; the
// caller then dispatches through the relay CLI, which applies the selection refusal first.
func Precheck(prog string, argv []string, stdout, stderr io.Writer) (code int, handled bool) {
	_, rest, err := globals(argv)
	if err != nil || len(rest) == 0 {
		return 0, false
	}
	var chosen *command
	for i := range commands {
		if commands[i].name == rest[0] {
			chosen = &commands[i]
		}
	}
	if chosen == nil {
		return 0, false
	}
	_, err = chosen.parse(prog, rest[1:])
	var bad *usageError
	if !errors.As(err, &bad) {
		return 0, false
	}
	if bad.message == "help" {
		fmt.Fprintln(stdout, bad.usage)
		return contract.ExitOk, true
	}
	fmt.Fprintln(stderr, bad.usage)
	fmt.Fprintln(stderr, prog+" "+chosen.name+": error: "+bad.message)
	return 2, true
}

// relayCommands are this package's commands as the relay CLI registers them. Their lines were
// already accepted by Precheck, so the flag set only has to carry the values through.
func relayCommands() []cli.Command {
	out := make([]cli.Command, len(commands))
	for i, c := range commands {
		out[i] = cli.Command{
			Name: c.name,
			Flags: func(f *flag.FlagSet) {
				for _, o := range c.options {
					if o.flag {
						f.Bool(o.name, false, "")
					} else {
						f.String(o.name, o.def, "")
					}
				}
			},
			Run: func(ctx context.Context, services cli.Services, args cli.Args) (any, error) {
				p := parsed{values: map[string]string{}, set: map[string]bool{}}
				for _, o := range c.options {
					if o.flag {
						p.set[o.name] = args.Bool(o.name)
						continue
					}
					p.values[o.name], p.set[o.name] = args.String(o.name)
				}
				// cli.cmd_region_settle refuses before services.edit_regions is reached, so
				// that refusal leaves no store behind.
				if c.name == "region-settle" && p.text("disposition") == "accepted" && p.set["condition"] {
					return nil, settleConditionRefusal()
				}
				s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
				if err != nil {
					return nil, err
				}
				defer s.Close()
				return c.run(ctx, s, p)
			},
		}
	}
	return out
}

// Registered in the relay CLI's command list, as later domain ports append theirs.
func init() { cli.Commands = append(cli.Commands, relayCommands()...) }
