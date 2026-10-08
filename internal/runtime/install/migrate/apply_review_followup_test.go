package migrate

// apply_review_followup_test.go holds the CRW-879 cases for the two findings the operator's pair evaluation of the
// state-copy apply left open after CRW-812: the dependency order judged a QA receipt by its file name instead of its
// content, and the write count inferred a completed rename from the destination's bytes. Each case names the behaviour it
// pins and fails on the code before CRW-879 for the reason its name gives.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// R1: a receipt under another name still publishes after the artifact its own manifest names, so an interruption between
// the two publications cannot leave the reference dangling.
func TestMigrateApplyReviewFollowupPublishesTheArtifactOfAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if v, i := slices.Index(got, "verdict.json"), slices.Index(got, "a.json"); v < 0 || i < 0 || v > i {
		t.Errorf("the artifact must publish before the receipt that names it: %v", got)
	}
}

// Control: the QA receipt name still takes the referrer's place, for a manifest this run can read and for one it cannot
// judge by content, so the dependencies-first order an unreadable record kept before is unchanged.
func TestMigrateApplyReviewFollowupKeepsTheQaReceiptOrder(t *testing.T) {
	for name, receipt := range map[string]string{
		"readable manifest":          "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
		"manifest of the wrong type": "{\"artifactManifest\":\"verdict.json\"}",
	} {
		t.Run(name, func(t *testing.T) {
			_, r, p := apPlan(t, map[string]string{
				"evidence/s/qa-receipt.json": receipt,
				"evidence/s/verdict.json":    "v",
			}, nil)
			pub, leaves := migrateApplyReviewRenames(t)
			if _, err := applyWith(r, p, pub); err != nil {
				t.Fatal(err)
			}
			got := leaves()
			if v, i := slices.Index(got, "verdict.json"), slices.Index(got, "qa-receipt.json"); v < 0 || i < 0 || v > i {
				t.Errorf("the artifact must publish before the receipt that names it: %v", got)
			}
		})
	}
}

// Control: a directory sync failure after this run's own rename still counts the write and records the sync failure.
func TestMigrateApplyReviewFollowupCountsARenameThatThenLostItsSync(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	seen := 0
	pub.at = func(step string) error {
		if step == "dirsync" {
			if seen++; seen == 2 { // 1 the canonical .gitignore, 2 the session file this run renamed
				return errApplyInterrupted
			}
		}
		return nil
	}
	res, err := applyWith(r, p, pub)
	if !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("a failed directory sync must stop the run: %v", err)
	}
	if res.WritesCompleted != 1 {
		t.Errorf("the rename this run completed must be counted: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); !strings.Contains(ai.Note, "directory sync failed") {
		t.Errorf("the item must record the directory sync failure: %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the final file must be whole: %q", got)
	}
}

// R2: a failure before this run's rename is not this run's write, whatever the destination holds. settle saw nothing, a
// competing writer publishes the same bytes and mode in the window that opens, and this run's own temporary create then
// fails with ENOSPC: nothing of this run's was renamed, so nothing may be counted and no directory sync failed.
func TestMigrateApplyReviewFollowupDoesNotCountAWriteItNeverRenamed(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	real := pub.createTemp
	seen := 0
	pub.createTemp = func(dir *Dir, name string) (int, error) {
		if seen++; seen == 1 { // 1 the canonical .gitignore, 2 the session file under test
			return real(dir, name)
		}
		put(t, apDst(ws, "sessions/a.json"), "{\"phase\":\"P\"}", 0o644)
		return -1, unix.ENOSPC
	}
	res, err := applyWith(r, p, pub)
	if !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("a failed temporary create must stop the run: %v", err)
	}
	if res.WritesCompleted != 0 {
		t.Errorf("a file this run never renamed must not be counted: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); ai.Note != "" {
		t.Errorf("no directory sync failed, so the item must carry no note: %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the competing writer's file must stay: %q", got)
	}
}

