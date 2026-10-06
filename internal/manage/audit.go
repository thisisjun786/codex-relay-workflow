package manage

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The names inside a bundle directory: what the bundle declares itself to be and the file
// a grader must leave behind. The prompt this product writes beside them is added with the
// command that writes it.
const (
	auditBundleFile = "bundle.json"
	auditGradeFile  = "grade.json"
)

// The two schemas of the audit surface: what a bundle declares, and what a grader writes.
const (
	auditBundleSchema = "crw-audit-bundle/1"
	auditResultSchema = "crw-audit-result/1"
)

// The two modes a bundle can be built and graded in.
const (
	auditModePR      = "pr"
	auditModePackage = "package"
)

// auditPromptTemplate is the built-in grader prompt. It is embedded rather than read from
// the host so the product works with no files of its own installed, and it carries no
// private path and no model name, because the grader is blind to both.
//
//go:embed audit_prompt.md
var auditPromptTemplate string

// AuditJob is one bundle to grade, with the ledger metadata the caller attaches. The
// bundle itself carries no pair or model, because the grader is blind to them.
type AuditJob struct {
	Bundle string // the bundle directory
	Pair   string
	Phase  string
	Round  string
}

// AuditDefect is one defect a grader reported, in the shape crw-audit-result/1 fixes.
type AuditDefect struct {
	Severity string `json:"severity"`
	What     string `json:"what"`
	Where    string `json:"where"`
	Repro    string `json:"repro"`
}

// AuditResult is one graded bundle: the ledger row's fields and the defects it found. It
// is what a mode issue reads to decide what to report.
type AuditResult struct {
	Mode     string        `json:"mode"`
	Subject  string        `json:"subject"`
	Head     string        `json:"head"`
	Issue    string        `json:"issue"`
	Pair     string        `json:"pair"`
	Phase    string        `json:"phase"`
	Round    string        `json:"round"`
	Status   string        `json:"status"`
	Score    int           `json:"score"`
	GradedAt string        `json:"graded_at"`
	Bundle   string        `json:"bundle"`
	Defects  []AuditDefect `json:"defects"`
}

// auditBundle is a bundle.json this product has checked.
type auditBundle struct {
	Path                string
	Mode                string
	Subject             string
	Head                string
	Issue               string
	CriteriaUnavailable bool
}

// auditBundleDoc is the document a bundle declares. The identifying fields are pointers so
// a document that omits one is told apart from one that carries an empty value.
type auditBundleDoc struct {
	Schema              *string `json:"schema"`
	Mode                *string `json:"mode"`
	Subject             *string `json:"subject"`
	Head                *string `json:"head"`
	Issue               *string `json:"issue"`
	CriteriaUnavailable bool    `json:"criteria_unavailable"`
}

// auditRequiredFields is what a bundle document must carry, each named as the error names
// it. A bundle without them is not something to grade: the ledger row and the alert line
// both quote the subject, the head and the issue.
var auditRequiredFields = []string{"schema", "mode", "subject", "head", "issue"}

// auditReadBundle reads and checks a bundle's bundle.json. A missing directory, a missing
// file, malformed JSON, another schema, a mode that is neither pr nor package, or a
// required field that is absent or empty is an error naming what is wrong, because a
// bundle this product cannot trust is not something to grade.
func auditReadBundle(dir string) (*auditBundle, error) {
	if dir == "" {
		return nil, errors.New("the bundle directory is empty")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle %s: not a directory", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, auditBundleFile))
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", dir, err)
	}
	var doc auditBundleDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("bundle %s: %s: %v", dir, auditBundleFile, err)
	}
	values := map[string]*string{
		"schema": doc.Schema, "mode": doc.Mode, "subject": doc.Subject,
		"head": doc.Head, "issue": doc.Issue,
	}
	for _, name := range auditRequiredFields {
		if value := values[name]; value == nil || *value == "" {
			return nil, fmt.Errorf("bundle %s: %s: the required field %q is missing or empty", dir, auditBundleFile, name)
		}
	}
	if *doc.Schema != auditBundleSchema {
		return nil, fmt.Errorf("bundle %s: schema %q is not %s", dir, *doc.Schema, auditBundleSchema)
	}
	if *doc.Mode != auditModePR && *doc.Mode != auditModePackage {
		return nil, fmt.Errorf("bundle %s: mode %q is not %q or %q", dir, *doc.Mode, auditModePR, auditModePackage)
	}
	return &auditBundle{
		Path: dir, Mode: *doc.Mode, Subject: *doc.Subject, Head: *doc.Head,
		Issue: *doc.Issue, CriteriaUnavailable: doc.CriteriaUnavailable,
	}, nil
}

// auditPrompt is the prompt written into a bundle: the built-in template with the
// guidance the bundle's mode needs, and a word about a bundle that came without criteria.
func auditPrompt(b *auditBundle) string {
	var out strings.Builder
	out.WriteString(strings.TrimRight(auditPromptTemplate, "\n"))
	out.WriteString("\n\n## Mode\n\n")
	if b.Mode == auditModePR {
		out.WriteString("This is a pull request audit. `candidate/diff.patch` is the change that was merged, `candidate/pr.md` is the description its author wrote, and `candidate/tree/` is the whole source tree as it stands after the merge, for reading the code around the change.")
	} else {
		out.WriteString("This is a package audit. There is no single change to review: `candidate/tree/` is the whole source tree, and the criteria and the issue text name what it is judged against. Read the tree as it stands and judge the package.")
	}
	if b.CriteriaUnavailable {
		out.WriteString("\n\nThis bundle declares `criteria_unavailable`. Judge against the issue text under `inputs/` and the candidate's own description, and say in each note which of the two you used.")
	}
	return out.String()
}

// auditCommand is crw manage audit.
var auditCommand = Command{Name: "audit", Summary: "grade an audit bundle and record the result", Run: auditRun}

func init() { Register(auditCommand) }

// auditUsage is the one line the audit command prints.
const auditUsage = "usage: crw manage audit [-h]"

// auditRun is crw manage audit. This issue registers the command and its help; the
// subcommand that builds a bundle and the one that grades it arrive in later issues, so
// every argument other than the help flags is a usage error.
func auditRun(_ context.Context, e *Env, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprintln(e.Stdout, auditUsage)
			return 0
		}
	}
	fmt.Fprintln(e.Stderr, auditUsage)
	if len(args) > 0 {
		fmt.Fprintf(e.Stderr, "crw manage audit: error: invalid command %q\n", args[0])
	}
	return usageExit
}
