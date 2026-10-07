// Loop CLI argument parsing and slug resolution: goalplan-cli.ts (:171-322) at
// v0.2.40. Command registration and the init/show/validate/ready verbs are CRW-646; this
// file ports only the structural parser and the slug resolver those verbs share.
package cli

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// LoopVerb is goalplan-cli.ts GoalplanVerb (:55-67). The type carries "help"; the VERBS
// set the parser matches against does not, because help early-returns first (:176-179).
type LoopVerb string

// The loop verbs.
const (
	LoopVerbInit          LoopVerb = "init"
	LoopVerbShow          LoopVerb = "show"
	LoopVerbValidate      LoopVerb = "validate"
	LoopVerbSteer         LoopVerb = "steer"
	LoopVerbAddCriterion  LoopVerb = "add-criterion"
	LoopVerbAddWorkPhase  LoopVerb = "add-work-phase"
	LoopVerbReady         LoopVerb = "ready"
	LoopVerbAddTask       LoopVerb = "add-task"
	LoopVerbCompleteTask  LoopVerb = "complete-task"
	LoopVerbMeetCriterion LoopVerb = "meet-criterion"
	LoopVerbAsk           LoopVerb = "ask"
	LoopVerbDecide        LoopVerb = "decide"
	LoopVerbHelp          LoopVerb = "help"
)

// loopVerbList is the VERBS set in insertion order (:127-140), as the unknown-verb
// message prints it.
const loopVerbList = "init|show|validate|steer|add-criterion|add-work-phase|ready|add-task|complete-task|meet-criterion|ask|decide"

// loopFlag is one flag goalplan-cli.ts accepts (:142-148).
type loopFlag string

// The loop flags.
const (
	loopFlagObjective      loopFlag = "--objective"
	loopFlagSlug           loopFlag = "--slug"
	loopFlagCriterion      loopFlag = "--criterion"
	loopFlagCwd            loopFlag = "--cwd"
	loopFlagSession        loopFlag = "--session"
	loopFlagBatchJSON      loopFlag = "--batch-json"
	loopFlagSurface        loopFlag = "--surface"
	loopFlagPresented      loopFlag = "--presented"
	loopFlagID             loopFlag = "--id"
	loopFlagTitle          loopFlag = "--title"
	loopFlagWorkPhase      loopFlag = "--work-phase"
	loopFlagOutcome        loopFlag = "--outcome"
	loopFlagSchemaVersion  loopFlag = "--schema-version"
	loopFlagEvidence       loopFlag = "--evidence"
	loopFlagJSON           loopFlag = "--json"
	loopFlagDependsOn      loopFlag = "--depends-on"
	loopFlagQuestion       loopFlag = "--question"
	loopFlagRecommendation loopFlag = "--recommendation"
	loopFlagOption         loopFlag = "--option"
	loopFlagAnswer         loopFlag = "--answer"
)

// loopRule is one verb's allowed and repeatable flags (goalplan-cli.ts VerbRule).
type loopRule struct {
	allowed    []loopFlag
	repeatable []loopFlag
}

// loopIsVerb reports whether v is in the VERBS set (:127-140). help is not: it is
// handled before this check.
func loopIsVerb(v LoopVerb) bool {
	switch v {
	case LoopVerbInit, LoopVerbShow, LoopVerbValidate, LoopVerbSteer, LoopVerbAddCriterion,
		LoopVerbAddWorkPhase, LoopVerbReady, LoopVerbAddTask, LoopVerbCompleteTask,
		LoopVerbMeetCriterion, LoopVerbAsk, LoopVerbDecide:
		return true
	}
	return false
}

