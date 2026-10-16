package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// `crw skill size-fit`: the advisory suggester that puts the answer of `crw skill issue-size check` beside
// the counts an issue body states and proposes a size_reason from a closed set.
//
// It makes no grade. The report answers ok, over_line or over_line_accepted and nothing else; size-fit
// shows that answer and the report's exception record next to four counts (edit regions, packages whose
// contract changes, ordered steps, minutes of verification) and an own-goal flag, and proposes the first
// reason of the set whose count crosses its line, else fits_executor. A reason other than fits_executor comes
// with the size_fit split (sub-tasks on one executor thread), never bundle_split (dividing the issue into
// issues is the parent's call). No count alone forces a split: the proposal is read by the planner, who sets
// size_fit and size_reason (crw-plan), and a readable input is always answered with exit 0. `crw flow
// compile` carries what the planner set unchanged.

const sizeFitSchema = "crw-size-fit/1"

// The closed sets of the size fields of an issue line. The order of SizeReasons is the order a proposal
// prefers: the counts first, then own_goal, then fits_executor.
var (
	SizeReasons      = []string{"regions_gt3", "packages_contract_gt2", "steps_gt5", "verify_gt20m", "own_goal", "fits_executor"}
	SizeFits         = []string{"executor", "split", "bundle_split"}
	IssueSizeAnswers = []string{"ok", "over_line", "over_line_accepted"}
)

// The lines the counts are held against: a count of the value or less fits one executor.
const (
	sizeFitMaxRegions         = 3
	sizeFitMaxContractPackage = 2
	sizeFitMaxSteps           = 5
	sizeFitMaxVerifyMinutes   = 20
)

// SizeFitCounts are what the body of an issue states. A nil count was not measured and holds nothing.
type SizeFitCounts struct {
	EditRegions      *int `json:"edit_regions"`
	ContractPackages *int `json:"contract_packages"`
	Steps            *int `json:"steps"`
	VerifyMinutes    *int `json:"verify_minutes"`
}

// SizeFitInput is what a proposal is made from.
type SizeFitInput struct {
	Issue     string
	Answer    string // ok, over_line or over_line_accepted
	Reasons   []string
	Counts    SizeFitCounts
	OwnGoal   bool
	Bundle    string
	Exception *sizeException
	Record    string
}

type sizeFitLimits struct {
	EditRegions      int `json:"edit_regions"`
	ContractPackages int `json:"contract_packages"`
	Steps            int `json:"steps"`
	VerifyMinutes    int `json:"verify_minutes"`
}

// SizeFitProposal is the answer: the report's answer and exception record beside the counts, and a proposed
// reason. Triggers lists every reason whose count crossed its line, in the closed set's order.
type SizeFitProposal struct {
	Schema           string         `json:"schema"`
	Issue            string         `json:"issue"`
	Advisory         bool           `json:"advisory"`
	IssueSize        string         `json:"issue_size"`
	ReportReasons    []string       `json:"report_reasons"`
	Counts           SizeFitCounts  `json:"counts"`
	OwnGoal          bool           `json:"own_goal"`
	Limits           sizeFitLimits  `json:"limits"`
	Triggers         []string       `json:"triggers"`
	SizeReason       string         `json:"size_reason"`
	SuggestedSizeFit string         `json:"suggested_size_fit"`
	BundleReason     string         `json:"bundle_reason,omitempty"`
	Exception        *sizeException `json:"exception,omitempty"`
	ExceptionRecord  string         `json:"exception_record,omitempty"`
	Note             string         `json:"note"`
}

var sizeFitLimitsApplied = sizeFitLimits{sizeFitMaxRegions, sizeFitMaxContractPackage, sizeFitMaxSteps, sizeFitMaxVerifyMinutes}

// SuggestSizeFit is the proposal for one issue. It decides nothing and refuses nothing.
func SuggestSizeFit(in SizeFitInput) SizeFitProposal {
	over := func(n *int, max int) bool { return n != nil && *n > max }
	held := map[string]bool{
		"regions_gt3":           over(in.Counts.EditRegions, sizeFitMaxRegions),
		"packages_contract_gt2": over(in.Counts.ContractPackages, sizeFitMaxContractPackage),
		"steps_gt5":             over(in.Counts.Steps, sizeFitMaxSteps),
		"verify_gt20m":          over(in.Counts.VerifyMinutes, sizeFitMaxVerifyMinutes),
		"own_goal":              in.OwnGoal,
	}
	triggers := []string{}
	for _, reason := range SizeReasons {
		if held[reason] {
			triggers = append(triggers, reason)
		}
	}
	p := SizeFitProposal{
		Schema: sizeFitSchema, Issue: in.Issue, Advisory: true, IssueSize: in.Answer, ReportReasons: append([]string{}, in.Reasons...),
		Counts: in.Counts, OwnGoal: in.OwnGoal, Limits: sizeFitLimitsApplied, Triggers: triggers,
		SizeReason: "fits_executor", SuggestedSizeFit: "executor", BundleReason: in.Bundle, Exception: in.Exception, ExceptionRecord: in.Record,
		Note: "A proposal for the planner to read: no count alone forces a split, and the report's answer is shown beside the counts, not graded.",
	}
	if len(triggers) > 0 {
		p.SizeReason, p.SuggestedSizeFit = triggers[0], "split"
	}
	return p
}

