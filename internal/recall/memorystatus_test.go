package recall

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type memoryStatusOracle struct {
	ID, Mode   string
	SQL, Newer []string
	Status     json.RawMessage
	Observed   []struct {
		Now                  float64
		Text, Notice, Custom string
	}
}

// The recorder seeds only temporary homes. The Go replay does not run Node.
func memoryStatusHome(t *testing.T, c memoryStatusOracle) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	for i, sql := range [][]string{c.SQL, c.Newer} {
		if sql == nil {
			continue
		}
		name := "memories_1.sqlite"
		if i == 1 {
			name = "memories_2.sqlite"
		}
		db, err := openDbReadWrite(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range sql {
			recallSQL(t, db, q)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "memories_1.sqlite")
	if c.Mode == "corrupt" {
		if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c.Mode == "directory" {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func memoryStatusFiles(t *testing.T, home string) map[string]string {
	t.Helper()
	files, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if f.IsDir() {
			out[f.Name()] = "directory"
			continue
		}
		data, err := os.ReadFile(filepath.Join(home, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name()] = string(data)
	}
	return out
}

func TestMemoryStatusOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/memorystatus/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var grid struct {
		Collect  []memoryStatusOracle
		Classify []struct {
			Raw any
			Out string
		}
	}
	if err := json.Unmarshal(data, &grid); err != nil {
		t.Fatal(err)
	}
	if len(grid.Collect) != 49 || len(grid.Classify) != 18 {
		t.Fatal("incomplete oracle grid")
	}
	fixes := portFixed(t, "memorystatus")
	for _, c := range grid.Collect {
		if fix, ok := fixes.lookup(c.ID); ok {
			// port: fixed (docs/port-cxc/known-defects/CRW-1128.md): the port's status and texts in place of the recorded ones.
			var fixed struct {
				Status   json.RawMessage
				Observed []struct {
					Now                  float64
					Text, Notice, Custom string
				}
			}
			if err := json.Unmarshal(fix, &fixed); err != nil {
				t.Fatal(err)
			}
			c.Status = fixed.Status
			if fixed.Observed != nil {
				c.Observed = fixed.Observed
			}
		}
		t.Run(c.ID, func(t *testing.T) {
			home := memoryStatusHome(t, c)
			before := memoryStatusFiles(t, home)
			s := CollectMemoryStatus(home)
			if c.Mode == "directory" {
				// port: fixed (docs/port-cxc/known-defects/CRW-1123.md): a directory named like the store
				// is no store; the oracle tried to open it and reported the open failure.
				if s.State != MemoryStatusUnavailable || s.StorePath != nil || s.Detail != "no memories store found under "+home {
					t.Fatalf("a directory is no memories store: %+v", s)
				}
				return
			}
			gotJSON, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal([]byte(strings.ReplaceAll(string(gotJSON), home, "<HOME>")), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Status, &want); err != nil {
				t.Fatal(err)
			}
			type observation struct {
				Now                  float64
				Text, Notice, Custom string
			}
			observed := []observation{}
			for _, o := range c.Observed {
				observed = append(observed, observation{o.Now, strings.ReplaceAll(FormatMemoryStatus(s, o.Now), home, "<HOME>"), strings.ReplaceAll(MemoryStatusNotice(s, o.Now), home, "<HOME>"), strings.ReplaceAll(MemoryStatusNotice(s, o.Now, 100), home, "<HOME>")})
			}
			dump := func() {
				portFixedDump("memorystatus", c.ID, map[string]any{"status": json.RawMessage(strings.ReplaceAll(string(gotJSON), home, "<HOME>")), "observed": observed})
			}
			if !reflect.DeepEqual(got, want) {
				dump()
				t.Fatalf("status: got %s, oracle %s", gotJSON, c.Status)
			}
			if s.ObservationSource != "jobs-db" || s.EffectiveExtractionRoute != "unknown" || s.StartupGuardDecision != "unknown" {
				t.Fatal("invented observation")
			}
			for _, o := range c.Observed {
				for label, pair := range map[string][2]string{
					"text":             {FormatMemoryStatus(s, o.Now), o.Text},
					"notice":           {MemoryStatusNotice(s, o.Now), o.Notice},
					"custom threshold": {MemoryStatusNotice(s, o.Now, 100), o.Custom},
				} {
					if value := strings.ReplaceAll(pair[0], home, "<HOME>"); value != pair[1] {
						dump()
						t.Errorf("%s at %v: got %q, oracle %q", label, o.Now, value, pair[1])
					}
				}
			}
			if !reflect.DeepEqual(before, memoryStatusFiles(t, home)) {
				t.Fatal("collector modified store bytes or file inventory")
			}
			if c.ID == "unsafe-integer" && (s.State != MemoryStatusUnavailable || s.LastSuccessAt != nil || s.LastFinishedAt != nil) {
				// port: fixed (CRW-1128, known-defects.md :538): an unreadable value is unavailable, and the counts read before it stay.
				t.Fatalf("a late read failure is unavailable, not a schema: %+v", s)
			}
			if c.ID == "non-numeric-time" && (s.LastSuccessAt != nil || !s.lastSuccessUnreadable) {
				t.Fatal("a time that is no number is unreadable, not NaN and not never (CRW-1128, known-defects.md :537)")
			}
		})
	}
	for _, c := range grid.Classify {
		if got := ClassifyMemoryError(c.Raw); got != c.Out {
			t.Errorf("classify %v: got %q, oracle %q", c.Raw, got, c.Out)
		}
	}
}

func TestMemoryStatusCauseOrder(t *testing.T) {
	var s MemoryStatus
	input := `{"state":"ok","observationSource":"jobs-db","effectiveExtractionRoute":"unknown","startupGuardDecision":"unknown","detail":"","storePath":"store","jobs":[],"exhausted":3,"exhaustedByCause":{"stream-closed":1,"capacity":1,"other":1},"lastSuccessAt":null,"lastFinishedAt":null}`
	if err := json.Unmarshal([]byte(input), &s); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if !strings.Contains(FormatMemoryStatus(s, 0), "[stream-closed=1, capacity=1, other=1]") {
			t.Fatal("stable ties lost document order")
		}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
	}
	var counts CauseCounts
	if err := json.Unmarshal([]byte(`{"other":1,"capacity":2,"other":3}`), &counts); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(counts)
	if err != nil || string(data) != `{"other":3,"capacity":2}` {
		t.Fatalf("duplicate keys: %s %v", data, err)
	}
	if data, err := json.Marshal(CauseCounts(nil)); err != nil || string(data) != "{}" {
		t.Fatalf("empty object: %s %v", data, err)
	}
	for _, bad := range []string{`[]`, `{"x":"not a count"}`} {
		if json.Unmarshal([]byte(bad), &counts) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestMemoryStatusDefaultsAndThresholds(t *testing.T) {
	home := memoryStatusHome(t, memoryStatusOracle{SQL: []string{"CREATE TABLE jobs (kind, status, retry_remaining, last_error, finished_at)", "INSERT INTO jobs VALUES ('stage','done',3,NULL,1000)"}})
	s := CollectMemoryStatus(home)
	if MemoryStatusNotice(s, 173800) != "" || !strings.Contains(MemoryStatusNotice(s, 173801), "2d ago") {
		t.Fatal("staleness must use strict greater-than")
	}
	for _, c := range []struct {
		Now float64
		Age string
	}{{1000, "0m ago"}, {999, "0m ago"}, {4599, "59m ago"}, {4600, "1h ago"}, {87399, "23h ago"}, {87400, "1d ago"}} {
		if !strings.Contains(FormatMemoryStatus(s, c.Now), "last success: "+c.Age+"\n") {
			t.Errorf("age %v != %s", c.Now, c.Age)
		}
	}
	future := float64(time.Now().Unix()) + 86400
	s.LastSuccessAt = &future
	if MemoryStatusNotice(s) != "" || !strings.Contains(FormatMemoryStatus(s), "last success: 0m ago\n") {
		t.Fatal("default time/future clamp")
	}
	missing := CollectMemoryStatus(filepath.Join(home, "missing"))
	if missing.State != MemoryStatusUnavailable || missing.StorePath != nil || MemoryStatusNotice(missing) != "" {
		t.Fatal("missing home must stay unavailable and silent")
	}
}

func TestMemoryStatusNegativeZeroJSON(t *testing.T) {
	home := memoryStatusHome(t, memoryStatusOracle{SQL: []string{"CREATE TABLE jobs (kind, status, retry_remaining, last_error, finished_at)", "INSERT INTO jobs VALUES ('stage','done',3,NULL,'-0')"}})
	s := CollectMemoryStatus(home)
	if s.LastSuccessAt == nil || !math.Signbit(*s.LastSuccessAt) {
		t.Fatal("collector must preserve Number('-0')")
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"lastSuccessAt":-0`) || !strings.Contains(string(data), `"lastSuccessAt":0`) {
		t.Fatalf("JSON.stringify(-0) is 0: %s", data)
	}
}
