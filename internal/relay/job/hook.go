// hook.go ports CXC v0.2.40 bg-wake/src/hook.ts and cli.ts:38-79. The registry
// remains local; these completions are neither relay receipts nor acknowledgements.
package job

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// HookPayload keeps the oracle's optional unknown-valued fields. Source and
// StopHookActive are declared but unused by the three handlers, as upstream.
type HookPayload struct {
	SessionID, Cwd, Source, StopHookActive any
}

const (
	// MaxHookUnits is cli.ts' limit: JavaScript string length, not UTF-8 bytes.
	MaxHookUnits = 1 << 20
	Affordance   = "[crw bg] 백그라운드 작업은 `crw relay job list`로 보고 `crw relay job get <id> --tail 40`으로 출력을 읽습니다."
)

func PayloadSessionID(p HookPayload, getenv func(string) string) *string {
	if s, ok := p.SessionID.(string); ok && s != "" {
		return &s
	}
	if s := text.Trim(getenv("CODEX_THREAD_ID")); s != "" {
		return &s
	}
	return nil
}

func PayloadCwd(p HookPayload, fallback string) string {
	if s, ok := p.Cwd.(string); ok && s != "" {
		return s
	}
	return fallback
}

// CompletionText is the text of a wake for recs: the jobs that fit the budget (fitWake), as a drain hands it over.
func CompletionText(recs []BgRecord) string {
	out, _ := fitWake(recs, completionBody, jsonSize)
	return out
}

// completionBody is the text of a wake that describes n jobs with lines.
func completionBody(n int, lines []string) string {
	lines = append([]string{"[crw bg] 백그라운드 작업 " + stringNumber(n) + "건이 끝났습니다."}, lines...)
	// map(...).join on an empty batch still contributes an empty body line.
	if n == 0 {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, "출력은 `crw relay job get <id> --tail 40`으로 봅니다. 전체 목록은 `crw relay job list`.\n결과를 확인하고 필요한 후속 작업을 이어가세요."), "\n")
}

// The byte budget of the text a wake, an adoption or a drain hands to the session (CRW-1095), measured as it is handed over: the
// serialized envelope, where JSON spells a control character in six bytes. A command longer than WakeCommandBytes and a note longer
// than WakeNoteBytes are cut at a rune boundary and the line points at `get`. A batch whose lines pass WakeLinesBytes, or whose text
// passes WakeTextBytes, is written again without commands and notes; a batch that still passes WakeTextBytes describes its first jobs
// that fit, and a wake leaves the others pending for the next one. The id, the status, the exit code, the duration and the get of
// every job described are always there, and the record keeps the whole command. One job always fits: a file name holds at most 255
// bytes, so its line, with the id twice and every byte escaped, stays near 3 KB.
const (
	WakeCommandBytes = 160
	WakeNoteBytes    = 120
	WakeLinesBytes   = 2048
	WakeTextBytes    = 4096
)

// fitWake is the text wrap builds from the lines of the first jobs of recs that fit the budget, and those jobs. wrap answers the text
// of n jobs and their lines as it is handed over, and size is what that text costs. A job whose command fits and that has no note
// gets DescribeRecord's line.
func fitWake(recs []BgRecord, wrap func(n int, lines []string) string, size func(string) int) (string, []BgRecord) {
	lines := make([]string, len(recs))
	for i, rec := range recs {
		lines[i] = briefRecord(rec, WakeCommandBytes, WakeNoteBytes)
	}
	if out := wrap(len(recs), lines); len(recs) == 0 || jsonSize(strings.Join(lines, "\n")) <= WakeLinesBytes && size(out) <= WakeTextBytes {
		return out, recs
	}
	for i, rec := range recs {
		lines[i] = briefRecord(rec, -1, -1)
	}
	for n := len(recs); ; n-- {
		if out := wrap(n, lines[:n]); n == 1 || size(out) <= WakeTextBytes {
			return out, recs[:n]
		}
	}
}

// jsonSize is the size of s as a JSON string, the way the store's serializer spells it.
func jsonSize(s string) int { return len(quote(s)) }

func envelopeSize(s string) int { return len(s) }

// briefRecord is DescribeRecord with the command cut to command bytes and the note, when there is one, to note bytes; a line that
// leaves anything out names the job's get. A negative command leaves the command and the note out.
func briefRecord(rec BgRecord, command, note int) string {
	full := strings.Join(rec.Command, " ")
	pointer := " (전체: crw relay job get " + rec.ID + ")"
	if command < 0 {
		return strings.TrimSuffix(DescribeRecord(rec), " — "+full) + pointer
	}
	shown, cut := clip(full, command)
	line := strings.TrimSuffix(DescribeRecord(rec), full) + shown
	if rec.Note != nil {
		n, noteCut := clip(*rec.Note, note)
		line += " [" + n + "]"
		cut = cut || noteCut
	}
	if cut {
		line += pointer
	}
	return line
}

// clip is s cut to at most n bytes at a rune boundary, with an ellipsis when anything was cut.
func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…", true
}

func stringNumber(n int) string { b, _ := value(n, 0); return string(b) }

func silent(fn func() string) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	return fn()
}

// HandleStop returns the block of the session's due completions and stamps them delivered: the caller has the text, which is the
// emission here. RunHook emits to its stdout first and stamps only after that write succeeded (CRW-1092).
func HandleStop(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time) string {
	return completion(p, cwd, getenv, clock, "Stop", acceptAll)
}

