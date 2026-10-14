package harness

// The PreToolUse guards' modes (CRW-1307). Jun decided on 10-13 that a reversible action gets no hard gate: a guard blocks
// only what loses data, destroys history or exposes a secret, and warns about the rest. Each PreToolUse leg still judges a
// call as before; SettleGuard then reads the leg's deny answer against the mode table. A rule the table keeps at block keeps
// the deny; any other deny becomes a pass with a warning the session sees and a decision record. A hook input the leg cannot
// read is released with a record too (fail-open), in every leg.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/jsnode"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// GuardMode is what a guard does with a call one of its rules refuses.
type GuardMode int

const (
	// GuardWarn lets the call through with a warning the session sees, and records the decision.
	GuardWarn GuardMode = iota
	// GuardBlock keeps the deny.
	GuardBlock
)

// The decisions a record names.
const (
	DecisionWarn    = "warn"    // a refusal turned into a pass with a warning
	DecisionBlock   = "block"   // a refusal the table keeps
	DecisionRelease = "release" // a hook input that could not be read, let through unchecked
)

// The rules the table keeps at block. A leg names the rule of its answer explicitly (Call.Rule, or the rule argument of
// SettleGuard); a rule derived from the answer's text is never one of these.
const (
	// RuleSharedBranchHistory is a relay child's force, delete or --mirror push onto dev or main.
	RuleSharedBranchHistory = "shared-branch-history"
	// RuleSecretInGitHubText is a secret in the title or body text of a GitHub post.
	RuleSecretInGitHubText = "secret-in-github-text"
	// RuleResolvedDeletion is a deletion of the session's managed worktree checkout, slot or an ancestor whose target the
	// guard resolved.
	RuleResolvedDeletion = "resolved-deletion"
	// RuleUnreadableInput names a hook input that could not be read whole.
	RuleUnreadableInput = "unreadable-input"
	// RuleInterviewInGoal names the goal-mode refusal of a blocking interview (request_user_input); it warns.
	RuleInterviewInGoal = "interview-in-goal"
)

// guardBlocks is the mode table: per leg, the rules that stay at block. Every other rule of every PreToolUse leg warns.
var guardBlocks = map[string][]string{
	"pre-tool-use-guarding-child-push":                {RuleSharedBranchHistory},
	"pre-tool-use-guarding-github-post":               {RuleSecretInGitHubText},
	"pre-tool-use-guarding-managed-worktree-deletion": {RuleResolvedDeletion},
}

// Mode is the table's mode for a leg's rule.
func Mode(leg, rule string) GuardMode {
	for _, r := range guardBlocks[leg] {
		if r == rule {
			return GuardBlock
		}
	}
	return GuardWarn
}

// WarningPrefix starts a warning a guard leaves in place of a refusal when its decision record was written; WarningPrefixUnrecorded
// starts it when no record was written (a hook outside a plugin, or a home the record cannot be written to).
const (
	WarningPrefix           = "[crw warning] Not blocked (recorded): "
	WarningPrefixUnrecorded = "[crw warning] Not blocked (no record written): "
)

// warnAnswer is the PreToolUse answer that lets the call through and gives the session the warning: additionalContext only, no
// permissionDecision (the host refuses a bare allow), so the host's own approval rules stay as they are. recorded is whether the
// decision record was written; the warning says which.
func warnAnswer(warning string, recorded bool) string {
	prefix := WarningPrefix
	if !recorded {
		prefix = WarningPrefixUnrecorded
	}
	type output struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	b, err := jsnode.Stringify(struct {
		Output output `json:"hookSpecificOutput"`
	}{output{"PreToolUse", prefix + warning}}, "")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}

// guardDeny is the text of a PreToolUse deny answer, and whether the answer is one.
func guardDeny(answer string) (string, bool) {
	var a struct {
		Output struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
			Context  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(answer)), &a) != nil || a.Output.Decision != "deny" {
		return "", false
	}
	// The context is what the session saw of the refusal; some legs put more in it than in the reason (goal-active=unreadable).
	if a.Output.Context == "" {
		return a.Output.Reason, true
	}
	return a.Output.Context, true
}

