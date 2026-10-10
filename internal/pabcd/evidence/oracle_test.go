package evidence

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// testdata/oracle-evidence.json holds what the CXC v0.2.40 oracle answered for the cases of testdata/cases.json, recorded
// once by testdata/record-oracle.mjs under Node 24 (no Node runs here). Each case runs in a fresh directory; "{S}" in a
// case is the state directory name (.codexclaw in the oracle, .crw here), "<CWD>" and "<OUT>" the workspace and an outside
// directory. Where the port is meant to differ from the oracle the test says so and states the port's answer.

type setupOp struct{ File, Dir, Symlink, Fifo, Chmod, Hardlink, Text, B64, To, Mode string }

type tombCase struct {
	ID             string
	StateRaw       *string
	StateDirIsFile bool
	SessionsIsFile bool
	Lock           string
	Changed        bool
	Ops            []struct {
		Op       string
		P        map[string]any
		Attempts int
	}
}

type caseFile struct {
	Extract []struct {
		ID      string
		Message *string
	}
	Names   []struct{ ID, Session, Agent, Turn string }
	Counter []struct {
		ID                   string
		Raw                  json.RawMessage
		Dir                  bool
		Session, Agent, Turn *string
	}
	Pressure []struct {
		ID           string
		B64          *string
		Fill, Tail   string
		Times        int
		Missing, Dir bool
	}
	Receipt []struct {
		ID    string
		Setup []setupOp
		Claim string
	}
	Writes []struct {
		ID, Pre string
		Ops     []struct {
			Write *int
			Clear bool
			Turn  string
		}
	}
	Tombstone []tombCase
}

type marked struct{ SessionID, AgentID string }

type golden struct {
	Gated struct {
		Types []string
		Max   int
	}
	Extract   map[string]*string
	Names     map[string]any
	Counter   map[string]int
	Pressure  map[string]bool
	Receipt   map[string]bool
	Writes    map[string]any
	Tombstone map[string]struct {
		Returns []bool
		Seed    *string
		State   map[string]any
		Markers []marked
	}
}

func load(t *testing.T) (caseFile, golden) {
	t.Helper()
	var c caseFile
	var g golden
	for name, into := range map[string]any{"cases.json": &c, "oracle-evidence.json": &g} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err == nil {
			err = json.Unmarshal(raw, into)
		}
		if err != nil {
			t.Fatal(name, err)
		}
	}
	return c, g
}

func sub(s, cwd, out string) string {
	return strings.NewReplacer("{S}", ".crw", "<CWD>", cwd, "<OUT>", out).Replace(s)
}

func body(b64 *string, fill string, times int, tail, text string) []byte {
	if b64 != nil {
		b, _ := base64.StdEncoding.DecodeString(*b64)
		return b
	}
	if times > 0 {
		return []byte(strings.Repeat(fill, times) + tail)
	}
	return []byte(text)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, path string, data []byte) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o777))
	must(t, os.WriteFile(path, data, 0o666))
}

func same(t *testing.T, id string, got, want any) {
	t.Helper()
	var g, w any
	for _, p := range []struct {
		v    any
		into *any
	}{{got, &g}, {want, &w}} {
		raw, err := json.Marshal(p.v)
		must(t, err)
		must(t, json.Unmarshal(raw, p.into))
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s:\n got %v\nwant %v", id, g, w)
	}
}

func TestGatedAgentTypesAndBudget(t *testing.T) {
	_, g := load(t)
	got := slices.Clone(GatedAgentTypes())
	slices.Sort(got)
	same(t, "types", got, g.Gated.Types)
	if MaxAttempts != g.Gated.Max || !IsGatedAgentType("executor") || !IsGatedAgentType("worker") || IsGatedAgentType("explorer") || IsGatedAgentType("default") || IsGatedAgentType("") {
		t.Errorf("MaxAttempts %d, gated types %v", MaxAttempts, got)
	}
}

func TestExtractReceiptPath(t *testing.T) {
	c, g := load(t)
	// Changed (port: fixed, CRW-1112): the marker is the last line that is a marker with a path, and nothing is read past its
	// line. An empty marker no longer takes the next line's text (U+2028 ends a line too), a marker inside a sentence or after
	// other text on its line is not a marker line, and of two marker lines the last wins.
	none, port := (*string)(nil), map[string]*string{}
	for _, id := range []string{"newline_after_colon", "ws_u2028", "prefixed_marker", "inline_in_prose"} {
		port[id] = none
	}
	later, renamed := "later.md", ".codexclaw/evidence/x.md"
	port["empty_first_then_valid"], port["first_marker_wins"] = &later, &renamed
	for _, k := range c.Extract {
		msg := ""
		if k.Message != nil {
			msg = *k.Message
		}
		got, ok := ExtractReceiptPath(msg)
		var gotPtr *string
		if ok {
			gotPtr = &got
		}
		want := g.Extract[k.ID]
		if p, ok := port[k.ID]; ok {
			want = p
		}
		same(t, k.ID, gotPtr, want)
	}
}