// Control: a record under evidence/ that holds no manifest array is not a receipt, so its dependencies are not hoisted and
// the plan order stands. This is the case that keeps the content judgement from reordering every artifact group.
func TestMigrateApplyReviewFollowupKeepsPlanOrderWithoutAManifest(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"verdict\":true}",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if a, v := slices.Index(got, "a.json"), slices.Index(got, "verdict.json"); a < 0 || v < 0 || a > v {
		t.Errorf("a record with no manifest array must keep the plan order: %v", got)
	}
}

// The two halves of the content judgement are pinned apart, because either one alone puts an artifact before the receipt
// that names it and would hide the other reverting to the name test.

// R1a, the ordering key half: the receipt itself is demoted to the referrer's place by its content, so a record under
// another name publishes after an unrelated record of its rank even though plan order puts the receipt first.
func TestMigrateApplyReviewFollowupDemotesAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json": "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/b.json": "plain",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if b, a := slices.Index(got, "b.json"), slices.Index(got, "a.json"); b < 0 || a < 0 || b > a {
		t.Errorf("the receipt under another name must publish after the record of its rank: %v", got)
	}
}

// R1b, the manifest scan half: the artifact a receipt under another name lists is hoisted above a plain record of the same
// rank, which only the scan reading that receipt's content can do.
func TestMigrateApplyReviewFollowupHoistsTheArtifactOfAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/b.json":         "plain",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if v, b := slices.Index(got, "verdict.json"), slices.Index(got, "b.json"); v < 0 || b < 0 || v > b {
		t.Errorf("the artifact must be hoisted above the plain record: %v", got)
	}
}

// R1c: a record the receipt reader could not read is not judged by content, so the plan's own measurement decides it
// before anything is opened. The plan's size stands in for the file, so the case needs no record of that size.
func TestMigrateApplyReviewFollowupKeepsTheNameOrderPastTheReceiptReadBound(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/qa-receipt.json": "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
	}, nil)
	it := apItem(t, &ApplyResult{Items: planItems(p)}, "evidence/s/qa-receipt.json").Item
	a := &applyRun{roots: r, plan: p, dirs: map[string]*Dir{}, srcs: map[string]*Dir{}, made: map[string]bool{}, result: &ApplyResult{}}
	defer a.close()
	if manifest, ok := a.migrateReviewFollowupReceiptManifest(it); !ok || len(manifest) != 1 {
		t.Fatalf("a record within the bound must be judged by content: %v %v", manifest, ok)
	}
	it.Size = migrateReviewFollowupReceiptReadCap + 1
	if manifest, ok := a.migrateReviewFollowupReceiptManifest(it); ok || manifest != nil {
		t.Errorf("a record past the receipt reader's bound must not be judged by content: %v %v", manifest, ok)
	}
}