// loopRuleFor is VERB_RULES (:154-168). The table is built per call rather than held in a
// package variable, so nothing runs at program start. The usage strings live in loopHelp.
func loopRuleFor(verb LoopVerb) loopRule {
	switch verb {
	case LoopVerbInit:
		return loopRule{
			allowed:    []loopFlag{loopFlagObjective, loopFlagSession, loopFlagCriterion, loopFlagSchemaVersion, loopFlagCwd},
			repeatable: []loopFlag{loopFlagCriterion},
		}
	case LoopVerbShow, LoopVerbValidate:
		return loopRule{allowed: []loopFlag{loopFlagSlug, loopFlagObjective, loopFlagSession, loopFlagCwd}}
	case LoopVerbSteer:
		return loopRule{allowed: []loopFlag{loopFlagSession, loopFlagBatchJSON, loopFlagCwd}}
	case LoopVerbAddCriterion:
		return loopRule{allowed: []loopFlag{loopFlagSession, loopFlagCriterion, loopFlagSurface, loopFlagPresented, loopFlagCwd}}
	case LoopVerbAddWorkPhase:
		return loopRule{
			allowed:    []loopFlag{loopFlagSession, loopFlagID, loopFlagTitle, loopFlagDependsOn, loopFlagCwd},
			repeatable: []loopFlag{loopFlagDependsOn},
		}
	case LoopVerbReady:
		return loopRule{allowed: []loopFlag{loopFlagSlug, loopFlagObjective, loopFlagSession, loopFlagJSON, loopFlagCwd}}
	case LoopVerbAddTask:
		return loopRule{
			allowed:    []loopFlag{loopFlagSession, loopFlagWorkPhase, loopFlagID, loopFlagTitle, loopFlagDependsOn, loopFlagCwd},
			repeatable: []loopFlag{loopFlagDependsOn},
		}
	case LoopVerbCompleteTask:
		return loopRule{allowed: []loopFlag{loopFlagSession, loopFlagWorkPhase, loopFlagID, loopFlagOutcome, loopFlagCwd}}
	case LoopVerbMeetCriterion:
		return loopRule{allowed: []loopFlag{loopFlagSession, loopFlagID, loopFlagEvidence, loopFlagCwd}}
	case LoopVerbAsk:
		return loopRule{
			allowed:    []loopFlag{loopFlagSession, loopFlagID, loopFlagQuestion, loopFlagRecommendation, loopFlagOption, loopFlagWorkPhase, loopFlagCwd},
			repeatable: []loopFlag{loopFlagOption, loopFlagWorkPhase},
		}
	}
	// decide: --session/--id/--answer/--cwd. help never reaches the parse loop.
	return loopRule{allowed: []loopFlag{loopFlagSession, loopFlagID, loopFlagAnswer, loopFlagCwd}}
}

// LoopCliArgs is goalplan-cli.ts GoalplanCliArgs (:69-124). A nil pointer is an absent
// flag; Criteria is always a list. DependsOn and WorkPhaseIDs are present as [] for a
// real verb and absent on the help result, so they are pointers, matching the two shapes
// the oracle builds (:178 and :188).
type LoopCliArgs struct {
	Verb           LoopVerb  `json:"verb"`
	Cwd            string    `json:"cwd"`
	Objective      *string   `json:"objective,omitempty"`
	Slug           *string   `json:"slug,omitempty"`
	Criteria       []string  `json:"criteria"`
	Session        *string   `json:"session,omitempty"`
	BatchJSON      *string   `json:"batchJson,omitempty"`
	Surface        *string   `json:"surface,omitempty"`
	SurfaceGiven   bool      `json:"surfaceGiven,omitempty"`
	Presented      *string   `json:"presented,omitempty"`
	SchemaVersion  *float64  `json:"schemaVersion,omitempty"`
	ID             *string   `json:"id,omitempty"`
	Title          *string   `json:"title,omitempty"`
	DependsOn      *[]string `json:"dependsOn,omitempty"`
	WorkPhaseID    *string   `json:"workPhaseId,omitempty"`
	WorkPhaseIDs   *[]string `json:"workPhaseIds,omitempty"`
	Question       *string   `json:"question,omitempty"`
	Recommendation *string   `json:"recommendation,omitempty"`
	Options        []string  `json:"options,omitempty"`
	Answer         *string   `json:"answer,omitempty"`
	Outcome        *string   `json:"outcome,omitempty"`
	Evidence       *string   `json:"evidence,omitempty"`
	JSON           bool      `json:"json,omitempty"`
}

// loopString is the address of v, for an optional field the parser just read.
func loopString(v string) *string { return &v }

// loopLower is the oracle's argv[0].toLowerCase() (:172) for a verb token: ASCII is
// folded and U+0130 expands to i plus a combining dot, as V8 lowercases it.
func loopLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307"))
}

