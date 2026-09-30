package registry

import (
	"context"
	"database/sql"
	"math/big"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// External relay commands: a package this one cannot import (because it imports this one)
// implements them over this package's argparse parsing, store opening and JSON emitting. The
// merge turn registers its merge-turn-* commands here from its init.

// Parsed is the parsed arguments of an external command.
type Parsed struct{ p parsed }

// Text is the argument's value, "" when it was not given.
func (p Parsed) Text(name string) string { return p.p.text(name) }

// Optional is the argument's value, or None when it was not given.
func (p Parsed) Optional(name string) sql.NullString { return p.p.optional(name) }

// Given reports whether the argument appeared on the command line.
func (p Parsed) Given(name string) bool { return p.p.set[name] }

// Values is every value an action="append" argument was given, in order.
func (p Parsed) Values(name string) []string { return append([]string{}, p.p.values[name]...) }

// Integer is a type=int argument's value.
func (p Parsed) Integer(name string) *big.Int { return p.p.integer(name) }

// AddCommand registers an external relay command; its argparse spec (argparse.Specs[name])
// parses its arguments.
func AddCommand(name string, run func(context.Context, *Registry, Parsed) (any, error)) {
	AddCheckedCommand(name, nil, run)
}

// AddCheckedCommand is AddCommand for a handler that refuses its own arguments before it touches
// the store: precheck is that refusal, run where cli.main would reach it (see run in cli.go).
func AddCheckedCommand(name string, precheck func(Parsed) error, run func(context.Context, *Registry, Parsed) (any, error)) {
	c := command{name: name, run: func(ctx context.Context, r *Registry, p parsed) (any, error) { return run(ctx, r, Parsed{p}) }}
	if precheck != nil {
		c.precheck = func(p *parsed) error { return precheck(Parsed{*p}) }
	}
	commands = append(commands, c)
}

// WithEnforcement is cli._with_enforcement.
func WithEnforcement(r *Registry, answer contract.OrderedObject) contract.OrderedObject {
	return withEnforcement(r, answer)
}

// PayloadExit is cli.PayloadExit for an external command: the whole answer, its own code.
func PayloadExit(payload contract.OrderedObject, code int) error {
	return &linkageExit{payload: payload, code: code}
}

// DecodeJSON is json.loads into Python-shaped values (objects ordered, integers exact); the
// error text is JSONDecodeError's.
func DecodeJSON(raw string) (any, error) {
	return pyjson.Loads(raw, pyjson.LoadOptions{Python: true})
}