// attemptsDir lists the counter directory, a temp file's pid and random part masked.
func attemptsDir(cwd string) []string {
	entries, err := os.ReadDir(filepath.Join(cwd, ".crw", AttemptsSubdir))
	if err != nil {
		return nil
	}
	tmp := regexp.MustCompile(`\.\d+\.[A-Z2-7]+\.tmp$`)
	names := []string{}
	for _, e := range entries {
		names = append(names, tmp.ReplaceAllString(e.Name(), ".<PID>.<MS>.tmp"))
	}
	slices.Sort(names)
	return names
}

func TestAttemptsFileNames(t *testing.T) {
	c, g := load(t)
	for _, k := range c.Names {
		cwd := t.TempDir()
		WriteAttempts(cwd, k.Session, k.Agent, 2, k.Turn)
		if !state.IsCanonicalSessionID(k.Session) {
			files := attemptsDir(cwd)
			if files == nil {
				files = []string{}
			}
			// CRW-1108's refusal takes precedence over CRW-1106's old v2 session layout. Every oracle row is retained.
			same(t, k.ID, map[string]any{"files": files, "content": []string{}}, map[string]any{"files": []any{}, "content": []any{}})
			continue
		}
		if ownDirectoryCounter(k.Session, k.Agent, k.Turn) {
			// Changed (port: fixed, CRW-1106): an agent or turn id that sanitising changes needs an identity-bearing counter
			// in its canonical session's own directory, so a prefix-sharing session does not inherit its budget.
			raw, err := os.ReadFile(counterPath(cwd, k.Session, k.Agent, k.Turn))
			want := fmt.Sprintf(`{"attempts":2%s%s%s}`+"\n", jsonField("sessionId", k.Session), jsonField("agentId", k.Agent), jsonField("turnId", k.Turn))
			if err != nil || string(raw) != want || len(attemptsDir(cwd)) != 1 || attemptsDir(cwd)[0] != counterVersionDir {
				t.Errorf("%s: %q (%v), want %q in %s", k.ID, raw, err, want, counterDir(cwd, k.Session))
			}
			continue
		}
		files, content := attemptsDir(cwd), []string{}
		if files == nil {
			files = []string{}
		}
		for _, f := range files {
			raw, err := os.ReadFile(filepath.Join(cwd, ".crw", AttemptsSubdir, f))
			must(t, err)
			content = append(content, string(raw))
		}
		same(t, k.ID, map[string]any{"files": files, "content": content}, g.Names[k.ID])
	}
}

// jsonField is `,"key":"value"` for a value that is not empty, as the record's omitempty fields are written.
func jsonField(key, value string) string {
	if value == "" {
		return ""
	}
	raw, _ := json.Marshal(value)
	return `,"` + key + `":` + string(raw)
}

func TestReadAttempts(t *testing.T) {
	c, g := load(t)
	// A counter nested deeper than Go's JSON depth limit that JSON.parse rejects for another reason (cut off, trailing
	// text) reads as the cap here, where the oracle reads 0: the port stops at the depth error.
	port := map[string]int{"depth_20000_truncated": MaxAttempts, "depth_20000_trailing_text": MaxAttempts}
	// Changed (port: fixed, CRW-1106): a counter file that is there but is not an object with an integer count from 0 to
	// MaxAttempts read as 0 in the oracle, which restarted the budget; it ends the budget now, as hasSpentBudget always read it.
	for _, id := range []string{"json_null", "json_zero", "json_five", "json_empty_string", "json_string", "json_true", "json_false",
		"counter_garbage", "counter_empty", "bom", "trailing_garbage", "path_is_directory"} {
		port[id] = MaxAttempts
	}
	for _, k := range c.Counter {
		cwd := t.TempDir()
		at := [3]string{"s1", "a1", ""}
		for i, v := range []*string{k.Session, k.Agent, k.Turn} {
			if v != nil {
				at[i] = *v
			}
		}
		WriteAttempts(cwd, at[0], at[1], 0, at[2])
		path := attemptsPath(cwd, at[0], at[1], at[2])
		must(t, os.Remove(path))
		var raw any
		must(t, json.Unmarshal(k.Raw, &raw))
		switch v := raw.(type) {
		case string:
			put(t, path, []byte(v))
		case map[string]any:
			depth, tail := int(v["deep"].(float64)), ""
			if s, ok := v["tail"].(string); ok {
				tail = s
			}
			closing := strings.Repeat("]", depth)
			if v["open"] == true {
				closing = ""
			}
			put(t, path, []byte(strings.Repeat("[", depth)+closing+tail))
		}
		if k.Dir {
			must(t, os.MkdirAll(path, 0o777))
		}
		want := g.Counter[k.ID]
		if p, ok := port[k.ID]; ok {
			want = p
		}
		if got := ReadAttempts(cwd, "s1", "a1", ""); got != want {
			t.Errorf("%s: %d, want %d", k.ID, got, want)
		}
	}
}

