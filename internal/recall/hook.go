// CXC v0.2.40 (3c1459ac) recall/src/hook.ts:40-220,599-788 and cli.ts:392-415.
package recall

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

type UserPromptSubmitPayload struct {
	HookEventName any    `json:"hook_event_name,omitempty"`
	Prompt        any    `json:"prompt,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
}
type SessionStartPayload struct {
	HookEventName any `json:"hook_event_name,omitempty"`
	Cwd           any `json:"cwd,omitempty"`
	SessionID     any `json:"session_id,omitempty"`
	Source        any `json:"source,omitempty"`
}
type SessionStartOptions struct {
	DedicatedTools     *bool
	MemoryNotice, Home string
}
type HookResult struct {
	Stdout, Stderr string
	Code           int
}

const recallHookMaxContext = 32768
const recallHookRecoveryBudget = 160
const recallHookSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

func recallHookRE(pattern string) *regexp.Regexp {
	return regexp.MustCompile(strings.ReplaceAll(pattern, `\s`, recallHookSpace))
}

// ECMAScript's non-unicode /i does not fold Kelvin sign or long s into ASCII.
func recallHookASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, s)
}
func DetectRecallIntent(prompt string) bool {
	if text.Trim(prompt) == "" || recallHookRE(`\bcrw\s+recall\s+(chat|memory)\s+(search|index)\b|\b(chat|memory)\s+search\s+["']|\$crw-recall\b`).MatchString(prompt) {
		return false
	}
	patterns := []string{
		`그때\s*(그|한|했|만든|작업)`, `지난\s*번`, `지난\s*세션`, `저번\s*(에|세션|주|것|거)`,
		`예전에\s*(하|했|만든|작업|쓰)`, `전에\s*(했|만든|작업했|얘기했|말했)`, `기억\s*(나|안\s*나|하니|하냐)`,
		`뭐였지|뭐\s*였더라|어떻게\s*했었지|어디까지\s*했`, `\blast\s+(time|session|week)\b`,
		`\bprevious\s+(session|work|conversation|discussion|chat)\b`, `\bpreviously\s+(we|i|you|the\s+team|discussed|agreed|decided)\b`,
		`\bwhat\s+did\s+(we|i|you)\s+(do|discuss|decide|build)\b`, `\bremember\s+(when|what|the|that|how)\b`,
		`\b(as|we)\s+discussed\s+(earlier|before|previously|last\s+time)\b`, `\bdiscussed\s+previously\b`, `\bearlier\s+(session|conversation|work)\b`,
		`이전에\s*(하|했|만든|작업|얘기|말)`, `그\s*세션`, `그때에(?:는|도)?`, `\bprior\s+(work|session|conversation|discussion)\b`,
		`\ba\s+while\s+ago\b`, `리콜해`, `^\s*리콜\s*$`, `이전\s*작업`, `메모리에서\s*찾아`,
	}
	for _, p := range patterns {
		if recallHookRE(p).MatchString(recallHookASCII(prompt)) {
			return true
		}
	}
	return false
}

