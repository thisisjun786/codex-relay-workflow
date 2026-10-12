package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
)

// The Runs page's API (CRW-1205). The runs are the flow runs of the relay store: GET /api/runs lists them, GET /api/runs/<id> is one
// run's tree (phase, then agent; the commits, reviews and tokens of its nodes; recovery_hold), and GET /api/runs/<id>/events is the
// run's journal as server-sent events, the same rows `crw flow watch` reads, with the row's seq as the event id: a reconnect that
// presents the last id (Last-Event-ID, which an EventSource does by itself, or ?after=) goes on from the next row and repeats none.
//
// The three writes are POSTs and go through the guard like every write of this server (loopback Host, JSON content type, the
// per-run token, a same-origin Origin): POST /api/runs/<id>/pause and /cancel signal the run's process, and POST /api/runs/<id>/answer
// applies a manager's answer record to a wait of the run. Each runs the command of `crw flow` in this process, so the screen and the
// command line judge an answer by the same rules and write the same rows.
//
// A store that cannot be read is reported with its reason, never as an empty list.

// runsSource is what the handlers read: the runs of the relay store.
type runsSource interface {
	Infos(ctx context.Context) ([]flowcli.RunInfo, error)
	Detail(ctx context.Context, run string) (flowcli.RunDetail, error)
	Tail(ctx context.Context, run string, after, limit int, wait time.Duration) (flowcli.Page, error)
	Close() error
}

// runsOpen opens the source; a test replaces it. The production value opens the relay store the server's own selection names,
// read-only.
var runsOpen = func(ctx context.Context) (runsSource, error) { return flowcli.OpenReader(ctx, "", "") }

// runsControl runs one command of `crw flow` in this process and returns its status and output; a test replaces it.
var runsControl = func(ctx context.Context, args []string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := flowcli.Run(ctx, args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The timing of the event stream: how long one read waits for a row, and how many rows it takes at once.
var (
	runsEventWait  = 10 * time.Second
	runsEventLimit = 100
)

const runsPrefix = "/api/runs/"

func init() {
	Register(http.MethodGet, "/api/runs", runsListHandler)
	Register(http.MethodGet, runsPrefix, runsGetHandler)
	Register(http.MethodPost, runsPrefix, runsPostHandler)
}

// runsListHandler answers GET /api/runs.
func runsListHandler(_ *Env, r *http.Request) (Response, error) {
	src, err := runsOpen(r.Context())
	if err != nil {
		return Response{Status: http.StatusOK, Body: map[string]any{"runs": []flowcli.RunInfo{}, "unknown": messageText(err.Error())}}, nil
	}
	defer src.Close()
	infos, err := src.Infos(r.Context())
	if err != nil {
		return Response{Status: http.StatusOK, Body: map[string]any{"runs": []flowcli.RunInfo{}, "unknown": messageText(err.Error())}}, nil
	}
	if infos == nil {
		infos = []flowcli.RunInfo{}
	}
	return Response{Status: http.StatusOK, Body: map[string]any{"runs": infos}}, nil
}

// runsPath splits /api/runs/<id>[/<verb>] into the run id and the verb.
func runsPath(path string) (id, verb string, ok bool) {
	rest := strings.TrimPrefix(path, runsPrefix)
	id, verb, _ = strings.Cut(rest, "/")
	if id == "" || strings.Contains(verb, "/") {
		return "", "", false
	}
	return id, verb, flowcliValidRun(id)
}

// flowcliValidRun is the id a path can carry, as the run directory of the store names it.
func flowcliValidRun(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// runsGetHandler answers GET /api/runs/<id> and GET /api/runs/<id>/events.
func runsGetHandler(_ *Env, r *http.Request) (Response, error) {
	id, verb, ok := runsPath(r.URL.Path)
	if !ok {
		return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
	}
	switch verb {
	case "":
		src, err := runsOpen(r.Context())
		if err != nil {
			return Response{Status: http.StatusServiceUnavailable, Body: map[string]any{"error": "unavailable", "reason": messageText(err.Error())}}, nil
		}
		defer src.Close()
		detail, err := src.Detail(r.Context(), id)
		switch {
		case errors.Is(err, flowcli.ErrNoRun):
			return Response{Status: http.StatusNotFound, Body: jsonError{Error: codeNotFound}}, nil
		case err != nil:
			return Response{Status: http.StatusServiceUnavailable, Body: map[string]any{"error": "unavailable", "reason": messageText(err.Error())}}, nil
		}
		return Response{Status: http.StatusOK, Body: detail}, nil
	case "events":
		after, ok := eventsCursor(r)
		if !ok {
			return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
		}
		return Response{Stream: func(w http.ResponseWriter, req *http.Request) { streamRunEvents(w, req, id, after) }}, nil
	}
	return Response{Status: http.StatusNotFound, Body: jsonError{Error: codeNotFound}}, nil
}

// eventsCursor is the row a stream goes on after: ?after=, else the Last-Event-ID an EventSource presents on a reconnect, else 0.
func eventsCursor(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n >= 0
}

// streamRunEvents writes the run's rows after the cursor as server-sent events and keeps writing as rows arrive, until the run is
// over (event end), the client goes, or the server shuts down. An `event: state` carries the run's state whenever it changes.
func streamRunEvents(w http.ResponseWriter, r *http.Request, id string, after int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, codeInternal)
		return
	}
	src, err := runsOpen(r.Context())
	if err != nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{"error": "unavailable", "reason": messageText(err.Error())})
		return
	}
	defer src.Close()
	// the first read decides whether the run exists, before the stream starts
	page, err := src.Tail(r.Context(), id, after, runsEventLimit, 0)
	switch {
	case errors.Is(err, flowcli.ErrNoRun):
		writeError(w, r, http.StatusNotFound, codeNotFound)
		return
	case err != nil:
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{"error": "unavailable", "reason": messageText(err.Error())})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	lastState := ""
	for {
		for _, row := range page.Rows {
			body, _ := json.Marshal(row)
			fmt.Fprintf(w, "id: %d\nevent: row\ndata: %s\n\n", row.Seq, body)
		}
		if page.State != lastState {
			lastState = page.State
			body, _ := json.Marshal(map[string]any{"state": page.State, "cursor": page.Cursor})
			fmt.Fprintf(w, "event: state\ndata: %s\n\n", body)
		}
		if len(page.Rows) == 0 {
			fmt.Fprint(w, ": keepalive\n\n")
		}
		flusher.Flush()
		after = page.Cursor
		if len(page.Rows) == 0 && !page.More && (flowcli.Over(page.State) || page.State == flowcli.StateDead || page.State == flowcli.StateHeld) {
			fmt.Fprintf(w, "event: end\ndata: {\"state\":%q,\"cursor\":%d}\n\n", page.State, after)
			flusher.Flush()
			return
		}
		wait := runsEventWait
		if page.More {
			wait = 0
		}
		page, err = src.Tail(r.Context(), id, after, runsEventLimit, wait)
		if err != nil {
			return
		}
	}
}

