package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSendByIDDocsAndHelpNameTheUnheldPath (CRW-1558 U3): the two texts that say what a send by id
// does when the hierarchy moved the message's endpoints - the send-by-id paragraph of
// docs/relay/supervisor-channel.md and the supervisor-send --message help - say it per hold. The hold
// rule answers only a superseded_by_report hold, which keeps its hold and writes nothing; a message
// with no hold is held hierarchy_unresolved by the attempt's resolve
// (TestSendByIDOfAnUnheldMessageWhoseEndpointsMovedHoldsItUnaddressed). Neither text may say a send
// by id after a move always writes nothing, and each names both holds; the docs also name the journal
// kind the unheld path writes.
func TestSendByIDDocsAndHelpNameTheUnheldPath(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "relay", "supervisor-channel.md"))
	if err != nil {
		t.Fatal(err)
	}
	paragraph := sendByIDDocsParagraph(string(docs))
	if paragraph == "" {
		t.Fatal(`docs/relay/supervisor-channel.md has no paragraph starting "A send by id is"`)
	}
	help, err := sendByIDMessageHelp(filepath.Join("..", "argparse", "specs.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []struct{ name, body string }{{"the docs paragraph", paragraph}, {"the supervisor-send --message help", help}} {
		for _, overPromise := range []string{"answers relation_owner_drift and writes nothing", "answers `relation_owner_drift` and writes nothing"} {
			if strings.Contains(text.body, overPromise) {
				t.Errorf("%s says a send by id after a move %q, which only the superseded_by_report hold does: %s", text.name, overPromise, text.body)
			}
		}
		for _, word := range []string{"superseded_by_report", "hierarchy_unresolved"} {
			if !strings.Contains(text.body, word) {
				t.Errorf("%s does not name %s: %s", text.name, word, text.body)
			}
		}
	}
	if !strings.Contains(paragraph, "supervisor_message_unaddressed") {
		t.Errorf("the docs paragraph does not name the journal kind supervisor_message_unaddressed: %s", paragraph)
	}
}

// sendByIDDocsParagraph is the paragraph of doc that starts "A send by id is", up to the next blank
// line, or "" when there is none.
func sendByIDDocsParagraph(doc string) string {
	start := strings.Index(doc, "\nA send by id is")
	if start < 0 {
		return ""
	}
	rest := doc[start+1:]
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// sendByIDMessageHelp decodes the argparse specs at path and answers the help of supervisor-send's
// --message option.
func sendByIDMessageHelp(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var specs []struct {
		Name    string `json:"name"`
		Options []struct {
			Flag string `json:"flag"`
			Help string `json:"help"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &specs); err != nil {
		return "", err
	}
	for _, spec := range specs {
		if spec.Name != "supervisor-send" {
			continue
		}
		for _, option := range spec.Options {
			if option.Flag == "--message" {
				if option.Help == "" {
					return "", os.ErrNotExist
				}
				return option.Help, nil
			}
		}
	}
	return "", os.ErrNotExist
}
