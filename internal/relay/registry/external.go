package registry

import (
	"context"
	"database/sql"
	"math/big"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
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

// AddCommand registers an external relay command (registration names it and its attributes);
// its argparse spec (argparse.Specs[registration.Name]) parses its arguments.
func AddCommand(registration dispatch.Command, run func(context.Context, *Registry, Parsed) (any, error)) {
	AddCheckedCommand(registration, nil, run)
}

// AddCheckedCommand is AddCommand for a handler that refuses its own arguments before it touches
// the store: precheck is that refusal, run where cli.main would reach it (see handle in cli.go).
func AddCheckedCommand(registration dispatch.Command, precheck func(Parsed) error, run func(context.Context, *Registry, Parsed) (any, error)) {
	c := command{Command: registration, run: func(ctx context.Context, r *Registry, p parsed) (any, error) { return run(ctx, r, Parsed{p}) }}
	if precheck != nil {
		c.precheck = func(p *parsed) error { return precheck(Parsed{*p}) }
	}
	register(c)
}

// WithEnforcement is cli._with_enforcement.
func WithEnforcement(r *Registry, answer contract.OrderedObject) contract.OrderedObject {
	return withEnforcement(r, answer)
}

// DecodeJSON is json.loads into Python-shaped values (objects ordered, integers exact); the
// error text is JSONDecodeError's.
func DecodeJSON(raw string) (any, error) {
	return pyjson.Loads(raw, pyjson.LoadOptions{Python: true})
}
