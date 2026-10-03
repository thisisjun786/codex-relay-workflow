package job

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// Expectations marked "oracle" are what Node v24 printed running the unmodified CXC v0.2.40 bg-wake/src/store.ts in a scratch
// workspace; the confinement cases have no oracle counterpart, they are the deliberate security difference.

func workspace(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func put(t *testing.T, path, text string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o777); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func store(t *testing.T, cwd string) string {
	t.Helper()
	dir, err := EnsureDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range list {
		out = append(out, e.Name())
	}
	return out
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestPathsLayout(t *testing.T) {
	cwd := "/w"
	for name, c := range map[string][2]string{
		"bgDir":        {BGDir(cwd), "/w/.crw/bg"},
		"recordPath":   {RecordPath(cwd, "bg1"), "/w/.crw/bg/bg1.json"},
		"outPath":      {OutPath(cwd, "bg1"), "/w/.crw/bg/bg1.out"},
		"exitPath":     {ExitPath(cwd, "bg1"), "/w/.crw/bg/bg1.exit"},
		"disabledPath": {DisabledPath(cwd), "/w/.crw/bg/disabled"},
		"enabledAt":    {EnabledAtPath(cwd), "/w/.crw/bg/enabled-at"},
		// path.join cleans, so an id climbs out and a separator nests (oracle paths[6], paths[7]).
		"climbing id": {RecordPath(cwd, "../../x"), "/w/x.json"},
		"nested id":   {ExitPath(cwd, "a/b"), "/w/.crw/bg/a/b.exit"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
}

func TestEnsureDirCreatesTheCrwDirectoryThenBgAndFailsOverAFile(t *testing.T) {
	cwd := workspace(t)
	dir, err := EnsureDir(cwd)
	if err != nil || dir != filepath.Join(cwd, ".crw", "bg") {
		t.Fatalf("EnsureDir = %q, %v", dir, err)
	}
	if got := get(t, filepath.Join(cwd, ".crw", ".gitignore")); got != crwdir.GitignoreText {
		t.Errorf(".gitignore = %q", got)
	}
	if again, err := EnsureDir(cwd); err != nil || again != dir {
		t.Errorf("second EnsureDir = %q, %v", again, err)
	}
	cwd = workspace(t)
	put(t, filepath.Join(cwd, ".crw", "bg"), "a file")
	if dir, err := EnsureDir(cwd); err == nil || dir != "" {
		t.Errorf("bg is a file: %q, %v", dir, err)
	}
}

func TestAtomicWritePublishesWholeFilesAndLeavesNoTmp(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	path := RecordPath(cwd, "r")
	for _, text := range []string{"one\n", "two\n"} {
		if err := AtomicWrite(cwd, path, text); err != nil {
			t.Fatal(err)
		}
	}
	if get(t, path) != "two\n" || !slices.Equal(names(t, dir), []string{"r.json"}) {
		t.Errorf("content %q, directory %v", get(t, path), names(t, dir))
	}
	// The cleaned path is the one written: sub/.. never reaches the file system.
	if err := AtomicWrite(cwd, dir+"/sub/../y.json", "y"); err != nil || get(t, filepath.Join(dir, "y.json")) != "y" {
		t.Errorf("a path with a dot-dot: %v", err)
	}
}

func TestAtomicWriteTakesARelativeAndAnAliasedWorkspace(t *testing.T) {
	cwd := workspace(t)
	store(t, cwd)
	t.Chdir(cwd)
	if err := AtomicWrite(".", RecordPath(".", "rel"), "relative"); err != nil || get(t, RecordPath(cwd, "rel")) != "relative" {
		t.Errorf("relative cwd: %v", err)
	}
	alias := filepath.Join(workspace(t), "alias")
	symlink(t, cwd, alias)
	if err := AtomicWrite(alias, RecordPath(alias, "al"), "alias"); err != nil || get(t, RecordPath(cwd, "al")) != "alias" {
		t.Errorf("aliased cwd: %v", err)
	}
}

func TestAtomicWriteFollowsAStoreLinkedInsideTheWorkspace(t *testing.T) {
	cwd := workspace(t)
	mkdir(t, filepath.Join(cwd, "elsewhere"))
	mkdir(t, filepath.Join(cwd, ".crw"))
	symlink(t, filepath.Join(cwd, "elsewhere"), filepath.Join(cwd, ".crw", "bg"))
	if err := AtomicWrite(cwd, RecordPath(cwd, "r"), "x"); err != nil || get(t, filepath.Join(cwd, "elsewhere", "r.json")) != "x" {
		t.Errorf("err = %v", err)
	}
}

// Oracle atomicDir: with a directory at the final path the write succeeds, the rename fails (EISDIR) and the temporary file stays.
func TestAtomicWriteLeavesTheTmpWhenTheRenameFails(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	final := RecordPath(cwd, "isdir")
	mkdir(t, final)
	if err := atomicWrite(cwd, final, "text", 42, 7); err == nil {
		t.Fatal("the rename over a directory succeeded")
	}
	if got := names(t, dir); !slices.Equal(got, []string{"isdir.json", "isdir.json.tmp-42-7"}) {
		t.Fatalf("directory %v", got)
	}
	if get(t, final+".tmp-42-7") != "text" {
		t.Error("the leaked tmp does not hold the text")
	}
}

// A taken tmp name (a concurrent write in the same millisecond, a planted entry) is never written through: the write takes the next.
func TestAtomicWriteTakesAnotherNameWhenTheTmpIsTaken(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	target := filepath.Join(workspace(t), "target")
	put(t, target, "untouched")
	path := RecordPath(cwd, "r")
	put(t, path+".tmp-42-7", "another writer")
	symlink(t, target, path+".tmp-42-7-1")
	if err := atomicWrite(cwd, path, "payload", 42, 7); err != nil || get(t, path) != "payload" {
		t.Fatalf("err = %v", err)
	}
	if get(t, target) != "untouched" || get(t, path+".tmp-42-7") != "another writer" || len(names(t, dir)) != 3 {
		t.Errorf("a taken name was written through: target %q, directory %v", get(t, target), names(t, dir))
	}
}

// Oracle readTextOrNull: Node's utf8 decoder gives one U+FFFD per maximal invalid subpart.
func TestReadTextDecodesLikeNode(t *testing.T) {
	dir := workspace(t)
	put(t, filepath.Join(dir, "inv"), "a\xffb\xe2\x82c\xc0\x80\xf0\x9f\x98A\xed\xa0\x80")
	got, ok := ReadText(filepath.Join(dir, "inv"))
	if want := "a\ufffdb\ufffdc\ufffd\ufffd\ufffdA\ufffd\ufffd\ufffd"; !ok || got != want {
		t.Errorf("ReadText = %q, %v", got, ok)
	}
}

// Oracle readJson: only a non-null object or array is a result.
func TestReadJSONKeepsOnlyObjectsAndArrays(t *testing.T) {
	dir := workspace(t)
	mkdir(t, filepath.Join(dir, "dir"))
	for _, c := range []struct {
		name, text string
		keep       bool
	}{
		{"obj", "{}", true}, {"arr", "[]", true}, {"ws", "  {\"a\":1}\n", true},
		{"nested", "{\"a\":[1,2,{\"b\":null}]}", true}, {"dup", "{\"a\":1,\"a\":2}", true}, {"lone", "{\"a\":\"\\ud800\"}", true},
		{"null", "null", false}, {"num", "1", false}, {"str", "\"s\"", false}, {"true", "true", false}, {"bad", "{", false},
		{"empty", "", false}, {"bom", "\ufeff{}", false}, {"trailing", "{} x", false},
	} {
		put(t, filepath.Join(dir, c.name), c.text)
		got, ok := ReadJSON(filepath.Join(dir, c.name))
		if ok != c.keep || (ok && string(got) != c.text) {
			t.Errorf("%s: %q, %v", c.name, got, ok)
		}
	}
	for _, name := range []string{"missing", "dir"} {
		if got, ok := ReadJSON(filepath.Join(dir, name)); ok || got != nil {
			t.Errorf("%s: %q, %v", name, got, ok)
		}
	}
}

func ledger(t *testing.T, cwd string) []string {
	t.Helper()
	text := get(t, filepath.Join(BGDir(cwd), LedgerFile))
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("the last row has no newline: %q", text)
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func at(s string) func() time.Time {
	return func() time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			panic(err)
		}
		return v
	}
}

// Oracle ledger[0..3]. The time is Date.prototype.toISOString: UTC with three fraction digits, even when they are zeros.
func TestAppendLedgerWritesNodesBytes(t *testing.T) {
	cwd := workspace(t)
	rows := []Event{
		{{"event", "registered"}, {"id", "bg1"}, {"pid", 42}, {"command", []string{"sh", "-c", "echo <&>"}}},
		{{"event", "completed"}, {"id", "bg1"}, {"exitCode", nil}},
		{{"event", "x"}, {"s", "q\"b\\ \n\r\b\f\t\x01\u2028\u2029 \x7f é 😀"}, {"num", 1.5}, {"big", 1e21}, {"small", 1e-7},
			{"neg0", math.Copysign(0, -1)}, {"nan", math.NaN()}, {"inf", math.Inf(1)}, {"t", true}, {"f", false},
			{"arr", []any{1, "a", nil, []any{2}, int64(-3)}}, {"obj", Event{{"k", "v"}}}, {"empty", Event{}}, {"none", []string{}},
			{"bad", "a\xffb"}, {"i", int64(9007199254740993)}, {"j", -3}},
	}
	for i, row := range rows {
		if err := appendLedger(cwd, row, at("2026-10-03T01:02:03Z")); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	want := []string{
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"event\":\"registered\",\"id\":\"bg1\",\"pid\":42,\"command\":[\"sh\",\"-c\",\"echo <&>\"]}",
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"event\":\"completed\",\"id\":\"bg1\",\"exitCode\":null}",
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"event\":\"x\",\"s\":\"q\\\"b\\\\ \\n\\r\\b\\f\\t\\u0001\u2028\u2029 \x7f é 😀\",\"num\":1.5,\"big\":1e+21,\"small\":1e-7," +
			"\"neg0\":0,\"nan\":null,\"inf\":null,\"t\":true,\"f\":false,\"arr\":[1,\"a\",null,[2],-3],\"obj\":{\"k\":\"v\"},\"empty\":{},\"none\":[],\"bad\":\"a\ufffdb\",\"i\":9007199254740992,\"j\":-3}",
	}
	if got := ledger(t, cwd); !slices.Equal(got, want) {
		t.Errorf("rows:\n%q\nwant\n%q", got, want)
	}
}

// Oracle ledger[1]: an event's own "at" replaces the value and the key stays first (registry.ts:245). Repeated keys follow a
// JavaScript object: the later value, the first position.
func TestAppendLedgerAtAndRepeatedKeys(t *testing.T) {
	cwd := workspace(t)
	for _, row := range []Event{
		{{"event", "adopted"}, {"id", "bg1"}, {"sessionId", "S1"}, {"at", "FIXED"}},
		{{"a", 1}, {"b", 2}, {"a", 3}, {"o", Event{{"x", 1}, {"y", 2}, {"x", 3}}}},
	} {
		if err := appendLedger(cwd, row, at("2026-10-03T01:02:03Z")); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"{\"at\":\"FIXED\",\"event\":\"adopted\",\"id\":\"bg1\",\"sessionId\":\"S1\"}",
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"a\":3,\"b\":2,\"o\":{\"x\":3,\"y\":2}}",
	}
	if got := ledger(t, cwd); !slices.Equal(got, want) {
		t.Errorf("rows %q", got)
	}
}

// Oracle ledger[3] "lone": a string the oracle held as a lone surrogate cannot be a Go string, so a caller passes its raw token. A raw
// value is written compact, on one line, as JSON.stringify would write it.
func TestAppendLedgerWritesARawJSONValueCompact(t *testing.T) {
	cwd := workspace(t)
	row := Event{{"lone", json.RawMessage("\"\\ud800\"")}, {"o", json.RawMessage("{\n  \"x\": [1, 2]\n}")}}
	if err := appendLedger(cwd, row, at("2026-10-03T01:02:03Z")); err != nil {
		t.Fatal(err)
	}
	if got := ledger(t, cwd); !slices.Equal(got, []string{"{\"at\":\"2026-10-03T01:02:03.000Z\",\"lone\":\"\\ud800\",\"o\":{\"x\":[1,2]}}"}) {
		t.Errorf("rows %q", got)
	}
	for _, bad := range []string{"{", "", "\"\xff\""} {
		if err := appendLedger(cwd, Event{{"bad", json.RawMessage(bad)}}, at("2026-10-03T01:02:03Z")); err == nil || len(ledger(t, cwd)) != 1 {
			t.Errorf("raw value %q: %v", bad, err)
		}
	}
}

func TestAppendLedgerCreatesTheStoreFromAnyNameOfTheWorkspace(t *testing.T) {
	for _, name := range []string{"absolute", ".", "", "child"} {
		ws := workspace(t)
		mkdir(t, filepath.Join(ws, "child"))
		t.Chdir(ws)
		cwd, root := name, ws
		if name == "absolute" {
			cwd = ws
		} else if name == "child" {
			root = filepath.Join(ws, "child")
		}
		AppendLedger(cwd, Event{{"event", "disabled"}})
		AppendLedger(cwd, Event{{"event", "enabled"}})
		rows := ledger(t, root)
		if len(rows) != 2 || !strings.HasPrefix(rows[0], "{\"at\":\"") || !strings.HasSuffix(rows[0], "Z\",\"event\":\"disabled\"}") {
			t.Errorf("%q: rows %q", name, rows)
		}
		if got := get(t, filepath.Join(root, ".crw", ".gitignore")); got != crwdir.GitignoreText {
			t.Errorf("%q: .gitignore = %q", name, got)
		}
	}
}

// Oracle ledgerFailOpen: when the store cannot be made nothing is thrown and nothing is written. The seam shows the failure.
func TestAppendLedgerFailsOpen(t *testing.T) {
	cwd := workspace(t)
	put(t, filepath.Join(cwd, ".crw", "bg"), "a file")
	AppendLedger(cwd, Event{{"event", "x"}})
	if err := appendLedger(cwd, Event{{"event", "x"}}, time.Now); err == nil {
		t.Error("the seam did not report the failure")
	}
	if get(t, filepath.Join(cwd, ".crw", "bg")) != "a file" {
		t.Error("the file at bg changed")
	}
	cwd = workspace(t)
	dir := store(t, cwd)
	if err := appendLedger(cwd, Event{{"event", "x"}, {"v", struct{}{}}}, time.Now); err == nil {
		t.Error("an unsupported value was written")
	}
	cycle := Event{{"event", "x"}, {"self", nil}}
	cycle[1].Value = cycle
	if err := appendLedger(cwd, cycle, time.Now); err == nil {
		t.Error("a value that contains itself was written")
	}
	if got := names(t, dir); len(got) != 0 {
		t.Errorf("a row was started: %v", got)
	}
	deep := Event{{"leaf", 1}}
	for i := 0; i < 300; i++ {
		deep = Event{{"n", deep}}
	}
	if err := appendLedger(cwd, deep, time.Now); err != nil || len(ledger(t, cwd)) != 1 {
		t.Errorf("an event nested 300 deep: %v", err)
	}
}

func TestAppendLedgerNeverFollowsAPlantedLink(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	target := filepath.Join(workspace(t), "target")
	put(t, target, "untouched")
	symlink(t, target, filepath.Join(dir, LedgerFile))
	AppendLedger(cwd, Event{{"event", "x"}})
	if err := appendLedger(cwd, Event{{"event", "x"}}, time.Now); err == nil {
		t.Error("the link was opened")
	}
	if get(t, target) != "untouched" {
		t.Errorf("the link target holds %q", get(t, target))
	}
}

// Oracle listRecordIds: every entry ending in .json (a directory and the bare ".json" included), sorted by UTF-16 code unit, so U+1F600
// comes before U+FFFF.
func TestListRecordIDsSortsByUTF16(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	for _, name := range []string{"b.json", "a.json", "ledger.jsonl", "x.json.tmp-1-2", "😀.json", "\uffff.json", ".json", "disabled", "enabled-at",
		"B.json", "10.json", "9.json", "a.JSON", "é.json"} {
		put(t, filepath.Join(dir, name), "{}")
	}
	mkdir(t, filepath.Join(dir, "dir.json"))
	want := []string{"", "10", "9", "B", "a", "b", "dir", "é", "😀", "\uffff"}
	if got := ListRecordIDs(cwd); !slices.Equal(got, want) {
		t.Errorf("ids %q, want %q", got, want)
	}
}

func TestMtimeMs(t *testing.T) {
	path := filepath.Join(workspace(t), "m")
	put(t, path, "x")
	if err := os.Chtimes(path, time.Unix(1700000000, 123_000_000), time.Unix(1700000000, 123_000_000)); err != nil {
		t.Fatal(err)
	}
	if got, ok := MtimeMs(path); !ok || got != 1700000000123 {
		t.Errorf("MtimeMs = %v, %v", got, ok)
	}
}

// statSync().mtimeMs has no range limit; a nanosecond count since 1970 overflows after the year 2262 and before 1678.
func TestMsOfSpansTheWholeCalendar(t *testing.T) {
	for _, c := range []struct {
		t    time.Time
		want float64
	}{
		{time.Date(2300, 1, 1, 0, 0, 0, 500_000_000, time.UTC), 10413792000500},
		{time.Date(1960, 1, 1, 0, 0, 0, 500_000_000, time.UTC), -315619199500},
		{time.Unix(0, 0), 0},
	} {
		if got := msOf(c.t); got != c.want {
			t.Errorf("msOf(%v) = %v, want %v", c.t, got, c.want)
		}
	}
}

// Oracle removePath: rmSync without recursive and with force. A file or a link goes, a missing path is nothing, a directory stays.
func TestRemovePathRemovesFilesAndLinksOnly(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	outside := filepath.Join(workspace(t), "target")
	put(t, outside, "t")
	put(t, filepath.Join(dir, "f"), "x")
	put(t, filepath.Join(dir, "full", "g"), "y")
	mkdir(t, filepath.Join(dir, "empty"))
	symlink(t, outside, filepath.Join(dir, "link"))
	for _, name := range []string{"f", "missing", "empty", "full", "link"} {
		if err := RemovePath(cwd, filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := names(t, dir); !slices.Equal(got, []string{"empty", "full"}) {
		t.Errorf("survivors %v", got)
	}
	if get(t, outside) != "t" {
		t.Error("the link target was removed")
	}
}

// linkedStore is a workspace whose .crw or .crw/bg is a link to a directory outside it; that directory holds a file named disabled.
func linkedStore(t *testing.T, linked string) (cwd, bg string) {
	t.Helper()
	cwd, outside := workspace(t), workspace(t)
	bg = filepath.Join(outside, "bg")
	put(t, filepath.Join(bg, DisabledFile), "keep")
	if linked == ".crw" {
		symlink(t, outside, filepath.Join(cwd, ".crw"))
		return cwd, bg
	}
	mkdir(t, filepath.Join(cwd, ".crw"))
	symlink(t, bg, filepath.Join(cwd, ".crw", "bg"))
	return cwd, bg
}

// The oracle followed these links: ensureDir made directories there, appendLedger and atomicWrite wrote, and cli.ts:150's
// removePath(disabledPath(cwd)) deleted a file.
func TestAStoreThatLeavesTheWorkspaceIsRefused(t *testing.T) {
	for _, linked := range []string{".crw", ".crw/bg"} {
		cwd, bg := linkedStore(t, linked)
		if dir, err := EnsureDir(cwd); !errors.Is(err, ErrOutsideStore) || dir != "" {
			t.Errorf("%s: EnsureDir = %q, %v", linked, dir, err)
		}
		if err := AtomicWrite(cwd, RecordPath(cwd, "r"), "x"); !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s: AtomicWrite: %v", linked, err)
		}
		if err := RemovePath(cwd, DisabledPath(cwd)); !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s: RemovePath: %v", linked, err)
		}
		if err := appendLedger(cwd, Event{{"event", "x"}}, time.Now); !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s: appendLedger: %v", linked, err)
		}
		if got := names(t, bg); !slices.Equal(got, []string{DisabledFile}) || get(t, filepath.Join(bg, DisabledFile)) != "keep" {
			t.Errorf("%s: the outside directory holds %v", linked, got)
		}
	}
}

func TestWhatIsNotAFileOfTheStoreIsRefused(t *testing.T) {
	cwd, other := workspace(t), workspace(t)
	store(t, cwd)
	store(t, other)
	t.Chdir(workspace(t)) // "x.json" below is relative; a regression must not write into the package directory
	for name, path := range map[string]string{
		"climbing id":             RecordPath(cwd, "../../x"),
		"nested id":               RecordPath(cwd, "sub/id"),
		"another workspace's bg":  RecordPath(other, "victim"),
		"the bg directory itself": BGDir(cwd),
		"the workspace":           filepath.Join(cwd, "x.json"),
		"a relative path":         "x.json",
	} {
		if _, err := os.Lstat(path); err != nil {
			put(t, path, "keep")
		}
		if err := AtomicWrite(cwd, path, "payload"); !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s: AtomicWrite: %v", name, err)
		}
		if err := RemovePath(cwd, path); !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s: RemovePath: %v", name, err)
		}
		if tmps, _ := filepath.Glob(path + ".tmp-*"); len(tmps) != 0 {
			t.Errorf("%s: a tmp was made: %v", name, tmps)
		}
		if info, err := os.Lstat(path); err != nil || (info.Mode().IsRegular() && get(t, path) != "keep") {
			t.Errorf("%s: the target changed: %v", name, err)
		}
	}
	if bare := workspace(t); RemovePath(bare, DisabledPath(bare)) != nil {
		t.Error("a workspace without a store: removal is not nothing")
	}
}
