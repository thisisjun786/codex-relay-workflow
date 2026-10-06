package migrate

// report.go renders one run's report: the text output and the versioned crw-state-migration/1 JSON
// document of docs/port-cxc/state-migration.md "Make migration an explicit install subcommand", and
// the no-replace publication of that document to --report.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// SchemaID is the versioned schema of the JSON report.
const SchemaID = "crw-state-migration/1"

// Report is one run: the outcome, the selected roots, every planned item with what happened to it,
// the attention entries, the whole-scope error and the two verification flags.
type Report struct {
	Result          string
	DryRun          bool
	Scope           Scope
	Roots           []ReportRoot
	Items           []ApplyItem
	Attention       []Attention
	Error           *ReportError
	WritesCompleted int
	SourceVerified  bool
}

// ReportRoot is one selected scope's source and destination.
type ReportRoot struct {
	Scope       Scope
	Source      string
	Destination string
}

// ReportError is the whole-scope refusal or failure a run stopped on.
type ReportError struct {
	Kind   string
	Reason string
	Path   string
	Detail string
}

// summarize picks the report's top-level result: refused or failed for a run that stopped, dry-run
// for one that wrote nothing by design, copied when anything was written, already-equal when there
// was nothing left to copy.
func summarize(r *Report) string {
	if r.Error != nil {
		return r.Error.Kind
	}
	if r.DryRun {
		return string(ResultDryRun)
	}
	for _, it := range r.Items {
		if it.Result == ResultFailed {
			return string(ResultFailed)
		}
	}
	for _, it := range r.Items {
		if it.Result == ResultCopied {
			return string(ResultCopied)
		}
	}
	return string(ResultAlreadyEqual)
}

// reportError reads a run's error as the report's error entry.
func reportError(err error) *ReportError {
	var refused *RefusedError
	if errors.As(err, &refused) {
		return &ReportError{Kind: string(ResultRefused), Reason: string(refused.Reason), Path: refused.Path, Detail: refused.Detail}
	}
	return &ReportError{Kind: string(ResultFailed), Detail: err.Error()}
}

// exitCode is the process exit code the report carries: 0 for a verified copy, an already-equal run
// or a dry run, 1 for a refusal or a failure.
func (r *Report) exitCode() int {
	if r.Error != nil || r.Result == string(ResultRefused) || r.Result == string(ResultFailed) {
		return codeRefused
	}
	return codeOK
}

// Object is the JSON report, in the field order the design names.
func (r *Report) Object() contract.OrderedObject {
	roots := make([]any, 0, len(r.Roots))
	for _, root := range r.Roots {
		roots = append(roots, contract.OrderedObject{
			{Key: "scope", Value: string(root.Scope)},
			{Key: "source", Value: root.Source},
			{Key: "destination", Value: root.Destination},
		})
	}
	items := make([]any, 0, len(r.Items))
	for _, it := range r.Items {
		items = append(items, contract.OrderedObject{
			{Key: "scope", Value: string(it.Scope)},
			{Key: "source", Value: it.Source},
			{Key: "destination", Value: it.Destination},
			{Key: "disposition", Value: string(it.Disposition)},
			{Key: "result", Value: string(it.Result)},
			{Key: "reason", Value: it.Reason},
			{Key: "bytes", Value: it.Size},
			{Key: "digest", Value: digestText(it.Digest)},
			{Key: "mode", Value: modeText(it.Mode)},
		})
	}
	attention := make([]any, 0, len(r.Attention))
	for _, a := range r.Attention {
		attention = append(attention, contract.OrderedObject{
			{Key: "scope", Value: string(a.Scope)},
			{Key: "item", Value: a.Item},
			{Key: "kind", Value: a.Kind},
			{Key: "field", Value: a.Field},
			{Key: "value", Value: a.Value},
			{Key: "detail", Value: a.Detail},
		})
	}
	var failure any
	if r.Error != nil {
		failure = contract.OrderedObject{
			{Key: "kind", Value: r.Error.Kind},
			{Key: "reason", Value: r.Error.Reason},
			{Key: "path", Value: r.Error.Path},
			{Key: "detail", Value: r.Error.Detail},
		}
	}
	return contract.OrderedObject{
		{Key: "schema", Value: SchemaID},
		{Key: "result", Value: r.Result},
		{Key: "dryRun", Value: r.DryRun},
		{Key: "scope", Value: string(r.Scope)},
		{Key: "roots", Value: roots},
		{Key: "items", Value: items},
		{Key: "attention", Value: attention},
		{Key: "error", Value: failure},
		{Key: "writesCompleted", Value: r.WritesCompleted},
		{Key: "sourceVerified", Value: r.SourceVerified},
	}
}