var (
	ruleCode  = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+\b`)
	ruleTag   = regexp.MustCompile(`\[crw[: ]+([a-z][a-z0-9-]*)\]`)
	ruleCause = regexp.MustCompile(`\(([a-z][a-z0-9]*(?:-[a-z0-9]+)+)\)`)
	// ruleTitle is a tag of words after a dash ([crw — final gate]) and ruleLead a lead of one to three lower-case words and a colon
	// at the very start (managed dispatch:): the spawn leg's refusals name their cause this way.
	ruleTitle = regexp.MustCompile(`^\[crw (?:—|-|:) ?([a-z][a-z0-9]*(?: [a-z0-9]+){0,3})\]`)
	ruleLead  = regexp.MustCompile(`^([a-z][a-z0-9]*(?: [a-z][a-z0-9]*){0,2}): `)
)

// RuleOf names the rule of a refusal from its text when the leg did not name it: a [crw tag] (comment-lint), else a code
// (GOAL-COMPLETE-GATE-01), else a cause in parentheses, else a titled tag ([crw — final gate]: final-gate) or a lead of lower-case
// words and a colon (managed dispatch: managed-dispatch), read from the head of the reason only, where the guards put them and before
// any part of the command a reason may quote. The name is a slug of at most 64 characters; "refusal" when there is none.
func RuleOf(reason string) string {
	head := reason
	if len(head) > 64 {
		head = head[:64]
	}
	name := ""
	if m := ruleTag.FindStringSubmatch(head); m != nil {
		name = m[1]
	} else if m := ruleCode.FindString(head); m != "" {
		name = strings.ToLower(m)
	} else if m := ruleCause.FindStringSubmatch(head); m != nil {
		name = m[1]
	} else if m := ruleTitle.FindStringSubmatch(head); m != nil {
		name = strings.ReplaceAll(m[1], " ", "-")
	} else if m := ruleLead.FindStringSubmatch(head); m != nil {
		name = strings.ReplaceAll(m[1], " ", "-")
	}
	if name == "" || len(name) > 64 {
		return "refusal"
	}
	return name
}

// SettleGuard is a PreToolUse leg's answer under the mode table. An answer that is not a deny stays as it is. A deny whose rule
// the table keeps at block stays a deny and is recorded; any other deny becomes a pass with the refusal as a warning, and is
// recorded. rule is the rule the leg names for its answer; "" derives a name from the reason, which never blocks.
func SettleGuard(leg, raw, answer, rule string, env host.LookupEnv) string {
	reason, deny := guardDeny(answer)
	if !deny {
		return answer
	}
	if rule != "" && Mode(leg, rule) == GuardBlock {
		RecordDecision(raw, leg, rule, DecisionBlock, env)
		return answer
	}
	if rule == "" {
		rule = RuleOf(reason)
	}
	return warnAnswer(reason, RecordDecision(raw, leg, rule, DecisionWarn, env))
}

// SettleGuardOnce is SettleGuard for a warning whose record a session leaves once (CRW-1387: the child push guard's unreadable lineage of
// a sub-agent, whose session is mostly no relay child, warned and recorded on every push). once names the warning; the first such warn
// of a session claims the marker <sha256 of the session's JSON>.<leg>.<once>.once beside the session's records (O_EXCL, so concurrent
// hooks claim it once) and records the decision; a later one of the same session passes with the same warning and leaves no record. A
// marker that cannot be made leaves the record as SettleGuard does. An answer that is no deny, and a deny the table keeps at block,
// are settled as SettleGuard settles them; once "" is SettleGuard.
func SettleGuardOnce(leg, raw, answer, rule, once string, env host.LookupEnv) string {
	reason, deny := guardDeny(answer)
	if !deny || !slug(once) || (rule != "" && Mode(leg, rule) == GuardBlock) {
		return SettleGuard(leg, raw, answer, rule, env)
	}
	if rule == "" {
		rule = RuleOf(reason)
	}
	marker, first := claimOnce(raw, leg, once, env)
	if !first {
		return warnAnswer(reason+" This session's first such warning was recorded; this one leaves no record.", false)
	}
	recorded := RecordDecision(raw, leg, rule, DecisionWarn, env)
	if !recorded && marker != "" {
		_ = os.Remove(marker)
	}
	return warnAnswer(reason, recorded)
}

// claimOnce creates the once marker of a session's warning: first is false only when the marker already exists; marker is the created
// file, "" when none could be made (no record directory: the caller records as it would without one).
func claimOnce(raw, leg, once string, env host.LookupEnv) (marker string, first bool) {
	if !slug(leg) {
		return "", true
	}
	dir, ok := decisionsDir(env)
	if !ok {
		return "", true
	}
	session, _ := decisionSubjects(raw)
	digest := sha256.Sum256([]byte(session))
	path := filepath.Join(dir, hex.EncodeToString(digest[:])+"."+leg+"."+once+".once")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case errors.Is(err, fs.ErrExist):
		return "", false
	case err != nil:
		return "", true
	}
	if f.Close() != nil {
		_ = os.Remove(path)
		return "", true
	}
	return path, true
}

// decisionsDir is <CODEX_HOME>/crw/hook-decisions, made if missing; false for a hook outside a plugin (PLUGIN_ROOT unset) or a directory
// that cannot be made.
func decisionsDir(env host.LookupEnv) (string, bool) {
	if root, _ := env("PLUGIN_ROOT"); root == "" {
		return "", false
	}
	home, set := env("CODEX_HOME")
	if !set || home == "" {
		h, err := host.Home(env)
		if err != nil {
			return "", false
		}
		home = filepath.Join(h, ".codex")
	}
	dir := filepath.Join(home, "crw", "hook-decisions")
	if os.MkdirAll(dir, 0o700) != nil {
		return "", false
	}
	return dir, true
}

// decisionSubjects are the JSON strings of the session_id and agent_id a hook input names, "null" for one it does not name.
func decisionSubjects(raw string) (session, agent string) {
	session, agent = "null", "null"
	if len(raw) <= MaxStdinBytes {
		if v, ok := decode(raw); ok {
			o, _ := v.(map[string]any)
			if s, _ := o["session_id"].(string); metadata(s) {
				session = jsnode.JSString(s)
			}
			if s, _ := o["agent_id"].(string); metadata(s) {
				agent = jsnode.JSString(s)
			}
		}
	}
	return session, agent
}

// ReleaseGuard is a PreToolUse leg's answer for a hook input it could not read whole: the call passes with a warning and the
// release is recorded (fail-open). raw is what was read, if anything; it only names the session.
func ReleaseGuard(leg, raw, cause string, env host.LookupEnv) string {
	return releaseAnswer(cause, RecordDecision(raw, leg, RuleUnreadableInput, DecisionRelease, env))
}

// releaseAnswer is the warning of a released hook input.
func releaseAnswer(cause string, recorded bool) string {
	return warnAnswer("the hook input could not be read ("+cause+"), so the guard did not check this call.", recorded)
}

// RecordDecision leaves the record of one guard decision: the leg, the rule, the decision, the time and the session (and the
// subagent, when the input names one), never the command text or any other part of the payload. A line is appended to the session's
// file <CODEX_HOME>/crw/hook-decisions/<sha256 of the session's JSON>.jsonl ("null" for an input that names none). Like
// RecordInvocation it is a diagnostic: it never disturbs the hook, writes nothing for a hook outside a plugin (PLUGIN_ROOT unset),
// and reports only whether it wrote a record.
func RecordDecision(raw, leg, rule, decision string, env host.LookupEnv) (recorded bool) {
	defer func() {
		if recover() != nil {
			recorded = false
		}
	}()
	if !slug(leg) || !slug(rule) || !slug(decision) {
		return false
	}
	dir, ok := decisionsDir(env)
	if !ok {
		return false
	}
	session, agent := decisionSubjects(raw)
	digest := sha256.Sum256([]byte(session))
	record := `{"schemaVersion":1,"sessionId":` + session + `,"agentId":` + agent + `,"leg":` + jsnode.JSString(leg) + `,"rule":` + jsnode.JSString(rule) +
		`,"decision":` + jsnode.JSString(decision) + `,"decidedAt":"` + time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + `"}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(digest[:])+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.WriteString(record) // one write of one line: O_APPEND keeps concurrent hooks' lines whole
	return f.Close() == nil && werr == nil
}

// ReadDecisions is every decision record under codexHome, oldest first (lines of one session in their order), for a test or a
// diagnostic reader.
func ReadDecisions(codexHome string) []map[string]any {
	paths, _ := filepath.Glob(filepath.Join(codexHome, "crw", "hook-decisions", "*.jsonl"))
	var out []map[string]any
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var v map[string]any
			if json.Unmarshal([]byte(line), &v) == nil {
				out = append(out, v)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b map[string]any) int {
		x, _ := a["decidedAt"].(string)
		y, _ := b["decidedAt"].(string)
		return strings.Compare(x, y)
	})
	return out
}
