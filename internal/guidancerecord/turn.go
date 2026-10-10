package guidancerecord

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"syscall"
)

// The evidence that a compact start is the compaction of the turn a resume ran in (CRW-1180, evaluation d1). The prompt hook cannot give
// it: it is a separate, separately trusted hook whose notification can be missing (disabled, untrusted, an input it refuses), and a
// compact payload names no turn. The session's transcript can: SessionStart names it (transcript_path), and Codex records every turn in
// it. In the isolated trial's codex 0.154.0 rollouts a resumed session whose first turn compacts writes the turn's start, the compaction
// (a `compacted` record and an item_completed ContextCompaction event of the turn), then runs the resume's SessionStart hooks, records
// their answers, runs the compact's, records theirs, and only then records the user's prompt and runs the prompt hook. So:
//
//   - a resume leaves a pair only when the end of the transcript shows that its turn compacted before it ran, and nothing of the turn after
//     the compaction but its context (resumeTurn); it keeps the turn and where the transcript ended;
//   - the compact takes the pair only when the same transcript has gained, since then, nothing but records that name no other turn, and no
//     user prompt, turn start or end, or compaction (compactOfTurn).
//
// Anything else (no transcript, one that cannot be read, another file, a record that does not parse, more than a bounded read) is no
// evidence, and the compact says the text: a duplicate costs tokens, a lost text costs the guidance.

const (
	// turnTail bounds what a resume reads of the end of the transcript to find the compaction of its turn.
	turnTail = 256 << 10
	// turnRange bounds what a compact reads of what the transcript gained since the resume.
	turnRange = 1 << 20
)

// rolloutRecord is the part of a transcript line the evidence reads.
type rolloutRecord struct {
	Type    string `json:"type"`
	Payload struct {
		Type string          `json:"type"`
		Turn any             `json:"turn_id"`
		Role any             `json:"role"`
		Item json.RawMessage `json:"item"`
	} `json:"payload"`
}

func (r rolloutRecord) turn() string { s, _ := r.Payload.Turn.(string); return s }

// item is the type of the item an item event names, or "".
func (r rolloutRecord) item() string {
	if r.Type != "event_msg" || len(r.Payload.Item) == 0 {
		return ""
	}
	var item struct {
		Type any `json:"type"`
	}
	if json.Unmarshal(r.Payload.Item, &item) != nil {
		return ""
	}
	s, _ := item.Type.(string)
	return s
}

// compaction is a compaction record or event of any form.
func (r rolloutRecord) compaction() bool {
	return r.Type == "compacted" || r.Type == "event_msg" && (r.Payload.Type == "context_compacted" || r.item() == "ContextCompaction")
}

// compacted is the event of a finished compaction, which names its turn.
func (r rolloutRecord) compacted() bool {
	return r.Type == "event_msg" && r.Payload.Type == "item_completed" && r.item() == "ContextCompaction"
}

// prompt is a user prompt's event.
func (r rolloutRecord) prompt() bool {
	return r.Type == "event_msg" && (r.Payload.Type == "user_message" || r.item() == "UserMessage")
}

// edge is a turn's start, end or abort.
func (r rolloutRecord) edge() bool {
	return r.Type == "event_msg" && (r.Payload.Type == "task_started" || r.Payload.Type == "task_complete" || r.Payload.Type == "turn_aborted")
}

// readRollout reads the whole records of the transcript at path, without waiting on a FIFO and refusing anything but a regular file. With
// from < 0 it reads the records that lie in the last limit bytes; otherwise those from the offset from to the end, which must be a record's
// start and at most limit bytes before the end, every byte of them landed. end is the offset after the last whole record read.
func readRollout(path string, from, limit int64) (records []rolloutRecord, end int64, ok bool) {
	if path == "" {
		return nil, 0, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, false
	}
	size := info.Size()
	start := max(0, size-limit-1)
	if from >= 0 {
		if from > size || size-from > limit {
			return nil, 0, false
		}
		start = max(0, from-1)
	}
	data := make([]byte, size-start)
	if n, err := f.ReadAt(data, start); n != len(data) || err != nil && err != io.EOF {
		return nil, 0, false
	}
	base := start
	if start > 0 {
		// The byte before the window says whether the window starts a record.
		lead := data[0]
		data, base = data[1:], start+1
		if lead != '\n' {
			if from >= 0 {
				return nil, 0, false
			}
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				return nil, 0, false
			}
			data, base = data[i+1:], base+int64(i+1)
		}
	}
	last := bytes.LastIndexByte(data, '\n')
	if from >= 0 && last != len(data)-1 && len(data) > 0 {
		return nil, 0, false // a record still being written: it could be anything
	}
	end = base + int64(last+1)
	for _, line := range bytes.Split(data[:last+1], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r rolloutRecord
		if json.Unmarshal(line, &r) != nil {
			return nil, 0, false
		}
		records = append(records, r)
	}
	return records, end, true
}

// resumeTurn is the turn a resume start runs in, read from the end of the transcript, and the offset where the transcript ended: a turn
// only when its compaction is the last thing the transcript shows of it but its context, so the resume's answer lands after the compaction
// record. A resume without that (no compaction in its turn yet, a prompt or an end after it, another turn) leaves no pair.
func resumeTurn(transcript string) (turn string, offset int64, ok bool) {
	records, end, ok := readRollout(transcript, -1, turnTail)
	if !ok {
		return "", 0, false
	}
	for _, r := range records {
		switch {
		case r.compacted() && r.turn() != "":
			turn = r.turn()
		case r.compaction(), r.prompt(), r.edge() && !(r.Payload.Type == "task_started" && r.turn() == turn), r.turn() != "" && r.turn() != turn:
			turn = ""
		}
	}
	return turn, end, turn != ""
}

// compactOfTurn reports whether the transcript a compact start names shows it is the compaction of the turn the mark's resume ran in:
// the same file, which since the resume gained only whole records of that turn or of none (the answers of the hooks), and no user prompt,
// turn start or end, or compaction.
func compactOfTurn(transcript string, m mark) bool {
	if transcript == "" || digest(transcript) != m.Transcript {
		return false
	}
	records, _, ok := readRollout(transcript, m.Offset, turnRange)
	if !ok {
		return false
	}
	for _, r := range records {
		if r.compaction() || r.prompt() || r.edge() || r.Type == "response_item" && r.Payload.Role == "user" ||
			r.turn() != "" && digest(r.turn()) != m.Turn {
			return false
		}
	}
	return true
}