func TestWriteAndClearAttempts(t *testing.T) {
	c, g := load(t)
	for _, k := range c.Writes {
		cwd := t.TempDir()
		switch k.Pre {
		case "state_dir_is_file":
			put(t, filepath.Join(cwd, ".crw"), []byte("x"))
		case "attempts_dir_is_file":
			put(t, filepath.Join(cwd, ".crw", AttemptsSubdir), []byte("x"))
		case "counter_is_directory":
			WriteAttempts(cwd, "s1", "a1", 0, "")
			path := attemptsPath(cwd, "s1", "a1", "")
			must(t, os.Remove(path))
			must(t, os.MkdirAll(path, 0o777))
		}
		returns := []any{}
		for _, o := range k.Ops {
			if o.Write != nil {
				returns = append(returns, WriteAttempts(cwd, "s1", "a1", *o.Write, o.Turn))
			} else {
				ClearAttempts(cwd, "s1", "a1", o.Turn)
				returns = append(returns, nil)
			}
		}
		dir := filepath.Join(cwd, ".crw", AttemptsSubdir)
		var files, content any
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			names, texts := attemptsDir(cwd), []string{}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.IsDir() {
					texts = append(texts, "<dir>")
				} else {
					raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
					texts = append(texts, string(raw))
				}
			}
			files, content = names, texts
		}
		want := g.Writes[k.ID]
		if k.ID == "write_counter_is_directory" {
			// Changed (port: fixed, CRW-1106): the write whose rename fails removes its own temp file; the oracle leaves it.
			w := want.(map[string]any)
			want = map[string]any{"returns": w["returns"], "files": w["files"].([]any)[:1], "content": w["content"].([]any)[:1]}
		}
		same(t, k.ID, map[string]any{"returns": returns, "files": files, "content": content}, want)
	}
}

func TestTranscriptHasContextPressure(t *testing.T) {
	c, g := load(t)
	for _, k := range c.Pressure {
		path := filepath.Join(t.TempDir(), "child.jsonl")
		switch {
		case k.Dir:
			must(t, os.Mkdir(path, 0o777))
		case !k.Missing:
			put(t, path, body(k.B64, k.Fill, k.Times, k.Tail, ""))
		}
		if got := TranscriptHasContextPressure(path); got != g.Pressure[k.ID] {
			t.Errorf("%s: %v, want %v", k.ID, got, g.Pressure[k.ID])
		}
	}
	if TranscriptHasContextPressure("") {
		t.Error("an empty path has no pressure")
	}
}

func TestHasValidReceipt(t *testing.T) {
	c, g := load(t)
	for _, k := range c.Receipt {
		root := t.TempDir()
		cwd, out := filepath.Join(root, "ws"), filepath.Join(root, "out")
		must(t, os.MkdirAll(cwd, 0o777))
		must(t, os.MkdirAll(out, 0o777))
		at := func(p string) string {
			if p = sub(p, cwd, out); filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(cwd, p)
		}
		for _, o := range k.Setup {
			switch {
			case o.Dir != "":
				must(t, os.MkdirAll(at(o.Dir), 0o777))
			case o.File != "":
				put(t, at(o.File), body(nilIf(o.B64), "", 0, "", o.Text))
			case o.Symlink != "":
				must(t, os.MkdirAll(filepath.Dir(at(o.Symlink)), 0o777))
				must(t, os.Symlink(sub(o.To, cwd, out), at(o.Symlink)))
			case o.Hardlink != "":
				must(t, os.MkdirAll(filepath.Dir(at(o.Hardlink)), 0o777))
				must(t, os.Link(sub(o.To, cwd, out), at(o.Hardlink)))
			case o.Fifo != "":
				must(t, os.MkdirAll(filepath.Dir(at(o.Fifo)), 0o777))
				must(t, syscall.Mkfifo(at(o.Fifo), 0o666))
			case o.Chmod != "":
				var mode uint32
				for _, d := range o.Mode {
					mode = mode*8 + uint32(d-'0')
				}
				must(t, os.Chmod(at(o.Chmod), os.FileMode(mode)))
			}
		}
		want := g.Receipt[k.ID]
		if k.ID == "receipt_name_starts_with_dotdot" || k.ID == "dotdot_directory_name" {
			want = true // changed (port: fixed, CRW-1112): a name that starts with two dots lies inside the root
		}
		if got := HasValidReceipt(cwd, sub(k.Claim, cwd, out)); got != want {
			t.Errorf("%s: %v, want %v", k.ID, got, want)
		}
	}
}