// Length and slice use JS code units, preserving a cut lone surrogate as WTF-8.
func recallHookUnits(s string) []uint16 {
	u := []uint16{}
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		i += n
		if r > 0xffff {
			u = append(u, utf16.Encode([]rune{r})...)
		} else {
			u = append(u, uint16(r))
		}
	}
	return u
}
func recallHookString(u []uint16) string {
	var b strings.Builder
	for i := 0; i < len(u); i++ {
		r := rune(u[i])
		if r >= 0xd800 && r <= 0xdbff && i+1 < len(u) && u[i+1] >= 0xdc00 && u[i+1] <= 0xdfff {
			b.WriteRune(utf16.DecodeRune(r, rune(u[i+1])))
			i++
			continue
		}
		if pyjson.IsSurrogate(r) {
			b.Write([]byte{0xe0 | byte(r>>12), 0x80 | byte(r>>6&63), 0x80 | byte(r&63)})
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// A lone surrogate is an uncased boundary, but its identity must survive lowering.
func recallHookLower(s string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		if pyjson.IsSurrogate(r) {
			b.WriteString(Lower(s[start:i]))
			b.WriteString(s[i : i+n])
			start = i + n
		}
		i += n
	}
	b.WriteString(Lower(s[start:]))
	return b.String()
}
func recallHookTargetStop(key string) bool {
	switch key {
	case "그때", "지난번", "지난", "저번", "예전", "세션", "작업", "기억", "뭐였지", "last", "time", "session", "previous", "previously", "remember":
		return true
	}
	return false
}
func ExtractRecallTargets(prompt string, caps ...int) []string {
	out, seen := []string{}, map[string]bool{}
	push := func(raw string) {
		term := text.Trim(raw)
		n := len(recallHookUnits(term))
		key := recallHookLower(term)
		if n < 2 || n > 60 || seen[key] || recallHookTargetStop(key) {
			return
		}
		seen[key] = true
		out = append(out, term)
	}
	for _, p := range []string{`\b\d+\.\d+(?:\.\d+)?\b`, `\b[\w.-]+\.(?:ts|tsx|js|mjs|cjs|json|md|toml|py|rs)\b`, `\b(?:[A-Z]{2,}(?:-[A-Z0-9]+)+|ERR_[A-Z0-9_]+)\b`, `\b[A-Z][a-zA-Z]*[A-Z][A-Za-z0-9]*\b`} {
		for _, match := range recallHookRE(p).FindAllString(prompt, -1) {
			push(match)
		}
	}
	// Replay the greedy quoted regex on code units, including its mixed delimiters.
	u := recallHookUnits(prompt)
	for i := 0; i < len(u); i++ {
		if u[i] != '"' && u[i] != '\'' && u[i] != '`' {
			continue
		}
		end := -1
		for j := i + 1; j < len(u) && j <= i+61; j++ {
			if j >= i+4 && (u[j] == '"' || u[j] == '\'' || u[j] == '`') {
				end = j
			}
			if u[j] == '"' || u[j] == '\'' || u[j] == '\n' {
				break
			}
		}
		if end >= 0 {
			push(recallHookString(u[i+1 : end]))
			i = end
		}
	}
	cap := 4
	if len(caps) > 0 {
		cap = caps[0]
	}
	if cap < 0 {
		cap = max(0, len(out)+cap)
	}
	return out[:min(len(out), cap)]
}
func recallHookDirective(inv string, targets []string) string {
	rows := []string{"[crw-recall] The prompt references past work. Before asking the user to re-explain,", "search prior sessions (read-only):",
		"  " + inv + ` recall chat search "<distinctive terms>" --days 0   # full-history FTS over ~/.codex`,
		"  " + inv + ` recall memory search "<topic>"                      # durable per-thread summaries`,
		"Add --context 2 to read around a hit, --cwd <repo> to scope. Details: $crw-recall."}
	if len(targets) > 0 {
		rows = append(rows, "Suggested recall terms: "+strings.Join(targets, " ")+" (search not run by this hook).")
	}
	return strings.Join(rows, "\n")
}
func recallHookQuote(s string) string {
	return pyjson.Dumps(s, pyjson.Options{Compact: true, Unicode: true})
}
func recallHookContextOutput(event, ctx string) string {
	norm := text.Trim(strings.ReplaceAll(strings.ReplaceAll(ctx, "\r\n", "\n"), "\r", "\n"))
	if norm == "" {
		return ""
	}
	if u := recallHookUnits(norm); len(u) > recallHookMaxContext {
		norm = strings.TrimRight(recallHookString(u[:recallHookMaxContext-64]), " \t\r\n") + "\n\n[truncated]"
	}
	return `{"hookSpecificOutput":{"hookEventName":` + recallHookQuote(event) + `,"additionalContext":` + recallHookQuote(norm) + "}}\n"
}
func HandleUserPromptSubmit(p UserPromptSubmitPayload, invocation string) string {
	prompt, _ := p.Prompt.(string)
	event, _ := p.HookEventName.(string)
	if event != "UserPromptSubmit" || !DetectRecallIntent(prompt) {
		return ""
	}
	return recallHookContextOutput("UserPromptSubmit", recallHookDirective(invocation, ExtractRecallTargets(prompt)))
}
func recallHookMemoriesTableBody(s string) (string, bool) {
	rows := text.SplitLines(s)
	for i, row := range rows {
		if recallHookRE(`^[ \t]*\[memories\][ \t]*(?:#.*)?$`).MatchString(row) {
			rest := rows[i+1:]
			for j, line := range rest {
				if recallHookRE(`^[ \t]*\[`).MatchString(line) {
					rest = rest[:j]
					break
				}
			}
			return strings.Join(rest, "\n"), true
		}
	}
	return "", false
}
func DedicatedToolsEnabled(home string) bool {
	if home == "" {
		var err error
		home, err = codexHome()
		if err != nil {
			return false
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return false
	}
	body, ok := recallHookMemoriesTableBody(source.DecodeUTF8(data))
	// JS multiline anchors recognize CR and Unicode line separators as well as LF.
	body = strings.NewReplacer("\r", "\n", "\u2028", "\n", "\u2029", "\n").Replace(body)
	return ok && recallHookRE(`(?m)^[ \t]*dedicated_tools[ \t]*=[ \t]*true[ \t]*(?:#.*)?$`).MatchString(body)
}
func recallHookRecoveryLine(inv string, dedicated bool) string {
	line := "Recall: " + inv + ` recall chat search "<terms>" --days 0  |  ` + inv + ` recall memory search "<topic>"`
	if dedicated {
		line = `Recall: memories.search "<topic>" (native tool). Also: ` + inv + ` recall memory search "<topic>"`
	}
	u := recallHookUnits(line)
	return recallHookString(u[:min(len(u), recallHookRecoveryBudget)])
}
func recallHookSessionNotice(src, inv, status string, dedicated bool) string {
	rows := []string{"[crw-recall] Past-session recall is available (read-only). Before asking the user", `about prior work — unfamiliar terms, lost context, "그때/지난번/last time" — recover it.`}
	if src == "compact" {
		rows = []string{"[crw-recall] Context was just compacted. If any earlier detail is now missing,", "recover it from past sessions before asking the user to repeat themselves."}
	}
	if src == "resume" {
		rows = append(rows, "This session was resumed after a pause, so earlier turns may be missing here.")
	}
	rows = append(rows, recallHookRecoveryLine(inv, dedicated))
	line := "Details: $crw-recall."
	if status != "" {
		line = "Index: " + status + ". " + line
	}
	rows = append(rows, line)
	return strings.Join(rows, "\n")
}
func HandleSessionStart(status, cwd, src string, opts SessionStartOptions, deps RecallContextDeps) string {
	parts := []string{}
	if cwd != "" {
		budget := FullBudget()
		if src == "compact" {
			budget = CompactedBudget()
		}
		result := BuildCwdContextResult(cwd, deps, budget)
		switch result.Outcome {
		case CwdContextHits:
			parts = append(parts, result.Text)
			if deps.Rendered != nil {
				deps.Rendered(result.Refs)
			}
		case CwdContextUnavailable:
			parts = append(parts, "Recall unavailable for this project — the index could not be read. Run `crw recall chat index --status` to inspect it.")
		}
	}
	if opts.MemoryNotice != "" {
		parts = append(parts, opts.MemoryNotice)
	}
	dedicated := false
	if opts.DedicatedTools == nil {
		dedicated = DedicatedToolsEnabled(opts.Home)
	} else {
		dedicated = *opts.DedicatedTools
	}
	inv := deps.Invocation
	if inv == "" {
		inv = "crw"
	}
	parts = append(parts, recallHookSessionNotice(src, inv, status, dedicated))
	return recallHookContextOutput("SessionStart", strings.Join(parts, "\n\n"))
}
func HandlePostCompact(_ string) string { return "" }
func AssertLegalHookResult(result HookResult) error {
	if result.Code != 0 {
		return fmt.Errorf("hook exited %d", result.Code)
	}
	if strings.Contains(result.Stderr, "\x1b[") {
		return errors.New("hook wrote ANSI to stderr")
	}
	if result.Stdout == "" {
		return nil
	}
	if strings.Contains(result.Stdout, "\x1b[") {
		return errors.New("hook wrote ANSI to stdout")
	}
	v, err := pyjson.Loads(result.Stdout, pyjson.LoadOptions{Map: true, Surrogates: true, Deep: true})
	if err != nil {
		return err
	}
	if _, ok := v.(map[string]any); !ok {
		return errors.New("hook stdout is not a JSON object")
	}
	return nil
}

// The callback belongs to the ingress: it supplies status/notice from their CLI owner.
type RecallHookSessionStart func(home, indexPath, cwd, source string, deps RecallContextDeps) string

func RunHook(ctx context.Context, event string, in io.Reader, out io.Writer, env host.LookupEnv, cwd string, sessionStart RecallHookSessionStart) int {
	done := make(chan int, 1)
	go func() { done <- recallHookRun(ctx, event, in, out, env, cwd, sessionStart) }()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
		return harness.Interrupted
	}
}
func recallHookRun(ctx context.Context, event string, in io.Reader, out io.Writer, env host.LookupEnv, cwd string, sessionStart RecallHookSessionStart) (code int) {
	deadline := recallHookDeadline(ctx, time.Now())
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	input := harness.ReadInput(in)
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	if input.Failed() {
		return 0
	}
	if env == nil {
		env = os.LookupEnv
	}
	raw := input.Raw
	harness.RecordInvocation(raw, "recall", event, env)
	if event != "user-prompt-submit" && text.Trim(raw) == "" {
		raw = "{}"
	}
	v, err := pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil || v == nil {
		return 0
	}
	p, _ := v.(map[string]any)
	var answer string
	var deps RecallContextDeps
	var rendered []string
	switch event {
	case "user-prompt-submit":
		inv, err := host.Invocation(env)
		if err != nil {
			inv = "crw"
		}
		answer = HandleUserPromptSubmit(UserPromptSubmitPayload{HookEventName: p["hook_event_name"], Prompt: p["prompt"]}, inv)
	case "session-start":
		if sessionStart == nil {
			return 0
		}
		invalidCwd := false
		if value := p["cwd"]; value != nil {
			cwd = ""
			switch value := value.(type) {
			case string:
				cwd = value
			case bool:
				invalidCwd = value
			case json.Number:
				n, _ := value.Float64()
				invalidCwd = n != 0
			default:
				invalidCwd = true
			}
		}
		home, err := hookContextHome(env)
		if err != nil {
			return 0
		}
		path, err := indexPath(env)
		if err != nil {
			return 0
		}
		src, _ := p["source"].(string)
		deps = DefaultRecallDeps(env)
		// Every wait of the history store, while the entries are chosen and when they are counted, ends
		// with the hook's own time: a locked index cannot hold the hook past its timeout.
		deps.OpenHitCounts = func() (HitCountStore, error) { return hookContextOpenSidecarHitCountsUntil(env, deadline), nil }
		deps.Rendered = func(refs []string) { rendered = refs }
		if invalidCwd {
			// The oracle catches basename's type error before any context reader runs.
			cwd = "invalid-cwd"
			deps.ListCwdSessions = func(string, int) ([]CwdSession, error) { return nil, errors.New("cwd is not a string") }
		}
		answer = sessionStart(home, path, cwd, src, deps)
	case "post-compact":
		answer = HandlePostCompact(cwd)
	}
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	if answer != "" {
		// The entries this answer carries are counted once it has been written. A write that failed
		// shows nothing and counts nothing; one that succeeded is not proof the model read it, which
		// the next start's ordinary repeat penalty already accepts.
		if _, err := io.WriteString(out, answer); err == nil && len(rendered) > 0 {
			recallHookCountHits(deps, rendered, deadline)
		}
	}
	return 0
}

// recallHookCountHits raises the history of the rendered refs in one transaction, retrying a failed
// attempt under the same event identity so that an attempt whose outcome is unknown cannot count twice.
// A history that cannot be written changes nothing about the answer, which is already out.
//
// Counting is best effort inside two bounds: recallHookCountBudget from now, and hookDeadline, the end of
// the hook's own time as the hook started it (zero: none). No attempt, the first included, starts once
// the bound is passed, and a store that can limit its waits (hitCountLimiter) is held to the same bound,
// so a write lock held by another process cannot carry the hook past its timeout.
func recallHookCountHits(deps RecallContextDeps, refs []string, hookDeadline time.Time) {
	if deps.OpenHitCounts == nil {
		return
	}
	until := time.Now().Add(recallHookCountBudget)
	if !hookDeadline.IsZero() && hookDeadline.Before(until) {
		until = hookDeadline
	}
	if !time.Now().Before(until) {
		return
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return
	}
	event := hex.EncodeToString(id[:])
	store, err := deps.OpenHitCounts()
	if err != nil || store == nil {
		return
	}
	defer func() { _ = store.Close() }()
	if limiter, ok := store.(hitCountLimiter); ok {
		limiter.LimitTo(until)
	}
	for attempt := 0; attempt < 3 && time.Now().Before(until); attempt++ {
		if store.Bump(event, refs) == nil {
			return
		}
	}
}

// hitCountLimiter is implemented by a store whose waits can be held to a point in time.
type hitCountLimiter interface{ LimitTo(until time.Time) }

// recallHookCountBudget bounds the time the hook spends counting its rendered entries after the answer.
var recallHookCountBudget = 2 * time.Second

// recallHookTimeout is the timeout of the hook declarations in plugins/crw/wiring/hooks that run this
// code (session-start and post-compact, 10 s); recallHookReserve is what is left to the process to exit.
var (
	recallHookTimeout = 10 * time.Second
	recallHookReserve = 500 * time.Millisecond
)

// recallHookDeadline is the last moment the hook may still wait for anything: the host's timeout counted
// from the start of the hook, or the context's own deadline when that is earlier, less the reserve.
func recallHookDeadline(ctx context.Context, started time.Time) time.Time {
	deadline := started.Add(recallHookTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline.Add(-recallHookReserve)
}
