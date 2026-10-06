package harness

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Verb is one crw pabcd command: the issue that ports it adds its row to Verbs. A row that needs
// the invocation's context sets RunContext and leaves Run nil; the dispatch passes ctx to
// RunContext when it is set, and a row with neither function is not dispatched.
type Verb struct {
	Name       string
	Run        func(args []string, in io.Reader, stdout, stderr io.Writer) int
	RunContext func(ctx context.Context, args []string, in io.Reader, stdout, stderr io.Writer) int
}

// Verbs is the table of crw pabcd's commands, a row for each verb that is ported.
func Verbs() []Verb {
	return []Verb{
		{Name: "freeze", Run: freezeVerb},
		{Name: "plan", Run: planVerb},
		{Name: "receipt", RunContext: receiptVerb},
		{Name: "evidence", Run: evidenceVerb},
		{Name: "memory", Run: memoryVerb},
		{Name: "reset", Run: resetVerb},
		{Name: "config", Run: configVerb},
		{Name: "scan", Run: scanVerb},
		{Name: "review-round", Run: reviewRoundVerb},
		{Name: "metric", RunContext: metricVerb},
		{Name: "divergence", Run: divergenceVerb},
	}
}

// Pabcd is crw pabcd <verb> [args] without an invocation context: a caller that holds one calls
// PabcdContext.
func Pabcd(args []string, in io.Reader, stdout, stderr io.Writer, verbs []Verb) int {
	return PabcdContext(context.Background(), args, in, stdout, stderr, verbs)
}

// PabcdContext is crw pabcd <verb> [args] under the invocation's context: it hands the arguments
// after the verb to its row, passing ctx when the row sets RunContext, and answers an absent or
// unknown verb as the other crw modes do, with the usage and exit status 2.
func PabcdContext(ctx context.Context, args []string, in io.Reader, stdout, stderr io.Writer, verbs []Verb) int {
	var names []string
	for _, v := range verbs {
		names = append(names, v.Name)
	}
	usage := "usage: crw pabcd [-h] {" + strings.Join(names, ",") + "} ...\n"
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		io.WriteString(stdout, usage)
		return 0
	}
	for _, v := range verbs {
		if len(args) > 0 && v.Name == args[0] && (v.RunContext != nil || v.Run != nil) {
			if v.RunContext != nil {
				return v.RunContext(ctx, args[1:], in, stdout, stderr)
			}
			return v.Run(args[1:], in, stdout, stderr)
		}
	}
	io.WriteString(stderr, usage)
	if len(args) == 0 {
		io.WriteString(stderr, "crw pabcd: error: the following arguments are required: verb\n")
	} else {
		fmt.Fprintf(stderr, "crw pabcd: error: argument verb: invalid choice: %q (choose from '%s')\n", args[0], strings.Join(names, "', '"))
	}
	return 2
}
