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

// The byte-level expectations marked "oracle" are what Node v24 printed running the unmodified CXC v0.2.40 bg-wake/src/store.ts in a
// scratch workspace (ensureDir, the path builders, appendLedger, listRecordIds, readJsonOrNull, readTextOrNull, removePath,
// atomicWrite, mtimeMs); the confinement cases have no oracle counterpart, they are the deliberate security difference.

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
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
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
	if BGDirName != "bg" || DisabledFile != "disabled" || EnabledAtFile != "enabled-at" || LedgerFile != "ledger.jsonl" {
		t.Error("a file name constant changed")
	}
}

func TestEnsureDirCreatesTheCrwDirectoryThenBg(t *testing.T) {
	cwd := workspace(t)
	dir, err := EnsureDir(cwd)
	if err != nil || dir != filepath.Join(cwd, ".crw", "bg") {
		t.Fatalf("EnsureDir = %q, %v", dir, err)
	}
	if got := get(t, filepath.Join(cwd, ".crw", ".gitignore")); got != crwdir.GitignoreText {
		t.Errorf(".gitignore = %q", got)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("bg is not a directory: %v", err)
	}
	if again, err := EnsureDir(cwd); err != nil || again != dir {
		t.Errorf("second EnsureDir = %q, %v", again, err)
	}
	if got := names(t, filepath.Join(cwd, ".crw")); !slices.Equal(got, []string{".gitignore", "bg"}) {
		t.Errorf(".crw holds %v", got)
	}
}

func TestEnsureDirLeavesAnExistingCrwDirectoryWithoutGitignore(t *testing.T) {
	cwd := workspace(t)
	if err := os.Mkdir(filepath.Join(cwd, ".crw"), 0o777); err != nil {
		t.Fatal(err)
	}
	store(t, cwd)
	if got := names(t, filepath.Join(cwd, ".crw")); !slices.Equal(got, []string{"bg"}) {
		t.Errorf(".crw holds %v", got)
	}
}

func TestEnsureDirFails(t *testing.T) {
	if _, err := EnsureDir(filepath.Join(workspace(t), "missing")); err == nil {
		t.Error("a workspace that does not exist: no error")
	}
	cwd := workspace(t)
	put(t, filepath.Join(cwd, ".crw", "bg"), "a file")
	if dir, err := EnsureDir(cwd); err == nil || dir != "" {
		t.Errorf("bg is a file: %q, %v", dir, err)
	}
}

