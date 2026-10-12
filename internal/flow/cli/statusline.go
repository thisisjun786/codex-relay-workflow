package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// StatusLine is the line the Stop hook prints at the end of a turn of the thread that owns runs that are not over: one line, the
// most recent run first, "(+N more)" for the others. It is empty for any other thread. Progress is not mirrored into the owner's
// turns (a plan item needs a turn to carry it, and a turn for every step would cost a model turn each); while the thread is idle,
// `crw flow watch`, the GUI Runs page and Linear show it.
func StatusLine(runs []RunInfo, thread string) string {
	if thread == "" {
		return ""
	}
	var mine []RunInfo
	for _, r := range runs {
		if r.Owner == thread && !Over(r.State) {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		return ""
	}
	r := mine[len(mine)-1]
	line := fmt.Sprintf("crw flow %s %s", r.RunID, r.State)
	if r.Phase != "" {
		line += " · " + r.Phase
	}
	line += fmt.Sprintf(" · %d calls", r.Calls)
	if r.Unsettled > 0 {
		line += fmt.Sprintf(" · %d open", r.Unsettled)
	}
	if r.Holds > 0 {
		line += fmt.Sprintf(" · %d recovery_hold", r.Holds)
	}
	if r.Waiting > 0 {
		line += fmt.Sprintf(" · %d waiting for you", r.Waiting)
	}
	line += fmt.Sprintf(" · %s tokens", compactCount(r.Tokens))
	if len(mine) > 1 {
		line += fmt.Sprintf(" (+%d more)", len(mine)-1)
	}
	return line
}

func compactCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// statusLine is `crw flow status-line --thread T`: the line, or nothing.
func (c *command) statusLine(args []string) int {
	fs, cm := c.flags()
	thread := fs.String("thread", "", "the thread whose runs are shown (default: CODEX_THREAD_ID)")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 0 {
		return c.usage("takes no positional argument")
	}
	if *thread == "" {
		*thread = c.getenv("CODEX_THREAD_ID")
	}
	line := c.lineFor(*thread, cm)
	if line != "" {
		fmt.Fprintln(c.stdout, line)
	}
	return 0
}

func (c *command) lineFor(thread string, cm *common) string {
	if thread == "" {
		return ""
	}
	r, err := c.reader(cm)
	if err != nil {
		return ""
	}
	defer r.Close()
	infos, err := r.Infos(c.ctx)
	if err != nil {
		return ""
	}
	return StatusLine(infos, thread)
}

// HookInput is the part of a Stop hook payload the status line reads.
type HookInput struct {
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id"`
}

// RunStopHook is the leg of the Stop hook: the status line of the thread whose turn ended, as the hook's answer
// ({"systemMessage": line}), and nothing for a thread that owns no run, an unreadable payload or a store that cannot be read. The
// hook never blocks a turn and never fails it: it exits 0 whatever happens.
func RunStopHook(ctx context.Context, in io.Reader, out io.Writer, d *Deps, state, socket string, bound time.Duration) int {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	raw, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return 0
	}
	var payload HookInput
	if json.Unmarshal(raw, &payload) != nil {
		return 0
	}
	thread := payload.SessionID
	if thread == "" {
		thread = payload.ThreadID
	}
	c := &command{Deps: d, ctx: ctx, stdout: out, stderr: io.Discard, name: "status-line"}
	line := c.lineFor(thread, &common{state: state, socket: socket})
	if line == "" {
		return 0
	}
	b, _ := json.Marshal(map[string]string{"systemMessage": strings.TrimSpace(line)})
	fmt.Fprintf(out, "%s\n", b)
	return 0
}