// R1d: the bound is the bytes of the record itself, not the bytes up to the object's last token. The decoder consumes the
// whitespace after the object, so a record that grew past the bound after the plan measured it would otherwise be judged by
// content while the receipt reader refuses it. The helper takes the limit, so the case needs no record of that size.
func TestMigrateApplyReviewFollowupBoundsTheRecordNotTheObject(t *testing.T) {
	body := "{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}"
	for name, c := range map[string]struct {
		record string
		limit  int64
		want   bool
		// entries is how many of the record's entries name a dependency: a receipt whose entries are all of an
		// unexpected shape is still a receipt, with none.
		entries int
	}{
		"exactly the bound":             {record: body, limit: int64(len(body)), want: true, entries: 1},
		"past the bound by whitespace":  {record: body + "\n", limit: int64(len(body))},
		"past the bound by a long tail": {record: body + strings.Repeat(" ", 8), limit: int64(len(body))},
		"whitespace within the bound":   {record: body + "\n", limit: int64(len(body)) + 1, want: true, entries: 1},
		"truncated below the bound":     {record: body[:len(body)-1], limit: int64(len(body))},
		"data after the object":         {record: body + "{}", limit: int64(len(body)) + 2},
		"no manifest array":             {record: "{\"a\":1}", limit: 8},
		// The receipt reader looks the key up by its exact spelling in a decoded object, so a key that differs only in
		// case is not the manifest it reads, and this order must not treat it as one.
		"key of another case": {record: "{\"ArtifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", limit: 200},
		// The judgement is the array's presence and length, not the shape of its entries: an array whose entries this
		// reader cannot use is still a receipt, so it keeps the referrer's place rather than falling back to plan order.
		"entry of another type":      {record: "{\"artifactManifest\":[1,2]}", limit: 40, want: true},
		"entry of the wrong shape":   {record: "{\"artifactManifest\":[{\"path\":1,\"kind\":2}]}", limit: 60, want: true},
		"one good entry and one bad": {record: "{\"artifactManifest\":[1,{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", limit: 90, want: true, entries: 1},
		// An entry's own keys are read by their exact spelling too, so a field spelled differently names no dependency
		// rather than a false one the receipt reader would never use.
		"entry field of another case": {record: "{\"artifactManifest\":[{\"Path\":\"v.json\",\"Kind\":\"verdict\"}]}", limit: 80, want: true},
		// A duplicated key is read the way the receipt reader's own map decode reads it: the last value wins.
		"duplicate key keeps the last": {
			record:  "{\"artifactManifest\":[{\"path\":\"first.json\",\"kind\":\"verdict\"}],\"artifactManifest\":[{\"path\":\"last.json\",\"kind\":\"verdict\"}]}",
			limit:   200,
			want:    true,
			entries: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, ok := migrateReviewFollowupDecodeManifest(strings.NewReader(c.record), c.limit)
			if ok != c.want {
				t.Errorf("ok = %v, want %v (record %d bytes, limit %d)", ok, c.want, len(c.record), c.limit)
			}
			if ok && len(manifest) != c.entries {
				t.Errorf("manifest = %v, want %d entries", manifest, c.entries)
			}
			if name == "duplicate key keeps the last" && ok && manifest[0].Path != "last.json" {
				t.Errorf("the last value of a duplicated key must win, as the reader's map decode does: %v", manifest)
			}
			if !ok && manifest != nil {
				t.Errorf("a refused record must return no manifest: %v", manifest)
			}
		})
	}
}

func planItems(p *Plan) []ApplyItem {
	items := make([]ApplyItem, len(p.Items))
	for i, it := range p.Items {
		items[i].Item = it
	}
	return items
}

// R1e: a record that is both another receipt's artifact and a receipt itself still publishes after the artifact it names, so
// a chain of receipts is ordered by its dependencies rather than by plan order.
func TestMigrateApplyReviewFollowupOrdersAChainOfReceipts(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		// a.json names z/verdict.json, which is itself a receipt and names z/identity.json beside it: both artifacts are
		// referenced, so only the chain length can order them, and plan order would publish the middle record first.
		"evidence/s/a.json":            "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/z/verdict.json":    "{\"artifactManifest\":[{\"path\":\"z/identity.json\",\"kind\":\"artifact-identity\"}]}",
		"evidence/s/z/z/identity.json": "{\"ok\":true}",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	// Plan order is a.json, z/verdict.json, z/z/identity.json; the chain must publish deepest artifact first.
	deep, mid, top := slices.Index(got, "identity.json"), slices.Index(got, "verdict.json"), slices.Index(got, "a.json")
	if deep < 0 || mid < 0 || top < 0 {
		t.Fatalf("the chain must publish: %v", got)
	}
	if deep > mid || mid > top {
		t.Errorf("a chain of receipts must publish deepest artifact first: %v", got)
	}
}

// R1f: the key a cycle gets is the plan's own, not the order a Go map happened to hand the records over. Two records that
// name each other cannot be ordered against one another, but a rerun of the same plan must still publish them the same
// way, or the same input has two orders and neither is the one the report describes.
func TestMigrateApplyReviewFollowupOrdersACycleTheSameWayOnEveryRun(t *testing.T) {
	deps := map[string][]string{"a.json": {"b.json"}, "b.json": {"a.json"}}
	want := migrateReviewFollowupDepths(deps)
	for i := 0; i < 200; i++ {
		if got := migrateReviewFollowupDepths(deps); !maps.Equal(got, want) {
			t.Fatalf("the same plan must give the same key on every run: %v, then %v", want, got)
		}
	}
	// A record that names nothing is still 0, and a chain is still one longer than what it names.
	chain := map[string][]string{"a.json": {"b.json"}, "b.json": {"c.json"}}
	if got := migrateReviewFollowupDepths(chain); got["c.json"] != 0 || got["b.json"] != 1 || got["a.json"] != 2 {
		t.Errorf("a chain must keep its depth: %v", got)
	}
}

