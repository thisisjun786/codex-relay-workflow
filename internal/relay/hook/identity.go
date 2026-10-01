package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

const EventKeyTag = "crw-stop-event/1"
const ScanMaxBytes = 64 << 20
const scanChunk = 1 << 16

// EventKey is byte-identical to stopadapter.event_key, including ensure_ascii.
func EventKey(session, turn, active, item any) string {
	sum := sha256.Sum256([]byte(pyjson.Dumps([]any{EventKeyTag, session, turn, active, item}, pyjson.Options{Compact: true})))
	return hex.EncodeToString(sum[:])
}

type transcriptItem struct {
	kind             string
	id, said, thread any
}

func classify(row any, turn string) *transcriptItem {
	o, ok := evidence.Object(row)
	if !ok || get(o, "type") != "event_msg" {
		return nil
	}
	body, ok := evidence.Object(get(o, "payload"))
	if !ok || get(body, "turn_id") != turn {
		return nil
	}
	if get(body, "type") == "task_started" {
		return &transcriptItem{kind: "start"}
	}
	item, ok := evidence.Object(get(body, "item"))
	if !ok || get(body, "type") != "item_completed" {
		return nil
	}
	switch get(item, "type") {
	case "HookPrompt", "UserMessage":
		return &transcriptItem{kind: "input", id: get(item, "type")}
	case "AgentMessage":
		var said any
		if parts, ok := evidence.List(get(item, "content")); ok {
			var b strings.Builder
			for _, part := range parts {
				p, ok := evidence.Object(part)
				if !ok || get(p, "type") != "Text" {
					continue
				}
				if s, ok := get(p, "text").(string); ok {
					b.WriteString(s)
				}
			}
			said = b.String()
		}
		return &transcriptItem{kind: "answer", id: get(item, "id"), said: said, thread: get(body, "thread_id")}
	}
	return nil
}

