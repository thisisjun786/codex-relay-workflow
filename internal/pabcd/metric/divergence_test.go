package metric

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The first four tests are the library tests of divergence.test.ts at CXC v0.2.40 (its other two drive the divergence CLI); then come
// the cases the Go port adds, and the replay of the answers the oracle gave (testdata/record-divergence-oracle.mjs; no Node runs here).
// The helpers of metrics_test.go are shared.

func divClock(ts string) func() string { return func() string { return ts } }

func divPtr[T any](v T) *T { return &v }

// divEnum is the enum a scenario op names: nil when the op has none, which is not the same as the empty string.
func divEnum[T ~string](s *string) *T {
	if s == nil {
		return nil
	}
	return divPtr(T(*s))
}

func divAdd(t *testing.T, cwd string, in CandidateInput) DivergenceCandidate {
	t.Helper()
	c, err := RecordDivergenceCandidate(cwd, in)
	metricsMust(t, err)
	return c
}

func TestDivergenceModeWritesAndReadsSessionScopedMode(t *testing.T) { // divergence.test.ts:20
	cwd := t.TempDir()
	mode, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "s1", Active: true, CollapsePoint: CollapseD, Reason: "flat objective metric", Now: divClock("2026-07-01T00:00:00.000Z")})
	metricsMust(t, err)
	if got, ok := ReadDivergenceMode(cwd, "s1"); !mode.Active || !ok || got != mode || got.CollapsePoint != CollapseD {
		t.Errorf("wrote %+v, read %+v %v", mode, got, ok)
	}
	if got, ok := ReadDivergenceMode(cwd, "other"); ok {
		t.Errorf("another session read %+v", got)
	}
}

func TestCandidateArchiveRequiresSourceProvenanceAndFiltersBySession(t *testing.T) { // divergence.test.ts:38
	cwd := t.TempDir()
	if _, err := RecordDivergenceCandidate(cwd, CandidateInput{SessionID: "s1", Kind: KindStrong1, Title: "No source", Rationale: "memory only"}); err == nil || !strings.Contains(err.Error(), "source URL") {
		t.Fatalf("an unsourced candidate gave %v", err)
	}
	divAdd(t, cwd, CandidateInput{SessionID: "s1", Kind: KindStrong1, Title: "Alpha", Rationale: "primary path", SourceURLs: []string{"https://example.com/a", "https://example.com/a"}, Now: divClock("2026-07-01T00:01:00.000Z")})
	divAdd(t, cwd, CandidateInput{SessionID: "s2", Kind: KindAdd1, Title: "Beta", Rationale: "alternative", SourceURLs: []string{"https://example.com/b"}})
	if s1 := ReadDivergenceCandidates(cwd, "s1"); len(s1) != 1 || !reflect.DeepEqual(s1[0].SourceURLs, []string{"https://example.com/a"}) {
		t.Errorf("s1 holds %+v", s1)
	}
	if got := len(ReadDivergenceCandidates(cwd, "")); got != 2 {
		t.Errorf("%d candidates in all, want 2", got)
	}
}

func TestCandidateArchiveRoundtripsLoopMetadataAndToleratesLegacyRows(t *testing.T) { // divergence.test.ts:76
	cwd := t.TempDir()
	divAdd(t, cwd, CandidateInput{SessionID: "s1", Kind: KindStrong1, Title: "State redesign", Rationale: "widen state representation", SourceURLs: []string{"https://example.com/state"},
		Status: divPtr(StatusDiscarded), ChangeClass: divPtr(ChangeStateSpaceRedesign), KilledAtPhase: divPtr(PhaseD), Now: divClock("2026-07-01T00:01:00.000Z")})
	f, err := os.OpenFile(candidatesPath(cwd), os.O_WRONLY|os.O_APPEND, 0)
	metricsMust(t, err)
	_, err = f.WriteString("{\"ts\":\"2026-07-01T00:00:00.000Z\",\"sessionId\":\"s1\",\"id\":\"legacy\",\"kind\":\"add-1\",\"title\":\"Legacy\",\"rationale\":\"old row\",\"sourceUrls\":[\"https://example.com/legacy\"],\"status\":\"discarded\"}\n")
	metricsMust(t, errors.Join(err, f.Close()))
	got := ReadDivergenceCandidates(cwd, "s1")
	if len(got) != 2 || got[0].ChangeClass != ChangeStateSpaceRedesign || got[0].KilledAtPhase != PhaseD || got[1].Title != "Legacy" || got[1].ChangeClass != "" || got[1].KilledAtPhase != "" {
		t.Errorf("read %+v", got)
	}
}