func TestEnsureDirRefusesAStoreThatLeavesTheWorkspace(t *testing.T) {
	for _, linked := range []string{".crw", ".crw/bg"} {
		cwd, outside := workspace(t), workspace(t)
		if linked == ".crw" {
			symlink(t, outside, filepath.Join(cwd, ".crw"))
		} else {
			if err := os.Mkdir(filepath.Join(cwd, ".crw"), 0o777); err != nil {
				t.Fatal(err)
			}
			symlink(t, outside, filepath.Join(cwd, ".crw", "bg"))
		}
		if dir, err := EnsureDir(cwd); !errors.Is(err, ErrOutsideStore) || dir != "" {
			t.Errorf("%s -> outside: EnsureDir = %q, %v", linked, dir, err)
		}
		if got := names(t, outside); len(got) != 0 {
			t.Errorf("%s -> outside: created %v there", linked, got)
		}
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
	if err := AtomicWrite(cwd, filepath.Join(dir, "later.json"), "x"); err != nil {
		t.Errorf("a file not made by a builder: %v", err)
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

func TestAtomicWriteWritesTheCleanedPath(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	if err := AtomicWrite(cwd, dir+"/sub/../y.json", "y"); err != nil {
		t.Fatal(err)
	}
	if get(t, filepath.Join(dir, "y.json")) != "y" || !slices.Equal(names(t, dir), []string{"y.json"}) {
		t.Errorf("directory %v", names(t, dir))
	}
}

func TestAtomicWriteRefusesWhatIsNotAFileOfTheStore(t *testing.T) {
	cwd, other := workspace(t), workspace(t)
	store(t, cwd)
	store(t, other)
	for name, path := range map[string]string{
		"climbing id":             RecordPath(cwd, "../../x"),
		"nested id":               RecordPath(cwd, "sub/id"),
		"another workspace's bg":  RecordPath(other, "victim"),
		"the bg directory itself": BGDir(cwd),
		"the workspace":           filepath.Join(cwd, "x.json"),
		"a relative path":         "x.json",
	} {
		err := AtomicWrite(cwd, path, "payload")
		if !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, root := range []string{cwd, other} {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !strings.HasSuffix(p, ".gitignore") {
				t.Errorf("a file was made: %s", p)
			}
			return nil
		})
	}
}

func TestAtomicWriteRefusesAStoreThatLeavesTheWorkspace(t *testing.T) {
	for _, linked := range []string{".crw", ".crw/bg"} {
		cwd, outside := workspace(t), workspace(t)
		put(t, filepath.Join(outside, "bg", "keep"), "keep")
		if linked == ".crw" {
			symlink(t, outside, filepath.Join(cwd, ".crw"))
		} else {
			if err := os.Mkdir(filepath.Join(cwd, ".crw"), 0o777); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join(outside, "bg"), filepath.Join(cwd, ".crw", "bg"))
		}
		if err := AtomicWrite(cwd, RecordPath(cwd, "r"), "x"); !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s -> outside: err = %v", linked, err)
		}
		if got := names(t, filepath.Join(outside, "bg")); !slices.Equal(got, []string{"keep"}) {
			t.Errorf("%s -> outside: the outside directory holds %v", linked, got)
		}
	}
}

