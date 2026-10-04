package adjudication

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"golang.org/x/sys/unix"
)

func TestAppendReplayAndCoordinatedCorrection(t *testing.T) {
	records, prs, groups := scripted()
	file := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := Append(file, records...); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(bytes.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(records, loaded) {
		t.Fatal("record format did not round-trip")
	}
	var corrections []Record
	for _, r := range records {
		if r.Judgment != nil && r.Judgment.Problem == "S" && r.Judgment.Verdict == Correct {
			j := *r.Judgment
			j.Grade = review.P0
			corrections = append(corrections, Record{Schema, "fix-" + r.ID, nil, &j})
		}
	}
	if err := Append(file, corrections[0]); err == nil {
		t.Fatal("inconsistent single correction accepted")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("refused append changed history")
	}
	if err := Append(file, corrections...); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(file)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("append rewrote history")
	}
	loaded, err = Read(bytes.NewReader(after))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Report(loaded, prs, groups)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range s.Rows {
		if row.P0.Denominator != 2 || row.P1.Denominator != 1 {
			t.Fatalf("corrected severity: %+v", row)
		}
	}
	j := *corrections[0].Judgment
	j.Note = "subsequent truthful evidence"
	if err := Append(file, Record{Schema, "later", nil, &j}); err != nil {
		t.Fatalf("replay rejected historical intermediate conflict: %v", err)
	}
	if err := Append(file, records[0]); err == nil {
		t.Fatal("duplicate event accepted")
	}
}

func TestCorruptionAndWriterLock(t *testing.T) {
	records, _, _ := scripted()
	dir := t.TempDir()
	file := filepath.Join(dir, "ledger")
	if err := Append(file, records[0]); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := Append(file, records[1]); err == nil {
		t.Fatal("contended append did not fail")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	torn := append(bytes.Clone(before), '{')
	if err := os.WriteFile(file, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Append(file, records[1]); err == nil {
		t.Fatal("torn tail silently repaired")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(after, torn) {
		t.Fatal("corrupt data was discarded")
	}
	for _, data := range []string{"{}\n", "null\n", "{\"unexpected\":1}\n", string(before[:len(before)-1]), strings.Repeat("x", maxRecordBytes+1) + "\n"} {
		if _, err := Read(strings.NewReader(data)); err == nil {
			t.Fatal("corrupt snapshot accepted")
		}
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := Append(link, records[1]); err == nil {
		t.Fatal("ledger symlink followed")
	}
}

func TestInvalidRecordBoundaries(t *testing.T) {
	records, _, _ := scripted()
	data, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Record){
		"schema":    func(r *Record) { r.Schema = "future" },
		"payload":   func(r *Record) { r.Judgment = &Judgment{} },
		"identity":  func(r *Record) { r.Run.PR.Head = "wrong" },
		"source":    func(r *Record) { r.Run.Source = "other" },
		"model":     func(r *Record) { r.Run.Model = " " },
		"hash":      func(r *Record) { r.Run.ArtifactSHA256 = "bad" },
		"status":    func(r *Record) { r.Run.Status = "invalid"; r.Run.Reason = "invalid" },
		"reason":    func(r *Record) { r.Run.Reason = "unexpected" },
		"path":      func(r *Record) { r.Run.Findings[0].File = "../outside" },
		"line":      func(r *Record) { r.Run.Findings[0].Line = 0 },
		"findingId": func(r *Record) { r.Run.Findings[1].ID = r.Run.Findings[0].ID },
		"negative":  func(r *Record) { r.Run.Calls[0].Record.Tokens.Input = -1 },
		"class":     func(r *Record) { r.Run.Calls[0].Record.Class = "other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var r Record
			if err := json.Unmarshal(data, &r); err != nil {
				t.Fatal(err)
			}
			mutate(&r)
			if _, err := Report([]Record{r}, []PR{r.Run.PR}, []Reviewer{r.Run.Reviewer}); err == nil {
				t.Fatal("invalid record accepted")
			}
		})
	}
	r := *records[0].Run
	r.ID = "different" // The artifact identity must not be counted twice under a fresh ID.
	if _, err := replay([]Record{records[0], {Schema, "new-id", &r, nil}}); err == nil {
		t.Fatal("duplicate artifact accepted")
	}
	j := *records[6].Judgment
	j.FindingID = "missing"
	if _, err := replay([]Record{records[0], {Schema, "bad-ref", nil, &j}}); err == nil {
		t.Fatal("unknown finding accepted")
	}
	j = *records[6].Judgment
	j.ChangeCommit = records[0].Run.PR.Head
	if _, err := replay([]Record{records[0], {Schema, "bad-change", nil, &j}}); err == nil {
		t.Fatal("reviewed head accepted as later change")
	}
}
