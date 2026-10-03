package dispatch

import (
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Services is cli.Services: the selection every command resolves before its handler runs, and
// the global options. Nothing here opens a Store; a handler that needs one opens it itself (the
// admitted store, when dispatch admitted one).
type Services struct {
	Selection        store.StateSelection
	SocketPath       string
	AdapterRequested bool
	// Program is how the operator invoked this CLI, for printed recovery commands.
	Program string
}

// Args are a command line's options as the parser read them, with the command's defaults for
// the options the line left out. Options are named by their long flag without the dashes.
type Args struct {
	Parsed argparse.Result
	// Positionals are the subcommand words before the options (service's).
	Positionals []string
	Defaults    map[string]any
}

// Given reports whether the option appeared on the command line, so an empty value still counts.
func (a Args) Given(name string) bool { return a.Parsed.Given[name] }

// String is the option's text (its last value, else its default, else ""), and whether the line
// gave it.
func (a Args) String(name string) (string, bool) {
	if values := a.Parsed.Values[name]; len(values) > 0 {
		return values[len(values)-1], true
	}
	switch value := a.Defaults[name].(type) {
	case string:
		return value, false
	case int64:
		return strconv.FormatInt(value, 10), false
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), false
	case bool:
		return strconv.FormatBool(value), false
	}
	return "", false
}

// Text is String without whether it was given.
func (a Args) Text(name string) string {
	value, _ := a.String(name)
	return value
}

// TruthyString is a string option's value and whether it is non-empty: an omitted option and an
// explicitly empty value are both false.
func (a Args) TruthyString(name string) (string, bool) {
	value := a.Text(name)
	return value, value != ""
}

// Bool is a flag option's value.
func (a Args) Bool(name string) bool {
	if values := a.Parsed.Values[name]; len(values) > 0 {
		return values[len(values)-1] == "true"
	}
	value, _ := a.Defaults[name].(bool)
	return value
}

// Strings is every value an append option was given, in order (nil when none was).
func (a Args) Strings(name string) []string { return a.Parsed.Values[name] }

// Integer is an int option's value: the integer the line gave, else its default.
func (a Args) Integer(name string) int64 {
	if a.Given(name) {
		return a.Parsed.Numbers[name].(int64)
	}
	value, _ := a.Defaults[name].(int64)
	return value
}

// Float is a float option's value: the float the line gave, else its default.
func (a Args) Float(name string) float64 {
	if a.Given(name) {
		return a.Parsed.Numbers[name].(float64)
	}
	value, _ := a.Defaults[name].(float64)
	return value
}

// Number is an int (int64) or float (float64) option's value, else its default, else nil.
func (a Args) Number(name string) any {
	if a.Given(name) {
		return a.Parsed.Numbers[name]
	}
	return a.Defaults[name]
}