// ParseLoopCliArgs is parseGoalplanCliArgs (:171-276): a structural parse where the verb
// is argv[0] and every flag takes the next token. The error text is the oracle's,
// including the "crw pabcd loop --help" remedy (name-substitution).
func ParseLoopCliArgs(argv []string, cwd string) (LoopCliArgs, error) {
	verb := ""
	if len(argv) > 0 {
		verb = loopLower(argv[0])
	}
	if verb == "help" || verb == "--help" || verb == "-h" {
		if len(argv) > 1 {
			return LoopCliArgs{}, fmt.Errorf("help: unexpected argument '%s'", argv[1])
		}
		return LoopCliArgs{Verb: LoopVerbHelp, Cwd: cwd, Criteria: []string{}}, nil
	}
	if !loopIsVerb(LoopVerb(verb)) {
		raw := ""
		if len(argv) > 0 {
			raw = argv[0]
		}
		return LoopCliArgs{}, fmt.Errorf("unknown loop verb '%s' (expected %s); run crw pabcd loop --help", raw, loopVerbList)
	}
	selected := LoopVerb(verb)
	rule := loopRuleFor(selected)
	dependsOn, workPhaseIDs := []string{}, []string{}
	out := LoopCliArgs{Verb: selected, Cwd: cwd, Criteria: []string{}, DependsOn: &dependsOn, WorkPhaseIDs: &workPhaseIDs}
	seen := map[loopFlag]bool{}
	reject := func(message string) error { return fmt.Errorf("%s: %s", selected, message) }
	for i := 1; i < len(argv); i++ {
		token := argv[i]
		if !strings.HasPrefix(token, "--") {
			return LoopCliArgs{}, reject(fmt.Sprintf("unexpected positional argument '%s'", token))
		}
		// Every value flag also takes --flag=value, the only way to pass a value that
		// itself starts with -- (the space form treats that as a missing value).
		flag := token
		var inline *string
		if eq := strings.Index(token, "="); eq > 0 {
			flag = token[:eq]
			value := token[eq+1:]
			inline = &value
		}
		f := loopFlag(flag)
		if !slices.Contains(rule.allowed, f) {
			if selected == LoopVerbInit && f == loopFlagSurface {
				return LoopCliArgs{}, reject("--surface is not applied at init; use add-criterion --surface <logic|web|tui|desktop>. Nothing was written.")
			}
			return LoopCliArgs{}, reject(fmt.Sprintf("unknown flag '%s'", token))
		}
		if seen[f] && !slices.Contains(rule.repeatable, f) {
			return LoopCliArgs{}, reject(flag + " may be provided only once")
		}
		seen[f] = true
		if f == loopFlagJSON {
			if inline != nil {
				return LoopCliArgs{}, reject("--json takes no value")
			}
			out.JSON = true
			continue
		}
		var value string
		missing := false
		if inline != nil {
			value = *inline
			missing = value == ""
		} else {
			i++
			if i < len(argv) {
				value = argv[i]
				missing = strings.HasPrefix(value, "--")
			} else {
				missing = true
			}
		}
		if missing {
			if f == loopFlagSurface {
				return LoopCliArgs{}, reject("--surface needs a value (logic|web|tui|desktop)")
			}
			if inline != nil {
				return LoopCliArgs{}, reject(flag + " requires a value")
			}
			return LoopCliArgs{}, reject(flag + " requires a value (use " + flag + "=<value> for a value that starts with --)")
		}
		switch f {
		case loopFlagObjective:
			out.Objective = loopString(value)
		case loopFlagSlug:
			out.Slug = loopString(value)
		case loopFlagCriterion:
			out.Criteria = append(out.Criteria, value)
		case loopFlagCwd:
			out.Cwd = value
		case loopFlagSession:
			out.Session = loopString(value)
		case loopFlagBatchJSON:
			out.BatchJSON = loopString(value)
		case loopFlagSurface:
			out.SurfaceGiven = true
			out.Surface = loopString(value)
		case loopFlagPresented:
			out.Presented = loopString(value)
		case loopFlagID:
			out.ID = loopString(value)
		case loopFlagTitle:
			out.Title = loopString(value)
		case loopFlagWorkPhase:
			if selected != LoopVerbAsk {
				out.WorkPhaseID = loopString(value)
				break
			}
			phaseID := text.Trim(value)
			if phaseID == "" {
				return LoopCliArgs{}, reject("--work-phase requires one non-empty id")
			}
			if slices.Contains(*out.WorkPhaseIDs, phaseID) {
				return LoopCliArgs{}, reject(fmt.Sprintf("--work-phase must not repeat id '%s'", phaseID))
			}
			*out.WorkPhaseIDs = append(*out.WorkPhaseIDs, phaseID)
		case loopFlagQuestion:
			out.Question = loopString(value)
		case loopFlagRecommendation:
			out.Recommendation = loopString(value)
		case loopFlagOption:
			option := text.Trim(value)
			if option == "" {
				return LoopCliArgs{}, reject("--option requires one non-empty value")
			}
			if slices.Contains(out.Options, option) {
				return LoopCliArgs{}, reject(fmt.Sprintf("--option must not repeat '%s'", option))
			}
			out.Options = append(out.Options, option)
		case loopFlagAnswer:
			out.Answer = loopString(value)
		case loopFlagOutcome:
			out.Outcome = loopString(value)
		case loopFlagSchemaVersion:
			parsed := metricCliNumber(value)
			if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
				return LoopCliArgs{}, reject("--schema-version requires a finite number")
			}
			out.SchemaVersion = &parsed
		case loopFlagEvidence:
			out.Evidence = loopString(value)
		case loopFlagDependsOn:
			dependency := text.Trim(value)
			if dependency == "" {
				return LoopCliArgs{}, reject("--depends-on requires one non-empty prerequisite id")
			}
			if slices.Contains(*out.DependsOn, dependency) {
				return LoopCliArgs{}, reject(fmt.Sprintf("--depends-on must not repeat prerequisite id '%s'", dependency))
			}
			*out.DependsOn = append(*out.DependsOn, dependency)
		default:
			return LoopCliArgs{}, reject(fmt.Sprintf("unknown flag '%s'", flag))
		}
	}
	return out, nil
}

