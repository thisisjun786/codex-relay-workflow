package registry

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// External relay commands: a package this one cannot import (because it imports this one)
// implements them over this package's argparse parsing, store opening and JSON emitting. The
// merge turn registers its merge-turn-* commands here from its init.

// OptionSpec is one argparse option: add_argument("--name", required=..., action=...,
// type=int, choices=...).
type OptionSpec struct {
	Name                           string
	Required, Multi, Flag, Integer bool
	Choices                        []string
}

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
func (p Parsed) Integer(name string) int64 { return p.p.integer(name) }

// AddCommand registers an external relay command. exclusive names a required mutually
// exclusive group of flag options.
func AddCommand(name string, options []OptionSpec, exclusive []string, run func(context.Context, *Registry, Parsed) (any, error)) {
	converted := make([]option, len(options))
	for i, o := range options {
		converted[i] = option{name: o.Name, required: o.Required, multi: o.Multi, flag: o.Flag, integer: o.Integer, choices: o.Choices}
	}
	commands = append(commands, command{name: name, options: converted, exclusive: exclusive,
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) { return run(ctx, r, Parsed{p}) }})
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
	if message := store.PythonJSONError(raw); message != "" {
		return nil, errors.New(message)
	}
	return decodeJSON([]byte(raw))
}
