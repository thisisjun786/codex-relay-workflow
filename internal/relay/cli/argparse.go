package cli

import (
	"flag"
	"fmt"
	"io"
	"math/big"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

// FlagSet is the handler-facing value store, not the argument parser.
func parseRelayArgs(prog string, flags *flag.FlagSet, argv []string, stdout, stderr io.Writer) (map[string]bool, int, bool) {
	result := argparse.Parse(flags.Name(), argv)
	if result.Help {
		fmt.Fprint(stdout, argparse.Help(prog, flags.Name()))
		return nil, 0, true
	}
	if result.Message != "" {
		fmt.Fprint(stderr, result.Error(prog, flags.Name()))
		return nil, 2, true
	}
	for name, values := range result.Values {
		if number, ok := result.Numbers[name]; ok {
			flags.Lookup(name).Value = &numberValue{number}
			continue
		}
		for _, value := range values {
			if err := flags.Set(name, value); err != nil {
				result.Message = "argument --" + name + ": " + err.Error()
				fmt.Fprint(stderr, result.Error(prog, flags.Name()))
				return nil, 2, true
			}
		}
	}
	return result.Given, 0, false
}

type numberValue struct{ value any }

func (v *numberValue) String() string   { return argparse.NumberText(v.value) }
func (v *numberValue) Get() any         { return v.value }
func (v *numberValue) Set(string) error { return fmt.Errorf("numeric flags are bound by argparse") }

// Integer returns a converted Python integer, including FlagSet defaults.
func (a Args) Integer(name string) *big.Int {
	switch n := a.Flags.Lookup(name).Value.(flag.Getter).Get().(type) {
	case *big.Int:
		return n
	case int:
		return big.NewInt(int64(n))
	case int64:
		return big.NewInt(n)
	default:
		panic("integer action bound to a non-integer flag")
	}
}