// ResolveLoopSlug is resolveSlug (:277-289): the derived slug from --slug or --objective,
// else the slug a --session has bound into its state file. nil when none resolves.
func ResolveLoopSlug(args LoopCliArgs) *string {
	if args.Slug != nil && *args.Slug != "" {
		return loopString(interview.DeriveSlug(*args.Slug))
	}
	if args.Objective != nil && *args.Objective != "" {
		return loopString(interview.DeriveSlug(*args.Objective))
	}
	// The TRIMMED value, the one RunLoopCli's canonical guard tested (CRW-646 c3): reading the raw
	// flag here would let a blanks-only --session skip that guard and resolve to the sanitized
	// 'missing' session's state file, printing a plan the caller never named.
	if sessionID := loopSessionID(args); sessionID != "" {
		if bound := state.ReadState(args.Cwd, sessionID).Slug; bound != "" {
			return &bound
		}
	}
	return nil
}

// DescribeLoopReadFailure is describeReadFailure (:302-313): one sentence naming the
// actual failure, so a truncated write and an absent plan do not read alike. The absent
// sentence's remedy is spelled in crw names (name-substitution cli table).
func DescribeLoopReadFailure(read goalplan.GoalplanReadResult, verb string, slug string) string {
	d := read.Diagnostic
	var detail string
	switch {
	case d != nil && d.Kind == "absent":
		detail = fmt.Sprintf("no plan found at slug '%s' (%s does not exist) - run `crw pabcd loop init --objective \"...\"`", slug, d.Path)
	case d != nil && d.Kind == "invalid-json":
		detail = fmt.Sprintf("the plan at %s is not valid JSON: %s", d.Path, d.Detail)
	case d != nil && d.Kind == "invalid-shape":
		detail = fmt.Sprintf("the plan at %s is structurally invalid - field '%s': %s", d.Path, d.Field, d.Detail)
	default:
		path := slug
		if d != nil {
			path = d.Path
		}
		reason := "unknown"
		if d != nil && d.Kind == "unreadable" {
			reason = d.Detail
		}
		detail = fmt.Sprintf("the plan at %s could not be read: %s", path, reason)
	}
	return fmt.Sprintf("loop %s: %s", verb, detail)
}