func nilIf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// TestTombstone replays the oracle's recordTombstone and hasTombstone. A case the oracle answers by overwriting an unreadable
// session file with a default state that carries the corruption sentinel is intentionally changed: the port leaves that file
// as it is and hands the verdict to the marker writer (the state-loss fix).
func TestTombstone(t *testing.T) {
	c, g := load(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, k := range c.Tombstone {
		cwd := t.TempDir()
		sessions := filepath.Join(cwd, ".crw", "sessions")
		want := g.Tombstone[k.ID]
		if want.Seed != nil {
			put(t, filepath.Join(sessions, "s1.json"), []byte(*want.Seed))
		}
		if k.StateRaw != nil {
			put(t, filepath.Join(sessions, "s1.json"), []byte(*k.StateRaw))
		}
		if k.StateDirIsFile {
			put(t, filepath.Join(cwd, ".crw"), []byte("x"))
		}
		if k.SessionsIsFile {
			put(t, sessions, []byte("x"))
		}
		lock := lockFunc(state.WithSessionLock)
		switch k.Lock {
		case "held":
			put(t, filepath.Join(sessions, "s1.json.lock"), []byte(strconv.Itoa(os.Getpid()))) // a live holder: since CRW-1094 a dead owner's lock is taken over
		case "sentinel_window": // the first acquisition fails, the second finds the lock free
			calls := 0
			lock = func(cwd, sessionID string, fn func() error) error {
				if calls++; calls == 1 {
					return errors.New("held")
				}
				return state.WithSessionLock(cwd, sessionID, fn)
			}
		}
		markers := []marked{}
		marker := func(dir, sessionID, agentID string) error {
			if dir != cwd {
				t.Errorf("%s: marker called for %s, want %s", k.ID, dir, cwd)
			}
			markers = append(markers, marked{sessionID, agentID})
			return nil
		}
		returns := []bool{}
		for _, o := range k.Ops {
			str := func(key string) string { s, _ := o.P[key].(string); return s }
			p := Payload{AgentType: str("agent_type"), AgentID: str("agent_id"), TurnID: str("turn_id"), LastAssistantMessage: str("last_assistant_message")}
			if o.Op == "record" {
				returns = append(returns, recordTombstone(cwd, "s1", p, o.Attempts, now, lock, marker))
			} else {
				returns = append(returns, HasTombstone(cwd, "s1", p))
			}
		}
		fileState := func(raw []byte) map[string]any {
			var m map[string]any
			if json.Unmarshal(raw, &m) != nil {
				m = map[string]any{"raw": string(raw)}
			}
			delete(m, "updatedAt")
			return m
		}
		var gotState map[string]any
		if raw, err := os.ReadFile(filepath.Join(sessions, "s1.json")); err == nil {
			gotState = fileState(raw)
		}
		wantState, wantMarkers := any(want.State), want.Markers
		wantReturns := want.Returns
		if k.ID == "record_65th_when_64_exist" {
			// Changed (port: fixed, CRW-1110): the 65th verdict is recorded beside the main list, which keeps the 64 the reader
			// reads, and hasTombstone finds it there; the oracle appended it to the file, where no reader finds it.
			wantReturns, wantState = []bool{true, true}, fileStateSeed(t, want.Seed)
			if _, ok := readOverflow(overflowPath(cwd, "s1", "a1", "t1"), tupleDigest("a1", "t1")+".json", "s1"); !ok {
				t.Errorf("%s: no overflow verdict of a1", k.ID)
			}
		}
		switch {
		case k.Changed:
			wantState, wantMarkers = fileState([]byte(*k.StateRaw)), []marked{{"s1", "a1"}}
		case k.ID == "tier3_state_dir_is_file": // the oracle's marker write fails there too; the port still hands over
			wantMarkers = []marked{{"s1", "a1"}}
		}
		same(t, k.ID+" returns", returns, wantReturns)
		same(t, k.ID+" state", gotState, wantState)
		same(t, k.ID+" markers", markers, wantMarkers)
	}
}

// fileStateSeed is a recorded seed state as a map without its updatedAt.
func fileStateSeed(t *testing.T, seed *string) map[string]any {
	t.Helper()
	var m map[string]any
	must(t, json.Unmarshal([]byte(*seed), &m))
	delete(m, "updatedAt")
	return m
}