// runsPostHandler answers POST /api/runs/<id>/pause, /cancel and /answer.
func runsPostHandler(_ *Env, r *http.Request) (Response, error) {
	id, verb, ok := runsPath(r.URL.Path)
	if !ok {
		return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
	}
	switch verb {
	case "pause", "cancel":
		code, stdout, stderr := runsControl(r.Context(), []string{verb, id})
		return controlResponse(code, stdout, stderr), nil
	case "answer":
		var body struct {
			Decision string `json:"decision"`
			Node     string `json:"node"`
			Question string `json:"question"`
			Revision int    `json:"revision"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || body.Decision == "" || body.Question == "" || body.Revision < 1 {
			return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
		}
		code, stdout, stderr := runsControl(r.Context(), []string{"answer", "--json", "--thread", "gui", "--decision", body.Decision, "--run", id, "--node", body.Node, "--question", body.Question, "--revision", strconv.Itoa(body.Revision)})
		var reply json.RawMessage
		if json.Valid([]byte(stdout)) && strings.TrimSpace(stdout) != "" {
			reply = json.RawMessage(stdout)
		}
		resp := controlResponse(code, stdout, stderr)
		if reply != nil {
			resp.Body = map[string]any{"ok": code == 0, "reply": reply}
		}
		return resp, nil
	}
	return Response{Status: http.StatusNotFound, Body: jsonError{Error: codeNotFound}}, nil
}

// controlResponse is the answer to a control command: 200 when it did what was asked, 409 when it refused (exit 1: the run is over,
// has no process, a process holds it, the record does not hold), 400 for a command it could not use.
func controlResponse(code int, stdout, stderr string) Response {
	message := strings.TrimSpace(stdout)
	if message == "" || code != 0 {
		message = strings.TrimSpace(stderr)
	}
	body := map[string]any{"ok": code == 0, "message": messageText(message)}
	switch code {
	case 0:
		return Response{Status: http.StatusOK, Body: body}
	case 1:
		return Response{Status: http.StatusConflict, Body: body}
	}
	return Response{Status: http.StatusBadRequest, Body: body}
}