func TestAtomicWriteFollowsAStoreLinkedInsideTheWorkspace(t *testing.T) {
	cwd := workspace(t)
	if err := os.MkdirAll(filepath.Join(cwd, "elsewhere"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, ".crw"), 0o777); err != nil {
		t.Fatal(err)
	}
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
	if err := os.Mkdir(final, 0o777); err != nil {
		t.Fatal(err)
	}
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

func TestAtomicWriteNeverFollowsAPlantedTmpLink(t *testing.T) {
	cwd := workspace(t)
	store(t, cwd)
	target := filepath.Join(workspace(t), "target")
	put(t, target, "untouched")
	path := RecordPath(cwd, "r")
	symlink(t, target, path+".tmp-42-7")
	if err := atomicWrite(cwd, path, "payload", 42, 7); err == nil {
		t.Error("a planted tmp link was written through")
	}
	if get(t, target) != "untouched" {
		t.Errorf("the link target holds %q", get(t, target))
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
	put(t, filepath.Join(dir, "bom"), "\ufeff{}\n")
	if got, ok := ReadText(filepath.Join(dir, "bom")); !ok || got != "\ufeff{}\n" {
		t.Errorf("a BOM is kept: %q, %v", got, ok)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir"), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "dir"} {
		if got, ok := ReadText(filepath.Join(dir, name)); ok || got != "" {
			t.Errorf("%s: %q, %v", name, got, ok)
		}
	}
}

// Oracle readJson: only a non-null object or array is a result.
func TestReadJSONKeepsOnlyObjectsAndArrays(t *testing.T) {
	dir := workspace(t)
	if err := os.Mkdir(filepath.Join(dir, "dir"), 0o777); err != nil {
		t.Fatal(err)
	}
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
		{{"event", "x"}, {"s", "q\"b\\ \n\t\x01\u2028\u2029 \x7f é 😀"}, {"num", 1.5}, {"big", 1e21}, {"small", 1e-7},
			{"neg0", math.Copysign(0, -1)}, {"nan", math.NaN()}, {"inf", math.Inf(1)}, {"t", true}, {"f", false},
			{"arr", []any{1, "a", nil, []any{2}, int64(-3)}}, {"obj", Event{{"k", "v"}}}, {"empty", Event{}}, {"none", []string{}},
			{"bad", "a\xffb"}},
	}
	for i, row := range rows {
		if err := appendLedger(cwd, row, at("2026-10-03T01:02:03Z")); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	want := []string{
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"event\":\"registered\",\"id\":\"bg1\",\"pid\":42,\"command\":[\"sh\",\"-c\",\"echo <&>\"]}",
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"event\":\"completed\",\"id\":\"bg1\",\"exitCode\":null}",
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"event\":\"x\",\"s\":\"q\\\"b\\\\ \\n\\t\\u0001\u2028\u2029 \x7f é 😀\",\"num\":1.5,\"big\":1e+21,\"small\":1e-7," +
			"\"neg0\":0,\"nan\":null,\"inf\":null,\"t\":true,\"f\":false,\"arr\":[1,\"a\",null,[2],-3],\"obj\":{\"k\":\"v\"},\"empty\":{},\"none\":[],\"bad\":\"a\ufffdb\"}",
	}
	if got := ledger(t, cwd); !slices.Equal(got, want) {
		t.Errorf("rows:\n%q\nwant\n%q", got, want)
	}
}

func TestAppendLedgerTimestampKeepsMilliseconds(t *testing.T) {
	cwd := workspace(t)
	if err := appendLedger(cwd, Event{{"event", "e"}}, at("2026-10-03T10:20:30.0045+09:00")); err != nil {
		t.Fatal(err)
	}
	if got := ledger(t, cwd)[0]; got != "{\"at\":\"2026-10-03T01:20:30.004Z\",\"event\":\"e\"}" {
		t.Errorf("row %q", got)
	}
}

// Oracle ledger[1]: an event's own "at" replaces the value and the key stays first (registry.ts:245). Repeated keys follow a
// JavaScript object: the later value, the first position.
func TestAppendLedgerAtAndRepeatedKeys(t *testing.T) {
	cwd := workspace(t)
	for _, row := range []Event{
		{{"event", "adopted"}, {"id", "bg1"}, {"sessionId", "S1"}, {"at", "FIXED"}},
		{{"a", 1}, {"b", 2}, {"a", 3}},
	} {
		if err := appendLedger(cwd, row, at("2026-10-03T01:02:03Z")); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"{\"at\":\"FIXED\",\"event\":\"adopted\",\"id\":\"bg1\",\"sessionId\":\"S1\"}",
		"{\"at\":\"2026-10-03T01:02:03.000Z\",\"a\":3,\"b\":2}",
	}
	if got := ledger(t, cwd); !slices.Equal(got, want) {
		t.Errorf("rows %q", got)
	}
}

// Oracle ledger[3] "lone": a string the oracle held as a lone surrogate cannot be a Go string, so a caller passes its raw token.
func TestAppendLedgerWritesARawJSONValueAsItIs(t *testing.T) {
	cwd := workspace(t)
	if err := appendLedger(cwd, Event{{"lone", json.RawMessage(`"\\ud800"`)}}, at("2026-10-03T01:02:03Z")); err != nil {
		t.Fatal(err)
	}
	if got := ledger(t, cwd)[0]; got != "{\"at\":\"2026-10-03T01:02:03.000Z\",\"lone\":\"\\ud800\"}" {
		t.Errorf("row %q", got)
	}
	if err := appendLedger(cwd, Event{{"bad", json.RawMessage("{")}}, at("2026-10-03T01:02:03Z")); err == nil || len(ledger(t, cwd)) != 1 {
		t.Errorf("an invalid raw value: %v", err)
	}
}

func TestAppendLedgerCreatesTheStoreAndAppends(t *testing.T) {
	cwd := workspace(t)
	AppendLedger(cwd, Event{{"event", "disabled"}})
	AppendLedger(cwd, Event{{"event", "enabled"}})
	rows := ledger(t, cwd)
	if len(rows) != 2 || !strings.HasPrefix(rows[0], "{\"at\":\"") || !strings.HasSuffix(rows[0], "Z\",\"event\":\"disabled\"}") {
		t.Errorf("rows %q", rows)
	}
	if got := get(t, filepath.Join(cwd, ".crw", ".gitignore")); got != crwdir.GitignoreText {
		t.Errorf(".gitignore = %q", got)
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
	if got := names(t, dir); len(got) != 0 {
		t.Errorf("a row was started: %v", got)
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
	if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0o777); err != nil {
		t.Fatal(err)
	}
	want := []string{"", "10", "9", "B", "a", "b", "dir", "é", "😀", "\uffff"}
	if got := ListRecordIDs(cwd); !slices.Equal(got, want) {
		t.Errorf("ids %q, want %q", got, want)
	}
}

func TestListRecordIDsIsEmptyWithoutAStore(t *testing.T) {
	cwd := workspace(t)
	if got := ListRecordIDs(cwd); got == nil || len(got) != 0 {
		t.Errorf("no .crw: %#v", got)
	}
	put(t, filepath.Join(cwd, ".crw", "bg"), "a file")
	if got := ListRecordIDs(cwd); got == nil || len(got) != 0 {
		t.Errorf("bg is a file: %#v", got)
	}
}

func TestListRecordIDsDecodesNamesLikeNode(t *testing.T) {
	cwd := workspace(t)
	dir := store(t, cwd)
	if err := os.WriteFile(filepath.Join(dir, "bad\xff.json"), []byte("{}"), 0o666); err != nil {
		t.Skipf("the file system does not take a name that is not UTF-8: %v", err)
	}
	if got := ListRecordIDs(cwd); !slices.Equal(got, []string{"bad\ufffd"}) {
		t.Errorf("ids %q", got)
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
	if got, ok := MtimeMs(path + "-missing"); ok || got != 0 {
		t.Errorf("a missing file: %v, %v", got, ok)
	}
	if got, ok := MtimeMs(filepath.Dir(path)); !ok || got <= 0 {
		t.Errorf("a directory: %v, %v", got, ok)
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
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o777); err != nil {
		t.Fatal(err)
	}
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

func TestRemovePathRefusesWhatIsNotAFileOfTheStore(t *testing.T) {
	cwd, other := workspace(t), workspace(t)
	store(t, cwd)
	store(t, other)
	victims := map[string]string{
		"climbing id":            RecordPath(cwd, "../../victim"),
		"another workspace's bg": RecordPath(other, "victim"),
		"the workspace":          filepath.Join(cwd, "victim.json"),
	}
	for name, path := range victims {
		put(t, path, "keep")
		if err := RemovePath(cwd, path); !errors.Is(err, ErrOutsideStore) {
			t.Errorf("%s: err = %v", name, err)
		}
		if get(t, path) != "keep" {
			t.Errorf("%s: the file was removed", name)
		}
	}
	if err := RemovePath(cwd, BGDir(cwd)); !errors.Is(err, ErrOutsideStore) {
		t.Errorf("the bg directory: %v", err)
	}
}

// The oracle's own call removePath(disabledPath(cwd)) (cli.ts:150) with .crw/bg pointing out of the workspace.
func TestRemovePathRefusesAStoreThatLeavesTheWorkspace(t *testing.T) {
	cwd, outside := workspace(t), workspace(t)
	put(t, filepath.Join(outside, DisabledFile), "keep")
	if err := os.Mkdir(filepath.Join(cwd, ".crw"), 0o777); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, filepath.Join(cwd, ".crw", "bg"))
	if err := RemovePath(cwd, DisabledPath(cwd)); !errors.Is(err, ErrOutsideStore) {
		t.Errorf("err = %v", err)
	}
	if get(t, filepath.Join(outside, DisabledFile)) != "keep" {
		t.Error("the file outside the workspace was removed")
	}
}

func TestRemovePathWithoutAStoreIsNothing(t *testing.T) {
	cwd := workspace(t)
	if err := RemovePath(cwd, DisabledPath(cwd)); err != nil {
		t.Errorf("no .crw: %v", err)
	}
}