// sizeFitFile is the document the command reads: the report of `crw skill issue-size check` (or the issue it
// reads, which size-fit then checks itself), the counts the body states, and the own-goal flag.
type sizeFitFile struct {
	Report       json.RawMessage `json:"report"`
	Issue        json.RawMessage `json:"issue"`
	BundleReason string          `json:"bundle_reason"`
	Counts       SizeFitCounts   `json:"counts"`
	OwnGoal      bool            `json:"own_goal"`
}

// readSizeFit reads the command's input; an error is an input that cannot be read.
func readSizeFit(raw []byte) (SizeFitInput, error) {
	var file sizeFitFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return SizeFitInput{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SizeFitInput{}, errors.New("more than one JSON value, or text after the document")
	}
	for name, n := range map[string]*int{"edit_regions": file.Counts.EditRegions, "contract_packages": file.Counts.ContractPackages, "steps": file.Counts.Steps, "verify_minutes": file.Counts.VerifyMinutes} {
		if n != nil && *n < 0 {
			return SizeFitInput{}, fmt.Errorf("counts.%s is negative", name)
		}
	}
	var report sizeReport
	switch {
	case len(file.Report) > 0 && len(file.Issue) > 0:
		return SizeFitInput{}, errors.New("give the report or the issue, not both")
	case len(file.Report) > 0:
		if err := json.Unmarshal(file.Report, &report); err != nil {
			return SizeFitInput{}, fmt.Errorf("report: %w", err)
		}
	case len(file.Issue) > 0:
		var issue sizeInput
		if err := json.Unmarshal(file.Issue, &issue); err != nil {
			return SizeFitInput{}, fmt.Errorf("issue: %w", err)
		}
		var err error
		if report, err = sizeReportFor(issue, file.BundleReason); err != nil {
			return SizeFitInput{}, err
		}
	default:
		return SizeFitInput{}, errors.New("the report of `crw skill issue-size check` (report), or the issue it reads (issue), is required")
	}
	if !slices.Contains(IssueSizeAnswers, report.Decision) {
		return SizeFitInput{}, fmt.Errorf("the report's decision %q is not one of %s", report.Decision, strings.Join(IssueSizeAnswers, ", "))
	}
	counts := file.Counts
	if counts.EditRegions == nil {
		n := report.Signals.EditRegions
		counts.EditRegions = &n
	}
	bundle := report.BundleReason
	if bundle == "" {
		bundle = strings.TrimSpace(file.BundleReason)
	}
	return SizeFitInput{
		Issue: report.Issue, Answer: report.Decision, Reasons: report.Reasons, Counts: counts, OwnGoal: file.OwnGoal,
		Bundle: bundle, Exception: report.Exception, Record: report.ExceptionRecord,
	}, nil
}

func runSizeFit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	line := newCommandLine("size-fit", "", sizeFitSummary).takes("file", 0, 1)
	positionals, code := line.parse(args, stdout, stderr)
	if code >= 0 {
		return code
	}
	path := ""
	if len(positionals) == 1 {
		path = positionals[0]
	}
	raw, err := readFileOrStdin(path, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Size fit failed: %s. Nothing was proposed.\n", err)
		return 3
	}
	if !utf8.Valid(raw) {
		fmt.Fprintln(stderr, "Unreadable input: the input is "+errNotUTF8.Error())
		return 2
	}
	in, err := readSizeFit(raw)
	if err != nil {
		fmt.Fprintln(stderr, "Unreadable input: "+err.Error())
		return 2
	}
	_ = emit(stdout, SuggestSizeFit(in))
	return 0
}

const sizeFitSummary = "Read the report of `crw skill issue-size check` (report) or the issue it reads (issue), the counts the issue body states (counts: edit_regions, contract_packages, steps, verify_minutes) and own_goal, and propose a size_reason from the closed set (regions_gt3, packages_contract_gt2, steps_gt5, verify_gt20m, own_goal, fits_executor) beside the report's answer (ok, over_line, over_line_accepted) and its exception record.\n\n" +
	"The proposal is advisory and makes no grade: no count alone forces a split, and a readable input always exits 0. A count of 3 edit regions, 2 contract packages, 5 steps or 20 minutes fits one executor; edit_regions defaults to the report's own count. Exit 2: an input that cannot be read. Exit 3: a file that cannot be read."