// R1g: a record that is not a receipt must be judged without being held. The receipt reader's bound is the size of the
// largest string the oracle could hold, far larger than any record, and this scan looks at every planned file under
// evidence/, so an ordinary artifact of a few hundred megabytes must cost a bounded pass — the bytes of one token, never
// a copy of the record. Each record is streamed into the test's own directory, so nothing of that size is ever held by
// the test either. The threshold is generous: the point is that the judgement does not grow with the record, not a
// particular allocation count.
func TestMigrateApplyReviewFollowupJudgesALargeRecordWithoutHoldingIt(t *testing.T) {
	const recordSize = 256 << 20
	dir := t.TempDir()
	cases := map[string]struct {
		gen  func(w io.Writer) error
		want bool
	}{
		// A JSON object of a few hundred megabytes whose members are all small, one of them holding the byte run the old
		// marker scan looked for: the record is not a receipt, and the judgement must find that out without holding it.
		"an object with many small members": {gen: migrateReviewFollowupManySmallMembers(recordSize)},
		// A binary of the same size whose first bytes are that run, refused from its first byte without a decode.
		"not text": {gen: migrateReviewFollowupNotText(recordSize)},
		// An array of the same size holding that run: a JSON value, but not the object a receipt is.
		"not an object": {gen: migrateReviewFollowupNotAnObject(recordSize)},
		// Leading whitespace alone larger than the largest token, then an object holding that run: the walk must stream
		// the whitespace rather than buffer it, and must not hold the object either.
		"long leading whitespace": {gen: migrateReviewFollowupLeadingSpace(recordSize)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := migrateReviewFollowupStreamRecord(t, dir, name, c.gen)
			before, peak, ok := migrateReviewFollowupJudgePeakHeap(t, path)
			if ok != c.want {
				t.Errorf("ok = %v, want %v for a %d byte record", ok, c.want, recordSize)
			}
			if grew := peak - before; grew > recordSize/4 {
				t.Errorf("the judgement grew the live heap by %d bytes of a %d byte record", grew, recordSize)
			}
		})
	}
	// A receipt whose manifest is there is still read, and its references are kept however large the record is: the
	// bounded pass above must not be bought by dropping what a readable receipt names.
	t.Run("a receipt with a large manifest", func(t *testing.T) {
		path := migrateReviewFollowupStreamRecord(t, dir, "receipt", func(w io.Writer) error {
			if _, err := io.WriteString(w, "{\"filler\":\""); err != nil {
				return err
			}
			if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: recordSize / 2}, recordSize/2); err != nil {
				return err
			}
			_, err := io.WriteString(w, "\",\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}")
			return err
		})
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		manifest, ok := migrateReviewFollowupDecodeManifest(f, migrateReviewFollowupReceiptReadCap)
		if !ok || len(manifest) != 1 || manifest[0].Path != "z/verdict.json" {
			t.Errorf("a readable receipt must keep the references its manifest names: %v %v", manifest, ok)
		}
	})
}

// R1g2: the residual the streaming judgement leaves is one token, not the record. A member other than the manifest is
// skipped through encoding/json's token API, which materialises the bytes of the single token it hands back, so a record
// whose only member is a string of a few hundred megabytes still costs that string once. That is recorded as a kept
// limitation in the issue's defect record, and this case pins the bound so a change that starts holding the record whole,
// or holding a skipped member more than once, fails here. The record is streamed from disk, so the test's own heap never
// holds it and the growth measured is the judgement's own.
func TestMigrateApplyReviewFollowupHoldsOneTokenNotTheRecord(t *testing.T) {
	const token = 16 << 20
	path := migrateReviewFollowupStreamRecord(t, t.TempDir(), "one token", func(w io.Writer) error {
		if _, err := io.WriteString(w, "{\"payload\":\""); err != nil {
			return err
		}
		if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: token}, token); err != nil {
			return err
		}
		_, err := io.WriteString(w, "\"}")
		return err
	})
	before, peak, ok := migrateReviewFollowupJudgePeakHeap(t, path)
	if ok {
		t.Error("a record with no manifest array is not a receipt")
	}
	grew := peak - before
	if grew < token/2 {
		t.Errorf("the one token the walk reads must be held: grew %d, token %d", grew, token)
	}
	if grew > 8*token {
		t.Errorf("the judgement must hold a few copies of one token, not the record: grew %d, token %d", grew, token)
	}
}

