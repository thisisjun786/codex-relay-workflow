package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state files the CXC v0.2.40 oracle left in the expect.tree of contract/fixtures/cxc are goldens. They sit under
// .codexclaw/ in the fixtures; the port reads them under .crw/ (name-substitution R26). A state file written by the oracle
// (form json-pretty-nonl) or kept as given in full (json-line) must restore and encode back to the same bytes; a partial
// given file must restore; the one corrupt file is unreadable. Placeholders of the normalisation are bound to absolute paths
// first, because boundSourceRoot keeps only an absolute path.

type fixtureFile struct {
	Expect struct {
		Tree map[string]struct {
			Form string          `json:"form"`
			JSON json.RawMessage `json:"json"`
			Text string          `json:"text"`
		} `json:"tree"`
	} `json:"expect"`
}

func TestCorpusStateFilesAreGoldens(t *testing.T) {
	files, err := filepath.Glob("../../../contract/fixtures/cxc/*.json")
	if err != nil || len(files) < 500 {
		t.Fatalf("corpus: %d fixtures, %v", len(files), err)
	}
	bind := strings.NewReplacer("$"+"{WS}", "/ws", "$"+"{TMP}", "/tmp/t", "$"+"{HOME}", "/home/h", "$"+"{CODEX_HOME}", "/codex", "$"+"{CXC_HOME}", "/cxc")
	var encoded, partial, rejected int
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var fx fixtureFile
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for name, e := range fx.Expect.Tree {
			dir, base := filepath.Split(name)
			if !strings.HasSuffix(dir, "/.codexclaw/sessions/") || !strings.HasSuffix(base, ".json") {
				continue
			}
			id, golden := strings.TrimSuffix(base, ".json"), e.Text
			if e.Form != "text" {
				var compact, pretty bytes.Buffer
				if err := json.Compact(&compact, e.JSON); err != nil || json.Indent(&pretty, compact.Bytes(), "", "  ") != nil {
					t.Fatalf("%s %s: %v", file, name, err)
				}
				golden = bind.Replace(pretty.String())
			}
			s, unreadable := ReadStateStrict(put(t, id, golden), id)
			var keys map[string]json.RawMessage
			_ = json.Unmarshal([]byte(golden), &keys)
			_, full := keys["dcloseRecovery"]
			want := golden
			if filepath.Base(file) == "cli__orchestrate__i_to_p_agent_override.json" { // flags.interview:true without a tracker restores false
				want = strings.Replace(golden, "\"interview\": true", "\"interview\": false", 1)
			}
			switch {
			case e.Form == "text":
				if !unreadable {
					t.Errorf("%s %s: corrupt file read as %+v", file, name, s)
				}
				rejected++
			case unreadable:
				t.Errorf("%s %s: unreadable", file, name)
			case !full:
				partial++
			default:
				if got := mustEncode(t, s); got != want {
					t.Errorf("%s %s: re-encoded\n%s\nwant\n%s", file, name, got, want)
				}
				encoded++
			}
		}
	}
	if encoded < 100 || partial < 5 || rejected != 1 {
		t.Fatalf("goldens checked: %d full, %d partial, %d corrupt", encoded, partial, rejected)
	}
}