func TestDiscardStreakReportsTrailingDiscardedSameClassRun(t *testing.T) { // divergence.test.ts:119
	row := func(ts string, status CandidateStatus, class CandidateChangeClass) DivergenceCandidate {
		return DivergenceCandidate{TS: ts, SessionID: "s1", Kind: KindStrong1, Rationale: "r", SourceURLs: []string{"https://example.com"}, Status: status, ChangeClass: class}
	}
	for name, c := range map[string]struct {
		rows []DivergenceCandidate
		want Streak
	}{
		"none":       {nil, Streak{}},
		"kept last":  {[]DivergenceCandidate{row("2026-07-01T00:00:00.000Z", StatusDiscarded, ChangeParameterTweak), row("2026-07-01T00:01:00.000Z", StatusKept, ChangeParameterTweak)}, Streak{}},
		"unsorted":   {[]DivergenceCandidate{row("2026-07-01T00:03:00.000Z", StatusDiscarded, ChangeParameterTweak), row("2026-07-01T00:01:00.000Z", StatusDiscarded, ChangeBranchToggle), row("2026-07-01T00:02:00.000Z", StatusDiscarded, ChangeParameterTweak), row("2026-07-01T00:00:00.000Z", StatusDiscarded, ChangeBranchToggle)}, Streak{ChangeParameterTweak, 2}},
		"whole list": {[]DivergenceCandidate{row("2026-07-01T00:00:00.000Z", StatusDiscarded, ChangeParameterTweak), row("2026-07-01T00:01:00.000Z", StatusDiscarded, ChangeParameterTweak), row("2026-07-01T00:02:00.000Z", StatusDiscarded, ChangeParameterTweak)}, Streak{ChangeParameterTweak, 3}},
	} {
		if got := DiscardStreak(c.rows); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}

func TestDivergenceDefaultClockWritesIsoTimestamps(t *testing.T) {
	cwd := t.TempDir()
	mode, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "s", Active: true, CollapsePoint: CollapseP})
	metricsMust(t, err)
	c := divAdd(t, cwd, CandidateInput{SessionID: "s", Kind: KindAlternative, Title: "t", SourceURLs: []string{"u"}})
	_, modeErr := time.Parse(timestampLayout, mode.UpdatedAt)
	_, rowErr := time.Parse(timestampLayout, c.TS)
	if modeErr != nil || rowErr != nil {
		t.Errorf("updatedAt %q, ts %q", mode.UpdatedAt, c.TS)
	}
}

func TestRecordCandidateStartsANewLineAfterALedgerItCanNotReadToCheck(t *testing.T) {
	cwd := t.TempDir()
	row := "{\"ts\":\"t\",\"sessionId\":\"s\",\"id\":\"i\",\"kind\":\"add-1\",\"title\":\"t\",\"rationale\":\"r\",\"sourceUrls\":[\"u\"],\"status\":\"kept\"}"
	metricsWrite(t, cwd, DivergenceDir+"/"+CandidatesFile, []byte(row)) // a valid final row without its newline
	metricsUnreadable(t, candidatesPath(cwd))
	_, err := RecordDivergenceCandidate(cwd, CandidateInput{SessionID: "s", Kind: KindAdd1, Title: "next", SourceURLs: []string{"u"}})
	metricsMust(t, os.Chmod(candidatesPath(cwd), 0o600))
	if rows := ReadDivergenceCandidates(cwd, "s"); err != nil || len(rows) != 2 {
		t.Errorf("record error %v, %d rows read after the append, want 2", err, len(rows))
	}
}

