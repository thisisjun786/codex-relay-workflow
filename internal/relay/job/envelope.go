package job

import (
	"io"
	"os"
	"syscall"
)

// These are bg-wake's raw envelopes, not PABCD's normalized/truncated context.
// The store serializer keeps JSON.stringify's key order and string escaping.
func contextEnvelope(event, text string) string {
	b, _ := object(Event{{"hookSpecificOutput", Event{{"hookEventName", event}, {"additionalContext", text}}}}, 0)
	return string(b)
}

func blockEnvelope(reason string) string {
	b, _ := object(Event{{"decision", "block"}, {"reason", reason}}, 0)
	return string(b)
}

// os.File.Write on fd 1/2 turns EPIPE into a fatal runtime SIGPIPE. A direct
// syscall keeps it an ordinary error, as Node's runHook stdout catch requires,
// without changing the process's signal policy or any other component's output.
func writeHookOutput(out io.Writer, text string) {
	if f, ok := out.(*os.File); ok && f.Fd() <= 2 {
		body := []byte(text)
		for len(body) > 0 {
			n, err := syscall.Write(int(f.Fd()), body)
			if n > 0 {
				body = body[n:]
			}
			if err == syscall.EINTR {
				continue
			}
			if err != nil || n == 0 {
				return
			}
		}
		return
	}
	_, _ = io.WriteString(out, text)
}
