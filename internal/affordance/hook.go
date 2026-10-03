package affordance

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// object preserves JSON.parse's surrogate strings and unused out-of-range numbers.
func object(raw string) map[string]any {
	v, err := pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil {
		return nil
	}
	o, _ := v.(map[string]any)
	return o
}

func envelope(event string, lines []string) string {
	return pyjson.Dumps(pyjson.Object{{Key: "hookSpecificOutput", Value: pyjson.Object{
		{Key: "hookEventName", Value: event}, {Key: "additionalContext", Value: strings.Join(lines, "\n\n")},
	}}}, pyjson.Options{Compact: true, Unicode: true}) + "\n"
}

func rootStamp(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// sessionBytes counts JavaScript UTF-16 units and supplies Node's UTF-8 encoding:
// each lone surrogate becomes one replacement character when hashing, not three.
func sessionBytes(s string) (string, int) {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		r, size := pyjson.CodePoint(s, i)
		i += size
		n++
		if r > 0xffff {
			n++
		}
		if pyjson.IsSurrogate(r) {
			r = 0xfffd
		}
		b.WriteRune(r)
	}
	return b.String(), n
}

// RecoveryPath validates a root event before creating anything. The workspace
// itself is canonicalized; .crw and its recovery directory may not be symlinks.
// These are the oracle's pathname checks, not protection against concurrent swaps.
func RecoveryPath(raw, event string, create bool) string {
	p := object(raw)
	cwd, _ := p["cwd"].(string)
	sid, _ := p["session_id"].(string)
	encoded, units := sessionBytes(sid)
	if p["hook_event_name"] != event || !filepath.IsAbs(cwd) || text.Trim(sid) == "" || units > 256 ||
		!rootStamp(p["agent_id"]) || !rootStamp(p["agent_type"]) {
		return ""
	}
	ws, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return ""
	}
	state := filepath.Join(ws, crwdir.DirName)
	dir := filepath.Join(state, "affordance-recovery")
	for _, path := range []string{state, dir} {
		st, err := os.Lstat(path)
		if err != nil {
			if !create || !os.IsNotExist(err) {
				return ""
			}
			if path == state {
				_, err = crwdir.EnsureDir(ws)
			} else {
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				return ""
			}
			st, err = os.Lstat(path)
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return ""
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%x.pending", sha256.Sum256([]byte(encoded))))
}

// RunPostCompactAffordance coalesces compactions by exclusive creation. No
// PostCompact envelope is supported; failures lose only the advisory hint.
func RunPostCompactAffordance(raw string) string {
	if path := RecoveryPath(raw, "PostCompact", true); path != "" {
		if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			_, _ = f.WriteString("pending\n")
			_ = f.Close()
		}
	}
	return ""
}

// RunUserPromptAffordance consumes only a regular marker. Concurrent prompts
// have one unlink winner, and a child cannot consume its parent's queued hint.
func RunUserPromptAffordance(raw string, env host.LookupEnv) string {
	path := RecoveryPath(raw, "UserPromptSubmit", false)
	if path == "" {
		return ""
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || os.Remove(path) != nil {
		return ""
	}
	return envelope("UserPromptSubmit", []string{RenderBackgroundTerminalAffordance(), RenderLoopAffordance(env),
		RenderStackedPrAffordance(), RenderQuestionAffordance()})
}

// RunMapAffordanceSessionStart is read-only and keeps unconditional pointers on
// malformed stdin or a failed walk. Only this handler trims before JSON.parse.
func RunMapAffordanceSessionStart(raw, fallbackCwd string, env host.LookupEnv) string {
	cwd, sid := fallbackCwd, ""
	if p := object(text.Trim(raw)); p != nil {
		if s, ok := p["cwd"].(string); ok && s != "" {
			cwd = s
		}
		sid, _ = p["session_id"].(string)
	}
	lines := []string{}
	if sid != "" {
		lines = append(lines, RenderSessionBinding(sid, env))
	}
	if count := CountSourceFiles(cwd); count >= MapAffordanceMinFiles {
		lines = append(lines, RenderMapAffordance(count, env))
	}
	lines = append(lines, RenderSkillSearchAffordance(env), RenderKwriteAffordance(), RenderLoopAffordance(env),
		RenderStackedPrAffordance(), RenderBackgroundTerminalAffordance(), RenderQuestionAffordance())
	if inv := invocation(env); inv != "crw" {
		lines = append(lines, "[crw] `crw` is not on PATH here; wherever docs say `crw`, run: "+inv)
	}
	return envelope("SessionStart", lines)
}

// RunHook ports cxc-ops/cli.ts:19-25,116-133: unlike PABCD and bg-wake, stdin
// is unbounded. A read error means empty stdin, then cxc-ops is observed and the
// handler runs. Cancellation prevents any late-read observation or marker write.
func RunHook(ctx context.Context, event string, in io.Reader, out io.Writer, env host.LookupEnv, cwd string) int {
	done := make(chan int, 1)
	go func() { done <- runHook(ctx, event, in, out, env, cwd) }()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
		return harness.Interrupted
	}
}

func runHook(ctx context.Context, event string, in io.Reader, out io.Writer, env host.LookupEnv, cwd string) int {
	data, err := io.ReadAll(in)
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	raw := ""
	if err == nil {
		raw = strings.ToValidUTF8(string(data), "\ufffd")
	}
	harness.RecordInvocation(raw, "cxc-ops", event, env)
	var answer string
	switch event {
	case "session-start":
		answer = RunMapAffordanceSessionStart(raw, cwd, env)
	case "post-compact":
		answer = RunPostCompactAffordance(raw)
	case "user-prompt-submit":
		answer = RunUserPromptAffordance(raw, env)
	}
	if answer != "" {
		_, _ = io.WriteString(out, answer)
	}
	return 0
}
