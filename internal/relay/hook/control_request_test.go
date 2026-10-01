package hook

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// socketPair is a connected pair of stream sockets: the owner's end and the peer's.
func socketPair(t *testing.T) (owner, peer *net.UnixConn) {
	t.Helper()
	// Darwin has no SOCK_CLOEXEC: mark both ends close-on-exec under ForkLock instead, so a
	// concurrent exec inherits neither.
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		file := os.NewFile(uintptr(fd), "control-pair")
		conn, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		ends[i] = conn.(*net.UnixConn)
	}
	return ends[0], ends[1]
}

// askOwner writes frame to HandleControl over a socket pair, closes the peer's write side and
// returns the one line the owner answered.
func askOwner(t *testing.T, state string, frame []byte) []byte {
	t.Helper()
	owner, peer := socketPair(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- HandleControl(ctx, owner, state) }()
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write(frame); err != nil {
		t.Fatal(err)
	}
	if err := peer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(peer).ReadBytes('\n')
	_ = peer.Close()
	if err := <-done; err != nil {
		t.Errorf("the handler failed: %v", err)
	}
	return line
}

// The owner reads a guard request strictly: one line of UTF-8 JSON, an object, nested no deeper
// than controlDepth containers, whose params carry an object stopInput and an RFC 3339 deadline
// still ahead. Every request it cannot serve is answered with the relay's host record, exactly
// {"error": "host", "detail": <why>}, which the hook journals as guard_host_error; nothing is
// evaluated for it. What a hook forwards of a Stop payload it accepted (NaN and the infinities,
// a lone surrogate escape) is still served.
func TestControlAnswersEveryRequestItCannotServeWithTheHostRecord(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", root)
	later := time.Now().Add(time.Hour).UTC()
	request := func(protocol, deadline, extra, stop string) []byte {
		return []byte(`{"protocol":` + protocol + `,"method":"guard-evaluate","params":{"markerRoot":` + strconv.Quote(root) +
			`,"stopInput":` + stop + `,"mode":"observe","now":"2026-01-01T00:00:00Z","noRecord":true,"deadline":` + deadline + extra + "}}\n")
	}
	valid := func(extra string) []byte {
		return request("1", strconv.Quote(later.Format(time.RFC3339Nano)), extra, "{}")
	}
	nested := func(depth int) []byte {
		return []byte(strings.Repeat("[", depth) + strings.Repeat("]", depth) + "\n")
	}
	for name, frame := range map[string][]byte{
		"not json":                      []byte("not json\n"),
		"not UTF-8":                     []byte("{\"protocol\":1,\"x\":\"\xff\"}\n"),
		"led by a byte order mark":      append([]byte("\xef\xbb\xbf"), valid("")...),
		"UTF-16":                        []byte("\xff\xfe{\x00}\x00\n\x00"),
		"trailing data":                 []byte("{} {}\n"),
		"not an object":                 []byte("[1]\n"),
		"nested past the cap":           nested(controlDepth + 1),
		"nested past a goroutine stack": []byte(`{"params":` + strings.Repeat("[", 5000000) + "\n"),
		"no params":                     []byte(`{"protocol":1,"method":"guard-evaluate"}` + "\n"),
		"params not an object":          []byte(`{"protocol":1,"method":"guard-evaluate","params":[]}` + "\n"),
		"no stop input":                 []byte(`{"protocol":1,"method":"guard-evaluate","params":{}}` + "\n"),
		"stop input not an object":      request("1", strconv.Quote(later.Format(time.RFC3339Nano)), "", "[]"),
		"null deadline":                 request("1", "null", "", "{}"),
		"unparseable deadline":          request("1", `"soon"`, "", "{}"),
		"naive deadline":                request("1", `"2999-01-01T00:00:00"`, "", "{}"),
		"expired deadline":              request("1", `"2020-01-01T00:00:00+00:00"`, "", "{}"),
		"socketPath not a string":       valid(`,"socketPath":1`),
		"program not a string":          valid(`,"program":["crw"]`),
		"mode not a string":             valid(`,"mode":5`),
		"now not a string":              valid(`,"now":0`),
	} {
		t.Run(name, func(t *testing.T) {
			var answer map[string]any
			line := askOwner(t, state, frame)
			if err := json.Unmarshal(line, &answer); err != nil {
				t.Fatalf("not a JSON answer: %q", line)
			}
			detail, ok := answer["detail"].(string)
			if len(answer) != 2 || answer["error"] != "host" || !ok || detail == "" {
				t.Fatalf("answered %q, not the host record", line)
			}
		})
	}
	// A dispatcher that is not guard-evaluate at protocol 1 rejects the call before any command.
	for name, frame := range map[string][]byte{
		"protocol 1.0":  request("1.0", strconv.Quote(later.Format(time.RFC3339Nano)), "", "{}"),
		"protocol true": request("true", strconv.Quote(later.Format(time.RFC3339Nano)), "", "{}"),
		"protocol 2":    request("2", strconv.Quote(later.Format(time.RFC3339Nano)), "", "{}"),
	} {
		t.Run(name, func(t *testing.T) {
			if line := string(askOwner(t, state, frame)); line != "{\"protocol\":1,\"requestRejected\":true}\n" {
				t.Fatalf("answered %q, not the rejection", line)
			}
		})
	}
	// Served: the deadline as the hook spells it (UTC, nanoseconds), or with an offset, and a
	// Stop payload holding what the hook's stdin reading accepts.
	for name, frame := range map[string][]byte{
		"deadline in UTC":         valid(""),
		"deadline with an offset": request("1", strconv.Quote(later.In(time.FixedZone("", 5*3600+1800)).Format(time.RFC3339)), "", "{}"),
		"deadline in seconds":     request("1", strconv.Quote(later.Format(time.RFC3339)), "", "{}"),
		"constants and a surrogate in the payload": request("1", strconv.Quote(later.Format(time.RFC3339Nano)), "",
			`{"a":NaN,"b":-Infinity,"c":"\udcff","d":[Infinity]}`),
		"nested to the cap": request("1", strconv.Quote(later.Format(time.RFC3339Nano)), "",
			`{"a":`+strings.Repeat("[", controlDepth-3)+strings.Repeat("]", controlDepth-3)+"}"),
	} {
		t.Run(name, func(t *testing.T) {
			var verdict map[string]any
			line := askOwner(t, state, frame)
			if err := json.Unmarshal(line, &verdict); err != nil || verdict["decision"] != "release" {
				t.Fatalf("the owner did not serve it: %q %v", line, err)
			}
		})
	}
}

// noRecord asks for no record only when it is true: the hook sends a JSON boolean.
func TestControlReadsNoRecordAsTheBooleanTheHookSends(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", root)
	deadline := strconv.Quote(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	for _, c := range []struct {
		noRecord   string
		downgraded bool
	}{{"true", true}, {"false", false}, {"1", false}} {
		frame := []byte(`{"protocol":1,"method":"guard-evaluate","params":{"markerRoot":` + strconv.Quote(root) +
			`,"stopInput":{},"mode":"hold","now":"2026-01-01T00:00:00Z","noRecord":` + c.noRecord + `,"deadline":` + deadline + "}}\n")
		var verdict map[string]any
		line := askOwner(t, state, frame)
		if err := json.Unmarshal(line, &verdict); err != nil || verdict["decision"] != "release" {
			t.Fatalf("noRecord %s: not served: %q %v", c.noRecord, line, err)
		}
		if got := verdict["modeDowngraded"] == "hold_requires_a_recorded_observation"; got != c.downgraded {
			t.Errorf("noRecord %s: modeDowngraded %v", c.noRecord, verdict["modeDowngraded"])
		}
	}
}
