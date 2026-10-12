package hook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func ForwardChild(ctx context.Context, state, program string, argv []string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	address, release, err := ControlAddress(filepath.Join(state, "control.sock"))
	if err != nil {
		return nil, dispatch.Host("relay daemon unavailable; child report was not submitted: " + err.Error())
	}
	defer release()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return nil, dispatch.Host("relay daemon unavailable; child report was not submitted: " + err.Error())
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, dispatch.Host(err.Error())
	}
	if err := authenticatePeer(ctx, conn, filepath.Join(state, "control.sock")); err != nil {
		return nil, dispatch.Host(err.Error())
	}
	request := Object{{Key: "protocol", Value: int64(1)}, {Key: "method", Value: "child-command"}, {Key: "program", Value: program}, {Key: "argv", Value: argv}}
	if _, err := io.WriteString(conn, pyjson.Dumps(request, pyjson.Options{Compact: true})+"\n"); err != nil {
		return nil, dispatch.Host("relay daemon report outcome unknown; retry the same receipt identity: " + err.Error())
	}
	response, err := readFrame(conn)
	if err != nil {
		return nil, dispatch.Host("relay daemon report outcome unknown; retry the same receipt identity: " + err.Error())
	}
	code, valid := response.Get("exitCode").(int64)
	result, object := response.Get("result").(Object)
	if !valid || !object || code < 0 || code > 255 {
		return nil, dispatch.Host("relay daemon did not accept the child-command protocol; no local store fallback")
	}
	return nil, &dispatch.PayloadExit{Payload: result, Code: int(code)}
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
		var valid bool
		if argv[index], valid = value.(string); !valid {
			return answerHost(conn, "child arguments must be strings")
		}
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
	selected, err := store.ResolveStateDir(text(root.Values, "state"), text(root.Values, "socket"))
	if err != nil || filepath.Clean(selected.Path) != filepath.Clean(state) {
		return answerHost(conn, "child command may only write the daemon's own store")
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
	code := dispatch.ExecuteOwnedChild(ctx, pyjson.Text(request.Get("program")), argv, &stdout, &stderr)
	result, err := decodeObject(stdout.Bytes())
	if err != nil {
		return answerHost(conn, "child command produced no JSON answer: "+stderr.String())
	}
	if wake != nil && code == 0 {
		thread := text(parsed.Values, "turn-thread")
		if thread == "" {
			thread = text(parsed.Values, "session")
		}
		if thread != "" {
			wake(thread)
		}
	}
	_, err = io.WriteString(conn, pyjson.Dumps(Object{{Key: "exitCode", Value: int64(code)}, {Key: "result", Value: result}}, pyjson.Options{Compact: true})+"\n")
	return err
}
