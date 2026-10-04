package recall

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"testing"
)

func TestHitCountsPersistenceAndEmpty(t *testing.T) {
	db, path := indexTestDB(t)
	if err := bumpHitCounts(db, nil, "unused"); err != nil {
		t.Fatal(err)
	}
	refs := []string{"thread:a", "thread:b", "file:x' OR 1=1; --"}
	if err := bumpHitCounts(db, refs, "first"); err != nil {
		t.Fatal(err)
	}
	if err := bumpHitCounts(db, []string{"thread:a", "thread:a"}, "older timestamp"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err := openIndexReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := readHitCounts(db, append(refs, "unknown"))
	want := map[string]float64{refs[0]: 3, refs[1]: 1, refs[2]: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got, want)
	}
	if row := recallRow(t, recallStmt(t, db, "SELECT last_hit_at FROM recall_hit_counts WHERE ref=?"), refs[0]); row["last_hit_at"] != "older timestamp" {
		t.Fatal("timestamp was normalized", row)
	}
	if err := bumpHitCounts(db, nil, "unused"); err != nil {
		t.Fatal(err)
	}
	if err := bumpHitCounts(db, refs, "bad"); err == nil {
		t.Fatal("read-only writer did not fail")
	}
}
func TestHitCountsCoercionOracle(t *testing.T) {
	db, _ := indexTestDB(t)
	want := indexOracle(t)
	put := recallStmt(t, db, "INSERT INTO recall_hit_counts VALUES(?, ?, ?)")
	refs := []string{}
	for i, v := range want["inputs"].([]any) {
		ref := "text:" + strconv.Itoa(i)
		refs = append(refs, ref)
		if _, err := put.Run(ref, v, "stamp"); err != nil {
			t.Fatal(err)
		}
	}
	for i, v := range want["blobInputs"].([]any) {
		b := []byte{}
		for _, n := range v.([]any) {
			b = append(b, byte(n.(float64)))
		}
		ref := "blob:" + strconv.Itoa(i)
		refs = append(refs, ref)
		if _, err := put.Run(ref, b, "stamp"); err != nil {
			t.Fatal(err)
		}
	}
	counts := readHitCounts(db, refs)
	for _, v := range want["coercions"].([]any) {
		row := v.(map[string]any)
		n, exists := counts[row["ref"].(string)]
		if !exists {
			t.Fatal("missing coercion", row)
		}
		number := ""
		switch {
		case math.IsNaN(n):
			number = "NaN"
		case math.IsInf(n, 1):
			number = "Infinity"
		case math.IsInf(n, -1):
			number = "-Infinity"
		default:
			b, err := json.Marshal(n)
			if err != nil {
				t.Fatal(err)
			}
			number = string(b)
		}
		if number != row["number"] {
			t.Errorf("%s: %s, oracle %s", row["ref"], number, row["number"])
		}
	}
	if _, err := put.Run("null", nil, "stamp"); err == nil {
		t.Fatal("NOT NULL count weakened")
	}
}
func TestHitCountsWriteErrorKeepsPriorCommits(t *testing.T) {
	db, _ := indexTestDB(t)
	recallSQL(t, db, "CREATE TRIGGER fail_hit BEFORE INSERT ON recall_hit_counts WHEN new.ref='fail' BEGIN SELECT RAISE(ABORT,'hit refused'); END;")
	if err := bumpHitCounts(db, []string{"first", "fail", "last"}, "stamp"); err == nil || err.Error() != "hit refused" {
		t.Fatal(err)
	}
	if got := readHitCounts(db, []string{"first", "fail", "last"}); !reflect.DeepEqual(got, map[string]float64{"first": 1}) {
		t.Fatal("batch parity", got)
	}
	recallSQL(t, db, "DROP TABLE recall_hit_counts")
	if len(readHitCounts(db, []string{"first"})) != 0 {
		t.Fatal("missing table did not degrade")
	}
	if err := bumpHitCounts(db, nil, "stamp"); err != nil {
		t.Fatal(err)
	}
	if err := bumpHitCounts(db, []string{"first"}, "stamp"); err == nil {
		t.Fatal("missing table writer succeeded")
	}
	_ = db.Close()
	if len(readHitCounts(db, []string{"first"})) != 0 {
		t.Fatal("closed reader returned history")
	}
	if err := bumpHitCounts(db, []string{"first"}, "stamp"); err == nil {
		t.Fatal("closed writer succeeded")
	}
}