func TestRecordCandidatesAtOnceKeepsEveryRow(t *testing.T) {
	cwd, start := t.TempDir(), make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 20; i++ {
				if _, err := RecordDivergenceCandidate(cwd, CandidateInput{SessionID: "s", Kind: KindAdd1, Title: fmt.Sprintf("w%d-%d", w, i), SourceURLs: []string{"u"}}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := len(ReadDivergenceCandidates(cwd, "s")); got != 320 {
		t.Errorf("%d rows read, want 320", got)
	}
}

func TestWriteDivergenceModeRemovesItsTempFileWhenTheRenameFails(t *testing.T) {
	cwd, boom := t.TempDir(), errors.New("rename failed")
	_, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "s", Active: true, CollapsePoint: CollapseD})
	metricsMust(t, err)
	before, _ := os.ReadFile(modePath(cwd, "s"))
	_, err = writeDivergenceMode(cwd, ModeInput{SessionID: "s", CollapsePoint: CollapseP}, time.Now(), func(string, string) error { return boom })
	left, _ := filepath.Glob(filepath.Join(divergenceDir(cwd), "*.tmp"))
	if after, _ := os.ReadFile(modePath(cwd, "s")); !errors.Is(err, boom) || len(left) != 0 || string(after) != string(before) {
		t.Errorf("err %v, temp files %v, final file now %q", err, left, after)
	}
}

func TestWriteDivergenceModeDoesNotReplaceAFileItCanNotRead(t *testing.T) {
	cwd := t.TempDir()
	_, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "s", Active: true, CollapsePoint: CollapseD})
	metricsMust(t, err)
	path := modePath(cwd, "s")
	before, _ := os.ReadFile(path)
	metricsUnreadable(t, path)
	_, refused := WriteDivergenceMode(cwd, ModeInput{SessionID: "s", CollapsePoint: CollapseP})
	metricsMust(t, os.Chmod(path, 0o600))
	if after, _ := os.ReadFile(path); refused == nil || string(after) != string(before) {
		t.Errorf("write error %v, mode file now %q", refused, after)
	}
}

// divRefusesAFIFO runs write and fails the test when it blocks for 10 seconds or does not return an error.
func divRefusesAFIFO(t *testing.T, write func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- write() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was replaced or written")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write blocked on the FIFO")
	}
}

func TestWriteDivergenceModeLeavesANonRegularFileAloneInsteadOfBlockingOnIt(t *testing.T) {
	cwd := t.TempDir()
	metricsMust(t, os.MkdirAll(divergenceDir(cwd), 0o777))
	metricsMust(t, unix.Mkfifo(modePath(cwd, "s"), 0o666))
	divRefusesAFIFO(t, func() error {
		_, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "s", CollapsePoint: CollapseD})
		return err
	})
}

func TestRecordCandidateRefusesAFIFOInsteadOfBlockingOnIt(t *testing.T) {
	for _, withReader := range []bool{false, true} { // with a reader the oracle's append returns, without one it blocks
		cwd := t.TempDir()
		metricsMust(t, os.MkdirAll(divergenceDir(cwd), 0o777))
		metricsMust(t, unix.Mkfifo(candidatesPath(cwd), 0o666))
		if withReader {
			reader, err := os.OpenFile(candidatesPath(cwd), os.O_RDONLY|unix.O_NONBLOCK, 0)
			metricsMust(t, err)
			defer reader.Close()
		}
		divRefusesAFIFO(t, func() error {
			_, err := RecordDivergenceCandidate(cwd, CandidateInput{SessionID: "s", Kind: KindAdd1, Title: "t", SourceURLs: []string{"u"}})
			return err
		})
	}
}

func TestWriteDivergenceModeKeepsAFileNestedTooDeeplyToReadItsOwner(t *testing.T) {
	cwd := t.TempDir()
	deep := "{\"sessionId\":\"a/b\",\"x\":" + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + "}" // valid JSON, beyond the decoder's depth limit
	metricsWrite(t, cwd, DivergenceDir+"/a-b.mode.json", []byte(deep))
	if _, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "a?b", CollapsePoint: CollapseD}); err == nil {
		t.Error("a session replaced the file of an owner it could not read")
	}
	if raw, _ := os.ReadFile(modePath(cwd, "a-b")); string(raw) != deep {
		t.Errorf("mode file now %d bytes", len(raw))
	}
}