// Text is the report in text: result and scope, the root mappings, the counts, then one line per
// refusal, failure and attention entry.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "result: %s\n", r.Result)
	fmt.Fprintf(&b, "scope: %s\n", r.Scope)
	b.WriteString("roots:\n")
	for _, root := range r.Roots {
		fmt.Fprintf(&b, "  %s: %s -> %s\n", root.Scope, root.Source, root.Destination)
	}
	copied, equal, excluded, refused, attention := r.counts()
	fmt.Fprintf(&b, "counts: copied=%d equal-skipped=%d excluded=%d refused=%d attention=%d\n", copied, equal, excluded, refused, attention)
	if r.Error != nil {
		line := "error: " + r.Error.Kind
		if r.Error.Reason != "" {
			line += " (" + r.Error.Reason + ")"
		}
		if r.Error.Path != "" {
			line += " " + r.Error.Path
		}
		if r.Error.Detail != "" {
			line += ": " + r.Error.Detail
		}
		b.WriteString(line + "\n")
	}
	for _, it := range r.Items {
		switch it.Result {
		case ResultRefused:
			fmt.Fprintf(&b, "refused: %s: %s\n", it.Source, it.Reason)
		case ResultFailed:
			fmt.Fprintf(&b, "failed: %s: %s\n", it.Source, it.Note)
		}
	}
	for _, a := range r.Attention {
		fmt.Fprintf(&b, "attention: %s: %s %s=%s (%s)\n", a.Item, a.Kind, a.Field, a.Value, a.Detail)
	}
	return b.String()
}

// counts tallies the report's items: copied, already equal, excluded (a skip row), refused, and the
// attention entries. In a dry run copied counts the items a real run would copy.
func (r *Report) counts() (copied, equal, excluded, refused, attention int) {
	for _, it := range r.Items {
		switch {
		case it.Disposition == DispSkip:
			excluded++
		case it.Result == ResultCopied:
			copied++
		case it.Result == ResultAlreadyEqual:
			equal++
		case it.Result == ResultRefused:
			refused++
		case it.Result == ResultDryRun:
			copied++
		}
	}
	return copied, equal, excluded, refused, len(r.Attention)
}

// encode is the JSON report's bytes, the document a --report file carries.
func (r *Report) encode() ([]byte, error) {
	var buf bytes.Buffer
	if err := contract.Emit(&buf, r.Object()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// emit renders the report to stdout and returns the exit code it carries.
func (o cli) emit(stdout, stderr io.Writer, r *Report) int {
	if o.jsonOut {
		if err := contract.Emit(stdout, r.Object()); err != nil {
			fmt.Fprintln(stderr, "crw install migrate-state: "+err.Error())
			return codeRefused
		}
	} else if _, err := io.WriteString(stdout, r.Text()); err != nil {
		fmt.Fprintln(stderr, "crw install migrate-state: "+err.Error())
		return codeRefused
	}
	return r.exitCode()
}

// reportTarget is the validated --report destination: the pinned directory that holds the leaf and
// the leaf name. It is published no-replace after the run verified the copied data.
type reportTarget struct {
	parent *Dir
	leaf   string
}

// publish writes the JSON report no-replace, so an existing report is never replaced.
func (t *reportTarget) publish(r *Report) error {
	body, err := r.encode()
	if err != nil {
		return err
	}
	pub, err := NewPublisher()
	if err != nil {
		return err
	}
	_, err = pub.Publish(t.parent, t.leaf, bytes.NewReader(body), int64(len(body)), 0o644)
	return err
}

// digestText is a file's SHA-256 in hex, or "" for a row that carries no file bytes.
func digestText(d [sha256.Size]byte) string {
	if d == ([sha256.Size]byte{}) {
		return ""
	}
	return hex.EncodeToString(d[:])
}

// modeText is the permission bits as the four-digit octal this repository's records use.
func modeText(m fs.FileMode) string { return fmt.Sprintf("%04o", uint32(m.Perm())) }