// migrateReviewFollowupManySmallMembers streams a JSON object of about size bytes whose members are all small, so no
// single token is larger than a few kilobytes. One member holds the byte run the old marker scan looked for, so this
// case is the one that measured a copy of the whole record before the judgement was streamed.
func migrateReviewFollowupManySmallMembers(size int) func(w io.Writer) error {
	return func(w io.Writer) error {
		if _, err := io.WriteString(w, "{\"items\":["); err != nil {
			return err
		}
		const member = 64 << 10
		written := 0
		// The run first, so a judgement that reads the record whole pays for it immediately.
		if _, err := io.WriteString(w, "\"Manifest\""); err != nil {
			return err
		}
		written += len("\"Manifest\"")
		for ; written < size; written += member {
			if written > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, "\""); err != nil {
				return err
			}
			if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: member}, member); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\""); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]}")
		return err
	}
}

// migrateReviewFollowupNotText streams a record of size bytes that begins with the old marker run and continues with
// bytes that are not text at all.
func migrateReviewFollowupNotText(size int) func(w io.Writer) error {
	return func(w io.Writer) error {
		if _, err := io.WriteString(w, "Manifest"); err != nil {
			return err
		}
		buf := bytes.Repeat([]byte{0xff}, 64<<10)
		for written := len("Manifest"); written < size; written += len(buf) {
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
		return nil
	}
}

// migrateReviewFollowupNotAnObject streams a JSON array of size bytes that holds the old marker run: a JSON value, but
// not the object a receipt is.
func migrateReviewFollowupNotAnObject(size int) func(w io.Writer) error {
	return func(w io.Writer) error {
		if _, err := io.WriteString(w, "[\"Manifest\","); err != nil {
			return err
		}
		if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: int64(size)}, int64(size)); err != nil {
			return err
		}
		_, err := io.WriteString(w, "]")
		return err
	}
}

// migrateReviewFollowupLeadingSpace streams size bytes of whitespace followed by an object holding the old marker run.
func migrateReviewFollowupLeadingSpace(size int) func(w io.Writer) error {
	return func(w io.Writer) error {
		if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: int64(size)}, int64(size)); err != nil {
			return err
		}
		_, err := io.WriteString(w, "{\"payload\":\"Manifest\"}")
		return err
	}
}

// migrateReviewFollowupSpaces is an endless stream of JSON whitespace, so a large run of it costs no memory to write.
type migrateReviewFollowupSpaces struct{ n int64 }

func (s *migrateReviewFollowupSpaces) Read(p []byte) (int, error) {
	if s.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > s.n {
		p = p[:s.n]
	}
	for i := range p {
		p[i] = ' '
	}
	s.n -= int64(len(p))
	return len(p), nil
}

// migrateReviewFollowupStreamRecord writes a record of the case's own shape to a file in dir and returns its path.
func migrateReviewFollowupStreamRecord(t *testing.T, dir, name string, gen func(w io.Writer) error) string {
	t.Helper()
	p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	if err := gen(bw); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// migrateReviewFollowupJudgePeakHeap judges the record at path and returns the live heap before it and the largest live
// heap the runtime reported while it ran. The record is streamed, so a record that is not a receipt must not raise the
// peak by its own size.
func migrateReviewFollowupJudgePeakHeap(t *testing.T, path string) (before, peak uint64, ok bool) {
	t.Helper()
	prev := debug.SetGCPercent(1) // the judgement reads a live heap, not garbage the collector has not reclaimed
	defer debug.SetGCPercent(prev)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runtime.GC()
	var b runtime.MemStats
	runtime.ReadMemStats(&b)
	before = b.HeapAlloc
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
		}
	}()
	_, ok = migrateReviewFollowupDecodeManifest(f, migrateReviewFollowupReceiptReadCap)
	close(stop)
	<-done
	if peak < before {
		peak = before
	}
	return before, peak, ok
}