func TestWriteDivergenceModeDoesNotFollowASymlinkAtItsTempName(t *testing.T) {
	cwd, victim, wall := t.TempDir(), filepath.Join(t.TempDir(), "victim"), time.UnixMilli(1_790_000_000_000)
	metricsMust(t, os.WriteFile(victim, []byte("keep"), 0o666))
	metricsMust(t, os.MkdirAll(divergenceDir(cwd), 0o777))
	tmp := fmt.Sprintf("%s.%d.%d.tmp", modePath(cwd, "s"), os.Getpid(), wall.UnixMilli())
	metricsMust(t, os.Symlink(victim, tmp))
	_, err := writeDivergenceMode(cwd, ModeInput{SessionID: "s", CollapsePoint: CollapseD}, wall, crwdir.Rename)
	if got, _ := os.ReadFile(victim); err == nil || string(got) != "keep" {
		t.Errorf("write error %v, the file the link points at now holds %q", err, got)
	}
	if _, lerr := os.Lstat(tmp); lerr != nil {
		t.Errorf("the entry that was already there was removed: %v", lerr)
	}
}

func TestWriteDivergenceModeKeepsTheFileOfAnOwnerItCanNotTellApart(t *testing.T) {
	cwd := t.TempDir()
	owned := "{\"sessionId\": \"a\\ud800b\", \"active\": true}" // a lone surrogate, which a Go string reads as U+FFFD
	metricsWrite(t, cwd, DivergenceDir+"/a-b.mode.json", []byte(owned))
	if _, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "a\ufffdb", CollapsePoint: CollapseD}); err == nil {
		t.Error("a session replaced the file of an owner it can not tell apart from itself")
	}
	if raw, _ := os.ReadFile(modePath(cwd, "a-b")); string(raw) != owned {
		t.Errorf("mode file now %q", raw)
	}
}

func TestWriteDivergenceModeOfAliasedSessionsAtOnceKeepsExactlyOneFile(t *testing.T) {
	for round := 0; round < 20; round++ {
		cwd, start, winners := t.TempDir(), make(chan struct{}), make(chan string, 8)
		var wg sync.WaitGroup
		for _, id := range []string{"a/b", "a?b", "a b", "a:b", "a*b", "a|b", "a<b", "a>b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := WriteDivergenceMode(cwd, ModeInput{SessionID: id, Active: true, CollapsePoint: CollapseD}); err == nil {
					winners <- id
				}
			}()
		}
		close(start)
		wg.Wait()
		close(winners)
		var won []string
		for id := range winners {
			won = append(won, id)
		}
		raw, _ := os.ReadFile(modePath(cwd, "a-b"))
		if len(won) != 1 || !strings.Contains(string(raw), rowQuote(won[0])) {
			t.Fatalf("round %d: writes that succeeded: %q; mode file %s", round, won, raw)
		}
	}
}

// divOp is one step of a scenario in testdata/scenarios-divergence.json, divAnswer what the oracle answered: the text JSON.stringify
// gave each result, and the files the run left under the state directory. A scenario with Changed is an oracle case the port
// intentionally answers differently: its Go answers are written out there, with the reason.
type divOp struct {
	Op, Session, Collapse, Reason, Now, ID, Kind, Title, Rationale, Worktree, MetricName, Note string
	Status, ChangeClass, KilledAtPhase                                                         *string
	Path, Target, Text, B64                                                                    string
	Active                                                                                     bool
	URLs                                                                                       []string
	MetricValue                                                                                any
	Rows                                                                                       []struct{ TS, Status, ChangeClass string }
}

type divAnswer struct {
	Results []string
	Files   map[string]string
}

type divScenario struct {
	ID      string
	Capture bool
	Changed *struct {
		Reason string
		divAnswer
	}
	Ops []divOp
}

