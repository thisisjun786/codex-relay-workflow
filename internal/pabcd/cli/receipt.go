package cli

import (
	"context"
	"io"
)

type ReceiptCLIArgs struct {
	Verb      string   `json:"verb"`
	Cwd       string   `json:"cwd"`
	Session   string   `json:"session,omitempty"`
	Command   []string `json:"command"`
	Generated []string `json:"generated,omitempty"`
}

type ReceiptCLIParseError struct{ Message string }

func (e ReceiptCLIParseError) Error() string { return e.Message }

type ReceiptCLIResult struct {
	Output string
	Code   int
}
type ReceiptRunOptions struct {
	Context        context.Context
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

func ParseReceiptCLIArgs(argv []string, cwd string) (ReceiptCLIArgs, error) {
	return ReceiptCLIArgs{}, ReceiptCLIParseError{"not implemented"}
}
func ReceiptPathFor(cwd, sessionID string) string { return "" }
func RunReceiptCLI(args ReceiptCLIArgs, options ReceiptRunOptions) (ReceiptCLIResult, error) {
	return ReceiptCLIResult{Output: "not implemented", Code: 1}, nil
}