// migrateReviewFollowupJudgePeakHeapRecord is migrateReviewFollowupJudgePeakHeap for a record already in memory.
func migrateReviewFollowupJudgePeakHeapRecord(t *testing.T, record string) (before, peak uint64, ok bool) {
	t.Helper()
	prev := debug.SetGCPercent(1) // the judgement reads a live heap, not garbage the collector has not reclaimed
	defer debug.SetGCPercent(prev)
	runtime.GC()
	var b runtime.MemStats
	runtime.ReadMemStats(&b)
	before = b.HeapAlloc
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
		}
	}()
	_, ok = migrateReviewFollowupDecodeManifest(strings.NewReader(record), migrateReviewFollowupReceiptReadCap)
	close(stop)
	<-done
	if peak < before {
		peak = before
	}
	return before, peak, ok
}

// R1h: a receipt the reader can read keeps every reference it names, however large its manifest is and however long its
// other keys are. An earlier pass capped the manifest member at 1 MiB and an object key at 64 KiB, and either cap dropped
// references of a receipt the reader itself reads, which left its artifacts in plan order — the dangling reference this
// issue exists to remove. There is no cap of this order's own.
func TestMigrateApplyReviewFollowupKeepsAReceiptPastAnyCapOfItsOwn(t *testing.T) {
	const entries = 30000
	var b strings.Builder
	b.WriteString("{\"")
	b.WriteString(strings.Repeat("k", 128<<10)) // an unrelated key longer than any 64 KiB key cap
	b.WriteString("\":\"x\",\"artifactManifest\":[")
	for i := 0; i < entries; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "{\"path\":\"v%d.json\",\"kind\":\"verdict\"}", i)
	}
	b.WriteString("]}")
	record := b.String()
	if len(record) <= 1<<20 {
		t.Fatalf("the case must hold a manifest larger than 1 MiB: %d bytes", len(record))
	}
	manifest, ok := migrateReviewFollowupDecodeManifest(strings.NewReader(record), migrateReviewFollowupReceiptReadCap)
	if !ok {
		t.Fatal("a receipt the reader can read must be judged by content")
	}
	if len(manifest) != entries {
		t.Errorf("every entry of the manifest must be kept: %d, want %d", len(manifest), entries)
	}
	last := fmt.Sprintf("v%d.json", entries-1)
	if manifest[0].Path != "v0.json" || manifest[len(manifest)-1].Path != last {
		t.Errorf("the first and last entries must be kept: %v ... %v", manifest[0], manifest[len(manifest)-1])
	}
}

