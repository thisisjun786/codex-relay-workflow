// hook.go ports CXC v0.2.40 bg-wake/src/hook.ts and cli.ts:38-79. The registry
// remains local; these completions are neither relay receipts nor acknowledgements.
package job

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

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

func CompletionText(recs []BgRecord) string {
	lines := []string{"[crw bg] 백그라운드 작업 " + stringNumber(len(recs)) + "건이 끝났습니다."}
	for _, rec := range recs {
		lines = append(lines, DescribeRecord(rec))
	}
	// map(...).join on an empty batch still contributes an empty body line.
	if len(recs) == 0 {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, "출력은 `crw relay job get <id> --tail 40`으로 봅니다. 전체 목록은 `crw relay job list`.\n결과를 확인하고 필요한 후속 작업을 이어가세요."), "\n")
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

// HandleStop stamps before returning its block. A second Stop cannot wake the
// same stamped records, even if the caller lost the first output.
func HandleStop(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time) string {
	return completion(p, cwd, getenv, clock, "Stop")
}

// HandleUserPromptSubmit injects context only: a decision would reject the prompt.
func HandleUserPromptSubmit(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time) string {
	return completion(p, cwd, getenv, clock, "UserPromptSubmit")
}

func completion(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time, event string) string {
	return silent(func() string {
		ws := PayloadCwd(p, cwd)
		if WakeSuppressed(ws, getenv) {
			return ""
		}
		due, err := SelectWake(ws, PayloadSessionID(p, getenv), WakeBatchLimit, clock)
		if err != nil || len(due) == 0 {
			return ""
		}
		body := CompletionText(due)
		if text.Trim(body) == "" {
			return ""
		}
		MarkDelivered(ws, due, clock)
		if event == "Stop" {
			return blockEnvelope(body)
		}
		return contextEnvelope(event, body)
	})
}

// DrainNow deliberately ignores both wake switches: explicit collection still works.
func DrainNow(ws string, sessionID *string, clock func() time.Time) string {
	return silent(func() string {
		due, err := SelectWake(ws, sessionID, WakeBatchLimit, clock)
		if err != nil || len(due) == 0 {
			return ""
		}
		body := CompletionText(due)
		MarkDelivered(ws, due, clock)
		return body
	})
}

// HandleSessionStart adopts even while off. Adoption is not delivery; Stop or the
// next prompt stamps the completion. Only the first five adopted jobs are described.
func HandleSessionStart(p HookPayload, cwd string, getenv func(string) string, clock func() time.Time) string {
	return silent(func() string {
		ws, sid := PayloadCwd(p, cwd), PayloadSessionID(p, getenv)
		adopted, err := AdoptOrphans(ws, sid, clock)
		if err != nil || WakeSuppressed(ws, getenv) {
			return ""
		}
		has, err := HasAnyTask(ws, sid, clock)
		if err != nil || !has {
			return ""
		}
		lines := []string{}
		if len(adopted) > 0 {
			lines = append(lines, "[crw bg] 이전 세션에서 끝난 백그라운드 작업 "+stringNumber(len(adopted))+"건이 아직 전달되지 않았습니다.")
			for _, r := range adopted[:min(5, len(adopted))] {
				lines = append(lines, DescribeRecord(r))
			}
		}
		return contextEnvelope("SessionStart", strings.Join(append(lines, Affordance), "\n"))
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
	raw, overflow := harness.ReadStdin(in)
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	if overflow || hookUnits(raw) > MaxHookUnits {
		raw = ""
	}
	harness.RecordInvocation(raw, "bg-wake", event, env)
	p, out := parseHookPayload(raw), ""
	getenv := func(k string) string { v, _ := env(k); return v }
	switch event {
	case "stop":
		out = HandleStop(p, cwd, getenv, clock)
	case "user-prompt-submit":
		out = HandleUserPromptSubmit(p, cwd, getenv, clock)
	case "session-start":
		out = HandleSessionStart(p, cwd, getenv, clock)
	}
	if out != "" {
		writeHookOutput(stdout, out+"\n")
	}
	return 0
}