// divStep runs one op against the port and answers as the recorder does.
func divStep(t *testing.T, cwd string, op divOp) string {
	t.Helper()
	failed := func(err error) string {
		if msg := err.Error(); strings.HasPrefix(msg, "divergence candidate") {
			return "{\"error\":" + rowQuote(msg) + "}"
		}
		return "{\"error\":true}"
	}
	switch op.Op {
	case "mode":
		mode, err := WriteDivergenceMode(cwd, ModeInput{op.Session, op.Active, CollapsePoint(op.Collapse), op.Reason, divClock(op.Now)})
		if err != nil {
			return failed(err)
		}
		return EncodeMode(mode)
	case "mode-read":
		if mode, ok := ReadDivergenceMode(cwd, op.Session); ok {
			return EncodeMode(mode)
		}
		return "null"
	case "cand":
		in := CandidateInput{SessionID: op.Session, ID: op.ID, Kind: CandidateKind(op.Kind), Title: op.Title, Rationale: op.Rationale, SourceURLs: op.URLs, Status: divEnum[CandidateStatus](op.Status),
			Worktree: op.Worktree, MetricName: op.MetricName, Note: op.Note, ChangeClass: divEnum[CandidateChangeClass](op.ChangeClass), KilledAtPhase: divEnum[CandidateKilledAtPhase](op.KilledAtPhase), Now: divClock(op.Now)}
		if op.MetricValue != nil {
			v := metricsNumber(op.MetricValue)
			in.MetricValue = &v
		}
		c, err := RecordDivergenceCandidate(cwd, in)
		if err != nil {
			return failed(err)
		}
		return EncodeCandidate(c)
	case "cand-read":
		rows := []string{}
		for _, c := range ReadDivergenceCandidates(cwd, op.Session) {
			rows = append(rows, EncodeCandidate(c))
		}
		return "[" + strings.Join(rows, ",") + "]"
	case "streak":
		var rows []DivergenceCandidate
		for _, r := range op.Rows {
			rows = append(rows, DivergenceCandidate{TS: r.TS, Status: CandidateStatus(r.Status), ChangeClass: CandidateChangeClass(r.ChangeClass)})
		}
		class, streak := "null", DiscardStreak(rows)
		if streak.ChangeClass != "" {
			class = rowQuote(string(streak.ChangeClass))
		}
		return fmt.Sprintf("{\"changeClass\":%s,\"length\":%d}", class, streak.Length)
	case "file":
		data := []byte(op.Text)
		if op.B64 != "" {
			var err error
			if data, err = base64.StdEncoding.DecodeString(op.B64); err != nil {
				t.Fatal(err)
			}
		}
		metricsWrite(t, cwd, op.Path, data)
		return "null"
	case "symlink":
		link := filepath.Join(cwd, crwdir.DirName, op.Path)
		metricsMust(t, os.MkdirAll(filepath.Dir(link), 0o777))
		metricsMust(t, os.Symlink(op.Target, link))
		return "null"
	case "dir":
		metricsMust(t, os.MkdirAll(filepath.Join(cwd, crwdir.DirName, op.Path), 0o777))
		return "null"
	}
	t.Fatalf("unknown op %q", op.Op)
	return ""
}

// divFiles is what the run left under the state directory but its ignore file, by relative path, read as Node reads a utf8 file.
func divFiles(t *testing.T, cwd string) map[string]string {
	t.Helper()
	root, files := filepath.Join(cwd, crwdir.DirName), map[string]string{}
	metricsMust(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() == ".gitignore" {
			return err
		}
		raw, err := os.ReadFile(path)
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = ledgerUTF8(raw)
		return err
	}))
	return files
}

func TestDivergenceMatchesTheRecordedOracle(t *testing.T) {
	var scenarios []divScenario
	var oracle map[string]divAnswer
	for name, into := range map[string]any{"scenarios-divergence.json": &scenarios, "oracle-divergence.json": &oracle} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		metricsMust(t, err)
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(oracle) != len(scenarios) {
		t.Fatalf("%d scenarios, %d recorded answers", len(scenarios), len(oracle))
	}
	for _, sc := range scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			cwd, got := t.TempDir(), divAnswer{}
			for _, op := range sc.Ops {
				got.Results = append(got.Results, divStep(t, cwd, op))
			}
			if sc.Capture {
				got.Files = divFiles(t, cwd)
			}
			want := oracle[sc.ID]
			if c := sc.Changed; c != nil {
				if c.Reason == "" || reflect.DeepEqual(c.divAnswer, want) {
					t.Fatal("an intentionally-changed case needs a reason and an answer that differs from the oracle's")
				}
				want = c.divAnswer
			}
			if len(got.Results) != len(want.Results) {
				t.Fatalf("%d answers, want %d", len(got.Results), len(want.Results))
			}
			for i := range want.Results {
				if got.Results[i] != want.Results[i] {
					t.Errorf("op %d (%s): %s, want %s", i, sc.Ops[i].Op, got.Results[i], want.Results[i])
				}
			}
			if len(got.Files)+len(want.Files) > 0 && !reflect.DeepEqual(got.Files, want.Files) {
				t.Errorf("files %q, want %q", got.Files, want.Files)
			}
		})
	}
}