// R1i: the walk must decide exactly what decoding the record would decide. These are the records where a hand-rolled
// reader most easily drifts from encoding/json: an escaped key, a duplicate key, a member of every other JSON type, a
// nested object that also holds the key, braces and brackets inside strings, and a record with data after it.
func TestMigrateApplyReviewFollowupWalksAsTheDecoderWould(t *testing.T) {
	records := map[string]struct {
		record string
		want   bool
	}{
		"a plain manifest":         {"{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		"the member after another": {"{\"a\":1,\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}],\"b\":2}", true},
		"an escaped key":           {"{\"artifact\\u004danifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		// Every spelling of the key that decodes to it must be caught before the record is decoded, or a receipt the
		// reader can read would keep the name judgement instead of its content.
		"a fully escaped first character": {"{\"\\u0061rtifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		// An escape that decodes to a different key is not the manifest key, and the decoder is what decides: this one
		// is \u006d, a lower-case m, so it spells artifactmanifest.
		"an escape spelling another key": {"{\"artifact\\u006danifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", false},
		"a duplicate key, last wins":     {"{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}],\"artifactManifest\":[{\"path\":\"w.json\",\"kind\":\"verdict\"}]}", true},
		"an empty array then a member":   {"{\"artifactManifest\":[],\"artifactManifest\":[{\"path\":\"w.json\",\"kind\":\"verdict\"}]}", true},
		"an empty array":                 {"{\"artifactManifest\":[]}", false},
		// A record the receipt reader's own decode refuses for nesting is refused here too, and it is refused by a
		// walk with a depth count rather than by recursing until the stack runs out: json.Decoder.Token enforces no nesting
		// limit of its own, so a member nested past the reader's limit must end the walk.
		"a member nested past the reader's limit": {record: "{\"payload\":" + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + "}", want: false},
		"a member at the reader's limit":          {record: "{\"payload\":" + strings.Repeat("[", 9998) + strings.Repeat("]", 9998) + ",\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", want: true},
		"a null member":                           {"{\"artifactManifest\":null}", false},
		"a string member":                         {"{\"artifactManifest\":\"x\"}", false},
		"a number member":                         {"{\"artifactManifest\":1}", false},
		"an object member":                        {"{\"artifactManifest\":{}}", false},
		"a brace inside a string":                 {"{\"artifactManifest\":[{\"path\":\"a}\"}]}", true},
		"a bracket inside a string":               {"{\"artifactManifest\":[{\"path\":\"a]\"}]}", true},
		"a nested object holding it":              {"{\"nested\":{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}}", false},
		"nested values before it":                 {"{\"outer\":{\"x\":{\"y\":[1,2,{\"z\":\"}\"}]}},\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		"data after the object":                   {"{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]} {}", false},
		"an empty object":                         {"{}", false},
		"not an object":                           {"[]", false},
		"not JSON at all":                         {"", false},
		"an unterminated array":                   {"{\"artifactManifest\":[}", false},
		"a trailing comma":                        {"{\"a\":1,}", false},
	}
	for name, c := range records {
		t.Run(name, func(t *testing.T) {
			_, got := migrateReviewFollowupDecodeManifest(strings.NewReader(c.record), migrateReviewFollowupReceiptReadCap)
			if got != c.want {
				t.Errorf("record %s: judged %v, want %v", c.record, got, c.want)
			}
		})
	}
}

// R1j: a manifest path is read the way the receipt reader reads it, through the reader's own UTF-8 normalisation, so a
// path that holds an invalid run names the plan file the reader's text holds. The reader normalises a maximal invalid
// subpart to one U+FFFD (source.DecodeUTF8, gate/js.go:43), so the two bytes of a truncated three-byte sequence are one
// U+FFFD there; decoding the record's own bytes with encoding/json's rule gives two, which names no plan file, drops the
// reference, and leaves the referenced record in plan order behind the receipt that refers to it — the dangling
// reference this issue exists to remove. The plan file here is named with the single U+FFFD the reader resolves.
func TestMigrateApplyReviewFollowupReadsAPathAsTheReceiptReaderDoes(t *testing.T) {
	const fffd = "\uFFFD"
	_, r, p := apPlan(t, map[string]string{
		// The receipt spells the verdict's directory with the invalid bytes E2 82 (a truncated three-byte sequence).
		"evidence/s/qa-receipt.json": "{\"artifactManifest\":[{\"path\":\"z/" + string([]byte{0xe2, 0x82}) + "/verdict.json\",\"kind\":\"verdict\"}]}",
		// The plan's own directory is named with the one U+FFFD the reader's normalisation produces.
		"evidence/s/z/" + fffd + "/verdict.json":           "{\"artifactManifest\":[{\"path\":\"artifact-identity.json\",\"kind\":\"artifact-identity\"}]}",
		"evidence/s/z/" + fffd + "/artifact-identity.json": "i",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	identity, verdict, receipt := slices.Index(got, "artifact-identity.json"), slices.Index(got, "verdict.json"), slices.Index(got, "qa-receipt.json")
	if identity < 0 || verdict < 0 || receipt < 0 {
		t.Fatalf("every record must publish: %v", got)
	}
	if verdict > receipt {
		t.Errorf("the reader resolves the receipt's path, so the verdict must publish before the receipt that names it: %v", got)
	}
	if identity > verdict {
		t.Errorf("the identity the verdict names must publish before the verdict: %v", got)
	}
}

// R1k: a record the receipt reader refuses for nesting is not a receipt here either, however deep the nesting sits. The
// reader's own decode allows 10000 levels, and encoding/json's token API enforces none, so a walk that recursed per level
// would run out of stack on a record of a few megabytes. The case streams 12 Mi nested arrays from disk and must end the
// walk with a refusal; a positive control inside the limit still reads its manifest.
func TestMigrateApplyReviewFollowupRefusesAMemberNestedPastTheReaderLimit(t *testing.T) {
	const depth = 12 << 20
	path := migrateReviewFollowupStreamRecord(t, t.TempDir(), "deep", func(w io.Writer) error {
		if _, err := io.WriteString(w, "{\"payload\":"); err != nil {
			return err
		}
		for _, b := range []byte{'[', ']'} {
			chunk := bytes.Repeat([]byte{b}, 1<<20)
			for n := 0; n < depth; n += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return err
				}
			}
		}
		_, err := io.WriteString(w, ",\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}")
		return err
	})
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, ok := migrateReviewFollowupDecodeManifest(f, migrateReviewFollowupReceiptReadCap); ok {
		t.Error("a record nested past the receipt reader's limit must not be judged a receipt")
	}
	inside := "{\"payload\":" + strings.Repeat("[", 9000) + strings.Repeat("]", 9000) + ",\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}"
	if manifest, ok := migrateReviewFollowupDecodeManifest(strings.NewReader(inside), migrateReviewFollowupReceiptReadCap); !ok || len(manifest) != 1 {
		t.Errorf("a record inside the reader's limit must keep its manifest: %v %v", manifest, ok)
	}
}

// R1l: a manifest that is not an array is refused without being held. The judgement reads the first token of the value and
// skips a non-array one token at a time, so a 128 MiB object under the key costs the bytes of one token, not the object.
func TestMigrateApplyReviewFollowupHoldsNoNonArrayManifest(t *testing.T) {
	const size = 128 << 20
	path := migrateReviewFollowupStreamRecord(t, t.TempDir(), "object manifest", func(w io.Writer) error {
		if _, err := io.WriteString(w, "{\"artifactManifest\":{\"items\":["); err != nil {
			return err
		}
		const member = 64 << 10
		for written := 0; written < size; written += member {
			if written > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, "\""); err != nil {
				return err
			}
			if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: member}, member); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\""); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]}}")
		return err
	})
	before, peak, ok := migrateReviewFollowupJudgePeakHeap(t, path)
	if ok {
		t.Error("a manifest that is an object is not a receipt's array")
	}
	if grew := peak - before; grew > size/8 {
		t.Errorf("the judgement grew the live heap by %d bytes of a %d byte manifest object", grew, size)
	}
}

// R1m: a path or kind value that is a container is skipped token by token, never decoded whole. The value under the path
// key is a 128 MiB array of small strings: the judgement reads it without holding it, so it grows by the bytes of one
// token, and the entry names no path because a container is not the string the reader would use.
func TestMigrateApplyReviewFollowupHoldsNoContainerUnderAField(t *testing.T) {
	const size = 128 << 20
	path := migrateReviewFollowupStreamRecord(t, t.TempDir(), "container path", func(w io.Writer) error {
		if _, err := io.WriteString(w, "{\"artifactManifest\":[{\"path\":["); err != nil {
			return err
		}
		const member = 64 << 10
		for written := 0; written < size; written += member {
			if written > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, "\""); err != nil {
				return err
			}
			if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: member}, member); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\""); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "],\"kind\":\"verdict\"}]}")
		return err
	})
	before, peak, ok := migrateReviewFollowupJudgePeakHeap(t, path)
	if !ok {
		t.Error("a record whose manifest is an array is a receipt, whatever its entries hold")
	}
	if grew := peak - before; grew > size/8 {
		t.Errorf("the judgement grew the live heap by %d bytes of a %d byte container under a field", grew, size)
	}
}
