package agy

import (
	"io"
	"os"
	"regexp"
	"strconv"
)

var (
	// startedRe is agy's "Print mode: starting (promptLength=58, model="gemini-3.8-flash-high", conversationID="")".
	startedRe = regexp.MustCompile(`Print mode: starting \(promptLength=(\d+), model="([^"]*)"`)
	// servedRe is the line that names the model agy sends the turn to, as agy labels it ("Gemini 3.8 Flash (High)").
	servedRe = regexp.MustCompile(`Propagating selected model override to backend: label="([^"]*)"`)
)

// agyLog is what the runner reads from agy's per-call log.
type agyLog struct {
	promptLength int    // -1 when the log has no starting line
	requested    string // the model id agy was asked for
	served       string // the label of the model agy used, after the starting line
}

func parseLog(text string) agyLog {
	l := agyLog{promptLength: -1}
	rest := text
	if m := startedRe.FindStringSubmatchIndex(text); m != nil {
		if n, err := strconv.Atoi(text[m[2]:m[3]]); err == nil {
			l.promptLength = n
		}
		l.requested, rest = text[m[4]:m[5]], text[m[1]:]
	}
	if m := servedRe.FindStringSubmatch(rest); m != nil {
		l.served = m[1]
	}
	return l
}

// readLog is the first max bytes of the log, or nothing if there is none.
func readLog(path string, max int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, int64(max)))
	return string(b)
}
