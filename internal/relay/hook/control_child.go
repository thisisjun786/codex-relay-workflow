package hook

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ForwardChild carries a child command to the relay daemon: argv as base64, so a byte that is not UTF-8 arrives as it was
// given, and the daemon's stdout and stderr back as raw bytes, so the child prints exactly what the daemon's command wrote.
func ForwardChild(ctx context.Context, state, program string, argv []string, stdout, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	address, release, err := ControlAddress(filepath.Join(state, "control.sock"))
	if err != nil {
		return 0, dispatch.Host("relay daemon unavailable; child report was not submitted: " + err.Error())
	}
	defer release()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return 0, dispatch.Host("relay daemon unavailable; child report was not submitted: " + err.Error())
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, dispatch.Host(err.Error())
	}
	if err := authenticatePeer(ctx, conn, filepath.Join(state, "control.sock")); err != nil {
		return 0, dispatch.Host(err.Error())
	}
	encoded := make([]any, len(argv))
	for index, argument := range argv {
		encoded[index] = base64.StdEncoding.EncodeToString([]byte(argument))
	}
	request := Object{{Key: "protocol", Value: int64(1)}, {Key: "method", Value: "child-command"}, {Key: "program", Value: program}, {Key: "argv", Value: encoded}}
	if _, err := io.WriteString(conn, pyjson.Dumps(request, pyjson.Options{Compact: true})+"\n"); err != nil {
		return 0, dispatch.Host("relay daemon report outcome unknown; retry the same receipt identity: " + err.Error())
	}
	response, err := readFrame(conn)
	if err != nil {
		return 0, dispatch.Host("relay daemon report outcome unknown; retry the same receipt identity: " + err.Error())
	}
	if detail, refused := response.Get("detail").(string); refused && response.Get("error") != nil {
		return 0, dispatch.Host("relay daemon refused the child report: " + detail)
	}
	code, valid := response.Get("exitCode").(int64)
	outText, isOut := response.Get("stdout").(string)
	errText, isErr := response.Get("stderr").(string)
	out, outErr := base64.StdEncoding.DecodeString(outText)
	errOut, errErr := base64.StdEncoding.DecodeString(errText)
	if !valid || !isOut || !isErr || outErr != nil || errErr != nil || code < 0 || code > 255 {
		return 0, dispatch.Host("relay daemon did not accept the child-command protocol; no local store fallback")
	}
	_, _ = stdout.Write(out)
	_, _ = stderr.Write(errOut)
	return int(code), nil
}

func handleChildCommand(ctx context.Context, conn net.Conn, state string, request Object, wake func(string)) error {
	if !store.HasBorrowedStore(ctx) {
		return answerHost(conn, "child reports require the relay daemon's owned store")
	}
	values, ok := request.Get("argv").([]any)
	if !ok {
		return answerHost(conn, "child argv must be an array")
	}
	argv := make([]string, len(values))
	for index, value := range values {
		text, isText := value.(string)
		raw, err := base64.StdEncoding.DecodeString(text)
		if !isText || err != nil {
			return answerHost(conn, "child arguments must be base64 strings")
		}
		argv[index] = string(raw)
	}
	root := argparse.Parse("", argv)
	if root.Help || root.Message != "" || len(root.Remaining) == 0 || !dispatch.ChildCommand(root.Remaining[0]) {
		return rejectControl(conn)
	}
	name := root.Remaining[0]
	parsed := argparse.Parse(name, root.Remaining[1:])
	if parsed.Help || parsed.Message != "" {
		return rejectControl(conn)
	}
	text := func(values map[string][]string, key string) string {
		entries := values[key]
		if len(entries) == 0 {
			return ""
		}
		return entries[len(entries)-1]
	}
	// CRW-1286: the daemon alone decides where a child report is recorded. The caller's --state and environment only
	// found this socket; the command runs on the daemon's own state (OwnedChildState drops the caller's --state),
	// and a path relative to the caller is refused, never interpreted in the daemon's directory.
	for _, option := range []struct {
		values map[string][]string
		key    string
	}{{root.Values, "socket"}, {parsed.Values, "workspace"}, {parsed.Values, "marker-root"}, {parsed.Values, "independent-review"}} {
		for _, value := range option.values[option.key] {
			if !filepath.IsAbs(value) {
				return answerHost(conn, "--"+option.key+" "+strconv.Quote(value)+" is caller-relative; the relay daemon never interprets a path relative to the child: name it absolute")
			}
		}
	}
	if name != "emit" {
		selection, err := delivery.ResolveMarkerRoot(text(parsed.Values, "marker-root"))
		if err != nil {
			return answerHost(conn, err.Error())
		}
		params := Object{{Key: "markerRoot", Value: selection.Path}, {Key: "dbPath", Value: filepath.Join(state, "relay.sqlite3")}}
		marker, _, refused, err := ownerPaths(state, params)
		if err != nil || refused != "" {
			return answerHost(conn, fmt.Sprintf("child marker selection refused: %s %v", refused, err))
		}
		directory, err := delivery.AssignmentDir(marker, text(parsed.Values, "workspace"), text(parsed.Values, "assignment"))
		if err != nil {
			return answerHost(conn, err.Error())
		}
		facts, _ := delivery.ReadAssignment(ctx, directory)
		intent, _ := facts.Get("intent").(Object)
		if target, ok := intent.Get("dbPath").(string); ok && filepath.Clean(target) != filepath.Join(state, "relay.sqlite3") {
			return answerHost(conn, "child intent may only name the daemon's own store")
		}
	}
	var stdout, stderr bytes.Buffer
	code := dispatch.ExecuteOwnedChild(dispatch.OwnedChildState(ctx, state), pyjson.Text(request.Get("program")), argv, &stdout, &stderr)
	if wake != nil && code == 0 {
		thread := text(parsed.Values, "turn-thread")
		if thread == "" {
			thread = text(parsed.Values, "session")
		}
		if thread != "" {
			wake(thread)
		}
	}
	_, err := io.WriteString(conn, pyjson.Dumps(Object{{Key: "exitCode", Value: int64(code)}, {Key: "stdout", Value: base64.StdEncoding.EncodeToString(stdout.Bytes())}, {Key: "stderr", Value: base64.StdEncoding.EncodeToString(stderr.Bytes())}}, pyjson.Options{Compact: true})+"\n")
	return err
}