// EventIdentity scans newest-first, never crossing an unreadable or unfinished tail.
func EventIdentity(ctx context.Context, stop Object) (string, Object) {
	scanCtx, cancel := context.WithDeadline(ctx, time.Now().Add(750*time.Millisecond))
	defer cancel()
	identity := Object{{Key: "established", Value: false}, {Key: "reason", Value: nil}, {Key: "answerItem", Value: nil}, {Key: "transcriptPath", Value: nil}, {Key: "scannedBytes", Value: 0}, {Key: "scannedLines", Value: 0}}
	refuse := func(reason string) (string, Object) { return "", set(identity, "reason", reason) }
	session, sok := get(stop, "session_id").(string)
	turn, tok := get(stop, "turn_id").(string)
	active, aok := get(stop, "stop_hook_active").(bool)
	said, mok := get(stop, "last_assistant_message").(string)
	if !sok || session == "" || !tok || turn == "" || !aok || !mok {
		return refuse("identity_fields_incomplete")
	}
	path := text(get(stop, "transcript_path"))
	if path == "" {
		return refuse("transcript_path_missing")
	}
	identity = set(identity, "transcriptPath", path)
	if !filepath.IsAbs(path) {
		return refuse("transcript_path_relative")
	}
	// The payload's str reaches the system as os.fsencode's bytes: a surrogate escape is the byte
	// it stands for, and one nothing encodes is the ValueError Python counts as unreachable.
	path, encoded := pyvalue.FSEncode(path)
	if !encoded {
		return refuse("transcript_unreachable")
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return refuse("transcript_absent")
	}
	if err != nil {
		return refuse("transcript_unreachable")
	}
	if !info.Mode().IsRegular() {
		return refuse("transcript_not_regular")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return refuse("transcript_absent")
	}
	if err != nil {
		return refuse("transcript_unreachable")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return refuse("transcript_unreachable")
	}
	if !info.Mode().IsRegular() {
		return refuse("transcript_not_regular")
	}
	items, reason := turnItems(scanCtx, f, info.Size(), turn, &identity)
	if reason != "" {
		return refuse(reason)
	}
	hasAnswer := false
	for _, item := range items {
		hasAnswer = hasAnswer || item.kind == "answer"
	}
	if !hasAnswer {
		return refuse("no_answer_item_for_turn")
	}
	if items[0].kind != "answer" {
		return refuse("answer_precedes_latest_input")
	}
	var matching []transcriptItem
	flag := false
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.kind == "input" && item.id == "HookPrompt" {
			flag = true
		}
		if item.kind != "answer" || (i > 0 && items[i-1].kind != "input") {
			continue
		}
		if item.said == said && flag == active {
			matching = append(matching, item)
		}
	}
	if len(matching) == 0 {
		return refuse("answer_text_mismatch")
	}
	if len(matching) > 1 {
		return refuse("answer_text_ambiguous")
	}
	item := matching[0]
	id, ok := item.id.(string)
	if !ok || id == "" {
		return refuse("answer_item_unidentified")
	}
	identity = set(identity, "answerItem", id)
	if item.thread != session {
		return refuse("session_mismatch")
	}
	identity = set(identity, "established", true)
	return EventKey(session, turn, active, id), identity
}
func turnItems(ctx context.Context, f *os.File, size int64, turn string, identity *Object) ([]transcriptItem, string) {
	if size == 0 {
		return nil, "no_answer_item_for_turn"
	}
	tail := make([]byte, 1)
	if _, err := f.ReadAt(tail, size-1); err != nil {
		return nil, "transcript_unreachable"
	}
	unfinished := tail[0] != '\n'
	var items []transcriptItem
	var pieces [][]byte
	position := size
	newest := true
	scanned, lines := 0, 0
	defer func() {
		*identity = set(*identity, "scannedBytes", scanned)
		*identity = set(*identity, "scannedLines", lines)
	}()
	for position > 0 {
		if scanned >= ScanMaxBytes {
			return nil, "scan_bound_exceeded"
		}
		if ctx.Err() != nil {
			return nil, "scan_timed_out"
		}
		width := min(int64(scanChunk), position)
		position -= width
		chunk := make([]byte, width)
		n, err := f.ReadAt(chunk, position)
		if err != nil && err != io.EOF {
			return nil, "transcript_unreachable"
		}
		chunk = chunk[:n]
		scanned += n
		segments := bytes.Split(chunk, []byte{'\n'})
		var complete [][]byte
		if len(segments) == 1 {
			pieces = append(pieces, chunk)
			if position > 0 {
				continue
			}
			complete = [][]byte{joinReverse(pieces)}
			pieces = nil
		} else {
			last := append(segments[len(segments)-1], joinReverse(pieces)...)
			complete = append(complete, last)
			for i := len(segments) - 2; i > 0; i-- {
				complete = append(complete, segments[i])
			}
			if position == 0 {
				complete = append(complete, segments[0])
				pieces = nil
			} else {
				pieces = [][]byte{segments[0]}
			}
		}
		for _, line := range complete {
			lines++
			if newest {
				newest = false
				if unfinished {
					return nil, "transcript_tail_incomplete"
				}
				continue
			}
			if !bytes.Contains(line, []byte(turn)) {
				continue
			}
			if ctx.Err() != nil {
				return nil, "scan_timed_out"
			}
			v, e := Decode(line)
			if e != nil {
				return nil, "transcript_line_unreadable"
			}
			item := classify(v, turn)
			if item == nil {
				continue
			}
			if item.kind == "start" {
				return items, ""
			}
			items = append(items, *item)
		}
	}
	return nil, "turn_start_not_found"
}
func joinReverse(pieces [][]byte) []byte {
	n := 0
	for _, p := range pieces {
		n += len(p)
	}
	b := make([]byte, 0, n)
	for i := len(pieces) - 1; i >= 0; i-- {
		b = append(b, pieces[i]...)
	}
	return b
}
