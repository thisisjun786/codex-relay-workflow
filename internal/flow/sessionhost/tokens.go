package sessionhost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Usage is a token total: the total_token_usage of a rollout's token_count event.
type Usage struct {
	Input, CachedInput, Output, ReasoningOutput, Total int64
}

// Tokens reads the tokens a thread used from its rollout (Thread.path): the total of the last token_count event whose info
// is set. The file is only read. A rollout that is absent, or holds no such event, is unmeasured (measured false, no
// error); a line that does not decode is skipped, and what follows the last line feed (a write the host had not finished)
// is left out. An error is a rollout that exists but cannot be read.
func Tokens(rolloutPath string) (usage Usage, measured bool, err error) {
	if rolloutPath == "" {
		return Usage{}, false, nil
	}
	f, err := os.Open(rolloutPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Usage{}, false, nil
		}
		return Usage{}, false, fmt.Errorf("sessionhost: the rollout %s: %w", rolloutPath, err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			// the cut end of a write that did not finish is no event
			return usage, measured, nil
		}
		if err != nil {
			return Usage{}, false, fmt.Errorf("sessionhost: the rollout %s: %w", rolloutPath, err)
		}
		if !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		if u, ok := tokenCount(line); ok {
			usage, measured = u, true
		}
	}
}

// tokenCount decodes one rollout line that is a token_count event with its info set.
func tokenCount(line []byte) (Usage, bool) {
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			Info *struct {
				Total *struct {
					Input           int64 `json:"input_tokens"`
					CachedInput     int64 `json:"cached_input_tokens"`
					Output          int64 `json:"output_tokens"`
					ReasoningOutput int64 `json:"reasoning_output_tokens"`
					Total           int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil || record.Type != "event_msg" || record.Payload.Type != "token_count" ||
		record.Payload.Info == nil || record.Payload.Info.Total == nil {
		return Usage{}, false
	}
	t := record.Payload.Info.Total
	return Usage{Input: t.Input, CachedInput: t.CachedInput, Output: t.Output, ReasoningOutput: t.ReasoningOutput, Total: t.Total}, true
}
