package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// verify is `crw flow verify [--checkout DIR] [--head SHA] [--base SHA] [--record FILE] [--label LABEL]`
// (CRW-1594): the configured verifier verifies the head of the checkout — no relay store, no session — and
// its sealed record lands where --record names, whatever the result. The defaults come from the integration
// batch's own variables (CRW_VERIFY_HEAD, CRW_VERIFY_BASE, CRW_VERIFY_RECORD), so the batch's verify_command
// can be this command. Exit 0 a pass, 1 a failed verification, 2 a configuration or infrastructure failure.
func (c *command) verify(args []string) int {
	fs := flag.NewFlagSet("crw flow verify", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprintln(c.stderr, Usage) }
	checkout := fs.String("checkout", "", "the checkout the head is verified in (default: the working directory)")
	head := fs.String("head", "", "the head to verify (default: CRW_VERIFY_HEAD, else HEAD)")
	base := fs.String("base", "", "the base the verification's range covers (default: CRW_VERIFY_BASE, else origin/dev)")
	record := fs.String("record", "", "where the sealed verification-record/1 is written (default: CRW_VERIFY_RECORD)")
	label := fs.String("label", "verify", "the label the record's request carries")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		if err == nil {
			err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		return c.fail("%v", err)
	}
	if *checkout == "" {
		*checkout = "."
	}
	checkoutPath, err := filepath.Abs(*checkout)
	if err != nil {
		return c.fail("the checkout path: %v", err)
	}
	*checkout = checkoutPath
	if *head == "" {
		*head = c.getenv("CRW_VERIFY_HEAD")
	}
	if *head == "" {
		*head = "HEAD"
	}
	if *base == "" {
		*base = c.getenv("CRW_VERIFY_BASE")
	}
	if *record == "" {
		*record = c.getenv("CRW_VERIFY_RECORD")
	}
	if *record == "" {
		return c.fail("a record path is required (--record or CRW_VERIFY_RECORD)")
	}
	recordPath, err := filepath.Abs(*record)
	if err != nil {
		return c.fail("the record path: %v", err)
	}
	*record = recordPath
	tools, err := crwconfig.LoadVerifierTools(func(k string) string { return c.getenv(k) }, "")
	if err != nil {
		return c.fail("the verifier's configuration: %v", err)
	}
	out, verr := ops.NewVerifier(tools).Verify(c.ctx, ops.VerifyRequest{Node: *label, Head: *head, Worktree: *checkout, Base: *base})
	if len(out.Record) > 0 {
		if werr := verifyRecordWrite(*record, out.Record); werr != nil {
			return c.fail("the record could not be written: %v", werr)
		}
	}
	if verr != nil {
		return c.fail("%v", verr)
	}
	if out.Note != "" {
		fmt.Fprintf(c.stdout, "%s\n", out.Note)
	}
	fmt.Fprintf(c.stdout, "%s\n", out.Summary)
	if !out.Pass {
		return 1
	}
	return 0
}

// verifyRecordWrite writes raw to path through a temporary file of its directory, renamed over it.
func verifyRecordWrite(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".verify-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
