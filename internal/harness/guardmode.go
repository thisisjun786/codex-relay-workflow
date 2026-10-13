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

// WarningPrefix starts every warning a guard leaves in place of a refusal.
const WarningPrefix = "[crw warning] Not blocked (recorded): "

// warnAnswer is the PreToolUse answer that lets the call through and gives the session the warning: additionalContext only, no
// permissionDecision (the host refuses a bare allow), so the host's own approval rules stay as they are.
func warnAnswer(warning string) string {
	type output struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	b, err := jsnode.Stringify(struct {
		Output output `json:"hookSpecificOutput"`
	}{output{"PreToolUse", WarningPrefix + warning}}, "")
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
)

// RuleOf names the rule of a refusal from its text when the leg did not name it: a [crw tag] (comment-lint), else a code
// (GOAL-COMPLETE-GATE-01), else a cause in parentheses, read from the head of the reason only, where the guards put them and before
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
	RecordDecision(raw, leg, rule, DecisionWarn, env)
	return warnAnswer(reason)
}

// ReleaseGuard is a PreToolUse leg's answer for a hook input it could not read whole: the call passes with a warning and the
// release is recorded (fail-open). raw is what was read, if anything; it only names the session.
func ReleaseGuard(leg, raw, cause string, env host.LookupEnv) string {
	RecordDecision(raw, leg, RuleUnreadableInput, DecisionRelease, env)
	return releaseAnswer(cause)
}

// releaseAnswer is the warning of a released hook input.
func releaseAnswer(cause string) string {
	return warnAnswer("the hook input could not be read (" + cause + "), so the guard did not check this call.")
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
	if root, _ := env("PLUGIN_ROOT"); root == "" {
		return false
	}
	session, agent := "null", "null"
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
	home, set := env("CODEX_HOME")
	if !set || home == "" {
		h, err := host.Home(env)
		if err != nil {
			return false
		}
		home = filepath.Join(h, ".codex")
	}
	dir := filepath.Join(home, "crw", "hook-decisions")
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
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
