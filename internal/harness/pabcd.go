package harness

import (
	"fmt"
	"io"
	"strings"
)

// Verb is one crw pabcd command: the issue that ports it adds its row to Verbs.
type Verb struct {
	Name string
	Run  func(args []string, in io.Reader, stdout, stderr io.Writer) int
}

// Verbs is the table of crw pabcd's commands; none is ported yet.
func Verbs() []Verb { return nil }

// Pabcd is crw pabcd <verb> [args]: it hands the arguments after the verb to its row, and answers an
// absent or unknown verb as the other crw modes do, with the usage and exit status 2.
func Pabcd(args []string, in io.Reader, stdout, stderr io.Writer, verbs []Verb) int {
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
		if len(args) > 0 && v.Name == args[0] && v.Run != nil {
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
