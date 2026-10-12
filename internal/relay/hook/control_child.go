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

// callerPaths are the values a child names relative to its own working directory or home: the child makes each absolute
// where it runs, and the daemon refuses a request that still carries a relative one (CRW-1286).
var callerPaths = map[string]func(string) (string, error){"state": store.AbsoluteExpanded, "socket": store.Absolute,
	"workspace": store.AbsoluteExpanded, "marker-root": store.AbsoluteExpanded, "independent-review": store.Absolute}

// childArgv is the child's line with every caller selection resolved in the child: its path values absolute, the state
// directory it selected (flag, environment or discovery) named by --state, an intent command's marker root by --marker-root.
func childArgv(state string, argv []string) ([]string, error) {
	absolute := func(key, value string) (string, error) {
		if resolve := callerPaths[key]; resolve != nil {
			return resolve(value)
		}
		return value, nil
	}
	root := argparse.Parse("", argv)
	name := root.Remaining[0]
	head, err := argparse.Rewrite("", argv[:len(argv)-len(root.Remaining)], absolute)
	if err != nil {
		return nil, err
	}
	line, err := argparse.Rewrite(name, root.Remaining[1:], absolute)
	if err != nil {
		return nil, err
	}
	if n := len(head); n > 0 && head[n-1] == "--" {
		head = head[:n-1]
	}
	out := append(append(head, "--state", state, name), line...)
	if name == "emit" {
		return out, nil
	}
	selection, err := delivery.ResolveMarkerRoot(lastValue(argparse.Parse(name, line).Values, "marker-root"))
	return append(out, "--marker-root", selection.Path), err
}

func lastValue(values map[string][]string, key string) string {
	if entries := values[key]; len(entries) > 0 {
		return entries[len(entries)-1]
	}
	return ""
}

func ForwardChild(ctx context.Context, state, program string, argv []string) (any, error) {
	argv, err := childArgv(state, argv)
	if err != nil {
		return nil, err
	}
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
	if detail, refused := response.Get("detail").(string); refused && response.Get("error") != nil {
		return nil, dispatch.Host("relay daemon refused the child report: " + detail)
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
	relative := ""
	check := func(key, value string) (string, error) {
		if callerPaths[key] != nil && !filepath.IsAbs(value) && relative == "" {
			relative = "--" + key
		}
		return value, nil
	}
	_, _ = argparse.Rewrite("", argv, check)
	_, _ = argparse.Rewrite(name, root.Remaining[1:], check)
	if relative != "" {
		return answerHost(conn, "the child request carries an unresolved caller-relative "+relative+"; the child resolves it where it runs, the daemon never does")
	}
	if selected := lastValue(root.Values, "state"); selected == "" || filepath.Clean(selected) != filepath.Clean(state) {
		return answerHost(conn, "child command may only write the daemon's own store")
	}
	if name != "emit" {
		if lastValue(parsed.Values, "marker-root") == "" {
			return answerHost(conn, "the child request names no --marker-root; the child resolves its marker root where it runs")
		}
		params := Object{{Key: "markerRoot", Value: lastValue(parsed.Values, "marker-root")}, {Key: "dbPath", Value: filepath.Join(state, "relay.sqlite3")}}
		marker, _, refused, err := ownerPaths(state, params)
		if err != nil || refused != "" {
			return answerHost(conn, fmt.Sprintf("child marker selection refused: %s %v", refused, err))
		}
		directory, err := delivery.AssignmentDir(marker, lastValue(parsed.Values, "workspace"), lastValue(parsed.Values, "assignment"))
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
		thread := lastValue(parsed.Values, "turn-thread")
		if thread == "" {
			thread = lastValue(parsed.Values, "session")
		}
		if thread != "" {
			wake(thread)
		}
	}
	_, err = io.WriteString(conn, pyjson.Dumps(Object{{Key: "exitCode", Value: int64(code)}, {Key: "result", Value: result}}, pyjson.Options{Compact: true})+"\n")
	return err
}
