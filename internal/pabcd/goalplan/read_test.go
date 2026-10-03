package goalplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const readTestPlan = `{"objective":"o","slug":"demo","workPhases":[],"criteria":[],"host":{"armed":false,"armedAt":null,"source":"none"}}`

func readWorkspace(t *testing.T) (string, string) {
	t.Helper()
	cwd := t.TempDir()
	dir, err := GoalplanDir(cwd, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeReadFile(t, filepath.Join(dir, GoalplanFile), readTestPlan)
	return cwd, dir
}
func writeReadFile(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadRecordedDiagnostics(t *testing.T) {
	raw, err := os.ReadFile("testdata/oracle-read-lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Cases []struct {
			ID             string                        `json:"id"`
			Raw            *string                       `json:"raw"`
			Read           GoalplanReadResult            `json:"read"`
			Lock           struct{ Kind, Reason string } `json:"lock"`
			Classification string                        `json:"classification"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for _, c := range record.Cases {
		if strings.Contains(c.ID, "symlink") || strings.Contains(c.ID, "dangling") {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			cwd, dir := readWorkspace(t)
			path := filepath.Join(dir, GoalplanFile)
			if c.Raw == nil {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeReadFile(t, path, *c.Raw)
			}
			got := ReadGoalplanDetailed(cwd, "demo")
			if c.Classification == "intentionally-changed" {
				if got.Plan != nil || got.Diagnostic == nil || got.Diagnostic.Kind != "unreadable" {
					t.Fatalf("lossy read: %+v", got)
				}
			} else if c.Read.Plan != nil {
				if compact(t, got.Plan) != compact(t, c.Read.Plan) || got.Diagnostic != nil {
					t.Fatalf("read %+v, oracle %+v", got, c.Read)
				}
			} else {
				if got.Plan != nil || got.Diagnostic == nil || got.Diagnostic.Kind != c.Read.Diagnostic.Kind || got.Diagnostic.Field != c.Read.Diagnostic.Field {
					t.Fatalf("read %+v, oracle %+v", got, c.Read)
				}
				if got.Diagnostic.Path != path {
					t.Fatalf("path %q", got.Diagnostic.Path)
				}
				if c.Read.Diagnostic.Kind == "invalid-shape" && got.Diagnostic.Detail != c.Read.Diagnostic.Detail {
					t.Fatal(got.Diagnostic.Detail)
				}
				if got.Diagnostic.Kind != "absent" && got.Diagnostic.Detail == "" {
					t.Fatal("missing detail")
				}
			}
			if compact(t, ReadGoalplan(cwd, "demo")) != compact(t, got.Plan) {
				t.Fatal("wrapper differs")
			}
			lock, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (string, error) { return "entered", nil }, &GoalplanWriteLockOptions{RetryDelaysMs: []int{}})
			if err != nil {
				t.Fatal(err)
			}
			wantKind := c.Lock.Kind
			if c.Classification == "intentionally-changed" {
				wantKind = "unreadable"
			}
			if lock.Kind != wantKind {
				t.Fatalf("lock %+v, oracle %+v", lock, c.Lock)
			}
			if c.Raw != nil {
				b, _ := os.ReadFile(path)
				if string(b) != *c.Raw {
					t.Fatal("reader/lock changed plan bytes")
				}
			}
		})
	}
}

func TestReadStoredPlans(t *testing.T) {
	cwd, dir := readWorkspace(t)
	for name, c := range loadOraclePlans(t).Plans {
		if c.Slug != "rec-plan" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw := strings.ReplaceAll(c.Raw, "rec-plan", "demo")
			writeReadFile(t, filepath.Join(dir, GoalplanFile), raw)
			want := "null"
			if c.Oracle != nil {
				want = strings.ReplaceAll(*c.Oracle, "rec-plan", "demo")
			}
			if got := compact(t, ReadGoalplan(cwd, "demo")); got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		})
	}
}

func TestReadRefusesLinkedPathsAndSpecialFiles(t *testing.T) {
	for _, name := range []string{".crw", ".crw/goalplans", ".crw/goalplans/demo", ".crw/goalplans/demo/goalplan.json", "dangling-file", "dangling-root", "fifo", "directory"} {
		t.Run(name, func(t *testing.T) {
			cwd, dir := readWorkspace(t)
			outside := t.TempDir()
			writeReadFile(t, filepath.Join(outside, "sentinel"), "unchanged")
			path := filepath.Join(cwd, name)
			switch name {
			case "dangling-file", "fifo", "directory":
				path = filepath.Join(dir, GoalplanFile)
			case "dangling-root":
				path = filepath.Join(cwd, ".crw")
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "fifo":
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				target := outside
				if strings.Contains(name, "dangling") {
					target = filepath.Join(outside, "missing")
				}
				if strings.HasSuffix(name, GoalplanFile) {
					target = filepath.Join(outside, "sentinel")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			got := ReadGoalplanDetailed(cwd, "demo")
			if got.Plan != nil || got.Diagnostic == nil || got.Diagnostic.Kind != "unreadable" {
				t.Fatalf("unsafe read: %+v", got)
			}
			b, _ := os.ReadFile(filepath.Join(outside, "sentinel"))
			if string(b) != "unchanged" {
				t.Fatal("outside changed")
			}
		})
	}
	if got := ReadGoalplanDetailed(t.TempDir(), "../escape"); got.Diagnostic == nil || got.Diagnostic.Kind != "unreadable" || got.Diagnostic.Path != "../escape" {
		t.Fatalf("invalid slug %+v", got)
	}
}

func TestReadSurrogateControls(t *testing.T) {
	cwd, dir := readWorkspace(t)
	for _, s := range []string{`"\ud800"`, `"\udc00"`, `"a\ud800z"`, `"\ud800\ud801"`, `"\ud83d\ude00"`, `"\\ud800"`, `"\ufffd"`} {
		writeReadFile(t, filepath.Join(dir, GoalplanFile), strings.Replace(readTestPlan, `"o"`, s, 1))
		got := ReadGoalplanDetailed(cwd, "demo")
		bad := s == `"\ud800"` || s == `"\udc00"` || s == `"a\ud800z"` || s == `"\ud800\ud801"`
		if (got.Plan == nil) != bad {
			t.Fatalf("%s: %+v", s, got)
		}
	}
}