// HandleUserPromptSubmit injects context only: a decision would reject the prompt.
func HandleUserPromptSubmit(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time) string {
	return completion(p, cwd, getenv, clock, "UserPromptSubmit", acceptAll)
}

func acceptAll(string) error { return nil }

// completion selects, emits and stamps under the store lock (deliver): emit is the write of the envelope, and a completion whose
// envelope was not written stays pending. A store that is locked for longer than lockWait emits nothing.
func completion(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time, event string, emit func(string) error) string {
	return silent(func() string {
		ws := PayloadCwd(p, cwd)
		if WakeSuppressed(ws, getenv) {
			return ""
		}
		out, _ := deliver(ws, PayloadSessionID(p, getenv), clock, func(due []BgRecord) (string, []BgRecord) {
			return fitWake(due, func(n int, lines []string) string {
				body := completionBody(n, lines)
				if text.Trim(body) == "" {
					return ""
				}
				if event == "Stop" {
					return blockEnvelope(body)
				}
				return contextEnvelope(event, body)
			}, envelopeSize)
		}, emit)
		return out
	})
}

// HandleSessionStart adopts even while off. Adoption is not delivery; Stop or the
// next prompt stamps the completion. Only the first five adopted jobs are described.
// The adoption is written under the store lock over the records as they are then (CRW-1092).
func HandleSessionStart(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time) string {
	return silent(func() string {
		ws, sid := PayloadCwd(p, cwd), PayloadSessionID(p, getenv)
		// One reconciled snapshot serves the adoption and the has-task question, so each record is reconciled once in this event
		// (CRW-1095).
		unlock, err := lockStore(ws)
		if err != nil {
			return ""
		}
		defer unlock()
		recs, err := listRecords(ws, clock, true)
		if err != nil {
			return ""
		}
		adopted, err := adoptOrphans(ws, sid, clock, recs)
		if err != nil || WakeSuppressed(ws, getenv) {
			return ""
		}
		has := len(recs) > 0
		if sid != nil {
			has = len(adopted) > 0 || slices.ContainsFunc(recs, func(r BgRecord) bool { return ownedBy(r, *sid) })
		}
		if !has {
			return ""
		}
		out, _ := fitWake(adopted[:min(5, len(adopted))], func(_ int, described []string) string {
			lines := []string{}
			if len(adopted) > 0 {
				lines = append(lines, "[crw bg] 이전 세션에서 끝난 백그라운드 작업 "+stringNumber(len(adopted))+"건이 아직 전달되지 않았습니다.")
				lines = append(lines, described...)
			}
			return contextEnvelope("SessionStart", strings.Join(append(lines, Affordance), "\n"))
		}, envelopeSize)
		return out
	})
}

// parseHookPayload is JSON.parse-or-{} without event/canonical-session requirements.
// UseNumber admits 1e400 in unused fields; trailing documents and a BOM are invalid.
func parseHookPayload(raw string) HookPayload {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return HookPayload{}
	}
	if _, err := d.Token(); err != io.EOF {
		return HookPayload{}
	}
	o, _ := v.(map[string]any)
	return HookPayload{o["session_id"], o["cwd"], o["source"], o["stop_hook_active"]}
}

func hookUnits(raw string) int {
	n := 0
	for _, r := range raw {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// RunHook is cli.ts runHook: record bg-wake, parse even empty input, dispatch,
// write one newline when non-empty, and fail open on read/handler/stdout errors.
// Every accepted <=1Mi UTF-16 string fits harness.ReadStdin's 4MiB byte bound;
// either overflow is read as empty, never as PABCD's oversize block.
// Cancellation preserves the process's SIGINT result, with no late-input effects.
func RunHook(ctx context.Context, event string, in io.Reader, stdout io.Writer, env host.LookupEnv, cwd string, clock func() time.Time) int {
	done := make(chan int, 1)
	go func() { done <- runHook(ctx, event, in, stdout, env, cwd, clock) }()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
		return harness.Interrupted
	}
}

func runHook(ctx context.Context, event string, in io.Reader, stdout io.Writer, env host.LookupEnv, cwd string, clock func() time.Time) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	// harness.ReadStdin's bound and decoding, with the read error kept apart from empty input: input that failed to read or passed a
	// bound is not the empty payload, whose cwd and CODEX_THREAD_ID fallbacks could wake or adopt for the wrong session, and the hook
	// does nothing with it (CRW-1134).
	b, err := io.ReadAll(io.LimitReader(in, harness.MaxStdinBytes+1))
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	raw, unusable := decodeUTF8(b), err != nil || len(b) > harness.MaxStdinBytes
	if unusable || hookUnits(raw) > MaxHookUnits {
		raw, unusable = "", true
	}
	harness.RecordInvocation(raw, "bg-wake", event, env)
	if unusable {
		return 0
	}
	p, out := parseHookPayload(raw), ""
	getenv := func(k string) string { v, _ := env(k); return v }
	// Stop and UserPromptSubmit write their envelope from inside the delivery, so a completion is stamped only after its text reached
	// stdout; a failed write leaves it pending (CRW-1092).
	emit := func(text string) error { return writeHookOutput(stdout, text+"\n") }
	switch event {
	case "stop":
		completion(p, cwd, getenv, clock, "Stop", emit)
	case "user-prompt-submit":
		completion(p, cwd, getenv, clock, "UserPromptSubmit", emit)
	case "session-start":
		out = HandleSessionStart(p, cwd, getenv, clock)
	}
	if out != "" {
		_ = writeHookOutput(stdout, out+"\n")
	}
	return 0
}
